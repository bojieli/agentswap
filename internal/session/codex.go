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
)

type codexAdapter struct{}

func newCodexAdapter() Adapter    { return codexAdapter{} }
func (codexAdapter) Agent() Agent { return Codex }

func codexRoot() string { return envDir("CODEX_HOME", filepath.Join(homeDir(), ".codex")) }

// codexConfiguredModelProvider returns the provider Codex uses when a resume
// is launched without an explicit profile. A teleported rollout must carry
// that same provider in its session metadata; otherwise Codex tries to resolve
// a provider that only existed in the source environment (for example the
// agentswap provider) and refuses to bootstrap the session.
func codexConfiguredModelProvider() string {
	path := filepath.Join(codexRoot(), "config.toml")
	f, err := os.Open(path)
	if err != nil {
		return "openai"
	}
	defer f.Close()

	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "model_provider" {
			continue
		}
		value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			var provider string
			if err := json.Unmarshal([]byte(value), &provider); err == nil && provider != "" {
				return provider
			}
		}
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			if provider := value[1 : len(value)-1]; provider != "" {
				return provider
			}
		}
	}
	return "openai"
}

func (codexAdapter) Discover(_ context.Context, cwd string) ([]Candidate, error) {
	canonical, err := canonicalPath(cwd)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(codexRoot(), "sessions")
	var out []Candidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		meta, err := readCodexMeta(path)
		if err != nil || !samePath(meta.CWD, canonical) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		out = append(out, Candidate{
			Agent: Codex, ID: meta.ID, CWD: meta.CWD, Path: path,
			Format: "codex-rollout", UpdatedAt: info.ModTime(), Size: info.Size(),
		})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	SortCandidates(out)
	return out, nil
}

type codexMeta struct {
	ID        string
	CWD       string
	Timestamp time.Time
}

func readCodexMeta(path string) (codexMeta, error) {
	var result codexMeta
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	var record struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := decoder.Decode(&record); err != nil {
		return result, err
	}
	if record.Type != "session_meta" {
		return result, fmt.Errorf("first record is %q, want session_meta", record.Type)
	}
	result.ID, _ = record.Payload["id"].(string)
	if result.ID == "" {
		result.ID, _ = record.Payload["session_id"].(string)
	}
	result.CWD, _ = record.Payload["cwd"].(string)
	result.Timestamp = parseFlexibleTime(record.Payload["timestamp"])
	if result.ID == "" || result.CWD == "" {
		return result, fmt.Errorf("session_meta is missing id or cwd")
	}
	return result, nil
}

