package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

	r := e.mustRun("teleport", "codex", "--from", "claude", "--latest", "--cwd", project)
	for _, want := range []string{"Created Codex session", "codex resume", "Claude Code " + sourceID + " -> Codex"} {
		mustContain(t, r.out(), want, "teleport output")
	}
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
	for _, want := range []string{"session_meta", project, "Inspect the parser", "call-e2e", "parser.go", "package parser", "The parser is ready."} {
		mustContain(t, contents.String(), want, "Codex rollout")
	}
}

func TestTeleportDryRunAndAmbiguity(t *testing.T) {
	e := newEnv(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeTeleportFixture(t, e, project, "22222222-2222-4222-8222-222222222222", "older")
	writeClaudeTeleportFixture(t, e, project, "33333333-3333-4333-8333-333333333333", "newer")

	ambiguous := e.run("teleport", "codex", "--from", "claude", "--cwd", project)
	if ambiguous.code == 0 {
		t.Fatalf("ambiguous teleport succeeded:\n%s", ambiguous.out())
	}
	mustContain(t, ambiguous.out(), "selection is ambiguous", "ambiguous teleport")

	dry := e.mustRun("teleport", "codex", "--from", "claude", "--latest", "--cwd", project, "--dry-run")
	mustContain(t, dry.out(), "Dry run succeeded", "dry run")
	mustContain(t, dry.out(), "Nothing was written", "dry run")
	rollouts, _ := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(rollouts) != 0 {
		t.Fatalf("dry run wrote Codex rollouts: %v", rollouts)
	}
}

func TestTeleportLaunchUsesExactResumeAndKeepsTargetOnFailure(t *testing.T) {
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
	r := e.runEnv(extra, "teleport", "codex", "--from", "claude", "--session", sourceID, "--cwd", project, "--launch")
	if r.code != 0 {
		t.Fatalf("launch teleport exited %d:\n%s", r.code, r.out())
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, capture)), "\n")
	if len(lines) != 3 || lines[1] != "<resume>" {
		t.Fatalf("launch capture = %#v, want cwd, resume, exact id", lines)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != canonical {
		t.Fatalf("launch cwd = %q, want %q", lines[0], canonical)
	}
	targetID := strings.TrimSuffix(strings.TrimPrefix(lines[2], "<"), ">")
	for _, want := range []string{"Created Codex session " + targetID, "Resume: codex resume " + targetID} {
		mustContain(t, r.out(), want, "launch teleport")
	}

	failed := e.runEnv(append(extra, "FAKE_LAUNCH_EXIT=7"), "teleport", "codex", "--from", "claude", "--session", sourceID, "--cwd", project, "--launch")
	if failed.code == 0 {
		t.Fatalf("failed target launch succeeded:\n%s", failed.out())
	}
	for _, want := range []string{"Created Codex session", "teleported session was kept", "exit status 7"} {
		mustContain(t, failed.out(), want, "failed target launch")
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

	selected := e.runEnv([]string{"CLAUDE_SESSION_ID=" + firstID}, "teleport", "codex", "--from", "claude", "--cwd", project)
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
		{name: "same source and target", args: []string{"teleport", "claude", "--from", "claude", "--cwd", project}, want: "source and target agents are the same"},
		{name: "launch dry run", args: []string{"teleport", "codex", "--from", "claude", "--latest", "--cwd", project, "--launch", "--dry-run"}, want: "--launch cannot be combined"},
		{name: "unknown target", args: []string{"teleport", "other", "--cwd", project}, want: "unknown agent"},
		{name: "unexpected argument", args: []string{"teleport", "codex", "extra", "--cwd", project}, want: "unexpected arguments"},
		{name: "cwd missing", args: []string{"teleport", "codex", "--cwd", filepath.Join(project, "missing")}, want: "working directory"},
		{name: "cwd not directory", args: []string{"teleport", "codex", "--cwd", notDir}, want: "is not a directory"},
		{name: "session absent in cwd", args: []string{"teleport", "codex", "--from", "claude", "--session", "missing", "--cwd", project}, want: "was not found in the current working directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.run(tc.args...)
			if r.code == 0 {
				t.Fatalf("invalid invocation succeeded:\n%s", r.out())
			}
			mustContain(t, r.out(), tc.want, tc.name)
		})
	}
}
