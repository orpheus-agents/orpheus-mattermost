// Package service reconciles source threads with authoritative Orpheus history.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/agentbox"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

type API interface {
	Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error)
	Sessions(context.Context, string, string) ([]conversation.Session, error)
	Submit(context.Context, config.Workflow, conversation.Envelope, []conversation.InputMessage, string, string, string) (conversation.Accepted, error)
	Cancel(context.Context, string, string) error
	Run(context.Context, string, string) (conversation.Run, error)
	Watch(context.Context, string, func()) error
}
type MM interface {
	conversation.ContextSource
	delivery.Source
	Channels(context.Context) ([]mattermost.Channel, error)
	Posts(context.Context, string, time.Time) ([]mattermost.Post, error)
	Me(context.Context) (mattermost.User, error)
	MaxPostChars(context.Context) (int, error)
	Typing(context.Context, string, string) error
	Watch(context.Context, func(mattermost.Event)) error
}

// Sandbox access is limited to preparing a clarification for an active run.
type Sandbox interface {
	Prepare(context.Context, conversation.Session, conversation.Run, config.Workflow, attachments.Request) error
}

type pending struct {
	Workflow                  config.Workflow
	Envelope                  conversation.Envelope
	Session, Run, Predecessor string
	Messages                  []conversation.InputMessage
}
type Engine struct {
	Config       config.Config
	MM           MM
	API          API
	Sandbox      Sandbox
	SourceID     string
	mu           sync.Mutex
	pending      map[conversation.Key]pending
	pendingSince map[conversation.Key]time.Time
	inputQueue   map[conversation.Key]int
	outputQueue  map[string]bool
	Notify       func(conversation.Key)
	Bot          mattermost.User
	Reserve      func(conversation.Key, config.Workflow) bool
	MaxPostChars int
}

func (e *Engine) get(key conversation.Key) (pending, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pending[key]
	return p, ok
}
func (e *Engine) set(key conversation.Key, p pending) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		e.pending = map[conversation.Key]pending{}
	}
	if e.pendingSince == nil {
		e.pendingSince = map[conversation.Key]time.Time{}
	}
	if _, ok := e.pendingSince[key]; !ok {
		e.pendingSince[key] = time.Now()
	}
	e.pending[key] = p
}
func (e *Engine) clear(key conversation.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pending, key)
	delete(e.pendingSince, key)
	delete(e.inputQueue, key)
}

