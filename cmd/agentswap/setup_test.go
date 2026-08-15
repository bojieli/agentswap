package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This gates the warning about serving credentials to the network, so a wrong
// answer here is quiet in exactly the case that matters.
func TestIsLoopback(t *testing.T) {
	loopback := []string{
		"127.0.0.1:8420",
		"127.0.0.1",
		"127.0.0.2:8420",
		"localhost:8420",
		"LocalHost:8420",
		"[::1]:8420",
		"::1",
	}
	for _, addr := range loopback {
		if !isLoopback(addr) {
			t.Errorf("isLoopback(%q) = false, want true", addr)
		}
	}

	exposed := []string{
		":8420",        // every interface, which is the whole point of warning
		"0.0.0.0:8420", // likewise, spelled out
		"[::]:8420",
		"192.168.1.10:8420",
		"10.0.0.5:8420",
		"agentswap.example.com:8420",
	}
	for _, addr := range exposed {
		if isLoopback(addr) {
			t.Errorf("isLoopback(%q) = true, want false: this address is reachable from elsewhere", addr)
		}
	}
}

func TestWiredAddr(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings,
		[]byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8420/anthropic"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pointed at the running daemon.
	if got := wiredAddr(settings, "/anthropic", "127.0.0.1:8420", "127.0.0.1:8420"); got != "127.0.0.1:8420" {
		t.Errorf("wiredAddr = %q, want the address it is wired to", got)
	}
	// Pointed at the configured address while the daemon runs elsewhere: the
	// distinction that lets doctor give advice that would help.
	if got := wiredAddr(settings, "/anthropic", "127.0.0.1:18420", "127.0.0.1:8420"); got != "127.0.0.1:8420" {
		t.Errorf("wiredAddr = %q, want the address it is actually wired to", got)
	}
	// Not wired to anything we know about.
	if got := wiredAddr(settings, "/anthropic", "127.0.0.1:18420"); got != "" {
		t.Errorf("wiredAddr = %q, want empty", got)
	}
	// A file that is not there is not wired.
	if got := wiredAddr(filepath.Join(dir, "absent.json"), "/anthropic", "127.0.0.1:8420"); got != "" {
		t.Errorf("wiredAddr = %q, want empty for a missing file", got)
	}
	if got := wiredAddr("", "/anthropic", "127.0.0.1:8420"); got != "" {
		t.Errorf("wiredAddr = %q, want empty for an empty path", got)
	}
}

func TestDaemonAddrs(t *testing.T) {
	t.Setenv("AGENTSWAP_HOME", t.TempDir())

	// An explicit --addr is the user telling us where to look; nothing else
	// should be tried.
	got := daemonAddrs("127.0.0.1:8420", "127.0.0.1:9999")
	if len(got) != 1 || got[0] != "127.0.0.1:9999" {
		t.Errorf("daemonAddrs with an override = %v, want just the override", got)
	}

	// With no daemon running, the configured address is all there is.
	got = daemonAddrs("127.0.0.1:8420", "")
	if len(got) != 1 || got[0] != "127.0.0.1:8420" {
		t.Errorf("daemonAddrs = %v, want just the configured address", got)
	}
}

func TestHumanUntil(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Second:                  "now",
		0:                             "now",
		30 * time.Second:              "30s",
		90 * time.Second:              "1m",
		59 * time.Minute:              "59m",
		time.Hour:                     "1h 0m",
		2*time.Hour + 5*time.Minute:   "2h 5m",
		4*time.Hour + 59*time.Minute:  "4h 59m",
		25*time.Hour + 30*time.Minute: "25h 30m",
	}
	for d, want := range cases {
		if got := humanUntil(d); got != want {
			t.Errorf("humanUntil(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 18); got != "short" {
		t.Errorf("truncate = %q, want it untouched", got)
	}
	got := truncate("an-extremely-long-account-label", 18)
	if len([]rune(got)) != 18 {
		t.Errorf("truncate = %q (%d runes), want 18", got, len([]rune(got)))
	}
	if got[len(got)-3:] != "…" {
		t.Errorf("truncate = %q, want it to end with an ellipsis", got)
	}
}