func (codexAdapter) Read(_ context.Context, candidate Candidate) (*Session, error) {
	meta, err := readCodexMeta(candidate.Path)
	if err != nil {
		return nil, err
	}
	if meta.ID != candidate.ID {
		return nil, fmt.Errorf("session id changed from %q to %q", candidate.ID, meta.ID)
	}
	if !samePath(meta.CWD, candidate.CWD) {
		return nil, fmt.Errorf("session cwd %q does not match %q", meta.CWD, candidate.CWD)
	}
	info, err := os.Stat(candidate.Path)
	if err != nil {
		return nil, err
	}
	history := &Session{
		Source: Codex, SourceID: meta.ID, CWD: meta.CWD,
		CreatedAt: timestampOr(meta.Timestamp, info.ModTime()), UpdatedAt: info.ModTime(),
	}
	err = readJSONL(candidate.Path, func(_ int, raw json.RawMessage) error {
		var record struct {
			Timestamp any            `json:"timestamp"`
			Type      string         `json:"type"`
			Payload   map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		ts := parseFlexibleTime(record.Timestamp)
		switch record.Type {
		case "session_meta":
			if model, _ := record.Payload["model"].(string); model != "" {
				history.Model = model
			}
		case "turn_context":
			if model, _ := record.Payload["model"].(string); model != "" {
				history.Model = model
			}
		case "response_item":
			return readCodexResponse(history, record.Payload, ts)
		case "event_msg":
			if kind, _ := record.Payload["type"].(string); kind == "plan_update" {
				if text := codexPlanText(record.Payload["plan"]); text != "" {
					history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: ts, PlanText: text})
				}
			}
		case "compacted":
			history.Warnings = appendUnique(history.Warnings, "source contains a Codex compaction record; all original response items still present in the rollout were retained")
		case "world_state":
			// Runtime environment state is recreated by the destination harness.
		default:
			return fmt.Errorf("unsupported Codex record type %q", record.Type)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if history.Title == "" {
		history.Title = firstText(history)
	}
	return history, nil
}

func readCodexResponse(history *Session, payload map[string]any, ts time.Time) error {
	kind, _ := payload["type"].(string)
	switch kind {
	case "message":
		role, _ := payload["role"].(string)
		if role == "system" || role == "developer" {
			return nil
		}
		if role != "user" && role != "assistant" {
			return fmt.Errorf("unsupported Codex message role %q", role)
		}
		event := Event{Kind: Message, Role: role, Timestamp: ts}
		event.ID, _ = payload["id"].(string)
		content, ok := payload["content"].([]any)
		if !ok {
			return fmt.Errorf("Codex message content is not an array")
		}
		var plans []string
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("Codex content contains a non-object block")
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "input_text", "output_text", "text":
				text, _ := block["text"].(string)
				if plan, ok := proposedPlan(text); ok && role == "assistant" {
					plans = append(plans, plan)
				} else if text != "" {
					event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
				}
			case "input_image", "output_image", "image_url", "computer_screenshot", "input_file", "output_file", "file_url":
				media, err := mediaPartFromValue(block["image_url"], stringValue(block["media_type"]), stringValue(block["filename"]))
				if err != nil {
					media, err = mediaPartFromValue(block["file_url"], stringValue(block["media_type"]), stringValue(block["filename"]))
				}
				if err != nil && block["url"] != nil {
					media, err = mediaPartFromValue(block["url"], stringValue(block["media_type"]), stringValue(block["filename"]))
				}
				if err != nil {
					return fmt.Errorf("Codex image content: %w", err)
				}
				event.Parts = append(event.Parts, media)
			default:
				return fmt.Errorf("unsupported Codex message block %q", blockType)
			}
		}
		if len(event.Parts) > 0 {
			history.Events = append(history.Events, event)
		}
		for _, plan := range plans {
			history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: ts, PlanText: plan})
		}
	case "function_call", "custom_tool_call":
		callID, _ := payload["call_id"].(string)
		if callID == "" {
			callID, _ = payload["id"].(string)
		}
		name, _ := payload["name"].(string)
		input := payload["arguments"]
		if input == nil {
			input = payload["input"]
		}
		data, err := codexInputJSON(input)
		if err != nil {
			return fmt.Errorf("tool call %s input: %w", callID, err)
		}
		history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: ToolCall, ID: stringValue(payload["id"]), CallID: callID, ToolName: name, Data: data}}})
	case "function_call_output", "custom_tool_call_output":
		callID, _ := payload["call_id"].(string)
		parts, err := codexOutputParts(payload["output"], callID)
		if err != nil {
			return err
		}
		isError, _ := payload["agentswap_error"].(bool)
		for i := range parts {
			parts[i].Error = isError
		}
		history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Timestamp: ts, Parts: parts})
	case "reasoning":
		var texts []string
		if summary, ok := payload["summary"].([]any); ok {
			for _, item := range summary {
				if obj, ok := item.(map[string]any); ok {
					if text, _ := obj["text"].(string); text != "" {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: Reasoning, Text: strings.Join(texts, "\n")}}})
		}
		if payload["encrypted_content"] != nil {
			history.Warnings = appendUnique(history.Warnings, "Codex encrypted reasoning is provider-bound and was not transferred")
		}
	case "compacted":
		history.Warnings = appendUnique(history.Warnings, "source contains a Codex compaction marker; all original records still present in the rollout were retained")
	case "web_search_call", "computer_call":
		return fmt.Errorf("unsupported Codex conversation item %q", kind)
	case "":
		return fmt.Errorf("response_item has no payload type")
	default:
		return fmt.Errorf("unsupported Codex response item %q", kind)
	}
	return nil
}