// Gauges describe the last reconciled admission batches and runs blocked on
// publication. They are observations, not an independent delivery ledger.
func (e *Engine) queues() (inputs, outputs int, uncertain time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, count := range e.inputQueue {
		inputs += count
	}
	outputs = len(e.outputQueue)
	for _, since := range e.pendingSince {
		uncertain = max(uncertain, time.Since(since))
	}
	return
}
func (e *Engine) queued(key conversation.Key, count int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inputQueue == nil {
		e.inputQueue = map[conversation.Key]int{}
	}
	if count == 0 {
		delete(e.inputQueue, key)
	} else {
		e.inputQueue[key] = count
	}
}
func contract(s conversation.Session, r conversation.Run) (conversation.Envelope, error) {
	for _, m := range s.Messages {
		if m.RunID == r.ID && m.Role == "user" {
			if !conversation.HasMetadata(m.Metadata) {
				continue
			}
			env, err := conversation.Decode(m.Metadata)
			if err != nil {
				return env, err
			}
			if env.Kind == "initial" {
				return env, nil
			}
		}
	}
	return conversation.Envelope{}, errors.New("run lacks initial input contract")
}
func hookOutput(s conversation.Session, r conversation.Run, env conversation.Envelope, bot string) (attachments.Output, error) {
	if r.AgentStatus == "" {
		return attachments.Output{Files: []attachments.Artifact{}}, nil
	}
	for _, h := range r.Hooks {
		if h.Name == "after_run" && h.Status == "completed" && h.Completeness == "complete" {
			var o attachments.Output
			if len(h.Output) > 16<<10 || json.Unmarshal([]byte(h.Output), &o) != nil {
				return o, errors.New("invalid hook manifest")
			}
			if o.Stage != "ready" {
				return o, errors.New("export incomplete")
			}
			return o, o.Validate(attachments.Export{SourceID: env.Source, BotID: bot, ChannelID: env.Channel, SessionID: s.ID, RunID: r.ID, Limits: env.Request.Limits})
		}
	}
	return attachments.Output{}, errors.New("export result unknown")
}
func (e *Engine) publish(ctx context.Context, p *delivery.Publisher, s conversation.Session, r conversation.Run) (result error) {
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.outputQueue == nil {
			e.outputQueue = map[string]bool{}
		}
		if result != nil {
			e.outputQueue[r.ID] = true
		} else {
			delete(e.outputQueue, r.ID)
		}
	}()
	env, err := contract(s, r)
	if err != nil {
		return err
	}
	p.SkipAttachments = p.NoticeExists("attachments_failed", r.ID)
	defer func() { p.SkipAttachments = false }()
	send := func(mid, text string, files []attachments.Artifact) error {
		for _, part := range delivery.Parts(p.Key, s.ID, r.ID, mid, text, env.Render, files) {
			if err := p.Publish(ctx, part); err != nil {
				if !errors.Is(err, delivery.ErrUpload) {
					return err
				}
				for _, notice := range delivery.Parts(p.Key, s.ID, r.ID, "attachments_failed", "Could not deliver the result attachments.", env.Render, nil) {
					if err := p.Publish(ctx, notice); err != nil {
						return err
					}
				}
				p.SkipAttachments = true
				if part.Text == "" {
					continue
				}
				if err := p.Publish(ctx, part); err != nil {
					return err
				}
			}
		}
		return nil
	}
	messages := slices.Clone(s.Messages)
	slices.SortStableFunc(messages, func(a, b conversation.Message) int { return a.Position - b.Position })
	var output attachments.Output
	stopped := p.NoticeExists("attachments_failed", r.ID)
	if r.Terminal() && !stopped {
		output, err = hookOutput(s, r, env, e.Bot.ID)
		if err != nil {
			if err = send("attachments_failed", "Could not deliver the result attachments.", nil); err != nil {
				return err
			}
			stopped = true
			p.SkipAttachments = true
			output.Files = nil
		}
	}
	hasAnswer := false
	for _, m := range messages {
		if m.RunID != r.ID || m.Role != "assistant" || m.Position < 0 {
			continue
		}
		if m.Kind == "progress" {
			if env.Render.Commentary && m.Text != "" {
				if err = send(m.ID, m.Text, nil); err != nil {
					return err
				}
			}
			continue
		}
		if m.Kind != "answer" || !r.Terminal() {
			continue
		}
		var files []attachments.Artifact
		if m.ID == r.FinalMessageID && !stopped {
			files = output.Files
		}
		if m.Text == "" && len(files) == 0 {
			continue
		}
		hasAnswer = true
		if err = send(m.ID, m.Text, files); err != nil {
			return err
		}
	}
	if !r.Terminal() {
		return nil
	}
	if r.FinalMessageID == "" && len(output.Files) > 0 {
		if err = send("attachments_only", "", output.Files); err != nil {
			return err
		}
		hasAnswer = true
	}
	if text := delivery.Failure(r); text != "" {
		return send("run_status", text, nil)
	}
	if !hasAnswer && !stopped {
		return send("empty_result", "Run finished without a text answer or files.", nil)
	}
	return nil
}
func active(snapshot conversation.Snapshot) (*conversation.Session, *conversation.Run, error) {
	var session *conversation.Session
	var run *conversation.Run
	for i := range snapshot.Sessions {
		s := &snapshot.Sessions[i]
		for j := range s.Runs {
			r := &s.Runs[j]
			if !r.Terminal() {
				if run != nil {
					return nil, nil, errors.New("multiple active runs for thread")
				}
				session, run = s, r
			}
		}
	}
	return session, run, nil
}
func accepted(p pending, snapshot conversation.Snapshot) bool {
	for _, s := range snapshot.Sessions {
		for _, m := range s.Messages {
			if m.Role != "user" {
				continue
			}
			env, err := conversation.Decode(m.Metadata)
			if err == nil && env.Anchor == p.Envelope.Anchor && env.Predecessor == p.Envelope.Predecessor {
				return true
			}
		}
	}
	return false
}
func joinedInput(messages []conversation.InputMessage) string {
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Text)
	}
	return out.String()
}

