package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Compaction abridges a validated session so a target harness with a smaller
// context window can still resume it, and moves everything it removes into an
// archive the target reads on demand.
//
// The reduction is mechanical. Nothing here asks a model to summarize the
// history, because that would make a transfer non-deterministic, put it back
// on the network, and give it a credential — all three of which teleport is
// built to avoid. What replaces a summary is a derived digest: the task
// statement, the files the recorded tool calls wrote, the commands they ran,
// the latest plan, and the unfinished work, all extracted from the events
// themselves.
//
// The estimate below is deliberately approximate. agentswap cannot know the
// target's real window — the user may pass any --model straight through to the
// target CLI — so the budget is a transcript budget the user can override, and
// the token count errs toward over-counting so a budget is not quietly
// exceeded.

// bytesPerASCIIToken is the divisor for plain ASCII text. 3.5 sits between
// English prose (about 4 bytes per token) and dense source code (about 3), and
// rounds toward reporting more tokens rather than fewer.
const (
	asciiTokenNumerator   = 2
	asciiTokenDenominator = 7

	// mediaTokenEstimate is the flat cost charged for one inline image. Image
	// tokens have no relation to payload bytes — a megabyte of base64 is on the
	// order of a thousand tokens — so charging by size would over-compact a
	// session that pasted a few screenshots.
	mediaTokenEstimate = 1600

	// Structural overhead per event and per part, covering the role marker and
	// the block framing every harness adds around recorded content.
	eventTokenOverhead = 8
	partTokenOverhead  = 4
)

// DefaultBudget is the transcript budget used when the user names a target but
// not a size. These are budgets for the replayed conversation only: the target
// still needs room for its system prompt, its tool definitions, and the work
// it is being resumed to do. They sit well below any supported harness's window
// on purpose, and --budget overrides them.
func DefaultBudget(target Agent) int {
	switch target {
	case Claude, Codex, Kimi:
		return 120_000
	case OpenCode:
		// OpenCode runs whichever model its own configuration selects, which
		// agentswap cannot read from here, so it gets the most cautious value.
		return 100_000
	default:
		return 100_000
	}
}

// EstimateTokens approximates what a session's main thread costs when a target
// replays it. Branches are excluded: a delegated run is a separate transcript
// that the main model never read, and resuming the main thread does not load
// it, so counting it would compact a session that was never too large.
func EstimateTokens(s *Session) int {
	if s == nil {
		return 0
	}
	return estimateEvents(s.Events)
}

func estimateEvents(events []Event) int {
	total := 0
	for _, event := range events {
		total += eventTokenOverhead
		if event.Kind == Plan {
			total += estimateText(event.PlanText)
			continue
		}
		for _, part := range event.Parts {
			total += partTokenOverhead
			switch part.Kind {
			case Media:
				total += mediaTokenEstimate
			case ToolCall:
				total += estimateText(part.ToolName) + estimateText(string(part.Data))
			default:
				total += estimateText(part.Text)
			}
		}
	}
	return total
}

// estimateText counts ASCII by byte and everything else by rune. A CJK
// character is usually a token on its own, and a character outside the basic
// multilingual plane — an emoji, most commonly — is usually two.
func estimateText(s string) int {
	ascii, tokens := 0, 0
	for _, r := range s {
		switch {
		case r < 0x80:
			ascii++
		case r > 0xFFFF:
			tokens += 2
		default:
			tokens++
		}
	}
	return tokens + (ascii*asciiTokenNumerator+asciiTokenDenominator-1)/asciiTokenDenominator
}

// CompactOptions configures one compaction. ArchiveDir is the directory the
// archive will occupy; it appears verbatim in every elision marker, so it is
// decided before the reduction runs rather than after the target is written.
//
// ArchiveRoot moves the archive out of the project's own DefaultArchiveDirName
// and under a chosen parent instead. Somewhere the target cannot reach is a
// deliberate choice: a resumed agent is confined to its working directory, so
// an archive outside it needs the user's approval and a non-interactive resume
// cannot ask for it. ArchiveDir wins when both are set.
type CompactOptions struct {
	Budget      int
	ArchiveDir  string
	ArchiveRoot string
	Version     string
	Now         time.Time
}

// CompactionReport states what a compaction cost, in the terms a reader needs
// to decide whether the transfer kept enough.
type CompactionReport struct {
	Budget          int    `json:"budget_tokens"`
	Before          int    `json:"before_tokens"`
	After           int    `json:"after_tokens"`
	Fit             bool   `json:"fit"`
	ArchivePath     string `json:"archive_path"`
	ReasoningParts  int    `json:"reasoning_parts_dropped"`
	MediaParts      int    `json:"media_parts_archived"`
	ToolResults     int    `json:"tool_results_truncated"`
	TextParts       int    `json:"text_parts_truncated"`
	EventsCollapsed int    `json:"events_collapsed"`
}

