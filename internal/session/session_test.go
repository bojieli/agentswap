package session

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func isolatedHomes(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(root, "kimi-code"))
	t.Setenv("KIMI_SHARE_DIR", filepath.Join(root, "kimi-legacy"))
	t.Setenv("AGENTSWAP_KIMI_MODEL", "test/kimi")
	return root
}

func TestKimiResumeModel(t *testing.T) {
	t.Run("explicit override", func(t *testing.T) {
		isolatedHomes(t)
		t.Setenv("AGENTSWAP_KIMI_MODEL", " custom/model ")
		if got, err := kimiResumeModel(); err != nil || got != "custom/model" {
			t.Fatalf("kimiResumeModel = %q, %v", got, err)
		}
	})
	t.Run("config default", func(t *testing.T) {
		isolatedHomes(t)
		t.Setenv("AGENTSWAP_KIMI_MODEL", "")
		if err := os.MkdirAll(kimiCodeRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		config := "default_model = 'kimi-code/k3' # selected target model\n[models]\n"
		if err := os.WriteFile(filepath.Join(kimiCodeRoot(), "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := kimiResumeModel(); err != nil || got != "kimi-code/k3" {
			t.Fatalf("kimiResumeModel = %q, %v", got, err)
		}
	})
	t.Run("missing default", func(t *testing.T) {
		isolatedHomes(t)
		t.Setenv("AGENTSWAP_KIMI_MODEL", "")
		if err := os.MkdirAll(kimiCodeRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kimiCodeRoot(), "config.toml"), []byte("[models]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := kimiResumeModel(); err == nil || !strings.Contains(err.Error(), "AGENTSWAP_KIMI_MODEL") {
			t.Fatalf("missing default error = %v", err)
		}
	})
}

func sampleHistory(t *testing.T, cwd string) *Session {
	t.Helper()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{
		Source: Claude, SourceID: "source-session", CWD: cwd,
		Title: "Teleport fixture", Model: "claude-sonnet-4-6", CreatedAt: base, UpdatedAt: base.Add(5 * time.Minute),
		Events: []Event{
			{Kind: Message, ID: "user-1", Role: "user", Timestamp: base, Parts: []Part{{Kind: Text, Text: "Please inspect the parser."}}},
			{Kind: Message, ID: "assistant-1", ParentID: "user-1", Role: "assistant", Timestamp: base.Add(time.Second), Parts: []Part{
				{Kind: Text, Text: "I will inspect it."},
				{Kind: ToolCall, ID: "native-call", CallID: "call-1", ToolName: "Read", Data: json.RawMessage(`{"file_path":"parser.go"}`)},
			}},
			{Kind: Message, Role: "tool", Timestamp: base.Add(2 * time.Second), Parts: []Part{{Kind: ToolResult, CallID: "call-1", Text: "package parser", Error: true}}},
			{Kind: Message, Role: "assistant", Timestamp: base.Add(3 * time.Second), Parts: []Part{{Kind: Reasoning, Text: "The parser has one branch."}, {Kind: Text, Text: "The parser is small."}}},
			{Kind: Plan, Role: "assistant", Timestamp: base.Add(4 * time.Second), PlanText: "1. Add a test\n2. Fix the parser"},
		},
	}
	if err := history.Validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	return history
}

func TestParseAgentAliases(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]Agent{
		"claude-code": Claude, "Codex": Codex, "open-code": OpenCode, "kimi-code-cli": Kimi,
	} {
		got, err := ParseAgent(input)
		if err != nil || got != want {
			t.Errorf("ParseAgent(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseAgent("unknown"); err == nil {
		t.Fatal("unknown agent was accepted")
	}
}

func TestValidateToolRelationships(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	history := sampleHistory(t, cwd)
	history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Parts: []Part{{Kind: ToolCall, CallID: "pending", ToolName: "Bash", Data: json.RawMessage(`{}`)}}})
	if err := history.Validate(); err != nil {
		t.Fatalf("dangling call should be recoverable: %v", err)
	}
	if !strings.Contains(strings.Join(history.Warnings, "\n"), "pending") {
		t.Fatal("dangling call was not reported")
	}
	history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Parts: []Part{{Kind: ToolResult, CallID: "ghost", Text: "no"}}})
	if err := history.Validate(); err == nil || !strings.Contains(err.Error(), "matching call") {
		t.Fatalf("orphan result error = %v", err)
	}
}

