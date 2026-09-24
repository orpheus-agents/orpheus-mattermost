package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

type retry struct {
	At        time.Time
	Failures  int
	Permanent bool
}
type scheduled struct {
	Cancel                       context.CancelFunc
	Workflow                     config.Workflow
	Working, Dirty, CapacityWait bool
	Order                        uint64
	Next                         time.Time
	Retry                        retry
}

const maxQueuedThreads = 4096

type Runtime struct {
	Engine                                         *Engine
	Logger                                         *slog.Logger
	Reload                                         <-chan config.Config
	ready                                          atomic.Bool
	cycles, failures, reconnects, retries          atomic.Uint64
	cycleNanos, lastSuccess, replayLag, activeRuns atomic.Int64
	mu                                             sync.Mutex
	cfg                                            config.Config
	jobs                                           map[conversation.Key]*scheduled
	channels                                       map[string]mattermost.Channel
	active                                         map[conversation.Key]int
	unknown                                        map[conversation.Key]bool
	watchers                                       map[string]context.CancelFunc
	watchKeys                                      map[string]conversation.Key
	retired                                        map[string]config.Workflow
	digests                                        map[conversation.Key]string
	scanned                                        map[string]time.Time
	discovered                                     map[string]time.Time
	wake                                           chan struct{}
	sequence, epoch                                uint64
	running                                        int
	scanning, discovering, initialized             bool
	scanAt                                         time.Time
}

func (r *Runtime) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// enqueue requires mu. Events coalesce while a thread is already running.
func (r *Runtime) enqueue(key conversation.Key, w config.Workflow) bool {
	if j := r.jobs[key]; j != nil {
		j.Dirty = true
		return true
	}
	if len(r.jobs) >= maxQueuedThreads {
		r.scanAt = time.Time{}
		return false
	}
	r.sequence++
	r.jobs[key] = &scheduled{Workflow: w, Dirty: true, Order: r.sequence}
	return true
}
func (r *Runtime) hint(key conversation.Key) {
	r.mu.Lock()
	if j := r.jobs[key]; j != nil {
		j.Dirty = true
	} else {
		for _, w := range r.cfg.Workflows {
			if w.ID == key.Workflow {
				r.enqueue(key, w)
				break
			}
		}
	}
	r.mu.Unlock()
	r.notify()
}
func (r *Runtime) event(event mattermost.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := event.Post
	if p.ID == "" {
		r.scanAt = time.Time{}
		r.notify()
		return
	}
	if p.UserID == r.Engine.Bot.ID && !event.Deleted {
		return
	}
	ch, ok := r.channels[p.ChannelID]
	if !ok {
		r.scanAt = time.Time{}
		r.notify()
		return
	}
	w, err := r.cfg.Route(ch.ID, ch.Type)
	if err != nil || w == nil {
		return
	}
	key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: ch.ID, Root: p.Root()}
	// Passive posts only need immediate work in an already observed thread.
	if r.jobs[key] == nil && r.active[key] == 0 && !event.Deleted && !candidate(p, ch, *w, r.Engine.Bot) {
		return
	}
	r.enqueue(key, *w)
	r.notify()
}
func candidate(p mattermost.Post, ch mattermost.Channel, w config.Workflow, bot mattermost.User) bool {
	if p.DeleteAt != 0 || p.UserID == bot.ID || strings.HasPrefix(p.Type, "system_") || p.CreateAt < w.Since.UnixMilli() {
		return false
	}
	if strings.TrimSpace(p.Message) == "" && len(p.FileIDs) == 0 {
		return false
	}
	return ch.Type == "D" || !*w.StartOnMention || conversation.Mention(p.Message, []string{bot.Username})
}
func (r *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /healthz", health)
	ready := func(w http.ResponseWriter, _ *http.Request) {
		if !r.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	mux.HandleFunc("GET /readyz", ready)
	mux.HandleFunc("GET /ready", ready)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.metrics(w, "")
	})
	return mux
}
func (r *Runtime) metrics(w io.Writer, source string) {
	inputs, outputs, uncertain := r.Engine.queues()
	labels := ""
	if source != "" {
		labels = fmt.Sprintf("{source=%q}", source)
	}
	metrics := map[string]any{
		"pending_inputs": inputs, "pending_outputs": outputs, "uncertain_admission_age_seconds": uncertain.Seconds(),
		"reconciliations_total": r.cycles.Load(), "errors_total": r.failures.Load(),
		"reconnects_total": r.reconnects.Load(), "retries_total": r.retries.Load(),
		"reconcile_duration_seconds_sum":   float64(r.cycleNanos.Load()) / 1e9,
		"reconcile_duration_seconds_count": r.cycles.Load(),
		"last_success_timestamp_seconds":   r.lastSuccess.Load(), "replay_lag_seconds": float64(r.replayLag.Load()) / 1e9,
		"active_runs": r.activeRuns.Load(),
	}
	for name, value := range metrics {
		_, _ = fmt.Fprintf(w, "orpheus_mattermost_%s%s %v\n", name, labels, value)
	}
}
func reloadCompatible(a, b config.Config) bool {
	return a.Orpheus == b.Orpheus && a.AgentBoxAPIURL == b.AgentBoxAPIURL && a.Listen == b.Listen && a.HTTPTimeout == b.HTTPTimeout && a.MaxRequestBytes == b.MaxRequestBytes
}
func errorClass(err error) string {
	switch {
	case errors.Is(err, delivery.ErrConflict):
		return "delivery_conflict"
	case errors.Is(err, delivery.ErrUpload):
		return "attachment_recovery"

	}
	if code := orpheus.Code(err); code != "" {
		return "orpheus_" + code
	}

	if e, ok := errors.AsType[*mattermost.HTTPError](err); ok {
		return fmt.Sprintf("mattermost_http_%d", e.Status)
	}
	return fmt.Sprintf("%T", err)
}

