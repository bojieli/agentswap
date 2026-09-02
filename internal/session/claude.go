package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

type claudeAdapter struct{}

func newClaudeAdapter() Adapter    { return claudeAdapter{} }
func (claudeAdapter) Agent() Agent { return Claude }

func claudeRoot() string {
	return envDir("CLAUDE_CONFIG_DIR", filepath.Join(homeDir(), ".claude"))
}

func encodeClaudeProject(path string) string {
	units := utf16.Encode([]rune(filepath.ToSlash(path)))
	var encoded strings.Builder
	encoded.Grow(len(units))
	for _, unit := range units {
		if unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z' || unit >= '0' && unit <= '9' || unit == '-' {
			encoded.WriteByte(byte(unit))
		} else {
			encoded.WriteByte('-')
		}
	}
	return encoded.String()
}

func (claudeAdapter) Discover(_ context.Context, cwd string) ([]Candidate, error) {
	canonical, err := canonicalPath(cwd)
	if err != nil {
		return nil, err
	}
	projectsRoot := filepath.Join(claudeRoot(), "projects")
	dirs := []string{
		filepath.Join(projectsRoot, encodeClaudeProject(filepath.Clean(cwd))),
		filepath.Join(projectsRoot, encodeClaudeProject(canonical)),
	}
	// Claude indexes projects by an irreversible path encoding. Probe the fast
	// exact paths first, then inspect the remaining project directories so a
	// session created through a different symlink to the same directory is not
	// missed.
	projectEntries, readErr := os.ReadDir(projectsRoot)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	for _, entry := range projectEntries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(projectsRoot, entry.Name()))
		}
	}
	seenDirs := make(map[string]struct{})
	seen := make(map[string]struct{})
	var out []Candidate
	for _, dir := range dirs {
		if _, ok := seenDirs[dir]; ok {
			continue
		}
		seenDirs[dir] = struct{}{}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			full := filepath.Join(dir, entry.Name())
			if _, ok := seen[full]; ok {
				continue
			}
			seen[full] = struct{}{}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			recordedCWD, err := claudeSessionCWD(full)
			if err != nil || recordedCWD == "" || !samePath(recordedCWD, canonical) {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".jsonl")
			out = append(out, Candidate{
				Agent: Claude, ID: id, CWD: recordedCWD, Path: full,
				Format: "claude-jsonl", UpdatedAt: info.ModTime(), Size: info.Size(),
			})
		}
	}
	SortCandidates(out)
	return out, nil
}

func claudeSessionCWD(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 32<<10), maxJSONLRecord)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return "", err
		}
		if cwd, _ := record["cwd"].(string); cwd != "" {
			return cwd, nil
		}
	}
	return "", scanner.Err()
}

// claudeQueuedPrompt is a prompt the user typed but Claude had not consumed.
type claudeQueuedPrompt struct {
	text string
	ts   time.Time
}

// claudeStream accumulates one Claude JSONL transcript into canonical events:
// the main thread, or a single delegated run. Records marked isSidechain do not
// belong to the thread they appear in — older Claude versions inlined delegated
// runs into the main log — so unless this stream is itself a sidechain they are
// held aside by agent id and parsed separately.
type claudeStream struct {
	sidechain    bool
	events       []Event
	warnings     []string
	model        string
	title        string
	slug         string
	lastPlanText string
	pending      []claudeQueuedPrompt
	inline       map[string][]json.RawMessage
	inlineOrder  []string
}

