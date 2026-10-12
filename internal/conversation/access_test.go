package conversation

import (
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestCleanContextBoundary(t *testing.T) {
	b, key, source := builder(t)
	b.Workflow.Access = &config.Access{Namespace: "mattermost:test"}
	b.Workflow.Links.Enabled = new(false)
	b.TriggerOnly = true
	old := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "OLD_SECRET", CreateAt: 1000, FileIDs: []string{id(9)}}
	anchor := mattermost.Post{ID: id(7), RootID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "@orpheus current", CreateAt: 2000}
	posts := []mattermost.Post{old, anchor}
	env, messages, err := b.BuildMessages(t.Context(), key, mattermost.Channel{ID: key.Channel, Type: "P"}, posts, Snapshot{}, true, time.Now())
	if err != nil || len(messages) != 1 || len(env.Request.Files) != 0 || env.ContextStart == nil || env.ContextStart.PostID != anchor.ID || messages[0].Subject.ID != anchor.UserID {
		t.Fatal(env, messages, err)
	}
	// Even when the boundary post is deleted, older messages and files stay out.
	b.TriggerOnly, b.ContextStart = false, env.ContextStart
	next := anchor
	next.ID, next.CreateAt, next.Message = id(8), 3000, "@orpheus next"
	source.posts = map[string]mattermost.Post{old.ID: old, next.ID: next}
	env, messages, err = b.BuildMessages(t.Context(), key, mattermost.Channel{ID: key.Channel, Type: "P"}, []mattermost.Post{old, next}, Snapshot{}, false, time.Now())
	if err != nil || len(env.Request.Files) != 0 || len(messages) != 1 || strings.Contains(messages[0].Text, "OLD_SECRET") {
		t.Fatal(env, messages, err)
	}
	// Equal timestamps use the persisted ID as a deterministic tie-break.
	before := old
	before.CreateAt = 2000
	before.ID = id(6)
	_, messages, err = b.BuildMessages(t.Context(), key, mattermost.Channel{ID: key.Channel, Type: "P"}, []mattermost.Post{before, next}, Snapshot{}, false, time.Now())
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
}

func TestInvocationIdentity(t *testing.T) {
	k := Key{Source: "source", Workflow: "workflow", Channel: "channel", Root: "root"}
	first := k.Invocation("anchor")
	if first != k.Invocation("anchor") || first == k.Invocation("next") || first.String() == k.Operation("anchor", "session", "target", "") {
		t.Fatal("unstable invocation")
	}
	for _, changed := range []Key{{"other", k.Workflow, k.Channel, k.Root}, {k.Source, "other", k.Channel, k.Root}, {k.Source, k.Workflow, "other", k.Root}, {k.Source, k.Workflow, k.Channel, "other"}} {
		if first == changed.Invocation("anchor") {
			t.Fatal("invocation ignores routing identity")
		}
	}
}
