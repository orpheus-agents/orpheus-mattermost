package service

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestSuspendedThreadMetrics(t *testing.T) {
	for _, withSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_labels=%v", withSource), func(t *testing.T) {
			e, _, _, _, key := fixture(t)
			r := &Runtime{Engine: e}
			sources := []metricSource{{runtime: r, source: "one"}}
			if withSource {
				sources = append(sources, metricSource{runtime: &Runtime{Engine: e}, source: "two"})
			}
			handler := metricsHandler(sources, withSource)
			assert := func(want int) {
				t.Helper()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
				body := rec.Body.String()
				label := ""
				if withSource {
					label = `{source="one"}`
					if !strings.Contains(body, "orpheus_mattermost_suspended_threads{source=\"two\"} 0\n") {
						t.Fatal("suspended thread counts leaked between sources", body)
					}
				}
				sample := fmt.Sprintf("orpheus_mattermost_suspended_threads%s %d\n", label, want)
				if rec.Code != 200 || !strings.Contains(body, "# TYPE orpheus_mattermost_suspended_threads gauge\n") || !strings.Contains(body, sample) {
					t.Fatal("incorrect suspended thread gauge", body)
				}
			}
			assert(0)
			second, transient := key, key
			second.Root, transient.Root = "second", "transient"
			r.mu.Lock()
			r.jobs = map[conversation.Key]*scheduled{
				key:       {Retry: retry{Permanent: true}},
				second:    {Retry: retry{Permanent: true}},
				transient: {Retry: retry{Failures: 1}},
			}
			r.mu.Unlock()
			assert(2)
			r.mu.Lock()
			r.jobs[key].Retry = retry{}
			r.mu.Unlock()
			assert(1)
			r.mu.Lock()
			delete(r.jobs, second)
			r.mu.Unlock()
			assert(0)
		})
	}
}

func TestSchedulerMetrics(t *testing.T) {
	for _, withSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("source=%v", withSource), func(t *testing.T) {
			e, _, _, _, key := fixture(t)
			r := &Runtime{Engine: e, jobs: map[conversation.Key]*scheduled{}, capacity: newRunCapacity()}
			now := time.Now()
			for i := range 4 {
				key.Root = fmt.Sprint(i)
				j := &scheduled{Class: liveWork, Dirty: true, QueuedAt: now.Add(-time.Second)}
				switch i {
				case 1:
					j.Class = recoveryWork
				case 2:
					j.Retry.At = now.Add(time.Hour)
				case 3:
					j.Working = true
				}
				r.jobs[key] = j
			}
			r.queueWait[liveWork].observe(2 * time.Second)
			r.queueWait[liveWork].observe(20 * time.Second)
			r.queueWait[recoveryWork].observe(0)
			r.queueRejections = 3
			sources := []metricSource{{runtime: r, source: "one"}}
			if withSource {
				sources = append(sources, metricSource{runtime: &Runtime{Engine: e}, source: "two"})
			}
			rec := httptest.NewRecorder()
			metricsHandler(sources, withSource).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			label, plain := "", ""
			if withSource {
				label = `,source="one"`
				plain = `{source="one"}`
			}
			samples := []string{
				fmt.Sprintf(`orpheus_mattermost_ready_threads{class="live"%s} 1`, label),
				fmt.Sprintf(`orpheus_mattermost_ready_threads{class="recovery"%s} 1`, label),
				fmt.Sprintf(`orpheus_mattermost_queue_wait_seconds_count{class="live"%s} 2`, label),
				fmt.Sprintf(`orpheus_mattermost_queue_wait_seconds_sum{class="live"%s} 22`, label),
				fmt.Sprintf(`orpheus_mattermost_queue_wait_seconds_bucket{class="live",le="5"%s} 1`, label),
				fmt.Sprintf(`orpheus_mattermost_queue_wait_seconds_bucket{class="live",le="60"%s} 2`, label),
				fmt.Sprintf(`orpheus_mattermost_schedule_rejections_total%s 3`, plain),
			}
			// Prometheus emits source before the synthetic histogram le label.
			if withSource {
				for i, line := range samples {
					line = strings.ReplaceAll(line, `,le="5",source="one"`, `,source="one",le="5"`)
					line = strings.ReplaceAll(line, `,le="60",source="one"`, `,source="one",le="60"`)
					samples[i] = line
				}
				samples = append(samples, `orpheus_mattermost_ready_threads{class="live",source="two"} 0`, `orpheus_mattermost_schedule_rejections_total{source="two"} 0`)
			}
			for _, sample := range samples {
				if !strings.Contains(rec.Body.String(), sample+"\n") {
					t.Fatal("missing sample", sample, rec.Body.String())
				}
			}
		})
	}
}