// reduction is one rung of the ladder. Each rung is applied to the pristine
// source rather than to the previous rung's output, so a marker is never
// written over a marker and a rung's cost is exactly what its fields say.
type reduction struct {
	dropReasoning   bool
	toolResultLimit int
	textLimit       int
	stripMedia      bool
	keepRecent      int
}

// ladder orders the reductions by value: recorded reasoning first because the
// target cannot use it anyway, then the tool output that holds nearly all of a
// coding session's bytes, then long pasted text, then images, and only last the
// collapse of whole turns, which is the one step that removes conversation.
//
// The first rung is the empty one, which measures the session as it stands. The
// limits are byte counts, and each rung must remove at least as much as the one
// before it, because the search stops at the first rung that fits.
func ladder() []reduction {
	return []reduction{
		{},
		{dropReasoning: true},
		{dropReasoning: true, toolResultLimit: 8000},
		{dropReasoning: true, toolResultLimit: 2000},
		{dropReasoning: true, toolResultLimit: 600},
		{dropReasoning: true, toolResultLimit: 600, textLimit: 6000},
		{dropReasoning: true, toolResultLimit: 600, textLimit: 2000},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000, stripMedia: true},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000, stripMedia: true, keepRecent: 60},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000, stripMedia: true, keepRecent: 30},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000, stripMedia: true, keepRecent: 14},
		{dropReasoning: true, toolResultLimit: 300, textLimit: 2000, stripMedia: true, keepRecent: 6},
	}
}

// Compact reduces history until the estimate fits opts.Budget, returning the
// session to transfer, the archive holding everything removed, and a report.
// The source is never modified. The returned session is always valid: a rung
// that produced an invalid session would be a bug in the reduction, not a
// property of the source, so it is reported as an error rather than written.
func Compact(history *Session, opts CompactOptions) (*Session, *Archive, CompactionReport, error) {
	if history == nil {
		return nil, nil, CompactionReport{}, fmt.Errorf("nil session")
	}
	if opts.ArchiveDir == "" {
		return nil, nil, CompactionReport{}, fmt.Errorf("compaction needs an archive directory")
	}
	if opts.Budget <= 0 {
		return nil, nil, CompactionReport{}, fmt.Errorf("compaction needs a positive token budget")
	}
	if opts.Now.IsZero() {
		opts.Now = timestampOr(history.UpdatedAt, history.CreatedAt)
	}
	before := EstimateTokens(history)
	report := CompactionReport{Budget: opts.Budget, Before: before, After: before, ArchivePath: opts.ArchiveDir}

	rungs := ladder()
	var chosen *Session
	var chosenArchive *Archive
	for i, rung := range rungs {
		candidate, archive, counts := applyReduction(history, rung, opts)
		after := EstimateTokens(candidate)
		if err := candidate.Validate(); err != nil {
			return nil, nil, report, fmt.Errorf("compaction produced an untransferable session at level %d: %w", i, err)
		}
		chosen, chosenArchive = candidate, archive
		report.After = after
		report.ReasoningParts = counts.reasoning
		report.MediaParts = counts.media
		report.ToolResults = counts.toolResults
		report.TextParts = counts.textParts
		report.EventsCollapsed = counts.collapsed
		if after <= opts.Budget {
			report.Fit = true
			break
		}
	}
	// A session the ladder did not touch has nothing to archive: the transcript
	// is complete, carries no markers, and would only be pointed at a copy of
	// itself. That is the ordinary case of a session that already fits — but it
	// also covers a session with nothing removable in it, which does not fit
	// and must not be reported as though it did.
	if report.removedNothing() {
		if !report.Fit {
			chosen.Warnings = appendUnique(chosen.Warnings, fmt.Sprintf(
				"the thread is about %s tokens against a %s token budget and holds nothing that could be abridged; the target may exceed its context window",
				humanCount(report.After), humanCount(opts.Budget)))
		}
		return chosen, nil, report, nil
	}
	if report.Fit {
		chosen.Warnings = appendUnique(chosen.Warnings, fmt.Sprintf(
			"the thread was compacted from about %s to about %s tokens to fit the %s token budget; the complete history is archived at %s",
			humanCount(before), humanCount(report.After), humanCount(opts.Budget), opts.ArchiveDir))
	} else {
		chosen.Warnings = appendUnique(chosen.Warnings, fmt.Sprintf(
			"compaction reached its floor at about %s tokens, still above the %s token budget; the target may exceed its context window. The complete history is archived at %s",
			humanCount(report.After), humanCount(opts.Budget), opts.ArchiveDir))
	}
	chosenArchive.Manifest.Compaction = report
	return chosen, chosenArchive, report, nil
}