func (s *claudeStream) removePending(text string) {
	for i, item := range s.pending {
		if text == "" || item.text == text {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// flushPending appends prompts Claude never consumed, so a transfer does not
// silently drop work the user had already typed.
func (s *claudeStream) flushPending() {
	for _, item := range s.pending {
		s.events = append(s.events, Event{Kind: Message, Role: "user", Timestamp: item.ts, Parts: []Part{{Kind: Text, Text: item.text}}})
		s.warnings = appendUnique(s.warnings, "a queued Claude prompt had not been consumed; it was appended as a pending user message")
	}
	s.pending = nil
}

func (s *claudeStream) record(_ int, raw json.RawMessage) error {
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	if sidechain, _ := record["isSidechain"].(bool); sidechain && !s.sidechain {
		// A delegated run that an older Claude inlined here. It is not part of
		// this thread's conversation, so it is held aside under its agent id.
		id := stringValue(record["agentId"])
		if id == "" {
			id = "inline"
		}
		if s.inline == nil {
			s.inline = make(map[string][]json.RawMessage)
		}
		if _, seen := s.inline[id]; !seen {
			s.inlineOrder = append(s.inlineOrder, id)
		}
		s.inline[id] = append(s.inline[id], raw)
		return nil
	}
	rtype, _ := record["type"].(string)
	if value, _ := record["slug"].(string); value != "" {
		s.slug = value
	}
	for _, key := range []string{"customTitle", "aiTitle", "summary"} {
		if value, _ := record[key].(string); value != "" {
			s.title = value
			break
		}
	}
	timestamp := parseFlexibleTime(record["timestamp"])
	switch rtype {
	case "queue-operation":
		operation, _ := record["operation"].(string)
		content, _ := record["content"].(string)
		switch operation {
		case "enqueue":
			if content != "" {
				s.pending = append(s.pending, claudeQueuedPrompt{text: content, ts: timestamp})
			}
		case "remove", "dequeue":
			s.removePending(content)
		default:
			return fmt.Errorf("unsupported Claude queue operation %q", operation)
		}
		return nil
	case "attachment":
		attachment, ok := record["attachment"].(map[string]any)
		if !ok {
			return fmt.Errorf("attachment record has no attachment object")
		}
		kind, _ := attachment["type"].(string)
		switch kind {
		case "queued_command":
			prompt, _ := attachment["prompt"].(string)
			if prompt != "" {
				s.removePending(prompt)
				s.events = append(s.events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: prompt}}})
			}
		case "plan_file_reference":
			plan, _ := attachment["planContent"].(string)
			if strings.TrimSpace(plan) != "" && strings.TrimSpace(plan) != strings.TrimSpace(s.lastPlanText) {
				s.events = append(s.events, Event{Kind: Plan, Role: "assistant", Timestamp: timestamp, PlanText: plan})
				s.lastPlanText = plan
			}
		case "file":
			content, ok := attachment["content"].(map[string]any)
			if !ok {
				return fmt.Errorf("unsupported Claude file attachment (media cannot be teleported safely)")
			}
			contentType := stringValue(content["type"])
			if contentType == "image" || contentType == "document" {
				media, mediaErr := mediaPartFromValue(content["source"], stringValue(content["media_type"]), stringValue(attachment["filename"]))
				if mediaErr != nil {
					media, mediaErr = mediaPartFromValue(content, stringValue(content["media_type"]), stringValue(attachment["filename"]))
				}
				if mediaErr != nil {
					return fmt.Errorf("Claude media attachment: %w", mediaErr)
				}
				s.events = append(s.events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{media}})
			} else {
				if contentType != "text" {
					return fmt.Errorf("unsupported Claude file attachment (media cannot be teleported safely)")
				}
				file, ok := content["file"].(map[string]any)
				if !ok {
					return fmt.Errorf("Claude text attachment has no file object")
				}
				name := stringValue(attachment["filename"])
				if name == "" {
					name = stringValue(file["filePath"])
				}
				text := fmt.Sprintf("[Claude text attachment: %s]\n%s", name, stringValue(file["content"]))
				s.events = append(s.events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: text}}})
				s.warnings = appendUnique(s.warnings, "a Claude text-file attachment was retained as user-visible text")
			}
		case "edited_text_file":
			name := stringValue(attachment["filename"])
			text := fmt.Sprintf("[Claude edited-file context: %s]\n%s", name, stringValue(attachment["snippet"]))
			s.events = append(s.events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: text}}})
			s.warnings = appendUnique(s.warnings, "Claude edited-file context was retained as user-visible text")
		case "agent_listing_delta", "auto_mode", "budget_usd", "command_permissions", "compact_file_reference", "date_change", "deferred_tools_delta", "goal_status", "hook_non_blocking_error", "hook_system_message", "invoked_skills", "mcp_instructions_delta", "plan_mode", "plan_mode_exit", "plan_mode_reentry", "read_truncation_notice", "skill_listing", "task_reminder", "total_tokens_reminder":
			// Runtime instructions, state deltas and UI reminders are rebuilt by
			// the destination rather than replayed as user conversation.
		default:
			// Claude introduces new attachment kinds over time. An unfamiliar
			// attachment must not strand the whole session, so skip it with a
			// warning like the top-level default below.
			s.warnings = appendUnique(s.warnings, fmt.Sprintf("unsupported Claude attachment %q was skipped", kind))
		}
		return nil
	case "user", "assistant":
		// Parsed below.
	case "agent-name", "ai-title", "atis-latch", "bridge-session", "file-history-delta", "file-history-snapshot", "frame-link", "last-prompt", "mode", "permission-mode", "pr-link", "progress", "relocated", "result", "started", "summary", "system", "worktree-state":
		// Permission, progress, snapshots, summaries and hook bookkeeping do
		// not add user/assistant conversation beyond the records retained above.
		return nil
	default:
		// Claude adds top-level telemetry and UI bookkeeping records over time.
		// Only the known user and assistant records carry transferable
		// conversation; an unfamiliar envelope must not strand the whole session.
		return nil
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s record has no message object", rtype)
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" {
		return fmt.Errorf("unsupported Claude message role %q", role)
	}
	if model, _ := message["model"].(string); model != "" && model != "<synthetic>" && s.model == "" {
		s.model = model
	}
	event := Event{Kind: Message, Role: role, Timestamp: timestamp}
	var eventPlans []string
	event.ID, _ = record["uuid"].(string)
	event.ParentID, _ = record["parentUuid"].(string)
	content := message["content"]
	if text, ok := content.(string); ok {
		if text != "" {
			event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
		}
	} else if blocks, ok := content.([]any); ok {
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("message content contains a non-object block")
			}
			kind, _ := block["type"].(string)
			switch kind {
			case "text":
				if text, _ := block["text"].(string); text != "" {
					if plan, ok := proposedPlan(text); ok && role == "assistant" {
						eventPlans = append(eventPlans, plan)
					} else {
						event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
					}
				}
			case "thinking":
				text, _ := block["thinking"].(string)
				if stringValue(block["signature"]) != "" {
					s.warnings = appendUnique(s.warnings, "Claude thinking signatures are provider-bound; signed reasoning without visible text was not transferable")
				}
				if text != "" {
					event.Parts = append(event.Parts, Part{Kind: Reasoning, Text: text})
				}
			case "image", "document":
				source, ok := block["source"].(map[string]any)
				if !ok {
					return fmt.Errorf("Claude image block has no source")
				}
				sourceType := stringValue(source["type"])
				var media Part
				var mediaErr error
				switch sourceType {
				case "base64":
					media, mediaErr = mediaPart("data:"+stringValue(source["media_type"])+";base64,"+stringValue(source["data"]), stringValue(source["media_type"]), stringValue(block["filename"]))
				case "url":
					media, mediaErr = mediaPart(stringValue(source["url"]), stringValue(source["media_type"]), stringValue(block["filename"]))
				default:
					mediaErr = fmt.Errorf("unsupported Claude image source %q", sourceType)
				}
				if mediaErr != nil {
					return mediaErr
				}
				event.Parts = append(event.Parts, media)
			case "tool_use", "server_tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				input, err := json.Marshal(block["input"])
				if err != nil {
					return fmt.Errorf("tool %s input: %w", id, err)
				}
				if string(input) == "null" {
					input = []byte(`{}`)
				}
				event.Parts = append(event.Parts, Part{Kind: ToolCall, ID: id, CallID: id, ToolName: name, Data: input})
				if name == "ExitPlanMode" || name == "ExitPlanModeV2" || name == "exit-plan-mode-v2" {
					if obj, ok := block["input"].(map[string]any); ok {
						if plan, _ := obj["plan"].(string); strings.TrimSpace(plan) != "" {
							eventPlans = append(eventPlans, plan)
						}
					}
				}
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				isError, _ := block["is_error"].(bool)
				parts, err := claudeResultParts(block["content"], id, isError)
				if err != nil {
					return fmt.Errorf("tool result %s: %w", id, err)
				}
				event.Parts = append(event.Parts, parts...)
			default:
				return fmt.Errorf("unsupported Claude conversation block %q", kind)
			}
		}
	} else if content != nil {
		return fmt.Errorf("message content has unsupported type %T", content)
	}
	if len(event.Parts) > 0 {
		if hasOnlyToolResults(event.Parts) {
			event.Role = "tool"
		}
		s.events = append(s.events, event)
	}
	for _, plan := range eventPlans {
		s.events = append(s.events, Event{Kind: Plan, Role: "assistant", Timestamp: timestamp, PlanText: plan})
		s.lastPlanText = plan
	}
	return nil
}

