package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentDefinitionCLI(t *testing.T) {
	e := newEnv(t)
	project := t.TempDir()
	source := filepath.Join(project, ".claude", "agents")
	writeFile(t, filepath.Join(source, "reviewer.md"), "---\nname: reviewer\ndescription: Review code\ntools: [Read, Grep]\nmodel: sonnet\n---\nReview carefully.\n")
	target := filepath.Join(project, ".codex", "agents")
	r := e.mustRun("agents", "claude", "codex", "--cwd", project, "--dry-run")
	mustContain(t, r.out(), "Would create 1 Codex agent definitions", "dry run")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("dry run created target")
	}
	r = e.mustRun("agents", "claude", "codex", "--cwd", project, "--model", "gpt-5.4")
	mustContain(t, r.out(), "Created 1 Codex agent definitions", "agent conversion")
	config := readFile(t, filepath.Join(target, "reviewer.toml"))
	mustContain(t, config, "gpt-5.4", "target model")
	mustContain(t, config, "Review carefully.", "target prompt")
	mustContain(t, config, "read-only", "policy fallback")
	r = e.run("agents", "claude", "codex", "--cwd", project)
	if r.code == 0 {
		t.Fatal("overwrote installed agent")
	}
	mustContain(t, r.out(), "refusing to overwrite", "collision")
	target = filepath.Join(project, "strict-agents")
	r = e.run("agents", "claude", "codex", "--cwd", project, "--target-dir", target, "--strict")
	if r.code == 0 {
		t.Fatal("strict conversion accepted warnings")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("strict conversion wrote artifacts")
	}
	e.mustRun("agents", "--help")
}

func TestClaudeCodexSubagentHandoffRoundTrip(t *testing.T) {
	e := newEnv(t)
	project := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	main := writeClaudeTeleportFixture(t, e, project, id, "Inspect parser")
	subdir := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents")
	writeFile(t, filepath.Join(subdir, "agent-abc.jsonl"), `{"type":"user","uuid":"su","isSidechain":true,"agentId":"abc","message":{"role":"user","content":"Inspect the tokenizer"}}`+"\n"+`{"type":"assistant","uuid":"sa","isSidechain":true,"agentId":"abc","message":{"role":"assistant","content":"The tokenizer has a boundary bug."}}`+"\n")
	writeFile(t, filepath.Join(subdir, "agent-abc.meta.json"), `{"agentType":"explorer","description":"Inspect tokenizer","toolUseId":"call-e2e"}`)
	before := readFile(t, main)
	subBefore := readFile(t, filepath.Join(subdir, "agent-abc.jsonl"))
	r := e.mustRun("teleport", "claude", "codex", "--cwd", project)
	mustContain(t, r.out(), "Delegated agent runs: 1", "branch count")
	rollouts, _ := filepath.Glob(filepath.Join(e.codex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(rollouts) != 2 {
		t.Fatalf("rollouts = %v", rollouts)
	}
	var parentID string
	for _, path := range rollouts {
		first := strings.SplitN(readFile(t, path), "\n", 2)[0]
		var record struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(first), &record); err != nil {
			t.Fatal(err)
		}
		if record.Payload["parent_thread_id"] == nil {
			parentID, _ = record.Payload["id"].(string)
		}
	}
	if parentID == "" {
		t.Fatal("no parent rollout")
	}
	r = e.mustRun("teleport", "codex", "claude", "--session", parentID, "--cwd", project)
	mustContain(t, r.out(), "Delegated agent runs: 1", "return branch count")
	subs, _ := filepath.Glob(filepath.Join(e.claude, "projects", "*", "*", "subagents", "*.jsonl"))
	if len(subs) != 2 {
		t.Fatalf("subagent transcripts = %v", subs)
	}
	var found bool
	for _, path := range subs {
		if path != filepath.Join(subdir, "agent-abc.jsonl") {
			mustContain(t, readFile(t, path), "boundary bug", "branch history")
			found = true
		}
	}
	if !found {
		t.Fatal("no transferred Claude branch")
	}
	if readFile(t, main) != before || readFile(t, filepath.Join(subdir, "agent-abc.jsonl")) != subBefore {
		t.Fatal("source history changed")
	}
}
