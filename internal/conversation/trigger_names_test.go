package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func namedWorkflow(t *testing.T, fields string) config.Workflow {
	t.Helper()
	dir := t.TempDir()
	file := "---\nid: assistant\nrevision: '1'\nreconcile_from: '2026-01-01T00:00:00Z'\nprofile: default\nsandbox_template: assistant\nmattermost: {base_url: 'https://chat.example.com', token_env: MM_TOKEN}\n" + fields + "\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "assistant.md"), []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOWS_DIR", dir)
	t.Setenv("ORPHEUS_BASE_URL", "http://orpheus:8080")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Workflows[0]
}

func TestAddressesBot(t *testing.T) {
	w := namedWorkflow(t, "trigger_names: [Orpheus, Орфей, Орфеюшка, Орфи, 'Ops Bot', '@helper', 'C++']\ntrigger_name_regex: 'орфей(?:-[0-9]+)?'")
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"Orpheus check", true},
		{"orpheus, check", true},
		{"Орфей", true},
		{"орфей!", true},
		{"Орфеюшка: проверь", true},
		{"орфеюшка…", true},
		{"Орфи.", true},
		{"Orpheus. Check the logs", true},
		{"Orpheus... check the logs", true},
		{"Orpheus. **Check the logs**", true},
		{"orpheus.simla.tech просто убираю с окты?", false},
		{"Orpheus.Example.COM/path", false},
		{"орфей-42.example.org", false},
		{"орфи?", true},
		{"ОРФЕЙ-42: проверь", true},
		{"  OPS BOT\u00a0check", true},
		{"@helper check", true},
		{"ask @current about this", true},
		{"C++ check", true},
		{"Орфей, **проверь**", true},
		{"Орфей [проверь](https://example.com)", true},
		{"Орфей\r\nпроверь", true},
		{"\nОрфей\nпроверь", true},
		{"Вчера Орфей проверил логи", false},
		{"Вчера\nОрфей проверил логи", false},
		{"Орфейский отчёт", false},
		{"Орфей-42а", false},
		{"Орфей-лог", false},
		{"Орфей_тест", false},
		{"Орфей[ский](https://example.com)", false},
		{"Орфей**ский**", false},
		{"**Орфей**, проверь", false},
		{"`Орфей`, проверь", false},
		{"    Орфей, проверь", false},
		{"~~~\nОрфей\n~~~", false},
		{"> Орфей, проверь", false},
		{"> цитата\n\nОрфей, проверь", false},
		{"[Орфей](https://example.com)", false},
		{"[link]: https://example.com\n\nОрфей, проверь", false},
		{"![Орфей](https://example.com)", false},
		{"\\@helper check", false},
		{"@orpheus check", false},
		{"C+ check", false},
		{"", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := AddressesBot(tc.body, w, "current"); got != tc.want {
				t.Fatalf("AddressesBot(%q) = %t, want %t", tc.body, got, tc.want)
			}
		})
	}
}

func TestAddressRegexUsesOnlyOpeningProse(t *testing.T) {
	w := namedWorkflow(t, "trigger_name_regex: '.+'")
	for _, body := range []string{"https://example.com/request", "www.example.com", "`task`", "[task](https://example.com)", "# heading", "   "} {
		if AddressesBot(body, w, "current") {
			t.Errorf("matched non-prose %q", body)
		}
	}
	if !AddressesBot("task, `code`", w, "current") {
		t.Error("plain text before Markdown did not match")
	}
	w = namedWorkflow(t, `trigger_name_regex: '\b'`)
	if AddressesBot("no name", w, "current") {
		t.Error("accepted a zero-width match")
	}
}

func TestNamedTriggerAuthorPolicy(t *testing.T) {
	w := namedWorkflow(t, "trigger_names: [Орфей]\ntrigger_bot_ids: ["+id(3)+"]")
	b := Builder{Workflow: w, Bot: mattermost.User{ID: id(9), Username: "orpheus"}}
	for _, tc := range []struct {
		name, channel, body string
		author              int
		bot, webhook        bool
		existing, want      bool
	}{
		{name: "public root", channel: "O", author: 1, body: "Орфей, check", want: true},
		{name: "continue thread", channel: "P", author: 1, body: "Орфей, check", existing: true, want: true},
		{name: "allowed bot", channel: "G", author: 3, bot: true, body: "Орфей, check", want: true},
		{name: "foreign webhook", channel: "O", author: 4, webhook: true, body: "Орфей, check"},
		{name: "own bot", channel: "O", author: 9, bot: true, body: "Орфей, check"},
		{name: "own bot DM", channel: "D", author: 9, bot: true, body: "Орфей, check"},
		{name: "own bot mention", channel: "O", author: 9, bot: true, body: "@orpheus check", want: true},
		{name: "DM without name", channel: "D", author: 1, body: "check", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post := mattermost.Post{UserID: id(tc.author), Message: tc.body}
			if tc.webhook {
				post.Props = map[string]json.RawMessage{"from_webhook": json.RawMessage(`true`)}
			}
			if got := b.Trigger(post, mattermost.Channel{Type: tc.channel}, tc.existing, mattermost.User{IsBot: tc.bot}); got != tc.want {
				t.Fatalf("Trigger = %t, want %t", got, tc.want)
			}
		})
	}
	b.Workflow.StartOnMention = new(false)
	post := mattermost.Post{UserID: id(1), Message: "check"}
	channel := mattermost.Channel{Type: "O"}
	if !b.Trigger(post, channel, false, mattermost.User{}) || b.Trigger(post, channel, true, mattermost.User{}) {
		t.Fatal("start_on_mention disabled the mention requirement outside the first request")
	}
}
