// Package config loads immutable workflow definitions and routing policy.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	value, err := time.ParseDuration(string(b))
	*d = Duration(value)
	return err
}
func (d Duration) Value() time.Duration { return time.Duration(d) }

type Endpoint struct {
	BaseURL  string `yaml:"base_url"`
	TokenEnv string `yaml:"token_env"`
}
type Files struct {
	MaxPerPost     int   `yaml:"max_per_post" json:"max_per_post"`
	MaxFileBytes   int64 `yaml:"max_file_bytes" json:"max_file_bytes"`
	MaxImageBytes  int64 `yaml:"max_image_bytes" json:"max_image_bytes"`
	MaxBatchBytes  int64 `yaml:"max_batch_bytes" json:"max_batch_bytes"`
	MaxOutputFiles int   `yaml:"max_output_files" json:"max_output_files"`
	MaxOutputBytes int64 `yaml:"max_output_bytes" json:"max_output_bytes"`
}
type ChannelPair struct {
	Source      string `yaml:"source" json:"source"`
	Destination string `yaml:"destination" json:"destination"`
}
type Links struct {
	Enabled             *bool         `yaml:"enabled"`
	MaxLinks            int           `yaml:"max_links"`
	MaxPosts            int           `yaml:"max_posts"`
	AllowedChannelPairs []ChannelPair `yaml:"allowed_channel_pairs"`
}
type Workflow struct {
	Mattermost            Endpoint  `yaml:"mattermost"`
	ID                    string    `yaml:"id"`
	Revision              string    `yaml:"revision"`
	ReconcileFrom         string    `yaml:"reconcile_from"`
	Profile               string    `yaml:"profile"`
	SandboxTemplate       string    `yaml:"sandbox_template"`
	StartOnMention        *bool     `yaml:"start_on_mention"`
	DirectMessages        bool      `yaml:"direct_messages"`
	PrivateChannels       bool      `yaml:"private_channels"`
	GroupMessages         bool      `yaml:"group_messages"`
	SendCommentary        *bool     `yaml:"send_commentary_messages"`
	Draining              bool      `yaml:"draining"`
	IncludeIDs            []string  `yaml:"include_ids"`
	ExcludeIDs            []string  `yaml:"exclude_ids"`
	TriggerBotIDs         []string  `yaml:"trigger_bot_ids"`
	MessageBatchWindow    Duration  `yaml:"message_batch_window"`
	PollInterval          Duration  `yaml:"poll_interval"`
	FullReconcileInterval Duration  `yaml:"full_reconcile_interval"`
	MaxConcurrentRuns     int       `yaml:"max_concurrent_runs"`
	RunTimeoutSeconds     int       `yaml:"run_timeout_seconds"`
	HookTimeoutSeconds    int       `yaml:"hook_timeout_seconds"`
	MaxSessionTokens      int64     `yaml:"max_session_tokens"`
	MaxPostChars          int       `yaml:"max_post_chars"`
	InitialContextTokens  int       `yaml:"initial_context_token_budget"`
	Files                 Files     `yaml:"files"`
	Links                 Links     `yaml:"link_expansion"`
	Instructions          string    `yaml:"-"`
	Since                 time.Time `yaml:"-"`
	EffectiveRevision     string    `yaml:"-"`
}
type Config struct {
	AgentBoxAPIURL     string
	Orpheus            Endpoint
	Workflows          []Workflow
	WorkflowsDir       string
	Listen             string
	MaxRequestBytes    int
	MaxParallelThreads int
	HTTPTimeout        Duration
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var idPattern = regexp.MustCompile(`^[a-z0-9]{26}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

// Load reads process settings from ENV and all workflow Markdown files atomically.
// Secrets are resolved only when constructing clients, so validate remains offline.
func Load() (Config, error) {
	return load(os.LookupEnv)
}
func load(lookup func(string) (string, bool)) (Config, error) {
	value := func(key, fallback string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return fallback
	}
	cfg := Config{
		AgentBoxAPIURL: value("AGENTBOX_API_URL", ""),
		Orpheus:        Endpoint{BaseURL: value("ORPHEUS_BASE_URL", ""), TokenEnv: "ORPHEUS_API_KEY"},
		WorkflowsDir:   value("WORKFLOWS_DIR", "workflows"),
		Listen:         value("LISTEN_ADDR", ":8080"),
	}
	if strings.TrimSpace(cfg.WorkflowsDir) == "" || strings.TrimSpace(cfg.Listen) == "" {
		return Config{}, errors.New("WORKFLOWS_DIR and LISTEN_ADDR must not be empty")
	}
	for key, dst := range map[string]*int{"MAX_REQUEST_BYTES": &cfg.MaxRequestBytes, "MAX_PARALLEL_THREADS": &cfg.MaxParallelThreads} {
		fallback := "8"
		if key == "MAX_REQUEST_BYTES" {
			fallback = "1048576"
		}
		n, err := strconv.Atoi(value(key, fallback))
		if err != nil {
			return Config{}, fmt.Errorf("%s must be an integer", key)
		}
		*dst = n
	}
	if err := cfg.HTTPTimeout.UnmarshalText([]byte(value("HTTP_TIMEOUT", "30s"))); err != nil {
		return Config{}, errors.New("HTTP_TIMEOUT must be a duration")
	}
	entries, err := os.ReadDir(cfg.WorkflowsDir)
	if err != nil {
		return Config{}, fmt.Errorf("read workflows directory: %w", err)
	}
	// ReadDir sorts by filename; hidden files, editor backups and subdirectories are ignored.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") || strings.ToLower(filepath.Ext(name)) != ".md" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(cfg.WorkflowsDir, name))
		if err != nil {
			return Config{}, fmt.Errorf("read workflow %s: %w", name, err)
		}
		w, err := parseWorkflow(raw)
		if err != nil {
			return Config{}, fmt.Errorf("workflow %s: %w", name, err)
		}
		cfg.Workflows = append(cfg.Workflows, w)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
func parseWorkflow(raw []byte) (Workflow, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return Workflow{}, errors.New("YAML front matter is required")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		var w Workflow
		decoder := yaml.NewDecoder(strings.NewReader(strings.Join(lines[1:i], "\n")))
		decoder.KnownFields(true)
		if err := decoder.Decode(&w); err != nil {
			// Do not include YAML values: malformed deployment files can contain secrets.
			return Workflow{}, errors.New("invalid YAML front matter (unknown field, duplicate key or invalid type)")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return Workflow{}, errors.New("front matter must contain exactly one YAML document")
		}
		w.Instructions = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		return w, nil
	}
	return Workflow{}, errors.New("YAML front matter is not closed")
}
func validURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (c *Config) validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("LISTEN_ADDR must be a host:port address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("LISTEN_ADDR must contain a valid TCP port")
	}
	if !validURL(c.Orpheus.BaseURL) {
		return errors.New("API URLs must be absolute HTTP(S) URLs without credentials, query or fragment")
	}
	if c.AgentBoxAPIURL != "" && !validURL(c.AgentBoxAPIURL) {
		return errors.New("invalid AgentBox API URL")
	}
	if c.MaxRequestBytes < 4096 || c.MaxRequestBytes > 1<<20 || c.MaxParallelThreads < 1 || c.MaxParallelThreads > 128 || c.HTTPTimeout.Value() < time.Second {
		return errors.New("invalid request, concurrency or timeout limits")
	}
	if len(c.Workflows) == 0 {
		return errors.New("at least one workflow is required")
	}
	ids := map[string]bool{}
	dm := map[Endpoint]bool{}
	for i := range c.Workflows {
		w := &c.Workflows[i]
		if !validURL(w.Mattermost.BaseURL) || !envPattern.MatchString(w.Mattermost.TokenEnv) {
			return fmt.Errorf("workflow %s: mattermost.base_url and token_env are required", w.ID)
		}
		u, _ := url.Parse(w.Mattermost.BaseURL)
		u.Host = strings.ToLower(u.Host)
		w.Mattermost.BaseURL = strings.TrimRight(u.String(), "/")
		if !namePattern.MatchString(w.ID) || ids[w.ID] || strings.TrimSpace(w.Revision) == "" || w.Profile == "" || w.SandboxTemplate == "" {
			return fmt.Errorf("workflow %d: invalid identity, revision, profile or template", i)
		}
		ids[w.ID] = true
		since, err := time.Parse(time.RFC3339, w.ReconcileFrom)
		if err != nil || since.Location() != time.UTC {
			return fmt.Errorf("workflow %s: reconcile_from must be fixed RFC3339 UTC", w.ID)
		}
		w.Since = since
		if w.DirectMessages {
			if dm[w.Mattermost] {
				return errors.New("multiple DM workflows")
			}
			dm[w.Mattermost] = true
		}
		for _, list := range [][]string{w.IncludeIDs, w.ExcludeIDs, w.TriggerBotIDs} {
			for _, id := range list {
				if !ValidID(id) {
					return fmt.Errorf("workflow %s: invalid channel/bot ID", w.ID)
				}
			}
		}
		if w.StartOnMention == nil {
			w.StartOnMention = new(true)
		}
		if w.SendCommentary == nil {
			w.SendCommentary = new(true)
		}
		if w.MessageBatchWindow == 0 {
			w.MessageBatchWindow = Duration(2 * time.Second)
		}
		if w.PollInterval == 0 {
			w.PollInterval = Duration(30 * time.Second)
		}
		if w.FullReconcileInterval == 0 {
			w.FullReconcileInterval = Duration(5 * time.Minute)
		}
		if w.MaxConcurrentRuns == 0 {
			w.MaxConcurrentRuns = 10
		}
		if w.RunTimeoutSeconds == 0 {
			w.RunTimeoutSeconds = 3600
		}
		if w.HookTimeoutSeconds == 0 {
			w.HookTimeoutSeconds = 120
		}
		if w.MaxSessionTokens == 0 {
			w.MaxSessionTokens = 100000000
		}
		if w.MaxPostChars == 0 {
			w.MaxPostChars = 12000
		}
		if w.InitialContextTokens == 0 {
			w.InitialContextTokens = 100000
		}
		if w.MessageBatchWindow.Value() < time.Millisecond || w.PollInterval.Value() < time.Second || w.FullReconcileInterval < w.PollInterval || w.MaxConcurrentRuns < 1 || w.RunTimeoutSeconds < 1 || w.HookTimeoutSeconds < 1 || w.MaxSessionTokens < 1 || w.MaxPostChars < 64 || w.MaxPostChars > 16383 || w.InitialContextTokens < 64 {
			return fmt.Errorf("workflow %s: invalid limits", w.ID)
		}
		if w.Files.MaxPerPost == 0 {
			w.Files.MaxPerPost = 5
		}
		if w.Files.MaxFileBytes == 0 {
			w.Files.MaxFileBytes = 10 << 20
		}
		if w.Files.MaxImageBytes == 0 {
			w.Files.MaxImageBytes = 20 << 20
		}
		if w.Files.MaxBatchBytes == 0 {
			w.Files.MaxBatchBytes = 100 << 20
		}
		if w.Files.MaxOutputFiles == 0 {
			w.Files.MaxOutputFiles = 5
		}
		if w.Files.MaxOutputBytes == 0 {
			w.Files.MaxOutputBytes = 30 << 20
		}
		if w.Files.MaxPerPost < 1 || w.Files.MaxPerPost > 5 || w.Files.MaxOutputFiles < 1 || w.Files.MaxOutputFiles > 5 || w.Files.MaxFileBytes < 1 || w.Files.MaxImageBytes < 1 || w.Files.MaxBatchBytes < 1 || w.Files.MaxBatchBytes > 100<<20 || w.Files.MaxOutputBytes < 1 {
			return errors.New("invalid attachment limits")
		}
		if w.Links.Enabled == nil {
			w.Links.Enabled = new(true)
		}
		if w.Links.MaxLinks == 0 {
			w.Links.MaxLinks = 5
		}
		if w.Links.MaxPosts == 0 {
			w.Links.MaxPosts = 20
		}
		if w.Links.MaxLinks < 1 || w.Links.MaxLinks > 5 || w.Links.MaxPosts < 2 || w.Links.MaxPosts > 20 {
			return errors.New("invalid link expansion limits")
		}
		for _, pair := range w.Links.AllowedChannelPairs {
			if !ValidID(pair.Source) || !ValidID(pair.Destination) {
				return errors.New("invalid allowed channel pair")
			}
		}
		// Runtime polling and drain switches do not invalidate a retained sandbox.
		semantic := *w
		semantic.Mattermost = Endpoint{}
		semantic.EffectiveRevision = ""
		semantic.Since = time.Time{}
		semantic.ReconcileFrom = ""
		semantic.Draining = false
		semantic.PollInterval = 0
		semantic.FullReconcileInterval = 0
		semantic.MaxConcurrentRuns = 0
		semantic.IncludeIDs = nil
		semantic.ExcludeIDs = nil
		semantic.TriggerBotIDs = nil
		semantic.StartOnMention = nil
		semantic.DirectMessages = false
		semantic.PrivateChannels = false
		semantic.GroupMessages = false
		semantic.MessageBatchWindow = 0
		b, _ := json.Marshal(semantic)
		// A message-format change requires a fresh session even when the workflow file is unchanged.
		h := sha256.Sum256(append(b, (sandbox.Digest() + ":mattermost-front-matter-v2")...))
		w.EffectiveRevision = w.Revision + ":" + hex.EncodeToString(h[:])
	}
	for i, w := range c.Workflows {
		for _, other := range c.Workflows[i+1:] {
			if w.Mattermost != other.Mattermost {
				continue
			}
			if len(w.IncludeIDs) == 0 && len(other.IncludeIDs) == 0 {
				return errors.New("multiple generic workflows")
			}
			for _, id := range w.IncludeIDs {
				if slices.Contains(other.IncludeIDs, id) && !slices.Contains(w.ExcludeIDs, id) && !slices.Contains(other.ExcludeIDs, id) {
					return errors.New("overlapping workflow channels")
				}
			}
		}
	}
	return nil
}
func (c Config) Route(channelID, kind string) (*Workflow, error) {
	var selected *Workflow
	specialized := false
	for _, w := range c.Workflows {
		if slices.Contains(w.IncludeIDs, channelID) {
			specialized = true
		}
	}
	for i := range c.Workflows {
		w := &c.Workflows[i]
		allowed := kind == "O" || kind == "P" && w.PrivateChannels || kind == "G" && w.GroupMessages || kind == "D" && w.DirectMessages
		if !allowed {
			continue
		}
		if kind != "D" && (slices.Contains(w.ExcludeIDs, channelID) || len(w.IncludeIDs) > 0 && !slices.Contains(w.IncludeIDs, channelID) || len(w.IncludeIDs) == 0 && specialized) {
			continue
		}
		if selected != nil {
			return nil, errors.New("ambiguous channel owner")
		}
		selected = w
	}
	return selected, nil
}
func (w Workflow) AllowsLink(source, dest string) bool {
	return source == dest || slices.Contains(w.Links.AllowedChannelPairs, ChannelPair{source, dest})
}
func Secret(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("required environment variable %s is unset", name)
	}
	return value, nil
}

// Groups separates connections while retaining routing between workflows using
// the same Mattermost URL and credential reference.
func (c Config) Groups() map[Endpoint]Config {
	groups := map[Endpoint]Config{}
	for _, workflow := range c.Workflows {
		group, ok := groups[workflow.Mattermost]
		if !ok {
			group = c
			group.Workflows = nil
		}
		group.Workflows = append(group.Workflows, workflow)
		groups[workflow.Mattermost] = group
	}
	return groups
}

// Source identifies the installation and authenticated bot without deployment IDs.
func (e Endpoint) Source(botID string) string {
	hash := sha256.Sum256([]byte(e.BaseURL + "\n" + botID))
	return hex.EncodeToString(hash[:16])
}
