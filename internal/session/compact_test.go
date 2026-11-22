package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bulkyHistory builds the shape compaction exists for: a long coding session
// whose bytes are almost all tool output, with recorded reasoning, an inline
// screenshot, a plan, and an unanswered call at the end.
func bulkyHistory(t *testing.T, cwd string, turns, resultBytes int) *Session {
	t.Helper()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{
		Source: Claude, SourceID: "bulky-source", CWD: cwd,
		Model: "claude-sonnet-4-6", CreatedAt: base,
	}
	history.Events = append(history.Events, Event{
		Kind: Message, Role: "user", Timestamp: base,
		Parts: []Part{{Kind: Text, Text: "Refactor the parser so it reports column numbers."}},
	})
	for i := 0; i < turns; i++ {
		callID := fmt.Sprintf("call-%03d", i)
		editID := fmt.Sprintf("edit-%03d", i)
		ts := base.Add(time.Duration(i*10) * time.Second)
		history.Events = append(history.Events,
			Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{
				{Kind: Reasoning, Text: strings.Repeat("weighing the options ", 20)},
				{Kind: Text, Text: fmt.Sprintf("Checking step %d.", i)},
				{Kind: ToolCall, CallID: callID, ToolName: "Bash", Data: json.RawMessage(fmt.Sprintf(`{"command":"go test ./parser -run Case%d"}`, i))},
			}},
			Event{Kind: Message, Role: "tool", Timestamp: ts.Add(time.Second), Parts: []Part{
				{Kind: ToolResult, CallID: callID, Text: fmt.Sprintf("head-%03d\n", i) + strings.Repeat(fmt.Sprintf("noise line %03d\n", i), resultBytes/16) + fmt.Sprintf("tail-%03d", i)},
			}},
			Event{Kind: Message, Role: "assistant", Timestamp: ts.Add(2 * time.Second), Parts: []Part{
				{Kind: ToolCall, CallID: editID, ToolName: "Edit", Data: json.RawMessage(fmt.Sprintf(`{"file_path":%q,"old_string":"a","new_string":"b"}`, filepath.Join(cwd, "parser.go")))},
			}},
			Event{Kind: Message, Role: "tool", Timestamp: ts.Add(3 * time.Second), Parts: []Part{
				{Kind: ToolResult, CallID: editID, Text: "edited"},
			}},
		)
	}
	history.Events = append(history.Events,
		Event{Kind: Message, Role: "user", Timestamp: base.Add(time.Hour), Parts: []Part{
			{Kind: Media, MediaType: "image/png", MediaData: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("PNGDATA", 500))), Filename: "screenshot.png"},
			{Kind: Text, Text: "Here is the failing output."},
		}},
		Event{Kind: Plan, Role: "assistant", Timestamp: base.Add(time.Hour + time.Second), PlanText: "1. Track columns in the lexer\n2. Thread them through the parser"},
		Event{Kind: Message, Role: "assistant", Timestamp: base.Add(time.Hour + 2*time.Second), Parts: []Part{
			{Kind: Text, Text: "Starting on the lexer now."},
			{Kind: ToolCall, CallID: "dangling-call", ToolName: "Bash", Data: json.RawMessage(`{"command":"go build ./..."}`)},
		}},
	)
	history.UpdatedAt = history.Events[len(history.Events)-1].Timestamp
	if err := history.Validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	return history
}

func compactFixture(t *testing.T, history *Session, budget int) (*Session, *Archive, CompactionReport) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "archive")
	out, archive, report, err := Compact(history, CompactOptions{
		Budget: budget, ArchiveDir: dir, Version: "test",
		Now: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Compact(budget=%d): %v", budget, err)
	}
	return out, archive, report
}

func TestCompactLeavesAFittingSessionAlone(t *testing.T) {
	history := sampleHistory(t, t.TempDir())
	out, archive, report := compactFixture(t, history, 100_000)
	if !report.Fit {
		t.Fatalf("small session did not fit: %+v", report)
	}
	if report.Before != report.After {
		t.Fatalf("a fitting session was still reduced: %d -> %d", report.Before, report.After)
	}
	if !reflect.DeepEqual(out.Events, history.Events) {
		t.Fatalf("a fitting session had its events rewritten")
	}
	if archive != nil {
		t.Fatalf("a fitting session produced an archive at %s", archive.Dir)
	}
	if !strings.Contains(report.Summary(), "nothing was removed") {
		t.Fatalf("summary does not say the session already fit: %q", report.Summary())
	}
	if strings.Contains(renderEvents(out.Events), "agentswap-transferred-context") {
		t.Fatal("a fitting session was given a digest it does not need")
	}
}

// The invariant the whole feature rests on: whatever the budget, the session
// handed to a writer is one the writer can represent.
func TestCompactedSessionsAlwaysValidate(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 30, 4000)
	for _, budget := range []int{1_000, 2_500, 5_000, 12_000, 40_000, 120_000, 5_000_000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			out, _, report := compactFixture(t, history, budget)
			if err := out.Validate(); err != nil {
				t.Fatalf("compacted session is invalid: %v", err)
			}
			if report.After > report.Before {
				t.Fatalf("compaction grew the session: %d -> %d", report.Before, report.After)
			}
			if report.Fit && report.After > budget {
				t.Fatalf("report claims a fit at %d tokens against a %d budget", report.After, budget)
			}
			if EstimateTokens(out) != report.After {
				t.Fatalf("report.After = %d, estimate = %d", report.After, EstimateTokens(out))
			}
		})
	}
}

func TestCompactReducesUntilItFits(t *testing.T) {
	history := bulkyHistory(t, t.TempDir(), 40, 8000)
	const budget = 20_000
	out, _, report := compactFixture(t, history, budget)
	if !report.Fit {
		t.Fatalf("compaction did not reach the budget: %+v", report)
	}
	if report.After > budget {
		t.Fatalf("compacted to %d tokens, above the %d budget", report.After, budget)
	}
	if report.Before < budget*4 {
		t.Fatalf("fixture is not large enough to be a test: %d tokens", report.Before)
	}
	if report.ReasoningParts == 0 || report.ToolResults == 0 {
		t.Fatalf("expected reasoning and tool results to be reduced: %+v", report)
	}
	_ = out
}

