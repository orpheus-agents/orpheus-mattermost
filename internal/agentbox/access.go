// Package agentbox owns short, explicitly scoped direct sandbox access.
package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
)

var ErrRunEnded = errors.New("run no longer accepts clarification")

type Runs interface {
	Run(context.Context, string, string) (conversation.Run, error)
}
type Access struct {
	SDK    *sdk.Client
	Runs   Runs
	Source attachments.InputSource
}

func New(c config.Config, runs Runs, source attachments.InputSource) (*Access, error) {
	key, e := config.Secret("AGENTBOX_API_KEY")
	if e != nil {
		return nil, e
	}
	opts := []sdk.ClientOption{sdk.WithAPIKey(key)}
	if c.AgentBoxAPIURL != "" {
		opts = append(opts, sdk.WithAPIURL(c.AgentBoxAPIURL))
	}
	client, e := sdk.NewClient(opts...)
	return &Access{SDK: client, Runs: runs, Source: source}, e
}
func (a *Access) connect(ctx context.Context, s conversation.Session, r conversation.Run, w config.Workflow) (*sdk.Sandbox, error) {
	if s.SandboxID == "" || s.Workspace == "" {
		return nil, errors.New("sandbox unavailable")
	}
	info, e := a.SDK.Sandboxes.Info(ctx, s.SandboxID)
	if e != nil {
		return nil, e
	}
	timeout := time.Duration(w.HookTimeoutSeconds+60) * time.Second
	timeout = max(timeout, time.Until(info.EndAt)+time.Minute)
	if r.DeadlineAt != nil {
		timeout = max(timeout, time.Until(*r.DeadlineAt)+time.Duration(w.HookTimeoutSeconds+60)*time.Second)
	}
	return a.SDK.Sandboxes.Connect(ctx, s.SandboxID, &sdk.ConnectSandboxOptions{Timeout: timeout})
}
func accepts(r conversation.Run) bool { return r.Status == "running" }
func (a *Access) Prepare(ctx context.Context, s conversation.Session, r conversation.Run, w config.Workflow, req attachments.Request) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(w.HookTimeoutSeconds)*time.Second)
	defer cancel()
	if !accepts(r) {
		return ErrRunEnded
	}
	fresh, e := a.Runs.Run(ctx, s.ID, r.ID)
	if e != nil {
		return e
	}
	if !accepts(fresh) {
		return ErrRunEnded
	}
	box, e := a.connect(ctx, s, fresh, w)
	if e != nil {
		return e
	}
	fresh, e = a.Runs.Run(ctx, s.ID, r.ID)
	if e != nil {
		return e
	}
	if !accepts(fresh) {
		return ErrRunEnded
	}
	if e = req.Validate(); e != nil {
		return e
	}
	handle, e := box.Commands.Start(ctx, "python3", &sdk.CommandOptions{Args: []string{"-I", "-c", sandbox.Bootstrap(), "import-input"}, User: "user", Cwd: s.Workspace, Env: map[string]string{"ORPHEUS_WORKSPACE_PATH": s.Workspace, "MM_IMPORT_TIMEOUT": strconv.Itoa(w.HookTimeoutSeconds)}, Stdin: true, Streaming: &sdk.CommandStreamingOptions{}})
	if e != nil {
		return e
	}
	stdinClosed := false
	defer func() {
		if !stdinClosed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = handle.CloseStdin(cleanup)
			cancel()
		}
		_ = handle.Close()
	}()
	write := func(data []byte) error {
		for len(data) > 0 {
			n := min(len(data), 32<<10)
			if _, err := handle.Write(ctx, data[:n]); err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}
	record := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(data) >= 64<<10 {
			return errors.New("import record exceeds limit")
		}
		return write(append(data, '\n'))
	}
	if err := record(req); err != nil {
		return err
	}
	var total int64
	counts := map[string]int{}
	for _, file := range req.Files {
		counts[file.PostID]++
		if counts[file.PostID] > req.Limits.MaxPerPost {
			file.Status = "file_limit_exceeded"
		}
		var data []byte
		if file.Status == "" || file.Status == "ready" {
			if file.Size < 0 || file.Size > req.Limits.MaxBatchBytes-total {
				file.Status = "batch_limit_exceeded"
			} else {
				file, data = attachments.Fetch(ctx, a.Source, req, file)
				if file.Status == "ready" && file.Size > req.Limits.MaxBatchBytes-total {
					file.Status = "batch_limit_exceeded"
				}
			}
		}
		if err := record(file); err != nil {
			return err
		}
		if file.Status == "ready" {
			if err := write(data); err != nil {
				return err
			}
			total += file.Size
		}
	}
	if e = handle.CloseStdin(ctx); e != nil {
		return e
	}
	stdinClosed = true
	if _, e = handle.Wait(ctx); e != nil {
		return e
	}
	fresh, e = a.Runs.Run(ctx, s.ID, r.ID)
	if e != nil {
		return e
	}
	if !accepts(fresh) {
		return ErrRunEnded
	}
	return nil
}
