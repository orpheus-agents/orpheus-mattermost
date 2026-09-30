package sandbox_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
)

const diagnosticID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
const diagnosticSecret = "private-token-and-response-contents"

func diagnosticEnv(t *testing.T, dir, baseURL string) string {
	t.Helper()
	run := uuid.NewString()
	req := attachments.Request{Schema: 1, BotID: diagnosticID, SourceID: "chat", ChannelID: diagnosticID, RootID: diagnosticID, AnchorID: diagnosticID, Limits: config.Files{MaxPerPost: 10, MaxFileBytes: 100, MaxImageBytes: 100, MaxBatchBytes: 100, MaxOutputFiles: 10, MaxOutputBytes: 100}}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"ORPHEUS_WORKSPACE_PATH": dir, "ORPHEUS_RUN_ID": run, "ORPHEUS_SESSION_ID": uuid.NewString(), "MM_INPUT_MANIFEST": string(raw), "MM_BASE_URL": baseURL, "MM_TOKEN_ENV": "MM_TEST_TOKEN", "MM_TEST_TOKEN": diagnosticSecret, "MM_EXPECTED_BOT_ID": diagnosticID, "MM_SOURCE_ID": "chat", "MM_CHANNEL_ID": diagnosticID} {
		t.Setenv(key, value)
	}
	return run
}

func diagnosticFailure(t *testing.T, script string, expected ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("failure wrote to protocol stdout: %s", stdout.String())
	}
	text := stderr.String()
	for _, part := range expected {
		if !strings.Contains(text, part) {
			t.Errorf("missing %q in %q", part, text)
		}
	}
	for _, forbidden := range []string{diagnosticSecret, "Traceback", os.Getenv("ORPHEUS_WORKSPACE_PATH"), os.Getenv("MM_BASE_URL")} {
		if forbidden != "" && strings.Contains(text, forbidden) {
			t.Errorf("diagnostic exposed private data: %q", text)
		}
	}
	if strings.Count(text, "\n") != 1 {
		t.Errorf("expected one diagnostic line: %q", text)
	}
}

func TestEmbeddedBeforeRunDiagnostics(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/api/v4/users/me" || r.Header.Get("Authorization") != "Bearer "+diagnosticSecret {
					t.Error("unexpected bot verification request")
				}
				w.Header().Set("X-Private-Header", diagnosticSecret)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(diagnosticSecret))
			}))
			defer server.Close()
			diagnosticEnv(t, t.TempDir(), server.URL)
			diagnosticFailure(t, sandbox.BeforeRun(), "stage=bot_verification", fmt.Sprintf("HTTPError: HTTP %d", status))
			if count := requests.Load(); count != 1 {
				t.Fatalf("unexpected request count: %d", count)
			}
		})
	}
	t.Run("invalid_json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(diagnosticSecret))
		}))
		defer server.Close()
		diagnosticEnv(t, t.TempDir(), server.URL)
		diagnosticFailure(t, sandbox.BeforeRun(), "stage=bot_verification", "JSONDecodeError: invalid JSON")
	})
	t.Run("missing_workspace", func(t *testing.T) {
		diagnosticEnv(t, filepath.Join(t.TempDir(), diagnosticSecret), "https://example.invalid/"+diagnosticSecret)
		diagnosticFailure(t, sandbox.BeforeRun(), "stage=script_installation", "FileNotFoundError: errno=2 (ENOENT)")
	})
	t.Run("invalid_manifest", func(t *testing.T) {
		diagnosticEnv(t, t.TempDir(), "https://example.invalid/"+diagnosticSecret)
		t.Setenv("MM_INPUT_MANIFEST", diagnosticSecret)
		diagnosticFailure(t, sandbox.BeforeRun(), "stage=input_request", "JSONDecodeError: invalid JSON")
	})
	t.Run("missing_token", func(t *testing.T) {
		diagnosticEnv(t, t.TempDir(), "https://example.invalid/"+diagnosticSecret)
		t.Setenv("MM_TOKEN_ENV", diagnosticSecret)
		diagnosticFailure(t, sandbox.BeforeRun(), "stage=mattermost_configuration", "KeyError")
	})
	t.Run("script_symlink", func(t *testing.T) {
		dir := t.TempDir()
		diagnosticEnv(t, dir, "https://example.invalid/"+diagnosticSecret)
		name := filepath.Join(dir, sandbox.Path())
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, diagnosticSecret), name); err != nil {
			t.Fatal(err)
		}
		diagnosticFailure(t, sandbox.BeforeRun(), "stage=script_installation", "workspace symlink rejected")
	})
}

func TestEmbeddedAfterRunDiagnostics(t *testing.T) {
	t.Run("missing_workspace_variable", func(t *testing.T) {
		diagnosticEnv(t, t.TempDir(), "https://example.invalid/"+diagnosticSecret)
		if err := os.Unsetenv("ORPHEUS_WORKSPACE_PATH"); err != nil {
			t.Fatal(err)
		}
		diagnosticFailure(t, sandbox.AfterRun(), "stage=script_loading", "ORPHEUS_WORKSPACE_PATH is missing")
	})
	t.Run("missing_script", func(t *testing.T) {
		diagnosticEnv(t, t.TempDir(), "https://example.invalid/"+diagnosticSecret)
		diagnosticFailure(t, sandbox.AfterRun(), "stage=script_loading", "FileNotFoundError: errno=2 (ENOENT)")
	})
	t.Run("modified_script", func(t *testing.T) {
		dir := t.TempDir()
		diagnosticEnv(t, dir, "https://example.invalid/"+diagnosticSecret)
		name := filepath.Join(dir, sandbox.Path())
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(diagnosticSecret), 0600); err != nil {
			t.Fatal(err)
		}
		diagnosticFailure(t, sandbox.AfterRun(), "stage=script_integrity", "script checksum mismatch")
	})
	t.Run("upload_failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me" {
				_, _ = fmt.Fprintf(w, `{"id":%q}`, diagnosticID)
				return
			}
			if r.Method != http.MethodPost || r.URL.Path != "/api/v4/files" {
				t.Error("unexpected request")
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(diagnosticSecret))
		}))
		defer server.Close()
		dir := t.TempDir()
		run := diagnosticEnv(t, dir, server.URL)
		cmd := exec.CommandContext(t.Context(), "sh", "-c", sandbox.BeforeRun())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("before_run: %v: %s", err, out)
		}
		file := filepath.Join(dir, attachments.OutputPath(run), "outbox", diagnosticSecret)
		if err := os.WriteFile(file, []byte(diagnosticSecret), 0600); err != nil {
			t.Fatal(err)
		}
		diagnosticFailure(t, sandbox.AfterRun(), "stage=output_export", "HTTPError: HTTP 403")
	})
}

func TestEmbeddedImportDiagnostics(t *testing.T) {
	diagnosticEnv(t, t.TempDir(), "https://example.invalid/"+diagnosticSecret)
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-c", sandbox.Bootstrap(), "import-input")
	cmd.Stdin = strings.NewReader(diagnosticSecret + "\n")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("expected rejected import, got %v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "stage=input_import; JSONDecodeError: invalid JSON") || strings.Contains(stderr.String(), diagnosticSecret) {
		t.Fatalf("invalid import diagnostic: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