func codexOutputParts(value any, callID string) ([]Part, error) {
	if value == nil {
		return []Part{{Kind: ToolResult, CallID: callID}}, nil
	}
	if text, ok := value.(string); ok {
		return []Part{{Kind: ToolResult, CallID: callID, Text: text}}, nil
	}
	items, ok := value.([]any)
	if !ok {
		text, err := stringifyOutput(value)
		if err != nil {
			return nil, err
		}
		return []Part{{Kind: ToolResult, CallID: callID, Text: text}}, nil
	}
	var parts []Part
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Codex tool output contains a non-object block")
		}
		kind := stringValue(obj["type"])
		switch kind {
		case "text", "output_text", "input_text":
			parts = append(parts, Part{Kind: ToolResult, CallID: callID, Text: stringValue(obj["text"])})
		case "input_image", "output_image", "image_url", "computer_screenshot", "image", "input_file", "output_file", "file_url", "file":
			media, err := mediaPartFromValue(obj["image_url"], stringValue(obj["media_type"]), stringValue(obj["filename"]))
			if err != nil {
				media, err = mediaPartFromValue(obj, stringValue(obj["media_type"]), stringValue(obj["filename"]))
			}
			if err != nil {
				return nil, err
			}
			media.CallID = callID
			parts = append(parts, media)
		default:
			return nil, fmt.Errorf("unsupported Codex tool output block %q", kind)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, Part{Kind: ToolResult, CallID: callID})
	}
	return parts, nil
}

func codexInputJSON(value any) (json.RawMessage, error) {
	if text, ok := value.(string); ok {
		if json.Valid([]byte(text)) {
			return jsonObject([]byte(text)), nil
		}
		b, err := json.Marshal(map[string]any{"input": text})
		return b, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(b) == "null" {
		b = []byte(`{}`)
	}
	return jsonObject(b), nil
}

func stringifyOutput(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("tool output: %w", err)
	}
	return string(b), nil
}

func stringValue(value any) string { text, _ := value.(string); return text }

func codexPlanText(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	var lines []string
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		step, _ := obj["step"].(string)
		status, _ := obj["status"].(string)
		if step != "" {
			lines = append(lines, fmt.Sprintf("- [%s] %s", status, step))
		}
	}
	return strings.Join(lines, "\n")
}

