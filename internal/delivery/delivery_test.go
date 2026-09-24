package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type fakeMM struct {
	posts      []mattermost.Post
	files      map[string]mattermost.FileInfo
	lose, drop bool
	calls      int
}

func (f *fakeMM) Post(_ context.Context, id string) (mattermost.Post, error) {
	for _, p := range f.posts {
		if p.ID == id {
			return p, nil
		}
	}
	return mattermost.Post{}, &mattermost.HTTPError{Status: 404}
}
func (f *fakeMM) Thread(context.Context, string) ([]mattermost.Post, error) { return f.posts, nil }
func (f *fakeMM) File(_ context.Context, id string) (mattermost.FileInfo, error) {
	v, ok := f.files[id]
	if !ok {
		return v, &mattermost.HTTPError{Status: 404}
	}
	return v, nil
}
func (f *fakeMM) Create(_ context.Context, p mattermost.CreatePost) (mattermost.Post, error) {
	f.calls++
	b, _ := json.Marshal(p.Props[Property])
	post := mattermost.Post{ID: fmt.Sprint(f.calls), ChannelID: p.ChannelID, RootID: p.RootID, UserID: "bot", Message: p.Message, FileIDs: p.FileIDs, Props: map[string]json.RawMessage{Property: b}}
	if f.drop {
		post.FileIDs = nil
		f.drop = false
	}
	for _, id := range post.FileIDs {
		v := f.files[id]
		v.PostID = post.ID
		f.files[id] = v
	}
	f.posts = append(f.posts, post)
	if f.lose {
		f.lose = false
		return mattermost.Post{}, errors.New("lost response")
	}
	return post, nil
}
func setup() (*Publisher, *fakeMM, Part) {
	mm := &fakeMM{files: map[string]mattermost.FileInfo{"file": {ID: "file"}}}
	key := conversation.Key{Source: "chat", Workflow: "assistant", Channel: "channel", Root: "root"}
	p := &Publisher{MM: mm, Key: key, BotID: "bot"}
	part := Parts(key, "session", "run", "answer", "Готово", conversation.Render{Version: 1, MaxChars: 64}, []attachments.Artifact{{ArtifactID: "artifact", FileID: "file", SHA256: "hash"}})[0]
	return p, mm, part
}
func TestLostCreateResponseReconciles(t *testing.T) {
	p, mm, part := setup()
	mm.lose = true
	if e := p.Publish(t.Context(), part); e == nil {
		t.Fatal("expected loss")
	}
	// There is no pending_post_id cache in this fake; only persisted receipts
	// survive the restart, including after the server's cache TTL expires.
	p = &Publisher{MM: mm, Key: p.Key, BotID: p.BotID}
	if e := p.Refresh(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := p.Publish(t.Context(), part); e != nil {
		t.Fatal(e)
	}
	if mm.calls != 1 {
		t.Fatal("duplicate post")
	}
}
func TestPartialAttachmentRepair(t *testing.T) {
	p, mm, part := setup()
	mm.drop = true
	if e := p.Publish(t.Context(), part); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e := p.Refresh(t.Context()); e != nil {
			t.Fatal(e)
		}
		if e := p.Publish(t.Context(), part); e != nil {
			t.Fatal(e)
		}
	}
	if mm.calls != 2 || mm.posts[1].Message == part.Text {
		t.Fatal("answer repeated", mm.calls)
	}
}

