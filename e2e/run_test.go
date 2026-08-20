package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCLI writes a stand-in for `claude` or `codex` that records how it was
// called. The name matters: the supervisor decides how to resume from it.
func fakeCLI(t *testing.T, dir, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}
	path := filepath.Join(dir, name)
	writeFile(t, path, "#!/bin/sh\n"+script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return path
}

func TestRunNeedsACommand(t *testing.T) {
	e := newEnv(t)
	r := e.run("run")
	if r.code == 0 {
		t.Error("`agentswap run` with no command succeeded")
	}
	mustContain(t, r.out(), "Usage:", "run")
}

// The supervised child has to reach the proxy even on a machine where
// `agentswap install` was never run.
func TestRunSetsTheEnvironmentItself(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	cli := fakeCLI(t, dir, "claude", `env | grep ANTHROPIC > `+log+`
exit 0
`)

	e.mustRun("run", "--", cli, "do the thing")

	got := readFile(t, log)
	mustContain(t, got, "ANTHROPIC_BASE_URL="+"http://"+e.configAddr+"/anthropic", "child environment")
	mustContain(t, got, "ANTHROPIC_AUTH_TOKEN=", "child environment")
}

// The supervisor passes native Codex arguments through unchanged. Provider
// routing belongs in Codex's own configuration, not in an injected profile.
func TestRunLeavesCodexArgumentsNative(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := fakeCLI(t, dir, "codex", `echo "$@" > `+log+`
exit 0
`)

	e.mustRun("run", "--", cli, "exec", "fix the tests")

	if got := readFile(t, log); got != "exec fix the tests\n" {
		t.Fatalf("codex invocation = %q, want native arguments", got)
	}
}

func TestRunKeepsAProfileTheUserChose(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := fakeCLI(t, dir, "codex", `echo "$@" > `+log+`
exit 0
`)

	e.mustRun("run", "--", cli, "--profile", "mine", "exec")

	got := readFile(t, log)
	mustContain(t, got, "--profile mine", "codex invocation")
	mustNotContain(t, got, "agentswap", "codex invocation")
}

// The whole point of the supervisor: the pool runs dry for longer than a
// connection should be held, and the session continues by itself afterwards.
func TestRunResumesAfterTheQuotaWait(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	writeFile(t, filepath.Join(e.home, "config.json"),
		`{"addr":"`+e.configAddr+`","park":{"enabled":true,"buffer":"1s","max_hold":"1s"}}`)

	e.pool("only")
	e.upstream.handle(func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
	})
	e.serve()

	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	// First run: spend the pool, which makes the daemon write a resume ticket.
	// Second run: succeed, so the supervisor stops.
	cli := fakeCLI(t, dir, "claude", `echo "$@" >> `+log+`
if [ ! -f `+dir+`/fired ]; then
  touch `+dir+`/fired
  curl -s -X POST "$ANTHROPIC_BASE_URL/v1/messages" -d '{}' > /dev/null
  exit 1
fi
exit 0
`)

	r := e.run("run", "--max-resumes", "2", "--", cli, "refactor the parser")
	if r.code != 0 {
		t.Fatalf("run exited %d:\n%s", r.code, r.out())
	}

	lines := strings.Split(strings.TrimSpace(readFile(t, log)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the CLI ran %d times, want 2 (original then resume):\n%v", len(lines), lines)
	}
	if lines[0] != "refactor the parser" {
		t.Errorf("first invocation = %q", lines[0])
	}
	// Replaying the instruction would make the agent start over rather than
	// pick up where the quota ran out.
	if lines[1] != "--continue" {
		t.Errorf("resume invocation = %q, want --continue", lines[1])
	}
	mustContain(t, r.out(), "quota is spent", "run output")
}

// A CLI that exits for its own reasons is not something to resume.
func TestRunDoesNotResumeWithoutATicket(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := fakeCLI(t, dir, "claude", `echo "$@" >> `+log+`
exit 3
`)

	// A non-zero exit is normal when the pool runs dry; the ticket decides.
	e.run("run", "--", cli, "do the thing")

	if lines := strings.Split(strings.TrimSpace(readFile(t, log)), "\n"); len(lines) != 1 {
		t.Errorf("the CLI ran %d times, want 1: there was nothing to resume", len(lines))
	}
}

// A ticket left over from an earlier session would make the next run wait out
// a reset that has already happened.
func TestRunIgnoresAStaleTicket(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	writeFile(t, filepath.Join(e.home, "pending", "anthropic.json"),
		`{"lane":"anthropic","until":"2099-01-01T00:00:00Z","written_at":"2020-01-01T00:00:00Z"}`)

	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := fakeCLI(t, dir, "claude", `echo "$@" >> `+log+`
exit 1
`)

	e.run("run", "--", cli, "do the thing")

	if lines := strings.Split(strings.TrimSpace(readFile(t, log)), "\n"); len(lines) != 1 {
		t.Errorf("the CLI ran %d times, want 1: the ticket was from a previous run", len(lines))
	}
}

// Something we do not know how to resume is run once and left alone.
func TestRunLeavesAnUnknownCommandAlone(t *testing.T) {
	e := newEnv(t)
	e.pinAddr()
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := fakeCLI(t, dir, "some-other-tool", `echo "$@" >> `+log+`
exit 1
`)

	e.run("run", "--", cli, "arg")

	if lines := strings.Split(strings.TrimSpace(readFile(t, log)), "\n"); len(lines) != 1 {
		t.Errorf("the tool ran %d times, want 1", len(lines))
	}
}
