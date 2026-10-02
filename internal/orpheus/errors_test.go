package orpheus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestRunEndpointsPreserveErrorMessage(t *testing.T) {
	for _, tc := range []struct{ name, code, message string }{
		{"capacity", "harness_failed", "Selected model is at capacity.\nPlease try a different model."},
		{"without message", "harness_failed", ""},
		{"without error", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sid, rid := uuid.NewString(), uuid.NewString()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var problem any
				if tc.code != "" {
					problem = map[string]any{"code": tc.code, "message": tc.message, "phase": "execution", "details": []any{}}
				}
				run := map[string]any{"id": rid, "session_id": sid, "number": 1, "status": "failed", "error": problem}
				var body any = run
				if r.URL.Path == "/api/v1/sessions/"+sid+"/runs" {
					body = map[string]any{"items": []any{run}, "next_cursor": nil}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}}, "token")
			if err != nil {
				t.Fatal(err)
			}
			check := func(run conversation.Run) {
				t.Helper()
				if run.ID != rid || run.Error != tc.code || run.ErrorMessage != tc.message {
					t.Fatalf("lost run error: %+v", run)
				}
			}
			run, err := client.Run(t.Context(), sid, rid)
			if err != nil {
				t.Fatal(err)
			}
			check(run)
			runs, err := client.Runs(t.Context(), sid)
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%+v err=%v", runs, err)
			}
			check(runs[0])
		})
	}
}
