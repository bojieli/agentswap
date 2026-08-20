package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func claudeProjectKey(path string) string {
	path = filepath.ToSlash(path)
	return strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(path)
}

func writeClaudeTeleportFixture(t *testing.T, e *env, cwd, id, prompt string) string {
	t.Helper()
	path := filepath.Join(e.claude, "projects", claudeProjectKey(cwd), id+".jsonl")
	lines := []map[string]any{
		{"type": "permission-mode", "permissionMode": "default", "sessionId": id},
		{
			"type": "user", "uuid": "u1", "parentUuid": nil, "sessionId": id, "cwd": cwd,
			"timestamp": "2026-08-19T10:00:00Z", "slug": "teleport-test",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}},
		},
		{
			"type": "assistant", "uuid": "a1", "parentUuid": "u1", "sessionId": id, "cwd": cwd,
			"timestamp": "2026-08-19T10:00:01Z", "slug": "teleport-test",
			"message": map[string]any{
				"id": "msg_a1", "role": "assistant", "model": "claude-sonnet-4-6",
				"content": []any{
					map[string]any{"type": "text", "text": "I will inspect it."},
					map[string]any{"type": "tool_use", "id": "call-e2e", "name": "Read", "input": map[string]any{"file_path": "parser.go"}},
				},
			},
		},
		{
			"type": "user", "uuid": "u2", "parentUuid": "a1", "sessionId": id, "cwd": cwd,
			"timestamp": "2026-08-19T10:00:02Z", "slug": "teleport-test",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-e2e", "content": "package parser"}}},
		},
		{
			"type": "assistant", "uuid": "a2", "parentUuid": "u2", "sessionId": id, "cwd": cwd,
			"timestamp": "2026-08-19T10:00:03Z", "slug": "teleport-test",
			"message": map[string]any{"id": "msg_a2", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{map[string]any{"type": "text", "text": "The parser is ready."}}},
		},
	}
	var body strings.Builder
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(b)
		body.WriteByte('\n')
	}
	writeFile(t, path, body.String())
	return path
}

func TestTeleportClaudeToCodex(t *testing.T) {
	e := newEnv(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceID := "11111111-1111-4111-8111-111111111111"
	source := writeClaudeTeleportFixture(t, e, project, sourceID, "Inspect the parser")
	sourceBefore := readFile(t, source)

	r := e.mustRun("teleport", "claude", "codex", "--cwd", project)
	for _, want := range []string{"Created Codex session", "codex resume", "Claude Code " + sourceID + " -> Codex"} {
		mustContain(t, r.out(), want, "teleport output")
	}
	mustNotContain(t, r.out(), "--profile", "teleport output")
	if got := readFile(t, source); got != sourceBefore {
		t.Fatal("teleport modified the source session")
	}

	rollouts, err := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil || len(rollouts) != 1 {
		t.Fatalf("Codex rollouts = %v, %v", rollouts, err)
	}
	f, err := os.Open(rollouts[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var contents strings.Builder
	for scanner.Scan() {
		contents.WriteString(scanner.Text())
		contents.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// canonicalPath deliberately compares Windows paths case-insensitively.
		// EvalSymlinks also expands 8.3 aliases on the hosted runner, so compare
		// the same canonical representation that the rollout records.
		canonical = strings.ToLower(canonical)
	}
	encodedCWD, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, contents.String(), `"cwd":`+string(encodedCWD), "Codex rollout cwd")
	for _, want := range []string{"session_meta", "Inspect the parser", "call-e2e", "parser.go", "package parser", "The parser is ready."} {
		mustContain(t, contents.String(), want, "Codex rollout")
	}
}

func TestTeleportDefaultsToLatestAndDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	oldID := "22222222-2222-4222-8222-222222222222"
	newID := "33333333-3333-4333-8333-333333333333"
	oldPath := writeClaudeTeleportFixture(t, e, project, oldID, "older")
	newPath := writeClaudeTeleportFixture(t, e, project, newID, "newer")
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	dry := e.mustRun("teleport", "claude", "codex", "--cwd", project, "--dry-run")
	mustContain(t, dry.out(), "Dry run succeeded", "dry run")
	mustContain(t, dry.out(), "Nothing was written", "dry run")
	mustContain(t, dry.out(), "Claude Code "+newID+" -> Codex", "latest selection")
	mustNotContain(t, dry.out(), "Claude Code "+oldID+" -> Codex", "latest selection")
	rollouts, _ := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(rollouts) != 0 {
		t.Fatalf("dry run wrote Codex rollouts: %v", rollouts)
	}
}

func TestHandoffUsesExactResumeAndKeepsTargetOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake target CLI is a POSIX shell script")
	}
	e := newEnv(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceID := "44444444-4444-4444-8444-444444444444"
	writeClaudeTeleportFixture(t, e, project, sourceID, "launch this exact session")

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "launch.txt")
	fakeCodex := filepath.Join(binDir, "codex")
	script := `#!/bin/sh
set -eu
{
  pwd -P
  for arg in "$@"; do printf '<%s>\n' "$arg"; done
} > "$FAKE_LAUNCH_CAPTURE"
exit "${FAKE_LAUNCH_EXIT:-0}"
`
	writeFile(t, fakeCodex, script)
	if err := os.Chmod(fakeCodex, 0o700); err != nil {
		t.Fatal(err)
	}
	extra := []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_LAUNCH_CAPTURE=" + capture}
	r := e.runEnv(extra, "handoff", "claude", "codex", "--session", sourceID, "--cwd", project,
		"--dangerously-bypass-approvals-and-sandbox", "continue here")
	if r.code != 0 {
		t.Fatalf("handoff exited %d:\n%s", r.code, r.out())
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, capture)), "\n")
	if len(lines) != 5 || lines[1] != "<resume>" ||
		lines[3] != "<--dangerously-bypass-approvals-and-sandbox>" || lines[4] != "<continue here>" {
		t.Fatalf("handoff capture = %#v, want cwd, resume, exact id, and unchanged target args", lines)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != canonical {
		t.Fatalf("launch cwd = %q, want %q", lines[0], canonical)
	}
	targetID := strings.TrimSuffix(strings.TrimPrefix(lines[2], "<"), ">")
	for _, want := range []string{
		"Created Codex session " + targetID,
		"Resume: codex resume " + targetID + " --dangerously-bypass-approvals-and-sandbox 'continue here'",
		"Launching: codex resume " + targetID + " --dangerously-bypass-approvals-and-sandbox 'continue here'",
	} {
		mustContain(t, r.out(), want, "handoff")
	}

	failed := e.runEnv(append(extra, "FAKE_LAUNCH_EXIT=7"), "handoff", "claude", "codex", "--session", sourceID, "--cwd", project)
	if failed.code == 0 {
		t.Fatalf("failed target launch succeeded:\n%s", failed.out())
	}
	for _, want := range []string{"Created Codex session", "teleported session was kept", "exit status 7"} {
		mustContain(t, failed.out(), want, "failed handoff launch")
	}
	rollouts, err := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil || len(rollouts) != 2 {
		t.Fatalf("Codex rollouts after failed launch = %v, %v; target should be retained", rollouts, err)
	}
}