func TestCompactDropsRecordedReasoningBeforeToolOutput(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{
		Source: Claude, SourceID: "reasoning-heavy", CWD: cwd, CreatedAt: base,
		Events: []Event{
			{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{{Kind: Text, Text: "Do the work."}}},
			{Kind: Message, Role: "assistant", Timestamp: base.Add(time.Second), Parts: []Part{
				{Kind: Reasoning, Text: strings.Repeat("thinking hard ", 4000)},
				{Kind: ToolCall, CallID: "c1", ToolName: "Bash", Data: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Kind: Message, Role: "tool", Timestamp: base.Add(2 * time.Second), Parts: []Part{
				{Kind: ToolResult, CallID: "c1", Text: "a.go\nb.go"},
			}},
		},
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _, report := compactFixture(t, history, 2_000)
	if !report.Fit || report.ReasoningParts != 1 {
		t.Fatalf("expected the reasoning block alone to be enough: %+v", report)
	}
	if report.ToolResults != 0 {
		t.Fatalf("tool output was truncated when dropping reasoning was enough: %+v", report)
	}
	text := renderEvents(out.Events)
	if strings.Contains(text, "thinking hard") {
		t.Fatal("recorded reasoning survived")
	}
	if !strings.Contains(text, "a.go\nb.go") {
		t.Fatalf("tool output was lost: %s", text)
	}
}

func TestCompactTruncationPointsAtTheArchivedText(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 20, 6000)
	out, archive, report := compactFixture(t, history, 15_000)
	if report.ToolResults == 0 {
		t.Fatalf("no tool results were truncated: %+v", report)
	}
	text := renderEvents(out.Events)
	if !strings.Contains(text, "[agentswap:elided") {
		t.Fatal("a truncated result carried no marker")
	}
	if !strings.Contains(text, "head-000") || !strings.Contains(text, "tail-000") {
		t.Fatalf("truncation dropped both ends of the output instead of the middle:\n%s", firstLines(text, 40))
	}
	// Every marker must name a file the archive actually holds, with the whole
	// original payload in it.
	if err := archive.Write(); err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, line := range strings.Split(text, "\n") {
		path, ok := markerPath(line)
		if !ok {
			continue
		}
		markers++
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("marker names a file that does not exist: %v", err)
		}
		if len(body) == 0 {
			t.Fatalf("archived shard %s is empty", path)
		}
	}
	if markers == 0 {
		t.Fatal("no marker named an archive file")
	}
	original := history.Events[2].Parts[0].Text
	found := false
	for _, shard := range archive.Manifest.Shards {
		body, err := os.ReadFile(filepath.Join(archive.Dir, shard.File))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) == original {
			found = true
		}
		if hashHex(body) != shard.SHA256 {
			t.Fatalf("shard %s checksum does not match its contents", shard.File)
		}
	}
	if !found {
		t.Fatal("the archive does not hold the complete text of a truncated result")
	}
}

// markerPath reads the file an elision marker names.
func markerPath(line string) (string, bool) {
	i := strings.Index(line, "full text: ")
	if i >= 0 {
		rest := line[i+len("full text: "):]
		if j := strings.Index(rest, "]"); j >= 0 {
			return rest[:j], true
		}
	}
	i = strings.Index(line, "full=")
	if i >= 0 {
		rest := line[i+len("full="):]
		if j := strings.Index(rest, "]"); j >= 0 {
			return rest[:j], true
		}
	}
	return "", false
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestCompactCollapseKeepsToolCallsBesideTheirResults(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 60, 200)
	out, _, report := compactFixture(t, history, 1_200)
	if report.EventsCollapsed == 0 {
		t.Fatalf("expected turns to be collapsed at this budget: %+v", report)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("collapse produced an invalid session: %v", err)
	}
	// Validate already rejects an orphaned result; this asserts the stronger
	// property that no kept result lost the call that produced it.
	defined := map[string]bool{}
	for _, event := range out.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolCall {
				defined[part.CallID] = true
			}
		}
	}
	for _, event := range out.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult && !defined[part.CallID] {
				t.Fatalf("kept a result for collapsed call %q", part.CallID)
			}
		}
	}
}

func TestCompactKeepsTheOpeningRequestThePlanAndTheUnfinishedWork(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 60, 500)
	out, _, report := compactFixture(t, history, 1_500)
	if report.EventsCollapsed == 0 {
		t.Fatalf("expected a collapse: %+v", report)
	}
	text := renderEvents(out.Events)
	for _, want := range []string{
		"Refactor the parser so it reports column numbers.",
		"Track columns in the lexer",
		"dangling-call",
		"parser.go",
		"go test ./parser",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("compacted session lost %q", want)
		}
	}
	if !strings.Contains(text, "agentswap-transferred-context") {
		t.Error("compacted session carries no digest")
	}
	if !strings.Contains(text, "FILES WRITTEN") || !strings.Contains(text, "COMMANDS RUN") {
		t.Errorf("digest is missing its derived ledger:\n%s", firstLines(text, 60))
	}
	if !strings.Contains(text, "UNFINISHED WORK") {
		t.Error("digest does not name the unanswered tool call")
	}
}

func TestCompactNeverModifiesTheSource(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 25, 3000)
	before, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{1_000, 8_000, 60_000} {
		compactFixture(t, history, budget)
	}
	after, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("compaction mutated the source session")
	}
}

// A delegated run is a separate transcript that the main model never read, so
// resuming the main thread does not load it. Counting or archiving one would
// compact a session that was never too large, and would lose fidelity on the
// targets that can keep it.
func TestCompactLeavesBranchesToTheWriters(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 20, 4000)
	history.Branches = sampleBranches()
	history.Branches[0].CallID = "call-000"
	history.Branches[1].CallID = "branch-call-1"
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	withBranches := EstimateTokens(history)
	bare := *history
	bare.Branches = nil
	if withBranches != EstimateTokens(&bare) {
		t.Fatal("branches were counted against the transcript budget")
	}
	out, _, _ := compactFixture(t, history, 2_000)
	assertBranchesMatch(t, out.Branches, history.Branches)
}

