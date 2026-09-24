package orpheus

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestPaginationBeyond200Items(t *testing.T) {
	for _, kind := range []string{"sessions", "runs", "history"} {
		t.Run(kind, func(t *testing.T) {
			sid, rid := uuid.NewString(), uuid.NewString()
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				offset := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					var err error
					offset, err = strconv.Atoi(cursor)
					if err != nil {
						t.Error(err)
					}
				}
				if r.URL.Query().Get("limit") != "200" || offset != (pages-1)*200 {
					t.Error("unexpected pagination", r.URL.RawQuery)
				}
				items := []any{}
				for i := offset; i < min(offset+200, 405); i++ {
					id := uuid.NewSHA1(uuid.Nil, []byte(strconv.Itoa(i))).String()
					var item any
					switch kind {
					case "sessions":
						item = map[string]any{"id": id, "namespace": "mattermost/assistant", "created_at": time.Unix(1, 0), "configuration": map[string]any{"limits": map[string]int{"max_session_tokens": 100}}}
					case "runs":
						item = map[string]any{"id": id, "session_id": sid, "number": i + 1, "status": "completed"}
					case "history":
						item = map[string]any{"type": "message", "message": map[string]any{"id": id, "run_id": rid, "role": "assistant", "kind": "answer", "text": strconv.Itoa(i), "position": map[string]int{"item_index": i}}}
					}
					items = append(items, item)
				}
				var next *string
				if offset+200 < 405 {
					next = new(strconv.Itoa(offset + 200))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": next, "event_cursor": "500"})
			}))
			defer server.Close()
			client, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}}, "token")
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			switch kind {
			case "sessions":
				items, e := client.Sessions(t.Context(), "mattermost/assistant", "")
				err = e
				for _, item := range items {
					ids = append(ids, item.ID)
				}
			case "runs":
				items, e := client.Runs(t.Context(), sid)
				err = e
				for _, item := range items {
					ids = append(ids, item.ID)
				}
			case "history":
				items, e := client.History(t.Context(), sid)
				err = e
				for _, item := range items {
					ids = append(ids, item.ID)
				}
			}
			if err != nil || len(ids) != 405 || pages != 3 {
				t.Fatalf("items=%d pages=%d err=%v", len(ids), pages, err)
			}
			for i, id := range ids {
				if id != uuid.NewSHA1(uuid.Nil, []byte(strconv.Itoa(i))).String() {
					t.Fatal("item skipped, reordered or duplicated", i)
				}
			}
		})
	}
}

func TestSubmitUsesPinnedClientAndStableKey(t *testing.T) {
	var bodies [][]byte
	var keys []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private" {
			t.Error("missing bearer")
		}
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		b, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
		}
		bodies = append(bodies, b)
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": uuid.NewString(), "run_id": uuid.NewString(), "message_id": uuid.NewString()})
	}))
	defer s.Close()
	t.Setenv("ORPHEUS_BASE_URL", "http://orpheus:8080")
	t.Setenv("WORKFLOWS_DIR", "../../workflows")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orpheus.BaseURL = s.URL
	c, err := New(cfg, "private")
	if err != nil {
		t.Fatal(err)
	}
	env := conversation.Envelope{Schema: 1, Source: "chat", Workflow: "assistant", Channel: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Root: "bbbbbbbbbbbbbbbbbbbbbbbbbb", Anchor: "cccccccccccccccccccccccccc", Kind: "initial", Render: conversation.Render{Version: 1, MaxChars: 64}, TriggerIDs: []string{"cccccccccccccccccccccccccc"}, Request: attachments.Request{Schema: 1, SourceID: "chat"}}
	text, _ := env.Encode("hello")
	for range 2 {
		if _, err = c.Submit(t.Context(), cfg.Workflows[0], env, text, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if keys[0] != keys[1] || string(bodies[0]) != string(bodies[1]) {
		t.Fatal("retry changed request")
	}
	if strings.Contains(string(bodies[0]), "private") {
		t.Fatal("bearer included in request body")
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(bodies[0], &body)
	if len(body["env_from"]) == 0 || len(body["input_fingerprint"]) == 0 {
		t.Fatal("run environment or fingerprint missing")
	}
}
func TestSessionPagination(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if pages == 2 && r.URL.Query().Get("cursor") != "next" {
			t.Error("cursor not propagated")
		}
		var next *string
		if pages == 1 {
			next = new("next")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": uuid.NewString(), "namespace": "mattermost/assistant", "created_at": time.Now(), "configuration": map[string]any{"limits": map[string]int{"max_session_tokens": 100}}}}, "next_cursor": next})
	}))
	defer server.Close()
	c, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}, HTTPTimeout: config.Duration(time.Second)}, "token")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := c.Sessions(t.Context(), "mattermost/assistant", "")
	if err != nil || len(sessions) != 2 || pages != 2 {
		t.Fatal(len(sessions), pages, err)
	}
}
func TestErrorsDoNotEchoServerSecrets(t *testing.T) {
	res := &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"bad", "message":"TOP_SECRET"}}`))}
	_, err := decode[map[string]any](res, nil)
	if err == nil || strings.Contains(err.Error(), "TOP_SECRET") || Code(err) != "bad" {
		t.Fatal(err)
	}
}

func TestSSEReconnectRetainsCursor(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		want := "0"
		if calls > 1 {
			want = "42"
		}
		if r.URL.Query().Get("after") != want {
			t.Error("SSE cursor not retained")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			_, _ = w.Write([]byte("id: 42\ndata: {}\n\n"))
		}
	}))
	defer s.Close()
	c, e := New(config.Config{Orpheus: config.Endpoint{BaseURL: s.URL}}, "token")
	if e != nil {
		t.Fatal(e)
	}
	sid := uuid.NewString()
	notifications := 0
	for range 2 {
		if e = c.Watch(t.Context(), sid, func() { notifications++ }); e != nil {
			t.Fatal(e)
		}
	}
	if notifications != 1 {
		t.Fatal(notifications)
	}
}

func TestHistoryWatermarkSeedsFirstSSESubscription(t *testing.T) {
	history := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/history") {
			history++
			var next *string
			cursor := "17"
			if history == 1 {
				next = new("page2")
			} else {
				cursor = "21"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "event_cursor": cursor, "next_cursor": next})
			return
		}
		if r.URL.Query().Get("after") != "17" {
			t.Error("SSE did not start from history snapshot watermark", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()
	c, err := New(config.Config{Orpheus: config.Endpoint{BaseURL: server.URL}}, "token")
	if err != nil {
		t.Fatal(err)
	}
	sid := uuid.NewString()
	if _, err = c.History(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	if err = c.Watch(t.Context(), sid, func() {}); err != nil {
		t.Fatal(err)
	}
}
