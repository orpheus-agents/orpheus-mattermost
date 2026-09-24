package service

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type runtimeAPI struct {
	API
	count  atomic.Int32
	cancel context.CancelFunc
}

func (*runtimeAPI) Sessions(context.Context, string, string) ([]conversation.Session, error) {
	return nil, nil
}
func (*runtimeAPI) Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error) {
	return conversation.Snapshot{}, nil
}
func (a *runtimeAPI) Submit(ctx context.Context, _ config.Workflow, _ conversation.Envelope, _, _, _, _ string) (conversation.Accepted, error) {
	if a.count.Add(1) == 2 {
		a.cancel()
	}
	<-ctx.Done() // Both threads must reach admission before either can finish.
	return conversation.Accepted{}, nil
}

type runtimeSource struct {
	*fakeSource
	channel string
}

func (s runtimeSource) Me(context.Context) (mattermost.User, error) {
	return mattermost.User{ID: s.bot, Username: "orpheus"}, nil
}
func (runtimeSource) MaxPostChars(context.Context) (int, error) { return 16383, nil }
func (s runtimeSource) Channels(context.Context) ([]mattermost.Channel, error) {
	return []mattermost.Channel{{ID: s.channel, Type: "O"}}, nil
}
func (s runtimeSource) Posts(context.Context, string, time.Time) ([]mattermost.Post, error) {
	return s.posts, nil
}
func (s runtimeSource) Thread(_ context.Context, root string) ([]mattermost.Post, error) {
	var posts []mattermost.Post
	for _, p := range s.posts {
		if p.Root() == root {
			posts = append(posts, p)
		}
	}
	return posts, nil
}
func (runtimeSource) Watch(ctx context.Context, _ func(mattermost.Event)) error {
	<-ctx.Done()
	return ctx.Err()
}
func TestConcurrentReconciliationAndShutdown(t *testing.T) {
	engine, _, mm, _, key := fixture(t)
	second := mm.posts[0]
	second.ID = "tttttttttttttttttttttttttt"
	mm.posts = append(mm.posts, second)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	api := &runtimeAPI{cancel: cancel}
	engine.API = api
	engine.MM = runtimeSource{mm, key.Channel}
	r := &Runtime{Engine: engine}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if api.count.Load() != 2 {
		t.Fatalf("threads did not progress independently: submitted %d inputs", api.count.Load())
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatal("ready after shutdown")
	}
}
func TestReloadTransportIdentityImmutable(t *testing.T) {
	a := config.Config{Orpheus: config.Endpoint{BaseURL: "http://orpheus"}}
	b := a
	b.Orpheus.BaseURL = "http://other"
	if reloadCompatible(a, b) {
		t.Fatal("source identity changed during reload")
	}
}

func TestQueueAndHealthMetrics(t *testing.T) {
	engine, _, _, _, key := fixture(t)
	engine.queued(key, 2)
	engine.set(key, pending{})
	runtime := &Runtime{Engine: engine}
	rec := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "orpheus_mattermost_pending_inputs 2\n") {
		t.Fatal(rec.Body.String())
	}
	inputs, _, uncertain := engine.queues()
	if inputs != 2 || uncertain <= 0 {
		t.Fatal("pending admission was not observed")
	}
	engine.clear(key)
	inputs, _, uncertain = engine.queues()
	if inputs != 0 || uncertain != 0 {
		t.Fatal("confirmed admission remains queued")
	}
	for _, path := range []string{"/health", "/healthz", "/ready", "/readyz"} {
		rec = httptest.NewRecorder()
		runtime.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		want := 200
		if strings.HasPrefix(path, "/ready") {
			want = 503
		}
		if rec.Code != want {
			t.Fatal(path, rec.Code)
		}
	}
}

func TestReloadPreservesServerPostLimit(t *testing.T) {
	e, _, _, _, _ := fixture(t)
	e.MaxPostChars = 4000
	cfg := e.Config
	cfg.Workflows[0].MaxPostChars = 12000
	e.limitPosts(&cfg)
	if cfg.Workflows[0].MaxPostChars != 4000 {
		t.Fatal("reload exceeds server post limit")
	}
}

type schedulerSource struct {
	MM
	mu           sync.Mutex
	posts        []mattermost.Post
	events       chan mattermost.Event
	scans        atomic.Int32
	bot, channel string
}

func (s *schedulerSource) Me(context.Context) (mattermost.User, error) {
	return mattermost.User{ID: s.bot, Username: "orpheus"}, nil
}
func (*schedulerSource) MaxPostChars(context.Context) (int, error) { return 12000, nil }
func (s *schedulerSource) Channels(context.Context) ([]mattermost.Channel, error) {
	return []mattermost.Channel{{ID: s.channel, Type: "O"}}, nil
}
func (s *schedulerSource) Posts(context.Context, string, time.Time) ([]mattermost.Post, error) {
	s.scans.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.posts), nil
}
func (s *schedulerSource) Thread(_ context.Context, root string) ([]mattermost.Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var posts []mattermost.Post
	for _, p := range s.posts {
		if p.Root() == root {
			posts = append(posts, p)
		}
	}
	return posts, nil
}
func (*schedulerSource) User(_ context.Context, id string) (mattermost.User, error) {
	return mattermost.User{ID: id, Username: "human"}, nil
}
func (*schedulerSource) Typing(context.Context, string, string) error { return nil }
func (s *schedulerSource) Watch(ctx context.Context, notify func(mattermost.Event)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-s.events:
			notify(event)
		}
	}
}
func (s *schedulerSource) add(p mattermost.Post) {
	s.mu.Lock()
	s.posts = append(s.posts, p)
	s.mu.Unlock()
	s.events <- mattermost.Event{Post: p}
}

