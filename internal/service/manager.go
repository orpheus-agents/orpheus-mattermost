package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/orpheus-agents/orpheus-mattermost/internal/agentbox"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type connection struct {
	runtime *Runtime
	reload  chan config.Config
}

// Manager shares one HTTP listener across independent Mattermost connections.
// Workflows using the same URL/token reference share routing and one event stream.
type Manager struct {
	Config      config.Config
	API         API
	Reload      <-chan config.Config
	Logger      *slog.Logger
	connections map[config.Endpoint]connection
}

func (m *Manager) Init(ctx context.Context) error {
	if m.Logger == nil {
		m.Logger = slog.Default()
	}
	m.connections = map[config.Endpoint]connection{}
	bots := map[string]bool{}
	for endpoint, cfg := range m.Config.Groups() {
		token, err := config.Secret(endpoint.TokenEnv)
		if err != nil {
			return err
		}
		mm := mattermost.New(endpoint.BaseURL, token, cfg.HTTPTimeout.Value())
		engine := &Engine{Config: cfg, MM: mm, API: m.API}
		if os.Getenv("AGENTBOX_API_KEY") != "" {
			sandbox, err := agentbox.New(cfg, m.API, mm)
			if err != nil {
				return err
			}
			engine.Sandbox = sandbox
		}
		if err := engine.Validate(ctx); err != nil {
			return err
		}
		if bots[engine.SourceID] {
			return errors.New("workflows for the same Mattermost bot must share one token_env reference")
		}
		bots[engine.SourceID] = true
		reload := make(chan config.Config, 1)
		m.connections[endpoint] = connection{runtime: &Runtime{Engine: engine, Reload: reload, Logger: m.Logger}, reload: reload}
	}
	return nil
}
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	ready := func(w http.ResponseWriter, _ *http.Request) {
		for _, c := range m.connections {
			if !c.runtime.ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /ready", ready)
	mux.HandleFunc("GET /readyz", ready)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for _, c := range m.connections {
			c.runtime.metrics(w, c.runtime.Engine.SourceID)
		}
	})
	return mux
}
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	done := make(chan error, len(m.connections))
	for _, connection := range m.connections {
		wg.Go(func() { done <- connection.runtime.Run(ctx) })
	}
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			cancel()
			return err
		case cfg := <-m.Reload:
			if err := m.reload(cfg); err != nil {
				m.Logger.Error("configuration reload rejected", "reason", err.Error())
			}
		}
	}
}
func (m *Manager) reload(cfg config.Config) error {
	if !reloadCompatible(m.Config, cfg) {
		return errors.New("process settings require restart")
	}
	groups := cfg.Groups()
	for endpoint := range groups {
		if _, ok := m.connections[endpoint]; !ok {
			return errors.New("new Mattermost connections require restart")
		}
	}
	for _, c := range m.connections {
		if len(c.reload) != 0 {
			return errors.New("configuration reload already pending")
		}
	}
	for endpoint, c := range m.connections {
		group, ok := groups[endpoint]
		if !ok {
			group = cfg
			group.Workflows = nil
		}
		c.reload <- group
	}
	m.Config = cfg
	return nil
}
