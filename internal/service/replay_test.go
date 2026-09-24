package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type replaySource struct {
	*schedulerSource
	created int
}

func (s *replaySource) Create(_ context.Context, p mattermost.CreatePost) (mattermost.Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	raw, _ := json.Marshal(p.Props)
	var props map[string]json.RawMessage
	_ = json.Unmarshal(raw, &props)
	post := mattermost.Post{ID: fmt.Sprintf("%026d", 10000+s.created), RootID: p.RootID, ChannelID: p.ChannelID, UserID: s.bot, Message: p.Message, Props: props, CreateAt: time.Now().UnixMilli()}
	s.posts = append(s.posts, post)
	return post, nil
}

type replayAPI struct {
	API
	sessions  []conversation.Session
	snapshots map[conversation.Key]conversation.Snapshot
	submits   atomic.Int32
}

func (a *replayAPI) Sessions(context.Context, string, string) ([]conversation.Session, error) {
	return a.sessions, nil
}
func (a *replayAPI) Snapshot(_ context.Context, key conversation.Key) (conversation.Snapshot, error) {
	return a.snapshots[key], nil
}
func (a *replayAPI) Submit(context.Context, config.Workflow, conversation.Envelope, string, string, string, string) (conversation.Accepted, error) {
	a.submits.Add(1)
	return conversation.Accepted{}, fmt.Errorf("replay must not create a new run")
}

// Measures local reconciliation, not network latency or a production SLA.
// All source messages predate this process; WebSocket/SSE never emit a hint.
func TestReplayRecovery1000Threads(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.PollInterval = config.Duration(time.Hour)
	e.Config.Workflows[0] = w
	key.Source = w.Mattermost.Source(mm.bot)
	source := &replaySource{schedulerSource: &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event)}}
	api := &replayAPI{snapshots: map[conversation.Key]conversation.Snapshot{}}
	for thread := range 1000 {
		key.Root = fmt.Sprintf("%026d", thread*10+1)
		for post := range 10 {
			p := mattermost.Post{ID: fmt.Sprintf("%026d", thread*10+post+1), ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: 1000, Message: "old passive context"}
			if post != 0 {
				p.RootID = key.Root
			} else if thread < 250 {
				p.Message = "@orpheus accepted request"
			}
			source.posts = append(source.posts, p)
		}
		if thread >= 250 {
			continue
		}
		env := conversation.Envelope{Schema: 1, Source: key.Source, Workflow: key.Workflow, Revision: w.EffectiveRevision, Channel: key.Channel, Root: key.Root, Anchor: key.Root, Kind: "initial", TriggerIDs: []string{key.Root}, Render: conversation.Render{Version: 2, MaxChars: 12000}}
		body, err := env.Encode("accepted request")
		if err != nil {
			t.Fatal(err)
		}
		sid, rid, mid := uuid.NewString(), uuid.NewString(), uuid.NewString()
		s := conversation.Session{ID: sid, ExternalKey: key.External(), Revision: w.EffectiveRevision, MaxTokens: 1000000,
			Runs:     []conversation.Run{{ID: rid, SessionID: sid, Status: "completed", FinalMessageID: mid}},
			Messages: []conversation.Message{{ID: uuid.NewString(), RunID: rid, Role: "user", Delivery: "delivered", Text: body, ExternalKey: key.MessageKey(key.Root)}, {ID: mid, RunID: rid, Role: "assistant", Kind: "answer", Text: "recovered answer", Position: 1}}}
		api.sessions = append(api.sessions, s)
		api.snapshots[key] = conversation.Snapshot{Sessions: []conversation.Session{s}}
	}
	for pass := range 2 {
		// Only external state survives; discard all scheduler and engine caches.
		engine := &Engine{Config: e.Config, MM: source, API: api, Bot: e.Bot}
		runtime := &Runtime{Engine: engine}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		done := make(chan error, 1)
		started := time.Now()
		go func() { done <- runtime.Run(ctx) }()
		var recovered bool
		for ctx.Err() == nil {
			runtime.mu.Lock()
			idle := runtime.initialized && !runtime.scanning && runtime.running == 0 && len(runtime.jobs) == 0
			runtime.mu.Unlock()
			if idle && runtime.ready.Load() {
				recovered = true
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond):
			}
		}
		elapsed := time.Since(started)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !recovered || source.created != 250 || api.submits.Load() != 0 || runtime.failures.Load() != 0 {
			t.Fatalf("pass %d: recovered=%v posts=%d new inputs=%d failures=%d", pass, recovered, source.created, api.submits.Load(), runtime.failures.Load())
		}
		t.Logf("pass %d: 1000 threads, 10000 source posts, 250 sessions; recovery=%s, reconciliations=%d, total published=%d", pass+1, elapsed, runtime.cycles.Load(), source.created)
	}
}
