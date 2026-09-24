package sandbox_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
)

func TestEmbeddedHooksAndGoContract(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const fileID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := t.TempDir()
	run, session := uuid.NewString(), uuid.NewString()
	req := attachments.Request{Schema: 1, BotID: id, SourceID: "chat", ChannelID: id, RootID: id, AnchorID: id, Limits: config.Files{MaxPerPost: 5, MaxFileBytes: 100, MaxImageBytes: 100, MaxBatchBytes: 100, MaxOutputFiles: 5, MaxOutputBytes: 100}, Files: []attachments.InputFile{{PostID: id, FileID: fileID, ChannelID: id, Origin: "thread", Name: "<тест>.png", Path: attachments.InputPath(fileID, "<тест>.png"), MIME: "image/png", Size: 3}}}
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing token")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		case "/api/v4/posts/" + id:
			_, _ = fmt.Fprintf(w, `{"id":%q,"channel_id":%q,"file_ids":[%q]}`, id, id, fileID)
		case "/api/v4/files/" + fileID + "/info":
			_, _ = fmt.Fprintf(w, `{"id":%q,"post_id":%q,"size":3,"mime_type":"image/png"}`, fileID, id)
		case "/api/v4/files/" + fileID:
			_, _ = w.Write([]byte("png"))
		case "/api/v4/files":
			uploads++
			if err := r.ParseMultipartForm(4096); err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = r.MultipartForm.RemoveAll() }()
			file, head, err := r.FormFile("files")
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = file.Close() }()
			data, _ := io.ReadAll(file)
			if string(data) != "abc" || head.Filename != "<отчёт>&.txt" || r.FormValue("channel_id") != id {
				t.Error("upload changed")
			}
			_, _ = fmt.Fprintf(w, `{"file_infos":[{"id":%q,"size":3}]}`, fileID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(req)
	for key, value := range map[string]string{"ORPHEUS_WORKSPACE_PATH": dir, "ORPHEUS_RUN_ID": run, "ORPHEUS_SESSION_ID": session, "MM_INPUT_MANIFEST": string(raw), "MM_BASE_URL": server.URL, "MM_TOKEN_ENV": "MM_TEST_TOKEN", "MM_TEST_TOKEN": "test", "MM_EXPECTED_BOT_ID": id, "MM_SOURCE_ID": "chat", "MM_CHANNEL_ID": id} {
		t.Setenv(key, value)
	}
	hook := func(script string) []byte {
		t.Helper()
		if len(script) > 65536 {
			t.Fatal("hook exceeds Orpheus API limit")
		}
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook: %v: %s", err, out)
		}
		return out
	}
	hook(sandbox.BeforeRun())
	installed, err := os.ReadFile(filepath.Join(dir, sandbox.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if attachments.Hash(installed) != sandbox.Digest() {
		t.Fatal("wrong script installed")
	}
	var manifest attachments.Manifest
	raw, err = os.ReadFile(filepath.Join(dir, attachments.ManifestPath(id)))
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Files[0].Status != "ready" {
		t.Fatal("invalid input manifest", err)
	}
	frozen, _ := os.ReadFile(filepath.Join(dir, attachments.BatchPath(id), "request.json"))
	if attachments.Hash(frozen) != manifest.RequestHash {
		t.Fatal("request hash mismatch")
	}
	// Clarifications use the exact SDK command, with stdin reserved for records and raw bytes.
	var input bytes.Buffer
	_ = json.NewEncoder(&input).Encode(req)
	_ = json.NewEncoder(&input).Encode(manifest.Files[0])
	input.WriteString("png")
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-c", sandbox.Bootstrap(), "import-input")
	cmd.Stdin = &input
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("import: %v %s", err, out)
	}
	outbox := filepath.Join(dir, attachments.OutputPath(run), "outbox", "<отчёт>&.txt")
	if err = os.WriteFile(outbox, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	var output attachments.Output
	raw = hook(sandbox.AfterRun())
	if err = json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	if err = output.Validate(attachments.Export{SourceID: "chat", BotID: id, ChannelID: id, SessionID: session, RunID: run, Limits: req.Limits}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(outbox, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, hook(sandbox.AfterRun())) || uploads != 1 {
		t.Fatal("retry changed sealed result")
	}
	if err = os.WriteFile(filepath.Join(dir, sandbox.Path()), []byte("raise SystemExit(0)"), 0600); err != nil {
		t.Fatal(err)
	}
	tampered := exec.CommandContext(t.Context(), "sh", "-c", sandbox.AfterRun())
	if out, err := tampered.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("checksum mismatch")) {
		t.Fatal("modified script executed", err, string(out))
	}
	t.Logf("before_run: %d bytes; after_run: %d bytes; script: %d bytes", len(sandbox.BeforeRun()), len(sandbox.AfterRun()), len(installed))
}

func TestBootstrapCoexistsWithOldCodeAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORPHEUS_WORKSPACE_PATH", dir)
	old := filepath.Join(dir, ".orpheus/mattermost/code/old.py")
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old code"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-c", sandbox.Bootstrap(), "import-input")
	cmd.Stdin = strings.NewReader("invalid\n")
	if err := cmd.Run(); err == nil {
		t.Fatal("invalid import accepted")
	}
	data, err := os.ReadFile(old)
	if err != nil || string(data) != "old code" {
		t.Fatal("old code replaced")
	}
	target := filepath.Join(dir, sandbox.Path())
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(old, target); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(t.Context(), "python3", "-I", "-c", sandbox.Bootstrap(), "import-input")
	if err = cmd.Run(); err == nil {
		t.Fatal("symlink accepted")
	}
	data, err = os.ReadFile(old)
	if err != nil || string(data) != "old code" {
		t.Fatal("symlink target modified")
	}
}
