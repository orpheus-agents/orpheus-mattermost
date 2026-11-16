package service

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type windowDiscoveryAPI struct {
	API
	discover func(context.Context, string, time.Time, time.Time) ([]conversation.Session, error)
}

func (windowDiscoveryAPI) ActiveRuns(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error) {
	return nil, nil
}

func (a windowDiscoveryAPI) DiscoverSessions(ctx context.Context, namespace string, from, to time.Time) ([]conversation.Session, error) {
	return a.discover(ctx, namespace, from, to)
}

func discoveryRuntime(t *testing.T, api API) *Runtime {
	t.Helper()
	engine, _, _, _, key := fixture(t)
	engine.SourceID = key.Source
	engine.API = api
	engine.MM = &membershipSource{}
	return &Runtime{
		Engine: engine, Logger: slog.New(slog.DiscardHandler), cfg: engine.Config,
		jobs: map[conversation.Key]*scheduled{}, channels: map[string]mattermost.Channel{},
		capacity: newRunCapacity(),
		scanned:  map[string]time.Time{}, discovered: map[string]time.Time{},
		digests: map[conversation.Key]string{}, wake: make(chan struct{}, 1),
	}
}

func TestSessionDiscoveryWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wantDays, calls := 21, 0
		var failure error
		api := windowDiscoveryAPI{discover: func(_ context.Context, namespace string, from, to time.Time) ([]conversation.Session, error) {
			calls++
			if namespace != "mattermost/assistant" || !to.Equal(time.Now()) || to.Location() != time.UTC || !from.Equal(to.Add(-time.Duration(wantDays)*24*time.Hour)) {
				t.Fatalf("want %d-day window ending now: namespace=%s from=%s to=%s", wantDays, namespace, from, to)
			}
			return nil, failure
		}}
		r := discoveryRuntime(t, api)
		workflow := r.cfg.Workflows[0]
		failure = errors.New("incomplete discovery")
		r.scan(t.Context(), r.cfg, nil, 0)
		if !r.discovered[workflow.ID].IsZero() || r.ready.Load() {
			t.Fatal("failed startup discovery advanced recovery")
		}

		// A retry still needs the 21-day startup window.
		time.Sleep(3 * time.Second)
		failure = nil
		r.scan(t.Context(), r.cfg, nil, 0)
		if !r.discovered[workflow.ID].Equal(time.Now()) || !r.ready.Load() {
			t.Fatal("successful startup discovery was not recorded")
		}
		r.scan(t.Context(), r.cfg, nil, 0)
		if calls != 2 {
			t.Fatal("discovery ran before its interval")
		}

		// Later scans use seven days; their failures preserve the last success.
		wantDays = 7
		time.Sleep(workflow.FullReconcileInterval.Value())
		r.scan(t.Context(), r.cfg, nil, 0)
		lastSuccess := r.discovered[workflow.ID]
		time.Sleep(workflow.FullReconcileInterval.Value())
		failure = errors.New("later page failed")
		r.scan(t.Context(), r.cfg, nil, 0)
		if !r.discovered[workflow.ID].Equal(lastSuccess) {
			t.Fatal("failed periodic discovery advanced its watermark")
		}
		time.Sleep(3 * time.Second)
		failure = nil
		r.scan(t.Context(), r.cfg, nil, 0)
		if !r.discovered[workflow.ID].Equal(time.Now()) || calls != 5 {
			t.Fatal("periodic retry did not succeed", calls)
		}

		// A fresh process has no discovery state and starts with 21 days again.
		wantDays = 21
		restarted := discoveryRuntime(t, api)
		restarted.scan(t.Context(), restarted.cfg, nil, 0)
		if calls != 6 || !restarted.ready.Load() {
			t.Fatal("restart did not restore the startup discovery window")
		}
	})
}

func TestSessionDiscoveryKeepsTrackedWork(t *testing.T) {
	r := discoveryRuntime(t, windowDiscoveryAPI{discover: func(context.Context, string, time.Time, time.Time) ([]conversation.Session, error) {
		return nil, nil // All previously known threads are outside this discovery window.
	}})
	w := r.cfg.Workflows[0]
	activeKey := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: "cccccccccccccccccccccccccc", Root: "aaaaaaaaaaaaaaaaaaaaaaaaaa"}
	pendingKey := activeKey
	pendingKey.Root = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	old := time.Now().Add(-30 * 24 * time.Hour)
	activeJob := &scheduled{Workflow: w, Next: old}
	pendingJob := &scheduled{Workflow: w, Next: old, Retry: retry{Failures: 1, At: old}}
	r.jobs[activeKey], r.jobs[pendingKey] = activeJob, pendingJob
	r.capacity.observe(activeKey, 1)
	r.Engine.set(pendingKey, pending{Workflow: w})
	r.discovered[w.ID] = time.Now().Add(-w.FullReconcileInterval.Value())
	r.scan(t.Context(), r.cfg, nil, 0)
	if r.jobs[activeKey] != activeJob || r.jobs[pendingKey] != pendingJob || r.capacity.count(activeKey) != 1 {
		t.Fatal("bounded discovery discarded tracked active or pending work")
	}
	if _, ok := r.Engine.get(pendingKey); !ok {
		t.Fatal("bounded discovery discarded a frozen admission")
	}
}

func TestSessionDiscoveryWindowResetsOnReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine, _, mm, _, key := fixture(t)
		engine.SourceID = key.Source
		engine.MM = &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event)}
		windows := make(chan time.Duration, 8)
		engine.API = windowDiscoveryAPI{discover: func(_ context.Context, _ string, from, to time.Time) ([]conversation.Session, error) {
			windows <- to.Sub(from)
			return nil, nil
		}}
		reload := make(chan config.Config, 1)
		r := &Runtime{Engine: engine, Reload: reload, Logger: slog.New(slog.DiscardHandler)}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		assertWindow := func(days int) {
			t.Helper()
			synctest.Wait()
			select {
			case window := <-windows:
				if window != time.Duration(days)*24*time.Hour {
					t.Fatalf("want %d-day window, got %s", days, window)
				}
			default:
				t.Fatal("no discovery after startup, interval or reload")
			}
		}
		interval := engine.Config.Workflows[0].FullReconcileInterval.Value() + engine.Config.Workflows[0].PollInterval.Value()
		assertWindow(21)
		time.Sleep(interval)
		assertWindow(7)
		reload <- engine.Config
		assertWindow(21)
		time.Sleep(interval)
		assertWindow(7)
	})
}
