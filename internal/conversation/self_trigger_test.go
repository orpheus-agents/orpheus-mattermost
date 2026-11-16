package conversation

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestSelfTriggerPolicy(t *testing.T) {
	b, key, _ := builder(t)
	for _, tc := range []struct {
		name, message, kind           string
		reply, allowed, deleted, want bool
	}{
		{name: "root mention", message: "@OrPhEuS process task", want: true},
		{name: "root without mention", message: "process task"},
		{name: "root code mention", message: "`@orpheus` process task"},
		{name: "reply mention", message: "@orpheus process task", reply: true},
		{name: "allowlisted reply", message: "@orpheus process task", reply: true, allowed: true},
		{name: "allowlisted root without mention", message: "process task", allowed: true},
		{name: "deleted root", message: "@orpheus process task", deleted: true},
		{name: "system root", message: "@orpheus process task", kind: "system_join_channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mattermost.Post{ID: key.Root, UserID: b.Bot.ID, Message: tc.message, Type: tc.kind}
			if tc.reply {
				p.RootID = id(10)
			}
			if tc.deleted {
				p.DeleteAt = 1
			}
			b.Workflow.TriggerBotIDs = nil
			if tc.allowed {
				b.Workflow.TriggerBotIDs = []string{b.Bot.ID}
			}
			for _, channel := range []string{"O", "P", "G", "D"} {
				for _, startOnMention := range []bool{true, false} {
					b.Workflow.StartOnMention = new(startOnMention)
					for _, existing := range []bool{false, true} {
						if got := b.Trigger(p, mattermost.Channel{Type: channel}, existing, mattermost.User{ID: b.Bot.ID, IsBot: true}); got != tc.want {
							t.Fatalf("channel=%s start_on_mention=%v existing=%v: got %v, want %v", channel, startOnMention, existing, got, tc.want)
						}
					}
				}
			}
		})
	}
}

func TestSelfRootInputAndDeduplication(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "new session", false: "existing session"}[initial], func(t *testing.T) {
			b, key, source := builder(t)
			source.users[b.Bot.ID] = mattermost.User{ID: b.Bot.ID, Username: b.Bot.Username, IsBot: true}
			root := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: b.Bot.ID, CreateAt: 1000, Message: "@orpheus SCHEDULED_TASK", FileIDs: []string{id(20)}}
			source.posts[root.ID] = root
			channel := mattermost.Channel{ID: key.Channel, Type: "O"}
			snapshot := Snapshot{}
			if !initial {
				snapshot.Sessions = []Session{{ID: "existing"}}
			}
			env, messages, err := b.BuildMessages(t.Context(), key, channel, []mattermost.Post{root}, snapshot, initial, time.Unix(10, 0))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(env.TriggerIDs, []string{root.ID}) || len(messages) != 1 || messages[0].PostID != root.ID || !strings.Contains(messages[0].Text, root.Message) {
				t.Fatalf("self-authored task lost: envelope=%+v messages=%+v", env, messages)
			}
			if len(env.Request.Files) != 1 || env.Request.Files[0].FileID != root.FileIDs[0] {
				t.Fatal("self-authored task attachment lost", env.Request.Files)
			}
			snapshot.Sessions = []Session{{Messages: []Message{{Role: "user", Delivery: "delivered", Metadata: []byte(mustJSON(env))}}}}
			reply := mattermost.Post{ID: id(21), RootID: root.ID, ChannelID: key.Channel, UserID: b.Bot.ID, CreateAt: 2000, Message: "@orpheus TASK_ANSWER"}
			// An edited accepted root and an own reply must not resubmit the task.
			root.Message = "@orpheus EDITED_TASK"
			root.UpdateAt = 3000
			env, messages, err = b.BuildMessages(t.Context(), key, channel, []mattermost.Post{root, reply}, snapshot, false, time.Unix(10, 0))
			if err != nil || len(env.TriggerIDs) != 0 || len(messages) != 0 {
				t.Fatalf("self-authored task repeated: envelope=%+v messages=%+v err=%v", env, messages, err)
			}
		})
	}
}