// parseClaudeRecords reads records already held in memory, which is how the
// sidechain records inlined into a main transcript are parsed.
func parseClaudeRecords(records []json.RawMessage) (*claudeStream, error) {
	stream := &claudeStream{sidechain: true}
	for i, raw := range records {
		if err := stream.record(i+1, raw); err != nil {
			return nil, err
		}
	}
	stream.flushPending()
	return stream, nil
}

// parseClaudeFile reads one Claude transcript file. sidechain marks a file that
// is itself a delegated run, whose records are all sidechain records.
func parseClaudeFile(path string, sidechain bool) (*claudeStream, error) {
	stream := &claudeStream{sidechain: sidechain}
	if err := readJSONL(path, stream.record); err != nil {
		return nil, err
	}
	stream.flushPending()
	return stream, nil
}

func (claudeAdapter) Read(_ context.Context, candidate Candidate) (*Session, error) {
	info, err := os.Stat(candidate.Path)
	if err != nil {
		return nil, err
	}
	history := &Session{
		Source: Claude, SourceID: candidate.ID, CWD: candidate.CWD,
		CreatedAt: info.ModTime(), UpdatedAt: info.ModTime(),
	}
	main, err := parseClaudeFile(candidate.Path, false)
	if err != nil {
		return nil, err
	}
	history.Events = main.events
	history.Model = main.model
	history.Title = main.title
	history.Warnings = append(history.Warnings, main.warnings...)
	var planFileText string
	if main.slug != "" {
		path, err := safeChildPath(filepath.Join(claudeRoot(), "plans"), main.slug+".md")
		if err != nil {
			return nil, fmt.Errorf("unsafe Claude plan slug: %w", err)
		}
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			planFileText = string(b)
		}
	}
	if strings.TrimSpace(planFileText) != "" && strings.TrimSpace(planFileText) != strings.TrimSpace(main.lastPlanText) {
		history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", PlanText: planFileText, Timestamp: history.UpdatedAt})
	}
	branches, warnings, err := readClaudeBranches(candidate.Path, main)
	if err != nil {
		return nil, err
	}
	history.Branches = branches
	history.Warnings = append(history.Warnings, warnings...)
	sortBranches(history.Branches, history.Events)
	if history.Title == "" {
		history.Title = firstText(history)
	}
	return history, nil
}

