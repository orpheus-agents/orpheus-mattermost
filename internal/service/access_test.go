package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	api "github.com/orpheus-agents/orpheus/client"
)

type accessSource struct {
	*fakeSource
	channel          mattermost.Channel
	audience         []string
	reads, fileReads int
	fail             bool
	users            map[string]mattermost.User
}

func (s *accessSource) Audience(context.Context, string) ([]string, error) {
	s.reads++
	if s.fail {
		return nil, errors.New("audience unavailable")
	}
	return slices.Clone(s.audience), nil
}
func (s *accessSource) Channel(context.Context, string) (mattermost.Channel, error) {
	return s.channel, nil
}
func (s *accessSource) User(ctx context.Context, id string) (mattermost.User, error) {
	if user, ok := s.users[id]; ok {
		return user, nil
	}
	return s.fakeSource.User(ctx, id)
}
func (s *accessSource) Files(ctx context.Context, id string) ([]mattermost.FileInfo, error) {
	s.fileReads++
	return s.fakeSource.Files(ctx, id)
}

type accessAPI struct {
	*fakeAPI
	failure  error
	attempts []pending
}

func (a *accessAPI) Submit(ctx context.Context, w config.Workflow, env conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	a.attempts = append(a.attempts, pending{Workflow: w, Envelope: env, Messages: messages, Session: sid, Run: rid, Predecessor: pred})
	if a.failure != nil {
		err := a.failure
		a.failure = nil
		return conversation.Accepted{}, err
	}
	return a.fakeAPI.Submit(ctx, w, env, messages, sid, rid, pred)
}
func protectedFixture(t *testing.T) (*Engine, *accessAPI, *accessSource, *fakeSandbox, conversation.Key, config.Workflow) {
	t.Helper()
	e, a, mm, box, key := fixture(t)
	w := e.Config.Workflows[0]
	w.Access = &config.Access{Namespace: "mattermost:test", BotID: e.Bot.ID,
		ScopeContext: map[string]any{"workspace": "test"},
		IdentityProjection: config.IdentityProjection{ActorField: "requester", AudienceField: "readers", Subjects: map[string]config.SubjectID{
			mm.posts[0].UserID:           config.SubjectID(`42`),
			"nnnnnnnnnnnnnnnnnnnnnnnnnn": config.SubjectID(`77`),
		}},
	}
	w.IncludeIDs = []string{key.Channel}
	w.Links.Enabled = new(false)
	source := &accessSource{fakeSource: mm, channel: mattermost.Channel{ID: key.Channel, Type: "P"}, audience: []string{mm.bot, mm.posts[0].UserID}}
	api := &accessAPI{fakeAPI: a}
	e.MM, e.API = source, api
	return e, api, source, box, key, w
}
func completeAccessRun(a *accessAPI) {
	s := &a.snapshot.Sessions[len(a.snapshot.Sessions)-1]
	s.Runs[len(s.Runs)-1].Status = "completed"
}
func appendAccessPost(s *accessSource, key conversation.Key, id, author, text string, at int64, files ...string) {
	s.posts = append(s.posts, mattermost.Post{ID: id, RootID: key.Root, ChannelID: key.Channel, UserID: author, Message: text, CreateAt: at, FileIDs: files})
}
func TestProtectedAdmissionFrozenAndRecovered(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.failure = &orpheus.Error{Status: 429, Code: "capacity_exceeded"}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("capacity error lost")
	}
	first := a.attempts[0]
	mm.audience = append(mm.audience, "nnnnnnnnnnnnnnnnnnnnnnnnnn")
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if mm.reads != 1 || !reflect.DeepEqual(first.Envelope, a.attempts[1].Envelope) {
		t.Fatal("retry refreshed snapshot")
	}
	// Restart after acceptance recovers through authoritative history, even if MM
	// membership is unavailable. It must not issue the same invocation again.
	e.pending = nil
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 2 || mm.reads != 1 {
		t.Fatal("duplicate accepted run")
	}
}
func TestProtectedAdmissionWaitsForSlot(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	e.Reserve = func(conversation.Key, config.Workflow) bool { return false }
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if mm.reads != 0 || len(a.attempts) != 0 {
		t.Fatal("snapshot taken while waiting")
	}
	e.Reserve = nil
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil || len(a.attempts) != 0 {
		t.Fatal("failed audience admitted")
	}
}
func TestActiveAuthorFilteredBeforeFiles(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	outsider := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", outsider, "@orpheus outsider", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || box.prepared != 0 || mm.fileReads != 0 {
		t.Fatal("outsider file crossed active boundary")
	}
	appendAccessPost(mm, key, "tttttttttttttttttttttttttt", mm.audience[1], "@orpheus allowed", 5000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	submitted := a.attempts[len(a.attempts)-1]
	if submitted.Run == "" || len(submitted.Envelope.InputSubjects) != len(submitted.Messages) || strings.Contains(joinedInput(submitted.Messages), "outsider") || mm.reads != 1 {
		t.Fatal("invalid active batch")
	}
	completeAccessRun(a)
	mm.audience = append(mm.audience, outsider)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if a.attempts[len(a.attempts)-1].Envelope.Anchor != "ssssssssssssssssssssssssss" || mm.reads != 2 {
		t.Fatal("excluded trigger did not remain pending")
	}
}
func TestScopeChangedCreatesCleanSessionWithSameInvocation(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "pppppppppppppppppppppppppp", mm.audience[1], "OLD_PRIVATE_CONTEXT", 2000, "ffffffffffffffffffffffffff")
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus current", 4000, "gggggggggggggggggggggggggg")
	a.failure = &orpheus.Error{Status: 409, Code: "service_scope_changed"}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	before, after := a.attempts[1], a.attempts[2]
	if before.Session == "" || after.Session != "" || !reflect.DeepEqual(before.Envelope.AccessContext, after.Envelope.AccessContext) || !reflect.DeepEqual(before.Envelope.AccessProof, after.Envelope.AccessProof) || mm.reads != 2 {
		t.Fatal("scope reset reissued identity")
	}
	if after.Envelope.Request.PreviousIndex != "" || len(after.Envelope.Request.DeliveredBatches) != 0 || strings.Contains(joinedInput(after.Messages), "OLD_PRIVATE_CONTEXT") || len(after.Envelope.Request.Files) != 1 || after.Envelope.ContextStart.PostID != after.Envelope.Anchor {
		t.Fatal("old context crossed reset", after.Envelope)
	}
	// A subsequent process inherits the new history boundary, even after deletion
	// of the boundary post; only the new session remains reusable.
	completeAccessRun(a)
	e.pending = nil
	mm.posts = slices.DeleteFunc(mm.posts, func(p mattermost.Post) bool { return p.ID == after.Envelope.Anchor })
	appendAccessPost(mm, key, "tttttttttttttttttttttttttt", mm.audience[1], "@orpheus later", 6000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	last := a.attempts[len(a.attempts)-1]
	if strings.Contains(joinedInput(last.Messages), "OLD_PRIVATE_CONTEXT") || last.Envelope.ContextStart.PostID != after.Envelope.Anchor {
		t.Fatal("boundary lost on restart")
	}
}
func TestOnlyScopeConflictResets(t *testing.T) {
	for _, failure := range []*orpheus.Error{{Status: 503, Code: "service_scope_changed"}, {Status: 409, Code: "session_busy"}} {
		t.Run(failure.Code+string(rune(failure.Status)), func(t *testing.T) {
			e, a, mm, _, key, w := protectedFixture(t)
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
				t.Fatal(err)
			}
			completeAccessRun(a)
			appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus next", 4000)
			a.failure = failure
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
				t.Fatal("failure swallowed")
			}
			if len(a.attempts) != 2 || len(a.snapshot.Sessions) != 1 {
				t.Fatal("unexpected new session")
			}
		})
	}
}
func TestLostProtectedAdmissionDoesNotReset(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.lost = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("lost response not simulated")
	}
	e.pending = nil
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || len(a.snapshot.Sessions) != 1 {
		t.Fatal("duplicate invocation")
	}
}
func TestUnknownProvenanceStartsClean(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	unprotected := w
	unprotected.Access = nil
	if err := e.Thread(t.Context(), unprotected, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus next", 4000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	last := a.attempts[1]
	if last.Session != "" || len(last.Messages) != 1 || last.Envelope.Request.PreviousIndex != "" {
		t.Fatal("unknown memory resumed")
	}
}

func TestAnotherAuthorKeepsCompatibleSession(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	other := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	mm.audience = append(mm.audience, other)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", other, "@orpheus continue", 4000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.snapshot.Sessions) != 1 || a.attempts[1].Session != a.snapshot.Sessions[0].ID || a.attempts[1].Envelope.AccessContext.Subject.ID != other || a.attempts[1].Envelope.AccessContext.InvocationID == a.attempts[0].Envelope.AccessContext.InvocationID {
		t.Fatal("author change lost compatible session")
	}
}

