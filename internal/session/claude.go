package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

func (claudeAdapter) Read(_ context.Context, candidate Candidate) (*Session, error) {
	info, err := os.Stat(candidate.Path)
	if err != nil {
		return nil, err
	}
	history := &Session{
		Source: Claude, SourceID: candidate.ID, CWD: candidate.CWD,
		CreatedAt: info.ModTime(), UpdatedAt: info.ModTime(),
	}
	subagentsDir := strings.TrimSuffix(candidate.Path, filepath.Ext(candidate.Path))
	subagentsDir = filepath.Join(subagentsDir, "subagents")
	if entries, readErr := os.ReadDir(subagentsDir); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				return nil, fmt.Errorf("Claude session contains subagent transcript %q; branched agent histories are not safely transferable yet", entry.Name())
			}
		}
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	}
	var slug, planFileText, lastPlanText string
	type queuedPrompt struct {
		text string
		ts   time.Time
	}
	var pending []queuedPrompt
	removePending := func(text string) {
		for i, item := range pending {
			if text == "" || item.text == text {
				pending = append(pending[:i], pending[i+1:]...)
				return
			}
		}
	}
	err = readJSONL(candidate.Path, func(_ int, raw json.RawMessage) error {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		rtype, _ := record["type"].(string)
		if value, _ := record["cwd"].(string); value != "" {
			if !samePath(value, candidate.CWD) {
				return fmt.Errorf("record cwd %q does not match %q", value, candidate.CWD)
			}
			history.CWD = value
		}
		if value, _ := record["slug"].(string); value != "" {
			slug = value
		}
		for _, key := range []string{"customTitle", "aiTitle", "summary"} {
			if value, _ := record[key].(string); value != "" {
				history.Title = value
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
					pending = append(pending, queuedPrompt{text: content, ts: timestamp})
				}
			case "remove", "dequeue":
				removePending(content)
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
					removePending(prompt)
					history.Events = append(history.Events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: prompt}}})
				}
			case "plan_file_reference":
				plan, _ := attachment["planContent"].(string)
				if strings.TrimSpace(plan) != "" && strings.TrimSpace(plan) != strings.TrimSpace(lastPlanText) {
					history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: timestamp, PlanText: plan})
					lastPlanText = plan
				}
			case "file":
				content, ok := attachment["content"].(map[string]any)
				if !ok || stringValue(content["type"]) != "text" {
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
				history.Events = append(history.Events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: text}}})
				history.Warnings = appendUnique(history.Warnings, "a Claude text-file attachment was retained as user-visible text")
			case "edited_text_file":
				name := stringValue(attachment["filename"])
				text := fmt.Sprintf("[Claude edited-file context: %s]\n%s", name, stringValue(attachment["snippet"]))
				history.Events = append(history.Events, Event{Kind: Message, ID: stringValue(record["uuid"]), ParentID: stringValue(record["parentUuid"]), Role: "user", Timestamp: timestamp, Parts: []Part{{Kind: Text, Text: text}}})
				history.Warnings = appendUnique(history.Warnings, "Claude edited-file context was retained as user-visible text")
			case "agent_listing_delta", "auto_mode", "budget_usd", "command_permissions", "compact_file_reference", "date_change", "deferred_tools_delta", "goal_status", "hook_non_blocking_error", "hook_system_message", "invoked_skills", "mcp_instructions_delta", "plan_mode", "plan_mode_exit", "plan_mode_reentry", "read_truncation_notice", "skill_listing", "task_reminder", "total_tokens_reminder":
				// Runtime instructions, state deltas and UI reminders are rebuilt by
				// the destination rather than replayed as user conversation.
			default:
				return fmt.Errorf("unsupported Claude attachment %q", kind)
			}
			return nil
		case "user", "assistant":
			// Parsed below.
		case "agent-name", "ai-title", "atis-latch", "bridge-session", "file-history-delta", "file-history-snapshot", "frame-link", "last-prompt", "mode", "permission-mode", "pr-link", "progress", "relocated", "result", "started", "summary", "system", "worktree-state":
			// Permission, progress, snapshots, summaries and hook bookkeeping do
			// not add user/assistant conversation beyond the records retained above.
			return nil
		default:
			return fmt.Errorf("unsupported Claude record type %q", rtype)
		}
		message, ok := record["message"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s record has no message object", rtype)
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			return fmt.Errorf("unsupported Claude message role %q", role)
		}
		if model, _ := message["model"].(string); model != "" && model != "<synthetic>" && history.Model == "" {
			history.Model = model
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
						history.Warnings = appendUnique(history.Warnings, "Claude thinking signatures are provider-bound; signed reasoning without visible text was not transferable")
					}
					if text != "" {
						event.Parts = append(event.Parts, Part{Kind: Reasoning, Text: text})
					}
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
					text, err := claudeResultText(block["content"])
					if err != nil {
						return fmt.Errorf("tool result %s: %w", id, err)
					}
					isError, _ := block["is_error"].(bool)
					event.Parts = append(event.Parts, Part{Kind: ToolResult, CallID: id, Text: text, Error: isError})
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
			history.Events = append(history.Events, event)
		}
		for _, plan := range eventPlans {
			history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: timestamp, PlanText: plan})
			lastPlanText = plan
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, item := range pending {
		history.Events = append(history.Events, Event{Kind: Message, Role: "user", Timestamp: item.ts, Parts: []Part{{Kind: Text, Text: item.text}}})
		history.Warnings = appendUnique(history.Warnings, "a queued Claude prompt had not been consumed; it was appended as a pending user message")
	}
	if slug != "" {
		path, err := safeChildPath(filepath.Join(claudeRoot(), "plans"), slug+".md")
		if err != nil {
			return nil, fmt.Errorf("unsafe Claude plan slug: %w", err)
		}
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			planFileText = string(b)
		}
	}
	if strings.TrimSpace(planFileText) != "" && strings.TrimSpace(planFileText) != strings.TrimSpace(lastPlanText) {
		history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", PlanText: planFileText, Timestamp: history.UpdatedAt})
	}
	if history.Title == "" {
		history.Title = firstText(history)
	}
	return history, nil
}