// claudeSubagentMeta is the sidecar Claude writes beside a delegated run's
// transcript. toolUseId is what links that run back to the Task block in the
// thread that spawned it.
type claudeSubagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
}

// readClaudeBranches collects the delegated runs of one session: the
// per-transcript files Claude writes under <session>/subagents, plus any
// sidechain records an older version inlined into the main log.
func readClaudeBranches(transcript string, main *claudeStream) ([]Branch, []string, error) {
	byID := make(map[string]*Branch)
	var order []string
	add := func(id string, events []Event) *Branch {
		if branch, ok := byID[id]; ok {
			branch.Events = append(branch.Events, events...)
			return branch
		}
		byID[id] = &Branch{ID: id, Events: events}
		order = append(order, id)
		return byID[id]
	}
	var warnings []string
	for _, id := range main.inlineOrder {
		inline, err := parseClaudeRecords(main.inline[id])
		if err != nil {
			return nil, nil, fmt.Errorf("read inline Claude subagent %s: %w", id, err)
		}
		add(id, inline.events)
		warnings = append(warnings, inline.warnings...)
	}
	dir := filepath.Join(strings.TrimSuffix(transcript, filepath.Ext(transcript)), "subagents")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		stream, err := parseClaudeFile(filepath.Join(dir, name), true)
		if err != nil {
			return nil, nil, fmt.Errorf("read Claude subagent %s: %w", id, err)
		}
		if len(stream.events) == 0 {
			warnings = appendUnique(warnings, fmt.Sprintf("Claude subagent transcript %s recorded no messages; it was skipped", id))
			continue
		}
		branch := add(id, stream.events)
		branch.Model = stream.model
		warnings = append(warnings, stream.warnings...)
		metaPath := filepath.Join(dir, strings.TrimSuffix(name, ".jsonl")+".meta.json")
		b, err := os.ReadFile(metaPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		var meta claudeSubagentMeta
		if err := json.Unmarshal(b, &meta); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", metaPath, err)
		}
		branch.CallID = meta.ToolUseID
		branch.Name = meta.AgentType
		branch.Description = meta.Description
	}
	branches := make([]Branch, 0, len(order))
	var unattached []string
	for _, id := range order {
		branch := byID[id]
		if len(branch.Events) == 0 {
			continue
		}
		if branch.CallID == "" {
			unattached = append(unattached, id)
		}
		branches = append(branches, *branch)
	}
	if len(unattached) > 0 {
		warnings = appendUnique(warnings, fmt.Sprintf("Claude recorded no spawning tool call for subagent %s; %s moved as unattached branches", strings.Join(unattached, ", "), plural(len(unattached), "it", "they")))
	}
	// Claude's sidecar records the spawning call but not the agent tree, so a
	// nested run's parent is recovered from which stream owns that call.
	linkBranchParents(branches)
	return branches, warnings, nil
}

