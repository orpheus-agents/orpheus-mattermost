package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

type fakeAPI struct {
	API
	snapshot  conversation.Snapshot
	submitted []pending
	lost      bool
	cancelled int
}

func (f *fakeAPI) Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error) {
	return f.snapshot, nil
}
func (f *fakeAPI) Cancel(context.Context, string, string) error { f.cancelled++; return nil }
func (f *fakeAPI) Submit(_ context.Context, w config.Workflow, env conversation.Envelope, text, sid, rid, pred string) (conversation.Accepted, error) {
	f.submitted = append(f.submitted, pending{w, env, text, sid, rid, pred})
	if sid == "" {
		sid = uuid.NewString()
		f.snapshot.Sessions = append(f.snapshot.Sessions, conversation.Session{ID: sid, Revision: w.EffectiveRevision, MaxTokens: 1000000})
	}
	s := &f.snapshot.Sessions[len(f.snapshot.Sessions)-1]
	if rid == "" {
		rid = uuid.NewString()
		s.Runs = append(s.Runs, conversation.Run{ID: rid, SessionID: sid, Status: "running", Number: len(s.Runs) + 1, Hooks: []conversation.Hook{{Name: "before_run", Status: "completed"}}})
	}
	mid := uuid.NewString()
	s.Messages = append(s.Messages, conversation.Message{ID: mid, RunID: rid, Role: "user", Text: text, Delivery: "delivered", ExternalKey: env.Key().MessageKey(env.Anchor), Position: len(s.Messages)})
	if f.lost {
		f.lost = false
		return conversation.Accepted{}, errors.New("lost response")
	}
	return conversation.Accepted{SessionID: sid, RunID: rid, MessageID: mid}, nil
}

type fakeSource struct {
	MM
	posts []mattermost.Post
	bot   string
	calls int
}

func (f *fakeSource) Thread(context.Context, string) ([]mattermost.Post, error) { return f.posts, nil }
func (f *fakeSource) User(_ context.Context, id string) (mattermost.User, error) {
	return mattermost.User{ID: id, Username: "human"}, nil
}
func (f *fakeSource) Files(_ context.Context, id string) ([]mattermost.FileInfo, error) {
	var files []mattermost.FileInfo
	for _, post := range f.posts {
		if post.ID == id {
			for _, file := range post.FileIDs {
				files = append(files, mattermost.FileInfo{ID: file, PostID: id, Name: "image.png", MIME: "image/png", Size: 3})
			}
		}
	}
	return files, nil
}
func (f *fakeSource) Typing(context.Context, string, string) error { return nil }
func (f *fakeSource) Create(_ context.Context, p mattermost.CreatePost) (mattermost.Post, error) {
	f.calls++
	raw, _ := json.Marshal(p.Props)
	var props map[string]json.RawMessage
	_ = json.Unmarshal(raw, &props)
	post := mattermost.Post{ID: fmt.Sprintf("%026d", 1000+f.calls), RootID: p.RootID, ChannelID: p.ChannelID, UserID: f.bot, CreateAt: time.Now().UnixMilli(), Message: p.Message, Props: props, FileIDs: p.FileIDs}
	f.posts = append(f.posts, post)
	return post, nil
}

type fakeSandbox struct{ prepared int }

