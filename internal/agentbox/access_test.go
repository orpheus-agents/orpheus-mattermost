package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

type runs struct {
	reads  int
	endOn  int
	status string
}

func (r *runs) Run(context.Context, string, string) (conversation.Run, error) {
	r.reads++
	status := r.status
	if r.endOn > 0 && r.reads >= r.endOn {
		status = "completed"
	}
	return conversation.Run{ID: "run", Status: status}, nil
}
func TestFinishDuringConnectNeverWritesOrPausesActiveRun(t *testing.T) {
	connects, pauses := 0, 0
	end := time.Now().Add(2 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/connect") {
			connects++
			var body struct {
				Timeout int `json:"timeout"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Timeout < 7200 {
				t.Error("connect shortened sandbox timeout")
			}
			_, _ = w.Write([]byte(`{"sandboxID":"box","templateID":"template","envdVersion":"0.4.0"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pause") {
			pauses++
			w.WriteHeader(204)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "box", "templateID": "template", "envdVersion": "0.4.0", "state": "running", "endAt": end})
	}))
	defer server.Close()
	client, err := sdk.NewClient(sdk.WithAPIURL(server.URL), sdk.WithAPIKey("test"))
	if err != nil {
		t.Fatal(err)
	}
	core := &runs{status: "running", endOn: 2}
	a := &Access{SDK: client, Runs: core}
	s := conversation.Session{ID: "session", SandboxID: "box", Workspace: "/home/user/workspace"}
	r := conversation.Run{ID: "run", Status: "running"}
	err = a.Prepare(t.Context(), s, r, config.Workflow{HookTimeoutSeconds: 120}, attachments.Request{Schema: 1})
	if !errors.Is(err, ErrRunEnded) || connects != 1 || pauses != 0 {
		t.Fatal(err, connects, pauses)
	}
}
func TestFinalizingNeverAllowsClarification(t *testing.T) {
	if accepts(conversation.Run{Status: "finalizing"}) || accepts(conversation.Run{Status: "completed"}) {
		t.Fatal("terminal run accepted input")
	}
}