func (r *Runtime) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Engine.SourceID == "" {
		if err := r.Engine.Validate(ctx); err != nil {
			return err
		}
	}
	r.cfg = r.Engine.Config
	r.wake = make(chan struct{}, 1)
	r.jobs = map[conversation.Key]*scheduled{}
	r.channels = map[string]mattermost.Channel{}
	r.active = map[conversation.Key]int{}
	r.unknown = map[conversation.Key]bool{}
	r.watchers = map[string]context.CancelFunc{}
	r.watchKeys = map[string]conversation.Key{}
	r.retired = map[string]config.Workflow{}
	r.digests = map[conversation.Key]string{}
	r.scanned = map[string]time.Time{}
	r.discovered = map[string]time.Time{}
	r.Engine.Notify = func(key conversation.Key) {
		if ctx.Err() == nil {
			r.hint(key)
		}
	}
	r.Engine.Reserve = r.reserve
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait(); r.ready.Store(false) }()
	wg.Go(func() {
		for ctx.Err() == nil {
			_ = r.Engine.MM.Watch(ctx, r.event)
			if ctx.Err() != nil {
				return
			}
			r.reconnects.Add(1)
			r.event(mattermost.Event{})
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	})
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	r.notify()
	for {
		select {
		case <-ctx.Done():
			return nil
		case cfg, ok := <-r.Reload:
			if !ok {
				r.Reload = nil
				continue
			}
			r.mu.Lock()
			if !reloadCompatible(r.cfg, cfg) {
				r.Logger.Error("configuration reload rejected", "reason", "process settings require restart")
			} else {
				for _, old := range r.cfg.Workflows {
					r.retired[old.ID] = old
				}
				for _, w := range cfg.Workflows {
					delete(r.retired, w.ID)
				}
				r.Engine.limitPosts(&cfg)
				r.cfg = cfg
				r.epoch++
				r.scanAt = time.Time{}
				r.scanned = map[string]time.Time{}
				r.discovered = map[string]time.Time{}
				r.digests = map[conversation.Key]string{}
				for _, j := range r.jobs {
					j.Retry = retry{}
					j.Dirty = true
					if j.Cancel != nil {
						j.Cancel()
					}
				}
			}
			r.mu.Unlock()
		case <-r.wake:
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		r.mu.Lock()
		if !r.scanning && !time.Now().Before(r.scanAt) {
			r.scanning = true
			r.discovering = true
			cfg, epoch := r.cfg, r.epoch
			retired := make([]config.Workflow, 0, len(r.retired))
			for _, w := range r.retired {
				retired = append(retired, w)
			}
			wg.Go(func() { r.scan(ctx, cfg, retired, epoch) })
		}
		// Ordered dispatch prevents a noisy thread from monopolizing the worker pool.
		keys := make([]conversation.Key, 0, len(r.jobs))
		for key, j := range r.jobs {
			if !j.Working && !j.Retry.Permanent && !time.Now().Before(j.Retry.At) && (j.Dirty || !time.Now().Before(j.Next)) {
				keys = append(keys, key)
			}
		}
		slices.SortFunc(keys, func(a, b conversation.Key) int {
			if r.jobs[a].Order < r.jobs[b].Order {
				return -1
			}
			return 1
		})
		for _, key := range keys {
			if r.running >= r.cfg.MaxParallelThreads {
				break
			}
			j := r.jobs[key]
			j.Working = true
			j.Dirty = false
			j.CapacityWait = false
			r.running++
			ch, member := r.channels[key.Channel]
			owner, _ := r.cfg.Route(ch.ID, ch.Type)
			inScope := member && owner != nil && owner.ID == key.Workflow
			w := j.Workflow
			if inScope {
				w = *owner
				j.Workflow = w
			}
			if j.Retry.Failures > 0 {
				r.retries.Add(1)
			}
			jobCtx, cancel := context.WithCancel(ctx)
			j.Cancel = cancel
			wg.Go(func() { defer cancel(); r.work(jobCtx, ctx, &wg, key, w, ch, inScope) })
		}
		r.mu.Unlock()
	}
}

func (r *Runtime) reserve(key conversation.Key, w config.Workflow) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Complete discovery first, so unobserved sessions cannot evade the run limit.
	blocked := !r.initialized || r.discovering
	count := 0
	for k, n := range r.active {
		if k.Workflow == w.ID {
			count += n
		}
	}
	for k := range r.unknown {
		if k.Workflow == w.ID {
			blocked = true
		}
	}
	if blocked || count >= w.MaxConcurrentRuns {
		if j := r.jobs[key]; j != nil {
			j.CapacityWait = true
		}
		return false
	}
	r.active[key] = 1 // Hold capacity even when the admission response is lost.
	return true
}

