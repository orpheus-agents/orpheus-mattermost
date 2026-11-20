package service

import (
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestOpeningProfileSurvivesBatchReplayAndRotation(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.ProfileByTag = map[string]string{"sl-deepseek": "tagged", "glm": "another"}
	w.MessageBatchWindow = config.Duration(500 * time.Millisecond)
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	mm.posts[0].Message = "passive #glm"
	add := func(id, body string, ts int64) {
		mm.posts = append(mm.posts, mattermost.Post{ID: id, RootID: key.Root, ChannelID: key.Channel, UserID: mm.posts[0].UserID, Message: body, CreateAt: ts})
	}
	advance := func() {
		t.Helper()
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	advance()
	if len(api.submitted) != 0 {
		t.Fatal("passive tag launched")
	}
	anchor := "ssssssssssssssssssssssssss"
	add(anchor, "@orpheus прочитай #SL-DEEPSEEK!", 2000)
	add("tttttttttttttttttttttttttt", "@orpheus вторая просьба #glm", 2100)
	api.lost = true
	if err := e.Thread(t.Context(), w, key, ch, true); err == nil {
		t.Fatal("lost response fixture did not fire")
	}
	p := api.submitted[0]
	if p.Envelope.Anchor != anchor || len(p.Envelope.TriggerIDs) != 2 || p.Envelope.ProfileOverride != "tagged" {
		t.Fatalf("initial selection %v/%s", p.Envelope.TriggerIDs, p.Envelope.ProfileOverride)
	}
	mm.posts[1].Message = "@orpheus исправлено #glm"
	mm.posts[1].EditAt = 2200
	w.ProfileByTag = nil
	e = &Engine{Config: e.Config, API: api, MM: mm, Bot: e.Bot}
	advance()
	if len(api.submitted) != 1 {
		t.Fatal("duplicate after restart")
	}
	add("uuuuuuuuuuuuuuuuuuuuuuuuuu", "@orpheus уточнение #glm", 3300)
	advance()
	if api.submitted[1].Run == "" || len(api.snapshot.Sessions) != 1 {
		t.Fatal("clarification rerouted")
	}
	s := &api.snapshot.Sessions[0]
	s.Runs[0].Status = "completed"
	add("vvvvvvvvvvvvvvvvvvvvvvvvvv", "@orpheus продолжай #glm", 4400)
	advance()
	if api.submitted[2].Session != s.ID || api.submitted[2].Run != "" || len(api.snapshot.Sessions) != 1 {
		t.Fatal("continuation rerouted")
	}
	s.Runs[1].Status = "completed"
	s.SandboxState = "deleted"
	add("wwwwwwwwwwwwwwwwwwwwwwwwww", "@orpheus продолжай #glm", 5500)
	advance()
	q := api.submitted[3]
	if q.Envelope.ProfileOverride != "tagged" || len(api.snapshot.Sessions) != 2 {
		t.Fatalf("rotation forgot choice: %v", q.Envelope.ProfileOverride)
	}
}
func TestConflictingProfileTagsRejectOnlyTheAnchor(t *testing.T) {
	for _, withNeighbor := range []bool{false, true} {
		e, api, mm, _, key := fixture(t)
		w := e.Config.Workflows[0]
		w.ProfileByTag = map[string]string{"sl-deepseek": "tagged", "glm": "another"}
		w.MessageBatchWindow = config.Duration(500 * time.Millisecond)
		ch := mattermost.Channel{ID: key.Channel, Type: "O"}
		mm.posts[0].Message = "@orpheus #glm #sl-deepseek"
		if withNeighbor {
			mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: mm.posts[0].UserID, Message: "@orpheus #sl-deepseek", CreateAt: 1100})
		}
		advance := func() {
			t.Helper()
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
		}
		advance()
		if len(api.submitted) != 0 || mm.calls != 1 {
			t.Fatal("initial rejection")
		}
		if withNeighbor {
			advance()
			if len(api.submitted) != 1 || api.submitted[0].Envelope.Anchor == key.Root || api.submitted[0].Envelope.ProfileOverride != "tagged" {
				t.Fatal("neighbor suppressed")
			}
		} else {
			advance()
			if len(api.submitted) != 0 || mm.calls != 1 {
				t.Fatal("duplicate rejection")
			}
			mm.posts[0].Message = "@orpheus #sl-deepseek"
			mm.posts[0].EditAt = 2500
			advance()
			if len(api.submitted) != 1 || api.submitted[0].Envelope.ProfileOverride != "tagged" {
				t.Fatal("edit could not start")
			}
		}
	}
}

