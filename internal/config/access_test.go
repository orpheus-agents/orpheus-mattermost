package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func accessFixture() *Access {
	return &Access{
		Namespace: "mattermost:test", BotID: "bbbbbbbbbbbbbbbbbbbbbbbbbb",
		ScopeContext:       map[string]any{"workspace": "test"},
		IdentityProjection: IdentityProjection{ActorField: "requester", AudienceField: "readers", Subjects: map[string]SubjectID{"hhhhhhhhhhhhhhhhhhhhhhhhhh": SubjectID(`42`)}},
	}
}

func TestProtectedWorkflow(t *testing.T) {
	base, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	configure := func() Config {
		c := base
		c.Workflows = []Workflow{base.Workflows[0]}
		w := &c.Workflows[0]
		w.Access = accessFixture()
		w.Services = []string{"protected"}
		w.IncludeIDs = []string{"cccccccccccccccccccccccccc"}
		w.PrivateChannels, w.DirectMessages, w.GroupMessages = true, false, false
		w.StartOnMention, w.Links.Enabled = new(true), new(false)
		return c
	}
	c := configure()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Workflows[0].EffectiveRevision == base.Workflows[0].EffectiveRevision {
		t.Fatal("binding did not rotate revision")
	}
	for name, change := range map[string]func(*Workflow){
		"namespace":   func(w *Workflow) { w.Access.Namespace = "" },
		"bot":         func(w *Workflow) { w.Access.BotID = "bad" },
		"actor field": func(w *Workflow) { w.Access.IdentityProjection.ActorField = "" },
		"scope":       func(w *Workflow) { w.Access.ScopeContext = nil },
		"links":       func(w *Workflow) { w.Links.Enabled = new(true) },
		"dm":          func(w *Workflow) { w.DirectMessages = true },
		"group":       func(w *Workflow) { w.GroupMessages = true },
		"channels":    func(w *Workflow) { w.IncludeIDs = nil },
		"private":     func(w *Workflow) { w.PrivateChannels = false },
		"services":    func(w *Workflow) { w.Services = nil },
		"mention":     func(w *Workflow) { w.StartOnMention = new(false) },
	} {
		t.Run(name, func(t *testing.T) {
			c := configure()
			change(&c.Workflows[0])
			if c.validate() == nil {
				t.Fatal("unsafe binding accepted")
			}
		})
	}
}

func TestIdentityProjectionValidation(t *testing.T) {
	for name, change := range map[string]func(*Access){
		"same fields":                func(a *Access) { a.IdentityProjection.AudienceField = a.IdentityProjection.ActorField },
		"actor overlap":              func(a *Access) { a.ScopeContext["requester"] = "forged" },
		"audience overlap":           func(a *Access) { a.ScopeContext["readers"] = []int{1} },
		"invalid field":              func(a *Access) { a.IdentityProjection.ActorField = "nested.actor" },
		"empty mapping":              func(a *Access) { a.IdentityProjection.Subjects = nil },
		"invalid source":             func(a *Access) { a.IdentityProjection.Subjects["bad"] = SubjectID(`43`) },
		"own bot mapped":             func(a *Access) { a.IdentityProjection.Subjects[a.BotID] = SubjectID(`43`) },
		"duplicate service identity": func(a *Access) { a.IdentityProjection.Subjects["nnnnnnnnnnnnnnnnnnnnnnnnnn"] = SubjectID(`42`) },
		"duplicate escaped identity": func(a *Access) {
			a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`"person"`)
			a.IdentityProjection.Subjects["nnnnnnnnnnnnnnnnnnnnnnnnnn"] = SubjectID(`"\u0070erson"`)
		},
		"duplicate zero": func(a *Access) {
			a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`0`)
			a.IdentityProjection.Subjects["nnnnnnnnnnnnnnnnnnnnnnnnnn"] = SubjectID(`-0`)
		},
		"object identity": func(a *Access) { a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`{}`) },
		"float identity":  func(a *Access) { a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`1.0`) },
		"null identity":   func(a *Access) { a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`null`) },
		"blank identity":  func(a *Access) { a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`" "`) },
		"oversized scope": func(a *Access) { a.ScopeContext["large"] = strings.Repeat("x", 16<<10) },
	} {
		t.Run(name, func(t *testing.T) {
			a := accessFixture()
			change(a)
			if a.Validate() == nil {
				t.Fatal("invalid projection accepted")
			}
		})
	}
}

func TestIdentityProjectionYAML(t *testing.T) {
	for _, tc := range []struct {
		value, want string
		valid       bool
	}{
		{"9007199254740993", "9007199254740993", true},
		{"-7", "-7", true},
		{"0", "0", true},
		{"'employee-42'", `"employee-42"`, true},
		{"'42'", `"42"`, true},
		{"true", "", false}, {"null", "", false}, {"1.0", "", false},
		{"0x2a", "", false}, {"01", "", false}, {"[]", "", false},
		{"{}", "", false}, {"''", "", false}, {"'   '", "", false},
		{"'" + strings.Repeat("x", 129) + "'", "", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			source := "namespace: mattermost:test\nbot_id: bbbbbbbbbbbbbbbbbbbbbbbbbb\nscope_context: {workspace: test}\nidentity_projection:\n  actor_field: requester\n  audience_field: readers\n  subjects:\n    hhhhhhhhhhhhhhhhhhhhhhhhhh: " + tc.value + "\n"
			var a Access
			err := yaml.Unmarshal([]byte(source), &a)
			if err == nil {
				err = a.Validate()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if tc.valid {
				got, err := json.Marshal(a.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"])
				if err != nil || string(got) != tc.want {
					t.Fatalf("mapped ID=%s, err=%v", got, err)
				}
			}
		})
	}
	var a Access
	if yaml.Unmarshal([]byte("identity_projection:\n  subjects:\n    hhhhhhhhhhhhhhhhhhhhhhhhhh: 1\n    hhhhhhhhhhhhhhhhhhhhhhhhhh: 2\n"), &a) == nil {
		t.Fatal("duplicate source mapping accepted")
	}
}

func TestProjectionChangesRotateWorkflowRevision(t *testing.T) {
	base, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &base.Workflows[0]
	w.Access = accessFixture()
	w.Services = []string{"protected"}
	w.IncludeIDs = []string{"cccccccccccccccccccccccccc"}
	w.PrivateChannels, w.DirectMessages, w.GroupMessages = true, false, false
	w.StartOnMention, w.Links.Enabled = new(true), new(false)
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	original := w.EffectiveRevision
	w.Access.IdentityProjection.Subjects["hhhhhhhhhhhhhhhhhhhhhhhhhh"] = SubjectID(`43`)
	if err := base.validate(); err != nil || w.EffectiveRevision == original {
		t.Fatal("mapping change retained revision", err)
	}
	changed := w.EffectiveRevision
	if err := base.validate(); err != nil || w.EffectiveRevision != changed {
		t.Fatal("validation changed stable revision", err)
	}
}
