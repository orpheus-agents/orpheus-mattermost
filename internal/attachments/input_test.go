package attachments

import (
	"context"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type source struct {
	channel, post string
	data          []byte
}

func (s source) Post(context.Context, string) (mattermost.Post, error) {
	return mattermost.Post{ID: s.post, ChannelID: s.channel, FileIDs: []string{"bbbbbbbbbbbbbbbbbbbbbbbbbb"}}, nil
}
func (s source) File(context.Context, string) (mattermost.FileInfo, error) {
	return mattermost.FileInfo{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", PostID: s.post, Size: int64(len(s.data)), MIME: "image/png"}, nil
}
func (s source) Download(context.Context, string, int64) ([]byte, error) { return s.data, nil }
func TestFetchChecksAssociationAccessAndLimits(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const fid = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	req := Request{Schema: 1, SourceID: "chat", ChannelID: id, RootID: id, AnchorID: id, Limits: config.Files{MaxPerPost: 5, MaxFileBytes: 100, MaxImageBytes: 100, MaxBatchBytes: 100}}
	file := InputFile{PostID: id, FileID: fid, ChannelID: id, Name: "image.png", Path: InputPath(fid, "image.png")}
	req.Files = []InputFile{file}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	src := source{id, id, []byte("png")}
	got, data := Fetch(t.Context(), src, req, file)
	if got.Status != "ready" || got.SHA256 != Hash(data) {
		t.Fatal(got)
	}
	req.Limits.MaxImageBytes = 1
	got, _ = Fetch(t.Context(), src, req, file)
	if got.Status != "file_limit_exceeded" {
		t.Fatal(got)
	}
	req.Limits.MaxImageBytes = 100
	src.channel = "cccccccccccccccccccccccccc"
	file.ChannelID = src.channel
	got, _ = Fetch(t.Context(), src, req, file)
	if got.Status != "unavailable" {
		t.Fatal(got)
	}
	req.AllowedPairs = []config.ChannelPair{{Source: src.channel, Destination: id}}
	got, _ = Fetch(t.Context(), src, req, file)
	if got.Status != "ready" {
		t.Fatal(got)
	}
	src.post = fid
	got, _ = Fetch(t.Context(), src, req, file)
	if got.Status != "unavailable" {
		t.Fatal(got)
	}
	req.Files[0].Path = "../escape"
	if req.Validate() == nil {
		t.Fatal("traversal accepted")
	}
}
