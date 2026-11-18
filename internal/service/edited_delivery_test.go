package service

import (
	"slices"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestEditedAgentMessageDoesNotBlockFollowup(t *testing.T) {
	for _, kind := range []string{"answer", "progress"} {
		for _, restart := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same process", true: "/restart"}[restart], func(t *testing.T) {
				e, api, mm, _, key := fixture(t)
				w := e.Config.Workflows[0]
				channel := mattermost.Channel{ID: key.Channel, Type: "O"}
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal(err)
				}
				session := &api.snapshot.Sessions[0]
				run := &session.Runs[0]
				runID := run.ID
				if kind == "answer" {
					run.Status, run.FinalMessageID = "completed", "agent-message"
				}
				session.Messages = append(session.Messages, conversation.Message{ID: "agent-message", RunID: runID, Role: "assistant", Kind: kind, Text: "original agent text", Position: 1})
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal(err)
				}
				if mm.calls != 1 {
					t.Fatal("agent message was not published")
				}
				mm.posts[1].Message = "@orpheus corrected agent text"
				mm.posts[1].EditAt = 3000
				if restart {
					e = &Engine{Config: e.Config, MM: mm, API: api, Sandbox: e.Sandbox, Bot: e.Bot}
				}
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal("edited output blocked reconciliation", err)
				}
				if len(api.submitted) != 1 {
					t.Fatal("edited bot reply triggered a run")
				}
				followup := mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus next question", CreateAt: 4000}
				mm.posts = append(mm.posts, followup)
				for range 2 {
					if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
						t.Fatal(err)
					}
				}
				if len(api.submitted) != 2 || !slices.Equal(api.submitted[1].Envelope.TriggerIDs, []string{followup.ID}) {
					t.Fatal("human follow-up lost or duplicated", api.submitted)
				}
				if (api.submitted[1].Run == runID) != (kind == "progress") {
					t.Fatal("follow-up routed to the wrong run")
				}
				if mm.calls != 1 || mm.posts[1].Message != "@orpheus corrected agent text" {
					t.Fatal("edited output was overwritten or duplicated")
				}
			})
		}
	}
}

func TestEditingOldAnswerDoesNotBlockNewAnswer(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	session := &api.snapshot.Sessions[0]
	first := &session.Runs[0]
	first.Status, first.FinalMessageID = "completed", "first-answer"
	session.Messages = append(session.Messages, conversation.Message{ID: "first-answer", RunID: first.ID, Role: "assistant", Kind: "answer", Text: "original link", Position: 1})
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus fix the link", CreateAt: 4000})
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	mm.posts[1].Message, mm.posts[1].EditAt = "corrected link", 5000
	second := &session.Runs[1]
	second.Status, second.FinalMessageID = "completed", "second-answer"
	session.Messages = append(session.Messages, conversation.Message{ID: "second-answer", RunID: second.ID, Role: "assistant", Kind: "answer", Text: "the link is fixed", Position: len(session.Messages)})
	for range 2 {
		e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
		if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 2 || mm.posts[1].Message != "corrected link" || mm.posts[len(mm.posts)-1].Message != "the link is fixed" || len(api.submitted) != 2 {
		t.Fatal("old edit blocked new answer or replay duplicated work")
	}
}
