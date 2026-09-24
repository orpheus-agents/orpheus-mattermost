package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/service"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err.Error())
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: orpheus-mattermost serve|validate|healthcheck")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	address := flags.String("url", healthURL(), "health endpoint")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "healthcheck":
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, *address, nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return errors.New("health endpoint unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	case "validate":
		_, err := config.Load()
		if err == nil {
			_, err = fmt.Fprintln(os.Stdout, "Configuration valid.")
		}
		return err
	case "serve":
		return serve(ctx)
	default:
		return errors.New("unknown command")
	}
}
func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	bearer, err := config.Secret(cfg.Orpheus.TokenEnv)
	if err != nil {
		return err
	}
	api, err := orpheus.New(cfg, bearer)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reload := make(chan config.Config, 1)
	runtime := &service.Manager{Config: cfg, API: api, Reload: reload, Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	if err := runtime.Init(ctx); err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.Listen, Handler: runtime.Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	for {
		select {
		case err := <-done:
			return err
		case err := <-serverErrors:
			cancel()
			<-done
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-hup:
			next, err := config.Load()
			if err != nil {
				slog.Error("configuration reload failed", "error", err.Error())
				continue
			}
			select {
			case reload <- next:
			default:
				slog.Warn("configuration reload already pending")
			}
		}
	}
}

func healthURL() string {
	host, port, err := net.SplitHostPort(cmp.Or(os.Getenv("LISTEN_ADDR"), ":8080"))
	if err != nil {
		return "http://127.0.0.1:8080/healthz"
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
