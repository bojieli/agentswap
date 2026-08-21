// Package config loads agentswap's settings. Everything has a working default,
// so an absent config file is not an error.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the daemon's tunable behavior.
type Config struct {
	// Addr is the local listen address. Binding to loopback is deliberate and
	// should not be relaxed casually: the proxy accepts any request on this
	// port and answers it with real credentials.
	Addr string `json:"addr"`

	// AllowedHosts extends the set of Host header values the proxy will answer
	// to, beyond loopback names and Addr's own host.
	//
	// The check exists to stop DNS rebinding: a web page the user visits cannot
	// otherwise be prevented from making requests to 127.0.0.1, and those
	// requests would be served with real credentials. Rebinding needs the
	// attacker's own hostname in the Host header, so refusing unknown names
	// closes it. Set this when reaching agentswap by another name on purpose —
	// from a container, or across a tunnel.
	AllowedHosts []string `json:"allowed_hosts,omitempty"`

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

	// AuthRefreshAttempts caps 401-driven token refreshes per account, for one
	// request. Per account rather than per request because it measures whether
	// renewing *this* credential is still worth trying.
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

	// Keepalive is "silent" or "ping".
	//
	// Silent holds the connection with no bytes written and is the default:
	// the client is on loopback, so nothing between us can time out an idle
	// socket, and writing nothing means we never commit to a status code we
	// might need to take back. Ping commits to a 200 event-stream and sends
	// SSE pings, which is only worth its extra risk if a client refuses to
	// wait without seeing bytes.
	Keepalive string `json:"keepalive"`
}

// DefaultAddr is the address agentswap serves on, and the one every other
// command looks for a daemon at, when nothing configures one.
const DefaultAddr = "127.0.0.1:8420"

// defaultAddr honors AGENTSWAP_ADDR the way Dir honors AGENTSWAP_HOME. Without
// it the listen address is the one piece of agentswap's environment that a
// caller cannot move: two installations on one machine, or a test that must
// not find the developer's own daemon, would otherwise both fall back to
// DefaultAddr and reach whichever daemon got there first.
func defaultAddr() string {
	if a := strings.TrimSpace(os.Getenv("AGENTSWAP_ADDR")); a != "" {
		return a
	}
	return DefaultAddr
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Addr: defaultAddr(),
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
			Keepalive:         "silent",
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
	// An empty file says the same thing as no file, and is easy to create by
	// accident: `agentswap config --json > config.json` truncates the file
	// before agentswap runs and reads it. Refusing to start over zero bytes
	// would leave every command failing until the user worked out that
	// deleting the file was the cure.
	if len(bytes.TrimSpace(b)) == 0 {
		return cfg, nil
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
	switch c.Park.Keepalive {
	case "", "silent", "ping":
	default:
		return fmt.Errorf("park.keepalive must be \"silent\" or \"ping\", got %q", c.Park.Keepalive)
	}
	return nil
}
