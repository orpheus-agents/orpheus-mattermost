package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

type discoveryWindow struct {
	workflow config.Workflow
	from, to time.Time
	active   map[conversation.Key]int
}

// discover restores admission accounting before scheduling delivery recovery.
// Only the Orpheus adapter knows how active work is selected by the remote API.
func (r *Runtime) discover(ctx context.Context, cfg config.Config, retired []config.Workflow, epoch uint64) bool {
	var windows []discoveryWindow
	for _, w := range append(slices.Clone(cfg.Workflows), retired...) {
		r.mu.Lock()
		last := r.discovered[w.ID]
		r.mu.Unlock()
		if time.Since(last) < w.FullReconcileInterval.Value() {
			continue
		}
		lookback := periodicDiscoveryLookback
		if last.IsZero() {
			lookback = startupDiscoveryLookback
		}
		to := time.Now().UTC()
		windows = append(windows, discoveryWindow{workflow: w, from: to.Add(-lookback), to: to})
	}
	r.mu.Lock()
	if epoch != r.epoch {
		r.mu.Unlock()
		return false
	}
	for _, window := range windows {
		delete(r.capacity.ready, window.workflow.ID)
	}
	r.mu.Unlock()
	complete := true
	for i := range windows {
		window := &windows[i]
		active, err := r.activeThreads(ctx, *window)
		if err != nil {
			r.Logger.Error("active run discovery failed", "workflow", window.workflow.ID, "class", errorClass(err))
			complete = false
			continue
		}
		r.mu.Lock()
		if epoch != r.epoch {
			r.mu.Unlock()
			return false
		}
		window.active = active
		r.capacity.restore(window.workflow.ID, active)
		r.activeRuns.Store(int64(r.capacity.total()))
		r.wakeCapacityWaiters()
		r.mu.Unlock()
		r.notify()
	}
	// Even a full delivery queue must not delay restoring another workflow's
	// capacity. Queue active threads only after all due counts are restored.
	for _, window := range windows {
		for key := range window.active {
			if !r.discoveredJob(ctx, epoch, key, window.workflow) {
				return false
			}
		}
	}
	for _, window := range windows {
		if window.active == nil { // This workflow's accounting failed.
			continue
		}
		w := window.workflow
		sessions, err := r.Engine.API.DiscoverSessions(ctx, "mattermost/"+w.ID, window.from, window.to)
		if err != nil {
			r.Logger.Error("session discovery failed", "workflow", w.ID, "class", errorClass(err))
			complete = false
			continue
		}
		for _, session := range sessions {
			key, owned, err := r.discoveryKey(w.ID, session.ExternalKey)
			if err != nil {
				r.Logger.Error("session identity invalid")
				return false
			}
			if owned && !r.discoveredJob(ctx, epoch, key, w) {
				return false
			}
		}
		r.mu.Lock()
		if epoch != r.epoch {
			r.mu.Unlock()
			return false
		}
		r.discovered[w.ID] = time.Now()
		r.mu.Unlock()
	}
	return complete
}

func (r *Runtime) activeThreads(ctx context.Context, window discoveryWindow) (map[conversation.Key]int, error) {
	runs, err := r.Engine.API.ActiveRuns(ctx, "mattermost/"+window.workflow.ID, window.from, window.to)
	if err != nil {
		return nil, err
	}
	active := map[conversation.Key]int{}
	for _, run := range runs {
		key, owned, err := r.discoveryKey(window.workflow.ID, run.ExternalKey)
		if err != nil {
			return nil, err
		}
		if owned {
			active[key]++
		}
	}
	return active, nil
}

func (r *Runtime) discoveryKey(workflow, external string) (conversation.Key, bool, error) {
	if !strings.HasPrefix(external, r.Engine.SourceID+":channel:") {
		return conversation.Key{}, false, nil
	}
	key, err := conversation.ParseKey(r.Engine.SourceID, workflow, external)
	return key, true, err
}

// wakeCapacityWaiters requires Runtime.mu.
func (r *Runtime) wakeCapacityWaiters() {
	for _, j := range r.jobs {
		if j.CapacityWait {
			j.Dirty = true
		}
	}
}
