package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/store"
)

func TestDetectKind(t *testing.T) {
	cases := map[string]Kind{
		"claude":                KindClaude,
		"/usr/local/bin/claude": KindClaude,
		"claude.exe":            KindClaude,
		"codex":                 KindCodex,
		"/opt/bin/codex":        KindCodex,
		"vim":                   KindUnknown,
	}
	for in, want := range cases {
		if got := DetectKind(in); got != want {
			t.Errorf("DetectKind(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResumeArgsDropsTheOriginalPrompt(t *testing.T) {
	// Replaying the original instruction would make the agent start over
	// rather than pick up where the quota ran out.
	got := resumeArgs(KindClaude, []string{"claude", "refactor the parser"})
	want := []string{"claude", "--continue"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claude resume = %v, want %v", got, want)
	}

	got = resumeArgs(KindCodex, []string{"codex", "exec", "fix tests", "--profile", "agentswap"})
	want = []string{"codex", "resume", "--last", "--profile", "agentswap"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codex resume = %v, want %v", got, want)
	}
}

func TestEnsureCodexProfile(t *testing.T) {
	// Without a profile Codex silently bypasses the proxy entirely, which
	// looks like agentswap doing nothing at all.
	got := ensureCodexProfile([]string{"codex", "exec", "hi"})
	if len(got) < 2 || got[len(got)-2] != "--profile" {
		t.Errorf("got %v, want a --profile appended", got)
	}
	// A profile the user chose must win.
	orig := []string{"codex", "--profile", "mine", "exec"}
	if got := ensureCodexProfile(orig); !reflect.DeepEqual(got, orig) {
		t.Errorf("got %v, want the user's own profile untouched", got)
	}
}

func TestTicketRoundTrip(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).Round(time.Second)

	if err := WriteTicket(dir, Ticket{Lane: store.LaneAnthropic, Until: until, WrittenAt: time.Now()}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadTicket(dir, store.LaneAnthropic)
	if err != nil || got == nil {
		t.Fatalf("read: %v %v", got, err)
	}
	if !got.Until.Equal(until) {
		t.Errorf("until = %v, want %v", got.Until, until)
	}
	if err := ConsumeTicket(dir, store.LaneAnthropic); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got, _ := ReadTicket(dir, store.LaneAnthropic); got != nil {
		t.Error("ticket survived being consumed")
	}
	// Consuming twice is not an error; a resumed run should not fail because
	// something else already cleaned up.
	if err := ConsumeTicket(dir, store.LaneAnthropic); err != nil {
		t.Errorf("second consume: %v", err)
	}
}

func TestPendingSinceIgnoresStaleTickets(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Left over from an earlier run: acting on it would wait out a reset that
	// has already happened. Well outside the freshness grace.
	if err := WriteTicket(dir, Ticket{
		Lane: store.LaneAnthropic, Until: now.Add(time.Hour), WrittenAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := PendingSince(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got a stale ticket %+v, want none", got)
	}

	// Written by this run and still in the future: actionable.
	if err := WriteTicket(dir, Ticket{
		Lane: store.LaneOpenAI, Until: now.Add(time.Hour), WrittenAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	got, err = PendingSince(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Lane != store.LaneOpenAI {
		t.Errorf("got %+v, want the fresh openai ticket", got)
	}
}

func TestPendingSinceIgnoresElapsedDeadline(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := WriteTicket(dir, Ticket{
		Lane: store.LaneAnthropic, Until: now.Add(-time.Minute), WrittenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := PendingSince(dir, now.Add(-time.Hour))
	if got != nil {
		t.Errorf("got %+v, want none: the deadline already passed", got)
	}
}

// Run should resume the session once, then stop, when a ticket is pending.
func TestRunResumesAfterATicket(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations.log")

	// A stand-in for `claude`: records how it was called, and on the first run
	// drops a ticket the way the proxy would.
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + log + "\n" +
		"if [ ! -f " + dir + "/fired ]; then\n" +
		"  touch " + dir + "/fired\n" +
		"  mkdir -p " + TicketDir(dir) + "\n" +
		"  printf '{\"lane\":\"anthropic\",\"until\":\"%s\",\"written_at\":\"%s\"}' \\\n" +
		"    \"$(date -u -d '+3 seconds' +%Y-%m-%dT%H:%M:%SZ)\" \\\n" +
		"    \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" > " + TicketDir(dir) + "/anthropic.json\n" +
		"  exit 1\n" +
		"fi\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := Run(ctx, Options{
		ConfigDir: dir, Addr: "127.0.0.1:8420",
		Args: []string{fake, "do the thing"}, MaxResumes: 3, Out: &out,
	})
	if err != nil {
		t.Fatalf("Run: %v (output: %s)", err, out.String())
	}

	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("invocations = %q, want two (original then resume)", lines)
	}
	if lines[0] != "do the thing" {
		t.Errorf("first invocation = %q", lines[0])
	}
	if lines[1] != "--continue" {
		t.Errorf("resume invocation = %q, want --continue", lines[1])
	}
}