func (f *fakeSandbox) Prepare(context.Context, conversation.Session, conversation.Run, config.Workflow, attachments.Request) error {
	f.prepared++
	return nil
}
func fixture(t *testing.T) (*Engine, *fakeAPI, *fakeSource, *fakeSandbox, conversation.Key) {
	t.Helper()
	t.Setenv("ORPHEUS_BASE_URL", "http://orpheus:8080")
	t.Setenv("WORKFLOWS_DIR", "../../workflows")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workflows[0].Since = time.Unix(0, 0)
	key := conversation.Key{Source: "chat", Workflow: cfg.Workflows[0].ID, Channel: "cccccccccccccccccccccccccc", Root: "rrrrrrrrrrrrrrrrrrrrrrrrrr"}
	mm := &fakeSource{bot: "aaaaaaaaaaaaaaaaaaaaaaaaaa", posts: []mattermost.Post{{ID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus ORIGINAL_REQUEST_BODY", CreateAt: 1000}}}
	api := &fakeAPI{}
	box := &fakeSandbox{}
	engine := &Engine{Config: cfg, MM: mm, API: api, Sandbox: box, Bot: mattermost.User{ID: mm.bot, Username: "orpheus"}}
	return engine, api, mm, box, key
}
func TestLostAdmissionRecoveredFromHistory(t *testing.T) {
	e, api, _, _, key := fixture(t)
	api.lost = true
	w := e.Config.Workflows[0]
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err == nil {
		t.Fatal("expected uncertain POST")
	}
	e = &Engine{Config: e.Config, MM: e.MM, API: api, Sandbox: e.Sandbox, Bot: e.Bot}
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 {
		t.Fatal("duplicate agent run")
	}
	_, body, _ := strings.Cut(api.submitted[0].Text, "\n\n")
	if !strings.Contains(body, "ORIGINAL_REQUEST_BODY") {
		t.Fatal("user body truncated")
	}
}
func TestClarificationPreparesWithoutNewRun(t *testing.T) {
	e, api, mm, box, key := fixture(t)
	w := e.Config.Workflows[0]
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus clarification", CreateAt: 4000})
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if box.prepared != 0 || len(api.snapshot.Sessions[0].Runs) != 1 || api.submitted[1].Envelope.Kind != "clarification" {
		t.Fatal("clarification created a run")
	}
}

func TestAcceptedClarificationStatesSurviveRestart(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus follow-up", CreateAt: 4000})
	api.lost = true
	if err := e.Thread(t.Context(), w, key, ch, true); err == nil {
		t.Fatal("expected lost clarification response")
	}
	for _, status := range []string{"pending", "sending", "uncertain", "delivered"} {
		api.snapshot.Sessions[0].Messages[1].Delivery = status
		e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(status, err)
		}
		if len(api.submitted) != 2 || len(api.snapshot.Sessions[0].Runs) != 1 {
			t.Fatal("accepted clarification resubmitted", status)
		}
	}
	// Exercise the alternative terminal outcome: only proven rejection permits
	// one transfer after normal completion, carrying the original message key.
	message := &api.snapshot.Sessions[0].Messages[1]
	message.Delivery, message.Error = "rejected", "run_finished_before_delivery"
	predecessor := message.ID
	api.snapshot.Sessions[0].Runs[0].Status = "completed"
	for range 2 {
		e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.submitted) != 3 || len(api.snapshot.Sessions[0].Runs) != 2 || api.submitted[2].Envelope.Predecessor != predecessor {
		t.Fatal("rejected clarification was not transferred exactly once")
	}
}
func TestScopeRevocationCancelsAndSuppressesDelivery(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if err := e.Thread(t.Context(), w, key, ch, false); err != nil {
		t.Fatal(err)
	}
	if api.cancelled != 1 || mm.calls != 0 {
		t.Fatal("revocation not respected")
	}
}

func TestFinalizingScopeRevocationSuppressesReadyAnswer(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	s := &api.snapshot.Sessions[0]
	s.Runs[0].Status = "finalizing"
	s.Messages = append(s.Messages, conversation.Message{ID: "answer", RunID: s.Runs[0].ID, Role: "assistant", Kind: "answer", Text: "must not be published", Position: 1})
	if err := e.Thread(t.Context(), w, key, ch, false); err != nil {
		t.Fatal(err)
	}
	s.Runs[0].Status = "completed"
	if err := e.Thread(t.Context(), w, key, ch, false); err != nil {
		t.Fatal(err)
	}
	if api.cancelled != 1 || mm.calls != 0 {
		t.Fatal("revoked channel received an answer")
	}
}

func TestLostAdmissionWithEditedPostAndNewRevisionDoesNotResubmit(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	api.lost = true
	if err := e.Thread(t.Context(), w, key, ch, true); err == nil {
		t.Fatal("expected uncertain POST")
	}
	mm.posts[0].Message = "@orpheus edited request"
	mm.posts[0].EditAt = 2000
	w.EffectiveRevision = "new-revision"
	e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 || !strings.Contains(api.submitted[0].Text, "ORIGINAL_REQUEST_BODY") {
		t.Fatal("accepted envelope replaced after restart")
	}
}
func TestTerminalReplayPublishesOnceBeforeNextRun(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	s := &api.snapshot.Sessions[0]
	s.Runs[0].Status = "completed"
	s.Runs[0].FinalMessageID = uuid.NewString()
	s.Messages = append(s.Messages, conversation.Message{ID: s.Runs[0].FinalMessageID, RunID: s.Runs[0].ID, Role: "assistant", Kind: "answer", Position: 1, Text: "answer"})
	for range 2 {
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 1 || len(api.submitted) != 1 {
		t.Fatal("replay duplicated work")
	}
}
func TestMissingExportHasDurableFailureReceipt(t *testing.T) {
	e, api, mm, box, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	s := &api.snapshot.Sessions[0]
	s.Runs[0].Status = "completed"
	s.Runs[0].AgentStatus = "completed"
	for range 2 {
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	if box.prepared != 0 || mm.calls != 1 {
		t.Fatal("unbounded export retry", box.prepared, mm.calls)
	}
}

func TestUnacceptedClarificationRetargetsAfterSandboxLoss(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	old := api.snapshot.Sessions[0]
	reply := mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus remaining question", CreateAt: 4000}
	mm.posts = append(mm.posts, reply)
	env := api.submitted[0].Envelope
	env.Anchor, env.TriggerIDs, env.Kind = reply.ID, []string{reply.ID}, "clarification"
	text, _ := env.Encode(reply.Message)
	e.set(key, pending{Workflow: w, Envelope: env, Text: text, Session: old.ID, Run: old.Runs[0].ID})
	api.snapshot.Sessions[0].Runs[0].Status = "completed"
	api.snapshot.Sessions[0].SandboxState = "unavailable"
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 2 {
		t.Fatal("input was lost")
	}
	got := api.submitted[1]
	if got.Session != "" || got.Run != "" || got.Envelope.Kind != "initial" || got.Predecessor != old.Runs[0].ID {
		t.Fatal("reused obsolete admission", got.Session, got.Run, got.Predecessor)
	}
}

func TestEditedRejectedInputCanBeAdmitted(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	e.Config.MaxRequestBytes = 4096
	mm.posts[0].Message = "@orpheus " + strings.Repeat("large ", 2000)
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	for range 2 {
		if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.submitted) != 0 || mm.calls != 1 {
		t.Fatal("rejection not durable")
	}
	mm.posts[0].Message = "@orpheus fixed"
	mm.posts[0].UpdateAt = 2000
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 {
		t.Fatal("edited rejected post remains blocked")
	}
}
func TestPendingRunRebuildsAfterTokenExhaustion(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	old := conversation.Session{ID: uuid.NewString(), Revision: w.EffectiveRevision, MaxTokens: 100, TotalTokens: 100, SandboxState: "paused"}
	api.snapshot.Sessions = []conversation.Session{old}
	e.set(key, pending{Workflow: w, Session: old.ID, Envelope: conversation.Envelope{Anchor: mm.posts[0].ID}})
	if err := e.Thread(t.Context(), w, key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 || api.submitted[0].Session != "" || api.submitted[0].Envelope.Request.PreviousIndex != "" {
		t.Fatal("unaccepted input retried against exhausted session")
	}
}

func TestClarificationAttachmentsUseOptionalSDKOrWaitForNextRun(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			e, api, mm, box, key := fixture(t)
			if !enabled {
				e.Sandbox = nil
			}
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus inspect image", CreateAt: 4000, FileIDs: []string{"ffffffffffffffffffffffffff"}})
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if enabled {
				if len(api.submitted) != 2 || box.prepared != 1 || api.submitted[1].Run == "" {
					t.Fatal("attachment clarification not prepared before submission")
				}
				return
			}
			if len(api.submitted) != 1 || box.prepared != 0 {
				t.Fatal("attachment delivered without preparation")
			}
			if _, ok := e.get(key); ok {
				t.Fatal("deferred batch frozen before admission")
			}
			api.snapshot.Sessions[0].Runs[0].Status = "completed"
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 2 || api.submitted[1].Run != "" || len(api.submitted[1].Envelope.Request.Files) != 1 {
				t.Fatal("deferred file lost instead of entering next before_run")
			}
		})
	}
}
func TestTextClarificationNeedsNoSandboxAccess(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	e.Sandbox = nil
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus additional text", CreateAt: 4000})
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 2 || api.submitted[1].Run == "" || len(api.submitted[1].Envelope.Request.Files) != 0 {
		t.Fatal("text clarification unexpectedly needs file preparation")
	}
	api.snapshot.Sessions[0].Runs[0].Status = "completed"
	mm.posts = append(mm.posts, mattermost.Post{ID: "tttttttttttttttttttttttttt", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus next", CreateAt: 7000})
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 3 || len(api.submitted[2].Envelope.Request.DeliveredBatches) != 0 {
		t.Fatal("next index references nonexistent text-only manifest")
	}
}

func TestAttachmentClarificationLimitSurvivesRestartAndDefersWholeBatch(t *testing.T) {
	e, api, mm, box, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	reconcile := func() {
		t.Helper()
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	addPost := func(i int, file bool) {
		post := mattermost.Post{ID: fmt.Sprintf("%026d", i+1), RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus inspect", CreateAt: int64(4000 + i*3000)}
		if file {
			post.FileIDs = []string{fmt.Sprintf("%026d", i+100)}
		}
		mm.posts = append(mm.posts, post)
	}
	reconcile()
	for i := range maxAttachmentClarifications {
		addPost(i, true)
		reconcile()
	}
	if box.prepared != maxAttachmentClarifications || len(api.submitted) != maxAttachmentClarifications+1 {
		t.Fatal("attachment limit reached too early")
	}
	// A restarted connector derives the quota from durable admission history.
	e = &Engine{Config: e.Config, MM: mm, API: api, Sandbox: box, Bot: e.Bot}
	addPost(maxAttachmentClarifications, false)
	reconcile()
	if len(api.submitted) != maxAttachmentClarifications+2 {
		t.Fatal("text-only clarification consumed the attachment quota")
	}
	addPost(maxAttachmentClarifications+1, true)
	deferredID := mm.posts[len(mm.posts)-1].ID
	for range 2 {
		reconcile()
	}
	if box.prepared != maxAttachmentClarifications || len(api.submitted) != maxAttachmentClarifications+2 {
		t.Fatal("over-limit batch prepared or submitted to active run")
	}
	if _, frozen := e.get(key); frozen {
		t.Fatal("over-limit batch frozen before admission")
	}
	api.snapshot.Sessions[0].Runs[0].Status = "completed"
	reconcile()
	last := api.submitted[len(api.submitted)-1]
	if len(api.submitted) != maxAttachmentClarifications+3 || last.Run != "" || last.Envelope.Anchor != deferredID || len(last.Envelope.Request.Files) != 1 {
		t.Fatal("over-limit batch did not enter next run intact")
	}
	if len(last.Envelope.Request.DeliveredBatches) != maxAttachmentClarifications {
		t.Fatal("previously delivered attachments were truncated")
	}
	// The next run has its own quota.
	addPost(maxAttachmentClarifications+2, true)
	reconcile()
	if box.prepared != maxAttachmentClarifications+1 || api.submitted[len(api.submitted)-1].Run == "" {
		t.Fatal("attachment quota did not reset for the next run")
	}
}

func TestRejectedRootDoesNotRetryAfterRepliesOrReactions(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	e.Config.MaxRequestBytes = 4096
	mm.posts[0].Message = "@orpheus " + strings.Repeat("huge", 2000)
	for i := range 5 {
		mm.posts[0].UpdateAt = int64(10000 + i)
		mm.posts[0].Metadata = json.RawMessage(fmt.Sprintf(`{"reply_count":%d}`, i))
		if err := e.Thread(t.Context(), e.Config.Workflows[0], key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.submitted) != 0 || mm.calls != 1 {
		t.Fatal("root rejection repeated", mm.calls)
	}
}
func TestContinuationSkipsRunsWhoseIndexWasNeverPrepared(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			first := api.snapshot.Sessions[0].Runs[0].ID
			if !prepared {
				api.snapshot.Sessions[0].Runs[0].Hooks = nil
			}
			api.snapshot.Sessions[0].Runs[0].Status = "failed"
			api.snapshot.Sessions[0].Runs[0].Error = "before_run_failed"
			for i := range 2 {
				mm.posts = append(mm.posts, mattermost.Post{ID: fmt.Sprintf("%026d", i+1), RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: int64(4000 + i*3000), Message: "@orpheus next"})
				if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
					t.Fatal(err)
				}
				got := api.submitted[len(api.submitted)-1].Envelope.Request.PreviousIndex
				want := ""
				if prepared {
					want = attachments.IndexPath(first)
				}
				if got != want {
					t.Fatal("referenced nonexistent index", got, want)
				}
				last := api.snapshot.Sessions[0].Latest()
				last.Hooks = nil
				last.Status = "failed"
				last.Error = "before_run_failed"
			}
		})
	}
}

type rejectedAPI struct {
	*fakeAPI
	code  string
	calls int
}

func (a *rejectedAPI) Submit(ctx context.Context, w config.Workflow, e conversation.Envelope, text, sid, rid, pred string) (conversation.Accepted, error) {
	a.calls++
	if a.code != "" {
		return conversation.Accepted{}, &orpheus.Error{Status: 422, Code: a.code}
	}
	return a.fakeAPI.Submit(ctx, w, e, text, sid, rid, pred)
}
func TestValidationErrorIsDurablyRejected(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	bad := &rejectedAPI{fakeAPI: api, code: "validation_error"}
	e.API = bad
	for range 3 {
		if err := e.Thread(t.Context(), e.Config.Workflows[0], key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if bad.calls != 1 || mm.calls != 1 {
		t.Fatal("invalid input retried", bad.calls, mm.calls)
	}
	bad.code = ""
	updated := e.Config.Workflows[0]
	updated.EffectiveRevision = "fixed-configuration"
	if err := e.Thread(t.Context(), updated, key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 {
		t.Fatal("fixed configuration still blocked by old rejection")
	}
}
func TestClosedRunWaitsWithoutRetryingFinalizingTarget(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: 4000, Message: "@orpheus next"})
	bad := &rejectedAPI{fakeAPI: api, code: "run_not_accepting_messages"}
	e.API = bad
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	api.snapshot.Sessions[0].Runs[0].Status = "finalizing"
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if bad.calls != 1 {
		t.Fatal("finalizing target retried")
	}
	api.snapshot.Sessions[0].Runs[0].Status = "completed"
	bad.code = ""
	if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 2 || api.submitted[1].Run != "" {
		t.Fatal("frozen clarification lost")
	}
}
