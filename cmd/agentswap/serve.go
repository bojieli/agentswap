package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/engine"
	"github.com/bojieli/agentswap/internal/proxy"
	"github.com/bojieli/agentswap/internal/store"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address (default from config)")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}

	log := newLogger(*verbose)
	client := upstreamClient()

	srv := &proxy.Server{
		Engine:    engine.New(cfg, st, buildLanes(), client, log),
		Store:     st,
		Config:    cfg,
		Keepalive: proxy.KeepaliveMode(orDefault(cfg.Park.Keepalive, string(proxy.KeepaliveSilent))),
		Log:       log,
		ConfigDir: dir,
	}

	// No write timeout: a parked request holds the connection open on purpose,
	// and a streaming answer can run for many minutes.
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	done := make(chan struct{})
	go st.RunHealthFlusher(done, 5*time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		close(done)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	summarize(log, st, cfg)
	log.Info("listening", "addr", cfg.Addr,
		"anthropic", "http://"+cfg.Addr+"/anthropic",
		"openai", "http://"+cfg.Addr+"/openai")

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-ctx.Done()
	return st.FlushHealth()
}

// summarize warns at startup about the states that make the daemon useless,
// rather than letting the first request discover them.
func summarize(log logger, st *store.Store, cfg config.Config) {
	for _, l := range []store.LaneID{store.LaneAnthropic, store.LaneOpenAI} {
		n := len(st.Accounts(l))
		if n == 0 {
			log.Warn("lane has no accounts; requests to it will fail",
				"lane", l, "fix", fmt.Sprintf("agentswap import  (or: agentswap add-key %s --key ...)", l))
			continue
		}
		log.Info("lane ready", "lane", l, "accounts", n)
	}
	if !cfg.Park.Enabled {
		log.Warn("parking is disabled; requests will fail when every account is spent")
	}
}

// logger is the slice of *slog.Logger used here, kept small so tests can
// substitute a recorder.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
