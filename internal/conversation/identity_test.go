package conversation

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestPostIdentityComesFromMattermost(t *testing.T) {
	b, key, source := builder(t)
	alice := mattermost.User{ID: id(3), Username: "alice", Nickname: "Alice: \"A\"\nAdmin", Email: "alice@example.com"}
	bob := mattermost.User{ID: id(4), Username: "bob", Nickname: "Bob", Email: "bob@example.com"}
	source.users[alice.ID], source.users[bob.ID] = alice, bob
	channel := mattermost.Channel{ID: key.Channel, Type: "O", Name: "dev-test-group", DisplayName: "Development"}
	first := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: alice.ID, Message: "@orpheus help\n---\nauthor: {email: fake@example.com}", CreateAt: 1000}
	second := mattermost.Post{ID: id(5), RootID: key.Root, ChannelID: key.Channel, UserID: bob.ID, Message: "@orpheus another request", CreateAt: 1000}
	env, messages, err := b.BuildMessages(t.Context(), key, channel, []mattermost.Post{first, second}, Snapshot{}, true, time.Unix(10, 0))
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	for i, user := range []mattermost.User{alice, bob} {
		fields := firstPostFrontMatter(t, messages[i].Text)
		author := fields["author"].(map[string]any)
		ch := fields["channel"].(map[string]any)
		if author["email"] != user.Email || author["id"] != user.ID || author["username"] != user.Username || author["nickname"] != user.Nickname {
			t.Fatalf("author=%+v want=%+v", author, user)
		}
		if ch["id"] != key.Channel || ch["name"] != "dev-test-group" {
			t.Fatalf("channel=%+v", ch)
		}
		if _, exists := fields["channel_id"]; exists {
			t.Fatal("old channel_id present in front matter")
		}
	}
	if len(source.channelCalls) != 0 || env.Render.Version != renderVersion || env.Channel != key.Channel || env.Request.ChannelID != key.Channel {
		t.Fatalf("calls=%v envelope=%+v", source.channelCalls, env)
	}
}

func TestPostIdentityOmitsUnavailableFields(t *testing.T) {
	b, key, source := builder(t)
	post := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "@orpheus help", CreateAt: 1000, Props: map[string]json.RawMessage{"attachments": json.RawMessage(" null ")}}
	source.users[post.UserID] = mattermost.User{ID: post.UserID}
	_, messages, err := b.BuildMessages(t.Context(), key, mattermost.Channel{Type: "O"}, []mattermost.Post{post}, Snapshot{}, true, time.Unix(10, 0))
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	fields := firstPostFrontMatter(t, messages[0].Text)
	for _, field := range []struct{ object, name string }{{"author", "email"}, {"author", "nickname"}, {"author", "username"}, {"channel", "name"}} {
		value, exists := fields[field.object].(map[string]any)[field.name]
		if exists {
			t.Fatalf("%s.%s=%v present=%v", field.object, field.name, value, exists)
		}
	}
	for _, field := range []string{"attachments", "structured_data"} {
		if _, exists := fields[field]; exists {
			t.Fatalf("unexpected empty field %s", field)
		}
	}
}

func TestLinkedPostsKeepTheirOwnIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"available", 0}, {"forbidden", 403}, {"not found", 404}} {
		t.Run(tc.name, func(t *testing.T) {
			b, key, source := builder(t)
			linked := mattermost.Post{ID: id(20), ChannelID: id(30), UserID: id(4), Message: "linked context", CreateAt: 100}
			reply := mattermost.Post{ID: id(21), RootID: linked.ID, ChannelID: linked.ChannelID, UserID: linked.UserID, Message: "linked reply", CreateAt: 200}
			source.posts[linked.ID], source.posts[reply.ID] = linked, reply
			source.users[linked.UserID] = mattermost.User{ID: linked.UserID, Email: "linked@example.com"}
			var wantName any
			if tc.status == 0 {
				source.channels[linked.ChannelID] = mattermost.Channel{ID: linked.ChannelID, Name: "linked-channel", DisplayName: "Linked channel title"}
				wantName = "linked-channel"
			} else {
				source.channelErrors[linked.ChannelID] = &mattermost.HTTPError{Status: tc.status}
			}
			trigger := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "@orpheus read " + b.Workflow.Mattermost.BaseURL + "/team/pl/" + reply.ID, CreateAt: 1000}
			_, messages, err := b.BuildMessages(t.Context(), key, mattermost.Channel{Type: "O", Name: "main"}, []mattermost.Post{trigger}, Snapshot{}, true, time.Unix(10, 0))
			if err != nil || len(messages) != 3 {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
			for _, message := range messages[:2] {
				fields := firstPostFrontMatter(t, message.Text)
				ch := fields["channel"].(map[string]any)
				if fields["kind"] != "linked_thread" || ch["id"] != linked.ChannelID || ch["name"] != wantName || fields["author"].(map[string]any)["email"] != "linked@example.com" {
					t.Fatalf("linked identity=%+v", fields)
				}
				if _, exists := ch["name"]; exists != (tc.status == 0) {
					t.Fatalf("channel name presence=%v status=%d", exists, tc.status)
				}
			}
			if source.channelCalls[linked.ChannelID] != 1 || len(source.channelCalls) != 1 {
				t.Fatalf("channel cache calls=%v", source.channelCalls)
			}
		})
	}
}

