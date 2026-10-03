package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type uncertainServicesAPI struct {
	*fakeAPI
	attempts []config.Workflow
}

func (a *uncertainServicesAPI) Submit(ctx context.Context, w config.Workflow, e conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	a.attempts = append(a.attempts, w)
	if len(a.attempts) == 1 {
		return conversation.Accepted{}, errors.New("uncertain admission")
	}
	return a.fakeAPI.Submit(ctx, w, e, messages, sid, rid, pred)
}

func TestUncertainRequestKeepsServicesUntilAdmissionIsResolved(t *testing.T) {
	e, api, _, _, key := fixture(t)
	uncertain := &uncertainServicesAPI{fakeAPI: api}
	e.API = uncertain
	w := e.Config.Workflows[0]
	w.Services = []string{"gitlab"}
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err == nil {
		t.Fatal("expected uncertain admission")
	}
	w.Services = []string{"redmine"}
	w.EffectiveRevision = "changed-services"
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(uncertain.attempts) != 2 || !slices.Equal(uncertain.attempts[1].Services, []string{"gitlab"}) || uncertain.attempts[1].EffectiveRevision != uncertain.attempts[0].EffectiveRevision {
		t.Fatal("uncertain request was replaced with the current service selection")
	}
}

func TestRevisedWorkflowCanRejectAgainAndRecover(t *testing.T) {
	for _, codes := range [][2]string{
		{"unknown_profile", "unknown_template"},
		{"unknown_service", "validation_error"},
		{"unknown_service", "unknown_service"},
	} {
		t.Run(codes[0]+"-"+codes[1], func(t *testing.T) {
			e, api, mm, _, key := fixture(t)
			bad := &rejectedAPI{fakeAPI: api, code: codes[0]}
			e.API = bad
			w := e.Config.Workflows[0]
			channel := mattermost.Channel{ID: key.Channel, Type: "O"}
			for revision, code := range codes {
				bad.code = code
				w.EffectiveRevision = []string{"first", "second"}[revision]
				for range 3 {
					if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
						t.Fatal("revised rejection conflicts with previous receipt", err)
					}
				}
				if bad.calls != revision+1 || mm.calls != revision+1 {
					t.Fatal("rejection was not durable for each revision", bad.calls, mm.calls)
				}
				if revision == 0 {
					// Persisted receipts retain the rejection across process restarts.
					e = &Engine{Config: e.Config, MM: mm, API: bad, Bot: e.Bot}
					if err := e.Thread(t.Context(), w, key, channel, true); err != nil || bad.calls != revision+1 || mm.calls != revision+1 {
						t.Fatal("rejected input repeated after restart", err)
					}
				}
			}
			bad.code = ""
			w.EffectiveRevision = "fixed"
			if err := e.Thread(t.Context(), w, key, channel, true); err != nil || len(api.submitted) != 1 {
				t.Fatal("fixed workflow remains blocked", err)
			}
			if api.submitted[0].Workflow.EffectiveRevision != "fixed" {
				t.Fatal("recovery reused the rejected workflow")
			}
		})
	}
}
