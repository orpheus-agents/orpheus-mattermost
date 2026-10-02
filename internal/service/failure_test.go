package service

import (
	"fmt"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestFailureNoticeSurvivesReplay(t *testing.T) {
	for version := 1; version <= 4; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			channel := mattermost.Channel{ID: key.Channel, Type: "O"}
			if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			s := &api.snapshot.Sessions[0]
			message := &s.Messages[len(s.Messages)-1]
			env, err := conversation.Decode(message.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			if env.Render.Version != 4 {
				t.Fatalf("new run uses renderer %d", env.Render.Version)
			}
			env.Render.Version = version
			message.Metadata = testMetadata(env)
			run := &s.Runs[0]
			run.Status, run.Error = "failed", "harness_failed"
			want := "Run failed (harness_failed)."
			if version == 4 {
				run.ErrorMessage = "Selected model is at capacity. Please try a different model."
				want += "\n\n> " + run.ErrorMessage
			}
			if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			if mm.calls != 1 || mm.posts[len(mm.posts)-1].Message != want {
				t.Fatalf("calls=%d posts=%+v", mm.calls, mm.posts)
			}
			// A restarted connector now reads the full error, including for old runs.
			run.ErrorMessage = "Selected model is at capacity. Please try a different model."
			for range 2 {
				e = &Engine{Config: e.Config, MM: mm, API: api, Sandbox: e.Sandbox, Bot: e.Bot}
				if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
					t.Fatal(err)
				}
			}
			if mm.calls != 1 || len(api.submitted) != 1 {
				t.Fatalf("replay duplicated post or run: posts=%d runs=%d", mm.calls, len(api.submitted))
			}
		})
	}
}
