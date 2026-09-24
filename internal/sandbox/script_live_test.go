//go:build live

package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/agentbox"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
)

type activeRun struct{ run conversation.Run }

func (a activeRun) Run(context.Context, string, string) (conversation.Run, error) { return a.run, nil }

// Exercise sandbox file hooks independently of the Orpheus worker and model.
func TestLiveSandboxFileHooks(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" || os.Getenv("MM_TEST_URL") == "" {
		t.Skip("manual live environment is not configured")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	mm := mattermost.New(os.Getenv("MM_TEST_URL"), os.Getenv("MATTERMOST_BOT_TOKEN"), 30*time.Second)
	channel, err := mm.Channel(ctx, os.Getenv("MM_TEST_CHANNEL_ID"))
	if err != nil || channel.Name != "dev-test-group" {
		t.Fatal("test channel guard", err)
	}
	bot, err := mm.Me(ctx)
	if err != nil || bot.ID != os.Getenv("MM_TEST_BOT_ID") {
		t.Fatal("test bot guard", err)
	}
	run := conversation.Run{ID: uuid.NewString(), Status: "running"}
	access, err := agentbox.New(config.Config{AgentBoxAPIURL: os.Getenv("AGENTBOX_API_URL")}, activeRun{run}, mm)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := access.SDK.Sandboxes.Create(ctx, &sdk.CreateSandboxOptions{Template: "codex", Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := remote.ID
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = access.SDK.Sandboxes.Kill(cleanup, sandboxID)
	})
	const workspace = "/home/user/workspace"
	result, err := remote.Commands.Run(ctx, "/bin/sh", &sdk.CommandOptions{User: "user", Args: []string{"-c", "mkdir -p /home/user/workspace && python3 --version"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("stock codex template:", string(result.Stdout), "envd:", remote.EnvdVersion)
	upload, err := mm.Upload(ctx, channel.ID, "hook-input.txt", bytes.NewReader([]byte("input")))
	if err != nil {
		t.Fatal(err)
	}
	root, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, Message: "[orpheus-mattermost live test] Python file hooks; automatic cleanup.", FileIDs: []string{upload.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		posts, e := mm.Thread(cleanup, root.ID)
		if e == nil {
			for i := len(posts) - 1; i >= 0; i-- {
				_ = mm.Delete(cleanup, posts[i].ID)
			}
		}
	})
	session := conversation.Session{ID: uuid.NewString(), SandboxID: remote.ID, Workspace: workspace}
	req := attachments.Request{Schema: 1, BotID: bot.ID, SourceID: "live-python", ChannelID: channel.ID, RootID: root.ID, AnchorID: root.ID, Limits: config.Files{MaxPerPost: 5, MaxFileBytes: 1 << 20, MaxImageBytes: 1 << 20, MaxBatchBytes: 1 << 20, MaxOutputFiles: 5, MaxOutputBytes: 1 << 20}, Files: []attachments.InputFile{{PostID: root.ID, FileID: upload.ID, ChannelID: channel.ID, Name: upload.Name, Path: attachments.InputPath(upload.ID, upload.Name), Size: upload.Size, MIME: upload.MIME, Origin: "thread"}}}
	env := map[string]string{"ORPHEUS_WORKSPACE_PATH": workspace, "ORPHEUS_RUN_ID": run.ID, "ORPHEUS_SESSION_ID": session.ID, "MM_BASE_URL": os.Getenv("MM_TEST_URL"), "MM_TOKEN_ENV": "MATTERMOST_BOT_TOKEN", "MATTERMOST_BOT_TOKEN": os.Getenv("MATTERMOST_BOT_TOKEN"), "MM_EXPECTED_BOT_ID": bot.ID, "MM_SOURCE_ID": req.SourceID, "MM_CHANNEL_ID": channel.ID}
	hook := func(script string) []byte {
		t.Helper()
		raw, _ := json.Marshal(req)
		env["MM_INPUT_MANIFEST"] = string(raw)
		result, e := remote.Commands.Run(ctx, "/bin/sh", &sdk.CommandOptions{User: "user", Cwd: workspace, Args: []string{"-c", script}, Env: env})
		if e != nil {
			t.Fatal("file hook failed", e)
		}
		return result.Stdout
	}
	hook(sandbox.BeforeRun())
	t.Log("before_run downloaded the Mattermost attachment without template installation")
	// Import uses the connector's actual SDK path, not a test-specific file upload.
	clarification, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, RootID: root.ID, Message: "File clarification hook test."})
	if err != nil {
		t.Fatal(err)
	}
	follow := req
	follow.AnchorID = clarification.ID
	if err = access.Prepare(ctx, session, run, config.Workflow{HookTimeoutSeconds: 60}, follow); err != nil {
		t.Fatal("SDK import", err)
	}
	t.Log("SDK clarification stream imported")
	outputPath := path.Join(workspace, attachments.OutputPath(run.ID), "outbox", "report.txt")
	if _, err = remote.Files.WriteBytes(ctx, outputPath, []byte("output"), &sdk.WriteFileOptions{User: "user"}); err != nil {
		t.Fatal(err)
	}
	var output attachments.Output
	if err = json.Unmarshal(hook(sandbox.AfterRun()), &output); err != nil {
		t.Fatal(err)
	}
	if err = output.Validate(attachments.Export{SourceID: req.SourceID, BotID: bot.ID, ChannelID: channel.ID, SessionID: session.ID, RunID: run.ID, Limits: req.Limits}); err != nil || len(output.Files) != 1 {
		t.Fatal("output contract", err)
	}
	if data, err := mm.Download(ctx, output.Files[0].FileID, 100); err != nil || string(data) != "output" {
		t.Fatal("output content", err)
	}
	post, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, RootID: root.ID, Message: "Python hook output.", FileIDs: []string{output.Files[0].FileID}})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := mm.File(ctx, output.Files[0].FileID); err != nil || info.PostID != post.ID {
		t.Fatal("output association", err)
	}
	t.Log("after_run uploaded output and attached it to a Mattermost post")
	if err = remote.Pause(ctx, &sdk.PauseOptions{Memory: new(true)}); err != nil {
		t.Fatal(err)
	}
	remote, err = access.SDK.Sandboxes.Connect(ctx, remote.ID, &sdk.ConnectSandboxOptions{Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req.PreviousIndex = attachments.IndexPath(run.ID)
	req.DeliveredBatches = []string{attachments.ManifestPath(clarification.ID)}
	req.AnchorID = clarification.ID
	env["ORPHEUS_RUN_ID"] = uuid.NewString()
	hook(sandbox.BeforeRun())
	result, err = remote.Commands.Run(ctx, "/bin/cat", &sdk.CommandOptions{User: "user", Args: []string{path.Join(workspace, req.Files[0].Path)}})
	if err != nil || string(result.Stdout) != "input" {
		t.Fatal("resumed input", err)
	}
	t.Log("memory pause/resume retained files and continued the input index")
}