type reductionCounts struct {
	reasoning   int
	media       int
	toolResults int
	textParts   int
	collapsed   int
}

// applyReduction builds one candidate from the pristine source.
func applyReduction(history *Session, r reduction, opts CompactOptions) (*Session, *Archive, reductionCounts) {
	arch := newArchiveBuilder(opts.ArchiveDir, history, opts)
	out := cloneSession(history)
	var counts reductionCounts

	// A result recorded in a different event than the media that also answers
	// the same call cannot become a second tool_result without tripping the
	// duplicate-result rule, so the original pairing is measured up front.
	resultEvent := resultEventIndex(history.Events)

	events := make([]Event, 0, len(out.Events))
	for i, event := range out.Events {
		if event.Kind == Plan {
			events = append(events, event)
			continue
		}
		parts := make([]Part, 0, len(event.Parts))
		for _, part := range event.Parts {
			switch {
			case part.Kind == Reasoning && r.dropReasoning:
				counts.reasoning++
				continue
			case part.Kind == Media && r.stripMedia:
				converted, ok := archiveMedia(part, event.Role, i, resultEvent, arch)
				counts.media++
				if !ok {
					continue
				}
				parts = append(parts, converted)
			case part.Kind == ToolResult && r.toolResultLimit > 0 && len(part.Text) > r.toolResultLimit:
				path := arch.addToolResult(i, part)
				part.Text = truncateWithMarker(part.Text, r.toolResultLimit, path)
				counts.toolResults++
				parts = append(parts, part)
			case (part.Kind == Text) && r.textLimit > 0 && len(part.Text) > r.textLimit:
				path := arch.addText(i, event.Role, part)
				part.Text = truncateWithMarker(part.Text, r.textLimit, path)
				counts.textParts++
				parts = append(parts, part)
			default:
				parts = append(parts, part)
			}
		}
		// An assistant turn that held nothing but recorded reasoning has no
		// content left once the reasoning is dropped. It carries no tool call,
		// so removing the turn cannot orphan anything.
		if len(parts) == 0 {
			continue
		}
		event.Parts = parts
		events = append(events, event)
	}

	if r.keepRecent > 0 {
		var collapsed int
		events, collapsed = collapseOldTurns(events, r.keepRecent, history.CWD, arch, opts)
		counts.collapsed = collapsed
	}

	// The title is pinned before the digest goes in front, because every writer
	// falls back to the first user message for a session title and the digest
	// would otherwise become it.
	if strings.TrimSpace(out.Title) == "" {
		out.Title = firstText(history)
	}
	if counts != (reductionCounts{}) {
		digest := Event{
			Kind: Message, Role: "user", Timestamp: firstTimestamp(events, opts.Now),
			Parts: []Part{{Kind: Text, Text: buildDigest(history, opts, counts)}},
		}
		events = append([]Event{digest}, events...)
	}
	out.Events = events
	return out, arch.archive(), counts
}

// archiveMedia replaces an inline payload with a pointer to the file the
// archive holds it in. A tool or user message must express that pointer as a
// tool result so the call it answers stays answered; an assistant message must
// express it as text, because a tool result there is not representable.
func archiveMedia(part Part, role string, eventIndex int, resultEvent map[string]int, arch *archiveBuilder) (Part, bool) {
	path := arch.addMedia(eventIndex, part)
	marker := fmt.Sprintf("[agentswap:elided media type=%s%s; full text: %s]", part.MediaType, filenameField(part.Filename), path)
	if role == "assistant" || part.CallID == "" {
		if role == "tool" {
			// Nothing in a tool message may carry plain text, and without a
			// call id there is no result to attach the pointer to. The payload
			// is still in the archive and still counted in the digest.
			return Part{}, false
		}
		return Part{Kind: Text, Text: marker}, true
	}
	if at, exists := resultEvent[part.CallID]; exists && at != eventIndex {
		// The call already has its result in another event; adding a second one
		// here would make the session invalid.
		return Part{}, false
	}
	return Part{Kind: ToolResult, CallID: part.CallID, Text: marker}, true
}

func filenameField(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return " name=" + strconv.Quote(name)
}

// resultEventIndex records which event holds the tool_result for each call.
func resultEventIndex(events []Event) map[string]int {
	at := make(map[string]int)
	for i, event := range events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult && part.CallID != "" {
				at[part.CallID] = i
			}
		}
	}
	return at
}

