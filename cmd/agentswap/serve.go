package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/daemon"
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

	// Publish where we are listening so `status` and `doctor` can find us even
	// when --addr disagrees with the config file.
	if err := daemon.Write(dir, daemon.Info{
		Addr: cfg.Addr, PID: os.Getpid(), Version: version, StartedAt: time.Now(),
	}); err != nil {
		log.Warn("could not record the daemon address", "err", err,
			"effect", "status and doctor will look at the configured address instead")
	}
	defer func() { _ = daemon.Clear(dir) }()

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
	if !isLoopback(cfg.Addr) {
		log.Warn("listening beyond loopback: anyone who can reach this port can spend your subscriptions",
			"addr", cfg.Addr, "fix", "set addr to 127.0.0.1:8420 unless you have a reason not to")
	}
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

// isLoopback reports whether addr keeps the daemon reachable only from this
// machine. An unparseable or hostname-based address is treated as not
// loopback: warning about an address that turns out to be safe is cheaper than
// staying quiet about one that is not.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// A bare ":8420" binds every interface, so an empty host is the least
	// loopback address there is.
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
