package mattermost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAudienceCompleteness(t *testing.T) {
	for _, mode := range []string{"complete", "exact_page", "duplicate", "truncated", "wrong_channel", "bad_id", "unavailable", "changed", "missing_count", "null", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			const channel = "cccccccccccccccccccccccccc"
			count, statsCalls, pages := 405, 0, 0
			if mode == "exact_page" {
				count = 400
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test" {
					t.Error("missing auth")
				}
				if strings.HasSuffix(r.URL.Path, "/stats") {
					statsCalls++
					if mode == "missing_count" {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					n := count
					if mode == "changed" && statsCalls > 1 {
						n++
					}
					if mode == "oversized" {
						n = 4097
					}
					_ = json.NewEncoder(w).Encode(map[string]int{"member_count": n})
					return
				}
				pages++
				if mode == "unavailable" && pages == 2 {
					w.WriteHeader(503)
					return
				}
				if mode == "null" {
					_, _ = w.Write([]byte(`null`))
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if r.URL.Query().Get("per_page") != "200" {
					t.Error("wrong page size")
				}
				members := []map[string]string{}
				end := min(count, (page+1)*200)
				if mode == "truncated" && page == 1 {
					end = 201
				}
				for i := page * 200; i < end; i++ {
					id, ch := fmt.Sprintf("%026d", i+1), channel
					if mode == "duplicate" && i == 200 {
						id = fmt.Sprintf("%026d", 1)
					}
					if mode == "wrong_channel" {
						ch = "other"
					}
					if mode == "bad_id" {
						id = "bad"
					}
					members = append(members, map[string]string{"user_id": id, "channel_id": ch, "roles": "channel_user channel_admin"})
				}
				_ = json.NewEncoder(w).Encode(members)
			}))
			defer server.Close()
			got, err := New(server.URL, "test", time.Second).Audience(t.Context(), channel)
			if mode != "complete" && mode != "exact_page" {
				if err == nil || got != nil {
					t.Fatal("partial audience accepted", len(got), err)
				}
				return
			}
			if err != nil || len(got) != count || pages != 3 || statsCalls != 2 {
				t.Fatal(len(got), pages, statsCalls, err)
			}
		})
	}
}
