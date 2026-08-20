package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/lane/anthropic"
	"github.com/bojieli/agentswap/internal/lane/openai"
	"github.com/bojieli/agentswap/internal/store"
)

// openStore loads the pool from the configured directory.
func openStore() (*store.Store, config.Config, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, config.Config{}, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, config.Config{}, err
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, config.Config{}, err
	}
	return st, cfg, nil
}

// openStoreReadOnly resolves the same files as openStore without creating or
// repairing anything. A dry run must remain read-only even on a fresh machine.
func openStoreReadOnly() (*store.Store, config.Config, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, config.Config{}, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, config.Config{}, err
	}
	st, err := store.OpenReadOnly(dir)
	if err != nil {
		return nil, config.Config{}, err
	}
	return st, cfg, nil
}

// upstreamClient is the HTTP client used for model traffic.
//
// It deliberately has no Timeout: a single response can stream for many
// minutes, and a client-level deadline would sever it mid-answer. Per-request
// deadlines come from the request context instead.
func upstreamClient() *http.Client {
	return &http.Client{
		// Never follow a redirect. A proxy has no business deciding to send
		// somebody's credential to a host they did not configure — Go strips
		// Authorization across domains but knows nothing about X-Api-Key, so
		// an upstream redirect would hand the key to whoever it named. It also
		// rewrites POST as GET on a 302, which would turn a message into a
		// silent no-op. Relaying the 3xx lets the client decide.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// buildLanes constructs the protocol adapters. Token refresh gets its own
// short-timeout client, since an auth call that hangs should fail fast rather
// than stall the request waiting on it.
func buildLanes() map[store.LaneID]lane.Lane {
	authClient := &http.Client{Timeout: 30 * time.Second}
	return map[store.LaneID]lane.Lane{
		store.LaneAnthropic: anthropic.New(authClient),
		store.LaneOpenAI:    openai.New(authClient),
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
