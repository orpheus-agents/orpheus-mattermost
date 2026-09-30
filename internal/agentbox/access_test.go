package agentbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func TestImportDiagnosticAcceptsOnlyBoundedExpectedLine(t *testing.T) {
	valid := "Mattermost file hook failed: stage=input_import; JSONDecodeError: invalid JSON\n"
	for _, test := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"valid", []string{valid}, strings.TrimSuffix(valid, "\n")},
		{"chunked", []string{"Mattermost file hook failed: stage=input_", "import; TimeoutError: operation timed out\n"}, "Mattermost file hook failed: stage=input_import; TimeoutError: operation timed out"},
		{"arbitrary", []string{"private token and traceback\n"}, ""},
		{"extra line", []string{valid, "private token\n"}, ""},
		{"carriage return", []string{strings.TrimSuffix(valid, "\n") + "\r\n"}, ""},
		{"too large", []string{strings.Repeat("x", importDiagnosticLimit+1)}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var diagnostic importDiagnostic
			for _, chunk := range test.chunks {
				diagnostic.add([]byte(chunk))
			}
			if got := diagnostic.message(); got != test.want {
				t.Fatalf("message = %q, want %q", got, test.want)
			}
		})
	}
}

func TestImportDiagnosticLogging(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	a := &Access{Logger: logger}
	valid := &importDiagnostic{}
	valid.add([]byte("Mattermost file hook failed: stage=input_import; JSONDecodeError: invalid JSON\n"))
	a.logImportFailure(t.Context(), conversation.Session{ID: "session"}, conversation.Run{ID: "run"}, "common", valid)
	text := output.String()
	for _, value := range []string{"Mattermost attachment import failed", "workflow=common", "session_id=session", "run_id=run", `diagnostic="Mattermost file hook failed: stage=input_import; JSONDecodeError: invalid JSON"`} {
		if !strings.Contains(text, value) {
			t.Fatalf("missing %q in log %q", value, text)
		}
	}
	output.Reset()
	invalid := &importDiagnostic{}
	invalid.add([]byte("private token\n"))
	a.logImportFailure(t.Context(), conversation.Session{ID: "session"}, conversation.Run{ID: "run"}, "common", invalid)
	if output.Len() != 0 {
		t.Fatalf("unexpected untrusted stderr log: %q", output.String())
	}
}

func TestNewRequiresAndKeepsLogger(t *testing.T) {
	t.Setenv("AGENTBOX_API_KEY", "test")
	if _, err := New(config.Config{}, &runs{}, nil, nil); err == nil || err.Error() != "logger is required" {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	access, err := New(config.Config{}, &runs{}, nil, logger)
	if err != nil || access.Logger != logger {
		t.Fatal(err)
	}
}
