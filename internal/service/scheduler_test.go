package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestSchedulerSharesWorkersWithoutStarvingRecovery(t *testing.T) {
	for _, slots := range []int{1, 8} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			var ready []readyThread
			for i := range 64 {
				ready = append(ready, readyThread{key: conversation.Key{Root: fmt.Sprint(i)}, order: uint64(i), class: liveWork})
			}
			for i := range 8 {
				ready = append(ready, readyThread{key: conversation.Key{Root: fmt.Sprint(100 + i)}, order: uint64(i), class: recoveryWork})
			}
			s := threadScheduler{}
			var dispatched []readyThread
			for len(ready) > 0 {
				picked := s.take(ready, slots)
				if len(picked) != min(slots, len(ready)) {
					t.Fatal("workers left idle", picked)
				}
				dispatched = append(dispatched, picked...)
				ready = slices.DeleteFunc(ready, func(item readyThread) bool { return slices.Contains(picked, item) })
			}
			for i, item := range dispatched[:64] {
				want := liveWork
				if i%8 == 7 {
					want = recoveryWork
				}
				if item.class != want {
					t.Fatalf("dispatch %d: got %v want %v", i, item.class, want)
				}
			}
			for _, class := range []workClass{liveWork, recoveryWork} {
				var order uint64
				for _, item := range dispatched {
					if item.class != class {
						continue
					}
					if item.order < order {
						t.Fatal("FIFO order lost", dispatched)
					}
					order = item.order
				}
			}
		})
	}
	for _, class := range []workClass{recoveryWork, liveWork} {
		ready := make([]readyThread, 16)
		for i := range ready {
			ready[i].class = class
		}
		s := threadScheduler{}
		if len(s.take(ready, 8)) != 8 {
			t.Fatal("single class did not use whole pool")
		}
	}
}

func TestQueueReserveAndPromotionAtCapacity(t *testing.T) {
	r := discoveryRuntime(t, &runtimeAPI{})
	w := r.cfg.Workflows[0]
	key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID}
	for i := range maxQueuedThreads - liveQueueReserve {
		key.Root = fmt.Sprint(i)
		if !r.enqueue(key, w, recoveryWork) {
			t.Fatal("recovery admission stopped early", i)
		}
	}
	key.Root = "overflow"
	if r.enqueue(key, w, recoveryWork) {
		t.Fatal("recovery used live reserve")
	}
	for i := range liveQueueReserve {
		key.Root = fmt.Sprintf("live-%d", i)
		r.hint(key)
	}
	if len(r.jobs) != maxQueuedThreads || r.queueRejections != 0 {
		t.Fatal("live reserve unavailable")
	}
	key.Root = "overflow"
	r.hint(key)
	if len(r.jobs) != maxQueuedThreads || r.queueRejections != 1 {
		t.Fatal("overload not bounded and visible")
	}
	key.Root = "0"
	r.hint(key)
	j := r.jobs[key]
	order, since := j.Order, j.QueuedAt
	r.hint(key)
	if !r.enqueue(key, w, recoveryWork) || j.Class != liveWork || j.Order != order || j.QueuedAt != since || len(r.jobs) != maxQueuedThreads {
		t.Fatal("promotion/coalescing lost at full capacity")
	}
	// A hint during a running pass remains pending, once, for the next pass.
	j.Working, j.Dirty = true, false
	r.hint(key)
	order = j.Order
	r.hint(key)
	if !j.Dirty || j.Class != liveWork || j.Order != order || len(r.runnable(time.Now())) != maxQueuedThreads-1 {
		t.Fatal("inflight event was lost or duplicated")
	}
}

func TestPriorityRespectsEligibilityAndRetryDelay(t *testing.T) {
	r := discoveryRuntime(t, &runtimeAPI{})
	now := time.Now()
	key := conversation.Key{Root: "thread"}
	j := &scheduled{Class: recoveryWork, Next: now.Add(-time.Second), Order: 1}
	r.jobs[key] = j
	r.capacity.active[key] = 1
	ready := r.runnable(now)
	if len(ready) != 1 || ready[0].class != liveWork {
		t.Fatal("active polling is not live")
	}
	j.Retry.At = now.Add(time.Minute)
	r.request(j, liveWork)
	if len(r.runnable(now)) != 0 {
		t.Fatal("live event bypassed backoff")
	}
	ready = r.runnable(now.Add(2 * time.Minute))
	if len(ready) != 1 || !ready[0].since.Equal(j.Retry.At) {
		t.Fatal("backoff included in queue wait", ready)
	}
	j.Retry = retry{Permanent: true}
	j.Dirty = false
	delete(r.capacity.active, key)
	if len(r.runnable(now.Add(time.Hour))) != 0 {
		t.Fatal("suspended idle job became runnable")
	}
	r.capacity.active[key] = 1
	if len(r.runnable(now.Add(time.Hour))) != 1 {
		t.Fatal("suspended active job stopped observation")
	}
	j.Working = true
	if len(r.runnable(now.Add(time.Hour))) != 0 {
		t.Fatal("same thread scheduled twice")
	}
}