func (codexAdapter) Write(_ context.Context, history *Session, opts WriteOptions) (result Result, err error) {
	now := time.Now().UTC()
	id, err := newUUID7(now)
	if err != nil {
		return Result{}, err
	}
	canonical, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Join(codexRoot(), "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	name := "rollout-" + now.Format("2006-01-02T15-04-05") + "-" + id + ".jsonl"
	final := filepath.Join(dir, name)
	// The agentswap provider is configured in Codex's own config by
	// `agentswap install`. Keep the resume command native: callers should not
	// need an agentswap-specific profile flag just to open a teleported session.
	result = Result{Agent: Codex, ID: id, Path: final, Resume: []string{"codex", "resume", id}, Files: []string{final}}
	if opts.DryRun {
		return result, nil
	}
	if err := ensureDir(dir); err != nil {
		return Result{}, err
	}
	tmp := final + ".tmp"
	removeIfExists(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			removeIfExists(tmp)
		}
	}()
	write := func(recordType string, ts time.Time, payload any) error {
		return writeJSONLine(f, map[string]any{
			"timestamp": timestampOr(ts, now).UTC().Format(time.RFC3339Nano),
			"type":      recordType, "payload": payload,
		})
	}
	meta := map[string]any{
		"id": id, "session_id": id, "timestamp": now.Format(time.RFC3339Nano),
		"cwd": canonical, "originator": "agentswap", "cli_version": "agentswap",
		"source": "cli", "thread_source": "user", "model_provider": codexConfiguredModelProvider(),
		"model": history.Model,
		"agentswap_source": map[string]any{
			"agent": history.Source, "session_id": history.SourceID,
			"created_at": history.CreatedAt, "updated_at": history.UpdatedAt,
		},
	}
	if err := write("session_meta", now, meta); err != nil {
		return Result{}, err
	}
	results := make(map[string]bool)
	for _, event := range history.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult || (part.Kind == Media && part.CallID != "") {
				results[part.CallID] = true
			}
		}
	}
	for _, event := range history.Events {
		if event.Kind == Plan {
			text := "<proposed_plan>\n" + event.PlanText + "\n</proposed_plan>"
			msgID, err := shortID("msg_")
			if err != nil {
				return Result{}, err
			}
			payload := map[string]any{"type": "message", "id": msgID, "role": "assistant", "phase": "planning", "content": []any{map[string]any{"type": "output_text", "text": text}}}
			if err := write("response_item", event.Timestamp, payload); err != nil {
				return Result{}, err
			}
			if err := write("event_msg", event.Timestamp, map[string]any{"type": "agent_message", "message": text, "phase": "planning"}); err != nil {
				return Result{}, err
			}
			continue
		}
		role := event.Role
		toolEvent := role == "tool"
		if role == "tool" {
			role = "user"
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		var content []any
		var displayText []string
		flushMessage := func() error {
			if len(content) == 0 {
				return nil
			}
			msgID, err := shortID("msg_")
			if err != nil {
				return err
			}
			payload := map[string]any{"type": "message", "id": msgID, "role": role, "content": content}
			if err := write("response_item", event.Timestamp, payload); err != nil {
				return err
			}
			eventType := "user_message"
			if role == "assistant" {
				eventType = "agent_message"
			}
			if err := write("event_msg", event.Timestamp, map[string]any{"type": eventType, "message": strings.Join(displayText, "\n")}); err != nil {
				return err
			}
			content = nil
			displayText = nil
			return nil
		}
		for _, part := range event.Parts {
			switch part.Kind {
			case Text, Reasoning:
				content = append(content, map[string]any{"type": contentType, "text": part.Text})
				displayText = append(displayText, part.Text)
				if part.Kind == Reasoning {
					result.Warnings = appendUnique(result.Warnings, "recorded reasoning was stored as assistant text because reasoning signatures are provider-specific")
				}
			case Media:
				mediaType := "input_image"
				if !strings.HasPrefix(strings.ToLower(part.MediaType), "image/") {
					mediaType = "input_file"
				}
				if role == "assistant" {
					if mediaType == "input_image" {
						mediaType = "output_image"
					} else {
						mediaType = "output_file"
					}
				}
				if toolEvent && part.CallID != "" {
					if err := flushMessage(); err != nil {
						return Result{}, err
					}
					outID, err := shortID("fco_")
					if err != nil {
						return Result{}, err
					}
					block := map[string]any{"type": mediaType, "media_type": part.MediaType, "filename": part.Filename}
					if strings.HasPrefix(strings.ToLower(part.MediaType), "image/") {
						block["image_url"] = mediaDataURL(part)
					} else {
						block["file_url"] = mediaDataURL(part)
					}
					output := []any{block}
					if err := write("response_item", event.Timestamp, map[string]any{"type": "function_call_output", "id": outID, "call_id": part.CallID, "output": output, "agentswap_error": part.Error}); err != nil {
						return Result{}, err
					}
					continue
				}
				block := map[string]any{"type": mediaType}
				if strings.HasPrefix(strings.ToLower(part.MediaType), "image/") {
					block["image_url"] = mediaDataURL(part)
				} else {
					block["file_url"] = mediaDataURL(part)
				}
				if part.MediaType != "" {
					block["media_type"] = part.MediaType
				}
				if part.Filename != "" {
					block["filename"] = part.Filename
				}
				content = append(content, block)
			case ToolCall:
				if err := flushMessage(); err != nil {
					return Result{}, err
				}
				itemID, err := shortID("fc_")
				if err != nil {
					return Result{}, err
				}
				payload := map[string]any{"type": "function_call", "id": itemID, "name": part.ToolName, "arguments": string(jsonObject(part.Data)), "call_id": part.CallID}
				if err := write("response_item", event.Timestamp, payload); err != nil {
					return Result{}, err
				}
				if !results[part.CallID] {
					outID, err := shortID("fco_")
					if err != nil {
						return Result{}, err
					}
					if err := write("response_item", event.Timestamp, map[string]any{"type": "function_call_output", "id": outID, "call_id": part.CallID, "output": "[Tool execution was interrupted before teleport]", "agentswap_error": true}); err != nil {
						return Result{}, err
					}
				}
			case ToolResult:
				if err := flushMessage(); err != nil {
					return Result{}, err
				}
				outID, err := shortID("fco_")
				if err != nil {
					return Result{}, err
				}
				output := any(part.Text)
				if part.Kind == Media {
					output = []any{map[string]any{"type": "output_image", "image_url": mediaDataURL(part), "media_type": part.MediaType, "filename": part.Filename}}
				}
				if err := write("response_item", event.Timestamp, map[string]any{"type": "function_call_output", "id": outID, "call_id": part.CallID, "output": output, "agentswap_error": part.Error}); err != nil {
					return Result{}, err
				}
			}
		}
		if err := flushMessage(); err != nil {
			return Result{}, err
		}
	}
	if err := f.Sync(); err != nil {
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return Result{}, err
	}
	committed = true
	return result, nil
}