func TestCompactArchivesInlineMediaAsAReadableFile(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 4, 200)
	out, archive, report := compactFixture(t, history, 1_000)
	if report.MediaParts == 0 {
		t.Fatalf("expected the attachment to be archived: %+v", report)
	}
	if err := archive.Write(); err != nil {
		t.Fatal(err)
	}
	var mediaShard *Shard
	for i := range archive.Manifest.Shards {
		if archive.Manifest.Shards[i].Kind == "media" {
			mediaShard = &archive.Manifest.Shards[i]
		}
	}
	if mediaShard == nil {
		t.Fatal("no media shard was recorded")
	}
	if filepath.Ext(mediaShard.File) != ".png" {
		t.Fatalf("media shard kept no usable extension: %s", mediaShard.File)
	}
	body, err := os.ReadFile(filepath.Join(archive.Dir, mediaShard.File))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != strings.Repeat("PNGDATA", 500) {
		t.Fatal("the archived attachment is not the original payload")
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("stripping media produced an invalid session: %v", err)
	}
	if strings.Contains(renderEvents(out.Events), base64.StdEncoding.EncodeToString([]byte("PNGDATA"))) {
		t.Fatal("the inline payload survived in the transcript")
	}
}

func TestCompactReportsWhenItCannotReachTheBudget(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	// One enormous user message with nothing removable around it: the floor is
	// still far above any small budget.
	history := &Session{
		Source: Codex, SourceID: "irreducible", CWD: cwd, CreatedAt: base,
		Events: []Event{{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{
			{Kind: Text, Text: strings.Repeat("x", 4_000_000)},
		}}},
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _, report := compactFixture(t, history, 1_000)
	if report.Fit {
		t.Fatalf("claimed a fit it cannot have reached: %+v", report)
	}
	if report.After >= report.Before {
		t.Fatalf("no reduction was made at all: %+v", report)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(out.Warnings, "\n")
	if !strings.Contains(joined, "floor") || !strings.Contains(joined, "may exceed its context window") {
		t.Fatalf("the user is not told the budget was missed: %q", out.Warnings)
	}
}

func TestEstimateTokensChargesTextByScriptAndMediaFlat(t *testing.T) {
	cwd := "/tmp/project"
	build := func(parts ...Part) *Session {
		return &Session{Source: Claude, SourceID: "e", CWD: cwd, Events: []Event{{Kind: Message, Role: "user", Parts: parts}}}
	}
	ascii := EstimateTokens(build(Part{Kind: Text, Text: strings.Repeat("a", 700)}))
	cjk := EstimateTokens(build(Part{Kind: Text, Text: strings.Repeat("界", 700)}))
	if cjk <= ascii {
		t.Fatalf("CJK text was not charged more than ASCII: %d vs %d", cjk, ascii)
	}
	if ascii < 180 || ascii > 260 {
		t.Fatalf("700 ASCII bytes estimated at %d tokens, which is not near 3.5 bytes per token", ascii)
	}
	// A megabyte of base64 is on the order of a thousand image tokens, not a
	// quarter million text ones.
	image := EstimateTokens(build(Part{Kind: Media, MediaType: "image/png", MediaData: strings.Repeat("A", 1<<20)}))
	if image > 4*mediaTokenEstimate {
		t.Fatalf("an inline image was charged by payload size: %d tokens", image)
	}
}

func TestParseBudgetAcceptsTheFormsPeopleWrite(t *testing.T) {
	for input, want := range map[string]int{
		"120000": 120_000, "120k": 120_000, "120K": 120_000,
		"1.5M": 1_500_000, "1m": 1_000_000, " 64k ": 64_000,
	} {
		got, err := ParseBudget(input)
		if err != nil || got != want {
			t.Errorf("ParseBudget(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "abc", "12", "0", "-5k", "120 tokens"} {
		if _, err := ParseBudget(input); err == nil {
			t.Errorf("ParseBudget(%q) was accepted", input)
		}
	}
}

// fixtureDir is an absolute project directory written the way the host writes
// one. The ledger relativizes with filepath, and "/p" is not an absolute path
// on Windows, so a POSIX-only fixture would leave every path unrelativized
// there and test nothing.
func fixtureDir() string {
	if runtime.GOOS == "windows" {
		return `C:\p`
	}
	return "/p"
}

func fixturePath(name string) string { return filepath.Join(fixtureDir(), name) }

// fixtureInput encodes a tool call's input, so that a Windows separator in a
// fixture path is escaped the way a harness would write it.
func fixtureInput(t *testing.T, input any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDigestLedgerReadsEveryHarnessInputShape(t *testing.T) {
	events := []Event{
		{Kind: Message, Role: "user", Parts: []Part{{Kind: Text, Text: "start"}}},
		{Kind: Message, Role: "assistant", Parts: []Part{
			{Kind: ToolCall, CallID: "a", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("one.go")})},
			{Kind: ToolCall, CallID: "b", ToolName: "Read", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("two.go")})},
			{Kind: ToolCall, CallID: "c", ToolName: "shell", Data: json.RawMessage(`{"command":["bash","-lc","go test ./..."]}`)},
			{Kind: ToolCall, CallID: "d", ToolName: "apply_patch", Data: fixtureInput(t, map[string]string{"input": "*** Begin Patch\n*** Update File: " + fixturePath("three.go") + "\n"})},
			{Kind: ToolCall, CallID: "e", ToolName: "MultiEdit", Data: fixtureInput(t, map[string]any{"edits": []map[string]string{
				{"file_path": fixturePath("four.go")}, {"file_path": fixturePath("five.go")},
			}})},
			{Kind: ToolCall, CallID: "f", ToolName: "Bash", Data: json.RawMessage(`{"command":"git   status\n"}`)},
		}},
		{Kind: Message, Role: "tool", Parts: []Part{
			{Kind: ToolResult, CallID: "a", Text: "ok"}, {Kind: ToolResult, CallID: "b", Text: "ok"},
			{Kind: ToolResult, CallID: "c", Text: "ok"}, {Kind: ToolResult, CallID: "d", Text: "ok"},
			{Kind: ToolResult, CallID: "e", Text: "ok"},
		}},
		{Kind: Plan, Role: "assistant", PlanText: "the plan"},
	}
	l := deriveLedger(events, fixtureDir())
	if !reflect.DeepEqual(l.writeOrder, []string{"one.go", "three.go", "four.go", "five.go"}) {
		t.Errorf("writes = %v", l.writeOrder)
	}
	if !reflect.DeepEqual(l.readOrder, []string{"two.go"}) {
		t.Errorf("reads = %v", l.readOrder)
	}
	if !reflect.DeepEqual(l.commands, []string{"go test ./...", "git status"}) {
		t.Errorf("commands = %v", l.commands)
	}
	if l.plan != "the plan" {
		t.Errorf("plan = %q", l.plan)
	}
	if !reflect.DeepEqual(l.dangling, []string{"f (Bash)"}) {
		t.Errorf("dangling = %v", l.dangling)
	}
}

func TestArchiveWritesACompleteReadableDirectory(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 15, 4000)
	_, archive, _ := compactFixture(t, history, 8_000)
	if err := archive.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive.Dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("staging directory was left behind")
	}
	for _, name := range []string{"INDEX.md", "history.json", "manifest.json", "transcript.txt"} {
		info, err := os.Stat(filepath.Join(archive.Dir, name))
		if err != nil {
			t.Fatalf("archive is missing %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("archive file %s is empty", name)
		}
	}
	body, err := os.ReadFile(filepath.Join(archive.Dir, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restored Session
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatalf("archived history.json does not parse: %v", err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatalf("archived history.json is not a valid session: %v", err)
	}
	if len(restored.Events) != len(history.Events) {
		t.Fatalf("archived history has %d events, source had %d", len(restored.Events), len(history.Events))
	}
	transcript, err := os.ReadFile(filepath.Join(archive.Dir, "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "weighing the options") {
		t.Fatal("transcript.txt does not hold the reasoning the transfer dropped")
	}
	index, err := os.ReadFile(filepath.Join(archive.Dir, "INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"transcript.txt", "history.json", "agentswap:elided"} {
		if !strings.Contains(string(index), want) {
			t.Errorf("INDEX.md does not mention %q", want)
		}
	}
	var manifest Manifest
	manifestBody, err := os.ReadFile(filepath.Join(archive.Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != archiveSchemaVersion || manifest.AgentswapVersion != "test" {
		t.Fatalf("manifest header = %+v", manifest)
	}
	if manifest.Source.SessionID != history.SourceID || manifest.Target != nil {
		t.Fatalf("manifest endpoints = %+v / %+v", manifest.Source, manifest.Target)
	}
	if len(manifest.Shards) == 0 {
		t.Fatal("manifest records no shards")
	}
}

func TestArchiveNamesStayInsideTheArchiveDirectory(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	// Call ids and filenames come from the source session, so they are treated
	// as untrusted input when they become file names.
	history := &Session{
		Source: Claude, SourceID: "hostile", CWD: cwd, CreatedAt: base,
		Events: []Event{
			{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{{Kind: Text, Text: "go"}}},
			{Kind: Message, Role: "assistant", Timestamp: base, Parts: []Part{
				{Kind: ToolCall, CallID: "../../../etc/passwd", ToolName: "Bash", Data: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Kind: Message, Role: "tool", Timestamp: base, Parts: []Part{
				{Kind: ToolResult, CallID: "../../../etc/passwd", Text: strings.Repeat("leak\n", 5000)},
			}},
			{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{
				{Kind: Media, MediaType: "image/png", MediaData: base64.StdEncoding.EncodeToString([]byte("x")), Filename: "../../escape.png"},
			}},
		},
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	_, archive, _ := compactFixture(t, history, 1_000)
	if err := archive.Write(); err != nil {
		t.Fatal(err)
	}
	for _, shard := range archive.Manifest.Shards {
		if strings.Contains(shard.File, "..") {
			t.Fatalf("shard escapes the archive: %s", shard.File)
		}
		full := filepath.Join(archive.Dir, shard.File)
		if !strings.HasPrefix(full, archive.Dir+string(os.PathSeparator)) {
			t.Fatalf("shard path %s is outside %s", full, archive.Dir)
		}
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("shard %s was not written: %v", shard.File, err)
		}
	}
}

// FuzzCompactKeepsSessionsValid asserts the one property a writer depends on:
// anything Validate accepts stays acceptable after compaction, at any budget.
func FuzzCompactKeepsSessionsValid(f *testing.F) {
	f.Add([]byte(`{"source":"claude","source_id":"s","cwd":"/tmp/p","events":[
		{"kind":"message","role":"user","parts":[{"kind":"text","text":"hello"}]},
		{"kind":"message","role":"assistant","parts":[{"kind":"reasoning","text":"why"},{"kind":"tool_call","call_id":"c1","tool_name":"Bash","data":{"command":"ls"}}]},
		{"kind":"message","role":"tool","parts":[{"kind":"tool_result","call_id":"c1","text":"a\nb\nc"}]},
		{"kind":"plan","role":"assistant","plan_text":"do it"}
	]}`), 2000)
	f.Add([]byte(`{"source":"codex","source_id":"s","cwd":"/tmp/p","events":[
		{"kind":"message","role":"assistant","parts":[{"kind":"tool_call","call_id":"c1","tool_name":"t","data":{}}]},
		{"kind":"message","role":"user","parts":[{"kind":"media","call_id":"c1","media_type":"image/png","media_data":"eA=="}]}
	]}`), 1000)
	f.Add([]byte(`{"source":"kimi","source_id":"s","cwd":"/tmp/p","events":[
		{"kind":"message","role":"user","parts":[{"kind":"text","text":"x"}]},
		{"kind":"message","role":"assistant","parts":[{"kind":"tool_call","call_id":"c1","tool_name":"t","data":{}}]},
		{"kind":"message","role":"tool","parts":[{"kind":"tool_result","call_id":"c1","text":"r"},{"kind":"media","call_id":"c1","media_type":"image/png","media_data":"eA=="}]}
	]}`), 1500)

	f.Fuzz(func(t *testing.T, data []byte, budget int) {
		var history Session
		if err := json.Unmarshal(data, &history); err != nil {
			return
		}
		if history.Validate() != nil {
			return
		}
		if budget < 1000 || budget > 1<<22 {
			budget = 1000
		}
		out, _, report, err := Compact(&history, CompactOptions{
			Budget: budget, ArchiveDir: filepath.Join(t.TempDir(), "a"), Now: time.Unix(0, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("Compact returned an error for a valid session: %v", err)
		}
		if err := out.Validate(); err != nil {
			t.Fatalf("compacted session is invalid: %v", err)
		}
		if report.After > report.Before {
			t.Fatalf("compaction grew the session: %d -> %d", report.Before, report.After)
		}
	})
}

// --- transfer-level behavior -------------------------------------------------

func teleportFixture(t *testing.T, source Adapter, cwd string, history *Session) Candidate {
	t.Helper()
	native, err := source.Write(context.Background(), history, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return candidateForResult(source.Agent(), native, cwd)
}

func TestTeleportCompactsIntoEveryFileBackedTarget(t *testing.T) {
	for _, target := range []Adapter{claudeAdapter{}, codexAdapter{}, kimiAdapter{}} {
		t.Run(string(target.Agent()), func(t *testing.T) {
			isolatedHomes(t)
			cwd := t.TempDir()
			var source Adapter = claudeAdapter{}
			if target.Agent() == Claude {
				source = codexAdapter{}
			}
			candidate := teleportFixture(t, source, cwd, bulkyHistory(t, cwd, 30, 4000))
			manager := NewManagerWith(source, target)
			result, history, err := manager.Teleport(context.Background(), candidate, target.Agent(), TransferOptions{
				WriteOptions: WriteOptions{CWD: cwd},
				Compact:      &CompactOptions{Budget: 12_000, Version: "test"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Compaction == nil || !result.Compaction.Fit {
				t.Fatalf("compaction report = %+v", result.Compaction)
			}
			if result.ArchivePath == "" {
				t.Fatal("no archive path was reported")
			}
			if _, err := os.Stat(filepath.Join(result.ArchivePath, "INDEX.md")); err != nil {
				t.Fatalf("archive was not written: %v", err)
			}
			// The manifest now names the session that was created from it.
			var manifest Manifest
			body, err := os.ReadFile(filepath.Join(result.ArchivePath, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Target == nil || manifest.Target.SessionID != result.ID || manifest.Target.Agent != target.Agent() {
				t.Fatalf("manifest target = %+v, want session %s on %s", manifest.Target, result.ID, target.Agent())
			}
			// The written target must read back as a valid session that still
			// carries the digest and the archive path.
			back, err := target.Read(context.Background(), candidateForResult(target.Agent(), result, cwd))
			if err != nil {
				t.Fatal(err)
			}
			if err := back.Validate(); err != nil {
				t.Fatalf("compacted target does not read back cleanly: %v", err)
			}
			text := renderEvents(back.Events)
			for _, want := range []string{
				"agentswap-transferred-context",
				result.ArchivePath,
				"Refactor the parser so it reports column numbers.",
				"Track columns in the lexer",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("target session lost %q", want)
				}
			}
			if history.Events[0].Parts[0].Text == text {
				t.Fatal("the returned history should be the source, not the abridged copy")
			}
		})
	}
}

func TestTeleportWarnsInsteadOfCompactingOnItsOwn(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, bulkyHistory(t, cwd, 60, 20_000))
	manager := NewManagerWith(claudeAdapter{}, codexAdapter{})
	result, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Compaction != nil || result.ArchivePath != "" {
		t.Fatal("a plain transfer compacted without being asked")
	}
	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "--compact") {
		t.Fatalf("an oversized transfer did not name the remedy: %q", result.Warnings)
	}
	if !strings.Contains(joined, "Codex") {
		t.Fatalf("the warning does not name the target: %q", result.Warnings)
	}
}

func TestTeleportDryRunWithCompactWritesNothing(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, bulkyHistory(t, cwd, 20, 4000))
	manager := NewManagerWith(claudeAdapter{}, codexAdapter{})
	result, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd, DryRun: true},
		Compact:      &CompactOptions{Budget: 10_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Compaction == nil {
		t.Fatal("a dry run reported no compaction")
	}
	if _, err := os.Stat(result.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("a dry run created the archive at %s", result.ArchivePath)
	}
	assertNoArchives(t, cwd)
}

// A resumed agent is normally confined to its project directory, so an archive
// it cannot reach is an archive it cannot use. ArchiveRoot puts one where the
// target can read it without being granted anything.
func TestTeleportHonorsAChosenArchiveRoot(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, bulkyHistory(t, cwd, 20, 4000))
	elsewhere := filepath.Join(t.TempDir(), "shared-archives")
	manager := NewManagerWith(claudeAdapter{}, codexAdapter{})
	result, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd},
		Compact:      &CompactOptions{Budget: 8_000, ArchiveRoot: elsewhere, Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(result.ArchivePath) != elsewhere {
		t.Fatalf("archive went to %s, want a directory under %s", result.ArchivePath, elsewhere)
	}
	if _, err := os.Stat(filepath.Join(result.ArchivePath, "transcript.txt")); err != nil {
		t.Fatalf("archive was not written to the chosen root: %v", err)
	}
	// Naming a root must stop the project-local default from being used too.
	assertNoArchives(t, cwd)
	// Every marker must name the chosen location, not the default one.
	back, err := (codexAdapter{}).Read(context.Background(), candidateForResult(Codex, result, cwd))
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, line := range strings.Split(renderEvents(back.Events), "\n") {
		path, ok := markerPath(line)
		if !ok {
			continue
		}
		markers++
		if !strings.HasPrefix(path, elsewhere+string(os.PathSeparator)) {
			t.Fatalf("marker does not point at the chosen root: %s", path)
		}
	}
	if markers == 0 {
		t.Fatal("no marker was written, so the path could not be checked")
	}
}

func TestNewArchiveDirAbsolutizesAndNeverRepeats(t *testing.T) {
	if _, err := NewArchiveDir(""); err == nil {
		t.Fatal("an empty archive root was accepted")
	}
	chosen, err := NewArchiveDir("relative-root")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(chosen) {
		t.Fatalf("NewArchiveDir returned %q; a marker must carry a path a target resumed elsewhere can open", chosen)
	}
	// Two allocations never collide, so one project can hold many archives.
	other, err := NewArchiveDir("relative-root")
	if err != nil || other == chosen {
		t.Fatalf("NewArchiveDir reused %s", chosen)
	}
}

// failingWriter accepts discovery and reads but refuses to write, so a target
// failure can be observed without corrupting a real harness directory.
type failingWriter struct{ Adapter }

func (failingWriter) Agent() Agent { return Codex }
func (failingWriter) Write(context.Context, *Session, WriteOptions) (Result, error) {
	return Result{}, fmt.Errorf("target is full")
}

func TestTeleportRemovesTheArchiveWhenTheTargetWriteFails(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, bulkyHistory(t, cwd, 20, 4000))
	manager := NewManagerWith(claudeAdapter{}, failingWriter{codexAdapter{}})
	_, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd},
		Compact:      &CompactOptions{Budget: 8_000},
	})
	if err == nil {
		t.Fatal("expected the target write to fail")
	}
	assertNoArchives(t, cwd)
}

// assertNoArchives checks that the project's archive directory holds nothing.
// The directory itself may exist — a rolled-back transfer created it before it
// failed — but it must contain no archive.
func assertNoArchives(t *testing.T, cwd string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cwd, DefaultArchiveDirName))
	if err != nil {
		return
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	if len(found) > 0 {
		t.Fatalf("archives were left in the project: %v", found)
	}
}

func TestTeleportCompactOnAFittingSessionReportsButArchivesNothing(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, sampleHistory(t, cwd))
	manager := NewManagerWith(claudeAdapter{}, codexAdapter{})
	result, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd},
		Compact:      &CompactOptions{Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Compaction == nil {
		t.Fatal("compaction was requested but not reported")
	}
	if result.ArchivePath != "" {
		t.Fatalf("a session that already fits produced an archive at %s", result.ArchivePath)
	}
	if result.Compaction.Budget != DefaultBudget(Codex) {
		t.Fatalf("budget = %d, want the target default %d", result.Compaction.Budget, DefaultBudget(Codex))
	}
	assertNoArchives(t, cwd)
	back, err := (codexAdapter{}).Read(context.Background(), candidateForResult(Codex, result, cwd))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(renderEvents(back.Events), "agentswap-transferred-context") {
		t.Fatal("a session that already fits was given a digest")
	}
}

func TestCompactRejectsIncompleteRequests(t *testing.T) {
	history := sampleHistory(t, t.TempDir())
	dir := filepath.Join(t.TempDir(), "archive")
	for _, tc := range []struct {
		name    string
		history *Session
		opts    CompactOptions
		want    string
	}{
		{name: "nil session", opts: CompactOptions{Budget: 1000, ArchiveDir: dir}, want: "nil session"},
		{name: "no archive directory", history: history, opts: CompactOptions{Budget: 1000}, want: "archive directory"},
		{name: "no budget", history: history, opts: CompactOptions{ArchiveDir: dir}, want: "positive token budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := Compact(tc.history, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compact = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	if EstimateTokens(nil) != 0 {
		t.Fatal("a nil session was estimated at a non-zero size")
	}
}

func TestDefaultBudgetsStayBelowEveryHarnessWindow(t *testing.T) {
	for _, agent := range append(Agents(), Agent("something-new")) {
		budget := DefaultBudget(agent)
		if budget < 50_000 || budget > 150_000 {
			t.Errorf("DefaultBudget(%s) = %d, which is not a cautious transcript budget", agent, budget)
		}
	}
}

func TestMediaExtensionsFollowTypeThenFilename(t *testing.T) {
	for _, tc := range []struct {
		mediaType, filename, want string
	}{
		{"image/png", "", ".png"},
		{"image/jpeg", "", ".jpg"},
		{"image/jpg", "", ".jpg"},
		{"image/gif", "", ".gif"},
		{"image/webp", "", ".webp"},
		{"image/svg+xml", "", ".svg"},
		{"application/pdf", "", ".pdf"},
		{"text/plain", "", ".txt"},
		{"application/x-unheard-of", "", ".bin"},
		// A usable filename extension outranks the MIME table, which is how a
		// source that recorded a precise name keeps it.
		{"application/octet-stream", "notes.md", ".md"},
		{"image/png", "shot.PNG", ".png"},
		// An implausibly long "extension" is not one.
		{"image/png", "archive.tar.verylongthing", ".png"},
	} {
		if got := mediaExtension(tc.mediaType, tc.filename); got != tc.want {
			t.Errorf("mediaExtension(%q, %q) = %q, want %q", tc.mediaType, tc.filename, got, tc.want)
		}
	}
}

func TestDigestListsTheMostEditedFilesFirst(t *testing.T) {
	events := []Event{
		{Kind: Message, Role: "user", Parts: []Part{{Kind: Text, Text: "start"}}},
		{Kind: Message, Role: "assistant", Parts: []Part{
			{Kind: ToolCall, CallID: "1", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("rare.go")})},
			{Kind: ToolCall, CallID: "2", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("hot.go")})},
			{Kind: ToolCall, CallID: "3", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("hot.go")})},
			{Kind: ToolCall, CallID: "4", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("mid.go")})},
			{Kind: ToolCall, CallID: "5", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("hot.go")})},
			{Kind: ToolCall, CallID: "6", ToolName: "Edit", Data: fixtureInput(t, map[string]string{"file_path": fixturePath("mid.go")})},
		}},
		{Kind: Message, Role: "tool", Parts: []Part{
			{Kind: ToolResult, CallID: "1", Text: "ok"}, {Kind: ToolResult, CallID: "2", Text: "ok"},
			{Kind: ToolResult, CallID: "3", Text: "ok"}, {Kind: ToolResult, CallID: "4", Text: "ok"},
			{Kind: ToolResult, CallID: "5", Text: "ok"}, {Kind: ToolResult, CallID: "6", Text: "ok"},
		}},
	}
	l := deriveLedger(events, fixtureDir())
	ordered := sortedPathCounts(l.writeCounts, append([]string(nil), l.writeOrder...))
	if !reflect.DeepEqual(ordered, []string{"hot.go", "mid.go", "rare.go"}) {
		t.Fatalf("ledger order = %v, want most-edited first", ordered)
	}
}

