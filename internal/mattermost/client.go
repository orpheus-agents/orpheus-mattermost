// Package mattermost implements the bounded Mattermost REST and WebSocket transport.
package mattermost

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type Post struct {
	ID        string                     `json:"id"`
	RootID    string                     `json:"root_id"`
	ChannelID string                     `json:"channel_id"`
	UserID    string                     `json:"user_id"`
	Message   string                     `json:"message"`
	Type      string                     `json:"type"`
	CreateAt  int64                      `json:"create_at"`
	EditAt    int64                      `json:"edit_at"`
	UpdateAt  int64                      `json:"update_at"`
	DeleteAt  int64                      `json:"delete_at"`
	FileIDs   []string                   `json:"file_ids"`
	Props     map[string]json.RawMessage `json:"props"`
	Metadata  json.RawMessage            `json:"metadata,omitempty"`
}

func (p Post) Root() string { return cmp.Or(p.RootID, p.ID) }

type Channel struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	DeleteAt    int64  `json:"delete_at"`
}
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	IsBot    bool   `json:"is_bot"`
	DeleteAt int64  `json:"delete_at"`
}
type FileInfo struct {
	ID     string `json:"id"`
	PostID string `json:"post_id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime_type"`
}
type CreatePost struct {
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id"`
	Message   string         `json:"message"`
	FileIDs   []string       `json:"file_ids,omitempty"`
	PendingID string         `json:"pending_post_id"`
	Props     map[string]any `json:"props"`
}
type PostList struct {
	Order   []string        `json:"order"`
	Posts   map[string]Post `json:"posts"`
	HasNext *bool           `json:"has_next,omitempty"`
}
type HTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("mattermost HTTP %d", e.Status) }
func Status(err error) int {
	if e, ok := errors.AsType[*HTTPError](err); ok {
		return e.Status
	}
	return 0
}

type Client struct {
	BaseURL string
	token   string
	HTTP    *http.Client
}

