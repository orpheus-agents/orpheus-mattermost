package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func example(t *testing.T, changes map[string]string) (Config, error) {
	t.Helper()
	env := map[string]string{
		"ORPHEUS_BASE_URL": "http://orpheus:8080",
		"WORKFLOWS_DIR":    "../../workflows",
	}
	maps.Copy(env, changes)
	return load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
}
func workflowText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../workflows/assistant.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func writeWorkflow(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestExampleAndRouting(t *testing.T) {
	c, e := example(t, nil)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(c.Workflows[0].Instructions, "view_image") || c.HTTPTimeout.Value().Seconds() != 30 {
		t.Fatal("workflow body or defaults missing")
	}
	w, e := c.Route("bbbbbbbbbbbbbbbbbbbbbbbbbb", "O")
	if e != nil || w == nil || w.ID != "assistant" {
		t.Fatal(w, e)
	}
	w, e = c.Route("bbbbbbbbbbbbbbbbbbbbbbbbbb", "P")
	if e != nil || w != nil {
		t.Fatal("private channel enabled")
	}
	special := c.Workflows[0]
	special.ID = "special"
	special.IncludeIDs = []string{"bbbbbbbbbbbbbbbbbbbbbbbbbb"}
	c.Workflows = append(c.Workflows, special)
	w, e = c.Route(special.IncludeIDs[0], "O")
	if e != nil || w.ID != "special" {
		t.Fatal(w, e)
	}
}
func TestInvalidEnvironment(t *testing.T) {
	for _, change := range []map[string]string{
		{"ORPHEUS_BASE_URL": "https://user:private-token@example.com"},
		{"HTTP_TIMEOUT": "private-token"}, {"HTTP_TIMEOUT": "0s"},
		{"MAX_REQUEST_BYTES": "private-token"}, {"MAX_REQUEST_BYTES": "0"},
		{"MAX_PARALLEL_THREADS": "129"}, {"WORKFLOWS_DIR": ""},
		{"WORKFLOWS_DIR": "/missing"}, {"LISTEN_ADDR": ""},
	} {
		c, err := example(t, change)
		if err == nil || len(c.Workflows) != 0 {
			t.Fatalf("invalid ENV accepted: %v", change)
		}
		if strings.Contains(err.Error(), "private-token") {
			t.Fatal("secret leaked in error")
		}
	}
	c, err := example(t, map[string]string{"MAX_PARALLEL_THREADS": "3", "HTTP_TIMEOUT": "45s"})
	if err != nil || c.MaxParallelThreads != 3 || c.HTTPTimeout.Value().Seconds() != 45 {
		t.Fatal(c, err)
	}
}
func TestInvalidFrontMatter(t *testing.T) {
	base := workflowText(t)
	for name, body := range map[string]string{
		"missing":            "id: assistant\n",
		"unclosed":           "---\nid: assistant\n",
		"empty":              "---\n---\nbody",
		"unknown":            strings.Replace(base, "---\n", "---\nunknown: private-token\n", 1),
		"nested unknown":     strings.Replace(base, "mattermost:\n", "mattermost:\n  unknown: private-token\n", 1),
		"duplicate":          strings.Replace(base, "---\n", "---\nid: private-token\n", 1),
		"type":               strings.Replace(base, "---\n", "---\nfiles:\n  max_per_post: private-token\n", 1),
		"limit":              strings.Replace(base, "---\n", "---\nfiles:\n  max_per_post: 6\n", 1),
		"duration":           strings.Replace(base, "---\n", "---\nmessage_batch_window: private-token\n", 1),
		"cutover":            strings.Replace(base, "2026-09-23T00:00:00Z", "yesterday", 1),
		"application config": strings.Replace(base, "---\n", "---\nlisten: ':8081'\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkflow(t, dir, "bad.md", body)
			c, err := example(t, map[string]string{"WORKFLOWS_DIR": dir})
			if err == nil || len(c.Workflows) != 0 {
				t.Fatal("invalid workflow accepted")
			}
			if strings.Contains(err.Error(), "private-token") {
				t.Fatal("secret leaked in error")
			}
		})
	}
}
func TestDirectoryAndAtomicReload(t *testing.T) {
	base := workflowText(t)
	dir := t.TempDir()
	if _, err := example(t, map[string]string{"WORKFLOWS_DIR": dir}); err == nil {
		t.Fatal("empty directory accepted")
	}
	writeWorkflow(t, dir, "z.md", base)
	for _, name := range []string{".hidden.md", "#scratch.md", "editor.md~", "editor.md.tmp", "config.txt"} {
		writeWorkflow(t, dir, name, "bad")
	}
	if err := os.Mkdir(filepath.Join(dir, "nested.md"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := example(t, map[string]string{"WORKFLOWS_DIR": dir})
	if err != nil || len(c.Workflows) != 1 {
		t.Fatal(c, err)
	}
	second := strings.Replace(base, "id: assistant", "id: specialized", 1)
	second = strings.Replace(second, "---\n", "---\ninclude_ids: [bbbbbbbbbbbbbbbbbbbbbbbbbb]\n", 1)
	writeWorkflow(t, dir, "a.md", strings.ReplaceAll(second, "\n", "\r\n"))
	c, err = example(t, map[string]string{"WORKFLOWS_DIR": dir})
	if err != nil || len(c.Workflows) != 2 || c.Workflows[0].ID != "specialized" {
		t.Fatal(c, err)
	}
	writeWorkflow(t, dir, "a.md", base)
	if c, err = example(t, map[string]string{"WORKFLOWS_DIR": dir}); err == nil || len(c.Workflows) != 0 {
		t.Fatal("partial config or duplicate accepted")
	}
	writeWorkflow(t, dir, "a.md", "bad")
	if c, err = example(t, map[string]string{"WORKFLOWS_DIR": dir}); err == nil || len(c.Workflows) != 0 {
		t.Fatal("partial config accepted")
	}
}
func TestRevisionTracksInstructionsAndPolicy(t *testing.T) {
	base := workflowText(t)
	dir := t.TempDir()
	revision := func(body string) string {
		t.Helper()
		writeWorkflow(t, dir, "assistant.md", body)
		c, err := example(t, map[string]string{"WORKFLOWS_DIR": dir})
		if err != nil {
			t.Fatal(err)
		}
		return c.Workflows[0].EffectiveRevision
	}
	initial := revision(base)
	if revision(base+"\nAdditional instructions.") == initial {
		t.Fatal("body change ignored")
	}
	if revision(strings.Replace(base, "---\n", "---\nrun_timeout_seconds: 4000\n", 1)) == initial {
		t.Fatal("policy change ignored")
	}
	for _, setting := range []string{"draining: true", "poll_interval: 40s", "private_channels: true", "message_batch_window: 3s", "include_ids: [aaaaaaaaaaaaaaaaaaaaaaaaaa]", "trigger_bot_ids: [bbbbbbbbbbbbbbbbbbbbbbbbbb]"} {
		if revision(strings.Replace(base, "---\n", "---\n"+setting+"\n", 1)) != initial {
			t.Fatal("operational change rotated session")
		}
	}
	c, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := c.Workflows[0].EffectiveRevision
	if err := c.validate(); err != nil || c.Workflows[0].EffectiveRevision != original {
		t.Fatal("revision is not stable", err)
	}
}

func TestMattermostSettingsBelongToWorkflow(t *testing.T) {
	base := workflowText(t)
	for _, bad := range []string{
		strings.Replace(base, "base_url: https://chat.example.com", "base_url: https://user:private-token@chat.example.com", 1),
		strings.Replace(base, "token_env: MATTERMOST_BOT_TOKEN", "token_env: bad-name", 1),
		strings.Replace(base, "mattermost:\n", "mattermost:\n  expected_bot_id: aaaaaaaaaaaaaaaaaaaaaaaaaa\n", 1),
	} {
		dir := t.TempDir()
		writeWorkflow(t, dir, "bad.md", bad)
		if _, err := example(t, map[string]string{"WORKFLOWS_DIR": dir}); err == nil {
			t.Fatal("invalid workflow connection accepted")
		}
	}
	cfg, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := cfg.Workflows[0]
	other := first
	other.ID = "other"
	other.Mattermost.BaseURL = "https://other.example.com"
	cfg.Workflows = append(cfg.Workflows, other)
	if err := cfg.validate(); err != nil {
		t.Fatal("independent generic routes rejected", err)
	}
	if len(cfg.Groups()) != 2 {
		t.Fatal("independent connections merged")
	}
	other.ID = "same-source"
	other.Mattermost = first.Mattermost
	cfg.Workflows = []Workflow{first, other}
	if err := cfg.validate(); err == nil {
		t.Fatal("overlapping routes accepted")
	}
	// No manually configured source ID or bot ID; credentials themselves are not identity.
	endpoint := first.Mattermost
	before := endpoint.Source("aaaaaaaaaaaaaaaaaaaaaaaaaa")
	endpoint.TokenEnv = "ROTATED_TOKEN"
	if before != endpoint.Source("aaaaaaaaaaaaaaaaaaaaaaaaaa") || before == endpoint.Source("bbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatal("identity depends on token reference or ignores bot")
	}
}
