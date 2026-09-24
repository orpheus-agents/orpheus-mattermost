// Package attachments prepares immutable inputs and exports a run's sealed output.
package attachments

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

const Base = ".orpheus/mattermost"

type InputFile struct {
	PostID    string `json:"post_id"`
	FileID    string `json:"file_id"`
	Origin    string `json:"origin"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size_bytes"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256,omitempty"`
	Status    string `json:"status,omitempty"`
	ChannelID string `json:"channel_id"`
}
type Request struct {
	BotID            string               `json:"bot_id"`
	Schema           int                  `json:"schema"`
	SourceID         string               `json:"source_id"`
	ChannelID        string               `json:"channel_id"`
	RootID           string               `json:"root_id"`
	AnchorID         string               `json:"anchor_post_id"`
	Files            []InputFile          `json:"files"`
	Limits           config.Files         `json:"limits"`
	AllowedPairs     []config.ChannelPair `json:"allowed_channel_pairs,omitempty"`
	PreviousIndex    string               `json:"previous_index,omitempty"`
	DeliveredBatches []string             `json:"delivered_batches,omitempty"`
}
type Manifest struct {
	Schema      int         `json:"schema"`
	SourceID    string      `json:"source_id"`
	AnchorID    string      `json:"anchor_post_id"`
	RequestHash string      `json:"request_hash"`
	Files       []InputFile `json:"files"`
}
type Artifact struct {
	ArtifactID string `json:"artifact_id"`
	FileID     string `json:"file_id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size_bytes"`
}
type Output struct {
	Schema    int        `json:"schema"`
	SourceID  string     `json:"source_id"`
	BotID     string     `json:"bot_id"`
	ChannelID string     `json:"channel_id"`
	SessionID string     `json:"session_id"`
	RunID     string     `json:"run_id"`
	Stage     string     `json:"stage"`
	Files     []Artifact `json:"files"`
	Errors    []string   `json:"errors,omitempty"`
}

func Hash(b []byte) string    { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ObjectHash(v any) string { b, _ := json.Marshal(v); return Hash(b) }

func BatchPath(anchor string) string    { return path.Join(Base, "input/batches", anchor) }
func ManifestPath(anchor string) string { return path.Join(BatchPath(anchor), "manifest.json") }
func IndexPath(run string) string       { return path.Join(Base, "runs", run, "input-index.json") }
func OutputPath(run string) string      { return path.Join(Base, "output", run) }
func SafeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == ':' {
			r = '_'
		}
		if b.Len()+len(string(r)) > 180 {
			break
		}
		b.WriteRune(r)
	}
	safe := strings.Trim(b.String(), " .")
	if safe == "" {
		safe = "attachment"
	}
	return safe
}
func InputPath(id, name string) string { return path.Join(Base, "input/files", id, SafeName(name)) }
func (r Request) Validate() error {
	if r.Schema != 1 || r.SourceID == "" || !config.ValidID(r.ChannelID) || !config.ValidID(r.RootID) || !config.ValidID(r.AnchorID) {
		return fmt.Errorf("invalid input request identity")
	}
	for _, f := range r.Files {
		if !config.ValidID(f.FileID) || !config.ValidID(f.PostID) || !config.ValidID(f.ChannelID) || f.Path != InputPath(f.FileID, f.Name) {
			return fmt.Errorf("invalid attachment path or identity")
		}
	}
	if r.Limits.MaxBatchBytes < 1 || r.Limits.MaxFileBytes < 1 || r.Limits.MaxImageBytes < 1 {
		return fmt.Errorf("invalid attachment limits")
	}
	return nil
}