func TestArchiveKeepsARemoteMediaReferenceItCannotDownload(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{
		Source: Codex, SourceID: "remote-media", CWD: cwd, CreatedAt: base,
		Events: []Event{
			{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{
				{Kind: Text, Text: strings.Repeat("context ", 2000)},
				{Kind: Media, MediaType: "image/png", MediaURL: "https://example.invalid/diagram.png"},
			}},
		},
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	out, archive, report := compactFixture(t, history, 1_000)
	if report.MediaParts != 1 {
		t.Fatalf("the remote attachment was not archived: %+v", report)
	}
	if err := archive.Write(); err != nil {
		t.Fatal(err)
	}
	var ref *Shard
	for i := range archive.Manifest.Shards {
		if archive.Manifest.Shards[i].Kind == "media_reference" {
			ref = &archive.Manifest.Shards[i]
		}
	}
	if ref == nil {
		t.Fatal("no media reference was recorded")
	}
	body, err := os.ReadFile(filepath.Join(archive.Dir, ref.File))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "https://example.invalid/diagram.png") {
		t.Fatalf("the archived reference lost the URL: %q", body)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveWriteLeavesNothingBehindWhenItCannotStage(t *testing.T) {
	cwd := t.TempDir()
	history := bulkyHistory(t, cwd, 10, 4000)
	_, archive, _ := compactFixture(t, history, 4_000)
	// A file where the archive's parent directory must be: staging cannot be
	// created, and the failure must not leave a half-written directory.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive.Dir = filepath.Join(blocked, "archive")
	if err := archive.Write(); err == nil {
		t.Fatal("writing into a file succeeded")
	}
	// Nothing may exist at either path. The error kind differs by platform —
	// a path under a regular file is ENOTDIR on macOS and ENOENT elsewhere —
	// so the assertion is that neither path resolves at all.
	if _, err := os.Stat(archive.Dir); err == nil {
		t.Fatalf("a failed archive write left %s behind", archive.Dir)
	}
	if _, err := os.Stat(archive.Dir + ".tmp"); err == nil {
		t.Fatal("a failed archive write left its staging directory behind")
	}
	// Remove and Finalize are safe on an archive that was never written.
	archive.Remove()
	if err := (*Archive)(nil).Write(); err != nil {
		t.Fatalf("writing a nil archive = %v", err)
	}
	(*Archive)(nil).Remove()
	if err := (*Archive)(nil).Finalize(Result{}); err != nil {
		t.Fatalf("finalizing a nil archive = %v", err)
	}
}

// The collapse boundary cannot fall just anywhere: a kept result whose call was
// collapsed away is an invalid session. This walks the boundary through every
// position in a thread whose calls and results sit at varying distances, so
// each awkward landing is exercised rather than whichever one a budget happens
// to produce.
func TestCollapseKeepsEveryKeptResultBesideItsCall(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	history := &Session{
		Source: Claude, SourceID: "straddling-calls", CWD: cwd, CreatedAt: base,
		Events: []Event{{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{{Kind: Text, Text: "Start the checks."}}}},
	}
	filler := func(n int) {
		for i := 0; i < n; i++ {
			history.Events = append(history.Events, Event{
				Kind: Message, Role: "assistant", Timestamp: base,
				Parts: []Part{{Kind: Text, Text: fmt.Sprintf("filler %03d", len(history.Events))}},
			})
		}
	}
	// Three pairs whose calls and results are separated by different gaps, plus
	// two calls opened together and answered together, plus one call answered
	// by media rather than text.
	call := func(id string) {
		history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: base,
			Parts: []Part{{Kind: ToolCall, CallID: id, ToolName: "Bash", Data: json.RawMessage(`{"command":"x"}`)}}})
	}
	result := func(id string) {
		history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Timestamp: base,
			Parts: []Part{{Kind: ToolResult, CallID: id, Text: id + " done"}}})
	}
	filler(5)
	call("near")
	result("near")
	filler(4)
	call("far")
	filler(9)
	result("far")
	filler(3)
	call("pair-a")
	call("pair-b")
	filler(6)
	history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Timestamp: base, Parts: []Part{
		{Kind: ToolResult, CallID: "pair-a", Text: "a done"},
		{Kind: ToolResult, CallID: "pair-b", Text: "b done"},
	}})
	filler(2)
	call("shot")
	history.Events = append(history.Events, Event{Kind: Message, Role: "user", Timestamp: base, Parts: []Part{
		{Kind: Media, CallID: "shot", MediaType: "image/png", MediaData: "eA=="},
	}})
	filler(4)
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}

	opts := CompactOptions{Budget: 1000, ArchiveDir: filepath.Join(t.TempDir(), "archive"), Now: base}
	collapsedAtLeastOnce := false
	for keepRecent := 1; keepRecent <= len(history.Events); keepRecent++ {
		arch := newArchiveBuilder(opts.ArchiveDir, history, opts)
		events, collapsed := collapseOldTurns(cloneEvents(history.Events), keepRecent, cwd, arch, opts)
		candidate := cloneSession(history)
		candidate.Events = events
		if err := candidate.Validate(); err != nil {
			t.Fatalf("keepRecent=%d produced an invalid session: %v", keepRecent, err)
		}
		if collapsed > 0 {
			collapsedAtLeastOnce = true
			if len(events) >= len(history.Events) {
				t.Fatalf("keepRecent=%d reported %d collapsed but kept every event", keepRecent, collapsed)
			}
		}
		// A collapse must never leave the thread claiming a result for work it
		// no longer contains.
		defined := map[string]bool{}
		for _, event := range events {
			for _, part := range event.Parts {
				if part.Kind == ToolCall {
					defined[part.CallID] = true
				}
			}
		}
		for _, event := range events {
			for _, part := range event.Parts {
				if part.CallID != "" && (part.Kind == ToolResult || part.Kind == Media) && !defined[part.CallID] {
					t.Fatalf("keepRecent=%d kept a result for collapsed call %q", keepRecent, part.CallID)
				}
			}
		}
	}
	if !collapsedAtLeastOnce {
		t.Fatal("no boundary in the sweep collapsed anything, so the invariant was never tested")
	}
}

