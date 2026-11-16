package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type selfTasksAPI struct {
	runtimeAPI
	threads map[conversation.Key]*fakeAPI
}

func (a *selfTasksAPI) Snapshot(ctx context.Context, key conversation.Key) (conversation.Snapshot, error) {
	if a.threads[key] == nil {
		a.threads[key] = &fakeAPI{}
	}
	return a.threads[key].Snapshot(ctx, key)
}

func (a *selfTasksAPI) Submit(ctx context.Context, w config.Workflow, env conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	return a.threads[env.Key()].Submit(ctx, w, env, messages, sid, rid, pred)
}

func TestSelfRootTasksThroughWebSocketAndReplay(t *testing.T) {
	for _, transport := range []string{"websocket", "REST"} {
		t.Run(transport, func(t *testing.T) {
			api := &selfTasksAPI{threads: map[conversation.Key]*fakeAPI{}}
			r := discoveryRuntime(t, api)
			w := r.cfg.Workflows[0]
			w.StartOnMention = new(false)
			w.Since = time.UnixMilli(500)
			r.cfg.Workflows[0] = w
			const channelID = "cccccccccccccccccccccccccc"
			channel := mattermost.Channel{ID: channelID, Type: "O"}
			mm := &fakeSource{bot: r.Engine.Bot.ID}
			for i, body := range []string{"@orpheus TASK_A", "@orpheus TASK_B", "PASSIVE", "`@orpheus` CODE", "@orpheus REPLY", "@orpheus DELETED", "@orpheus OLD", "@orpheus SYSTEM"} {
				mm.posts = append(mm.posts, mattermost.Post{ID: fmt.Sprintf("%026d", i+1), ChannelID: channelID, UserID: mm.bot, Message: body, CreateAt: 1000})
			}
			mm.posts[4].RootID = "rrrrrrrrrrrrrrrrrrrrrrrrrr"
			mm.posts[5].DeleteAt = 2000
			mm.posts[6].CreateAt = 100
			mm.posts[7].Type = "system_join_channel"
			source := runtimeSource{fakeSource: mm, channel: channelID}
			roots := []string{mm.posts[0].ID, mm.posts[1].ID}
			for pass := range 2 {
				if pass == 1 {
					// Restart with only Mattermost posts and accepted Orpheus history.
					r = discoveryRuntime(t, api)
					r.cfg.Workflows[0] = w
				}
				r.Engine.MM = source
				if transport == "websocket" && pass == 0 {
					r.channels[channelID] = channel
					for _, post := range mm.posts {
						r.event(mattermost.Event{Post: post})
					}
				} else {
					r.scan(t.Context(), r.cfg, nil, 0)
					if !r.ready.Load() {
						t.Fatal("REST replay failed")
					}
				}
				if len(r.jobs) != 2 {
					t.Fatalf("pass %d: expected two tasks, got %d", pass, len(r.jobs))
				}
				sessions := map[string]bool{}
				for _, root := range roots {
					key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: channelID, Root: root}
					job := r.jobs[key]
					if job == nil {
						t.Fatal("root was not routed to its own thread", key)
					}
					if err := r.Engine.Thread(t.Context(), w, key, channel, true); err != nil {
						t.Fatal(err)
					}
					state := api.threads[key]
					if len(state.submitted) != 1 || !slices.Equal(state.submitted[0].Envelope.TriggerIDs, []string{root}) || state.submitted[0].Session != "" {
						t.Fatal("task repeated or did not start a separate session", state.submitted)
					}
					body := joinedInput(state.submitted[0].Messages)
					if strings.Contains(body, "TASK_A") == strings.Contains(body, "TASK_B") {
						t.Fatal("tasks were mixed or lost", body)
					}
					session := &state.snapshot.Sessions[0]
					if sessions[session.ID] {
						t.Fatal("roots share a session")
					}
					sessions[session.ID] = true
					if pass == 0 {
						run := &session.Runs[0]
						run.Status = "completed"
						run.FinalMessageID = "answer-" + root
						session.Messages = append(session.Messages, conversation.Message{ID: run.FinalMessageID, RunID: run.ID, Role: "assistant", Kind: "answer", Text: "@orpheus TASK_ANSWER", Position: len(session.Messages)})
						if err := r.Engine.Thread(t.Context(), w, key, channel, true); err != nil {
							t.Fatal(err)
						}
						answer := mm.posts[len(mm.posts)-1]
						if answer.RootID != root || answer.UserID != mm.bot {
							t.Fatal("answer escaped its thread", answer)
						}
						job.Dirty = false
						r.event(mattermost.Event{Post: answer})
						if job.Dirty || len(state.submitted) != 1 {
							t.Fatal("own answer caused a self-trigger")
						}
					}
				}
			}
			if mm.calls != 2 {
				t.Fatal("answers were duplicated on restart", mm.calls)
			}
		})
	}
}

func TestSelfRootContinuationAfterSandboxLoss(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		t.Run(map[bool]string{false: "human follow-up", true: "rejected clarification transfer"}[transfer], func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			channel := mattermost.Channel{ID: key.Channel, Type: "O"}
			mm.posts[0].UserID = e.Bot.ID
			mm.posts[0].FileIDs = []string{"ffffffffffffffffffffffffff"}
			if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			old := &api.snapshot.Sessions[0]
			predecessor := old.Runs[0].ID
			reply := mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: 4000, Message: "@orpheus HUMAN_FOLLOW_UP", FileIDs: []string{"gggggggggggggggggggggggggg"}}
			mm.posts = append(mm.posts, reply)
			if transfer {
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal(err)
				}
				if len(api.submitted) != 2 || api.submitted[1].Envelope.Kind != "clarification" {
					t.Fatal("human follow-up was not submitted to the active run")
				}
				clarification := &old.Messages[len(old.Messages)-1]
				clarification.Delivery, clarification.Error = "rejected", "run_finished_before_delivery"
				predecessor = clarification.ID
				mm.posts[1].Message = "@orpheus EDITED_FOLLOW_UP"
			}
			old.Runs[0].Status = "completed"
			old.SandboxState = "deleted"
			mm.posts = append(mm.posts, mattermost.Post{ID: "tttttttttttttttttttttttttt", RootID: key.Root, ChannelID: key.Channel, UserID: e.Bot.ID, CreateAt: 3000, Message: "@orpheus BOT_ANSWER"})
			mattermost.Sort(mm.posts)
			e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
			before := len(api.submitted)
			if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != before+1 || len(api.snapshot.Sessions) != 2 {
				t.Fatal("follow-up did not start a new session")
			}
			got := api.submitted[before]
			if got.Session != "" || got.Predecessor != predecessor || got.Envelope.Request.PreviousIndex != "" || !slices.Equal(got.Envelope.TriggerIDs, []string{reply.ID}) {
				t.Fatal("invalid continuation or self-root retriggered", got)
			}
			body := joinedInput(got.Messages)
			if !strings.Contains(body, "ORIGINAL_REQUEST_BODY") || !strings.Contains(body, "HUMAN_FOLLOW_UP") || strings.Contains(body, "EDITED_FOLLOW_UP") || len(got.Envelope.Request.Files) != 2 {
				t.Fatal("self-root context, frozen follow-up or attachments lost", body, got.Envelope.Request)
			}
			for range 2 {
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(api.submitted) != before+1 {
				t.Fatal("continuation was duplicated")
			}
		})
	}
}
