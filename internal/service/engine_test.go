package service

import (
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func input(kind string) conversation.Envelope {
	return conversation.Envelope{Schema: 1, Source: "chat", Workflow: "assistant", Channel: "channel", Root: "root", Anchor: "anchor", TriggerIDs: []string{"anchor"}, Kind: kind, Render: conversation.Render{Version: 1, MaxChars: 64}}
}
func TestTransferOnlyRejectedClarificationsAfterCompletion(t *testing.T) {
	for _, tc := range []struct {
		kind, status, stop string
		want               bool
	}{{"initial", "completed", "", false}, {"initial", "failed", "", false}, {"clarification", "failed", "", false}, {"clarification", "cancelled", "", false}, {"clarification", "completed", "token_limit", false}, {"clarification", "completed", "", true}} {
		t.Run(tc.kind+tc.status+tc.stop, func(t *testing.T) {
			env := input(tc.kind)
			text, _ := env.Encode("actual user request")
			snap := conversation.Snapshot{Sessions: []conversation.Session{{Messages: []conversation.Message{{ID: "message", RunID: "run", Role: "user", Delivery: "rejected", Error: "run_finished_before_delivery", Text: text}}, Runs: []conversation.Run{{ID: "run", Status: tc.status, StopReason: tc.stop}}}}}
			e, body, pred, ok := transfer(snap)
			if ok != tc.want {
				t.Fatal(ok)
			}
			if ok && (e.Kind != "initial" || body != "actual user request" || pred != "message") {
				t.Fatal("invalid transfer")
			}
			if ok {
				e.Predecessor = pred
				transferred, _ := e.Encode(body)
				snap.Sessions[0].Messages = append(snap.Sessions[0].Messages, conversation.Message{Text: transferred})
				if _, _, _, ok = transfer(snap); ok {
					t.Fatal("transferred twice")
				}
			}
		})
	}
}
func TestConflictingGenerationsBlock(t *testing.T) {
	snap := conversation.Snapshot{Sessions: []conversation.Session{{Runs: []conversation.Run{{Status: "running"}}}, {Runs: []conversation.Run{{Status: "accepted"}}}}}
	if _, _, e := active(snap); e == nil {
		t.Fatal("multiple active runs accepted")
	}
}
func TestSessionRotationIsNotIdleBased(t *testing.T) {
	s := conversation.Session{Revision: "v1", CreatedAt: time.Unix(0, 0), SandboxState: "paused", MaxTokens: 100, Runs: []conversation.Run{{Status: "completed"}}}
	if !s.Reusable("v1") {
		t.Fatal("idle session rotated")
	}
	for _, reason := range []string{"sandbox_lost", "context_lost"} {
		s.Runs[0].Error = reason
		if s.Reusable("v1") {
			t.Fatal(reason)
		}
	}
	s.Runs[0].Error = ""
	s.TotalTokens = 100
	if s.Reusable("v1") {
		t.Fatal("token budget ignored")
	}
}
func TestMissingHookNotEquivalentToEmptyExport(t *testing.T) {
	s := conversation.Session{ID: "session"}
	r := conversation.Run{ID: "run", AgentStatus: "completed"}
	if _, e := hookOutput(s, r, input("initial"), "bot"); e == nil {
		t.Fatal("missing hook considered empty")
	}
	r.AgentStatus = ""
	o, e := hookOutput(s, r, input("initial"), "bot")
	if e != nil || len(o.Files) != 0 {
		t.Fatal(o, e)
	}
}