func transfer(snapshot conversation.Snapshot) (conversation.Envelope, []conversation.InputMessage, string, bool) {
	descendants := map[string]bool{}
	for _, s := range snapshot.Sessions {
		for _, m := range s.Messages {
			env, err := conversation.Decode(m.Metadata)
			if err == nil {
				descendants[env.Predecessor] = true
			}
		}
	}
	for _, s := range snapshot.Sessions {
		for index, m := range s.Messages {
			if m.Role != "user" || m.Delivery != "rejected" || m.Error != "run_finished_before_delivery" || descendants[m.ID] {
				continue
			}
			env, err := conversation.Decode(m.Metadata)
			if err != nil || env.Kind != "clarification" {
				continue
			}
			for _, r := range s.Runs {
				if r.ID != m.RunID || r.Status != "completed" || r.StopReason == "token_limit" {
					continue
				}
				messages := []conversation.InputMessage{{Text: m.Text}}
				for j := index - 1; j >= 0; j-- {
					prior := s.Messages[j]
					if prior.RunID != m.RunID || prior.Role != "user" || prior.Delivery != "rejected" || conversation.HasMetadata(prior.Metadata) {
						break
					}
					messages = append([]conversation.InputMessage{{Text: prior.Text}}, messages...)
				}
				env.Kind = "initial"
				env.Predecessor = m.ID
				return env, messages, m.ID, true
			}
		}
	}
	return conversation.Envelope{}, nil, "", false
}

// Thread is called under exclusive thread ownership by the coordinator.
func (e *Engine) Thread(ctx context.Context, w config.Workflow, key conversation.Key, channel mattermost.Channel, inScope bool) error {
	snapshot, err := e.API.Snapshot(ctx, key)
	if err != nil {
		return err
	}
	return e.reconcile(ctx, w, key, channel, inScope, snapshot)
}