func TestCompactSaysSoWhenNothingCanBeAbridged(t *testing.T) {
	cwd := t.TempDir()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	// No reasoning, no tool output, no attachments, every message below the
	// truncation floor, and too few turns for the collapse step to engage: the
	// ladder has nothing to take, and the thread is still over the budget.
	history := &Session{Source: Kimi, SourceID: "irreducible-small", CWD: cwd, CreatedAt: base}
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history.Events = append(history.Events, Event{
			Kind: Message, Role: role, Timestamp: base,
			Parts: []Part{{Kind: Text, Text: fmt.Sprintf("turn %d ", i) + strings.Repeat("c", 1800)}},
		})
	}
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	out, archive, report := compactFixture(t, history, 2_000)
	if report.Fit {
		t.Fatalf("a session over the budget was reported as fitting: %+v", report)
	}
	if !report.removedNothing() {
		t.Fatalf("something was removed from an irreducible session: %+v", report)
	}
	if archive != nil {
		t.Fatal("an archive was created with nothing to put in it")
	}
	if !strings.Contains(report.Summary(), "nothing in it could be abridged") {
		t.Fatalf("summary reads as a success: %q", report.Summary())
	}
	if !strings.Contains(strings.Join(out.Warnings, "\n"), "may exceed its context window") {
		t.Fatalf("the user is not warned: %q", out.Warnings)
	}
}

