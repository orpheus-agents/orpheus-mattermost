package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type permanentSource struct {
	*schedulerSource
	err      error
	attempts atomic.Int32
}

func (s *permanentSource) Create(context.Context, mattermost.CreatePost) (mattermost.Post, error) {
	s.attempts.Add(1)
	return mattermost.Post{}, s.err
}

type discoveredAPI struct{ *schedulerAPI }

func (a discoveredAPI) Sessions(context.Context, string, string) ([]conversation.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var sessions []conversation.Session
	for key, snapshot := range a.snapshots {
		for _, session := range snapshot.Sessions {
			sessions = append(sessions, conversation.Session{ID: session.ID, ExternalKey: key.External()})
		}
	}
	return sessions, nil
}

func TestPermanentThreadFailureKeepsWorkflowSchedulable(t *testing.T) {
	for _, status := range []int{400, 404, 413} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, seed, mm, _, key := fixture(t)
				w := e.Config.Workflows[0]
				if err := e.Thread(t.Context(), w, key, mattermost.Channel{Type: "O"}, true); err != nil {
					t.Fatal(err)
				}
				session := &seed.snapshot.Sessions[0]
				run := &session.Runs[0]
				run.Status, run.FinalMessageID = "completed", "final"
				session.Messages = append(session.Messages, conversation.Message{ID: "final", RunID: run.ID, Role: "assistant", Kind: "answer", Text: "result", Position: 1})
				e.SourceID = key.Source
				e.Config.Workflows[0].PollInterval = config.Duration(time.Second)
				e.Config.Workflows[0].FullReconcileInterval = config.Duration(time.Second)
				e.Config.Workflows[0].MaxConcurrentRuns = 1
				source := &permanentSource{schedulerSource: &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 8), posts: mm.posts}, err: &mattermost.HTTPError{Status: status}}
				api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{key: seed.snapshot}, reads: map[string]int{}, started: make(chan string, 8)}
				e.MM, e.API = source, discoveredAPI{api}
				reload := make(chan config.Config, 1)
				r := &Runtime{Engine: e, Reload: reload, Logger: slog.New(slog.DiscardHandler)}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
				synctest.Wait()
				if source.attempts.Load() != 1 {
					t.Fatal("initial publication failure was not exercised")
				}
				// Cross several full discoveries and the initial failure's backoff.
				time.Sleep(5 * time.Second)
				synctest.Wait()
				r.mu.Lock()
				blocked, unknown := r.jobs[key].Retry.Permanent, r.unknown[key]
				r.mu.Unlock()
				if !blocked || unknown || source.attempts.Load() != 1 {
					t.Fatalf("failed thread was retried or left unknown: permanent=%v unknown=%v attempts=%d", blocked, unknown, source.attempts.Load())
				}
				// Without a full discovery, a terminal blocked thread stays idle.
				r.mu.Lock()
				r.cfg.Workflows = slices.Clone(r.cfg.Workflows)
				r.cfg.Workflows[0].FullReconcileInterval = config.Duration(time.Hour)
				r.mu.Unlock()
				api.mu.Lock()
				reads := api.reads[key.Root]
				api.mu.Unlock()
				time.Sleep(3 * time.Second)
				synctest.Wait()
				api.mu.Lock()
				idleReads := api.reads[key.Root]
				api.mu.Unlock()
				if idleReads != reads {
					t.Fatal("terminal blocked thread kept polling")
				}
				// An active run still needs polling even without SSE or full discovery.
				api.mu.Lock()
				api.snapshots[key].Sessions[0].Runs[0].Status = "running"
				api.mu.Unlock()
				r.hint(key)
				synctest.Wait()
				time.Sleep(3 * time.Second)
				synctest.Wait()
				api.mu.Lock()
				activeReads := api.reads[key.Root]
				api.snapshots[key].Sessions[0].Runs[0].Status = "completed"
				api.mu.Unlock()
				if activeReads <= idleReads+1 {
					t.Fatal("active blocked thread stopped polling")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				fresh := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
				source.add(mattermost.Post{ID: fresh, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: time.Now().UnixMilli(), Message: "@orpheus new request"})
				synctest.Wait()
				select {
				case root := <-api.started:
					if root != fresh {
						t.Fatal("unexpected admitted thread", root)
					}
				default:
					t.Fatal("permanent publication failure blocked another thread")
				}
				// An SSE hint must observe state without reattempting publication.
				r.hint(key)
				synctest.Wait()
				if source.attempts.Load() != 1 {
					t.Fatal("notification retried permanent publication")
				}
				// Reload remains the explicit way to resume reconciliation.
				reload <- e.Config
				synctest.Wait()
				if source.attempts.Load() != 2 {
					t.Fatal("reload did not resume reconciliation")
				}
			})
		})
	}
}

func observationRuntime(e *Engine, key conversation.Key) *Runtime {
	return &Runtime{
		Engine: e, Logger: slog.New(slog.DiscardHandler), initialized: true,
		jobs:      map[conversation.Key]*scheduled{key: {Retry: retry{Permanent: true}}},
		active:    map[conversation.Key]int{},
		unknown:   map[conversation.Key]bool{key: true},
		watchers:  map[string]context.CancelFunc{},
		watchKeys: map[string]conversation.Key{},
		wake:      make(chan struct{}, 1),
	}
}

