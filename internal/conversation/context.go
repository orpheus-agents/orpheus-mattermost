package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Mention parses Markdown first; code and escaped @ cannot become triggers.
func Mention(body string, names []string) bool {
	source := []byte(body)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	found := false
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindCodeSpan, ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindAutoLink, ast.KindHTMLBlock, ast.KindRawHTML:
			return ast.WalkSkipChildren, nil
		}
		node, ok := n.(*ast.Text)
		if !ok {
			return ast.WalkContinue, nil
		}
		s := strings.ToLower(string(node.Segment.Value(source)))
		for _, name := range names {
			needle := "@" + strings.ToLower(name)
			offset := 0
			for {
				at := strings.Index(s[offset:], needle)
				if at < 0 {
					break
				}
				at += offset
				end := at + len(needle)
				valid := true
				if at > 0 {
					prev, _ := utf8.DecodeLastRuneInString(s[:at])
					valid = !unicode.IsLetter(prev) && !unicode.IsDigit(prev) && prev != '_' && prev != '@' && prev != '.'
					slashes := 0
					for i := at - 1; i >= 0 && s[i] == '\\'; i-- {
						slashes++
					}
					valid = valid && slashes%2 == 0
				}
				if end < len(s) {
					next, _ := utf8.DecodeRuneInString(s[end:])
					valid = valid && !unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_' && next != '-'
				}
				if valid {
					found = true
					return ast.WalkStop, nil
				}
				offset = end
				if offset >= len(s) {
					break
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return found
}

type ContextSource interface {
	Post(context.Context, string) (mattermost.Post, error)
	Thread(context.Context, string) ([]mattermost.Post, error)
	User(context.Context, string) (mattermost.User, error)
	Files(context.Context, string) ([]mattermost.FileInfo, error)
}
type Builder struct {
	Source   ContextSource
	Config   config.Config
	Workflow config.Workflow
	Bot      mattermost.User
}

type fileReference struct {
	FileID string `json:"file_id"`
	Name   string `json:"name,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Size   int64  `json:"size_bytes,omitempty"`
	Path   string `json:"path,omitempty"`
	Status string `json:"status,omitempty"`
}

func (b Builder) Trigger(p mattermost.Post, channel mattermost.Channel, existing bool, user mattermost.User) bool {
	if p.DeleteAt != 0 || p.UserID == b.Bot.ID || strings.HasPrefix(p.Type, "system_") || p.Type == "system_ephemeral" {
		return false
	}
	if (user.IsBot || p.Props["from_webhook"] != nil) && !slices.Contains(b.Workflow.TriggerBotIDs, p.UserID) {
		return false
	}
	if strings.TrimSpace(p.Message) == "" && len(p.FileIDs) == 0 {
		return false
	}
	if channel.Type == "D" {
		return true
	}
	if !existing && !*b.Workflow.StartOnMention {
		return true
	}
	return Mention(p.Message, []string{b.Bot.Username})
}

var linkPattern = regexp.MustCompile(`https?://[^\s<>\[\]"']+`)

func Permalinks(body, base string) []string {
	expected, err := url.Parse(base)
	if err != nil {
		return nil
	}
	var ids []string
	for _, raw := range linkPattern.FindAllString(body, -1) {
		raw = strings.TrimRight(raw, ").,;!")
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || !strings.EqualFold(u.Host, expected.Host) || u.Scheme != expected.Scheme {
			continue
		}
		prefix := strings.TrimRight(expected.Path, "/") + "/"
		p, ok := strings.CutPrefix(u.Path, prefix)
		if !ok {
			continue
		}
		parts := strings.Split(p, "/")
		var id string
		if len(parts) == 2 && parts[0] == "pl" {
			id = parts[1]
		} else if len(parts) == 3 && parts[1] == "pl" {
			id = parts[2]
		}
		if config.ValidID(id) && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}
func postCost(p mattermost.Post) int {
	props, _ := json.Marshal(p.Props)
	return len(p.Message) + len(props) + len(p.Metadata) + 256 + len(p.FileIDs)*512
}
func initialContext(posts []mattermost.Post, mandatory map[string]bool, root string, budget int) []mattermost.Post {
	selected := map[string]bool{}
	used := 0
	add := func(p mattermost.Post, required bool) {
		cost := postCost(p)
		if !required && used+cost > budget {
			return
		}
		if selected[p.ID] {
			return
		}
		selected[p.ID] = true
		used += cost
	}
	for _, p := range posts {
		if mandatory[p.ID] || p.ID == root {
			add(p, mandatory[p.ID])
		}
	}
	for i, p := range posts {
		if mandatory[p.ID] {
			for j := max(0, i-3); j < min(len(posts), i+4); j++ {
				add(posts[j], false)
			}
		}
	}
	for _, post := range slices.Backward(posts) {
		add(post, false)
	}
	var result []mattermost.Post
	for _, p := range posts {
		if selected[p.ID] {
			result = append(result, p)
		}
	}
	return result
}

// Build selects a deterministic prefix of triggers; none is consumed without its text.
func (b Builder) Build(ctx context.Context, key Key, channel mattermost.Channel, posts []mattermost.Post, snapshot Snapshot, initial bool, now time.Time) (Envelope, string, error) {
	known, contextIDs := snapshot.Known()
	knownVersions := map[string]Version{}
	for _, session := range snapshot.Sessions {
		for _, message := range session.Messages {
			if message.Role != "user" || message.Delivery != "delivered" {
				continue
			}
			env, err := Decode(message.Text)
			if err != nil {
				continue
			}
			for _, version := range env.Versions {
				knownVersions[version.ID] = version
			}
		}
	}
	existing := len(snapshot.Sessions) > 0
	users := map[string]mattermost.User{}
	var candidates []mattermost.Post
	for _, p := range posts {
		if p.DeleteAt != 0 {
			continue
		}
		if p.UserID == b.Bot.ID || strings.HasPrefix(p.Type, "system_") || p.CreateAt < b.Workflow.Since.UnixMilli() || known[p.ID] {
			continue
		}
		u, ok := users[p.UserID]
		if !ok {
			var err error
			u, err = b.Source.User(ctx, p.UserID)
			if err != nil {
				return Envelope{}, "", err
			}
			users[p.UserID] = u
		}
		if b.Trigger(p, channel, existing, u) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return Envelope{}, "", nil
	}
	anchor := candidates[0]
	end := anchor.CreateAt + b.Workflow.MessageBatchWindow.Value().Milliseconds()
	if now.UnixMilli() < end {
		return Envelope{}, "", nil
	}
	var triggers []mattermost.Post
	for _, p := range candidates {
		if p.CreateAt <= end {
			triggers = append(triggers, p)
		}
	}
	fileCache := map[string][]mattermost.FileInfo{}
	filesFor := func(id string) ([]mattermost.FileInfo, error) {
		if files, ok := fileCache[id]; ok {
			return files, nil
		}
		files, err := b.Source.Files(ctx, id)
		if err == nil {
			fileCache[id] = files
		}
		return files, err
	}
	for count := len(triggers); count > 0; count-- {
		budget := min(b.Workflow.InitialContextTokens*3, b.Config.MaxRequestBytes/3)
		for {
			cutoff := end
			if count < len(triggers) {
				cutoff = triggers[count].CreateAt /* ID tie-break below prevents swallowing the split trigger. */
			}
			mandatory := map[string]bool{}
			for _, p := range triggers[:count] {
				mandatory[p.ID] = true
			}
			var eligible []mattermost.Post
			for _, p := range posts {
				if p.DeleteAt != 0 || p.CreateAt > cutoff || count < len(triggers) && (p.CreateAt == cutoff && p.ID >= triggers[count].ID) {
					continue
				}
				if !initial && p.UserID == b.Bot.ID {
					continue
				}
				if initial || !contextIDs[p.ID] || mandatory[p.ID] {
					eligible = append(eligible, p)
				}
			}
			chosen := initialContext(eligible, mandatory, key.Root, budget)
			e := Envelope{Schema: 1, Source: key.Source, Workflow: key.Workflow, Revision: b.Workflow.EffectiveRevision, Channel: key.Channel, Root: key.Root, Anchor: anchor.ID, WindowEnd: cutoff, Kind: "initial", Render: Render{2, b.Workflow.MaxPostChars, *b.Workflow.SendCommentary}}
			for _, p := range triggers[:count] {
				e.TriggerIDs = append(e.TriggerIDs, p.ID)
			}
			e.Request = attachments.Request{Schema: 1, BotID: b.Bot.ID, SourceID: key.Source, ChannelID: key.Channel, RootID: key.Root, AnchorID: anchor.ID, Limits: b.Workflow.Files, AllowedPairs: b.Workflow.Links.AllowedChannelPairs, Files: []attachments.InputFile{}}
			seen := map[string]bool{}
			fileSeen := map[string]bool{}
			var out strings.Builder
			write := func(p mattermost.Post, origin string) error {
				if seen[p.ID] {
					return nil
				}
				seen[p.ID] = true
				u, ok := users[p.UserID]
				if !ok {
					var err error
					u, err = b.Source.User(ctx, p.UserID)
					if err != nil {
						return err
					}
					users[p.UserID] = u
				}
				message := p.Message
				if origin == "thread_reference" {
					message = ""
				}
				var postFiles []fileReference
				if len(p.FileIDs) > 0 {
					files, err := filesFor(p.ID)
					if err != nil {
						for _, id := range p.FileIDs {
							postFiles = append(postFiles, fileReference{FileID: id, Status: "source_access_failed"})
						}
					} else {
						found := map[string]bool{}
						for i, f := range files {
							if !slices.Contains(p.FileIDs, f.ID) {
								return fmt.Errorf("attachment not present on source post")
							}
							found[f.ID] = true
							if fileSeen[f.ID] {
								continue
							}
							fileSeen[f.ID] = true
							input := attachments.InputFile{PostID: p.ID, FileID: f.ID, Origin: origin, Name: f.Name, MIME: f.MIME, Size: f.Size, Path: attachments.InputPath(f.ID, f.Name), ChannelID: p.ChannelID}
							if i >= b.Workflow.Files.MaxPerPost {
								input.Status = "file_limit_exceeded"
							}
							ref := fileReference{FileID: f.ID, Name: f.Name, MIME: f.MIME, Size: f.Size, Status: input.Status}
							if input.Status == "" {
								ref.Path = input.Path
							}
							postFiles = append(postFiles, ref)
							e.Request.Files = append(e.Request.Files, input)
						}
						for _, id := range p.FileIDs {
							if !found[id] {
								postFiles = append(postFiles, fileReference{FileID: id, Status: "source_access_failed"})
							}
						}
					}
				}
				author, _ := json.Marshal(map[string]any{"id": p.UserID, "username": u.Username, "nickname": u.Nickname})
				fmt.Fprintf(&out, "---\nkind: %s\npost_id: %s\nroot_id: %s\nchannel_id: %s\nauthor: %s\ncreated_at: %s\n", origin, p.ID, p.Root(), p.ChannelID, author, time.UnixMilli(p.CreateAt).UTC().Format(time.RFC3339Nano))
				if raw := p.Props["attachments"]; len(raw) > 0 && origin != "thread_reference" {
					fmt.Fprintf(&out, "structured_data: %s\n", mustJSON(map[string]json.RawMessage{"attachments": raw}))
				}
				if len(postFiles) > 0 {
					fmt.Fprintf(&out, "attachments: %s\n", mustJSON(postFiles))
				}
				fmt.Fprintf(&out, "---\n%s\n", message)
				e.Versions = append(e.Versions, PostVersion(p))
				if (origin == "thread" || origin == "thread_reference") && !mandatory[p.ID] {
					e.ContextIDs = append(e.ContextIDs, p.ID)
				}
				out.WriteByte('\n')
				return nil
			}
			for _, p := range chosen {
				if err := write(p, "thread"); err != nil {
					return Envelope{}, "", err
				}
			}
			if len(chosen) < len(eligible) {
				fmt.Fprintf(&out, "[Context omitted: %d posts, %s — %s]\n", len(eligible)-len(chosen), time.UnixMilli(eligible[0].CreateAt).UTC().Format(time.RFC3339), time.UnixMilli(eligible[len(eligible)-1].CreateAt).UTC().Format(time.RFC3339))
			}
			if *b.Workflow.Links.Enabled {
				links := []string{}
				for _, p := range chosen {
					body := p.Message
					if len(p.Metadata) > 0 {
						body += "\n" + string(p.Metadata)
					}
					for _, id := range Permalinks(body, b.Workflow.Mattermost.BaseURL) {
						if !slices.Contains(links, id) {
							links = append(links, id)
						}
					}
				}
				for _, id := range links[:min(len(links), b.Workflow.Links.MaxLinks)] {
					p, err := b.Source.Post(ctx, id)
					if err != nil || p.DeleteAt != 0 {
						fmt.Fprintf(&out, "[Linked post %s: unavailable]\n", id)
						continue
					}
					if !b.Workflow.AllowsLink(p.ChannelID, key.Channel) {
						fmt.Fprintf(&out, "[Linked post %s: source_not_allowed]\n", id)
						continue
					}
					if p.ChannelID == key.Channel && p.Root() == key.Root {
						if !seen[p.ID] {
							origin := "thread"
							if v, ok := knownVersions[p.ID]; !initial && ok && v == PostVersion(p) {
								origin = "thread_reference"
							}
							if origin != "thread_reference" && postCost(p) > min(budget, b.Config.MaxRequestBytes/2)-out.Len() {
								fmt.Fprintf(&out, "[Linked post %s: context_limit_exceeded]\n", p.ID)
								continue
							}
							if err := write(p, origin); err != nil {
								return Envelope{}, "", err
							}
						}
						continue
					}
					if postCost(p) > min(budget, b.Config.MaxRequestBytes/2)-out.Len() {
						fmt.Fprintf(&out, "[Linked post %s: context_limit_exceeded]\n", p.ID)
						continue
					}
					linked, err := b.Source.Thread(ctx, p.ID)
					if err != nil {
						fmt.Fprintf(&out, "[Linked post %s: unavailable]\n", id)
						continue
					}
					selection := linkedSelection(linked, p.ID, p.Root(), b.Workflow.Links.MaxPosts)
					remaining := min(budget, b.Config.MaxRequestBytes/2) - out.Len() - postCost(p)
					// Reserve room for the exact linked target before optional neighboring posts.
					for _, lp := range selection {
						if lp.ID != p.ID {
							if postCost(lp) > remaining {
								fmt.Fprintf(&out, "[Linked post %s: context_limit_exceeded]\n", lp.ID)
								continue
							}
							remaining -= postCost(lp)
						}
						if lp.DeleteAt == 0 {
							if err := write(lp, "linked_thread"); err != nil {
								return Envelope{}, "", err
							}
						}
					}
				}
			}
			body := out.String()
			encoded, err := e.Encode(body)
			if err != nil {
				return Envelope{}, "", err
			}
			request, _ := json.Marshal(e.Request)
			if len(encoded) < b.Config.MaxRequestBytes*3/4 && len(request) < 60<<10 {
				return e, encoded, nil
			}
			if budget > 0 {
				budget /= 2
				continue
			}
			if count == 1 {
				return e, encoded, fmt.Errorf("input_too_large")
			}
			break
		}
	}
	return Envelope{}, "", nil
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func linkedSelection(posts []mattermost.Post, target, root string, limit int) []mattermost.Post {
	indices := []int{}
	selected := map[int]bool{}
	add := func(i int) {
		if i >= 0 && i < len(posts) && !selected[i] && len(indices) < limit {
			selected[i] = true
			indices = append(indices, i)
		}
	}
	at := 0
	for i, p := range posts {
		if p.ID == target {
			at = i
			add(i)
		}
	}
	for i, p := range posts {
		if p.ID == root {
			add(i)
		}
	}
	for d := 1; len(indices) < min(limit, len(posts)); d++ {
		add(at - d)
		add(at + d)
	}
	slices.Sort(indices)
	result := make([]mattermost.Post, 0, len(indices))
	for _, i := range indices {
		result = append(result, posts[i])
	}
	return result
}

// PostVersion excludes mutable transport metadata, reactions and reply timestamps.
func PostVersion(p mattermost.Post) Version {
	content := struct {
		Message     string
		Files       []string
		Attachments json.RawMessage
	}{p.Message, p.FileIDs, p.Props["attachments"]}
	return Version{p.ID, p.EditAt, attachments.ObjectHash(content)}
}