func TestReplacementInheritsExplicitProfileOnTransfer(t *testing.T) {
	for _, tagged := range []bool{true, false} {
		e, api, mm, _, key := fixture(t)
		w := e.Config.Workflows[0]
		w.ProfileByTag = map[string]string{"sl-deepseek": "tagged", "glm": "another"}
		ch := mattermost.Channel{ID: key.Channel, Type: "O"}
		advance := func() {
			t.Helper()
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
		}
		if tagged {
			mm.posts[0].Message = "@orpheus #sl-deepseek"
		}
		advance()
		mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: mm.posts[0].UserID, Message: "@orpheus #glm", CreateAt: 3000})
		advance()
		s := &api.snapshot.Sessions[0]
		m := &s.Messages[len(s.Messages)-1]
		m.Delivery = "rejected"
		m.Error = "run_finished_before_delivery"
		s.Runs[0].Status = "completed"
		s.SandboxState = "deleted"
		w.Profile = "updated-base"
		w.EffectiveRevision = "updated"
		w.ProfileByTag = map[string]string{"sl-deepseek": "changed-target", "glm": "ignored"}
		e = &Engine{Config: e.Config, API: api, MM: mm, Bot: e.Bot}
		advance()
		p := api.submitted[2]
		want := ""
		if tagged {
			want = "tagged"
		}
		if len(api.snapshot.Sessions) != 2 || p.Envelope.Predecessor != m.ID || p.Envelope.ProfileOverride != want {
			t.Fatalf("transfer profile=%s expected=%s", p.Envelope.ProfileOverride, want)
		}
	}
}

func TestUnavailableProfileOnTransferredClarification(t *testing.T) {
	e, api, mm, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.ProfileByTag = map[string]string{"glm": "selected"}
	ch := mattermost.Channel{ID: key.Channel, Type: "O"}
	advance := func() {
		t.Helper()
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
	}
	mm.posts[0].Message = "@orpheus #glm"
	advance()
	mm.posts = append(mm.posts, mattermost.Post{ID: "ssssssssssssssssssssssssss", RootID: key.Root, ChannelID: key.Channel, UserID: mm.posts[0].UserID, Message: "@orpheus follow up", CreateAt: 2000})
	advance()
	session := &api.snapshot.Sessions[0]
	m := &session.Messages[len(session.Messages)-1]
	m.Delivery = "rejected"
	m.Error = "run_finished_before_delivery"
	session.Runs[0].Status = "completed"
	session.SandboxState = "deleted"
	bad := &rejectedAPI{fakeAPI: api, code: "unknown_profile"}
	e.API = bad
	advance()
	mm.posts[1].Message = "@orpheus edited after rejection"
	mm.posts[1].EditAt = 3000
	advance()
	if bad.calls != 1 || len(api.snapshot.Sessions) != 1 {
		t.Fatalf("repeated rejected transfer: %d attempts", bad.calls)
	}
	w.EffectiveRevision = "fixed"
	bad.code = ""
	advance()
	got := api.submitted[len(api.submitted)-1]
	if got.Envelope.ProfileOverride != "selected" || got.Envelope.Predecessor != m.ID {
		t.Fatal("transfer lost saved profile or identity")
	}
}