func (e *Engine) reconcile(ctx context.Context, w config.Workflow, key conversation.Key, channel mattermost.Channel, inScope bool, snapshot conversation.Snapshot) error {
	current, run, err := active(snapshot)
	if err != nil {
		return err
	}
	if !inScope {
		e.clear(key)
		if run != nil {
			err = e.API.Cancel(ctx, current.ID, run.ID)
			if orpheus.Code(err) == "run_not_cancellable" {
				return nil
			}
			return err
		}
		return nil
	}
	p := &delivery.Publisher{MM: e.MM, Key: key, BotID: e.Bot.ID}
	readAt := time.Now()
	if err = p.Refresh(ctx); err != nil {
		if run != nil && (mattermost.Status(err) == 403 || mattermost.Status(err) == 404) {
			cancelErr := e.API.Cancel(ctx, current.ID, run.ID)
			if cancelErr != nil && orpheus.Code(cancelErr) != "run_not_cancellable" {
				return cancelErr
			}
		}
		return err
	}
	postCount := len(p.Posts)
	for _, s := range snapshot.Sessions {
		for _, r := range s.Runs {
			if err = e.publish(ctx, p, s, r); err != nil {
				return err
			}
		}
	}
	refresh := len(p.Posts) != postCount
	now := time.Now()
	for _, post := range p.Posts {
		end := time.UnixMilli(post.CreateAt).Add(w.MessageBatchWindow.Value())
		if end.After(readAt) && !end.After(now) {
			refresh = true
		}
	}
	if refresh {
		if err := p.Refresh(ctx); err != nil {
			return err
		}
	}
	if run != nil {
		_ = e.MM.Typing(ctx, key.Channel, key.Root)
	}
	if w.Draining {
		return nil
	}
	if frozen, ok := e.get(key); ok {
		if accepted(frozen, snapshot) {
			e.clear(key)
		} else {
			targetLost := false
			if frozen.Run == "" && frozen.Session != "" {
				for _, session := range snapshot.Sessions {
					if session.ID == frozen.Session && !session.Reusable(frozen.Workflow.EffectiveRevision) {
						targetLost = true
					}
				}
			}
			if targetLost || frozen.Run != "" && (run == nil || run.ID != frozen.Run) {
				// History proves that the clarification was not accepted and
				// its target no longer accepts input. Rebuild admission below:
				// the next run needs fresh revision, session and input indexes.
				e.clear(key)
			} else {
				if frozen.Run != "" && run.Status != "running" {
					return nil
				}
				if frozen.Run == "" && e.Reserve != nil && !e.Reserve(key, w) {
					return nil
				}
				return e.submit(ctx, key, frozen, current, run)
			}
		}
	}
	var latest *conversation.Session
	if len(snapshot.Sessions) > 0 {
		latest = &snapshot.Sessions[len(snapshot.Sessions)-1]
	}
	initial := latest == nil || !latest.Reusable(w.EffectiveRevision)
	if run != nil {
		if current.Revision != w.EffectiveRevision || run.Status != "running" {
			return nil
		}
		initial = false
	}
	rejected := p.Rejected(w.EffectiveRevision)
	posts := slices.DeleteFunc(slices.Clone(p.Posts), func(post mattermost.Post) bool { return rejected[post.ID] })
	builder := conversation.Builder{Source: e.MM, Config: e.Config, Workflow: w, Bot: e.Bot}
	env, messages, buildErr := builder.BuildMessages(ctx, key, channel, posts, snapshot, initial, time.Now())
	predecessor := ""
	if latest != nil {
		if last := latest.Latest(); last != nil {
			predecessor = last.ID
		}
	}
	if run == nil {
		if t, body, pred, ok := transfer(snapshot); ok {
			env = t
			env.Revision = w.EffectiveRevision
			messages = body
			predecessor = pred
			buildErr = nil
		}
	}
	e.queued(key, len(env.TriggerIDs))
	if env.Anchor == "" {
		if buildErr == nil && e.Notify != nil {
			for _, post := range posts {
				delay := time.Until(time.UnixMilli(post.CreateAt).Add(w.MessageBatchWindow.Value()))
				if delay > 0 && delay <= w.MessageBatchWindow.Value() {
					time.AfterFunc(delay, func() { e.Notify(key) })
					break
				}
			}
		}
		return buildErr
	}
	if buildErr != nil {
		return e.reject(ctx, p, env, messages, "Message exceeds the input size limit. Split the request.")
	}
	if run != nil {
		// Without optional SDK access, keep the entire batch in Mattermost until
		// before_run can prepare it. Never deliver text promising missing local files.
		if len(env.Request.Files) > 0 && e.Sandbox == nil {
			return nil
		}
		if len(env.Request.Files) > 0 {
			accepted := 0
			for _, message := range current.Messages {
				if message.Role != "user" || message.RunID != run.ID {
					continue
				}
				if !conversation.HasMetadata(message.Metadata) {
					continue
				}
				old, err := conversation.Decode(message.Metadata)
				if err != nil {
					return err
				}
				if old.Kind == "clarification" && len(old.Request.Files) > 0 {
					accepted++
				}
			}
			if accepted >= maxAttachmentClarifications {
				return nil
			}
		}
		env.Kind = "clarification"
	}
	if !initial && latest != nil && run == nil {
		// A failed preparation may never have created its index. Follow only a
		// confirmed successful hook; a subsequently corrupted index still fails.
		for _, prior := range slices.Backward(latest.Runs) {
			prepared := slices.ContainsFunc(prior.Hooks, func(h conversation.Hook) bool {
				return h.Name == "before_run" && h.Status == "completed"
			})
			if !prepared {
				continue
			}
			env.Request.PreviousIndex = attachments.IndexPath(prior.ID)
			for _, m := range latest.Messages {
				if m.Role != "user" || m.RunID != prior.ID || m.Delivery != "delivered" {
					continue
				}
				if !conversation.HasMetadata(m.Metadata) {
					continue
				}
				old, err := conversation.Decode(m.Metadata)
				if err != nil {
					return err
				}
				if old.Kind == "clarification" && len(old.Request.Files) > 0 {
					env.Request.DeliveredBatches = append(env.Request.DeliveredBatches, attachments.ManifestPath(old.Anchor))
				}
			}
			break
		}
	}
	// The complete linked index is bounded by the transport budget; payload is
	// frozen before the first POST, including the renderer and workflow revision.
	frozen := pending{Workflow: w, Envelope: env, Messages: messages, Predecessor: predecessor}
	if run != nil {
		frozen.Session = current.ID
		frozen.Run = run.ID
	} else if !initial {
		frozen.Session = latest.ID
	}
	if run == nil && e.Reserve != nil && !e.Reserve(key, w) {
		return nil
	}
	e.set(key, frozen)
	return e.submit(ctx, key, frozen, current, run)
}

