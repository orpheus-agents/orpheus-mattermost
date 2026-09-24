// Package delivery publishes immutable chunks and verifies Mattermost receipts.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

const Property = "orpheus_delivery"

var ErrConflict = errors.New("delivery receipt conflict")
var ErrUpload = errors.New("attachment requires upload recovery")

type Receipt struct {
	Schema        int                    `json:"schema"`
	Source        string                 `json:"source"`
	Workflow      string                 `json:"workflow"`
	SessionID     string                 `json:"session_id"`
	RunID         string                 `json:"run_id"`
	MessageID     string                 `json:"message_id"`
	Part          int                    `json:"part"`
	Parts         int                    `json:"parts"`
	Hash          string                 `json:"content_sha256"`
	Attachments   []attachments.Artifact `json:"attachments"`
	RenderVersion int                    `json:"render_version"`
	TriggerIDs    []string               `json:"trigger_post_ids,omitempty"`
	InputHash     string                 `json:"input_hash,omitempty"`
	Revision      string                 `json:"revision,omitempty"`
	InputVersions []conversation.Version `json:"input_versions,omitempty"`
}
type Part struct {
	Receipt Receipt
	Text    string
}
type Source interface {
	Post(context.Context, string) (mattermost.Post, error)
	Thread(context.Context, string) ([]mattermost.Post, error)
	Create(context.Context, mattermost.CreatePost) (mattermost.Post, error)
	File(context.Context, string) (mattermost.FileInfo, error)
}
type Publisher struct {
	MM              Source
	Key             conversation.Key
	BotID           string
	Posts           []mattermost.Post
	SkipAttachments bool
}

