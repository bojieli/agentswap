package session

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The digest is what replaces a written summary. Every line of it is derived
// from the recorded events — the opening request, the paths the tool calls
// wrote, the commands they ran, the latest plan, the calls that never came
// back — so it says what the session was doing without a model ever reading
// the history back.

const (
	digestTaskLimit    = 2000
	digestFileLimit    = 40
	digestReadLimit    = 25
	digestCommandLimit = 25
)

type pathLedger struct {
	writeCounts map[string]int
	writeOrder  []string
	readCounts  map[string]int
	readOrder   []string
	commands    []string
	seenCommand map[string]bool
	dangling    []string
	plan        string
	tools       map[string]int
	toolOrder   []string
}

func newPathLedger() *pathLedger {
	return &pathLedger{
		writeCounts: make(map[string]int), readCounts: make(map[string]int),
		seenCommand: make(map[string]bool), tools: make(map[string]int),
	}
}

// deriveLedger walks an event stream and extracts the state a resuming agent
// needs: what changed on disk, what was run, what is still open.
func deriveLedger(events []Event, cwd string) *pathLedger {
	l := newPathLedger()
	calls := make(map[string]string)
	answered := make(map[string]bool)
	var order []string
	for _, event := range events {
		if event.Kind == Plan {
			if strings.TrimSpace(event.PlanText) != "" {
				l.plan = event.PlanText
			}
			continue
		}
		for _, part := range event.Parts {
			switch part.Kind {
			case ToolCall:
				calls[part.CallID] = part.ToolName
				order = append(order, part.CallID)
				if _, seen := l.tools[part.ToolName]; !seen {
					l.toolOrder = append(l.toolOrder, part.ToolName)
				}
				l.tools[part.ToolName]++
				l.record(part, cwd)
			case ToolResult:
				answered[part.CallID] = true
			case Media:
				if part.CallID != "" {
					answered[part.CallID] = true
				}
			}
		}
	}
	for _, id := range order {
		if !answered[id] {
			l.dangling = append(l.dangling, fmt.Sprintf("%s (%s)", id, calls[id]))
		}
	}
	return l
}

func (l *pathLedger) record(part Part, cwd string) {
	switch toolClass(part.ToolName) {
	case "shell":
		if command := extractCommand(part.Data); command != "" && !l.seenCommand[command] {
			l.seenCommand[command] = true
			l.commands = append(l.commands, command)
		}
	case "write":
		for _, path := range extractPaths(part.Data) {
			path = relativeTo(cwd, path)
			if _, seen := l.writeCounts[path]; !seen {
				l.writeOrder = append(l.writeOrder, path)
			}
			l.writeCounts[path]++
		}
	case "read":
		for _, path := range extractPaths(part.Data) {
			path = relativeTo(cwd, path)
			if _, seen := l.readCounts[path]; !seen {
				l.readOrder = append(l.readOrder, path)
			}
			l.readCounts[path]++
		}
	}
}

// toolClass sorts a tool name into the three kinds the digest reports on. It is
// a naming heuristic and nothing depends on it being right: a misfiled tool
// shows up under the wrong heading in a summary, and the call itself is still
// in the transcript and the archive either way.
func toolClass(name string) string {
	lower := strings.ToLower(name)
	for _, marker := range []string{"bash", "shell", "exec", "terminal", "command"} {
		if strings.Contains(lower, marker) {
			return "shell"
		}
	}
	for _, marker := range []string{"edit", "write", "patch", "create", "update", "insert", "append", "replace", "move", "rename", "delete", "mkdir"} {
		if strings.Contains(lower, marker) {
			return "write"
		}
	}
	for _, marker := range []string{"read", "cat", "view", "open", "grep", "glob", "search", "find", "list", "fetch"} {
		if strings.Contains(lower, marker) {
			return "read"
		}
	}
	return ""
}

var pathKeys = []string{"file_path", "filePath", "path", "notebook_path", "notebookPath", "target_file", "filename", "file", "absolute_path"}

// extractPaths pulls file paths out of a recorded tool input. Harnesses agree
// on very little here, so the common key names are tried first and Codex's
// apply_patch envelope — which names its files inside the patch text — second.
func extractPaths(data json.RawMessage) []string {
	var obj map[string]any
	if len(data) == 0 || json.Unmarshal(data, &obj) != nil {
		return nil
	}
	var paths []string
	for _, key := range pathKeys {
		if value := stringValue(obj[key]); value != "" {
			paths = append(paths, value)
		}
	}
	// A batched edit tool records its targets in a list of operations.
	for _, key := range []string{"edits", "files", "operations", "changes"} {
		items, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			nested, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, pathKey := range pathKeys {
				if value := stringValue(nested[pathKey]); value != "" {
					paths = append(paths, value)
				}
			}
		}
	}
	if len(paths) == 0 {
		for _, key := range []string{"input", "patch", "diff", "content"} {
			paths = append(paths, patchPaths(stringValue(obj[key]))...)
		}
	}
	return dedupeStrings(paths)
}