func TestNoMentionDoesNotStartProtectedRun(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	mm.posts[0].Message = "passive context"
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 0 || mm.reads != 0 {
		t.Fatal("passive post started run")
	}
}

func TestProtectedActiveFileKeepsOriginalSnapshot(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	mm.fail = true
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus file", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if box.prepared != 1 || mm.reads != 1 || !reflect.DeepEqual(a.attempts[0].Envelope.AccessContext, a.attempts[1].Envelope.AccessContext) || len(a.attempts[1].Envelope.InputSubjects) != 1 {
		t.Fatal("active input changed its run snapshot")
	}
}

func TestAdmissionKeepsProviderScopeAndInputAuthoritySeparate(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	env := a.attempts[0].Envelope
	access := env.AccessContext
	raw, err := json.Marshal(access.ScopeContext)
	if err != nil || string(raw) != `{"readers":[42],"requester":42,"workspace":"test"}` {
		t.Fatalf("unexpected provider projection: %s, %v", raw, err)
	}
	if access.AllowedInputSubjects == nil || len(*access.AllowedInputSubjects) != len(mm.audience) || !slices.Equal(env.AccessProof.AudienceSubjects, mm.audience) {
		t.Fatal("missing explicit input authority or transport proof")
	}
	for _, id := range mm.audience {
		if !slices.Contains(*access.AllowedInputSubjects, api.AccessSubject{Namespace: w.Access.Namespace, ID: id}) {
			t.Fatal("incomplete input authority")
		}
	}
	mapped := w.Access.IdentityProjection.Subjects[mm.audience[1]]
	mapped[0] = '5'
	if string(access.ScopeContext["requester"]) != "42" {
		t.Fatal("frozen scope shares identity mapping storage")
	}
	mapped[0] = '4'
	mm.audience[0] = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	if err := validateProtectedContext(env, w); err != nil {
		t.Fatal("snapshot retained mutable source data", err)
	}
}

