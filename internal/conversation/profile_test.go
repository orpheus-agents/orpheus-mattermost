package conversation

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestTaggedProfileMarkdown(t *testing.T) {
	tags := map[string]string{"glm": "A", "alias": "A", "sl-deepseek": "B", "k": "K"}
	for _, tc := range []struct{ body, want string }{
		{"#GLM?!", "A"}, {"#glm,other #glm)other #glm.example", ""}, {"please\u00a0#sl-deepseek…", "B"}, {"#glm #ALIAS #glm", "A"}, {"**#glm**", "A"},
		{"#glm-extra #glm_2 #glm2 #glmя", ""}, {"word#glm \\#glm #unknown #K", ""},
		{"\u0060#glm\u0060\n\n    #glm\n\n~~~\n#glm\n~~~", ""},
		{"> #glm\n> **#alias**\n\n#sl-deepseek", "B"},
		{"[#glm](https://example.org/#glm) ![#glm](img) <https://example.org/#glm>", ""},
		{"https://example.org/**#glm** www.example.org/#glm", ""},
		{"[#glm][ref]\n\n[ref]: https://example.org", ""},
	} {
		got, err := taggedProfile(tc.body, tags)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.body, got, err, tc.want)
		}
	}
	if _, err := taggedProfile("#GLM #sl-deepseek", tags); !errors.Is(err, ErrConflictingProfileTags) {
		t.Fatal(err)
	}
}

func TestOnlyEligibleOpeningPostSelectsProfile(t *testing.T) {
	for _, tc := range []struct {
		name, kind, message            string
		mention, self, reply, accepted bool
	}{
		{"mention", "O", "@orpheus task #glm", true, false, false, true},
		{"passive", "O", "task #glm", true, false, false, false},
		{"DM", "D", "task #glm", true, false, false, true},
		{"start without mention", "O", "task #glm", false, false, false, true},
		{"own root", "O", "@orpheus task #glm", true, true, false, true},
		{"own reply", "D", "@orpheus task #glm", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, key, _ := builder(t)
			b.Workflow.ProfileByTag = map[string]string{"glm": "G"}
			b.Workflow.StartOnMention = &tc.mention
			p := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: id(1), CreateAt: 1000, Message: tc.message}
			if tc.self {
				p.UserID = b.Bot.ID
			}
			if tc.reply {
				p.RootID = key.Root
				p.ID = id(7)
			}
			env, body, err := buildJoined(b, t.Context(), key, mattermost.Channel{ID: key.Channel, Type: tc.kind}, []mattermost.Post{p}, Snapshot{}, true, time.Now())
			if err != nil || (env.Anchor != "") != tc.accepted || tc.accepted && (env.ProfileOverride != "G" || !strings.Contains(body, p.Message)) {
				t.Fatal(env.Anchor, env.ProfileOverride, err)
			}
		})
	}
}