// patchPaths reads the file headers of an apply_patch envelope.
func patchPaths(text string) []string {
	if !strings.Contains(text, "*** ") {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
			if strings.HasPrefix(line, prefix) {
				if value := strings.TrimSpace(strings.TrimPrefix(line, prefix)); value != "" {
					paths = append(paths, value)
				}
			}
		}
	}
	return paths
}

// extractCommand reads a shell invocation, which arrives either as a string or
// as the argv list Codex records.
func extractCommand(data json.RawMessage) string {
	var obj map[string]any
	if len(data) == 0 || json.Unmarshal(data, &obj) != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd", "script", "input"} {
		switch value := obj[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return collapseWhitespace(value)
			}
		case []any:
			var argv []string
			for _, item := range value {
				argv = append(argv, stringValue(item))
			}
			if len(argv) == 3 && (argv[0] == "bash" || argv[0] == "sh" || argv[0] == "zsh") && strings.HasPrefix(argv[1], "-") {
				return collapseWhitespace(argv[2])
			}
			if joined := strings.TrimSpace(strings.Join(argv, " ")); joined != "" {
				return collapseWhitespace(joined)
			}
		}
	}
	return ""
}

func collapseWhitespace(value string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// buildDigest writes the note that goes in front of an abridged thread.
func buildDigest(history *Session, opts CompactOptions, counts reductionCounts) string {
	l := deriveLedger(history.Events, history.CWD)
	var b strings.Builder
	b.WriteString("<agentswap-transferred-context>\n")
	fmt.Fprintf(&b, "This session was transferred from %s (source session %s) by agentswap.\n",
		history.Source.Display(), history.SourceID)
	b.WriteString("The conversation below is ABRIDGED so it fits this harness's context window.\n\n")

	b.WriteString("COMPLETE ORIGINAL HISTORY\n")
	fmt.Fprintf(&b, "  %s\n", opts.ArchiveDir)
	b.WriteString("    INDEX.md      what was removed, and which file holds each piece\n")
	b.WriteString("    history.json  the complete session, machine readable\n")
	b.WriteString("    events/       the full text of every elided tool result and message\n")
	b.WriteString("  Read those files before concluding that something is missing. Wherever\n")
	b.WriteString("  content was removed, the transcript below carries an inline marker that\n")
	b.WriteString("  says \"agentswap:elided\" and ends with the absolute path of the file\n")
	b.WriteString("  holding the complete text. Open that file rather than guessing.\n\n")

	if task := firstUserText(history.Events); task != "" {
		b.WriteString("ORIGINAL REQUEST\n")
		writeIndented(&b, truncateRunes(task, digestTaskLimit))
		b.WriteString("\n")
	}
	writeLedger(&b, l)
	writeRemoved(&b, counts)
	b.WriteString("</agentswap-transferred-context>")
	return b.String()
}

// buildElisionBrief stands in for the turns a collapse removed, in the place
// they were removed from.
func buildElisionBrief(elided []Event, cwd, path string) string {
	l := deriveLedger(elided, cwd)
	var b strings.Builder
	b.WriteString("<agentswap-elided-turns>\n")
	fmt.Fprintf(&b, "%d earlier %s were removed here to fit this harness's context window.\n",
		len(elided), plural(len(elided), "turn", "turns"))
	fmt.Fprintf(&b, "Their full text is at: %s\n", path)
	if summary := eventRoleSummary(elided); summary != "" {
		fmt.Fprintf(&b, "They contained: %s.\n", summary)
	}
	b.WriteString("\n")
	writeLedger(&b, l)
	b.WriteString("</agentswap-elided-turns>")
	return b.String()
}

func writeLedger(b *strings.Builder, l *pathLedger) {
	if len(l.writeOrder) > 0 {
		fmt.Fprintf(b, "FILES WRITTEN (%d, from the recorded tool calls)\n", len(l.writeOrder))
		for i, path := range sortedPathCounts(l.writeCounts, append([]string(nil), l.writeOrder...)) {
			if i == digestFileLimit {
				fmt.Fprintf(b, "  ... and %d more, listed in INDEX.md\n", len(l.writeOrder)-digestFileLimit)
				break
			}
			fmt.Fprintf(b, "  %s (%d %s)\n", path, l.writeCounts[path], plural(l.writeCounts[path], "call", "calls"))
		}
		b.WriteString("\n")
	}
	if len(l.readOrder) > 0 {
		fmt.Fprintf(b, "FILES READ (%d)\n", len(l.readOrder))
		shown := l.readOrder
		if len(shown) > digestReadLimit {
			shown = shown[:digestReadLimit]
		}
		fmt.Fprintf(b, "  %s\n", strings.Join(shown, ", "))
		if len(l.readOrder) > digestReadLimit {
			fmt.Fprintf(b, "  ... and %d more\n", len(l.readOrder)-digestReadLimit)
		}
		b.WriteString("\n")
	}
	if len(l.commands) > 0 {
		fmt.Fprintf(b, "COMMANDS RUN (%d distinct)\n", len(l.commands))
		shown := l.commands
		if len(shown) > digestCommandLimit {
			shown = shown[len(shown)-digestCommandLimit:]
			// The most recent commands are the ones a resuming agent needs, so
			// the older ones are dropped — but never silently, because a list
			// that looks complete and is not would be read as the whole story.
			fmt.Fprintf(b, "  ... %d earlier %s omitted; the archive has them all\n",
				len(l.commands)-digestCommandLimit, plural(len(l.commands)-digestCommandLimit, "command", "commands"))
		}
		for _, command := range shown {
			fmt.Fprintf(b, "  %s\n", truncateRunes(command, 200))
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(l.plan) != "" {
		b.WriteString("LATEST PLAN\n")
		writeIndented(b, l.plan)
		b.WriteString("\n")
	}
	if len(l.dangling) > 0 {
		b.WriteString("UNFINISHED WORK\n")
		for _, id := range l.dangling {
			fmt.Fprintf(b, "  tool call %s never recorded a result\n", id)
		}
		b.WriteString("\n")
	}
}

func writeRemoved(b *strings.Builder, counts reductionCounts) {
	var lines []string
	if counts.reasoning > 0 {
		lines = append(lines, fmt.Sprintf("%d recorded reasoning %s", counts.reasoning, plural(counts.reasoning, "block", "blocks")))
	}
	if counts.toolResults > 0 {
		lines = append(lines, fmt.Sprintf("%d tool %s truncated", counts.toolResults, plural(counts.toolResults, "result", "results")))
	}
	if counts.textParts > 0 {
		lines = append(lines, fmt.Sprintf("%d long %s truncated", counts.textParts, plural(counts.textParts, "message", "messages")))
	}
	if counts.media > 0 {
		lines = append(lines, fmt.Sprintf("%d inline %s", counts.media, plural(counts.media, "attachment", "attachments")))
	}
	if counts.collapsed > 0 {
		lines = append(lines, fmt.Sprintf("%d earlier %s collapsed", counts.collapsed, plural(counts.collapsed, "turn", "turns")))
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("REMOVED FROM THIS TRANSCRIPT\n")
	for _, line := range lines {
		fmt.Fprintf(b, "  %s\n", line)
	}
	b.WriteString("\n")
}

func eventRoleSummary(events []Event) string {
	var user, assistant, tool, plans int
	for _, event := range events {
		switch {
		case event.Kind == Plan:
			plans++
		case event.Role == "user":
			user++
		case event.Role == "assistant":
			assistant++
		case event.Role == "tool":
			tool++
		}
	}
	var parts []string
	for _, item := range []struct {
		n    int
		one  string
		many string
	}{
		{user, "user message", "user messages"},
		{assistant, "assistant message", "assistant messages"},
		{tool, "tool result", "tool results"},
		{plans, "plan", "plans"},
	} {
		if item.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", item.n, plural(item.n, item.one, item.many)))
		}
	}
	return strings.Join(parts, ", ")
}

func firstUserText(events []Event) string {
	for _, event := range events {
		if event.Kind != Message || event.Role != "user" {
			continue
		}
		for _, part := range event.Parts {
			if part.Kind == Text && strings.TrimSpace(part.Text) != "" {
				return strings.TrimSpace(part.Text)
			}
		}
	}
	return ""
}

func writeIndented(b *strings.Builder, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + fmt.Sprintf("\n  [... %d more characters, see the archive]", len(runes)-limit)
}

// renderEvents writes an event stream as plain text. This is the form the
// archive keeps, because every harness can read a text file and none of them
// need agentswap installed to do it.
func renderEvents(events []Event) string {
	var b strings.Builder
	for i, event := range events {
		role := event.Role
		if event.Kind == Plan {
			role = "plan"
		}
		fmt.Fprintf(&b, "--- [%d] %s", i, role)
		if !event.Timestamp.IsZero() {
			fmt.Fprintf(&b, " %s", event.Timestamp.UTC().Format("2006-01-02T15:04:05Z"))
		}
		b.WriteString("\n")
		if event.Kind == Plan {
			b.WriteString(event.PlanText)
			b.WriteString("\n\n")
			continue
		}
		for _, part := range event.Parts {
			switch part.Kind {
			case Text:
				b.WriteString(part.Text)
				b.WriteString("\n")
			case Reasoning:
				b.WriteString("<reasoning>\n")
				b.WriteString(part.Text)
				b.WriteString("\n</reasoning>\n")
			case ToolCall:
				fmt.Fprintf(&b, "<tool_call name=%s id=%s>\n%s\n</tool_call>\n",
					strconv.Quote(part.ToolName), strconv.Quote(part.CallID), string(part.Data))
			case ToolResult:
				status := ""
				if part.Error {
					status = " error=true"
				}
				fmt.Fprintf(&b, "<tool_result id=%s%s>\n%s\n</tool_result>\n", strconv.Quote(part.CallID), status, part.Text)
			case Media:
				fmt.Fprintf(&b, "<media type=%s%s/>\n", strconv.Quote(part.MediaType), filenameField(part.Filename))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