// truncateWithMarker keeps the opening and closing of a long payload and names
// the file holding the rest. Both ends matter: the opening of a command's
// output says what ran, and the closing says how it ended.
func truncateWithMarker(text string, limit int, path string) string {
	if limit < 200 {
		limit = 200
	}
	if limit >= len(text) {
		return text
	}
	head := limit * 6 / 10
	tail := limit - head
	headText := cutAtLineBoundary(text[:head], true)
	tailText := cutAtLineBoundary(text[len(text)-tail:], false)
	elided := len(text) - len(headText) - len(tailText)
	if elided <= 0 {
		return text
	}
	lines := strings.Count(text, "\n") + 1
	kept := strings.Count(headText, "\n") + strings.Count(tailText, "\n") + 2
	elidedLines := lines - kept
	if elidedLines < 0 {
		elidedLines = 0
	}
	return headText +
		fmt.Sprintf("\n\n[agentswap:elided %s bytes, about %s lines; full text: %s]\n\n",
			humanCount(elided), humanCount(elidedLines), path) +
		tailText
}

// cutAtLineBoundary trims a slice back to a newline so a truncation does not
// land mid-line. It gives up when the boundary would cost more than a quarter
// of the slice, which is what happens to minified or single-line payloads.
func cutAtLineBoundary(s string, fromEnd bool) string {
	if s == "" {
		return s
	}
	limit := len(s) / 4
	if fromEnd {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 && len(s)-i <= limit {
			return s[:i]
		}
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 && i <= limit {
		return s[i+1:]
	}
	return s
}

// collapseOldTurns replaces the middle of a thread with one brief, keeping the
// opening request and the recent turns verbatim.
//
// The boundary is not free to fall anywhere. A kept turn holding a tool result
// whose call was collapsed away would be an invalid session, so the boundary
// moves back until every result in the kept tail has its call beside it.
func collapseOldTurns(events []Event, keepRecent int, cwd string, arch *archiveBuilder, opts CompactOptions) ([]Event, int) {
	if len(events) <= keepRecent+2 {
		return events, 0
	}
	head := 0
	// The opening request is the most load-bearing message in a session, so it
	// survives — but only when it is a plain user turn. A first event holding a
	// tool call would leave that call unanswered once the middle is gone.
	if first := events[0]; first.Kind == Message && first.Role == "user" && !hasToolCall(first) {
		head = 1
	}
	start := len(events) - keepRecent
	for start > head {
		defined := make(map[string]bool)
		for _, event := range events[start:] {
			for _, part := range event.Parts {
				if part.Kind == ToolCall {
					defined[part.CallID] = true
				}
			}
		}
		// The boundary only ever moves earlier, so this converges either way;
		// taking the earliest index any unresolved result needs reaches the
		// answer in one pass instead of walking back a call at a time.
		next := start
		for _, event := range events[start:] {
			for _, part := range event.Parts {
				if part.CallID == "" || defined[part.CallID] {
					continue
				}
				if part.Kind != ToolResult && part.Kind != Media {
					continue
				}
				for i := start - 1; i >= 0; i-- {
					if definesCall(events[i], part.CallID) {
						if i < next {
							next = i
						}
						break
					}
				}
			}
		}
		if next == start {
			break
		}
		start = next
	}
	if start <= head {
		return events, 0
	}
	elided := events[head:start]
	path := arch.addCollapsed(head, start, elided)
	brief := Event{
		Kind: Message, Role: "user", Timestamp: firstTimestamp(events[start:], opts.Now),
		Parts: []Part{{Kind: Text, Text: buildElisionBrief(elided, cwd, path)}},
	}
	out := make([]Event, 0, head+1+len(events)-start)
	out = append(out, events[:head]...)
	out = append(out, brief)
	out = append(out, events[start:]...)
	return out, len(elided)
}

func hasToolCall(event Event) bool {
	for _, part := range event.Parts {
		if part.Kind == ToolCall {
			return true
		}
	}
	return false
}

func definesCall(event Event, id string) bool {
	for _, part := range event.Parts {
		if part.Kind == ToolCall && part.CallID == id {
			return true
		}
	}
	return false
}

func firstTimestamp(events []Event, fallback time.Time) time.Time {
	for _, event := range events {
		if !event.Timestamp.IsZero() {
			return event.Timestamp
		}
	}
	return fallback
}

func cloneSession(s *Session) *Session {
	out := *s
	out.Events = cloneEvents(s.Events)
	out.Warnings = append([]string(nil), s.Warnings...)
	if s.Branches != nil {
		out.Branches = make([]Branch, len(s.Branches))
		for i, branch := range s.Branches {
			out.Branches[i] = branch
			out.Branches[i].Events = cloneEvents(branch.Events)
		}
	}
	return &out
}

func cloneEvents(events []Event) []Event {
	if events == nil {
		return nil
	}
	out := make([]Event, len(events))
	for i, event := range events {
		out[i] = event
		if event.Parts != nil {
			out[i].Parts = make([]Part, len(event.Parts))
			for j, part := range event.Parts {
				out[i].Parts[j] = part
				if part.Data != nil {
					out[i].Parts[j].Data = append(json.RawMessage(nil), part.Data...)
				}
			}
		}
	}
	return out
}

// humanCount renders a count the way a person reads one, so a warning about
// 1.8M tokens is not printed as 1800000.
func humanCount(n int) string {
	switch {
	case n >= 10_000_000:
		return strconv.Itoa(n/1_000_000) + "M"
	case n >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64), ".0") + "M"
	case n >= 10_000:
		return strconv.Itoa(n/1000) + "k"
	case n >= 1_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64), ".0") + "k"
	default:
		return strconv.Itoa(n)
	}
}