func (r *Runtime) work(ctx, watchCtx context.Context, wg *sync.WaitGroup, key conversation.Key, w config.Workflow, ch mattermost.Channel, inScope bool) {
	started := time.Now()
	snapshot, err := r.Engine.API.Snapshot(ctx, key)
	if err == nil {
		r.observe(watchCtx, wg, key, snapshot)
		err = r.Engine.reconcile(ctx, w, key, ch, inScope, snapshot)
	}
	r.cycleNanos.Add(int64(time.Since(started)))
	r.cycles.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[key]
	j.Working = false
	j.Cancel = nil
	r.running--
	r.sequence++
	j.Order = r.sequence
	j.Next = time.Now().Add(w.PollInterval.Value())
	if err != nil && ctx.Err() == nil {
		r.failures.Add(1)
		r.Logger.Error("thread reconciliation failed", "workflow", key.Workflow, "channel", key.Channel, "root", key.Root, "class", errorClass(err))
		j.Retry.Failures++
		delay := min(5*time.Minute, time.Second*time.Duration(1<<min(j.Retry.Failures, 8)))
		delay += time.Duration(rand.Int64N(int64(delay/2) + 1))
		if mm, ok := errors.AsType[*mattermost.HTTPError](err); ok {
			delay = max(delay, mm.RetryAfter)
			j.Retry.Permanent = mm.Status == 400 || mm.Status == 404 || mm.Status == 413
		}
		if errors.Is(err, delivery.ErrConflict) || orpheus.Code(err) == "idempotency_conflict" {
			j.Retry.Permanent = true
		}
		j.Retry.At = time.Now().Add(delay)
		j.Dirty = true
	} else if err == nil {
		j.Retry = retry{}
		r.lastSuccess.Store(time.Now().Unix())
		r.Engine.mu.Lock()
		pending := r.Engine.inputQueue[key] > 0
		_, frozen := r.Engine.pending[key]
		r.Engine.mu.Unlock()
		if !j.Dirty && !pending && !frozen && r.active[key] == 0 {
			delete(r.jobs, key)
		}
	}
	r.notify()
}
func (r *Runtime) observe(ctx context.Context, wg *sync.WaitGroup, key conversation.Key, snapshot conversation.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wasUnknown := r.unknown[key]
	delete(r.unknown, key)
	old := r.active[key]
	delete(r.active, key)
	watching := map[string]bool{}
	for _, s := range snapshot.Sessions {
		for _, run := range s.Runs {
			if !run.Terminal() {
				r.active[key]++
				watching[s.ID] = true
			}
		}
	}
	for sid := range watching {
		if _, ok := r.watchers[sid]; ok {
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		r.watchers[sid] = cancel
		r.watchKeys[sid] = key
		wg.Go(func() {
			for watchCtx.Err() == nil {
				_ = r.Engine.API.Watch(watchCtx, sid, func() { r.hint(key) })
				if watchCtx.Err() != nil {
					return
				}
				r.reconnects.Add(1)
				select {
				case <-watchCtx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		})
	}
	for sid, k := range r.watchKeys {
		if k == key && !watching[sid] {
			r.watchers[sid]()
			delete(r.watchers, sid)
			delete(r.watchKeys, sid)
		}
	}
	if old > r.active[key] || wasUnknown && len(r.unknown) == 0 {
		for _, j := range r.jobs {
			if j.CapacityWait {
				j.Dirty = true
			}
		}
	}
	total := 0
	for _, n := range r.active {
		total += n
	}
	r.activeRuns.Store(int64(total))
}

// A single discovery goroutine runs independently of thread workers. Its bounded
// queue can apply backpressure without blocking unrelated thread notifications.
func (r *Runtime) discoveredJob(ctx context.Context, epoch uint64, key conversation.Key, w config.Workflow, unknown bool) bool {
	for ctx.Err() == nil {
		r.mu.Lock()
		if epoch != r.epoch {
			r.mu.Unlock()
			return false
		}
		ok := r.enqueue(key, w)
		if ok && unknown {
			r.unknown[key] = true
		}
		r.mu.Unlock()
		r.notify()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}
func (r *Runtime) scan(ctx context.Context, cfg config.Config, retired []config.Workflow, epoch uint64) {
	success := false
	defer func() {
		r.mu.Lock()
		r.scanning = false
		r.discovering = false
		if epoch == r.epoch {
			r.initialized = success
			interval := 30 * time.Second
			for _, w := range cfg.Workflows {
				interval = min(interval, w.PollInterval.Value())
			}
			if !success {
				interval = 3 * time.Second
			}
			r.scanAt = time.Now().Add(interval)
			r.ready.Store(success)
			for _, j := range r.jobs {
				if j.CapacityWait {
					j.Dirty = true
				}
			}
		}
		r.mu.Unlock()
		r.notify()
	}()
	channels, err := r.Engine.MM.Channels(ctx)
	if err != nil {
		r.Logger.Error("channel discovery failed", "class", errorClass(err))
		return
	}
	channelMap := map[string]mattermost.Channel{}
	for _, ch := range channels {
		if ch.DeleteAt == 0 {
			channelMap[ch.ID] = ch
		}
	}
	r.mu.Lock()
	if epoch != r.epoch {
		r.mu.Unlock()
		return
	}
	for key, j := range r.jobs {
		old, had := r.channels[key.Channel]
		current, has := channelMap[key.Channel]
		if had != has || old.Type != current.Type {
			j.Dirty = true
			if j.Cancel != nil {
				j.Cancel()
			}
		}
	}
	r.channels = channelMap
	r.mu.Unlock()
	// Register existing sessions before admitting new runs.
	workflows := append(slices.Clone(cfg.Workflows), retired...)
	for _, w := range workflows {
		r.mu.Lock()
		due := time.Since(r.discovered[w.ID]) >= w.FullReconcileInterval.Value()
		r.mu.Unlock()
		if !due {
			continue
		}
		sessions, err := r.Engine.API.Sessions(ctx, "mattermost/"+w.ID, "")
		if err != nil {
			r.Logger.Error("session discovery failed", "class", errorClass(err))
			return
		}
		for _, session := range sessions {
			if !strings.HasPrefix(session.ExternalKey, r.Engine.SourceID+":channel:") {
				continue
			}
			key, err := conversation.ParseKey(r.Engine.SourceID, w.ID, session.ExternalKey)
			if err != nil {
				r.Logger.Error("session identity invalid")
				return
			}
			if !r.discoveredJob(ctx, epoch, key, w, true) {
				return
			}
		}
		r.mu.Lock()
		if epoch == r.epoch {
			r.discovered[w.ID] = time.Now()
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	if epoch != r.epoch {
		r.mu.Unlock()
		return
	}
	r.discovering = false
	r.initialized = true
	for _, j := range r.jobs {
		if j.CapacityWait {
			j.Dirty = true
		}
	}
	r.mu.Unlock()
	r.notify()
	for _, ch := range channels {
		if ch.DeleteAt != 0 {
			continue
		}
		w, err := cfg.Route(ch.ID, ch.Type)
		if err != nil {
			return
		}
		if w == nil || w.Draining {
			continue
		}
		r.mu.Lock()
		due := time.Since(r.scanned[ch.ID]) >= w.PollInterval.Value()
		r.mu.Unlock()
		if !due {
			continue
		}
		posts, err := r.Engine.MM.Posts(ctx, ch.ID, w.Since)
		if err != nil {
			r.Logger.Error("channel replay failed", "channel", ch.ID, "class", errorClass(err))
			return
		}
		versions := map[string][]conversation.Version{}
		triggers := map[string]bool{}
		for _, p := range posts {
			versions[p.Root()] = append(versions[p.Root()], conversation.PostVersion(p))
			if candidate(p, ch, *w, r.Engine.Bot) {
				triggers[p.Root()] = true
			}
		}
		for root, v := range versions {
			key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: ch.ID, Root: root}
			digest := attachments.ObjectHash(v)
			r.mu.Lock()
			changed := r.digests[key] != digest
			relevant := triggers[root] || r.active[key] > 0 || r.jobs[key] != nil
			r.mu.Unlock()
			if !changed || !relevant {
				continue
			}
			if !r.discoveredJob(ctx, epoch, key, *w, false) {
				return
			}
			r.mu.Lock()
			if epoch == r.epoch {
				r.digests[key] = digest
			}
			r.mu.Unlock()
		}
		r.mu.Lock()
		if epoch == r.epoch {
			r.scanned[ch.ID] = time.Now()
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	var lag time.Duration
	for ch, when := range r.scanned {
		if _, ok := channelMap[ch]; ok {
			lag = max(lag, time.Since(when))
		}
	}
	r.replayLag.Store(int64(lag))
	r.mu.Unlock()
	success = true
}
