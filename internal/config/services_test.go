package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestWorkflowServices(t *testing.T) {
	base := workflowText(t)
	dir := t.TempDir()
	loadServices := func(setting string) (Config, error) {
		t.Helper()
		writeWorkflow(t, dir, "assistant.md", strings.Replace(base, "---\n", "---\n"+setting+"\n", 1))
		return example(t, map[string]string{"WORKFLOWS_DIR": dir})
	}
	initial, err := loadServices("")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := loadServices("services: []")
	if err != nil || empty.Workflows[0].EffectiveRevision != initial.Workflows[0].EffectiveRevision {
		t.Fatal("empty services changed the legacy revision", err)
	}
	raw, err := json.Marshal(initial.Workflows[0])
	if err != nil || strings.Contains(string(raw), `"services"`) {
		t.Fatal("omitted services changed the legacy workflow fingerprint", err)
	}
	// Validation is offline; the connector needs neither the catalog nor values.
	configured, err := loadServices("services: [redmine, gitlab]\nenv_from: [EXTRA_TOKEN]")
	if err != nil {
		t.Fatal(err)
	}
	w := configured.Workflows[0]
	if !slices.Equal(w.Services, []string{"gitlab", "redmine"}) || !slices.Equal(w.EnvFrom, []string{"EXTRA_TOKEN"}) || w.EffectiveRevision == initial.Workflows[0].EffectiveRevision {
		t.Fatal("services were not normalized, did not rotate the session, or replaced env_from")
	}
	reordered, err := loadServices("services: [gitlab, redmine]\nenv_from: [EXTRA_TOKEN]")
	if err != nil || reordered.Workflows[0].EffectiveRevision != w.EffectiveRevision {
		t.Fatal("service order changed the revision", err)
	}
	if err = configured.validate(); err != nil || configured.Workflows[0].EffectiveRevision != w.EffectiveRevision {
		t.Fatal("repeat validation changed the revision", err)
	}
	for _, setting := range []string{
		"services: [gitlab, gitlab]",
		"services: ['']",
		"services: [Uppercase]",
		"services: [1first]",
		"services: [has.dot]",
		"services: [has space]",
		"services: [" + strings.Repeat("a", 65) + "]",
		"services: private-token",
		"services: {name: private-token}",
	} {
		t.Run(setting, func(t *testing.T) {
			_, err := loadServices(setting)
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatal("invalid services accepted or supplied content leaked", err)
			}
		})
	}
	if _, err := loadServices("services: [a, chat-assistant, service_1, " + strings.Repeat("a", 64) + "]"); err != nil {
		t.Fatal("valid boundary codes rejected", err)
	}
}
