// Package config loads agentswap's settings. Everything has a working default,
// so an absent config file is not an error.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is the daemon's tunable behavior.
type Config struct {
	// Addr is the local listen address. Binding to loopback is deliberate and
	// should not be relaxed casually: the proxy accepts any request on this
	// port and answers it with real credentials.
	Addr string `json:"addr"`

	Rotation Rotation `json:"rotation"`
	Retry    Retry    `json:"retry"`
	Park     Park     `json:"park"`
}

type Rotation struct {
	// DrainAbove proactively retires an account once an observed window
	// crosses this utilization percentage, instead of waiting for the 429.
	// The quota headers make this possible; it is the main edge over
	// reactive-only rotation.
	DrainAbove float64 `json:"drain_above"`

	// Sticky prefers the account that last served the same conversation.
	// Prompt caches are per-account, so rotating for its own sake silently
	// converts cache hits into full-price cache misses.
	Sticky    bool     `json:"sticky"`
	StickyTTL Duration `json:"sticky_ttl"`
}

type Retry struct {
	// BurstCutoff separates a short per-minute throttle from real quota
	// exhaustion. A Retry-After at or below this means wait and reuse the same
	// account; above it means the account is spent and we should rotate.
	BurstCutoff Duration `json:"burst_cutoff"`

	// Overload backoff for 429-burst, 529 and 5xx. Retries are unbounded by
	// design: an overloaded server is temporary, and giving up is what makes
	// the agent stall.
	OverloadInitial Duration `json:"overload_initial"`
	OverloadMax     Duration `json:"overload_max"`

	// RotateAfter is how many consecutive overload responses to absorb on one
	// account before trying a different one, in case the fault is
	// account-scoped rather than global.
	RotateAfter int `json:"rotate_after"`

	// AuthRefreshAttempts caps 401-driven token refreshes per request.
	AuthRefreshAttempts int `json:"auth_refresh_attempts"`
}

type Park struct {
	// Enabled parks a request when every account is spent, instead of
	// returning an error the agent would treat as fatal.
	Enabled bool `json:"enabled"`

	// Buffer is added to every observed reset time. Server and client clocks
	// disagree, and retrying one second early wastes the whole wait.
	Buffer Duration `json:"buffer"`

	// MaxHold caps how long a request may be parked before we give up and hand
	// off to the supervisor for a resume.
	MaxHold Duration `json:"max_hold"`

	// KeepaliveInterval is the gap between SSE pings sent while parked.
	KeepaliveInterval Duration `json:"keepalive_interval"`
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Addr: "127.0.0.1:8420",
		Rotation: Rotation{
			DrainAbove: 98,
			Sticky:     true,
			StickyTTL:  Duration(30 * time.Minute),
		},
		Retry: Retry{
			BurstCutoff:         Duration(120 * time.Second),
			OverloadInitial:     Duration(time.Second),
			OverloadMax:         Duration(60 * time.Second),
			RotateAfter:         3,
			AuthRefreshAttempts: 1,
		},
		Park: Park{
			Enabled:           true,
			Buffer:            Duration(60 * time.Second),
			MaxHold:           Duration(30 * time.Minute),
			KeepaliveInterval: Duration(15 * time.Second),
		},
	}
}

// Dir returns agentswap's configuration directory, honoring AGENTSWAP_HOME and
// then XDG_CONFIG_HOME before falling back to ~/.config/agentswap.
func Dir() (string, error) {
	if d := os.Getenv("AGENTSWAP_HOME"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "agentswap"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "agentswap"), nil
}

// Load reads config.json from dir, filling in defaults for absent fields.
func Load(dir string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// Unmarshalling onto the defaults means an absent key keeps its default
	// rather than becoming a zero value.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config.json: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate rejects settings that would make the daemon misbehave in ways that
// are hard to diagnose from its logs.
func (c Config) Validate() error {
	if c.Addr == "" {
		return errors.New("addr must not be empty")
	}
	if c.Rotation.DrainAbove <= 0 || c.Rotation.DrainAbove > 100 {
		return fmt.Errorf("rotation.drain_above must be in (0,100], got %v", c.Rotation.DrainAbove)
	}
	if c.Retry.OverloadInitial <= 0 || c.Retry.OverloadMax < c.Retry.OverloadInitial {
		return errors.New("retry.overload_max must be >= retry.overload_initial > 0")
	}
	if c.Retry.RotateAfter < 1 {
		return errors.New("retry.rotate_after must be >= 1")
	}
	if c.Park.Enabled && c.Park.KeepaliveInterval <= 0 {
		return errors.New("park.keepalive_interval must be > 0 when parking is enabled")
	}
	return nil
}
