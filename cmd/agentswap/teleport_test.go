package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestChooseSessionWithIO(t *testing.T) {
	candidates := []session.Candidate{
		{Agent: session.Claude, ID: "claude-old", Title: "first\nline", UpdatedAt: time.Date(2026, 8, 19, 10, 0, 0, 0, time.Local)},
		{Agent: session.Codex, ID: "codex-new", Title: strings.Repeat("界", 61), UpdatedAt: time.Date(2026, 8, 19, 11, 0, 0, 0, time.Local)},
	}
	for _, tc := range []struct {
		name     string
		input    string
		terminal bool
		wantID   string
		wantErr  string
	}{
		{name: "select first", input: "1\n", terminal: true, wantID: "claude-old"},
		{name: "select second with whitespace", input: " 2 \n", terminal: true, wantID: "codex-new"},
		{name: "not a number", input: "x\n", terminal: true, wantErr: "invalid session selection"},
		{name: "zero", input: "0\n", terminal: true, wantErr: "invalid session selection"},
		{name: "too large", input: "3\n", terminal: true, wantErr: "invalid session selection"},
		{name: "closed input", terminal: true, wantErr: "read selection"},
		{name: "non-terminal", input: "1\n", wantErr: "selection is ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := chooseSessionWithIO(candidates, strings.NewReader(tc.input), &out, tc.terminal)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || got.ID != tc.wantID {
				t.Fatalf("selection = %+v, %v; want %s", got, err, tc.wantID)
			}
			listing := out.String()
			if !strings.Contains(listing, `"first line"`) || strings.Contains(listing, "first\nline") {
				t.Fatalf("picker did not sanitize title: %q", listing)
			}
			if !strings.Contains(listing, strings.Repeat("界", 60)+"…") {
				t.Fatalf("picker did not rune-truncate title: %q", listing)
			}
		})
	}
	if _, err := chooseSessionWithIO(nil, strings.NewReader(""), &bytes.Buffer{}, true); err == nil || !strings.Contains(err.Error(), "no source sessions") {
		t.Fatalf("empty picker error = %v", err)
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