func New(base, token string, timeout time.Duration) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), token: token, HTTP: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v4"+path, body)
	if err != nil {
		return nil, errors.New("invalid Mattermost request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mattermost transport: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer func() { _ = res.Body.Close() }()
		seconds, err := strconv.Atoi(res.Header.Get("Retry-After"))
		delay := time.Duration(max(0, seconds)) * time.Second
		if err != nil {
			if date, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil {
				delay = max(0, time.Until(date))
			}
		}
		return nil, &HTTPError{res.StatusCode, min(delay, 24*time.Hour)}
	}
	return res, nil
}
func (c *Client) json(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	res, err := c.request(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 64<<20 {
		return errors.New("mattermost JSON exceeds limit")
	}
	if err = json.Unmarshal(raw, output); err != nil {
		return errors.New("invalid Mattermost JSON response")
	}
	return nil
}
func (c *Client) Me(ctx context.Context) (User, error) { return c.User(ctx, "me") }
func (c *Client) User(ctx context.Context, id string) (User, error) {
	var v User
	err := c.json(ctx, "GET", "/users/"+url.PathEscape(id), nil, &v)
	return v, err
}

// The all-teams membership endpoint is intentionally unpaginated in API v4.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var v []Channel
	err := c.json(ctx, "GET", "/users/me/channels", nil, &v)
	return v, err
}
func (c *Client) Channel(ctx context.Context, id string) (Channel, error) {
	var v Channel
	err := c.json(ctx, "GET", "/channels/"+url.PathEscape(id), nil, &v)
	return v, err
}
func (c *Client) Post(ctx context.Context, id string) (Post, error) {
	var v Post
	err := c.json(ctx, "GET", "/posts/"+url.PathEscape(id), nil, &v)
	return v, err
}
func (c *Client) Files(ctx context.Context, post string) ([]FileInfo, error) {
	var v []FileInfo
	err := c.json(ctx, "GET", "/posts/"+url.PathEscape(post)+"/files/info", nil, &v)
	return v, err
}
func (c *Client) File(ctx context.Context, id string) (FileInfo, error) {
	var v FileInfo
	err := c.json(ctx, "GET", "/files/"+url.PathEscape(id)+"/info", nil, &v)
	return v, err
}
func (c *Client) Create(ctx context.Context, post CreatePost) (Post, error) {
	var v Post
	err := c.json(ctx, "POST", "/posts", post, &v)
	return v, err
}
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.json(ctx, "DELETE", "/posts/"+url.PathEscape(id), nil, nil)
}
func (c *Client) MaxPostChars(ctx context.Context) (int, error) {
	var v map[string]string
	err := c.json(ctx, "GET", "/config/client?format=old", nil, &v)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v["MaxPostSize"])
	if err != nil || n < 1 {
		return 0, errors.New("mattermost did not advertise MaxPostSize")
	}
	return n, nil
}
func (c *Client) Typing(ctx context.Context, channel, root string) error {
	return c.json(ctx, "POST", "/users/me/typing", map[string]string{"channel_id": channel, "parent_id": root}, nil)
}
func Sort(posts []Post) {
	slices.SortFunc(posts, func(a, b Post) int {
		if n := cmp.Compare(a.CreateAt, b.CreateAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
}
func values(page PostList) []Post {
	v := make([]Post, 0, len(page.Posts))
	for _, p := range page.Posts {
		v = append(v, p)
	}
	Sort(v)
	return v
}

// Posts walks all pages from the fixed replay boundary. Mattermost's before=
// filter compares only timestamps and permanently skips ties at page boundaries.
// Offset pages can shift during concurrent writes; the next complete replay
// covers those shifts, while per-post admission keys suppress duplicates.
func (c *Client) Posts(ctx context.Context, channel string, since time.Time) ([]Post, error) {
	seen := map[string]bool{}
	var result []Post
	for number := 0; ; number++ {
		q := url.Values{"per_page": {"200"}, "page": {strconv.Itoa(number)}, "skipFetchThreads": {"true"}}
		var page PostList
		if err := c.json(ctx, "GET", "/channels/"+url.PathEscape(channel)+"/posts?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		added := 0
		stop := false
		for _, id := range page.Order {
			p, ok := page.Posts[id]
			if !ok || p.ChannelID != channel {
				return nil, errors.New("incomplete channel page")
			}
			if p.CreateAt < since.UnixMilli() {
				stop = true
				continue
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				result = append(result, p)
				added++
			}
		}
		if stop || len(page.Order) < 200 {
			break
		}
		if added == 0 {
			return nil, errors.New("mattermost channel pagination made no progress")
		}
	}
	Sort(result)
	return result, nil
}
func (c *Client) Thread(ctx context.Context, id string) ([]Post, error) {
	target, err := c.Post(ctx, id)
	if err != nil {
		return nil, err
	}
	root := target.Root()
	seen := map[string]bool{}
	var result []Post
	cursor := root
	rootPost, err := c.Post(ctx, root)
	if err != nil {
		return nil, err
	}
	cursorTime := rootPost.CreateAt
	for {
		q := url.Values{"perPage": {"200"}, "direction": {"down"}, "fromPost": {cursor}, "fromCreateAt": {strconv.FormatInt(cursorTime, 10)}}
		var page PostList
		if err := c.json(ctx, "GET", "/posts/"+url.PathEscape(root)+"/thread?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		posts := values(page)
		newReplies := 0
		next := cursor
		for _, p := range posts {
			if p.Root() != root || p.ChannelID != target.ChannelID {
				return nil, errors.New("thread page mismatch")
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				result = append(result, p)
				if p.ID != root {
					newReplies++
				}
			}
			if p.ID != root {
				next = p.ID
				cursorTime = p.CreateAt
			}
		}
		if page.HasNext != nil && !*page.HasNext {
			break
		}
		if len(page.Order) < 200 && page.HasNext == nil {
			break
		}
		if newReplies == 0 || next == cursor {
			return nil, errors.New("mattermost repeated thread cursor")
		}
		cursor = next
	}
	if !seen[root] {
		p, err := c.Post(ctx, root)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if !seen[target.ID] && target.ID != root {
		return nil, errors.New("mattermost thread omitted target post")
	}
	Sort(result)
	return result, nil
}
func (c *Client) Download(ctx context.Context, id string, limit int64) ([]byte, error) {
	res, err := c.request(ctx, "GET", "/files/"+url.PathEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file exceeds byte limit")
	}
	return b, nil
}
func (c *Client) Upload(ctx context.Context, channel, name string, content io.Reader) (FileInfo, error) {
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		err := multipartWriter.WriteField("channel_id", channel)
		if err == nil {
			var part io.Writer
			part, err = multipartWriter.CreateFormFile("files", name)
			if err == nil {
				_, err = io.Copy(part, content)
			}
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	res, err := c.request(ctx, "POST", "/files", reader, multipartWriter.FormDataContentType())
	_ = reader.Close()
	writeErr := <-done
	if err != nil {
		return FileInfo{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if writeErr != nil {
		return FileInfo{}, writeErr
	}
	var payload struct {
		Files []FileInfo `json:"file_infos"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&payload); err != nil {
		return FileInfo{}, errors.New("invalid upload response")
	}
	if len(payload.Files) != 1 || payload.Files[0].ID == "" {
		return FileInfo{}, errors.New("upload did not return exactly one file")
	}
	return payload.Files[0], nil
}

// Watch authenticates without putting the token in URLs or loggable errors.
// Event is a thread hint; a zero Post requests channel membership reconciliation.
type Event struct {
	Post    Post
	Deleted bool
}

func (c *Client) Watch(ctx context.Context, notify func(Event)) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v4/websocket"
	conn, res, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.HTTP})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		return errors.New("mattermost WebSocket connection failed")
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(4 << 20)
	challenge, _ := json.Marshal(map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": c.token}})
	if err = conn.Write(ctx, websocket.MessageText, challenge); err != nil {
		return errors.New("mattermost WebSocket authentication failed")
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return errors.New("mattermost WebSocket disconnected")
		}
		var event struct {
			Event  string `json:"event"`
			Status string `json:"status"`
			Data   struct {
				Post      string `json:"post"`
				ChannelID string `json:"channel_id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		if event.Status == "FAIL" {
			return errors.New("mattermost WebSocket authentication rejected")
		}
		switch event.Event {
		case "posted", "post_edited", "post_deleted":
			var p Post
			if json.Unmarshal([]byte(event.Data.Post), &p) == nil {
				notify(Event{Post: p, Deleted: event.Event == "post_deleted"})
			}
		case "user_added", "user_removed", "channel_deleted", "channel_updated":
			notify(Event{})
		}
	}
}