func TestInvalidRestoredAuthorityBlocksFilesAndStartsNextRunClean(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*conversation.Envelope)
	}{
		{"legacy missing input authority", func(e *conversation.Envelope) { e.AccessContext.AllowedInputSubjects = nil }},
		{"legacy missing transport proof", func(e *conversation.Envelope) { e.AccessProof = nil }},
		{"empty input authority", func(e *conversation.Envelope) { e.AccessContext.AllowedInputSubjects = new([]api.AccessSubject{}) }},
		{"incomplete input authority", func(e *conversation.Envelope) {
			e.AccessContext.AllowedInputSubjects = new((*e.AccessContext.AllowedInputSubjects)[:1])
		}},
		{"duplicate input authority", func(e *conversation.Envelope) {
			(*e.AccessContext.AllowedInputSubjects)[0] = (*e.AccessContext.AllowedInputSubjects)[1]
		}},
		{"input namespace mismatch", func(e *conversation.Envelope) { (*e.AccessContext.AllowedInputSubjects)[1].Namespace = "telegram:test" }},
		{"input author outside audience", func(e *conversation.Envelope) {
			(*e.AccessContext.AllowedInputSubjects)[1].ID = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
		}},
		{"subject namespace mismatch", func(e *conversation.Envelope) { e.AccessContext.Subject.Namespace = "mattermost:other" }},
		{"subject outside audience", func(e *conversation.Envelope) { e.AccessContext.Subject.ID = "nnnnnnnnnnnnnnnnnnnnnnnnnn" }},
		{"missing observation", func(e *conversation.Envelope) { e.AccessContext.ContextObservedAt = time.Time{} }},
		{"missing scope", func(e *conversation.Envelope) { e.AccessContext.ScopeContext = nil }},
		{"unconfigured scope field", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["unconfigured"] = json.RawMessage(`[]`) }},
		{"missing actor", func(e *conversation.Envelope) { delete(e.AccessContext.ScopeContext, "requester") }},
		{"wrong actor type", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["requester"] = json.RawMessage(`"42"`) }},
		{"wrong actor", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["requester"] = json.RawMessage(`77`) }},
		{"wrong static context", func(e *conversation.Envelope) {
			e.AccessContext.ScopeContext["workspace"] = json.RawMessage(`"another"`)
		}},
		{"malformed service audience", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["readers"] = json.RawMessage(`{}`) }},
		{"null service audience", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["readers"] = json.RawMessage(`null`) }},
		{"duplicate service readers", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["readers"] = json.RawMessage(`[42,42]`) }},
		{"expanded service readers", func(e *conversation.Envelope) { e.AccessContext.ScopeContext["readers"] = json.RawMessage(`[42,77]`) }},
		{"missing projection digest", func(e *conversation.Envelope) { e.AccessProof.ProjectionDigest = "" }},
		{"changed projection digest", func(e *conversation.Envelope) { e.AccessProof.ProjectionDigest = strings.Repeat("0", 64) }},
		{"empty transport audience", func(e *conversation.Envelope) { e.AccessProof.AudienceSubjects = nil }},
		{"invalid transport audience", func(e *conversation.Envelope) { e.AccessProof.AudienceSubjects[0] = "bad" }},
		{"duplicate transport audience", func(e *conversation.Envelope) { e.AccessProof.AudienceSubjects[0] = e.AccessProof.AudienceSubjects[1] }},
		{"missing own bot", func(e *conversation.Envelope) { e.AccessProof.AudienceSubjects = e.AccessProof.AudienceSubjects[1:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, a, mm, box, key, w := protectedFixture(t)
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
				t.Fatal(err)
			}
			session := &a.snapshot.Sessions[0]
			initial := &session.Messages[len(session.Messages)-1]
			env, err := conversation.Decode(initial.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&env)
			initial.Metadata = testMetadata(env)
			appendAccessPost(mm, key, "pppppppppppppppppppppppppp", mm.audience[1], "OLD_PRIVATE_CONTEXT", 2000, "ffffffffffffffffffffffffff")
			appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus next", 4000, "gggggggggggggggggggggggggg")
			e.pending = nil
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
				t.Fatal("invalid recovered authority admitted")
			}
			if len(a.attempts) != 1 || box.prepared != 0 || mm.fileReads != 0 || mm.reads != 1 {
				t.Fatal("invalid authority reached context, files or new admission")
			}
			completeAccessRun(a)
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
				t.Fatal(err)
			}
			next := a.attempts[len(a.attempts)-1]
			if len(a.snapshot.Sessions) != 2 || next.Session != "" || next.Envelope.ContextStart.PostID != next.Envelope.Anchor || next.Envelope.Request.PreviousIndex != "" || len(next.Envelope.Request.Files) != 1 || strings.Contains(joinedInput(next.Messages), "OLD_PRIVATE_CONTEXT") || strings.Contains(joinedInput(next.Messages), "ORIGINAL_REQUEST_BODY") {
				t.Fatal("untrusted context crossed clean boundary")
			}
		})
	}
}

