package service

import (
	"cmp"
	"slices"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

type workClass uint8

const (
	recoveryWork workClass = iota
	liveWork
	workClasses

	maxQueuedThreads = 4096
	liveQueueReserve = 512
	liveBurst        = 7
)

func (c workClass) String() string {
	if c == liveWork {
		return "live"
	}
	return "recovery"
}

type readyThread struct {
	key   conversation.Key
	class workClass
	order uint64
	since time.Time
}

// threadScheduler chooses runnable work, without inspecting sessions, messages,
// or delivery receipts. Runtime owns eligibility and serializes access.
type threadScheduler struct {
	liveStreak int
}

func (s *threadScheduler) take(ready []readyThread, slots int) []readyThread {
	if slots <= 0 {
		return nil
	}
	var queues [workClasses][]readyThread
	for _, item := range ready {
		queues[item.class] = append(queues[item.class], item)
	}
	for _, queue := range queues {
		slices.SortFunc(queue, func(a, b readyThread) int { return cmp.Compare(a.order, b.order) })
	}
	var picked []readyThread
	for len(picked) < slots {
		class := liveWork
		if len(queues[liveWork]) == 0 || s.liveStreak >= liveBurst && len(queues[recoveryWork]) > 0 {
			class = recoveryWork
		}
		if len(queues[class]) == 0 {
			break
		}
		picked = append(picked, queues[class][0])
		queues[class] = queues[class][1:]
		if class == liveWork {
			s.liveStreak = min(s.liveStreak+1, liveBurst)
		} else {
			s.liveStreak = 0
		}
	}
	return picked
}

func queueHasRoom(size int, class workClass) bool {
	limit := maxQueuedThreads
	if class == recoveryWork {
		limit -= liveQueueReserve
	}
	return size < limit
}

// request requires Runtime.mu. Repeated hints coalesce without moving a thread
// back in its queue. Promotion starts a new wait in the live class.
func (r *Runtime) request(j *scheduled, class workClass) {
	if !j.Dirty || class > j.Class {
		r.sequence++
		j.Order = r.sequence
		j.QueuedAt = time.Now()
	}
	j.Class = max(j.Class, class)
	j.Dirty = true
}

// runnable requires Runtime.mu. Retry delays are not queue waiting time.
func (r *Runtime) runnable(now time.Time) []readyThread {
	var ready []readyThread
	for key, j := range r.jobs {
		active := r.capacity.count(key) > 0
		poll := !j.Retry.Permanent || active
		if j.Working || now.Before(j.Retry.At) || !j.Dirty && (!poll || now.Before(j.Next)) {
			continue
		}
		class, since := j.Class, j.Next
		if active {
			class = liveWork
		}
		if j.Dirty {
			since = j.QueuedAt
		}
		if since.Before(j.Retry.At) {
			since = j.Retry.At
		}
		if since.IsZero() {
			since = now
		}
		ready = append(ready, readyThread{key: key, class: class, order: j.Order, since: since})
	}
	return ready
}