func runThreadWork(t *testing.T, r *Runtime, wg *sync.WaitGroup, key conversation.Key) {
	t.Helper()
	r.jobs[key].Working, r.jobs[key].Dirty, r.running = true, false, 1
	r.work(t.Context(), t.Context(), wg, key, r.Engine.Config.Workflows[0], mattermost.Channel{Type: "O"}, true)
}

func TestPermanentThreadObservationRetainsRunCapacity(t *testing.T) {
	e, _, _, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.MaxConcurrentRuns = 1
	e.Config.Workflows[0] = w
	snapshot := conversation.Snapshot{Sessions: []conversation.Session{{ID: "session", Runs: []conversation.Run{{ID: "run", Status: "running"}}}}}
	api := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{key: snapshot}, reads: map[string]int{}}
	e.API = api
	r := observationRuntime(e, key)
	fresh := key
	fresh.Root = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	r.jobs[fresh] = &scheduled{}
	var wg sync.WaitGroup
	defer func() {
		for _, cancel := range r.watchers {
			cancel()
		}
		wg.Wait()
	}()
	runThreadWork(t, r, &wg, key)
	if r.unknown[key] || r.active[key] != 1 || len(r.watchers) != 1 || r.reserve(fresh, w) {
		t.Fatal("active run in a blocked thread lost its reserved capacity")
	}
	api.mu.Lock()
	api.snapshots[key].Sessions[0].Runs[0].Status = "completed"
	api.mu.Unlock()
	if !r.discoveredJob(t.Context(), r.epoch, key, w, true) || r.reserve(fresh, w) {
		t.Fatal("rediscovery admitted work before observing the session")
	}
	runThreadWork(t, r, &wg, key)
	if r.unknown[key] || r.active[key] != 0 || len(r.watchers) != 0 || !r.jobs[fresh].Dirty || !r.reserve(fresh, w) {
		t.Fatal("completed run did not release capacity and wake waiting work")
	}
	if !r.jobs[key].Retry.Permanent {
		t.Fatal("successful observation resumed reconciliation")
	}
}

func TestPermanentThreadObservationFailuresBackOff(t *testing.T) {
	for name, err := range map[string]error{
		"network": errors.New("connection lost"),
		"http":    &mattermost.HTTPError{Status: 500, RetryAfter: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			e, api, _, _, key := fixture(t)
			e.API = failedSnapshot{err: err}
			r := observationRuntime(e, key)
			var wg sync.WaitGroup
			fresh := key
			fresh.Root = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			for attempt := 1; attempt <= 2; attempt++ {
				started := time.Now()
				runThreadWork(t, r, &wg, key)
				job := r.jobs[key]
				if !job.Retry.Permanent || !r.unknown[key] || job.Retry.Failures != attempt || !job.Dirty || r.reserve(fresh, e.Config.Workflows[0]) {
					t.Fatal("observation failure resumed reconciliation or admitted unknown capacity")
				}
				delay := time.Second * time.Duration(1<<attempt)
				if name == "http" {
					delay = time.Minute
				}
				if job.Retry.At.Before(started.Add(delay)) {
					t.Fatal("observation ignored backoff or Retry-After")
				}
			}
			e.API = api
			runThreadWork(t, r, &wg, key)
			job := r.jobs[key]
			if !job.Retry.Permanent || job.Retry.Failures != 0 || !job.Retry.At.IsZero() || r.unknown[key] || !r.reserve(fresh, e.Config.Workflows[0]) {
				t.Fatal("successful observation did not clear the transient failure while preserving the permanent failure")
			}
		})
	}
}

func TestPermanentAdmissionFailureReleasesReservation(t *testing.T) {
	for _, code := range []string{"idempotency_conflict", "request_too_large"} {
		t.Run(code, func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			e.Config.Workflows[0].MaxConcurrentRuns = 1
			bad := &rejectedAPI{fakeAPI: api, code: code}
			source := &permanentSource{schedulerSource: &schedulerSource{bot: mm.bot, channel: key.Channel, posts: mm.posts}, err: &mattermost.HTTPError{Status: 400}}
			e.API, e.MM = bad, source
			r := observationRuntime(e, key)
			r.jobs[key].Retry = retry{}
			delete(r.unknown, key)
			e.Reserve = r.reserve
			var wg sync.WaitGroup
			runThreadWork(t, r, &wg, key)
			if bad.calls != 1 || len(api.snapshot.Sessions) != 0 || r.active[key] != 1 || !r.jobs[key].Retry.Permanent || !r.jobs[key].Dirty || len(r.unknown) != 0 {
				t.Fatal("permanent admission failure did not leave a reservation without a run")
			}
			wantPosts := int32(0)
			if code == "request_too_large" {
				wantPosts = 1
			}
			if source.attempts.Load() != wantPosts {
				t.Fatal("admission did not exercise the expected rejection path")
			}
			fresh := key
			fresh.Root = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			r.jobs[fresh] = &scheduled{}
			if r.reserve(fresh, e.Config.Workflows[0]) {
				t.Fatal("unobserved reservation was ignored")
			}
			runThreadWork(t, r, &wg, key)
			if r.active[key] != 0 || !r.jobs[key].Retry.Permanent || bad.calls != 1 || source.attempts.Load() != wantPosts || !r.jobs[fresh].Dirty || !r.reserve(fresh, e.Config.Workflows[0]) {
				t.Fatal("observation did not release the reservation without retrying admission or rejection")
			}
		})
	}
}
