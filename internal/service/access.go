package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	api "github.com/orpheus-agents/orpheus/client"
)

// projectAccess keeps source identities in the core's input authority and maps
// service identities only into the configured opaque provider fields.
func projectAccess(binding config.Access, audience []string, actor string) (api.AccessScopeContext, []api.AccessSubject, error) {
	if err := binding.Validate(); err != nil {
		return nil, nil, err
	}
	if len(audience) < 2 || len(audience) > 4096 || actor == binding.BotID {
		return nil, nil, errors.New("invalid protected audience or actor")
	}
	members := slices.Clone(audience)
	slices.Sort(members)
	allowed := make([]api.AccessSubject, 0, len(members))
	readers := make([]json.RawMessage, 0, len(members)-1)
	seen := make(map[string]bool, len(members))
	for _, id := range members {
		if !config.ValidID(id) || seen[id] {
			return nil, nil, errors.New("invalid protected audience")
		}
		seen[id] = true
		allowed = append(allowed, api.AccessSubject{Namespace: binding.Namespace, ID: id})
		if id == binding.BotID {
			continue
		}
		mapped, ok := binding.IdentityProjection.Subjects[id]
		if !ok {
			return nil, nil, errors.New("audience member lacks service identity")
		}
		raw, err := mapped.MarshalJSON()
		if err != nil {
			return nil, nil, err
		}
		readers = append(readers, raw)
	}
	if !seen[actor] || !seen[binding.BotID] {
		return nil, nil, errors.New("protected actor or bot outside audience")
	}
	raw, err := json.Marshal(binding.ScopeContext)
	if err != nil {
		return nil, nil, err
	}
	var scope api.AccessScopeContext
	if err := json.Unmarshal(raw, &scope); err != nil {
		return nil, nil, err
	}
	actorID, err := binding.IdentityProjection.Subjects[actor].MarshalJSON()
	if err != nil {
		return nil, nil, err
	}
	scope[binding.IdentityProjection.ActorField] = bytes.Clone(actorID)
	scope[binding.IdentityProjection.AudienceField], err = json.Marshal(readers)
	return scope, allowed, err
}

func validateProtectedContext(env conversation.Envelope, w config.Workflow) error {
	a, proof := env.AccessContext, env.AccessProof
	if a == nil || proof == nil || a.Subject.Namespace != w.Access.Namespace || a.ContextObservedAt.IsZero() || a.AllowedInputSubjects == nil {
		return errors.New("missing protected authority")
	}
	digest, err := w.Access.Digest()
	if err != nil || digest != proof.ProjectionDigest {
		return errors.New("protected projection configuration changed")
	}
	scope, allowed, err := projectAccess(*w.Access, proof.AudienceSubjects, a.Subject.ID)
	if err != nil {
		return err
	}
	if len(allowed) != len(*a.AllowedInputSubjects) {
		return errors.New("protected input authority mismatch")
	}
	seen := make(map[api.AccessSubject]bool, len(allowed))
	for _, subject := range *a.AllowedInputSubjects {
		if seen[subject] || !slices.Contains(allowed, subject) {
			return errors.New("protected input authority mismatch")
		}
		seen[subject] = true
	}
	want, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(a.ScopeContext)
	if err != nil || !bytes.Equal(want, actual) {
		return errors.New("protected service projection mismatch")
	}
	return nil
}

func protectedContract(s *conversation.Session, r *conversation.Run, w config.Workflow, key conversation.Key) (conversation.Envelope, error) {
	if s == nil || r == nil {
		return conversation.Envelope{}, errors.New("missing protected run")
	}
	env, err := contract(*s, *r)
	if err != nil {
		return env, err
	}
	a := env.AccessContext
	if validateProtectedContext(env, w) != nil || env.ContextStart == nil || env.ContextStart.CreatedAt <= 0 || !config.ValidID(env.ContextStart.PostID) || env.Key() != key || a.InvocationID != key.Invocation(env.Anchor) {
		return env, errors.New("unknown protected context provenance")
	}
	return env, nil
}

// Admission is called after the coordinator reserves a run slot. The result is
// frozen in the existing pending input, then in the core's accepted metadata.
func (e *Engine) admission(ctx context.Context, w config.Workflow, key conversation.Key, env conversation.Envelope, posts []mattermost.Post) (*api.AccessContext, *conversation.AccessProof, error) {
	observed := time.Now().UTC()
	ch, err := e.MM.Channel(ctx, key.Channel)
	if err != nil {
		return nil, nil, err
	}
	if ch.ID != key.Channel || ch.Type != "P" || ch.DeleteAt != 0 || w.Access.BotID != e.Bot.ID || !slices.Contains(w.IncludeIDs, key.Channel) {
		return nil, nil, errors.New("protected channel binding mismatch")
	}
	audience, err := e.MM.Audience(ctx, key.Channel)
	if err != nil {
		return nil, nil, err
	}
	if len(audience) == 0 || !slices.Contains(audience, e.Bot.ID) {
		return nil, nil, errors.New("bot missing from audience")
	}
	var subject string
	for _, post := range posts {
		if !slices.Contains(env.TriggerIDs, post.ID) {
			continue
		}
		if !slices.Contains(audience, post.UserID) {
			return nil, nil, errors.New("trigger author outside audience")
		}
		if post.ID == env.Anchor {
			subject = post.UserID
		}
	}
	scope, allowed, err := projectAccess(*w.Access, audience, subject)
	if err != nil {
		return nil, nil, err
	}
	// Only this authenticated execution bot is excluded from service readers.
	// Other bots are not silently treated as integration identities.
	for _, id := range audience {
		if id == e.Bot.ID {
			continue
		}
		user, err := e.MM.User(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if user.ID != id || user.DeleteAt != 0 || user.IsBot {
			return nil, nil, errors.New("unconfirmed human audience member")
		}
	}
	if time.Since(observed) >= time.Minute {
		return nil, nil, errors.New("audience observation expired")
	}
	digest, err := w.Access.Digest()
	if err != nil {
		return nil, nil, err
	}
	proof := &conversation.AccessProof{AudienceSubjects: slices.Clone(audience), ProjectionDigest: digest}
	access := &api.AccessContext{InvocationID: key.Invocation(env.Anchor), Subject: api.AccessSubject{Namespace: w.Access.Namespace, ID: subject}, ScopeContext: scope, ContextObservedAt: observed, AllowedInputSubjects: &allowed}
	return access, proof, nil
}

func (e *Engine) cleanPending(ctx context.Context, p pending) (pending, error) {
	// Only frozen trigger posts enter this builder. Do not transfer an old payload,
	// a previous file index, or context from the previous sandbox.
	builder := conversation.Builder{Source: e.MM, Config: e.Config, Workflow: p.Workflow, Bot: e.Bot, TriggerOnly: true}
	env, messages, err := builder.BuildMessages(ctx, p.Envelope.Key(), p.Channel, p.TriggerPosts, conversation.Snapshot{}, true, time.Now())
	if err != nil {
		return p, err
	}
	if env.Anchor != p.Envelope.Anchor || !slices.Equal(env.TriggerIDs, p.Envelope.TriggerIDs) {
		return p, errors.New("clean input changed its frozen triggers")
	}
	env.AccessContext = p.Envelope.AccessContext
	env.AccessProof = p.Envelope.AccessProof
	p.Envelope, p.Messages, p.Session = env, messages, ""
	return p, nil
}
