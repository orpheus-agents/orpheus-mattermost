package service

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

// Delete wins between Snapshot and Submit, before the input is accepted.
type retentionRaceAPI struct {
	*fakeAPI
	rejected *pending
}

func (f *retentionRaceAPI) Submit(ctx context.Context, w config.Workflow, env conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	if f.rejected == nil {
		f.rejected = &pending{Workflow: w, Envelope: env, Messages: messages, Session: sid, Run: rid, Predecessor: pred}
		f.snapshot.Sessions[0].SandboxState = "deleting"
		return conversation.Accepted{}, &orpheus.Error{Status: 409, Code: "session_unavailable"}
	}
	return f.fakeAPI.Submit(ctx, w, env, messages, sid, rid, pred)
}

func TestRetentionRaceRebuildsUnacceptedContext(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_process", true: "restart"}[restart], func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			mm.posts[0].FileIDs = []string{"ffffffffffffffffffffffffff"}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			old := &api.snapshot.Sessions[0]
			old.SandboxState = "paused"
			old.Runs[0].Status = "completed"
			oldID, oldRun := old.ID, old.Runs[0].ID
			reply := mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus follow-up question", CreateAt: 4000, FileIDs: []string{"gggggggggggggggggggggggggg"}}
			mm.posts = append(mm.posts, reply)
			race := &retentionRaceAPI{fakeAPI: api}
			e.API = race
			if err := e.Thread(t.Context(), w, key, ch, true); orpheus.Code(err) != "session_unavailable" {
				t.Fatal(err)
			}
			if race.rejected == nil || race.rejected.Session != oldID || race.rejected.Envelope.Request.PreviousIndex == "" {
				t.Fatal("did not race a continuation")
			}
			if len(api.submitted) != 1 {
				t.Fatal("rejected input was recorded as accepted")
			}
			if _, ok := e.get(key); !ok {
				t.Fatal("unaccepted input was discarded before reconciliation")
			}
			if restart {
				e = &Engine{Config: e.Config, MM: mm, API: race, Bot: e.Bot}
			}
			api.snapshot.Sessions[0].SandboxState = "deleted"
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 2 || len(api.snapshot.Sessions) != 2 {
				t.Fatal("new request not recovered", len(api.submitted), len(api.snapshot.Sessions))
			}
			got := api.submitted[1]
			if got.Session != "" || got.Run != "" || got.Predecessor != oldRun || got.Envelope.Request.PreviousIndex != "" || len(got.Envelope.Request.DeliveredBatches) != 0 {
				t.Fatal("reused old sandbox state", got)
			}
			if !slices.Equal(got.Envelope.TriggerIDs, []string{reply.ID}) || len(got.Envelope.Request.Files) != 2 || !strings.Contains(joinedInput(got.Messages), "follow-up question") {
				t.Fatal("context or attachments not rebuilt", got.Envelope)
			}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 2 {
				t.Fatal("reconciliation duplicated the new request")
			}
		})
	}
}

func TestLostAcceptedRunIsNotRetriedAfterSandboxLoss(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_process", true: "restart"}[restart], func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			api.lost = true
			if err := e.Thread(t.Context(), w, key, ch, true); err == nil {
				t.Fatal("expected lost acceptance response")
			}
			s := &api.snapshot.Sessions[0]
			s.SandboxState = "unavailable"
			s.Runs[0].Status, s.Runs[0].Error = "failed", "sandbox_lost"
			if restart {
				e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
			}
			for range 2 {
				if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(api.submitted) != 1 || len(api.snapshot.Sessions) != 1 || mm.calls != 1 {
				t.Fatal("accepted failed run was duplicated or failure not published", len(api.submitted), mm.calls)
			}
			if _, ok := e.get(key); ok {
				t.Fatal("accepted input remained pending")
			}
			mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus try again", CreateAt: 4000})
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 2 || api.submitted[1].Session != "" {
				t.Fatal("new explicit input did not create a session")
			}
		})
	}
}

func TestDelayedRejectedClarificationRebuildsExpiredContext(t *testing.T) {
	for _, state := range []string{"deleting", "deleted", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			w := e.Config.Workflows[0]
			ch := mattermost.Channel{ID: key.Channel, Type: "O"}
			mm.posts[0].FileIDs = []string{"ffffffffffffffffffffffffff"}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			reply := mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", Message: "@orpheus ORIGINAL_CLARIFICATION", CreateAt: 4000, FileIDs: []string{"gggggggggggggggggggggggggg"}}
			mm.posts = append(mm.posts, reply)
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			old := &api.snapshot.Sessions[0]
			clarification := &old.Messages[len(old.Messages)-1]
			clarification.Delivery, clarification.Error = "rejected", "run_finished_before_delivery"
			predecessor := clarification.ID
			old.Runs[0].Status = "completed"
			old.SandboxState = state
			// Restart after a long outage. The original post may have changed or disappeared.
			mm.posts[1].Message = "@orpheus EDITED_CLARIFICATION"
			if state == "deleted" {
				mm.posts[1].DeleteAt = 5000
			}
			e = &Engine{Config: e.Config, MM: mm, API: api, Bot: e.Bot}
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 3 {
				t.Fatal("clarification was not transferred", len(api.submitted))
			}
			got := api.submitted[2]
			text := joinedInput(got.Messages)
			if got.Session != "" || got.Predecessor != predecessor || got.Envelope.Predecessor != predecessor || got.Envelope.Request.PreviousIndex != "" {
				t.Fatal("invalid new session transfer", got)
			}
			if !strings.Contains(text, "ORIGINAL_REQUEST_BODY") || !strings.Contains(text, "ORIGINAL_CLARIFICATION") || strings.Contains(text, "EDITED_CLARIFICATION") || len(got.Envelope.Request.Files) != 2 {
				t.Fatal("missing context or changed frozen input", text, got.Envelope.Request)
			}
			if !slices.Equal(got.Envelope.TriggerIDs, []string{reply.ID}) {
				t.Fatal("transferred other triggers", got.Envelope.TriggerIDs)
			}
			for range 2 {
				if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(api.submitted) != 3 {
				t.Fatal("clarification transferred twice")
			}
		})
	}
}