func claudeResultText(content any) (string, error) {
	switch value := content.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case []any:
		var parts []string
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("contains a non-object content block")
			}
			kind, _ := block["type"].(string)
			if kind != "text" {
				return "", fmt.Errorf("unsupported content block %q (media cannot be teleported safely)", kind)
			}
			text, _ := block["text"].(string)
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("unsupported content type %T", content)
	}
}

func hasOnlyToolResults(parts []Part) bool {
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part.Kind != ToolResult {
			return false
		}
	}
	return true
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
	defer func() {
		_ = f.Close()
		if !committed {
			removeIfExists(tmp)
			removeIfExists(planTmp)
			removeIfExists(planFinal)
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
	results := make(map[string]bool)
	for _, event := range history.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult {
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
		ts := timestampOr(event.Timestamp, time.Now()).UTC().Format(time.RFC3339Nano)
		record := map[string]any{
			"type": rtype, "uuid": recordID, "parentUuid": nullableString(prev),
			"sessionId": id, "timestamp": ts, "isSidechain": false,
			"userType": "external", "cwd": canonical, "slug": slug,
			"version": "agentswap", "entrypoint": "cli", "gitBranch": "HEAD",
		}
		role := rtype
		if rtype == "user" {
			role = "user"
		}
		message := map[string]any{"role": role, "content": blocks}
		if rtype == "assistant" {
			msgID, e := shortID("msg_")
			if e != nil {
				return e
			}
			message["id"] = msgID
			message["type"] = "message"
			message["model"] = claudeModel(history.Model)
			reqID, e := shortID("req_")
			if e != nil {
				return e
			}
			record["requestId"] = reqID
		}
		record["message"] = message
		if e := writeJSONLine(f, record); e != nil {
			return e
		}
		prev = recordID
		return nil
	}
	for _, event := range history.Events {
		if event.Kind == Plan {
			if strings.TrimSpace(event.PlanText) == "" {
				continue
			}
			if err := writeRecord("assistant", event, []map[string]any{{"type": "text", "text": "<proposed_plan>\n" + event.PlanText + "\n</proposed_plan>"}}); err != nil {
				return Result{}, err
			}
			planFinal = filepath.Join(claudeRoot(), "plans", slug+".md")
			planTmp = planFinal + ".tmp"
			continue
		}
		var assistant, user []map[string]any
		var dangling []string
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
				result.Warnings = appendUnique(result.Warnings, "recorded reasoning was stored as assistant text because Claude signatures are provider-specific")
			case ToolCall:
				assistant = append(assistant, map[string]any{"type": "tool_use", "id": part.CallID, "name": part.ToolName, "input": jsonObject(part.Data)})
				if !results[part.CallID] {
					dangling = append(dangling, part.CallID)
				}
			case ToolResult:
				user = append(user, map[string]any{"type": "tool_result", "tool_use_id": part.CallID, "content": part.Text, "is_error": part.Error})
			}
		}
		if len(assistant) > 0 {
			if err := writeRecord("assistant", event, assistant); err != nil {
				return Result{}, err
			}
		}
		if len(user) > 0 {
			if err := writeRecord("user", event, user); err != nil {
				return Result{}, err
			}
		}
		if len(dangling) > 0 {
			blocks := make([]map[string]any, 0, len(dangling))
			for _, callID := range dangling {
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "[Tool execution was interrupted before teleport]", "is_error": true})
			}
			if err := writeRecord("user", event, blocks); err != nil {
				return Result{}, err
			}
		}
	}
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
		var latest string
		for i := len(history.Events) - 1; i >= 0; i-- {
			if history.Events[i].Kind == Plan {
				latest = history.Events[i].PlanText
				break
			}
		}
		if err := os.WriteFile(planTmp, []byte(latest), 0o600); err != nil {
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
