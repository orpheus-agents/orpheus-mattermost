package orpheus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	api "github.com/orpheus-agents/orpheus/client"
)

func TestAccessContextUsesPublicAPIAndMetadata(t *testing.T) {
	type body struct {
		Access   json.RawMessage `json:"access_context"`
		Messages []struct {
			Text     string                `json:"text"`
			Metadata conversation.Envelope `json:"metadata"`
		} `json:"messages"`
		Env map[string]string `json:"env"`
	}
	var bodies []body
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b body
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, b)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": uuid.NewString(), "run_id": uuid.NewString(), "message_id": uuid.NewString()})
	}))
	defer server.Close()
	t.Setenv("ORPHEUS_BASE_URL", server.URL)
	t.Setenv("WORKFLOWS_DIR", "../../workflows")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, "server-only")
	if err != nil {
		t.Fatal(err)
	}
	e := conversation.Envelope{Source: "test", Workflow: "assistant", Channel: "channel", Root: "root", Anchor: "anchor"}
	e.InputSubjects = []api.AccessSubject{{Namespace: "mattermost:test", ID: "author"}, {Namespace: "mattermost:test", ID: "other"}}
	e.AccessContext = &api.AccessContext{InvocationID: e.Key().Invocation(e.Anchor), ContextObservedAt: time.Now().UTC(), Subject: e.InputSubjects[0], ScopeContext: api.AccessScopeContext{
		"visible_to": json.RawMessage(`[42,77]`),
		"requester":  json.RawMessage(`42`),
		"workspace":  json.RawMessage(`"test"`),
	}, AllowedInputSubjects: new(e.InputSubjects)}
	e.AccessProof = &conversation.AccessProof{AudienceSubjects: []string{"author", "other"}, ProjectionDigest: "projection-digest"}
	messages := []conversation.InputMessage{{Text: "first"}, {Text: "second"}}
	sid, rid := uuid.NewString(), uuid.NewString()
	for _, target := range [][2]string{{"", ""}, {sid, ""}, {sid, rid}} {
		if _, err := c.Submit(t.Context(), cfg.Workflows[0], e, messages, target[0], target[1], ""); err != nil {
			t.Fatal(err)
		}
	}
	for i, b := range bodies {
		if len(b.Messages) != 2 || b.Messages[0].Text != "first" || b.Messages[1].Text != "second" || !reflect.DeepEqual(b.Messages[1].Metadata.AccessContext, e.AccessContext) || !reflect.DeepEqual(b.Messages[1].Metadata.AccessProof, e.AccessProof) {
			t.Fatal("snapshot not persisted outside model text")
		}
		if i < 2 {
			var access api.AccessContext
			if json.Unmarshal(b.Access, &access) != nil || !reflect.DeepEqual(access, *e.AccessContext) {
				t.Fatal("new run lost snapshot")
			}
		} else {
			var access api.InputAccessContext
			if json.Unmarshal(b.Access, &access) != nil || !reflect.DeepEqual(access.InputSubjects, e.InputSubjects) {
				t.Fatal("clarification lost author order")
			}
		}
		for _, value := range b.Env {
			if value == "server-only" || value == e.AccessContext.InvocationID.String() {
				t.Fatal("authority exposed through env")
			}
		}
	}
	if keys[0] == keys[1] || keys[1] == keys[2] {
		t.Fatal("operations share idempotency keys")
	}
}
