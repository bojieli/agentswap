package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/session"
	"github.com/bojieli/agentswap/internal/store"
	"github.com/bojieli/agentswap/internal/supervisor"
)

func TestFreshTicketSuggestsSourceNotTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSWAP_HOME", dir)
	if err := supervisor.WriteTicket(dir, supervisor.Ticket{
		Lane: store.LaneAnthropic, Until: time.Now().Add(time.Hour), WrittenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := freshTicketSource(session.Codex); got != session.Claude {
		t.Fatalf("ticket source = %q, want Claude", got)
	}
	if got := freshTicketSource(session.Claude); got != "" {
		t.Fatalf("ticket suggested the target as its own source: %q", got)
	}
}

func TestActiveSessionIDRequiresKnownSource(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "thread-1")
	if got := activeSessionID(session.Codex); got != "thread-1" {
		t.Fatalf("Codex active id = %q", got)
	}
	if got := activeSessionID(""); got != "" {
		t.Fatalf("source-less active id = %q", got)
	}
	t.Setenv("AGENTSWAP_SESSION_ID", "explicit")
	if got := activeSessionID(""); got != "explicit" {
		t.Fatalf("generic active id = %q", got)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"codex", "resume", "plain", "two words", "it's"})
	want := "codex resume plain 'two words' 'it'\\''s'"
	if got != want {
		t.Fatalf("shellJoin = %q, want %q", got, want)
	}
}

func TestFreshTicketIgnoresOldTicket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSWAP_HOME", dir)
	if err := supervisor.WriteTicket(filepath.Clean(dir), supervisor.Ticket{
		Lane: store.LaneOpenAI, Until: time.Now().Add(time.Hour), WrittenAt: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := freshTicketSource(session.Claude); got != "" {
		t.Fatalf("stale ticket source = %q", got)
	}
}
