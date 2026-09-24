package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestMentionMarkdown(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{{"@OrPhEuS help", true}, {"@orpheus help", true}, {"привет, @orpheus!", true}, {"`@orpheus`", false}, {"~~~go\n@orpheus\n~~~", false}, {"````\n```\n@orpheus\n````", false}, {"\\@orpheus", false}, {"me@orpheus.example", false}, {"@orpheus-bot", false}, {"**@orpheus**", true}, {"[open](https://example.com/@orpheus)", false}, {"[ask @orpheus](https://example.com)", true}, {"    @orpheus\n", false}} {
		t.Run(tc.body, func(t *testing.T) {
			if got := Mention(tc.body, []string{"orpheus"}); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestTriggerPolicy(t *testing.T) {
	b, key, _ := builder(t)
	for _, tc := range []struct {
		name, channel, message, kind            string
		file, bot, self, webhook, deleted, want bool
	}{
		{name: "public mention", channel: "O", message: "@orpheus help", want: true},
		{name: "private mention", channel: "P", message: "@orpheus help", want: true},
		{name: "group passive", channel: "G", message: "context only"},
		{name: "DM file only", channel: "D", file: true, want: true},
		{name: "public file only", channel: "O", file: true},
		{name: "empty DM", channel: "D"},
		{name: "system", channel: "D", message: "@orpheus", kind: "system_join_channel"},
		{name: "own bot", channel: "D", message: "@orpheus", self: true},
		{name: "foreign bot", channel: "D", message: "@orpheus", bot: true},
		{name: "webhook", channel: "O", message: "@orpheus", webhook: true},
		{name: "deleted", channel: "D", message: "@orpheus", deleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mattermost.Post{ID: key.Root, UserID: id(3), Message: tc.message, Type: tc.kind}
			if tc.file {
				p.FileIDs = []string{id(4)}
			}
			if tc.self {
				p.UserID = b.Bot.ID
			}
			if tc.webhook {
				p.Props = map[string]json.RawMessage{"from_webhook": json.RawMessage(`true`)}
			}
			if tc.deleted {
				p.DeleteAt = 1
			}
			if got := b.Trigger(p, mattermost.Channel{Type: tc.channel}, true, mattermost.User{IsBot: tc.bot}); got != tc.want {
				t.Fatal("unexpected trigger", got)
			}
		})
	}
}
func id(n int) string { return fmt.Sprintf("%026d", n) }

type contextSource struct {
	posts   map[string]mattermost.Post
	threads int
}

func (s *contextSource) Post(_ context.Context, id string) (mattermost.Post, error) {
	p, ok := s.posts[id]
	if !ok {
		return p, &mattermost.HTTPError{Status: 404}
	}
	return p, nil
}
func (s *contextSource) Thread(_ context.Context, id string) ([]mattermost.Post, error) {
	s.threads++
	p := s.posts[id]
	var out []mattermost.Post
	for _, v := range s.posts {
		if v.Root() == p.Root() {
			out = append(out, v)
		}
	}
	mattermost.Sort(out)
	return out, nil
}
func (s *contextSource) User(_ context.Context, id string) (mattermost.User, error) {
	return mattermost.User{ID: id, Username: "human"}, nil
}
func (s *contextSource) Files(_ context.Context, post string) ([]mattermost.FileInfo, error) {
	var out []mattermost.FileInfo
	for _, id := range s.posts[post].FileIDs {
		out = append(out, mattermost.FileInfo{ID: id, PostID: post, Name: "test.png", MIME: "image/png", Size: 3})
	}
	return out, nil
}
func builder(t *testing.T) (Builder, Key, *contextSource) {
	t.Helper()
	t.Setenv("ORPHEUS_BASE_URL", "http://orpheus:8080")
	t.Setenv("WORKFLOWS_DIR", "../../workflows")
	c, e := config.Load()
	if e != nil {
		t.Fatal(e)
	}
	c.Workflows[0].Since = time.Unix(0, 0)
	s := &contextSource{posts: map[string]mattermost.Post{}}
	key := Key{"chat", "assistant", id(1), id(2)}
	return Builder{Source: s, Config: c, Workflow: c.Workflows[0], Bot: mattermost.User{ID: id(9), Username: "orpheus"}}, key, s
}
func TestSameThreadReferenceDeduplicatesFiles(t *testing.T) {
	b, k, s := builder(t)
	posts := []mattermost.Post{{ID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 1000, Message: "original", FileIDs: []string{id(4)}}, {ID: id(5), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 2000, Message: "@orpheus see https://chat.example.com/team/pl/" + k.Root}}
	for _, p := range posts {
		s.posts[p.ID] = p
	}
	e, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, posts, Snapshot{}, true, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Request.Files) != 1 || s.threads != 0 || strings.Count(body, "original") != 1 {
		t.Fatal("duplicate context or attachment", s.threads, len(e.Request.Files))
	}
}
func TestLinkedFarTargetAndChannelPolicy(t *testing.T) {
	b, k, s := builder(t)
	linkedRoot := id(100)
	target := id(250)
	for i := 100; i < 300; i++ {
		root := linkedRoot
		if i == 100 {
			root = ""
		}
		s.posts[id(i)] = mattermost.Post{ID: id(i), RootID: root, ChannelID: k.Channel, UserID: id(3), CreateAt: int64(i), Message: fmt.Sprintf("body-%d", i)}
	}
	p := s.posts[target]
	p.FileIDs = []string{id(700)}
	s.posts[target] = p
	trigger := mattermost.Post{ID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 1000, Message: "@orpheus https://chat.example.com/team/pl/" + target}
	s.posts[k.Root] = trigger
	env, text, e := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{trigger}, Snapshot{}, true, time.Unix(10, 0))
	if e != nil || !strings.Contains(text, "body-250") || len(env.Request.Files) != 1 {
		t.Fatal("linked target missing", e)
	}
	p.ChannelID = id(800)
	s.posts[target] = p
	s.threads = 0
	env, text, e = b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{trigger}, Snapshot{}, true, time.Unix(10, 0))
	if e != nil || strings.Contains(text, "body-250") || len(env.Request.Files) != 0 || s.threads != 0 {
		t.Fatal("cross-channel context leaked", e)
	}
}
func TestContextDoesNotConsumeTrigger(t *testing.T) {
	b, k, s := builder(t)
	post := mattermost.Post{ID: id(5), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 2000, Message: "@orpheus new"}
	s.posts[post.ID] = post
	old := Envelope{Schema: 1, Source: k.Source, Workflow: k.Workflow, Channel: k.Channel, Root: k.Root, Anchor: k.Root, TriggerIDs: []string{k.Root}, ContextIDs: []string{post.ID}, Kind: "initial", Render: Render{Version: 1, MaxChars: 12000}}
	text, _ := old.Encode("previous")
	snapshot := Snapshot{Sessions: []Session{{Messages: []Message{{Role: "user", Delivery: "delivered", Text: text}}}}}
	env, _, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{post}, snapshot, false, time.Unix(10, 0))
	if err != nil || len(env.TriggerIDs) != 1 || env.TriggerIDs[0] != post.ID {
		t.Fatal("passive context consumed trigger", err)
	}
}
func TestOperationIncludesGeneration(t *testing.T) {
	k := Key{"s", "w", "c", "r"}
	if k.Operation("a", "session", "target", "old") == k.Operation("a", "session", "target", "new") {
		t.Fatal("generations collide")
	}
}