func TestReorderedExplicitInputAuthorityPreservesSession(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	initial := &a.snapshot.Sessions[0].Messages[0]
	env, err := conversation.Decode(initial.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(*env.AccessContext.AllowedInputSubjects)
	initial.Metadata = testMetadata(env)
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus allowed", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.snapshot.Sessions) != 1 || len(a.attempts) != 2 || box.prepared != 1 || mm.reads != 1 {
		t.Fatal("input list ordering changed authority")
	}
}

func TestAdmissionRejectsUnmappedOrUnconfirmedReaders(t *testing.T) {
	for name, change := range map[string]func(*accessSource, *config.Access){
		"missing actor mapping":  func(s *accessSource, a *config.Access) { delete(a.IdentityProjection.Subjects, s.posts[0].UserID) },
		"unmapped silent reader": func(s *accessSource, a *config.Access) { s.audience = append(s.audience, "zzzzzzzzzzzzzzzzzzzzzzzzzz") },
		"duplicate audience":     func(s *accessSource, a *config.Access) { s.audience = append(s.audience, s.audience[1]) },
		"missing own bot":        func(s *accessSource, a *config.Access) { s.audience = s.audience[1:] },
		"actor outside audience": func(s *accessSource, a *config.Access) { s.audience[1] = "nnnnnnnnnnnnnnnnnnnnnnnnnn" },
		"mapped other bot": func(s *accessSource, a *config.Access) {
			id := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			s.audience = append(s.audience, id)
			s.users = map[string]mattermost.User{id: {ID: id, IsBot: true}}
		},
		"deleted reader": func(s *accessSource, a *config.Access) {
			id := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			s.audience = append(s.audience, id)
			s.users = map[string]mattermost.User{id: {ID: id, DeleteAt: 1}}
		},
		"unconfirmed reader": func(s *accessSource, a *config.Access) {
			id := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			s.audience = append(s.audience, id)
			s.users = map[string]mattermost.User{id: {ID: s.audience[1]}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, a, mm, box, key, w := protectedFixture(t)
			change(mm, w.Access)
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
				t.Fatal("invalid audience admitted")
			}
			if len(a.attempts) != 0 || box.prepared != 0 {
				t.Fatal("invalid audience reached execution")
			}
		})
	}
}

