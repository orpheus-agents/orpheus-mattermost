package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestWorkflowConnectionsDiscoverIdentityAndKeepRoutesSeparate(t *testing.T) {
	engine, api, _, _, _ := fixture(t)
	t.Setenv("AGENTBOX_API_KEY", "test-sdk-key")
	t.Setenv("FIRST_BOT_TOKEN", "first-token")
	t.Setenv("SECOND_BOT_TOKEN", "second-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/config/client" {
			_ = json.NewEncoder(w).Encode(map[string]string{"MaxPostSize": "4000"})
			return
		}
		if r.URL.Path != "/api/v4/users/me" {
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		bot := mattermost.User{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Username: "first"}
		switch r.Header.Get("Authorization") {
		case "Bearer first-token":
		case "Bearer second-token":
			bot.ID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
			bot.Username = "second"
		default:
			t.Error("wrong credential")
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(bot)
	}))
	defer server.Close()
	cfg := engine.Config
	cfg.Workflows[0].Mattermost = config.Endpoint{BaseURL: server.URL, TokenEnv: "FIRST_BOT_TOKEN"}
	second := cfg.Workflows[0]
	second.ID = "second"
	second.Mattermost.TokenEnv = "SECOND_BOT_TOKEN"
	cfg.Workflows = append(cfg.Workflows, second)
	manager := &Manager{Config: cfg, API: api}
	if err := manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(manager.connections) != 2 {
		t.Fatal("connections not isolated")
	}
	for endpoint, c := range manager.connections {
		got := c.runtime.Engine
		if got.SourceID != endpoint.Source(got.Bot.ID) || got.Config.Workflows[0].MaxPostChars != 4000 {
			t.Fatal("identity/limits not discovered")
		}
		workflow, err := got.Config.Route("cccccccccccccccccccccccccc", "O")
		if err != nil || workflow == nil || workflow.Mattermost != endpoint {
			t.Fatal("foreign workflow routed", err)
		}
	}
	rec := httptest.NewRecorder()
	manager.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatal("unreconciled connections are ready")
	}
	for _, c := range manager.connections {
		c.runtime.ready.Store(true)
	}
	rec = httptest.NewRecorder()
	manager.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != 200 {
		t.Fatal("reconciled connections not ready")
	}
	rec = httptest.NewRecorder()
	manager.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Count(rec.Body.String(), "orpheus_mattermost_pending_inputs{source=") != 2 {
		t.Fatal("connection metrics missing")
	}
	// An API URL alone does not require credentials or enable sandbox access.
	t.Setenv("AGENTBOX_API_KEY", "")
	optional := cfg
	optional.AgentBoxAPIURL = "https://unused.example.com"
	withoutSDK := &Manager{Config: optional, API: api}
	if err := withoutSDK.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range withoutSDK.connections {
		if c.runtime.Engine.Sandbox != nil {
			t.Fatal("SDK enabled without key")
		}
	}
	// Whole-file validation and compatibility precede delivery to any runtime.
	invalid := cfg
	invalid.Workflows = slices.Clone(cfg.Workflows)
	invalid.Workflows[0].Mattermost.BaseURL = "https://other.example.com"
	if err := manager.reload(invalid); err == nil {
		t.Fatal("new transport reloaded")
	}
	for _, c := range manager.connections {
		if len(c.reload) != 0 {
			t.Fatal("partial reload")
		}
	}
	removed := cfg
	removed.Workflows = cfg.Workflows[:1]
	if err := manager.reload(removed); err != nil {
		t.Fatal(err)
	}
	for endpoint, c := range manager.connections {
		next := <-c.reload
		if endpoint == second.Mattermost && len(next.Workflows) != 0 {
			t.Fatal("removed connection still admits inputs")
		}
	}
}
func TestManagerShutdownStopsAllConnections(t *testing.T) {
	engine, _, source, _, key := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	engine.API = &runtimeAPI{cancel: cancel}
	engine.MM = runtimeSource{source, key.Channel}
	// The run has no new triggers, so shutdown is driven solely by the context.
	source.posts = nil
	manager := &Manager{connections: map[config.Endpoint]connection{{BaseURL: "https://chat.example.com"}: {runtime: &Runtime{Engine: engine}}}}
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not stop child runtime")
	}
}