// The search stops at the first rung that fits, so a rung that removed less
// than the one before it would be unreachable — and worse, would let a session
// settle at a reduction it did not need.
func TestLadderRungsOnlyEverRemoveMore(t *testing.T) {
	rungs := ladder()
	if len(rungs) < 2 || rungs[0] != (reduction{}) {
		t.Fatalf("the ladder must open with a measure-only rung, got %+v", rungs[0])
	}
	looser := func(a, b int) bool { return b != 0 && (a == 0 || a > b) }
	for i := 1; i < len(rungs); i++ {
		prev, next := rungs[i-1], rungs[i]
		if prev.dropReasoning && !next.dropReasoning {
			t.Errorf("rung %d stopped dropping reasoning", i)
		}
		if prev.stripMedia && !next.stripMedia {
			t.Errorf("rung %d stopped stripping media", i)
		}
		if looser(next.toolResultLimit, prev.toolResultLimit) {
			t.Errorf("rung %d raised the tool result limit from %d to %d", i, prev.toolResultLimit, next.toolResultLimit)
		}
		if looser(next.textLimit, prev.textLimit) {
			t.Errorf("rung %d raised the text limit from %d to %d", i, prev.textLimit, next.textLimit)
		}
		if prev.keepRecent != 0 && next.keepRecent > prev.keepRecent {
			t.Errorf("rung %d kept more recent turns (%d) than rung %d (%d)", i, next.keepRecent, i-1, prev.keepRecent)
		}
		if prev == next {
			t.Errorf("rung %d is identical to rung %d", i, i-1)
		}
	}
	// The cheapest reduction must be reachable on its own, so a session that
	// only needs its reasoning dropped does not also lose tool output.
	if rungs[1] != (reduction{dropReasoning: true}) {
		t.Fatalf("the first reduction rung is %+v, want reasoning alone", rungs[1])
	}
}