func claudeResultParts(content any, callID string, isError bool) ([]Part, error) {
	switch value := content.(type) {
	case nil:
		return []Part{{Kind: ToolResult, CallID: callID, Error: isError}}, nil
	case string:
		return []Part{{Kind: ToolResult, CallID: callID, Text: value, Error: isError}}, nil
	case []any:
		var parts []Part
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("contains a non-object content block")
			}
			kind, _ := block["type"].(string)
			switch kind {
			case "text":
				text, _ := block["text"].(string)
				parts = append(parts, Part{Kind: ToolResult, CallID: callID, Text: text, Error: isError})
			case "image", "document":
				source, ok := block["source"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("image block has no source")
				}
				var media Part
				var err error
				switch stringValue(source["type"]) {
				case "base64":
					media, err = mediaPart("data:"+stringValue(source["media_type"])+";base64,"+stringValue(source["data"]), stringValue(source["media_type"]), stringValue(block["filename"]))
				case "url":
					media, err = mediaPart(stringValue(source["url"]), stringValue(source["media_type"]), stringValue(block["filename"]))
				default:
					err = fmt.Errorf("unsupported image source %q", stringValue(source["type"]))
				}
				if err != nil {
					return nil, err
				}
				media.CallID, media.Error = callID, isError
				parts = append(parts, media)
			default:
				return nil, fmt.Errorf("unsupported content block %q (media cannot be teleported safely)", kind)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("unsupported content type %T", content)
	}
}