func TestChannelBaselineIsRecoveryAndChangesAreLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := discoveryRuntime(t, &runtimeAPI{})
		_, _, mm, _, key := fixture(t)
		post := mm.posts[0]
		r.Engine.MM = &channelReplaySource{channels: []mattermost.Channel{{ID: key.Channel, Type: "O"}}, replay: func(context.Context, string, time.Time) ([]mattermost.Post, error) {
			return []mattermost.Post{post}, nil
		}}
		r.scan(t.Context(), r.cfg, nil, r.epoch)
		if len(r.jobs) != 1 {
			t.Fatal("baseline missed thread", r.jobs)
		}
		for _, j := range r.jobs {
			if j.Class != recoveryWork {
				t.Fatal("baseline flooded live queue")
			}
		}
		clear(r.jobs)
		time.Sleep(r.cfg.Workflows[0].PollInterval.Value())
		post.EditAt = time.Now().UnixMilli()
		r.scan(t.Context(), r.cfg, nil, r.epoch)
		if len(r.jobs) != 1 {
			t.Fatal("changed thread missed")
		}
		for _, j := range r.jobs {
			if j.Class != liveWork {
				t.Fatal("change found by replay is not live")
			}
		}
	})
}

type priorityAPI struct {
	*schedulerAPI
	live         conversation.Key
	archive      []conversation.Session
	release      chan struct{}
	signals      chan struct{}
	archiveReads atomic.Int32
	watchers     atomic.Int32
}

func (a *priorityAPI) DiscoverSessions(context.Context, string, time.Time, time.Time) ([]conversation.Session, error) {
	return a.archive, nil
}
func (a *priorityAPI) Snapshot(ctx context.Context, key conversation.Key) (conversation.Snapshot, error) {
	if key != a.live {
		a.archiveReads.Add(1)
		select {
		case <-ctx.Done():
			return conversation.Snapshot{}, ctx.Err()
		case <-a.release:
		}
	}
	return a.schedulerAPI.Snapshot(ctx, key)
}
func (a *priorityAPI) Watch(ctx context.Context, _ string, notify func()) error {
	a.watchers.Add(1)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.signals:
			notify()
		}
	}
}

func TestLiveAnswerArrivesBeforeArchiveDrains(t *testing.T) {
	for _, slots := range []int{1, 8} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprintf("slots=%d,sse=%v", slots, sse), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					e, _, mm, _, key := fixture(t)
					e.SourceID = key.Source
					e.Config.MaxParallelThreads = slots
					e.Config.Workflows[0].PollInterval = config.Duration(time.Second)
					source := &replaySource{schedulerSource: &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event, 1)}}
					api := &priorityAPI{schedulerAPI: &schedulerAPI{snapshots: map[conversation.Key]conversation.Snapshot{}, reads: map[string]int{}, started: make(chan string, 4)}, live: key, release: make(chan struct{}), signals: make(chan struct{}, 1)}
					for i := range 4000 {
						old := key
						old.Root = fmt.Sprintf("%026d", i)
						api.archive = append(api.archive, conversation.Session{ID: fmt.Sprint(i), ExternalKey: old.External()})
					}
					e.MM, e.API = source, api
					r := &Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler)}
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
					if len(r.jobs) != maxQueuedThreads-liveQueueReserve || int(api.archiveReads.Load()) != slots {
						t.Fatal("archive did not saturate admission and workers", len(r.jobs), api.archiveReads.Load())
					}
					source.add(mm.posts[0])
					synctest.Wait()
					if r.jobs[key] == nil || r.jobs[key].Class != liveWork {
						t.Fatal("live event rejected behind archive")
					}
					api.release <- struct{}{}
					synctest.Wait()
					select {
					case root := <-api.started:
						if root != key.Root {
							t.Fatal("wrong admission")
						}
					default:
						t.Fatal("new input waited for archive")
					}
					// The internal post-submit hint must beat the archive too, so observation
					// and SSE subscription are installed before the run finishes.
					if api.watchers.Load() != 1 {
						t.Fatal("post-submit observation delayed behind archive")
					}
					api.mu.Lock()
					session := &api.snapshots[key].Sessions[0]
					session.Runs[0].Status = "completed"
					session.Runs[0].FinalMessageID = "answer"
					session.Messages = append(session.Messages, conversation.Message{ID: "answer", RunID: session.Runs[0].ID, Role: "assistant", Kind: "answer", Text: "fresh answer", Position: 1})
					api.mu.Unlock()
					if sse {
						api.signals <- struct{}{}
					} else {
						time.Sleep(time.Second)
					}
					synctest.Wait()
					api.release <- struct{}{}
					synctest.Wait()
					if source.created != 1 || api.archiveReads.Load() > int32(slots+2) || r.failures.Load() != 0 {
						t.Fatal("answer waited for archive or failed", source.created, api.archiveReads.Load(), r.failures.Load())
					}
					source.mu.Lock()
					answer := source.posts[len(source.posts)-1]
					source.mu.Unlock()
					if answer.Message != "fresh answer" || answer.RootID != key.Root {
						t.Fatal("wrong delivered answer", answer.Message)
					}
					// Rediscovery and duplicate notifications still cannot publish twice.
					r.hint(key)
					synctest.Wait()
					api.release <- struct{}{}
					synctest.Wait()
					if source.created != 1 {
						t.Fatal("duplicate publication")
					}
				})
			})
		}
	}
}