func TestProjectionSupportsServiceStringsAndLargeIntegers(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	other := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	mm.audience = append(mm.audience, other)
	w.Access.IdentityProjection.ActorField = "requester_id"
	w.Access.IdentityProjection.AudienceField = "visible_to"
	w.Access.IdentityProjection.Subjects[mm.audience[1]] = config.SubjectID(`9007199254740993`)
	w.Access.IdentityProjection.Subjects[other] = config.SubjectID(`"person-77"`)
	w.Access.ScopeContext = map[string]any{"project": map[string]any{"id": 123, "rules": []string{"read"}}}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(a.attempts[0].Envelope.AccessContext.ScopeContext)
	if err != nil || string(raw) != `{"project":{"id":123,"rules":["read"]},"requester_id":9007199254740993,"visible_to":[9007199254740993,"person-77"]}` {
		t.Fatalf("service projection changed identity: %s, %v", raw, err)
	}
}

func TestMappingChangeDoesNotReinterpretActiveSnapshot(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	changed := w
	changed.Access = new(*w.Access)
	changed.Access.IdentityProjection.Subjects = maps.Clone(w.Access.IdentityProjection.Subjects)
	changed.Access.IdentityProjection.Subjects[mm.audience[1]] = config.SubjectID(`43`)
	changed.EffectiveRevision += ":new-projection"
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus new projection", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), changed, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || box.prepared != 0 || mm.fileReads != 0 || mm.reads != 1 {
		t.Fatal("changed mapping altered active run")
	}
	completeAccessRun(a)
	if err := e.Thread(t.Context(), changed, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	next := a.attempts[len(a.attempts)-1]
	if len(a.snapshot.Sessions) != 2 || next.Session != "" || next.Envelope.ContextStart.PostID != next.Envelope.Anchor || next.Envelope.Request.PreviousIndex != "" || strings.Contains(joinedInput(next.Messages), "ORIGINAL_REQUEST_BODY") {
		t.Fatal("changed mapping resumed previous memory")
	}
	if string(next.Envelope.AccessContext.ScopeContext["requester"]) != "43" || string(a.attempts[0].Envelope.AccessContext.ScopeContext["requester"]) != "42" || next.Envelope.AccessProof.ProjectionDigest == a.attempts[0].Envelope.AccessProof.ProjectionDigest {
		t.Fatal("mapping snapshot was not frozen")
	}
}