func claudeMediaBlock(part Part, source map[string]any) map[string]any {
	kind := "image"
	if !strings.HasPrefix(strings.ToLower(part.MediaType), "image/") {
		kind = "document"
	}
	block := map[string]any{"type": kind, "source": source}
	if part.Filename != "" {
		block["filename"] = part.Filename
	}
	return block
}

func hasOnlyToolResults(parts []Part) bool {
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part.Kind != ToolResult && !(part.Kind == Media && part.CallID != "") {
			return false
		}
	}
	return true
}

// claudeBlocks splits one canonical event into the assistant and user content
// blocks Claude records, and reports the calls that never received a result and
// therefore need a synthesized interrupted one. results holds every call id the
// same stream answered.
func claudeBlocks(event Event, results map[string]bool) (assistant, user []map[string]any, dangling []string, warnings []string) {
	for _, part := range event.Parts {
		switch part.Kind {
		case Text:
			block := map[string]any{"type": "text", "text": part.Text}
			if event.Role == "assistant" {
				assistant = append(assistant, block)
			} else {
				user = append(user, block)
			}
		case Reasoning:
			assistant = append(assistant, map[string]any{"type": "text", "text": part.Text})
			warnings = appendUnique(warnings, "recorded reasoning was stored as assistant text because Claude signatures are provider-specific")
		case ToolCall:
			assistant = append(assistant, map[string]any{"type": "tool_use", "id": part.CallID, "name": part.ToolName, "input": jsonObject(part.Data)})
			if !results[part.CallID] {
				dangling = append(dangling, part.CallID)
			}
		case Media:
			var source map[string]any
			if part.MediaData != "" {
				source = map[string]any{"type": "base64", "media_type": part.MediaType, "data": part.MediaData}
			} else {
				source = map[string]any{"type": "url", "url": part.MediaURL}
			}
			block := claudeMediaBlock(part, source)
			if event.Role == "assistant" {
				assistant = append(assistant, block)
			} else if part.CallID != "" {
				user = append(user, map[string]any{"type": "tool_result", "tool_use_id": part.CallID, "content": []any{block}, "is_error": part.Error})
			} else {
				user = append(user, block)
			}
		case ToolResult:
			user = append(user, map[string]any{"type": "tool_result", "tool_use_id": part.CallID, "content": part.Text, "is_error": part.Error})
		}
	}
	return assistant, user, dangling, warnings
}

