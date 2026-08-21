package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

// Shipping a default that the daemon would then refuse to start on is the
// worst possible first run.
func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the built-in default does not validate: %v", err)
	}
}

// Two installations on one machine, and any test that must not reach the
// developer's own daemon, need the listen address to be movable by environment
// alone. A config file still outranks the variable.
func TestDefaultAddrHonorsTheEnvironment(t *testing.T) {
	if got := Default().Addr; got != DefaultAddr {
		t.Fatalf("addr with no environment = %q, want %q", got, DefaultAddr)
	}
	t.Setenv("AGENTSWAP_ADDR", " 127.0.0.1:9137 ")
	if got := Default().Addr; got != "127.0.0.1:9137" {
		t.Errorf("addr from AGENTSWAP_ADDR = %q, want 127.0.0.1:9137", got)
	}
	if err := Default().Validate(); err != nil {
		t.Errorf("an overridden default does not validate: %v", err)
	}

	t.Setenv("AGENTSWAP_ADDR", "   ")
	if got := Default().Addr; got != DefaultAddr {
		t.Errorf("addr from a blank AGENTSWAP_ADDR = %q, want %q", got, DefaultAddr)
	}

	t.Setenv("AGENTSWAP_ADDR", "127.0.0.1:9137")
	cfg, err := Load(writeConfig(t, `{"addr": "127.0.0.1:9999"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q, want the config file to outrank AGENTSWAP_ADDR", cfg.Addr)
	}
}

func TestLoadWithoutAFileIsNotAnError(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != Default().Addr {
		t.Errorf("addr = %q, want the default %q", cfg.Addr, Default().Addr)
	}
}

// A user writing one key must not silently zero every other one — a config
// naming only `addr` would otherwise disable parking and set every duration
// to zero.
func TestLoadKeepsDefaultsForAbsentKeys(t *testing.T) {
	dir := writeConfig(t, `{"addr": "127.0.0.1:9999"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q, want the file's value", cfg.Addr)
	}
	if !cfg.Park.Enabled {
		t.Error("parking was disabled by a config that never mentioned it")
	}
	if cfg.Rotation.DrainAbove != Default().Rotation.DrainAbove {
		t.Errorf("drain_above = %v, want the default", cfg.Rotation.DrainAbove)
	}
	if cfg.Retry.BurstCutoff.D() != Default().Retry.BurstCutoff.D() {
		t.Errorf("burst_cutoff = %v, want the default", cfg.Retry.BurstCutoff.D())
	}
}

// A nested object must merge key by key too, not replace the whole struct.
func TestLoadMergesWithinASection(t *testing.T) {
	dir := writeConfig(t, `{"park": {"max_hold": "4h"}}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Park.MaxHold.D() != 4*time.Hour {
		t.Errorf("max_hold = %v, want 4h", cfg.Park.MaxHold.D())
	}
	if !cfg.Park.Enabled {
		t.Error("park.enabled was zeroed by a config that only set max_hold")
	}
	if cfg.Park.Buffer.D() != Default().Park.Buffer.D() {
		t.Errorf("park.buffer = %v, want the default", cfg.Park.Buffer.D())
	}
}

func TestLoadRejectsUnparseableJSON(t *testing.T) {
	dir := writeConfig(t, `{"addr": `)

	if _, err := Load(dir); err == nil {
		t.Fatal("want an error for a truncated config")
	} else if !strings.Contains(err.Error(), "config.json") {
		t.Errorf("err = %v, want it to name the file", err)
	}
}

func TestLoadValidates(t *testing.T) {
	dir := writeConfig(t, `{"rotation": {"drain_above": 150}}`)

	if _, err := Load(dir); err == nil {
		t.Fatal("want an error for an out-of-range drain_above")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"empty addr":                 func(c *Config) { c.Addr = "" },
		"drain_above above 100":      func(c *Config) { c.Rotation.DrainAbove = 101 },
		"drain_above zero":           func(c *Config) { c.Rotation.DrainAbove = 0 },
		"overload_max below initial": func(c *Config) { c.Retry.OverloadMax = c.Retry.OverloadInitial - 1 },
		"overload_initial zero":      func(c *Config) { c.Retry.OverloadInitial = 0 },
		"rotate_after zero":          func(c *Config) { c.Retry.RotateAfter = 0 },
		"unknown keepalive":          func(c *Config) { c.Park.Keepalive = "shout" },
		"keepalive_interval zero":    func(c *Config) { c.Park.KeepaliveInterval = 0 },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			break_(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() accepted %s", name)
			}
		})
	}
}

func TestValidateAcceptsBothKeepaliveModes(t *testing.T) {
	for _, mode := range []string{"", "silent", "ping"} {
		cfg := Default()
		cfg.Park.Keepalive = mode
		if err := cfg.Validate(); err != nil {
			t.Errorf("keepalive %q rejected: %v", mode, err)
		}
	}
}

func TestDirPrecedence(t *testing.T) {
	t.Setenv("AGENTSWAP_HOME", "/tmp/explicit")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got, err := Dir()
	if err != nil || got != "/tmp/explicit" {
		t.Errorf("Dir() = %q, %v; want AGENTSWAP_HOME to win", got, err)
	}

	t.Setenv("AGENTSWAP_HOME", "")
	got, err = Dir()
	if want := filepath.Join("/tmp/xdg", "agentswap"); err != nil || got != want {
		t.Errorf("Dir() = %q, %v; want %q", got, err, want)
	}
}

func TestDurationAcceptsHumanSpellings(t *testing.T) {
	var cfg struct {
		D Duration `json:"d"`
	}
	cases := map[string]time.Duration{
		`{"d": "90s"}`:   90 * time.Second,
		`{"d": "30m"}`:   30 * time.Minute,
		`{"d": "1h30m"}`: 90 * time.Minute,
		// A bare number is seconds: nanoseconds would be a bizarre thing to
		// type and a silent 1e9x error if we guessed wrong.
		`{"d": 45}`: 45 * time.Second,
	}
	for in, want := range cases {
		if err := json.Unmarshal([]byte(in), &cfg); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if cfg.D.D() != want {
			t.Errorf("Unmarshal(%s) = %v, want %v", in, cfg.D.D(), want)
		}
	}
}

func TestDurationRejectsNonsense(t *testing.T) {
	var cfg struct {
		D Duration `json:"d"`
	}
	for _, in := range []string{`{"d": "soon"}`, `{"d": true}`, `{"d": {}}`} {
		if err := json.Unmarshal([]byte(in), &cfg); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want an error", in)
		}
	}
}

// A config that agentswap writes must be one it can read back.
func TestDurationRoundTrips(t *testing.T) {
	b, err := json.Marshal(Default())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Park.MaxHold.D() != Default().Park.MaxHold.D() {
		t.Errorf("max_hold round-tripped to %v", back.Park.MaxHold.D())
	}
	if err := back.Validate(); err != nil {
		t.Errorf("a round-tripped default no longer validates: %v", err)
	}
}
