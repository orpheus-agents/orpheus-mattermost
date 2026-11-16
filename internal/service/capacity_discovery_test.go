package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type capacityDiscoveryAPI struct {
	API
	active func(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error)
}

func (a capacityDiscoveryAPI) ActiveRuns(ctx context.Context, ns string, from, to time.Time) ([]conversation.ActiveRun, error) {
	return a.active(ctx, ns, from, to)
}

type blockedHistoryAPI struct {
	*schedulerAPI
	key     conversation.Key
	listing bool
	entered chan struct{}
}

func (a blockedHistoryAPI) DiscoverSessions(ctx context.Context, _ string, _, _ time.Time) ([]conversation.Session, error) {
	if a.listing {
		close(a.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []conversation.Session{{ID: "old", ExternalKey: a.key.External()}}, nil
}

func (a blockedHistoryAPI) Snapshot(ctx context.Context, key conversation.Key) (conversation.Snapshot, error) {
	if key == a.key {
		close(a.entered)
		<-ctx.Done()
		return conversation.Snapshot{}, ctx.Err()
	}
	return a.schedulerAPI.Snapshot(ctx, key)
}

func TestNewRunDoesNotWaitForDeliveryRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		listing, active bool
	}{
		{name: "session listing", listing: true},
		{name: "completed thread history"},
		{name: "active thread history", active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, _, mm, _, key := fixture(t)
				e.SourceID = key.Source
				e.Config.MaxParallelThreads = 2
				e.Config.Workflows[0].MaxConcurrentRuns = 1
				source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1)}
				base := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 4)}
				if tc.active {
					e.Config.Workflows[0].MaxConcurrentRuns = 2
					base.snapshots[key] = conversation.Snapshot{Sessions: []conversation.Session{{ID: "old", Runs: []conversation.Run{{ID: "active", Status: "running"}}}}}
				}
				api := blockedHistoryAPI{schedulerAPI: base, key: key, listing: tc.listing, entered: make(chan struct{})}
				e.MM, e.API = source, api
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- (&Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler)}).Run(ctx) }()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				select {
				case <-api.entered:
				default:
					t.Fatal("recovery was not blocked")
				}
				p := mm.posts[0]
				p.ID = strings.Repeat("n", 26)
				source.add(p)
				synctest.Wait()
				select {
				case root := <-base.started:
					if root != p.ID {
						t.Fatal("wrong thread admitted", root)
					}
				default:
					t.Fatal("delivery recovery blocked new admission")
				}
				// The first request is active: removing the recovery barrier must
				// not remove the workflow's concurrency limit.
				p.ID = strings.Repeat("m", 26)
				source.add(p)
				synctest.Wait()
				select {
				case <-base.started:
					t.Fatal("concurrent run limit exceeded")
				default:
				}
			})
		})
	}
}

func TestCapacityDiscoveryWindowsAndFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var activeErr, archiveErr error
		wantDays := 21
		activeCalls, archiveCalls := 0, 0
		var fromActive, toActive time.Time
		r := discoveryRuntime(t, capacityDiscoveryAPI{
			API: windowDiscoveryAPI{discover: func(_ context.Context, _ string, from, to time.Time) ([]conversation.Session, error) {
				archiveCalls++
				if !from.Equal(fromActive) || !to.Equal(toActive) {
					t.Fatal("capacity and recovery used different discovery windows")
				}
				return nil, archiveErr
			}},
			active: func(_ context.Context, ns string, from, to time.Time) ([]conversation.ActiveRun, error) {
				activeCalls++
				fromActive, toActive = from, to
				if ns != "mattermost/assistant" || !to.Equal(time.Now()) || to.Location() != time.UTC || to.Sub(from) != time.Duration(wantDays)*24*time.Hour {
					t.Fatal("wrong active discovery window", ns, from, to)
				}
				return nil, activeErr
			},
		})
		w := r.cfg.Workflows[0]
		key := conversation.Key{Workflow: w.ID, Root: "fresh"}
		activeErr = errors.New("active page failed")
		r.scan(t.Context(), r.cfg, nil, 0)
		if r.reserve(key, w) || archiveCalls != 0 {
			t.Fatal("failed bootstrap allowed admission or recovery")
		}
		activeErr, archiveErr = nil, errors.New("archive failed")
		r.scan(t.Context(), r.cfg, nil, 0)
		if !r.reserve(key, w) || r.ready.Load() {
			t.Fatal("archive failure blocked valid capacity or reported successful recovery")
		}
		archiveErr = nil
		r.scan(t.Context(), r.cfg, nil, 0)
		if r.capacity.count(key) != 1 {
			t.Fatal("retry erased a local reservation")
		}
		wantDays = 7
		time.Sleep(w.FullReconcileInterval.Value())
		r.scan(t.Context(), r.cfg, nil, 0)
		if activeCalls != 4 || archiveCalls != 3 {
			t.Fatal("wrong discovery count", activeCalls, archiveCalls)
		}
		// Channel-only scans do not reset accounting or query active work.
		r.scan(t.Context(), r.cfg, nil, 0)
		if activeCalls != 4 || !r.capacity.ready[w.ID] {
			t.Fatal("channel replay reset capacity")
		}
	})
}