// writeClaudeEvents renders one canonical event stream as Claude JSONL records.
// extra carries the per-stream fields that mark a delegated run, so the same
// code produces both the main transcript and a sidechain transcript. Plan
// events are emitted inline; only the main thread also publishes a plan file.
func writeClaudeEvents(w io.Writer, events []Event, model, sessionID, cwd, slug string, extra map[string]any, now time.Time) (planText string, warnings []string, err error) {
	results := make(map[string]bool)
	for _, event := range events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult || (part.Kind == Media && part.CallID != "") {
				results[part.CallID] = true
			}
		}
	}
	var prev string
	writeRecord := func(rtype string, event Event, blocks []map[string]any) error {
		recordID, e := newUUID()
		if e != nil {
			return e
		}
		ts := timestampOr(event.Timestamp, now).UTC().Format(time.RFC3339Nano)
		record := map[string]any{
			"type": rtype, "uuid": recordID, "parentUuid": nullableString(prev),
			"sessionId": sessionID, "timestamp": ts, "isSidechain": false,
			"userType": "external", "cwd": cwd, "slug": slug,
			"version": "agentswap", "entrypoint": "cli", "gitBranch": "HEAD",
		}
		for key, value := range extra {
			record[key] = value
		}
		if prev != "" {
			// Claude stamps the prompt id on the record that opens a sidechain.
			delete(record, "promptId")
		}
		message := map[string]any{"role": rtype, "content": blocks}
		if rtype == "assistant" {
			msgID, e := shortID("msg_")
			if e != nil {
				return e
			}
			message["id"] = msgID
			message["type"] = "message"
			message["model"] = claudeModel(model)
			reqID, e := shortID("req_")
			if e != nil {
				return e
			}
			record["requestId"] = reqID
		}
		record["message"] = message
		if e := writeJSONLine(w, record); e != nil {
			return e
		}
		prev = recordID
		return nil
	}
	for _, event := range events {
		if event.Kind == Plan {
			if strings.TrimSpace(event.PlanText) == "" {
				continue
			}
			if err := writeRecord("assistant", event, []map[string]any{{"type": "text", "text": "<proposed_plan>\n" + event.PlanText + "\n</proposed_plan>"}}); err != nil {
				return "", nil, err
			}
			planText = event.PlanText
			continue
		}
		assistant, user, dangling, blockWarnings := claudeBlocks(event, results)
		for _, warning := range blockWarnings {
			warnings = appendUnique(warnings, warning)
		}
		if len(assistant) > 0 {
			if err := writeRecord("assistant", event, assistant); err != nil {
				return "", nil, err
			}
		}
		if len(user) > 0 {
			if err := writeRecord("user", event, user); err != nil {
				return "", nil, err
			}
		}
		if len(dangling) > 0 {
			blocks := make([]map[string]any, 0, len(dangling))
			for _, callID := range dangling {
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "[Tool execution was interrupted before teleport]", "is_error": true})
			}
			if err := writeRecord("user", event, blocks); err != nil {
				return "", nil, err
			}
		}
	}
	return planText, warnings, nil
}

