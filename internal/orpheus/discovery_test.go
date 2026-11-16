package orpheus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestDiscoveryPageFailureDiscardsPartialResult(t *testing.T) {
	to := time.Now().UTC()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if pages == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": uuid.NewString(), "namespace": "mattermost/assistant"}}, "next_cursor": "next",
			})
			return
		}
		if r.URL.Query().Get("cursor") != "next" {
			t.Error("missing second-page cursor")
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}}, "token")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := client.DiscoverSessions(t.Context(), "mattermost/assistant", to.Add(-21*24*time.Hour), to)
	if err == nil || len(sessions) != 0 || pages != 2 {
		t.Fatalf("partial discovery accepted: sessions=%d pages=%d err=%v", len(sessions), pages, err)
	}
}

func TestSnapshotKeepsOldGenerationsAfterDiscovery(t *testing.T) {
	key := conversation.Key{Source: "chat", Workflow: "assistant", Channel: strings.Repeat("a", 26), Root: strings.Repeat("b", 26)}
	oldID, recentID := uuid.NewString(), uuid.NewString()
	to := time.Now().UTC()
	old := to.Add(-60 * 24 * time.Hour)
	discovered, snapshotted := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var items []any
		switch {
		case r.URL.Path == "/api/v1/sessions":
			ids := []string{oldID, recentID}
			q := r.URL.Query()
			if q.Has("external_key") {
				snapshotted = true
				if q.Get("external_key") != key.External() || q.Has("last_run_created_from") || q.Has("last_run_created_to") {
					t.Error("thread snapshot inherited discovery filters", r.URL.RawQuery)
				}
			} else {
				discovered = true
				if !q.Has("last_run_created_from") || !q.Has("last_run_created_to") {
					t.Error("unbounded namespace discovery")
				}
				ids = []string{recentID}
			}
			for _, id := range ids {
				items = append(items, map[string]any{"id": id, "namespace": key.Namespace(), "external_key": key.External(), "created_at": old})
			}
		case strings.HasSuffix(r.URL.Path, "/runs"):
			sid := strings.Split(r.URL.Path, "/")[4]
			for number := 1; number <= 2; number++ {
				items = append(items, map[string]any{"id": uuid.NewString(), "session_id": sid, "number": number, "status": "completed", "created_at": old})
			}
		case strings.HasSuffix(r.URL.Path, "/history"):
			items = []any{map[string]any{"type": "message", "message": map[string]any{"id": uuid.NewString(), "role": "assistant", "kind": "answer", "text": "old answer", "created_at": old}}}
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	defer server.Close()
	client, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}}, "token")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := client.DiscoverSessions(t.Context(), key.Namespace(), to.Add(-7*24*time.Hour), to)
	if err != nil || len(sessions) != 1 || sessions[0].ID != recentID {
		t.Fatalf("discovery: sessions=%v err=%v", sessions, err)
	}
	snapshot, err := client.Snapshot(t.Context(), key)
	if err != nil || len(snapshot.Sessions) != 2 || !discovered || !snapshotted {
		t.Fatalf("snapshot: sessions=%v err=%v", snapshot.Sessions, err)
	}
	for _, session := range snapshot.Sessions {
		if len(session.Runs) != 2 || len(session.Messages) != 1 || session.Messages[0].Text != "old answer" {
			t.Fatalf("old run or message history was truncated: %+v", session)
		}
	}
}
