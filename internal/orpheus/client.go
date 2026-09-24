// Package orpheus adapts the versioned public API to connector domain types.
package orpheus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/sandbox"
	api "github.com/orpheus-agents/orpheus/client"
)

type Client struct {
	api     *api.Client
	stream  *api.Client
	Config  config.Config
	mu      sync.Mutex
	cursors map[string]string
}

var errorCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string { return fmt.Sprintf("Orpheus HTTP %d: %s", e.Status, e.Code) }
func Code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
func New(c config.Config, token string) (*Client, error) {
	editor := api.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
	h := &http.Client{Timeout: c.HTTPTimeout.Value(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a, err := api.NewClient(c.Orpheus.BaseURL, editor, api.WithHTTPClient(h))
	if err != nil {
		return nil, err
	}
	stream, err := api.NewClient(c.Orpheus.BaseURL, editor, api.WithHTTPClient(&http.Client{CheckRedirect: h.CheckRedirect}))
	return &Client{api: a, stream: stream, Config: c, cursors: map[string]string{}}, err
}
func decode[T any](res *http.Response, err error) (T, error) {
	var v T
	if err != nil {
		return v, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, (64<<20)+1))
	if err != nil {
		return v, err
	}
	if len(b) > 64<<20 {
		return v, errors.New("orpheus response exceeds limit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var p api.ErrorResponse
		_ = json.Unmarshal(b, &p)
		code := p.Error.Code
		if !errorCodePattern.MatchString(code) {
			code = "invalid_error"
		}
		return v, &Error{res.StatusCode, code}
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, errors.New("invalid Orpheus JSON response")
	}
	return v, nil
}
func ptr[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}
func (c *Client) Sessions(ctx context.Context, namespace, external string) ([]conversation.Session, error) {
	params := &api.ListSessionsParams{Namespace: &namespace, Order: new(api.ListSessionsParamsOrderAsc), Limit: new(200)}
	if external != "" {
		params.ExternalKey = &external
	}
	var result []conversation.Session
	seen := map[string]bool{}
	for {
		p, err := decode[api.SessionPage](c.api.ListSessions(ctx, params)) //nolint:bodyclose // decode owns and closes the response body.
		if err != nil {
			return nil, err
		}
		for _, s := range p.Items {
			if ptr(s.Namespace) != namespace {
				return nil, errors.New("orpheus namespace mismatch")
			}
			result = append(result, conversation.Session{ID: s.ID.String(), ExternalKey: ptr(s.ExternalKey), CreatedAt: s.CreatedAt, SandboxID: ptr(s.Sandbox.ID), SandboxState: string(s.Sandbox.State), SandboxLastState: ptr(s.Sandbox.LastKnownState), Workspace: ptr(s.Sandbox.Workspace), TotalTokens: s.Usage.TotalTokens, MaxTokens: s.Configuration.Limits.MaxSessionTokens})
		}
		if p.NextCursor == nil {
			break
		}
		if seen[*p.NextCursor] {
			return nil, errors.New("repeated session cursor")
		}
		seen[*p.NextCursor] = true
		params.Cursor = p.NextCursor
	}
	return result, nil
}
func (c *Client) Runs(ctx context.Context, id string) ([]conversation.Run, error) {
	sid, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	params := &api.ListRunsParams{Order: new(api.ListRunsParamsOrderAsc), Limit: new(200)}
	var result []conversation.Run
	seen := map[string]bool{}
	for {
		p, err := decode[api.RunPage](c.api.ListRuns(ctx, sid, params)) //nolint:bodyclose // decode owns and closes the response body.
		if err != nil {
			return nil, err
		}
		for _, r := range p.Items {
			result = append(result, run(r))
		}
		if p.NextCursor == nil {
			break
		}
		if seen[*p.NextCursor] {
			return nil, errors.New("repeated run cursor")
		}
		seen[*p.NextCursor] = true
		params.Cursor = p.NextCursor
	}
	return result, nil
}
func run(r api.Run) conversation.Run {
	v := conversation.Run{ID: r.ID.String(), SessionID: r.SessionID.String(), Number: r.Number, Status: string(r.Status), Observation: ptr(r.Observation), AgentStatus: ptr(r.AgentStatus), StopReason: ptr(r.StopReason), CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, DeadlineAt: r.DeadlineAt}
	if r.Error != nil {
		v.Error = r.Error.Code
	}
	if r.FinalMessage != nil {
		v.FinalMessageID = r.FinalMessage.ID.String()
	}
	for _, h := range r.Hooks {
		item := conversation.Hook{Name: string(h.Name), Status: string(h.Status), Completeness: string(h.OutputCompleteness)}
		if h.Output != nil {
			data, err := h.Output.AsTextResult()
			if err == nil {
				item.Output = data.Text
			}
		}
		v.Hooks = append(v.Hooks, item)
	}
	return v
}
func (c *Client) Run(ctx context.Context, sid, rid string) (conversation.Run, error) {
	s, e := uuid.Parse(sid)
	if e != nil {
		return conversation.Run{}, e
	}
	r, e := uuid.Parse(rid)
	if e != nil {
		return conversation.Run{}, e
	}
	v, e := decode[api.Run](c.api.GetRun(ctx, s, r)) //nolint:bodyclose // decode owns and closes the response body.
	return run(v), e
}
func (c *Client) History(ctx context.Context, id string) ([]conversation.Message, error) {
	sid, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	params := &api.GetHistoryParams{Limit: new(200)}
	cursor := ""
	var result []conversation.Message
	seen := map[string]bool{}
	for {
		p, err := decode[api.HistoryPage](c.api.GetHistory(ctx, sid, params)) //nolint:bodyclose // decode owns and closes the response body.
		if err != nil {
			return nil, err
		}
		if cursor == "" {
			cursor = p.EventCursor
		}
		for _, item := range p.Items {
			kind, err := item.Discriminator()
			if err != nil {
				return nil, err
			}
			if kind != "message" {
				continue
			}
			m, err := item.AsMessageItem()
			if err != nil {
				return nil, err
			}
			v := m.Message
			out := conversation.Message{ID: v.ID.String(), RunID: v.RunID.String(), Role: string(v.Role), Kind: ptr(v.Kind), Text: v.Text, ExternalKey: ptr(v.ExternalKey), Delivery: ptr(v.DeliveryStatus), Position: -1, CreatedAt: v.CreatedAt}
			if v.Position != nil {
				out.Position = v.Position.ItemIndex
			}
			if v.Error != nil {
				out.Error = v.Error.Code
			}
			result = append(result, out)
		}
		if p.NextCursor == nil {
			break
		}
		if seen[*p.NextCursor] {
			return nil, errors.New("repeated history cursor")
		}
		seen[*p.NextCursor] = true
		params.Cursor = p.NextCursor
	}
	// Seed only an unstarted subscription, after all history pages were read.
	// The first page's watermark cannot skip events concurrent with pagination.
	c.mu.Lock()
	if c.cursors[id] == "" && cursor != "" {
		c.cursors[id] = cursor
	}
	c.mu.Unlock()
	return result, nil
}
func (c *Client) Snapshot(ctx context.Context, key conversation.Key) (conversation.Snapshot, error) {
	sessions, err := c.Sessions(ctx, key.Namespace(), key.External())
	if err != nil {
		return conversation.Snapshot{}, err
	}
	for i := range sessions {
		s := &sessions[i]
		s.Runs, err = c.Runs(ctx, s.ID)
		if err != nil {
			return conversation.Snapshot{}, err
		}
		s.Messages, err = c.History(ctx, s.ID)
		if err != nil {
			return conversation.Snapshot{}, err
		}
		for _, m := range s.Messages {
			if m.Role == "user" {
				e, err := conversation.Decode(m.Text)
				if err != nil || e.Key() != key || m.ExternalKey != key.MessageKey(e.Anchor) {
					return conversation.Snapshot{}, errors.New("foreign input in owned Orpheus session")
				}
				if s.Revision == "" {
					s.Revision = e.Revision
				}
			}
		}
	}
	slices.SortFunc(sessions, func(a, b conversation.Session) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return conversation.Snapshot{Sessions: sessions}, nil
}

func (c *Client) env(w config.Workflow, e conversation.Envelope) (map[string]string, error) {
	b, err := json.Marshal(e.Request)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"MM_INPUT_MANIFEST": string(b), "MM_BASE_URL": w.Mattermost.BaseURL, "MM_CHANNEL_ID": e.Channel, "MM_ROOT_POST_ID": e.Root, "MM_SOURCE_ID": e.Source, "MM_EXPECTED_BOT_ID": e.Request.BotID, "MM_TOKEN_ENV": w.Mattermost.TokenEnv}
	size := 0
	for k, v := range env {
		size += len(k) + len(v) + 2
	}
	if size > 64<<10 {
		return nil, &Error{413, "environment_too_large"}
	}
	return env, nil
}
func accepted(a api.Accepted) conversation.Accepted {
	return conversation.Accepted{SessionID: a.SessionID.String(), RunID: a.RunID.String(), MessageID: a.MessageID.String()}
}
func (c *Client) Submit(ctx context.Context, w config.Workflow, e conversation.Envelope, text, sessionID, runID, predecessor string) (conversation.Accepted, error) {
	message := api.TextMessage{Text: text, ExternalKey: new(e.Key().MessageKey(e.Anchor))}
	env, err := c.env(w, e)
	if err != nil {
		return conversation.Accepted{}, err
	}
	envFrom := []string{w.Mattermost.TokenEnv}
	var rawBody any
	var key string
	switch {
	case runID != "":
		rawBody = api.SendMessage{Message: message}
		key = e.Key().Operation(e.Anchor, "message", runID, predecessor)
	case sessionID != "":
		rawBody = api.CreateRun{Message: message, Env: &env, EnvFrom: &envFrom, InputFingerprint: new("mm-v1:" + attachments.Hash([]byte(text)))}
		key = e.Key().Operation(e.Anchor, "run", sessionID, predecessor)
	default:
		conf := api.ConfigurationInput{Agent: api.AgentInput{Profile: w.Profile, Instructions: new(w.Instructions)}, Sandbox: api.SandboxInput{Template: w.SandboxTemplate}, Limits: &api.LimitsInput{RunTimeoutSeconds: &w.RunTimeoutSeconds, MaxSessionTokens: &w.MaxSessionTokens}, Hooks: &api.HooksInput{BeforeRun: new(sandbox.BeforeRun()), AfterRun: new(sandbox.AfterRun()), TimeoutSeconds: &w.HookTimeoutSeconds}}
		if len(w.EnvFrom) > 0 {
			conf.Sandbox.EnvFrom = &w.EnvFrom
		}
		rawBody = api.CreateSession{Namespace: new(e.Key().Namespace()), ExternalKey: new(e.Key().External()), Configuration: conf, Message: message, Env: &env, EnvFrom: &envFrom, InputFingerprint: new("mm-v1:" + attachments.Hash([]byte(text)))}
		key = e.Key().Operation(e.Anchor, "session", e.Key().External(), predecessor)
	}
	b, err := json.Marshal(rawBody)
	if err != nil {
		return conversation.Accepted{}, err
	}
	if len(b) > c.Config.MaxRequestBytes {
		return conversation.Accepted{}, &Error{413, "request_too_large"}
	}
	var result api.Accepted
	switch body := rawBody.(type) {
	case api.CreateSession:
		result, err = decode[api.Accepted](c.api.CreateSession(ctx, &api.CreateSessionParams{IdempotencyKey: &key}, body)) //nolint:bodyclose // decode owns and closes the response body.
	case api.CreateRun:
		sid, e := uuid.Parse(sessionID)
		if e != nil {
			return conversation.Accepted{}, e
		}
		result, err = decode[api.Accepted](c.api.CreateRun(ctx, sid, &api.CreateRunParams{IdempotencyKey: &key}, body)) //nolint:bodyclose // decode owns and closes the response body.
	case api.SendMessage:
		sid, e := uuid.Parse(sessionID)
		if e != nil {
			return conversation.Accepted{}, e
		}
		rid, e := uuid.Parse(runID)
		if e != nil {
			return conversation.Accepted{}, e
		}
		result, err = decode[api.Accepted](c.api.SendMessage(ctx, sid, rid, &api.SendMessageParams{IdempotencyKey: &key}, body)) //nolint:bodyclose // decode owns and closes the response body.
	}
	return accepted(result), err
}
func (c *Client) Cancel(ctx context.Context, sid, rid string) error {
	s, err := uuid.Parse(sid)
	if err != nil {
		return err
	}
	r, err := uuid.Parse(rid)
	if err != nil {
		return err
	}
	_, err = decode[api.Cancelled](c.api.CancelRun(ctx, s, r)) //nolint:bodyclose // decode owns and closes the response body.
	return err
}
func (c *Client) Watch(ctx context.Context, sid string, notify func()) error {
	s, err := uuid.Parse(sid)
	if err != nil {
		return err
	}
	c.mu.Lock()
	after := c.cursors[sid]
	c.mu.Unlock()
	if after == "" {
		after = "0"
	}
	res, err := c.stream.StreamEvents(ctx, s, &api.StreamEventsParams{After: &after})
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return errors.New("orpheus SSE unavailable")
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	id := ""
	data := false
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "id:"); ok {
			id = strings.TrimSpace(value)
		}
		if strings.HasPrefix(line, "data:") {
			data = true
		}
		if line == "" && data {
			if id != "" && len(id) <= 256 {
				c.mu.Lock()
				c.cursors[sid] = id
				c.mu.Unlock()
			}
			notify()
			data = false
			id = ""
		}
	}

	return scanner.Err()
}