type schedulerAPI struct {
	aborted chan string
	API
	mu        sync.Mutex
	snapshots map[conversation.Key]conversation.Snapshot
	reads     map[string]int
	started   chan string
	slow      string
	release   chan struct{}
}

func (*schedulerAPI) Sessions(context.Context, string, string) ([]conversation.Session, error) {
	return nil, nil
}
func (a *schedulerAPI) Snapshot(_ context.Context, key conversation.Key) (conversation.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads[key.Root]++
	return a.snapshots[key], nil
}
func (*schedulerAPI) Watch(ctx context.Context, _ string, _ func()) error {
	<-ctx.Done()
	return ctx.Err()
}
func (a *schedulerAPI) Submit(ctx context.Context, w config.Workflow, e conversation.Envelope, text, sid, rid, _ string) (conversation.Accepted, error) {
	a.started <- e.Root
	if e.Root == a.slow {
		select {
		case <-ctx.Done():
			if a.aborted != nil {
				a.aborted <- e.Root
			}
			return conversation.Accepted{}, ctx.Err()
		case <-a.release:
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid == "" {
		sid = uuid.NewString()
	}
	if rid == "" {
		rid = uuid.NewString()
	}
	a.snapshots[e.Key()] = conversation.Snapshot{Sessions: []conversation.Session{{ID: sid, Revision: w.EffectiveRevision, MaxTokens: 1000000, Runs: []conversation.Run{{ID: rid, SessionID: sid, Status: "running"}}, Messages: []conversation.Message{{ID: uuid.NewString(), RunID: rid, Role: "user", Text: text, Delivery: "delivered"}}}}}
	return conversation.Accepted{SessionID: sid, RunID: rid}, nil
}
func TestSlowThreadDoesNotBlockNewEventsOrRescanQuietHistory(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	e.Config.MaxParallelThreads = 2
	source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 8), posts: slices.Clone(mm.posts)}
	for i := range 100 {
		source.posts = append(source.posts, mattermost.Post{ID: fmt.Sprintf("%026d", i+1), ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: 1000, Message: "unrelated chat"})
	}
	api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 8), slow: key.Root, release: make(chan struct{})}
	e.API = api
	e.MM = source
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	r := &Runtime{Engine: e}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	await := func(want string) {
		t.Helper()
		select {
		case got := <-api.started:
			if got != want {
				t.Fatal("unexpected admission", got)
			}
		case <-ctx.Done():
			t.Fatal("thread event blocked behind unrelated work")
		}
	}
	await(key.Root)
	fast := "tttttttttttttttttttttttttt"
	source.add(mattermost.Post{ID: fast, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: 4000, Message: "@orpheus fast"})
	await(fast)
	if source.scans.Load() != 1 {
		t.Fatal("thread event rescanned source history")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.reads[key.Root] != 1 {
		t.Fatal("duplicate snapshot before admission", api.reads[key.Root])
	}
	for i := range 100 {
		if api.reads[fmt.Sprintf("%026d", i+1)] != 0 {
			t.Fatal("quiet thread entered reconciliation")
		}
	}
}

func TestBatchWakeSurvivesCompletionOfDiscoveryWork(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	e.Config.Workflows[0].MessageBatchWindow = config.Duration(200 * time.Millisecond)
	mm.posts[0].CreateAt = time.Now().UnixMilli()
	source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1), posts: slices.Clone(mm.posts)}
	api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 8)}
	e.API = api
	e.MM = source
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- (&Runtime{Engine: e}).Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-api.started:
		if time.Since(time.UnixMilli(mm.posts[0].CreateAt)) < 200*time.Millisecond {
			t.Fatal("batch admitted before window closed")
		}
	case <-ctx.Done():
		t.Fatal("batch wake lost when previous thread worker returned")
	}
}

func TestReloadRevokesInflightThreadRequest(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1), posts: slices.Clone(mm.posts)}
	api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 8), aborted: make(chan string, 1), slow: key.Root, release: make(chan struct{})}
	e.API = api
	e.MM = source
	reload := make(chan config.Config, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- (&Runtime{Engine: e, Reload: reload}).Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-api.started:
	case <-ctx.Done():
		t.Fatal("initial request did not start")
	}
	cfg := e.Config
	cfg.Workflows = slices.Clone(cfg.Workflows)
	cfg.Workflows[0].ExcludeIDs = []string{key.Channel}
	reload <- cfg
	select {
	case <-api.aborted:
	case <-ctx.Done():
		t.Fatal("revoked thread kept inflight request")
	}
	select {
	case <-api.started:
		t.Fatal("request retried after scope revocation")
	case <-time.After(100 * time.Millisecond):
	}
}
func TestSchedulerHonorsRunCapacityWithoutBusyRetry(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	e.Config.Workflows[0].MaxConcurrentRuns = 1
	posts := slices.Clone(mm.posts)
	second := posts[0]
	second.ID = "tttttttttttttttttttttttttt"
	posts = append(posts, second)
	source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1), posts: posts}
	api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 8)}
	e.API = api
	e.MM = source
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- (&Runtime{Engine: e}).Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-api.started:
	case <-ctx.Done():
		t.Fatal("no run admitted")
	}
	select {
	case <-api.started:
		t.Fatal("run capacity exceeded")
	case <-time.After(150 * time.Millisecond):
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, reads := range api.reads {
		if reads > 5 {
			t.Fatal("capacity wait caused busy retry", reads)
		}
	}
}