func TestCapacityDiscoveryCountsOnlyOwnedRunsAcrossGenerations(t *testing.T) {
	r := discoveryRuntime(t, &runtimeAPI{})
	w := r.cfg.Workflows[0]
	key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: strings.Repeat("c", 26), Root: strings.Repeat("r", 26)}
	other := key
	other.Source = "other-source"
	r.Engine.API = capacityDiscoveryAPI{API: &runtimeAPI{}, active: func(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error) {
		return []conversation.ActiveRun{{SessionID: "old", RunID: "one", ExternalKey: key.External()}, {SessionID: "new", RunID: "two", ExternalKey: key.External()}, {SessionID: "foreign", RunID: "three", ExternalKey: other.External()}}, nil
	}}
	r.scan(t.Context(), r.cfg, nil, 0)
	if r.capacity.count(key) != 2 || r.capacity.total() != 2 || r.activeRuns.Load() != 2 || len(r.jobs) != 1 {
		t.Fatal("capacity did not respect generation or source boundaries")
	}
	w.MaxConcurrentRuns = 2
	key.Root = strings.Repeat("n", 26)
	if r.reserve(key, w) {
		t.Fatal("restored runs did not occupy capacity")
	}
}

func TestCapacityDiscoveryIgnoresStaleConfiguration(t *testing.T) {
	r := discoveryRuntime(t, &runtimeAPI{})
	r.Engine.API = capacityDiscoveryAPI{API: &runtimeAPI{}, active: func(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error) {
		r.epoch++ // The active query completed after a reload.
		return nil, nil
	}}
	r.scan(t.Context(), r.cfg, nil, 0)
	if len(r.capacity.ready) != 0 || len(r.discovered) != 0 {
		t.Fatal("stale discovery initialized reloaded configuration")
	}
}

func TestCapacityRestoresAllWorkflowsBeforeQueueing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := discoveryRuntime(t, &runtimeAPI{})
		r.cfg.Workflows = slices.Clone(r.cfg.Workflows)
		other := r.cfg.Workflows[0]
		other.ID = "other"
		r.cfg.Workflows = append(r.cfg.Workflows, other)
		for i := range maxQueuedThreads {
			r.jobs[conversation.Key{Root: string(rune(i))}] = &scheduled{}
		}
		key := conversation.Key{Source: r.Engine.SourceID, Channel: strings.Repeat("c", 26), Root: strings.Repeat("r", 26)}
		r.Engine.API = capacityDiscoveryAPI{API: &runtimeAPI{}, active: func(_ context.Context, ns string, _, _ time.Time) ([]conversation.ActiveRun, error) {
			return []conversation.ActiveRun{{SessionID: ns, RunID: ns, ExternalKey: key.External()}}, nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { r.scan(ctx, r.cfg, nil, 0); close(done) }()
		synctest.Wait()
		r.mu.Lock()
		for _, w := range r.cfg.Workflows {
			key.Workflow = w.ID
			if !r.capacity.ready[w.ID] || r.capacity.count(key) != 1 {
				t.Error("full delivery queue blocked capacity restoration", w.ID)
			}
		}
		r.mu.Unlock()
		cancel()
		<-done
	})
}

func TestOlderThreadSnapshotCannotReleaseDiscoveredCapacity(t *testing.T) {
	e, _, _, _, key := fixture(t)
	r := observationRuntime(e, key)
	var wg sync.WaitGroup
	if r.observe(t.Context(), &wg, key, conversation.Snapshot{}, 0) || r.capacity.count(key) != 1 || !r.jobs[key].Dirty {
		t.Fatal("older history released discovered work")
	}
	if !r.observe(t.Context(), &wg, key, conversation.Snapshot{}, 1) || r.capacity.count(key) != 0 {
		t.Fatal("fresh history did not release completed work")
	}
}

