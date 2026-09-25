//go:build live

package mattermost

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"
)

// This test is opt-in and confined to the explicitly configured test channel.
func TestLiveFileAndThread(t *testing.T) {
	base := os.Getenv("MM_TEST_URL")
	if base == "" {
		t.Skip("manual live environment is not configured")
	}
	c := New(base, os.Getenv("MATTERMOST_BOT_TOKEN"), 30*time.Second)
	ctx := t.Context()
	me, e := c.Me(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if me.ID != os.Getenv("MM_TEST_BOT_ID") {
		t.Fatal("unexpected bot")
	}
	ch, e := c.Channel(ctx, os.Getenv("MM_TEST_CHANNEL_ID"))
	if e != nil {
		t.Fatal(e)
	}
	if ch.Name != "dev-test-group" {
		t.Fatal("live test channel must be dev-test-group")
	}
	limit, e := c.MaxPostChars(ctx)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("server max post chars: %d", limit)
	root, e := c.Create(ctx, CreatePost{ChannelID: ch.ID, Message: "[orpheus-mattermost live test] API and attachment check; deleted after the test."})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := c.Delete(context.Background(), root.ID); e != nil {
			t.Log("cleanup root failed")
		}
	})
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 128, 255})
		}
	}
	var data bytes.Buffer
	if e = png.Encode(&data, img); e != nil {
		t.Fatal(e)
	}
	upload, e := c.Upload(ctx, ch.ID, "connector-check.png", bytes.NewReader(data.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	post, e := c.Create(ctx, CreatePost{ChannelID: ch.ID, RootID: root.ID, Message: "Test PNG attachment.", FileIDs: []string{upload.ID}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), post.ID) })
	info, e := c.File(ctx, upload.ID)
	if e != nil || info.PostID != post.ID {
		t.Fatalf("file association: %v", e)
	}
	downloaded, e := c.Download(ctx, upload.ID, 1<<20)
	if e != nil || !bytes.Equal(downloaded, data.Bytes()) {
		t.Fatal("download differs", e)
	}
	posts, e := c.Thread(ctx, post.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(posts) != 2 || posts[0].ID != root.ID || posts[1].ID != post.ID {
		t.Fatalf("thread has %d posts", len(posts))
	}
	files, e := c.Files(ctx, post.ID)
	if e != nil || len(files) != 1 {
		t.Fatal("post files", e)
	}
}
