package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Access maps confirmed source identities into an integration's service context.
// The provider owns the meaning of fields and mapped identities.
type Access struct {
	Namespace          string             `yaml:"namespace" json:"namespace"`
	BotID              string             `yaml:"bot_id" json:"bot_id"`
	ScopeContext       map[string]any     `yaml:"scope_context" json:"scope_context"`
	IdentityProjection IdentityProjection `yaml:"identity_projection" json:"identity_projection"`
}

type IdentityProjection struct {
	ActorField    string               `yaml:"actor_field" json:"actor_field"`
	AudienceField string               `yaml:"audience_field" json:"audience_field"`
	Subjects      map[string]SubjectID `yaml:"subjects" json:"subjects"`
}

// SubjectID preserves a configured string or decimal integer without float64
// conversion. Integer values are bounded by length, not a service's ID range.
type SubjectID json.RawMessage

var integerIDPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func (id SubjectID) MarshalJSON() ([]byte, error) {
	if len(id) == 0 || len(id) > 1024 {
		return nil, errors.New("invalid mapped identity")
	}
	if id[0] == '"' {
		var value string
		if json.Unmarshal(id, &value) != nil || len(value) > 128 || strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return nil, errors.New("invalid mapped string identity")
		}
		return json.Marshal(value)
	}
	if len(id) > 128 || !integerIDPattern.Match(id) {
		return nil, errors.New("mapped identity must be a string or integer")
	}
	if string(id) == "-0" {
		return []byte("0"), nil
	}
	return id, nil
}

func (id *SubjectID) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("mapped identity must be a scalar")
	}
	var raw []byte
	switch node.Tag {
	case "!!str":
		raw, _ = json.Marshal(node.Value)
	case "!!int":
		raw = []byte(node.Value)
	default:
		return errors.New("mapped identity must be a string or decimal integer")
	}
	canonical, err := SubjectID(raw).MarshalJSON()
	if err != nil {
		return err
	}
	*id = SubjectID(canonical)
	return nil
}

func (a Access) Validate() error {
	if !accessNamePattern.MatchString(a.Namespace) || !ValidID(a.BotID) || a.ScopeContext == nil {
		return errors.New("invalid protected workflow binding")
	}
	p := a.IdentityProjection
	if !envPattern.MatchString(p.ActorField) || !envPattern.MatchString(p.AudienceField) || len(p.ActorField) > 128 || len(p.AudienceField) > 128 || p.ActorField == p.AudienceField {
		return errors.New("invalid identity projection fields")
	}
	if _, exists := a.ScopeContext[p.ActorField]; exists {
		return errors.New("actor field overlaps static context")
	}
	if _, exists := a.ScopeContext[p.AudienceField]; exists {
		return errors.New("audience field overlaps static context")
	}
	raw, err := json.Marshal(a.ScopeContext)
	if err != nil || len(raw) > 16<<10 || len(a.ScopeContext) > 64 {
		return errors.New("invalid static service context")
	}
	if len(p.Subjects) == 0 || len(p.Subjects) > 4096 {
		return errors.New("invalid identity mapping size")
	}
	seen := make(map[string]bool, len(p.Subjects))
	for source, id := range p.Subjects {
		if !ValidID(source) || source == a.BotID {
			return errors.New("invalid mapped source identity")
		}
		value, err := id.MarshalJSON()
		if err != nil || seen[string(value)] {
			return errors.New("invalid or duplicate mapped identity")
		}
		seen[string(value)] = true
	}
	return nil
}

// Digest binds recovered transport proof to the exact projection configuration.
// Workflow revisions already include this configuration and rotate on changes.
func (a Access) Digest() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