type callbackSnapshotAPI struct {
	API
	snapshot func() conversation.Snapshot
}

func (a callbackSnapshotAPI) Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error) {
	return a.snapshot(), nil
}

func TestSnapshotCannotReleaseCapacityReconfirmedDuringRead(t *testing.T) {
	e, _, _, _, key := fixture(t)
	e.Config.Workflows[0].MaxConcurrentRuns = 1
	r := observationRuntime(e, key)
	// The previous POST's response was lost: one place is already reserved.
	// An older empty snapshot arrives after discovery confirmed that same run.
	e.API = callbackSnapshotAPI{snapshot: func() conversation.Snapshot {
		r.mu.Lock()
		r.capacity.restore(key.Workflow, map[conversation.Key]int{key: 1})
		r.mu.Unlock()
		return conversation.Snapshot{}
	}}
	var wg sync.WaitGroup
	runThreadWork(t, r, &wg, key)
	fresh := key
	fresh.Root = strings.Repeat("n", 26)
	if r.capacity.count(key) != 1 || !r.jobs[key].Dirty || r.reserve(fresh, e.Config.Workflows[0]) {
		t.Fatal("unchanged count let older history release a confirmed run")
	}
	e.API = callbackSnapshotAPI{snapshot: func() conversation.Snapshot { return conversation.Snapshot{} }}
	runThreadWork(t, r, &wg, key)
	if !r.reserve(fresh, e.Config.Workflows[0]) {
		t.Fatal("a subsequent authoritative snapshot did not release capacity")
	}
}

func TestCapacityDiscoveryIsolatesWorkflowFailures(t *testing.T) {
	for _, failed := range []string{"assistant", "other", "retired"} {
		t.Run(failed, func(t *testing.T) {
			r := discoveryRuntime(t, &runtimeAPI{})
			other := r.cfg.Workflows[0]
			other.ID = "other"
			r.cfg.Workflows = append(slices.Clone(r.cfg.Workflows), other)
			retired := other
			retired.ID = "retired"
			key := conversation.Key{Source: r.Engine.SourceID, Channel: strings.Repeat("c", 26), Root: strings.Repeat("r", 26)}
			var archives []string
			r.Engine.API = capacityDiscoveryAPI{
				API: windowDiscoveryAPI{discover: func(_ context.Context, ns string, _, _ time.Time) ([]conversation.Session, error) {
					archives = append(archives, ns)
					return nil, nil
				}},
				active: func(_ context.Context, ns string, _, _ time.Time) ([]conversation.ActiveRun, error) {
					if ns == "mattermost/"+failed {
						return nil, errors.New("active discovery failed")
					}
					return []conversation.ActiveRun{{SessionID: ns, RunID: ns, ExternalKey: key.External()}}, nil
				},
			}
			r.scan(t.Context(), r.cfg, []config.Workflow{retired}, 0)
			for _, w := range append(slices.Clone(r.cfg.Workflows), retired) {
				key.Workflow = w.ID
				fresh := key
				fresh.Root = strings.Repeat("n", 26)
				w.MaxConcurrentRuns = 2
				want := w.ID != failed
				if r.capacity.ready[w.ID] != want || r.reserve(fresh, w) != want || slices.Contains(archives, key.Namespace()) != want {
					t.Fatal("one workflow's failure affected another", failed, w.ID)
				}
			}
		})
	}
}

func TestReloadCancelsObsoleteRecovery(t *testing.T) {
	for _, blocked := range []string{"archive", "channel replay"} {
		t.Run(blocked, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, _, mm, _, key := fixture(t)
				e.SourceID = key.Source
				source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1)}
				base := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 4)}
				queries, archiveCalls := 0, 0
				cancelled := make(chan struct{})
				block := func(ctx context.Context) {
					<-ctx.Done()
					close(cancelled)
				}
				e.API = capacityDiscoveryAPI{
					API: windowDiscoveryAPI{API: base, discover: func(ctx context.Context, _ string, _, _ time.Time) ([]conversation.Session, error) {
						archiveCalls++
						if blocked == "archive" && archiveCalls == 1 {
							block(ctx)
							return nil, ctx.Err()
						}
						return nil, nil
					}},
					active: func(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error) {
						queries++
						return nil, nil
					},
				}
				e.MM = source
				if blocked == "channel replay" {
					calls := 0
					e.MM = &channelReplaySource{MM: source, channels: []mattermost.Channel{{ID: key.Channel, Type: "O"}}, replay: func(ctx context.Context, _ string, _ time.Time) ([]mattermost.Post, error) {
						calls++
						if calls == 1 {
							block(ctx)
							return nil, ctx.Err()
						}
						return nil, nil
					}}
				}
				reload := make(chan config.Config, 1)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- (&Runtime{Engine: e, Reload: reload, Logger: slog.New(slog.DiscardHandler)}).Run(ctx) }()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				reload <- e.Config
				synctest.Wait()
				select {
				case <-cancelled:
				default:
					t.Fatal("reload waited for obsolete recovery")
				}
				if queries != 2 {
					t.Fatal("reload did not perform fresh active accounting", queries)
				}
				source.add(mm.posts[0])
				synctest.Wait()
				select {
				case <-base.started:
				default:
					t.Fatal("post-reload admission stayed blocked")
				}
			})
		})
	}
}

func TestReloadBootstrapsCapacityBeforeAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _, mm, _, key := fixture(t)
		e.SourceID = key.Source
		e.Config.Workflows[0].MaxConcurrentRuns = 2
		source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 2), posts: slices.Clone(mm.posts)}
		base := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 4)}
		queries := 0
		release := make(chan struct{})
		e.API = capacityDiscoveryAPI{API: base, active: func(ctx context.Context, ns string, from, to time.Time) ([]conversation.ActiveRun, error) {
			queries++
			if to.Sub(from) != 21*24*time.Hour {
				t.Error("reload did not restore the startup window")
			}
			if queries == 2 {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return base.ActiveRuns(ctx, ns, from, to)
		}}
		e.MM = source
		reload := make(chan config.Config, 1)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		r := &Runtime{Engine: e, Reload: reload, Logger: slog.New(slog.DiscardHandler)}
		go func() { done <- r.Run(ctx) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		select {
		case <-base.started:
		default:
			t.Fatal("first run did not start")
		}
		reload <- e.Config
		synctest.Wait()
		p := mm.posts[0]
		p.ID = strings.Repeat("n", 26)
		source.add(p)
		synctest.Wait()
		select {
		case <-base.started:
			t.Fatal("reload admitted work before restoring accounting")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case root := <-base.started:
			if root != p.ID {
				t.Fatal("wrong thread after reload", root)
			}
		default:
			t.Fatal("restored capacity did not wake pending work")
		}
		p.ID = strings.Repeat("m", 26)
		source.add(p)
		synctest.Wait()
		select {
		case <-base.started:
			t.Fatal("reload lost an existing run's capacity")
		default:
		}
	})
}

type lostAdmissionAPI struct{ *schedulerAPI }

func (a lostAdmissionAPI) Submit(ctx context.Context, w config.Workflow, env conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	_, err := a.schedulerAPI.Submit(ctx, w, env, messages, sid, rid, pred)
	if err != nil {
		return conversation.Accepted{}, err
	}
	return conversation.Accepted{}, errors.New("admission response lost")
}

func TestDiscoveryPreservesInflightAndLostAdmissionCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _, mm, _, key := fixture(t)
		e.SourceID = key.Source
		e.Config.Workflows[0].MaxConcurrentRuns = 1
		e.Config.Workflows[0].FullReconcileInterval = config.Duration(time.Second)
		e.Config.Workflows[0].PollInterval = config.Duration(time.Second)
		source := &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 2), posts: slices.Clone(mm.posts)}
		base := &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 4), slow: key.Root, release: make(chan struct{})}
		queries := 0
		e.API = capacityDiscoveryAPI{API: lostAdmissionAPI{base}, active: func(context.Context, string, time.Time, time.Time) ([]conversation.ActiveRun, error) {
			queries++
			// A run can be absent from this bounded snapshot while its POST is
			// in flight or its creation is beyond the query's exclusive bound.
			return nil, nil
		}}
		e.MM = source
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		r := &Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler)}
		go func() { done <- r.Run(ctx) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		select {
		case <-base.started:
		default:
			t.Fatal("first admission did not start")
		}
		p := mm.posts[0]
		p.ID = strings.Repeat("n", 26)
		source.add(p)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if queries < 2 {
			t.Fatal("periodic discovery was not exercised")
		}
		select {
		case <-base.started:
			t.Fatal("discovery released an in-flight reservation")
		default:
		}
		close(base.release)
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case <-base.started:
			t.Fatal("lost response duplicated admission or exceeded capacity")
		default:
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.capacity.count(key) != 1 {
			t.Fatal("accepted run's capacity was lost")
		}
	})
}