// ParseBudget reads a token budget written the way people write one: a plain
// integer, or a number with a k or M suffix.
func ParseBudget(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("budget is empty")
	}
	multiplier := 1
	switch last := trimmed[len(trimmed)-1]; last {
	case 'k', 'K':
		multiplier, trimmed = 1_000, trimmed[:len(trimmed)-1]
	case 'm', 'M':
		multiplier, trimmed = 1_000_000, trimmed[:len(trimmed)-1]
	}
	trimmed = strings.TrimSpace(trimmed)
	number, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("budget %q is not a token count (want 120000, 120k, or 1.5M)", value)
	}
	tokens := int(number * float64(multiplier))
	if tokens < 1000 {
		return 0, fmt.Errorf("budget %q is too small to hold a session; use at least 1000 tokens", value)
	}
	return tokens, nil
}

// sortedPathCounts orders a file ledger by how often the session wrote each
// path, so the digest opens with the files the work actually centered on.
func sortedPathCounts(counts map[string]int, order []string) []string {
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return naturalLess(order[i], order[j])
	})
	return order
}

func relativeTo(base, path string) string {
	if base == "" || !filepath.IsAbs(path) {
		return path
	}
	if rel, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// removedNothing reports a compaction that found nothing worth removing,
// because the session already fit.
func (r CompactionReport) removedNothing() bool {
	return r.Before == r.After && r.ReasoningParts == 0 && r.MediaParts == 0 &&
		r.ToolResults == 0 && r.TextParts == 0 && r.EventsCollapsed == 0
}

// Summary is the one-line form the CLI prints for a compaction.
func (r CompactionReport) Summary() string {
	if r.removedNothing() {
		if !r.Fit {
			return fmt.Sprintf("about %s tokens against a %s budget; nothing in it could be abridged",
				humanCount(r.After), humanCount(r.Budget))
		}
		return fmt.Sprintf("already about %s tokens, within the %s budget; nothing was removed",
			humanCount(r.After), humanCount(r.Budget))
	}
	line := fmt.Sprintf("about %s -> about %s tokens (budget %s)",
		humanCount(r.Before), humanCount(r.After), humanCount(r.Budget))
	if !r.Fit {
		line += "; still above the budget"
	}
	return line
}

// Detail names what the compaction removed, or is empty when it removed
// nothing because the session already fit.
func (r CompactionReport) Detail() string {
	var parts []string
	if r.ReasoningParts > 0 {
		parts = append(parts, fmt.Sprintf("%d recorded reasoning %s dropped", r.ReasoningParts, plural(r.ReasoningParts, "block", "blocks")))
	}
	if r.ToolResults > 0 {
		parts = append(parts, fmt.Sprintf("%d tool %s truncated", r.ToolResults, plural(r.ToolResults, "result", "results")))
	}
	if r.TextParts > 0 {
		parts = append(parts, fmt.Sprintf("%d long %s truncated", r.TextParts, plural(r.TextParts, "message", "messages")))
	}
	if r.MediaParts > 0 {
		parts = append(parts, fmt.Sprintf("%d inline %s archived", r.MediaParts, plural(r.MediaParts, "attachment", "attachments")))
	}
	if r.EventsCollapsed > 0 {
		parts = append(parts, fmt.Sprintf("%d earlier %s collapsed", r.EventsCollapsed, plural(r.EventsCollapsed, "turn", "turns")))
	}
	return strings.Join(parts, ", ")
}