func TestValidateRejectsEmptyContentAndOutOfOrderResults(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	empty := sampleHistory(t, cwd)
	empty.Events = append(empty.Events, Event{Kind: Message, Role: "assistant", Parts: []Part{{Kind: Text}}})
	if err := empty.Validate(); err == nil || !strings.Contains(err.Error(), "empty text") {
		t.Fatalf("empty content error = %v", err)
	}
	outOfOrder := &Session{Source: Claude, SourceID: "source", CWD: cwd, Events: []Event{
		{Kind: Message, Role: "tool", Parts: []Part{{Kind: ToolResult, CallID: "later", Text: "early"}}},
		{Kind: Message, Role: "assistant", Parts: []Part{{Kind: ToolCall, CallID: "later", ToolName: "Read", Data: json.RawMessage(`{}`)}}},
	}}
	if err := outOfOrder.Validate(); err == nil || !strings.Contains(err.Error(), "before its matching call") {
		t.Fatalf("out-of-order result error = %v", err)
	}
}

func TestSelectPrecedenceAndAmbiguity(t *testing.T) {
	t.Parallel()
	old := Candidate{Agent: Claude, ID: "same", UpdatedAt: time.Unix(1, 0)}
	newest := Candidate{Agent: Codex, ID: "new", UpdatedAt: time.Unix(3, 0)}
	mid := Candidate{Agent: Kimi, ID: "mid", UpdatedAt: time.Unix(2, 0)}
	candidates := []Candidate{old, newest, mid}
	SortCandidates(candidates)
	if got, err := Select(candidates, "mid", false); err != nil || got.ID != "mid" {
		t.Fatalf("explicit selection = %+v, %v", got, err)
	}
	if got, err := Select(candidates, "", true); err != nil || got.ID != "new" {
		t.Fatalf("latest selection = %+v, %v", got, err)
	}
	if _, err := Select(candidates, "", false); err == nil {
		t.Fatal("ambiguous selection succeeded")
	} else {
		var ambiguous *AmbiguousError
		if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 3 {
			t.Fatalf("ambiguity error = %T %v", err, err)
		}
	}
}

func TestClaudeRoundTrip(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	adapter := claudeAdapter{}
	result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("target session: %v", err)
	}
	candidates, err := adapter.Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover = %d candidates, %v", len(candidates), err)
	}
	assertRoundTrip(t, adapter, candidates[0])
}

func TestClaudeDiscoveryMatchesDifferentSymlinkAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}
	root := isolatedHomes(t)
	realCWD := t.TempDir()
	links := t.TempDir()
	first := filepath.Join(links, "first")
	second := filepath.Join(links, "second")
	if err := os.Symlink(realCWD, first); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCWD, second); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "claude", "projects", encodeClaudeProject(first))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"type": "user", "uuid": "u", "cwd": first, "message": map[string]any{"role": "user", "content": "hello"}}
	b, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := (claudeAdapter{}).Discover(context.Background(), second)
	if err != nil || len(candidates) != 1 || candidates[0].ID != id {
		t.Fatalf("symlink discovery = %+v, %v", candidates, err)
	}
}

func TestClaudeProjectEncodingMatchesNativePunctuationRules(t *testing.T) {
	t.Parallel()
	got := encodeClaudeProject("/private/tmp/agentswap-live.ZTiyOg/project_name")
	want := "-private-tmp-agentswap-live-ZTiyOg-project-name"
	if got != want {
		t.Fatalf("encodeClaudeProject = %q, want %q", got, want)
	}
}

