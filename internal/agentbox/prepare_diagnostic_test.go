package agentbox

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	sandboxhooks "github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
)

// emittedImportDiagnostic uses the real embedded script so this test also pins
// the contract between its stderr format and the connector's strict filter.
func emittedImportDiagnostic(t *testing.T) string {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing", "workspace")
	cmd := exec.CommandContext(t.Context(), "sh", "-c", sandboxhooks.BeforeRun())
	cmd.Env = append(os.Environ(), "ORPHEUS_WORKSPACE_PATH="+missing)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("diagnostic probe succeeded")
	}
	line := string(output)
	if strings.Count(line, "\n") != 1 || !strings.HasPrefix(line, "Mattermost file hook failed: stage=script_installation; ") {
		t.Fatalf("unexpected embedded diagnostic: %q", line)
	}
	return line
}

// fakeEnvd models envd removing an exited process from its live process map.
// SendInput and CloseStdin then return not_found even though the start stream
// still carries the process's final stderr and exit event.
func fakeEnvd(t *testing.T, exit, diagnostic string) *httptest.Server {
	t.Helper()
	exited, closed := make(chan struct{}), make(chan struct{})
	var exitOnce, closeOnce sync.Once
	gone := func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	unary := func(w http.ResponseWriter, missing bool) {
		w.Header().Set("Content-Type", "application/json")
		if missing {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"process with pid 42 not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/connect+json")
		send := func(flags byte, payload string) {
			var head [5]byte
			head[0] = flags
			binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
			_, _ = w.Write(append(head[:], payload...))
			w.(http.Flusher).Flush()
		}
		send(0, `{"event":{"start":{"pid":42}}}`)
		if exit == "after_eof" {
			<-closed
		}
		if exit == "before_close" {
			time.Sleep(100 * time.Millisecond)
		}
		send(0, fmt.Sprintf(`{"event":{"data":{"stderr":%q}}}`, base64.StdEncoding.EncodeToString([]byte(diagnostic))))
		send(0, `{"event":{"end":{"exited":true,"exitCode":1,"status":"exited"}}}`)
		exitOnce.Do(func() { close(exited) })
		send(2, `{}`)
	})
	mux.HandleFunc("/process.Process/SendInput", func(w http.ResponseWriter, _ *http.Request) {
		if exit == "before_write" {
			<-exited
		}
		unary(w, gone())
	})
	mux.HandleFunc("/process.Process/CloseStdin", func(w http.ResponseWriter, _ *http.Request) {
		if exit == "before_close" {
			<-exited
		}
		missing := gone()
		closeOnce.Do(func() { close(closed) })
		unary(w, missing)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/connect") {
			_, _ = w.Write([]byte(`{"sandboxID":"box","templateID":"template","envdVersion":"0.4.0"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "box", "templateID": "template", "envdVersion": "0.4.0", "state": "running", "endAt": time.Now().Add(2 * time.Hour)})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestPrepareLogsEmbeddedDiagnosticForEveryProcessExitPoint(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	diagnostic := strings.TrimSuffix(emittedImportDiagnostic(t), "\n")
	for _, exit := range []string{"after_eof", "before_write", "before_close"} {
		t.Run(exit, func(t *testing.T) {
			server := fakeEnvd(t, exit, diagnostic+"\n")
			client, err := sdk.NewClient(sdk.WithAPIURL(server.URL), sdk.WithSandboxURL(server.URL), sdk.WithAPIKey("test"))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			access := &Access{SDK: client, Runs: &runs{status: "running"}, Logger: logger}
			session := conversation.Session{ID: "session", SandboxID: "box", Workspace: "/home/user/workspace"}
			request := attachments.Request{Schema: 1, SourceID: "chat", ChannelID: id, RootID: id, AnchorID: id, Limits: config.Files{MaxPerPost: 10, MaxFileBytes: 100, MaxImageBytes: 100, MaxBatchBytes: 100}}
			err = access.Prepare(t.Context(), session, conversation.Run{ID: "run", Status: "running"}, config.Workflow{ID: "common", HookTimeoutSeconds: 120}, request)
			if err == nil {
				t.Fatal("failed import succeeded")
			}
			var entry map[string]any
			decoder := json.NewDecoder(&output)
			if err := decoder.Decode(&entry); err != nil {
				t.Fatalf("diagnostic is not JSON: %v: %q", err, output.String())
			}
			if entry["msg"] != "Mattermost attachment import failed" || entry["workflow"] != "common" || entry["session_id"] != "session" || entry["run_id"] != "run" || entry["diagnostic"] != diagnostic {
				t.Fatalf("unexpected diagnostic log: %#v", entry)
			}
			if decoder.Decode(&map[string]any{}) != io.EOF {
				t.Fatalf("unexpected extra log output: %q", output.String())
			}
		})
	}
}
