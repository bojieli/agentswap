package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
