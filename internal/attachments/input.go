package attachments

import (
	"context"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type InputSource interface {
	Post(context.Context, string) (mattermost.Post, error)
	File(context.Context, string) (mattermost.FileInfo, error)
	Download(context.Context, string, int64) ([]byte, error)
}

func allowed(r Request, ch string) bool {
	if r.ChannelID == ch {
		return true
	}
	for _, p := range r.AllowedPairs {
		if p.Source == ch && p.Destination == r.ChannelID {
			return true
		}
	}
	return false
}

// Fetch validates current access and association before downloading. Per-file
// failures remain explicit in the manifest; transport failures cannot become files.
func inspectInput(ctx context.Context, source InputSource, r Request, f InputFile) (mattermost.FileInfo, int64, string) {
	post, e := source.Post(ctx, f.PostID)
	if e != nil || post.DeleteAt != 0 || post.ChannelID != f.ChannelID || !allowed(r, post.ChannelID) || !slices.Contains(post.FileIDs, f.FileID) {
		return mattermost.FileInfo{}, 0, "unavailable"
	}
	info, e := source.File(ctx, f.FileID)
	if e != nil || info.PostID != f.PostID || info.ID != f.FileID {
		return mattermost.FileInfo{}, 0, "unavailable"
	}
	limit := r.Limits.MaxFileBytes
	if strings.HasPrefix(info.MIME, "image/") {
		limit = r.Limits.MaxImageBytes
	}
	if info.Size > limit || info.Size < 0 {
		return info, limit, "file_limit_exceeded"
	}
	return info, limit, ""
}
func Fetch(ctx context.Context, source InputSource, r Request, f InputFile) (InputFile, []byte) {
	info, limit, status := inspectInput(ctx, source, r, f)
	if status != "" {
		f.Status = status
		return f, nil
	}
	b, e := source.Download(ctx, f.FileID, limit)
	if e != nil || int64(len(b)) != info.Size {
		f.Status = "download_failed"
		return f, nil
	}
	f.Size = info.Size
	f.MIME = info.MIME
	f.SHA256 = Hash(b)
	f.Status = "ready"
	return f, b
}