func TestKnownSameThreadReferenceAndEditedVersion(t *testing.T) {
	b, k, source := builder(t)
	original := mattermost.Post{ID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 1000, Message: "ORIGINAL_BODY", FileIDs: []string{id(4)}}
	old := Envelope{Schema: 1, Anchor: id(8), Kind: "initial", TriggerIDs: []string{id(8)}, ContextIDs: []string{k.Root}, Versions: []Version{PostVersion(original)}, Render: Render{Version: 1, MaxChars: 12000}}
	oldText, err := old.Encode("previous request")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Sessions: []Session{{Messages: []Message{{Role: "user", Delivery: "delivered", Text: oldText}}}}}
	trigger := mattermost.Post{ID: id(5), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 2000, Message: "@orpheus see https://chat.example.com/team/pl/" + k.Root}
	source.posts[original.ID] = original
	source.posts[trigger.ID] = trigger
	env, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{original, trigger}, snapshot, false, time.Unix(10, 0))
	if err != nil || len(env.Request.Files) != 1 || source.threads != 0 || strings.Contains(body, "ORIGINAL_BODY") || !strings.Contains(body, "thread_reference") {
		t.Fatal("known same-thread context duplicated or files missing", err, body)
	}
	original.Message = "EDITED_BODY"
	original.UpdateAt = 3000
	source.posts[original.ID] = original
	_, body, err = b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{original, trigger}, snapshot, false, time.Unix(10, 0))
	if err != nil || !strings.Contains(body, "EDITED_BODY") || source.threads != 0 {
		t.Fatal("edited reference not refreshed", err)
	}
}
func TestOversizedLinkedPostDoesNotRejectTrigger(t *testing.T) {
	b, k, source := builder(t)
	b.Config.MaxRequestBytes = 20000
	trigger := mattermost.Post{ID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 1000, Message: "@orpheus see https://chat.example.com/team/pl/" + id(6)}
	source.posts[id(6)] = mattermost.Post{ID: id(6), ChannelID: k.Channel, UserID: id(3), Message: strings.Repeat("huge", 10000)}
	env, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{trigger}, Snapshot{}, true, time.Unix(10, 0))
	if err != nil || len(env.TriggerIDs) != 1 || !strings.Contains(body, "context_limit_exceeded") || source.threads != 0 {
		t.Fatal("linked context blocked required input", err)
	}
}