func (p *Publisher) Refresh(ctx context.Context) error {
	posts, e := p.MM.Thread(ctx, p.Key.Root)
	if e != nil {
		return e
	}
	p.Posts = posts
	return nil
}
func (p *Publisher) receipt(post mattermost.Post) (Receipt, bool) {
	var r Receipt
	if post.UserID != p.BotID || post.ChannelID != p.Key.Channel || post.Root() != p.Key.Root || post.DeleteAt != 0 {
		return r, false
	}
	if json.Unmarshal(post.Props[Property], &r) != nil || r.Schema != 1 || r.Source != p.Key.Source || r.Workflow != p.Key.Workflow {
		return r, false
	}
	return r, true
}
func sameKey(a, b Receipt) bool {
	return a.Source == b.Source && a.Workflow == b.Workflow && a.SessionID == b.SessionID && a.RunID == b.RunID && a.MessageID == b.MessageID && a.Part == b.Part
}
func sameArtifacts(a, b []attachments.Artifact) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ArtifactID != b[i].ArtifactID || a[i].SHA256 != b[i].SHA256 {
			return false
		}
	}
	return true
}
func (p *Publisher) find(part Part) (mattermost.Post, Receipt, bool, error) {
	var found mattermost.Post
	var receipt Receipt
	ok := false
	for _, post := range p.Posts {
		r, valid := p.receipt(post)
		if !valid || !sameKey(r, part.Receipt) {
			continue
		}
		if r.Hash != part.Receipt.Hash || attachments.Hash([]byte(post.Message)) != r.Hash || r.Parts != part.Receipt.Parts || r.RenderVersion != part.Receipt.RenderVersion || (!p.SkipAttachments && !sameArtifacts(r.Attachments, part.Receipt.Attachments)) {
			return post, r, true, ErrConflict
		}
		if ok {
			return post, r, true, ErrConflict
		}
		found, receipt, ok = post, r, true
	}
	return found, receipt, ok, nil
}
func (p *Publisher) verified(ctx context.Context, post mattermost.Post, a attachments.Artifact) (bool, error) {
	if !slices.Contains(post.FileIDs, a.FileID) {
		return false, nil
	}
	info, e := p.MM.File(ctx, a.FileID)
	if mattermost.Status(e) == 404 {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return info.ID == a.FileID && info.PostID == post.ID, nil
}
func (p *Publisher) NoticeExists(kind, run string) bool {
	for _, post := range p.Posts {
		r, ok := p.receipt(post)
		if ok && r.MessageID == kind && r.RunID == run && r.Hash == attachments.Hash([]byte(post.Message)) {
			return true
		}
	}
	return false
}
func (p *Publisher) Rejected(revision string) map[string]bool {
	ids := map[string]bool{}
	current := map[string]mattermost.Post{}
	for _, post := range p.Posts {
		current[post.ID] = post
	}
	for _, post := range p.Posts {
		r, ok := p.receipt(post)
		if !ok || r.MessageID != "input_rejected" || r.Hash != attachments.Hash([]byte(post.Message)) || r.InputHash == "" || r.Revision != revision {
			continue
		}
		for _, v := range r.InputVersions {
			source, exists := current[v.ID]
			if exists && slices.Contains(r.TriggerIDs, v.ID) && conversation.PostVersion(source) == v {
				ids[v.ID] = true
			}
		}
	}
	return ids
}

// Publish verifies persisted receipts first. Partial attachment creates a repair
// post with no repeated answer text. Missing uploads are reported without sandbox recovery.
func (p *Publisher) Publish(ctx context.Context, part Part) error {
	post, r, found, e := p.find(part)
	if e != nil {
		return e
	}
	if found {
		if p.SkipAttachments {
			return nil
		}
		for i, a := range r.Attachments {
			ok, e := p.verified(ctx, post, a)
			if e != nil {
				return e
			}
			if ok {
				continue
			}
			repair := part
			repair.Text = "Вложение к ответу."
			repair.Receipt.MessageID = "attachment_repair:" + part.Receipt.MessageID + ":" + a.ArtifactID
			repair.Receipt.Part = 0
			repair.Receipt.Parts = 1
			repair.Receipt.Hash = attachments.Hash([]byte(repair.Text))
			repair.Receipt.Attachments = []attachments.Artifact{part.Receipt.Attachments[i]}
			// The repair has its own receipt, so it remains idempotent after reupload.
			if e = p.publishOne(ctx, repair); e != nil {
				return e
			}
		}
		return nil
	}
	err := p.publishOne(ctx, part)
	if errors.Is(err, ErrUpload) {
		// Only a newly created, partially attached post can progress into repair.
		// An unavailable upload without a receipt must return to the coordinator.
		if _, _, found, findErr := p.find(part); findErr != nil {
			return findErr
		} else if found {
			return p.Publish(ctx, part)
		}
	}
	return err
}
func (p *Publisher) publishOne(ctx context.Context, part Part) error {
	post, r, found, e := p.find(part)
	if e != nil {
		return e
	}
	if found {
		if p.SkipAttachments {
			return nil
		}
		for _, a := range r.Attachments {
			ok, e := p.verified(ctx, post, a)
			if e != nil {
				return e
			}
			if !ok {
				return ErrUpload
			}
		}
		return nil
	}
	part.Receipt.Attachments = slices.Clone(part.Receipt.Attachments)
	if p.SkipAttachments {
		part.Receipt.Attachments = nil
	}
	var ids []string
	for _, a := range part.Receipt.Attachments {
		info, e := p.MM.File(ctx, a.FileID)
		if e != nil && mattermost.Status(e) != 404 {
			return e
		}
		if e == nil && info.PostID != "" {
			// A live foreign post is a manifest conflict; a deleted delivery needs fresh bytes.
			linked := false
			for _, post := range p.Posts {
				if post.ID == info.PostID {
					r, ok := p.receipt(post)
					if !ok || r.RunID != part.Receipt.RunID {
						return ErrConflict
					}
					linked = true
				}
			}
			if !linked {
				existing, readErr := p.MM.Post(ctx, info.PostID)
				if readErr != nil && mattermost.Status(readErr) != 404 {
					return readErr
				}
				if readErr == nil && existing.DeleteAt == 0 {
					return ErrConflict
				}
			}
		}
		if e != nil || info.PostID != "" {
			return ErrUpload
		}
		ids = append(ids, a.FileID)
	}
	key := attachments.ObjectHash([]any{part.Receipt.Source, part.Receipt.Workflow, part.Receipt.SessionID, part.Receipt.RunID, part.Receipt.MessageID, part.Receipt.Part})
	created, e := p.MM.Create(ctx, mattermost.CreatePost{ChannelID: p.Key.Channel, RootID: p.Key.Root, Message: part.Text, FileIDs: ids, PendingID: key[:26], Props: map[string]any{Property: part.Receipt}})
	if e != nil {
		return e
	}
	p.Posts = append(p.Posts, created)
	actual, rec, exists, e := p.find(part)
	if e != nil {
		return e
	}
	if !exists {
		return errors.New("created post lacks valid receipt")
	}
	for _, a := range rec.Attachments {
		ok, e := p.verified(ctx, actual, a)
		if e != nil {
			return e
		}
		if !ok {
			return ErrUpload
		}
	}
	return nil
}

// Split V2 prefers text boundaries while preserving every Unicode code point.
func Split(text string, limit int) []string {
	if text == "" {
		return []string{""}
	}
	limit = max(1, limit)
	runes := []rune(text)
	var parts []string
	for len(runes) > 0 {
		n := min(limit, len(runes))
		if n < len(runes) {
			// Prefer paragraph, line, then word boundaries in the latter half.
			for _, kind := range []int{2, 1, 0} {
				cut := 0
				for i := n - 1; i >= n/2; i-- {
					if kind == 2 && i > 0 && runes[i] == '\n' && runes[i-1] == '\n' || kind == 1 && runes[i] == '\n' || kind == 0 && unicode.IsSpace(runes[i]) {
						cut = i + 1
						break
					}
				}
				if cut > 0 {
					n = cut
					break
				}
			}
		}
		parts = append(parts, string(runes[:n]))
		runes = runes[n:]
	}
	return parts
}
func Parts(key conversation.Key, sid, rid, mid, text string, render conversation.Render, files []attachments.Artifact) []Part {
	chunks := Split(text, render.MaxChars)
	if render.Version == 1 {
		// Accepted runs retain their original deterministic publication contract.
		chunks = nil
		runes := []rune(text)
		for len(runes) > 0 {
			n := min(max(1, render.MaxChars), len(runes))
			chunks = append(chunks, string(runes[:n]))
			runes = runes[n:]
		}
		if len(chunks) == 0 {
			chunks = []string{""}
		}
	}
	result := make([]Part, len(chunks))
	for i, t := range chunks {
		r := Receipt{Schema: 1, Source: key.Source, Workflow: key.Workflow, SessionID: sid, RunID: rid, MessageID: mid, Part: i, Parts: len(chunks), Hash: attachments.Hash([]byte(t)), RenderVersion: render.Version, Attachments: []attachments.Artifact{}}
		if i == 0 {
			r.Attachments = files
		}
		result[i] = Part{r, t}
	}
	return result
}
func Failure(r conversation.Run) string {
	if r.StopReason == "token_limit" {
		return "Достигнут лимит токенов. Для продолжения отправьте новое обращение."
	}
	if r.Status == "cancelled" {
		return "Выполнение отменено."
	}
	if r.Status == "failed" {
		return fmt.Sprintf("Выполнение завершилось с ошибкой (%s).", r.Error)
	}
	return ""
}
