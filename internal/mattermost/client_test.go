package mattermost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestThreadPaginationRequiresTimestamp(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing auth")
		}
		if strings.HasSuffix(r.URL.Path, "/thread") {
			calls++
			if r.URL.Query().Get("fromCreateAt") == "" {
				t.Error("timestamp missing")
			}
			posts := map[string]Post{"root": {ID: "root", ChannelID: "ch", CreateAt: 1}}
			if calls == 1 {
				for i := range 199 {
					id := fmt.Sprintf("r%03d", i)
					posts[id] = Post{ID: id, RootID: "root", ChannelID: "ch", CreateAt: int64(i + 2)}
				}
			} else {
				posts["target"] = Post{ID: "target", RootID: "root", ChannelID: "ch", CreateAt: 500}
			}
			order := []string{}
			for id := range posts {
				order = append(order, id)
			}
			_ = json.NewEncoder(w).Encode(PostList{Posts: posts, Order: order, HasNext: new(calls == 1)})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v4/posts/")
		root := "root"
		created := int64(500)
		if id == "root" {
			root = ""
			created = 1
		}
		_ = json.NewEncoder(w).Encode(Post{ID: id, RootID: root, ChannelID: "ch", CreateAt: created})
	}))
	defer s.Close()
	c := New(s.URL, "token", time.Second)
	posts, e := c.Thread(t.Context(), "target")
	if e != nil || len(posts) != 201 || calls != 2 {
		t.Fatal(len(posts), calls, e)
	}
}
func TestRedirectAndActualDownloadLimit(t *testing.T) {
	escaped := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "redirect") {
			http.Redirect(w, r, other.URL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("123456"))
	}))
	defer server.Close()
	c := New(server.URL, "secret", time.Second)
	if _, e := c.Download(t.Context(), "redirect", 100); Status(e) != 302 {
		t.Fatal(e)
	}
	if escaped {
		t.Fatal("credentialed redirect followed")
	}
	if _, e := c.Download(t.Context(), "large", 3); e == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestChannelPaginationPreservesEqualTimestamps(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("before") != "" || r.URL.Query().Get("since") != "" {
			t.Error("lossy cursor used")
		}
		page := r.URL.Query().Get("page")
		result := PostList{Posts: map[string]Post{}}
		start, count := 0, 200
		if page == "1" {
			start, count = 200, 5
		}
		for i := start; i < start+count; i++ {
			id := fmt.Sprintf("%026d", i)
			result.Order = append(result.Order, id)
			result.Posts[id] = Post{ID: id, ChannelID: "ch", CreateAt: 1000}
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer s.Close()
	c := New(s.URL, "token", time.Second)
	posts, err := c.Posts(t.Context(), "ch", time.Unix(0, 0))
	if err != nil || len(posts) != 205 || calls != 2 {
		t.Fatal(len(posts), calls, err)
	}
}

func TestWebSocketAuthenticationAndHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		var challenge struct {
			Action string            `json:"action"`
			Data   map[string]string `json:"data"`
		}
		if json.Unmarshal(raw, &challenge) != nil || challenge.Action != "authentication_challenge" || challenge.Data["token"] != "private" {
			t.Error("invalid auth challenge")
		}
		post, _ := json.Marshal(Post{ID: "post", ChannelID: "channel"})
		event, _ := json.Marshal(map[string]any{"event": "posted", "data": map[string]string{"post": string(post)}})
		if err := conn.Write(r.Context(), websocket.MessageText, event); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := New(server.URL, "private", time.Second)
	notified := ""
	_ = c.Watch(t.Context(), func(event Event) { notified = event.Post.ChannelID })
	if notified != "channel" {
		t.Fatal("WebSocket hint lost")
	}
}