func TestTeleportSelectionEnvironmentAndInputValidation(t *testing.T) {
	e := newEnv(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	firstID := "55555555-5555-4555-8555-555555555555"
	secondID := "66666666-6666-4666-8666-666666666666"
	writeClaudeTeleportFixture(t, e, project, firstID, "selected by active environment")
	writeClaudeTeleportFixture(t, e, project, secondID, "must not be selected")

	selected := e.runEnv([]string{"CLAUDE_SESSION_ID=" + firstID}, "teleport", "claude", "codex", "--cwd", project)
	if selected.code != 0 {
		t.Fatalf("active-session teleport exited %d:\n%s", selected.code, selected.out())
	}
	mustContain(t, selected.out(), "Claude Code "+firstID+" -> Codex", "active-session selection")
	mustNotContain(t, selected.out(), "Claude Code "+secondID+" -> Codex", "active-session selection")

	notDir := filepath.Join(t.TempDir(), "file")
	writeFile(t, notDir, "not a directory")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "source required", args: []string{"teleport", "codex"}, want: "source agent is required"},
		{name: "handoff needs both agents", args: []string{"handoff", "codex"}, want: "source and target agents are required"},
		{name: "same source and target", args: []string{"teleport", "claude", "claude", "--cwd", project}, want: "source and target agents are the same"},
		{name: "launch dry run", args: []string{"teleport", "claude", "codex", "--cwd", project, "--launch", "--dry-run"}, want: "--launch cannot be combined"},
		{name: "unknown source", args: []string{"teleport", "other", "codex", "--cwd", project}, want: "source: unknown agent"},
		{name: "unknown target", args: []string{"teleport", "claude", "other", "--cwd", project}, want: "target: unknown agent"},
		{name: "positional and from", args: []string{"teleport", "claude", "codex", "--from", "kimi", "--cwd", project}, want: "--from cannot be combined"},
		{name: "unexpected argument", args: []string{"teleport", "claude", "codex", "extra", "--cwd", project}, want: "unexpected arguments"},
		{name: "cwd missing", args: []string{"teleport", "claude", "codex", "--cwd", filepath.Join(project, "missing")}, want: "working directory"},
		{name: "cwd not directory", args: []string{"teleport", "claude", "codex", "--cwd", notDir}, want: "is not a directory"},
		{name: "session absent in cwd", args: []string{"teleport", "claude", "codex", "--session", "missing", "--cwd", project}, want: "was not found in the current working directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.run(tc.args...)
			if r.code == 0 {
				t.Fatalf("invalid invocation succeeded:\n%s", r.out())
			}
			mustContain(t, r.out(), tc.want, tc.name)
		})
	}
	rollouts, err := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil || len(rollouts) != 1 {
		t.Fatalf("invalid invocations changed Codex targets: %v, %v", rollouts, err)
	}

	legacy := e.mustRun("teleport", "codex", "--from", "claude", "--session", firstID, "--cwd", project, "--dry-run")
	mustContain(t, legacy.out(), "is deprecated", "legacy teleport syntax")
	mustContain(t, legacy.out(), "Claude Code "+firstID+" -> Codex", "legacy teleport syntax")
}
