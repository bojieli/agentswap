package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/agentswap/internal/session"
)

func TestActiveSessionIDUsesExplicitSource(t *testing.T) {
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

func TestParseHandoffArgsPassesTargetArgumentsUnchanged(t *testing.T) {
	sourceID, cwd, targetArgs, err := parseHandoffArgs([]string{
		"--session", "source-id", "--cwd=/tmp/project",
		"--dangerously-bypass-approvals-and-sandbox", "continue here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sourceID != "source-id" || cwd != "/tmp/project" {
		t.Fatalf("handoff options = session %q cwd %q", sourceID, cwd)
	}
	want := []string{"--dangerously-bypass-approvals-and-sandbox", "continue here"}
	if !reflect.DeepEqual(targetArgs, want) {
		t.Fatalf("target args = %#v, want %#v", targetArgs, want)
	}

	_, _, targetArgs, err = parseHandoffArgs([]string{"--session=source-id", "--", "--cwd", "target-cwd", "--session", "target-id"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"--cwd", "target-cwd", "--session", "target-id"}
	if !reflect.DeepEqual(targetArgs, want) {
		t.Fatalf("separated target args = %#v, want %#v", targetArgs, want)
	}
}

func TestParseHandoffArgsValidatesOwnedFlagsAndPassesTargetFlags(t *testing.T) {
	for _, args := range [][]string{{"--session"}, {"--cwd="}} {
		if _, _, _, err := parseHandoffArgs(args); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("parseHandoffArgs(%q) error = %v", args, err)
		}
	}
	for _, args := range [][]string{{"--profile", "mine"}, {"--profile=mine"}, {"-p=mine"}, {"-pmine"}, {"--config", `model_provider="openai"`}} {
		_, _, got, err := parseHandoffArgs(args)
		if err != nil {
			t.Fatalf("parseHandoffArgs(%q) error = %v", args, err)
		}
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("parseHandoffArgs(%q) target args = %v, want unchanged", args, got)
		}
	}
}

func TestValidateTargetArgsPassesCodexOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"--oss"},
		{"--local-provider=ollama"},
		{"--config", `model_provider="openai"`},
		{"--config=model_providers.agentswap.base_url=\"https://example.test\""},
		{"-c", `model_providers={}`},
		{"-cmodel_provider=\"openai\""},
	} {
		if err := validateTargetArgs(session.Codex, args); err != nil {
			t.Errorf("validateTargetArgs(%q) = %v, want target args accepted", args, err)
		}
	}
	for _, args := range [][]string{
		{"--model", "gpt-5.6-sol"},
		{"--config", `model_reasoning_effort="high"`},
		{"-cmodel_verbosity=\"low\""},
	} {
		if err := validateTargetArgs(session.Codex, args); err != nil {
			t.Errorf("validateTargetArgs(%q) = %v, want allowed model option", args, err)
		}
	}
}

func TestLaunchTargetPassesExactArgsAndCWD(t *testing.T) {
	if os.Getenv("AGENTSWAP_TEST_LAUNCH_HELPER") == "1" {
		launchHelperProcess()
		return
	}
	cwd := t.TempDir()
	capture := filepath.Join(t.TempDir(), "launch.json")
	t.Setenv("AGENTSWAP_TEST_LAUNCH_HELPER", "1")
	t.Setenv("AGENTSWAP_TEST_LAUNCH_CAPTURE", capture)
	args := []string{os.Args[0], "-test.run=TestLaunchTargetPassesExactArgsAndCWD", "--", "resume", "session id", "--model", "provider/model"}
	if err := launchTarget(args, cwd); err != nil {
		t.Fatal(err)
	}
	var got struct {
		CWD  string   `json:"cwd"`
		Args []string `json:"args"`
	}
	b, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"resume", "session id", "--model", "provider/model"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("launched args = %#v, want %#v", got.Args, wantArgs)
	}
	wantCWD, _ := filepath.EvalSymlinks(cwd)
	gotCWD, _ := filepath.EvalSymlinks(got.CWD)
	if gotCWD != wantCWD {
		t.Fatalf("launched cwd = %q, want %q", gotCWD, wantCWD)
	}
}

func TestLaunchTargetErrorsKeepTheCreatedSession(t *testing.T) {
	if os.Getenv("AGENTSWAP_TEST_LAUNCH_HELPER") == "1" {
		launchHelperProcess()
		return
	}
	if err := launchTarget(nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "did not provide") {
		t.Fatalf("empty launch error = %v", err)
	}
	t.Setenv("AGENTSWAP_TEST_LAUNCH_HELPER", "1")
	t.Setenv("AGENTSWAP_TEST_LAUNCH_FAIL", "1")
	err := launchTarget([]string{os.Args[0], "-test.run=TestLaunchTargetErrorsKeepTheCreatedSession"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "teleported session was kept") || !strings.Contains(err.Error(), "exit status 9") {
		t.Fatalf("failed launch error = %v", err)
	}
}

func launchHelperProcess() {
	if os.Getenv("AGENTSWAP_TEST_LAUNCH_FAIL") == "1" {
		os.Exit(9)
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(10)
	}
	b, err := json.Marshal(map[string]any{"cwd": cwd, "args": os.Args[separator:]})
	if err != nil {
		os.Exit(11)
	}
	if err := os.WriteFile(os.Getenv("AGENTSWAP_TEST_LAUNCH_CAPTURE"), b, 0o600); err != nil {
		os.Exit(12)
	}
	os.Exit(0)
}