func TestPostVersionIgnoresReplyAndReactionMetadata(t *testing.T) {
	p := mattermost.Post{ID: id(1), Message: "request", FileIDs: []string{id(2)}}
	version := PostVersion(p)
	p.UpdateAt = 9000
	p.Metadata = []byte(`{"reactions":[{"emoji":"+1"}]}`)
	p.Props = map[string]json.RawMessage{"unrelated": json.RawMessage(`true`)}
	if PostVersion(p) != version {
		t.Fatal("transport metadata changed input version")
	}
	p.Props["attachments"] = json.RawMessage(`[{"text":"card"}]`)
	if PostVersion(p) == version {
		t.Fatal("card edit ignored")
	}
}
func TestContinuationSkipsOwnPostsExceptExplicitReferences(t *testing.T) {
	b, k, source := builder(t)
	answer := mattermost.Post{ID: id(10), RootID: k.Root, ChannelID: k.Channel, UserID: b.Bot.ID, CreateAt: 1000, Message: "PREVIOUS_ANSWER", FileIDs: []string{id(11)}}
	trigger := mattermost.Post{ID: id(12), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 4000, Message: "@orpheus next"}
	source.posts[answer.ID] = answer
	source.posts[trigger.ID] = trigger
	for _, initial := range []bool{false, true} {
		env, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{answer, trigger}, Snapshot{}, initial, time.Unix(10, 0))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "PREVIOUS_ANSWER") != initial || (len(env.Request.Files) > 0) != initial {
			t.Fatal("own answer context mismatch", initial)
		}
	}
	trigger.Message += " https://chat.example.com/team/pl/" + answer.ID
	env, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, []mattermost.Post{answer, trigger}, Snapshot{}, false, time.Unix(10, 0))
	if err != nil || !strings.Contains(body, "PREVIOUS_ANSWER") || len(env.Request.Files) != 1 {
		t.Fatal("explicit output reference lost", err)
	}
}
func TestPassiveFilesShrinkToTransportBudget(t *testing.T) {
	b, k, source := builder(t)
	var posts []mattermost.Post
	for i := range 250 {
		p := mattermost.Post{ID: id(100 + i), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: int64(1000 + i), Message: "screenshot", FileIDs: []string{id(1000 + i)}}
		posts = append(posts, p)
		source.posts[p.ID] = p
	}
	trigger := mattermost.Post{ID: id(400), RootID: k.Root, ChannelID: k.Channel, UserID: id(3), CreateAt: 4000, Message: "@orpheus summarize"}
	posts = append(posts, trigger)
	env, body, err := b.Build(t.Context(), k, mattermost.Channel{Type: "O"}, posts, Snapshot{}, true, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env.Request)
	if len(raw) >= 60<<10 || !strings.Contains(body, "Context omitted") || len(env.TriggerIDs) != 1 || env.TriggerIDs[0] != trigger.ID || len(env.Request.Files) >= 250 {
		t.Fatal("passive file history blocked required input")
	}
}