func TestClaudeReaderPreservesQueuedPromptsAndPlanAttachments(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "claude", "projects", encodeClaudeProject(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	records := []map[string]any{
		{"type": "user", "uuid": "u", "cwd": cwd, "message": map[string]any{"role": "user", "content": "start"}},
		{"type": "queue-operation", "operation": "enqueue", "timestamp": "2026-08-19T10:00:00Z", "content": "queued request"},
		{"type": "queue-operation", "operation": "remove", "content": "queued request"},
		{"type": "attachment", "uuid": "q", "parentUuid": "u", "timestamp": "2026-08-19T10:00:01Z", "attachment": map[string]any{"type": "queued_command", "prompt": "queued request"}},
		{"type": "attachment", "uuid": "p", "timestamp": "2026-08-19T10:00:02Z", "attachment": map[string]any{"type": "plan_file_reference", "planContent": "revision one"}},
		{"type": "attachment", "uuid": "b", "timestamp": "2026-08-19T10:00:03Z", "attachment": map[string]any{"type": "budget_usd", "used": 0.1, "total": 1.0, "remaining": 0.9}},
	}
	var lines []byte
	for _, record := range records {
		line, _ := json.Marshal(record)
		lines = append(lines, append(line, '\n')...)
	}
	if err := os.WriteFile(path, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	history, err := (claudeAdapter{}).Read(context.Background(), Candidate{Agent: Claude, ID: id, CWD: cwd, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(history.Events)
	if !bytesContainAll(encoded, []string{"queued request", "revision one"}) || strings.Count(string(encoded), "queued request") != 1 {
		t.Fatalf("queued prompt or plan was not preserved exactly once: %s", encoded)
	}
}

func TestClaudeReaderRejectsSubagentTranscripts(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "claude", "projects", encodeClaudeProject(cwd))
	if err := os.MkdirAll(filepath.Join(dir, id, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	record := `{"type":"user","uuid":"u","cwd":` + quoteJSON(cwd) + `,"message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "subagents", "agent-0.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (claudeAdapter{}).Read(context.Background(), Candidate{Agent: Claude, ID: id, CWD: cwd, Path: path})
	if err == nil || !strings.Contains(err.Error(), "subagent") {
		t.Fatalf("subagent read error = %v", err)
	}
}

func TestCodexRoundTrip(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	adapter := codexAdapter{}
	result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Path, result.ID+".jsonl") {
		t.Fatalf("Codex filename does not end in the resumable UUID: %s", result.Path)
	}
	candidates, err := adapter.Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover = %d candidates, %v", len(candidates), err)
	}
	assertRoundTrip(t, adapter, candidates[0])
}

func TestKimiCodeRoundTrip(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	adapter := kimiAdapter{}
	result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalPath(cwd)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := kimiWorkdirKey(canonical)
	if !strings.Contains(result.Path, wantKey) {
		t.Fatalf("Kimi path %q does not contain %q", result.Path, wantKey)
	}
	candidates, err := adapter.Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover = %d candidates, %v", len(candidates), err)
	}
	if candidates[0].Format != "kimi-code-wire" {
		t.Fatalf("format = %q", candidates[0].Format)
	}
	stateBytes, err := os.ReadFile(filepath.Join(result.Path, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatal(err)
	}
	if state["workDir"] == nil || state["cwd"] != nil || state["version"] != nil {
		t.Fatalf("current Kimi state schema = %#v", state)
	}
	if got := strings.Join(result.Resume, " "); !strings.Contains(got, "--model test/kimi") {
		t.Fatalf("current Kimi resume command = %v", result.Resume)
	}
	assertRoundTrip(t, adapter, candidates[0])
}

func TestKimiCodeRejectsUnrepresentableSubagentHistory(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	adapter := kimiAdapter{}
	result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(result.Path, "agents", "agent-0"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Read(context.Background(), candidateForResult(Kimi, result, cwd))
	if err == nil || !strings.Contains(err.Error(), "subagent") {
		t.Fatalf("subagent read error = %v", err)
	}
}

func TestKimiLegacyRoundTrip(t *testing.T) {
	root := isolatedHomes(t)
	t.Setenv("KIMI_CODE_HOME", filepath.Join(root, "modern-empty"))
	t.Setenv("KIMI_SHARE_DIR", filepath.Join(root, "legacy-only"))
	t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
	cwd := t.TempDir()
	adapter := kimiAdapter{}
	result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resume[1] != "-r" {
		t.Fatalf("legacy resume command = %v", result.Resume)
	}
	candidates, err := adapter.Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover = %d candidates, %v", len(candidates), err)
	}
	if candidates[0].Format != "kimi-cli-v1" {
		t.Fatalf("format = %q", candidates[0].Format)
	}
	assertRoundTrip(t, adapter, candidates[0])
}

func TestKimiLegacyDiscoveryMatchesSymlinkRegistryPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}
	root := isolatedHomes(t)
	realCWD := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realCWD, alias); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"work_dirs": []any{map[string]any{"path": alias, "kaos": "local", "last_session_id": "legacy"}}}
	if err := os.MkdirAll(filepath.Join(root, "kimi-legacy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(root, "kimi-legacy", "kimi.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := md5.Sum([]byte(alias))
	sessionDir := filepath.Join(root, "kimi-legacy", "sessions", hex.EncodeToString(hash[:]), "legacy")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "wire.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := (kimiAdapter{}).Discover(context.Background(), realCWD)
	if err != nil || len(candidates) != 1 || candidates[0].ID != "legacy" {
		t.Fatalf("legacy symlink discovery = %+v, %v", candidates, err)
	}
}

func TestKimiTargetsPreserveMixedUserResultOrdering(t *testing.T) {
	for _, format := range []string{"modern", "legacy"} {
		t.Run(format, func(t *testing.T) {
			isolatedHomes(t)
			t.Setenv("AGENTSWAP_KIMI_FORMAT", format)
			cwd := t.TempDir()
			history := sampleHistory(t, cwd)
			history.Events[2] = Event{Kind: Message, Role: "user", Timestamp: history.Events[2].Timestamp, Parts: []Part{
				{Kind: ToolResult, CallID: "call-1", Text: "package parser", Error: true},
				{Kind: Text, Text: "Now fix it."},
			}}
			if err := history.Validate(); err != nil {
				t.Fatal(err)
			}
			adapter := kimiAdapter{}
			result, err := adapter.Write(context.Background(), history, WriteOptions{CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			candidate := candidateForResult(Kimi, result, cwd)
			if format == "legacy" {
				candidate.Format = "kimi-cli-v1"
			}
			got, err := adapter.Read(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(got.Events)
			if !bytesContainAll(encoded, []string{"call-1", "package parser", "Now fix it.", `"error":true`}) {
				t.Fatalf("mixed user result was not preserved: %s", encoded)
			}
		})
	}
}

func TestCrossAdapterMatrix(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	adapters := []Adapter{claudeAdapter{}, codexAdapter{}, kimiAdapter{}}
	for _, sourceAdapter := range adapters {
		sourceAdapter := sourceAdapter
		t.Run(string(sourceAdapter.Agent())+"-source", func(t *testing.T) {
			native, err := sourceAdapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			sourceCandidate := candidateForResult(sourceAdapter.Agent(), native, cwd)
			history, err := sourceAdapter.Read(context.Background(), sourceCandidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, targetAdapter := range adapters {
				if targetAdapter.Agent() == sourceAdapter.Agent() {
					continue
				}
				t.Run("to-"+string(targetAdapter.Agent()), func(t *testing.T) {
					result, err := targetAdapter.Write(context.Background(), history, WriteOptions{CWD: cwd})
					if err != nil {
						t.Fatal(err)
					}
					assertRoundTrip(t, targetAdapter, candidateForResult(targetAdapter.Agent(), result, cwd))
				})
			}
		})
	}
}

func TestMultiplePlanRevisionsSurviveFileBackedTargets(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	for _, adapter := range []Adapter{claudeAdapter{}, kimiAdapter{}} {
		t.Run(string(adapter.Agent()), func(t *testing.T) {
			history := sampleHistory(t, cwd)
			history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: history.UpdatedAt, PlanText: "Revised plan\n1. Keep both revisions"})
			result, err := adapter.Write(context.Background(), history, WriteOptions{CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			got, err := adapter.Read(context.Background(), candidateForResult(adapter.Agent(), result, cwd))
			if err != nil {
				t.Fatal(err)
			}
			var plans []string
			for _, event := range got.Events {
				if event.Kind == Plan {
					plans = append(plans, event.PlanText)
				}
			}
			if len(plans) != 2 || !strings.Contains(plans[0], "Fix the parser") || !strings.Contains(plans[1], "Revised plan") {
				t.Fatalf("plan revisions = %#v", plans)
			}
		})
	}
}

func candidateForResult(agent Agent, result Result, cwd string) Candidate {
	format := map[Agent]string{Claude: "claude-jsonl", Codex: "codex-rollout", Kimi: "kimi-code-wire"}[agent]
	return Candidate{Agent: agent, ID: result.ID, CWD: cwd, Path: result.Path, Format: format, UpdatedAt: time.Now()}
}

func assertRoundTrip(t *testing.T, adapter Adapter, candidate Candidate) {
	t.Helper()
	got, err := adapter.Read(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("round-trip history is invalid: %v", err)
	}
	encoded, err := json.Marshal(got.Events)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"Please inspect the parser.", "call-1", "parser.go", "package parser", "Fix the parser"} {
		if !strings.Contains(text, want) {
			t.Errorf("round-trip is missing %q: %s", want, text)
		}
	}
	if !strings.Contains(text, `"call_id":"call-1","text":"package parser","error":true`) {
		t.Errorf("round-trip lost tool error state: %s", text)
	}
}

func TestClaudeReaderFailsClosedOnMedia(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "claude", "projects", encodeClaudeProject(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := `{"type":"user","uuid":"u","sessionId":"` + id + `","cwd":` + quoteJSON(cwd) + `,"message":{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}}` + "\n"
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (claudeAdapter{}).Read(context.Background(), Candidate{Agent: Claude, ID: id, CWD: cwd, Path: path})
	if err == nil || !strings.Contains(err.Error(), "unsupported Claude conversation block") {
		t.Fatalf("media read error = %v", err)
	}
}

func TestReadersRejectCorruptJSONL(t *testing.T) {
	isolatedHomes(t)
	path := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (claudeAdapter{}).Read(context.Background(), Candidate{Agent: Claude, ID: "broken", CWD: t.TempDir(), Path: path})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("corrupt JSONL error = %v", err)
	}
}

func TestReadJSONLRejectsOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 1<<20)
	for written := 0; written <= maxJSONLRecord; written += len(chunk) {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = readJSONL(path, func(int, json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 MiB") {
		t.Fatalf("oversized record error = %v", err)
	}
}

func TestCodexReaderFailsClosedOnMedia(t *testing.T) {
	isolate := isolatedHomes(t)
	cwd := t.TempDir()
	id := "77777777-7777-4777-8777-777777777777"
	path := filepath.Join(isolate, "codex-media.jsonl")
	records := []map[string]any{
		{"timestamp": "2026-08-19T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}},
		{"timestamp": "2026-08-19T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}}}},
	}
	var body strings.Builder
	for _, record := range records {
		b, _ := json.Marshal(record)
		body.Write(b)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (codexAdapter{}).Read(context.Background(), Candidate{Agent: Codex, ID: id, CWD: cwd, Path: path})
	if err == nil || !strings.Contains(err.Error(), "unsupported Codex image content") {
		t.Fatalf("media read error = %v", err)
	}
}

func TestOpenCodeReaderFailsClosedOnAttachmentsAndDelegation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake OpenCode executable is a POSIX shell script")
	}
	for _, kind := range []string{"file", "agent", "subtask", "future-conversation-part"} {
		t.Run(kind, func(t *testing.T) {
			root := isolatedHomes(t)
			cwd := t.TempDir()
			exportPath := filepath.Join(root, "export.json")
			exported := map[string]any{
				"info": map[string]any{"id": "ses_source", "directory": cwd},
				"messages": []any{map[string]any{
					"info":  map[string]any{"id": "msg", "role": "user"},
					"parts": []any{map[string]any{"id": "part", "type": kind}},
				}},
			}
			b, _ := json.Marshal(exported)
			if err := os.WriteFile(exportPath, b, 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(root, "opencode")
			body := "#!/bin/sh\nset -eu\nif [ \"$1\" = export ]; then cat \"$FAKE_EXPORT\"; exit 0; fi\nexit 2\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTSWAP_OPENCODE_BIN", script)
			t.Setenv("FAKE_EXPORT", exportPath)
			_, err := (openCodeAdapter{}).Read(context.Background(), Candidate{Agent: OpenCode, ID: "ses_source", CWD: cwd, Format: "opencode-export"})
			if err == nil || !strings.Contains(err.Error(), kind) {
				t.Fatalf("OpenCode %s read error = %v", kind, err)
			}
		})
	}
}

func TestKimiReadersFailClosedOnMedia(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	history := sampleHistory(t, cwd)
	t.Run("modern", func(t *testing.T) {
		t.Setenv("AGENTSWAP_KIMI_FORMAT", "modern")
		result, err := (kimiAdapter{}).Write(context.Background(), history, WriteOptions{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		wire := filepath.Join(result.Path, "agents", "main", "wire.jsonl")
		f, err := os.OpenFile(wire, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = writeJSONLine(f, map[string]any{"type": "context.append_message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "url": "data:image/png;base64,AA=="}}}})
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, err = (kimiAdapter{}).Read(context.Background(), Candidate{Agent: Kimi, ID: result.ID, CWD: cwd, Path: result.Path, Format: "kimi-code-wire"})
		if err == nil || !strings.Contains(err.Error(), "unsupported Kimi context block") {
			t.Fatalf("modern Kimi media error = %v", err)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
		result, err := (kimiAdapter{}).Write(context.Background(), history, WriteOptions{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		contextPath := filepath.Join(result.Path, "context.jsonl")
		f, err := os.OpenFile(contextPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = writeJSONLine(f, map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "url": "data:image/png;base64,AA=="}}})
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, err = (kimiAdapter{}).Read(context.Background(), Candidate{Agent: Kimi, ID: result.ID, CWD: cwd, Path: result.Path, Format: "kimi-cli-v1"})
		if err == nil || !strings.Contains(err.Error(), "unsupported legacy Kimi content block") {
			t.Fatalf("legacy Kimi media error = %v", err)
		}
	})
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestLargeMixedHistoryRoundTripsFileTargets(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	history := largeMixedHistory(t, cwd, 250)
	targets := []struct {
		name    string
		adapter Adapter
		format  string
		legacy  bool
	}{
		{name: "claude", adapter: claudeAdapter{}, format: "claude-jsonl"},
		{name: "codex", adapter: codexAdapter{}, format: "codex-rollout"},
		{name: "kimi-modern", adapter: kimiAdapter{}, format: "kimi-code-wire"},
		{name: "kimi-legacy", adapter: kimiAdapter{}, format: "kimi-cli-v1", legacy: true},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			if target.legacy {
				t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
			} else {
				t.Setenv("AGENTSWAP_KIMI_FORMAT", "modern")
			}
			result, err := target.adapter.Write(context.Background(), history, WriteOptions{CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			got, err := target.adapter.Read(context.Background(), Candidate{Agent: target.adapter.Agent(), ID: result.ID, CWD: cwd, Path: result.Path, Format: target.format})
			if err != nil {
				t.Fatal(err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("round-trip validation: %v", err)
			}
			var calls, results, plans int
			encoded, _ := json.Marshal(got.Events)
			for _, event := range got.Events {
				if event.Kind == Plan {
					plans++
				}
				for _, part := range event.Parts {
					switch part.Kind {
					case ToolCall:
						calls++
					case ToolResult:
						results++
					}
				}
			}
			if calls != 251 || results != 251 || plans != 5 {
				t.Fatalf("round-trip counts: calls=%d results=%d plans=%d", calls, results, plans)
			}
			if !bytesContainAll(encoded, []string{"large-user-000-界", "large-user-249-界", "mcp__acceptance__tool", "result-249-λ", "dangling-large", "interrupted before teleport", "large plan revision 200"}) {
				t.Fatalf("large round-trip lost boundary content; bytes=%d", len(encoded))
			}
		})
	}
}

func largeMixedHistory(t *testing.T, cwd string, turns int) *Session {
	t.Helper()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{Source: Claude, SourceID: "large-source", CWD: cwd, Title: "Large mixed acceptance", Model: "acceptance/model", CreatedAt: base}
	for i := 0; i < turns; i++ {
		callID := fmt.Sprintf("large-call-%03d", i)
		ts := base.Add(time.Duration(i*5) * time.Second)
		history.Events = append(history.Events,
			Event{Kind: Message, ID: fmt.Sprintf("u-%03d", i), Role: "user", Timestamp: ts, Parts: []Part{{Kind: Text, Text: fmt.Sprintf("large-user-%03d-界\n%s", i, strings.Repeat("payload ", 12))}}},
			Event{Kind: Message, ID: fmt.Sprintf("a-%03d", i), Role: "assistant", Timestamp: ts.Add(time.Second), Parts: []Part{
				{Kind: Reasoning, Text: fmt.Sprintf("recorded reasoning %03d", i)},
				{Kind: ToolCall, CallID: callID, ToolName: "mcp__acceptance__tool", Data: json.RawMessage(fmt.Sprintf(`{"turn":%d,"nested":{"unicode":"λ"}}`, i))},
			}},
			Event{Kind: Message, Role: "tool", Timestamp: ts.Add(2 * time.Second), Parts: []Part{{Kind: ToolResult, CallID: callID, Text: fmt.Sprintf("result-%03d-λ", i), Error: i%7 == 0}}},
			Event{Kind: Message, Role: "assistant", Timestamp: ts.Add(3 * time.Second), Parts: []Part{{Kind: Text, Text: fmt.Sprintf("finished-%03d", i)}}},
		)
		if i%50 == 0 {
			history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: ts.Add(4 * time.Second), PlanText: fmt.Sprintf("large plan revision %03d\n1. keep history\n2. continue", i)})
		}
	}
	history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: base.Add(time.Duration(turns*5) * time.Second), Parts: []Part{{Kind: ToolCall, CallID: "dangling-large", ToolName: "Bash", Data: json.RawMessage(`{"command":"sleep 100"}`)}}})
	history.UpdatedAt = history.Events[len(history.Events)-1].Timestamp
	if err := history.Validate(); err != nil {
		t.Fatalf("large fixture validation: %v", err)
	}
	return history
}

func TestKimiLegacyRollsBackPublishedSessionWhenMetadataIsInvalid(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
	legacyRoot := filepath.Join(root, "kimi-legacy")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(legacyRoot, "kimi.json")
	if err := os.WriteFile(metadata, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (kimiAdapter{}).Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd}); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("invalid metadata write error = %v", err)
	}
	var partial []string
	_ = filepath.WalkDir(filepath.Join(legacyRoot, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && path != filepath.Join(legacyRoot, "sessions") && (entry.IsDir() && (strings.HasPrefix(entry.Name(), ".agentswap-") || strings.Count(entry.Name(), "-") == 4)) {
			partial = append(partial, path)
		}
		return nil
	})
	if len(partial) != 0 {
		t.Fatalf("failed legacy publish left target artifacts: %v", partial)
	}
	if got, err := os.ReadFile(metadata); err != nil || string(got) != "{broken" {
		t.Fatalf("failed legacy publish changed metadata: %q, %v", got, err)
	}
}

func TestFileTargetsLeaveNoArtifactsWhenTheirRootIsNotADirectory(t *testing.T) {
	cwd := t.TempDir()
	for _, target := range []struct {
		name    string
		envKey  string
		adapter Adapter
		legacy  bool
	}{
		{name: "claude", envKey: "CLAUDE_CONFIG_DIR", adapter: claudeAdapter{}},
		{name: "codex", envKey: "CODEX_HOME", adapter: codexAdapter{}},
		{name: "kimi-modern", envKey: "KIMI_CODE_HOME", adapter: kimiAdapter{}},
		{name: "kimi-legacy", envKey: "KIMI_SHARE_DIR", adapter: kimiAdapter{}, legacy: true},
	} {
		t.Run(target.name, func(t *testing.T) {
			root := t.TempDir()
			blocked := filepath.Join(root, "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(target.envKey, blocked)
			t.Setenv("AGENTSWAP_KIMI_MODEL", "test/kimi")
			if target.legacy {
				t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
			} else {
				t.Setenv("AGENTSWAP_KIMI_FORMAT", "modern")
			}
			if _, err := target.adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd}); err == nil {
				t.Fatal("write unexpectedly succeeded with a file as its data root")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "blocked" || entries[0].IsDir() {
				t.Fatalf("failed write left target artifacts: %v", entries)
			}
		})
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := isolatedHomes(t)
	cwd := t.TempDir()
	for _, adapter := range []Adapter{claudeAdapter{}, codexAdapter{}, kimiAdapter{}, openCodeAdapter{}} {
		result, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd, DryRun: true})
		if err != nil {
			t.Fatalf("%s dry run: %v", adapter.Agent(), err)
		}
		if result.ID == "" || len(result.Resume) == 0 {
			t.Errorf("%s dry run returned incomplete result: %+v", adapter.Agent(), result)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		children, _ := os.ReadDir(filepath.Join(root, entry.Name()))
		if len(children) != 0 {
			t.Errorf("dry run wrote under %s", entry.Name())
		}
	}
}

func TestOpenCodeNativeCLIAdapter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake OpenCode executable is a POSIX shell script")
	}
	root := isolatedHomes(t)
	cwd := t.TempDir()
	exportPath := filepath.Join(root, "export.json")
	capturePath := filepath.Join(root, "imported.json")
	deletedPath := filepath.Join(root, "deleted.txt")
	exported := map[string]any{
		"info": map[string]any{"id": "ses_source", "title": "OpenCode source", "directory": cwd, "time": map[string]any{"created": 1_700_000_000_000, "updated": 1_700_000_001_000}},
		"messages": []any{
			map[string]any{"info": map[string]any{"id": "msg_u", "sessionID": "ses_source", "role": "user", "time": map[string]any{"created": 1_700_000_000_000}, "agent": "build", "model": map[string]any{"providerID": "openai", "modelID": "gpt"}}, "parts": []any{map[string]any{"id": "prt_u", "sessionID": "ses_source", "messageID": "msg_u", "type": "text", "text": "hello"}}},
			map[string]any{"info": map[string]any{"id": "msg_a", "sessionID": "ses_source", "role": "assistant", "time": map[string]any{"created": 1_700_000_000_100}, "parentID": "msg_u"}, "parts": []any{map[string]any{"id": "prt_t", "sessionID": "ses_source", "messageID": "msg_a", "type": "tool", "callID": "oc-call", "tool": "Bash", "state": map[string]any{"status": "completed", "input": map[string]any{"command": "pwd"}, "output": cwd, "title": "Bash", "metadata": map[string]any{}, "time": map[string]any{"start": 1, "end": 2}}}}},
		},
	}
	b, _ := json.Marshal(exported)
	if err := os.WriteFile(exportPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(root, "list.json")
	list, _ := json.Marshal([]any{map[string]any{"id": "ses_source", "title": "OpenCode source", "directory": cwd, "updated": 1_700_000_001_000}})
	if err := os.WriteFile(listPath, list, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "opencode")
	body := "#!/bin/sh\nset -eu\n" +
		"if [ \"$1 $2\" = \"session list\" ]; then cat \"$FAKE_LIST\"; exit 0; fi\n" +
		"if [ \"$1\" = export ]; then cat \"$FAKE_EXPORT\"; exit 0; fi\n" +
		"if [ \"$1\" = import ]; then cp \"$2\" \"$FAKE_CAPTURE\"; id=$(sed -n 's/.*\"id\": \"\\(ses_[^\"]*\\)\".*/\\1/p' \"$2\" | head -1); if [ \"${FAKE_FAIL:-}\" = 1 ]; then echo unconfirmed; else echo \"Imported session: $id\"; fi; exit 0; fi\n" +
		"if [ \"$1 $2\" = \"session delete\" ]; then echo \"$3\" > \"$FAKE_DELETED\"; exit 0; fi\n" +
		"exit 2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTSWAP_OPENCODE_BIN", script)
	t.Setenv("FAKE_LIST", listPath)
	t.Setenv("FAKE_EXPORT", exportPath)
	t.Setenv("FAKE_CAPTURE", capturePath)
	t.Setenv("FAKE_DELETED", deletedPath)
	adapter := openCodeAdapter{}
	candidates, err := adapter.Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover = %d, %v", len(candidates), err)
	}
	history, err := adapter.Read(context.Background(), candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, source := range []Agent{Claude, Codex, Kimi} {
		input := sampleHistory(t, cwd)
		input.Source = source
		result, err := adapter.Write(context.Background(), input, WriteOptions{CWD: cwd})
		if err != nil {
			t.Fatalf("%s to OpenCode: %v", source, err)
		}
		if !strings.HasPrefix(result.ID, "ses_") {
			t.Fatalf("import id = %q", result.ID)
		}
		captured, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytesContainAll(captured, []string{result.ID, "call-1", "package parser", "Teleport fixture"}) {
			t.Fatalf("import payload lost history: %s", captured)
		}
	}
	for _, target := range []Adapter{claudeAdapter{}, codexAdapter{}, kimiAdapter{}} {
		result, err := target.Write(context.Background(), history, WriteOptions{CWD: cwd})
		if err != nil {
			t.Fatalf("OpenCode to %s: %v", target.Agent(), err)
		}
		got, err := target.Read(context.Background(), candidateForResult(target.Agent(), result, cwd))
		if err != nil {
			t.Fatalf("read OpenCode->%s: %v", target.Agent(), err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("OpenCode->%s invalid: %v", target.Agent(), err)
		}
		encoded, _ := json.Marshal(got.Events)
		if !bytesContainAll(encoded, []string{"hello", "oc-call", "pwd"}) {
			t.Fatalf("OpenCode->%s lost history: %s", target.Agent(), encoded)
		}
	}
	t.Setenv("FAKE_FAIL", "1")
	if _, err := adapter.Write(context.Background(), sampleHistory(t, cwd), WriteOptions{CWD: cwd}); err == nil {
		t.Fatal("unconfirmed OpenCode import succeeded")
	}
	deleted, err := os.ReadFile(deletedPath)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(deleted)), "ses_") {
		t.Fatalf("rollback did not delete the generated id: %q, %v", deleted, err)
	}
}

func TestOpenCodeImportConfirmationRequiresExactID(t *testing.T) {
	t.Parallel()
	if !containsExactID("Imported session: ses_abc", "ses_abc") {
		t.Fatal("exact OpenCode id was not recognized")
	}
	for _, text := range []string{"Imported session: ses_abcdef", "Imported session: xses_abc", "no id"} {
		if containsExactID(text, "ses_abc") {
			t.Fatalf("non-exact OpenCode confirmation was accepted: %q", text)
		}
	}
}

func TestOpenCodeLargeMixedImportPayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake OpenCode executable is a POSIX shell script")
	}
	root := isolatedHomes(t)
	cwd := t.TempDir()
	capture := filepath.Join(root, "large-import.json")
	deleted := filepath.Join(root, "deleted.txt")
	script := filepath.Join(root, "opencode")
	body := "#!/bin/sh\nset -eu\n" +
		"if [ \"$1\" = import ]; then cp \"$2\" \"$FAKE_CAPTURE\"; id=$(sed -n 's/.*\"id\": \"\\(ses_[^\"]*\\)\".*/\\1/p' \"$2\" | head -1); echo \"Imported session: $id\"; exit 0; fi\n" +
		"if [ \"$1 $2\" = \"session delete\" ]; then echo \"$3\" > \"$FAKE_DELETED\"; exit 0; fi\n" +
		"exit 2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTSWAP_OPENCODE_BIN", script)
	t.Setenv("FAKE_CAPTURE", capture)
	t.Setenv("FAKE_DELETED", deleted)
	history := largeMixedHistory(t, cwd, 250)
	result, err := (openCodeAdapter{}).Write(context.Background(), history, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Info     map[string]any `json:"info"`
		Messages []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &exported); err != nil {
		t.Fatal(err)
	}
	if stringValue(exported.Info["id"]) != result.ID || len(exported.Messages) < 750 {
		t.Fatalf("large OpenCode payload id/messages = %q/%d", exported.Info["id"], len(exported.Messages))
	}
	var calls, interrupted int
	for _, message := range exported.Messages {
		for _, part := range message.Parts {
			if stringValue(part["type"]) != "tool" {
				continue
			}
			calls++
			if stringValue(part["callID"]) == "dangling-large" {
				state, _ := part["state"].(map[string]any)
				if stringValue(state["status"]) == "error" && strings.Contains(stringValue(state["error"]), "interrupted") {
					interrupted++
				}
			}
		}
	}
	if calls != 251 || interrupted != 1 || !bytesContainAll(b, []string{"large-user-000-界", "large-user-249-界", "result-249-λ", "large plan revision 200"}) {
		t.Fatalf("large OpenCode payload calls/interrupted/bytes = %d/%d/%d", calls, interrupted, len(b))
	}
}

func bytesContainAll(value []byte, wants []string) bool {
	for _, want := range wants {
		if !strings.Contains(string(value), want) {
			return false
		}
	}
	return true
}

func quoteJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