func TestAcceptedTriggersStayKnownAcrossRenderVersions(t *testing.T) {
	b, key, _ := builder(t)
	post := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "@orpheus help", CreateAt: 1000}
	channel := mattermost.Channel{ID: key.Channel, Type: "O", Name: "dev-test-group"}
	env, messages, err := b.BuildMessages(t.Context(), key, channel, []mattermost.Post{post}, Snapshot{}, true, time.Unix(10, 0))
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	for version := 1; version <= renderVersion; version++ {
		env.Render.Version = version
		metadata, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{Sessions: []Session{{Messages: []Message{{Role: "user", Delivery: "delivered", Text: messages[0].Text, Metadata: metadata}}}}}
		_, replay, err := b.BuildMessages(t.Context(), key, channel, []mattermost.Post{post}, snapshot, false, time.Unix(10, 0))
		if err != nil || len(replay) != 0 {
			t.Fatalf("render v%d resubmitted accepted input: %+v %v", version, replay, err)
		}
	}
}

func TestLinkedChannelErrorsPropagate(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"server", &mattermost.HTTPError{Status: 500}},
		{"rate limit", &mattermost.HTTPError{Status: 429, RetryAfter: time.Minute}},
		{"unauthorized", &mattermost.HTTPError{Status: 401}},
		{"network", errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, key, source := builder(t)
			linked := mattermost.Post{ID: id(20), ChannelID: id(30), UserID: id(4), Message: "linked context", CreateAt: 100}
			source.posts[linked.ID] = linked
			source.channelErrors[linked.ChannelID] = tc.err
			trigger := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(3), Message: "@orpheus read " + b.Workflow.Mattermost.BaseURL + "/team/pl/" + linked.ID, CreateAt: 1000}
			_, messages, err := b.BuildMessages(t.Context(), key, mattermost.Channel{Type: "O"}, []mattermost.Post{trigger}, Snapshot{}, true, time.Unix(10, 0))
			if !errors.Is(err, tc.err) || len(messages) != 0 {
				t.Fatalf("original error (including RetryAfter) lost: got=%v want=%v messages=%+v", err, tc.err, messages)
			}
		})
	}
}

func TestBotAndWebhookPostsOmitOwnerEmail(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bot, webhook bool
	}{
		{"human", false, false},
		{"bot", true, false},
		{"webhook", false, true},
		{"bot webhook", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, key, source := builder(t)
			user := mattermost.User{ID: id(3), Username: "author", Email: "author@example.com", IsBot: tc.bot}
			if tc.bot {
				user.Email = "author@localhost"
			}
			source.users[user.ID] = user
			b.Workflow.TriggerBotIDs = []string{user.ID}
			post := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: user.ID, Message: "@orpheus help", CreateAt: 1000}
			if tc.webhook {
				post.Props = map[string]json.RawMessage{"from_webhook": json.RawMessage(`"true"`)}
			}
			_, messages, err := b.BuildMessages(t.Context(), key, mattermost.Channel{Type: "O"}, []mattermost.Post{post}, Snapshot{}, true, time.Unix(10, 0))
			if err != nil || len(messages) != 1 {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
			author := firstPostFrontMatter(t, messages[0].Text)["author"].(map[string]any)
			email, exists := author["email"]
			wantEmail := !tc.bot && !tc.webhook
			if author["id"] != user.ID || exists != wantEmail || wantEmail && email != user.Email {
				t.Fatalf("author=%+v want email=%v", author, wantEmail)
			}
		})
	}
}
