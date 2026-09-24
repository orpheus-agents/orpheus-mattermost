package attachments

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

type Export struct {
	SourceID, BotID, ChannelID, SessionID, RunID string
	Limits                                       config.Files
}

func (o Output) Validate(e Export) error {
	if o.Schema != 1 || o.SourceID != e.SourceID || o.BotID != e.BotID || o.ChannelID != e.ChannelID || o.SessionID != e.SessionID || o.RunID != e.RunID || (o.Stage != "sealed" && o.Stage != "uploading" && o.Stage != "ready") || len(o.Files) > e.Limits.MaxOutputFiles {
		return errors.New("invalid output identity or limits")
	}
	seen := map[string]bool{}
	for _, f := range o.Files {
		if !fs.ValidPath(f.Path) || !strings.HasPrefix(f.Path, "snapshot/") || f.Name != SafeName(f.Name) || len(f.SHA256) != 64 || f.Size < 0 || f.Size > e.Limits.MaxOutputBytes || f.ArtifactID != ObjectHash([]string{o.RunID, strings.TrimPrefix(f.Path, "snapshot/"), f.SHA256}) || seen[f.ArtifactID] || (o.Stage == "ready" && !config.ValidID(f.FileID)) {
			return errors.New("invalid output artifact")
		}
		seen[f.ArtifactID] = true
	}
	b, _ := json.Marshal(o)
	if len(b) > 16<<10 {
		return errors.New("output manifest exceeds limit")
	}
	return nil
}
