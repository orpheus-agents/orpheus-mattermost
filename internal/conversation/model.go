// Package conversation holds connector identity and durable input contracts.
package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

var operationNamespace = uuid.MustParse("1cd37086-3385-5b91-8925-e9f1fd9fe83d")

type Key struct {
	Source   string
	Workflow string
	Channel  string
	Root     string
}

func (k Key) External() string {
	return fmt.Sprintf("%s:channel:%s:thread:%s", k.Source, k.Channel, k.Root)
}
func (k Key) Namespace() string { return "mattermost/" + k.Workflow }
func (k Key) MessageKey(anchor string) string {
	return fmt.Sprintf("mm:v1:%s:%s:%s", k.Source, k.Root, anchor)
}
func (k Key) Operation(anchor, operation, target, predecessor string) string {
	b, _ := json.Marshal([]string{k.Workflow, k.Source, k.Channel, k.Root, anchor, operation, target, predecessor})
	return uuid.NewSHA1(operationNamespace, b).String()
}
func ParseKey(source, workflow, external string) (Key, error) {
	prefix := source + ":channel:"
	rest, ok := strings.CutPrefix(external, prefix)
	if !ok {
		return Key{}, errors.New("external source mismatch")
	}
	channel, root, ok := strings.Cut(rest, ":thread:")
	if !ok || !config.ValidID(channel) || !config.ValidID(root) {
		return Key{}, errors.New("invalid external thread key")
	}
	return Key{source, workflow, channel, root}, nil
}

type Version struct {
	ID       string `json:"id"`
	EditedAt int64  `json:"edit_at"`
	Hash     string `json:"sha256"`
}
type Render struct {
	Version    int  `json:"version"`
	MaxChars   int  `json:"max_chars"`
	Commentary bool `json:"commentary"`
}
type Envelope struct {
	Schema      int                 `json:"schema"`
	Source      string              `json:"source"`
	Workflow    string              `json:"workflow"`
	Revision    string              `json:"revision"`
	Channel     string              `json:"channel"`
	Root        string              `json:"root"`
	Anchor      string              `json:"anchor"`
	WindowEnd   int64               `json:"window_end"`
	Kind        string              `json:"submission_kind"`
	TriggerIDs  []string            `json:"trigger_post_ids"`
	ContextIDs  []string            `json:"context_post_ids"`
	Versions    []Version           `json:"versions"`
	Request     attachments.Request `json:"input"`
	Render      Render              `json:"render"`
	Predecessor string              `json:"predecessor,omitempty"`
}

func (e Envelope) Key() Key                { return Key{e.Source, e.Workflow, e.Channel, e.Root} }
func HasMetadata(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }
func Decode(raw json.RawMessage) (Envelope, error) {
	var e Envelope
	if len(raw) == 0 {
		return e, errors.New("missing connector metadata")
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, errors.New("invalid connector envelope")
	}
	if e.Schema != 1 || e.Anchor == "" || (e.Render.Version != 1 && e.Render.Version != 2) || e.Render.MaxChars < 64 || len(e.TriggerIDs) == 0 || (e.Kind != "initial" && e.Kind != "clarification") {
		return e, errors.New("invalid input contract")
	}
	return e, nil
}

type Message struct {
	ID          string
	RunID       string
	Role        string
	Kind        string
	Text        string
	Metadata    json.RawMessage
	ExternalKey string
	Delivery    string
	Error       string
	Position    int
	CreatedAt   time.Time
}
type Hook struct {
	Name         string
	Status       string
	Completeness string
	Output       string
}
type Run struct {
	ID             string
	SessionID      string
	Number         int
	Status         string
	Observation    string
	Error          string
	StopReason     string
	AgentStatus    string
	Hooks          []Hook
	FinalMessageID string
	CreatedAt      time.Time
	FinishedAt     *time.Time
	DeadlineAt     *time.Time
}

func (r Run) Terminal() bool {
	return r.Status == "completed" || r.Status == "failed" || r.Status == "cancelled"
}

type Session struct {
	ID               string
	ExternalKey      string
	CreatedAt        time.Time
	SandboxID        string
	SandboxState     string
	SandboxLastState string
	Workspace        string
	TotalTokens      int64
	MaxTokens        int64
	Revision         string
	Runs             []Run
	Messages         []Message
}

func (s Session) Latest() *Run {
	if len(s.Runs) == 0 {
		return nil
	}
	return &s.Runs[len(s.Runs)-1]
}
func (s Session) Reusable(revision string) bool {
	r := s.Latest()
	return s.Revision == revision && s.SandboxState != "unavailable" && s.TotalTokens < s.MaxTokens && (r == nil || (r.Error != "sandbox_lost" && r.Error != "context_lost" && r.StopReason != "token_limit"))
}

type Accepted struct {
	SessionID string
	RunID     string
	MessageID string
}
type Snapshot struct{ Sessions []Session }

// Accepted and context are separate: linked/passive context cannot consume a trigger.
func (s Snapshot) Known() (map[string]bool, map[string]bool) {
	triggers := map[string]bool{}
	context := map[string]bool{}
	for _, session := range s.Sessions {
		for _, m := range session.Messages {
			if m.Role != "user" {
				continue
			}
			e, err := Decode(m.Metadata)
			if err != nil {
				continue
			}
			for _, id := range e.TriggerIDs {
				triggers[id] = true
			}
			if m.Delivery == "delivered" {
				for _, id := range e.ContextIDs {
					context[id] = true
				}
				for _, id := range e.TriggerIDs {
					context[id] = true
				}
			}
		}
	}
	return triggers, context
}