func TestArchiveDefaultsIntoTheProjectAndHidesItselfFromGit(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	candidate := teleportFixture(t, claudeAdapter{}, cwd, bulkyHistory(t, cwd, 20, 4000))
	manager := NewManagerWith(claudeAdapter{}, codexAdapter{})
	result, _, err := manager.Teleport(context.Background(), candidate, Codex, TransferOptions{
		WriteOptions: WriteOptions{CWD: cwd},
		Compact:      &CompactOptions{Budget: 8_000, Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The archive must land where a target confined to the project can read it.
	want := filepath.Join(cwd, DefaultArchiveDirName)
	if filepath.Dir(result.ArchivePath) != want {
		t.Fatalf("archive went to %s, want a directory under %s", result.ArchivePath, want)
	}
	// It is a complete copy of a session, so it must not become a commit.
	ignore, err := os.ReadFile(filepath.Join(result.ArchivePath, ".gitignore"))
	if err != nil {
		t.Fatalf("archive has no .gitignore: %v", err)
	}
	if strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("archive .gitignore = %q, want everything ignored", ignore)
	}
	// Every marker must resolve inside the project.
	back, err := (codexAdapter{}).Read(context.Background(), candidateForResult(Codex, result, cwd))
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, line := range strings.Split(renderEvents(back.Events), "\n") {
		path, ok := markerPath(line)
		if !ok {
			continue
		}
		markers++
		rel, err := filepath.Rel(cwd, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("marker points outside the project the target will run in: %s", path)
		}
	}
	if markers == 0 {
		t.Fatal("no marker was written, so the path could not be checked")
	}
}