func TestPartialAnswerSurvivesRestart(t *testing.T) {
	p, mm, _ := setup()
	parts := Parts(p.Key, "session", "run", "message", strings.Repeat("answer ", 100), conversation.Render{Version: 2, MaxChars: 64}, nil)
	if len(parts) < 3 {
		t.Fatal("fixture must span several posts")
	}
	for i, part := range parts {
		mm.lose = i == 1
		err := p.Publish(t.Context(), part)
		if i == 1 {
			if err == nil {
				t.Fatal("expected lost response")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	p = &Publisher{MM: mm, Key: p.Key, BotID: p.BotID}
	if err := p.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		if err := p.Publish(t.Context(), part); err != nil {
			t.Fatal(err)
		}
	}
	if len(mm.posts) != len(parts) || mm.calls != len(parts) {
		t.Fatal("partial replay duplicated or lost chunks")
	}
	for i, post := range mm.posts {
		if post.Message != parts[i].Text {
			t.Fatal("replay changed chunk order")
		}
	}
}

func TestUnavailableReceiptLeavesDocumentedDuplicateWindow(t *testing.T) {
	p, mm, _ := setup()
	part := Parts(p.Key, "session", "run", "message", "answer", conversation.Render{Version: 2, MaxChars: 64}, nil)[0]
	mm.lose = true
	if err := p.Publish(t.Context(), part); err == nil {
		t.Fatal("expected lost response")
	}
	committed := mm.posts[0]
	// A temporarily invisible receipt and an expired server deduplication cache
	// cannot be distinguished from an uncommitted POST without another ledger.
	mm.posts = nil
	p = &Publisher{MM: mm, Key: p.Key, BotID: p.BotID}
	if err := p.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 2 || mm.posts[0].Message != committed.Message {
		t.Fatal("fixture did not exercise the residual duplicate window")
	}
}
func TestEditedReceiptStopsDelivery(t *testing.T) {
	p, mm, part := setup()
	if e := p.Publish(t.Context(), part); e != nil {
		t.Fatal(e)
	}
	mm.posts[0].Message = "edited"
	if e := p.Refresh(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := p.Publish(t.Context(), part); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}
func TestUnicodeChunks(t *testing.T) {
	parts := Split("абв🙂世界", 2)
	if len(parts) != 3 || parts[1] != "в🙂" {
		t.Fatal(parts)
	}
}
func TestForgedReceiptIgnored(t *testing.T) {
	p, mm, part := setup()
	if e := p.Publish(t.Context(), part); e != nil {
		t.Fatal(e)
	}
	mm.posts[0].UserID = "attacker"
	if e := p.Refresh(t.Context()); e != nil {
		t.Fatal(e)
	}
	part.Receipt.Attachments = nil
	if e := p.Publish(t.Context(), part); e != nil {
		t.Fatal(e)
	}
	if mm.calls != 2 {
		t.Fatal("forged receipt trusted")
	}
}

func TestMissingUploadReturnsForRecovery(t *testing.T) {
	p, mm, part := setup()
	delete(mm.files, "file")
	if err := p.Publish(t.Context(), part); !errors.Is(err, ErrUpload) {
		t.Fatal(err)
	}
	if mm.calls != 0 {
		t.Fatal("post created without available bytes")
	}
}
func TestDeletedDeliveryDoesNotReupload(t *testing.T) {
	p, mm, part := setup()
	if err := p.Publish(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	mm.posts = nil
	if err := p.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(t.Context(), part); !errors.Is(err, ErrUpload) {
		t.Fatal(err)
	}
	if mm.calls != 1 {
		t.Fatal("missing upload created a duplicate post")
	}
}
func TestForeignAttachmentBindingStopsDelivery(t *testing.T) {
	p, mm, part := setup()
	mm.files["file"] = mattermost.FileInfo{ID: "file", PostID: "foreign"}
	mm.posts = []mattermost.Post{{ID: "foreign", UserID: "other"}}
	if err := p.Publish(t.Context(), part); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if mm.calls != 0 {
		t.Fatal("foreign file rebound")
	}
}

func TestSplitPreservesUnicodeAndWhitespaceAtBoundaries(t *testing.T) {
	text := "Первая строка.\n\nВторая строка с пробелами.\nЕще одна строка."
	parts := Split(text, 25)
	if strings.Join(parts, "") != text {
		t.Fatal("split lost text or whitespace")
	}
	for _, part := range parts {
		if utf8.RuneCountInString(part) > 25 {
			t.Fatal("oversized part")
		}
	}
	if parts[0] != "Первая строка.\n\n" {
		t.Fatal("paragraph boundary ignored", parts)
	}
}

func TestAcceptedRendererRetainsOriginalChunkBoundaries(t *testing.T) {
	text := "Первая строка.\n\nВторая строка с пробелами."
	old := Parts(conversation.Key{}, "s", "r", "m", text, conversation.Render{Version: 1, MaxChars: 25}, nil)
	fresh := Parts(conversation.Key{}, "s", "r", "m", text, conversation.Render{Version: 2, MaxChars: 25}, nil)
	if utf8.RuneCountInString(old[0].Text) != 25 || old[0].Text == fresh[0].Text {
		t.Fatal("accepted render contract changed")
	}
}