// Keep the next run's delivered-batch delta bounded without paging or truncation.
const maxAttachmentClarifications = 32

func (e *Engine) submit(ctx context.Context, key conversation.Key, p pending, current *conversation.Session, run *conversation.Run) error {
	if p.Run != "" && len(p.Envelope.Request.Files) > 0 {
		if e.Sandbox == nil {
			return nil
		}
		if current == nil || run == nil {
			return agentbox.ErrRunEnded
		}
		if err := e.Sandbox.Prepare(ctx, *current, *run, p.Workflow, p.Envelope.Request); err != nil {
			if errors.Is(err, agentbox.ErrRunEnded) {
				return nil
			}
			return err
		}
	}
	_, err := e.API.Submit(ctx, p.Workflow, p.Envelope, p.Messages, p.Session, p.Run, p.Predecessor)
	if err == nil {
		e.clear(key)
		if e.Notify != nil {
			e.Notify(key)
		}
		return nil
	}
	if orpheus.Code(err) == "run_not_accepting_messages" || errors.Is(err, agentbox.ErrRunEnded) {
		// Keep the frozen input until history proves whether it was accepted.
		return nil
	}
	if code := orpheus.Code(err); code == "request_too_large" || code == "environment_too_large" || code == "validation_error" {
		publisher := &delivery.Publisher{MM: e.MM, Key: key, BotID: e.Bot.ID}
		if readErr := publisher.Refresh(ctx); readErr != nil {
			return readErr
		}
		if err = e.reject(ctx, publisher, p.Envelope, p.Messages, "Orpheus rejected the request: "+code+". Check the request or configuration."); err == nil {
			e.clear(key)
		}
		return err
	}
	return err
}
func (e *Engine) reject(ctx context.Context, p *delivery.Publisher, env conversation.Envelope, messages []conversation.InputMessage, reason string) error {
	text := joinedInput(messages)
	parts := delivery.Parts(p.Key, "", env.Anchor+":"+attachments.Hash([]byte(text)), "input_rejected", reason, env.Render, nil)
	for _, part := range parts {
		part.Receipt.TriggerIDs = env.TriggerIDs
		for _, v := range env.Versions {
			if slices.Contains(env.TriggerIDs, v.ID) {
				part.Receipt.InputVersions = append(part.Receipt.InputVersions, v)
			}
		}
		part.Receipt.InputHash = attachments.Hash([]byte(text))
		part.Receipt.Revision = env.Revision
		if err := p.Publish(ctx, part); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Validate(ctx context.Context) error {
	me, err := e.MM.Me(ctx)
	if err != nil {
		return err
	}
	if !config.ValidID(me.ID) || me.Username == "" {
		return errors.New("invalid Mattermost bot identity")
	}
	e.Bot = me
	e.SourceID = e.Config.Workflows[0].Mattermost.Source(me.ID)
	limit, err := e.MM.MaxPostChars(ctx)
	if err != nil {
		return err
	}
	if limit < 64 {
		return fmt.Errorf("invalid Mattermost post limit")
	}
	e.MaxPostChars = limit
	e.limitPosts(&e.Config)
	return nil
}
func (e *Engine) limitPosts(cfg *config.Config) {
	if e.MaxPostChars <= 0 {
		return
	}
	for i := range cfg.Workflows {
		cfg.Workflows[i].MaxPostChars = min(cfg.Workflows[i].MaxPostChars, e.MaxPostChars)
	}
}