// writeClaudeBranches gives every delegated run the sidechain transcript and
// meta sidecar Claude writes natively, under the session's own directory. The
// meta's toolUseId is the source call id, which the main transcript preserves
// verbatim, so the run stays linked to the call that spawned it.
func writeClaudeBranches(branches []Branch, dir, sessionID, cwd, slug, model string, now time.Time) (files []string, warnings []string, err error) {
	if len(branches) == 0 {
		return nil, nil, nil
	}
	if err := ensureDir(dir); err != nil {
		return nil, nil, err
	}
	depth := make(map[string]int, len(branches))
	names := make(map[string]string, len(branches))
	for i, branch := range branches {
		suffix, err := shortID("")
		if err != nil {
			return nil, nil, err
		}
		// Claude agent ids are opaque hex. Leading with the branch index keeps
		// the lexical order of the transcript directory equal to the order the
		// source recorded, which is the only signal a re-read has when several
		// runs share one spawning call.
		names[branch.ID] = fmt.Sprintf("agent-a%04x%s", i&0xffff, suffix[:12])
	}
	var unattached []string
	for _, branch := range branches {
		name := names[branch.ID]
		depth[branch.ID] = depth[branch.ParentID] + 1
		path := filepath.Join(dir, name+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		promptID, err := newUUID()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		branchModel := branch.Model
		if branchModel == "" {
			branchModel = model
		}
		extra := map[string]any{"isSidechain": true, "agentId": strings.TrimPrefix(name, "agent-"), "promptId": promptID}
		_, branchWarnings, err := writeClaudeEvents(f, branch.Events, branchModel, sessionID, cwd, slug, extra, now)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("write Claude subagent %s: %w", branch.ID, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		if err := f.Close(); err != nil {
			return nil, nil, err
		}
		for _, warning := range branchWarnings {
			warnings = appendUnique(warnings, warning)
		}
		files = append(files, path)
		if branch.CallID == "" {
			unattached = append(unattached, branch.ID)
		}
		meta := claudeSubagentMeta{
			AgentType: branch.Name, Description: branch.Description,
			ToolUseID: branch.CallID, SpawnDepth: depth[branch.ID],
		}
		if meta.AgentType == "" {
			meta.AgentType = "general-purpose"
		}
		metaPath := filepath.Join(dir, name+".meta.json")
		if err := writeJSONFile(metaPath, meta, 0o600); err != nil {
			return nil, nil, err
		}
		files = append(files, metaPath)
	}
	warnings = append(warnings, fmt.Sprintf("%d delegated agent %s moved as Claude subagent %s; the transcripts are readable but a subagent cannot be resumed in the new session", len(branches), plural(len(branches), "run", "runs"), plural(len(branches), "transcript", "transcripts")))
	if len(unattached) > 0 {
		warnings = append(warnings, fmt.Sprintf("delegated %s %s had no spawning tool call to reference, so Claude will not show %s under a Task block", plural(len(unattached), "run", "runs"), strings.Join(unattached, ", "), plural(len(unattached), "it", "them")))
	}
	return files, warnings, nil
}

func (claudeAdapter) Write(_ context.Context, history *Session, opts WriteOptions) (result Result, err error) {
	id, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	canonical, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	projectDir := filepath.Join(claudeRoot(), "projects", encodeClaudeProject(canonical))
	final := filepath.Join(projectDir, id+".jsonl")
	title := safeTitle(history.Title, firstText(history))
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, title), "-")
	if slug == "" {
		slug = "agentswap"
	}
	slug += "-" + strings.ReplaceAll(id[:8], "-", "")
	result = Result{Agent: Claude, ID: id, Path: final, Resume: []string{"claude", "--resume", id}}
	if opts.DryRun {
		result.Files = []string{final}
		return result, nil
	}
	if err := ensureDir(projectDir); err != nil {
		return Result{}, err
	}
	tmp := final + ".tmp"
	removeIfExists(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	committed := false
	var planFinal, planTmp string
	sessionDir := filepath.Join(projectDir, id)
	defer func() {
		_ = f.Close()
		if !committed {
			removeIfExists(tmp)
			removeIfExists(planTmp)
			removeIfExists(planFinal)
			removeIfExists(sessionDir)
		}
	}()
	if err := writeJSONLine(f, map[string]any{
		"type": "permission-mode", "permissionMode": "default", "sessionId": id,
		"customTitle": title,
		"agentswapSource": map[string]any{
			"agent": history.Source, "sessionId": history.SourceID, "model": history.Model,
			"createdAt": history.CreatedAt, "updatedAt": history.UpdatedAt,
		},
	}); err != nil {
		return Result{}, err
	}
	planText, eventWarnings, err := writeClaudeEvents(f, history.Events, history.Model, id, canonical, slug, nil, time.Now())
	if err != nil {
		return Result{}, err
	}
	result.Warnings = append(result.Warnings, eventWarnings...)
	if strings.TrimSpace(planText) != "" {
		planFinal = filepath.Join(claudeRoot(), "plans", slug+".md")
		planTmp = planFinal + ".tmp"
	}
	branchFiles, branchWarnings, err := writeClaudeBranches(history.Branches, filepath.Join(projectDir, id, "subagents"), id, canonical, slug, history.Model, time.Now())
	if err != nil {
		return Result{}, err
	}
	result.Files = append(result.Files, branchFiles...)
	result.Warnings = append(result.Warnings, branchWarnings...)
	if err := f.Sync(); err != nil {
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		return Result{}, err
	}
	if planFinal != "" {
		if err := ensureDir(filepath.Dir(planFinal)); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(planTmp, []byte(planText), 0o600); err != nil {
			return Result{}, err
		}
		if err := os.Rename(planTmp, planFinal); err != nil {
			return Result{}, err
		}
		result.Files = append(result.Files, planFinal)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Result{}, err
	}
	committed = true
	result.Files = append([]string{final}, result.Files...)
	return result, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func claudeModel(source string) string {
	if strings.HasPrefix(source, "claude-") {
		return source
	}
	return "claude-sonnet-4-6"
}
