package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestExportFailuresPreserveAnswerAndPublishOneNotice(t *testing.T) {
	for _, failure := range []string{"missing", "failed", "truncated", "invalid-json", "uploading", "different-bot"} {
		t.Run(failure, func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			s := &api.snapshot.Sessions[0]
			r := &s.Runs[0]
			r.Status, r.AgentStatus, r.FinalMessageID = "completed", "completed", "final"
			s.Messages = append(s.Messages, conversation.Message{ID: "final", RunID: r.ID, Role: "assistant", Kind: "answer", Text: "available answer", Position: 1})
			output := attachments.Output{Schema: 1, SourceID: key.Source, BotID: mm.bot, ChannelID: key.Channel, SessionID: s.ID, RunID: r.ID, Stage: "ready"}
			if failure == "different-bot" {
				output.BotID = "other"
			}
			if failure == "uploading" {
				output.Stage = "uploading"
			}
			raw, _ := json.Marshal(output)
			hook := conversation.Hook{Name: "after_run", Status: "completed", Completeness: "complete", Output: string(raw)}
			switch failure {
			case "failed":
				hook.Status = "failed"
			case "truncated":
				hook.Completeness = "truncated"
			case "invalid-json":
				hook.Output = "{"
			}
			if failure != "missing" {
				r.Hooks = append(r.Hooks, hook)
			}
			for range 2 {
				e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
				if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
					t.Fatal(err)
				}
			}
			answers, notices := 0, 0
			for _, post := range mm.posts {
				var receipt delivery.Receipt
				if json.Unmarshal(post.Props[delivery.Property], &receipt) != nil {
					continue
				}
				if receipt.MessageID == "final" && post.Message == "available answer" {
					answers++
				}
				if receipt.MessageID == "attachments_failed" {
					notices++
				}
			}
			if mm.calls != 2 || answers != 1 || notices != 1 {
				t.Fatalf("calls=%d answers=%d notices=%d", mm.calls, answers, notices)
			}
		})
	}
}

type failedSnapshot struct {
	API
	err error
}

func (a failedSnapshot) Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error) {
	return conversation.Snapshot{}, a.err
}

func TestHTTPFailuresBackOffAndRespectRetryAfter(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			e, _, _, _, key := fixture(t)
			delay := time.Duration(0)
			if status == 429 {
				delay = time.Minute
			}
			e.API = failedSnapshot{err: &mattermost.HTTPError{Status: status, RetryAfter: delay}}
			job := &scheduled{Working: true}
			r := &Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler), jobs: map[conversation.Key]*scheduled{key: job}, running: 1, wake: make(chan struct{}, 1)}
			var wg sync.WaitGroup
			started := time.Now()
			r.work(t.Context(), t.Context(), &wg, key, e.Config.Workflows[0], mattermost.Channel{}, true)
			if job.Retry.Failures != 1 || job.Retry.Permanent != (status == 400 || status == 404 || status == 413) || job.Working || r.running != 0 || !job.Dirty {
				t.Fatal("failure left incorrect scheduler state", job.Retry)
			}
			if job.Retry.At.Before(started.Add(max(2*time.Second, delay))) {
				t.Fatal("retry would spin or ignore Retry-After")
			}
		})
	}
}

type outputSource struct {
	*fakeSource
	file mattermost.FileInfo
}

func (s outputSource) File(_ context.Context, id string) (mattermost.FileInfo, error) {
	if id != s.file.ID {
		return mattermost.FileInfo{}, &mattermost.HTTPError{Status: 404}
	}
	f := s.file
	for _, post := range s.posts {
		for _, fid := range post.FileIDs {
			if fid == id {
				f.PostID = post.ID
			}
		}
	}
	return f, nil
}

func TestFinalMessageOwnsFilesAndFailureRemainsVisible(t *testing.T) {
	for _, scenario := range []string{"multiple-answers", "commentary-disabled", "file-only", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			if err := e.Thread(t.Context(), w, key, mattermost.Channel{Type: "O"}, true); err != nil {
				t.Fatal(err)
			}
			s := api.snapshot.Sessions[0]
			r := s.Runs[0]
			r.Status, r.AgentStatus, r.FinalMessageID = "completed", "completed", "final"
			if scenario == "failed" {
				r.Status, r.Error = "failed", "test_failure"
			}
			env := api.submitted[0].Envelope
			env.Render.Commentary = scenario != "commentary-disabled"
			s.Messages[0].Text, _ = env.Encode("accepted input")
			finalText := "final answer"
			if scenario == "file-only" {
				finalText = ""
			}
			s.Messages = append(s.Messages,
				conversation.Message{ID: "progress", RunID: r.ID, Role: "assistant", Kind: "progress", Text: "working", Position: 1},
				conversation.Message{ID: "earlier", RunID: r.ID, Role: "assistant", Kind: "answer", Text: "earlier answer", Position: 2},
				conversation.Message{ID: "final", RunID: r.ID, Role: "assistant", Kind: "answer", Text: finalText, Position: 3})
			file := attachments.Artifact{FileID: "ffffffffffffffffffffffffff", Name: "report.txt", Path: "snapshot/report.txt", SHA256: attachments.Hash([]byte("result")), Size: 6}
			file.ArtifactID = attachments.ObjectHash([]string{r.ID, file.Name, file.SHA256})
			output := attachments.Output{Schema: 1, SourceID: key.Source, BotID: mm.bot, ChannelID: key.Channel, SessionID: s.ID, RunID: r.ID, Stage: "ready", Files: []attachments.Artifact{file}}
			raw, _ := json.Marshal(output)
			r.Hooks = []conversation.Hook{{Name: "after_run", Status: "completed", Completeness: "complete", Output: string(raw)}}
			e.MM = outputSource{fakeSource: mm, file: mattermost.FileInfo{ID: file.FileID, Name: file.Name, Size: file.Size}}
			for range 2 {
				p := &delivery.Publisher{MM: e.MM, Key: key, BotID: mm.bot}
				if err := p.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := e.publish(t.Context(), p, s, r); err != nil {
					t.Fatal(err)
				}
			}
			var messages []string
			for _, post := range mm.posts {
				var receipt delivery.Receipt
				if json.Unmarshal(post.Props[delivery.Property], &receipt) != nil {
					continue
				}
				messages = append(messages, receipt.MessageID)
				if (len(post.FileIDs) == 1) != (receipt.MessageID == "final") {
					t.Fatal("files attached to wrong message", receipt.MessageID)
				}
			}
			want := []string{"progress", "earlier", "final"}
			if scenario == "commentary-disabled" {
				want = want[1:]
			}
			if scenario == "failed" {
				want = append(want, "run_status")
			}
			if len(messages) != len(want) {
				t.Fatal("missing or duplicate messages", messages)
			}
			for i := range want {
				if messages[i] != want[i] {
					t.Fatal("message order", messages)
				}
			}
		})
	}
}
