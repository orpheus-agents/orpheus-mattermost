package service

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

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
