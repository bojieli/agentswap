package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type openCodeAdapter struct{}

func newOpenCodeAdapter() Adapter    { return openCodeAdapter{} }
func (openCodeAdapter) Agent() Agent { return OpenCode }

func openCodeBinary() string {
	if value := os.Getenv("AGENTSWAP_OPENCODE_BIN"); value != "" {
		return value
	}
	return "opencode"
}

func runOpenCode(ctx context.Context, cwd string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, openCodeBinary(), args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 4096 {
			detail = detail[:4096] + "…"
		}
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func (openCodeAdapter) Discover(ctx context.Context, cwd string) ([]Candidate, error) {
	canonical, err := canonicalPath(cwd)
	if err != nil {
		return nil, err
	}
	stdout, _, err := runOpenCode(ctx, canonical, "session", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("run `opencode session list --format json`: %w", err)
	}
	var rows []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Updated   any    `json:"updated"`
		Created   any    `json:"created"`
	}
	if err := json.Unmarshal(stdout, &rows); err != nil {
		return nil, fmt.Errorf("parse OpenCode session list: %w", err)
	}
	var out []Candidate
	for _, row := range rows {
		if row.ID == "" || !samePath(row.Directory, canonical) {
			continue
		}
		updated := parseFlexibleTime(row.Updated)
		if updated.IsZero() {
			updated = parseFlexibleTime(row.Created)
		}
		out = append(out, Candidate{Agent: OpenCode, ID: row.ID, CWD: row.Directory, Format: "opencode-export", Title: row.Title, UpdatedAt: updated})
	}
	SortCandidates(out)
	return out, nil
}

func (openCodeAdapter) Read(ctx context.Context, candidate Candidate) (*Session, error) {
	stdout, _, err := runOpenCode(ctx, candidate.CWD, "export", candidate.ID)
	if err != nil {
		return nil, fmt.Errorf("run `opencode export %s`: %w", candidate.ID, err)
	}
	var exported struct {
		Info     map[string]any `json:"info"`
		Messages []struct {
			Info  map[string]any   `json:"info"`
			Parts []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(stdout, &exported); err != nil {
		return nil, fmt.Errorf("parse OpenCode export: %w", err)
	}
	id, _ := exported.Info["id"].(string)
	if id != candidate.ID {
		return nil, fmt.Errorf("export returned session %q, want %q", id, candidate.ID)
	}
	directory, _ := exported.Info["directory"].(string)
	if !samePath(directory, candidate.CWD) {
		return nil, fmt.Errorf("exported session cwd %q does not match %q", directory, candidate.CWD)
	}
	history := &Session{Source: OpenCode, SourceID: id, CWD: directory, Title: stringValue(exported.Info["title"])}
	if model, ok := exported.Info["model"].(map[string]any); ok {
		history.Model = stringValue(model["id"])
	}
	if history.Model == "" {
		if metadata, ok := exported.Info["metadata"].(map[string]any); ok {
			if source, ok := metadata["agentswap"].(map[string]any); ok {
				history.Model = stringValue(source["sourceModel"])
			}
		}
	}
	if tm, ok := exported.Info["time"].(map[string]any); ok {
		history.CreatedAt = parseFlexibleTime(tm["created"])
		history.UpdatedAt = parseFlexibleTime(tm["updated"])
	}
	for _, message := range exported.Messages {
		role := stringValue(message.Info["role"])
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("unsupported OpenCode message role %q", role)
		}
		ts := time.Time{}
		if tm, ok := message.Info["time"].(map[string]any); ok {
			ts = parseFlexibleTime(tm["created"])
		}
		if history.Model == "" {
			if model, ok := message.Info["model"].(map[string]any); ok {
				history.Model = stringValue(model["modelID"])
				if history.Model == "" {
					history.Model = stringValue(model["id"])
				}
			}
			if history.Model == "" {
				history.Model = stringValue(message.Info["modelID"])
			}
		}
		event := Event{Kind: Message, ID: stringValue(message.Info["id"]), ParentID: stringValue(message.Info["parentID"]), Role: role, Timestamp: ts}
		flush := func() {
			if len(event.Parts) > 0 {
				history.Events = append(history.Events, event)
				event = Event{Kind: Message, Role: role, Timestamp: ts}
			}
		}
		for _, part := range message.Parts {
			kind := stringValue(part["type"])
			switch kind {
			case "text":
				text := stringValue(part["text"])
				if plan, ok := proposedPlan(text); ok && role == "assistant" {
					flush()
					history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: ts, PlanText: plan})
				} else if text != "" {
					event.Parts = append(event.Parts, Part{Kind: Text, ID: stringValue(part["id"]), Text: text})
				}
			case "reasoning":
				if text := stringValue(part["text"]); text != "" {
					event.Parts = append(event.Parts, Part{Kind: Reasoning, ID: stringValue(part["id"]), Text: text})
				}
			case "tool":
				flush()
				callID := stringValue(part["callID"])
				name := stringValue(part["tool"])
				state, ok := part["state"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("OpenCode tool %s has no state", callID)
				}
				input, err := json.Marshal(state["input"])
				if err != nil {
					return nil, err
				}
				if string(input) == "null" {
					input = []byte(`{}`)
				}
				history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: ToolCall, ID: stringValue(part["id"]), CallID: callID, ToolName: name, Data: jsonObject(input)}}})
				status := stringValue(state["status"])
				var output any
				var isError bool
				switch status {
				case "completed":
					output = state["output"]
				case "error":
					output = state["error"]
					isError = true
				case "pending", "running":
					history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("OpenCode tool call %s was %s when recorded", callID, status))
					continue
				default:
					return nil, fmt.Errorf("OpenCode tool %s has unsupported state %q", callID, status)
				}
				parts, err := openCodeOutputParts(output, callID)
				if err != nil {
					return nil, err
				}
				for i := range parts {
					parts[i].Error = isError
				}
				history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Timestamp: ts, Parts: parts})
			case "step-start", "step-finish", "snapshot", "patch", "retry", "compaction":
				// Execution/UI bookkeeping. Conversation-bearing text, reasoning and
				// tools are represented by their own parts and retained above.
			case "file":
				media, err := mediaPartFromValue(part["url"], stringValue(part["mime"]), stringValue(part["filename"]))
				if err != nil {
					media, err = mediaPartFromValue(part["source"], stringValue(part["mime"]), stringValue(part["filename"]))
				}
				if err != nil {
					return nil, fmt.Errorf("unsupported OpenCode file media: %w", err)
				}
				event.Parts = append(event.Parts, media)
			case "agent", "subtask":
				return nil, fmt.Errorf("unsupported OpenCode %s part (attachments and agent delegation cannot be teleported safely)", kind)
			default:
				return nil, fmt.Errorf("unsupported OpenCode conversation part %q", kind)
			}
		}
		flush()
	}
	if history.Title == "" {
		history.Title = firstText(history)
	}
	return history, nil
}

func openCodeOutputParts(output any, callID string) ([]Part, error) {
	if text, ok := output.(string); ok {
		return []Part{{Kind: ToolResult, CallID: callID, Text: text}}, nil
	}
	if obj, ok := output.(map[string]any); ok {
		if kind := stringValue(obj["type"]); kind == "image" || kind == "input_image" {
			media, err := mediaPartFromValue(obj["url"], stringValue(obj["mime"]), stringValue(obj["filename"]))
			if err != nil {
				media, err = mediaPartFromValue(obj["image_url"], stringValue(obj["mime"]), stringValue(obj["filename"]))
			}
			if err != nil {
				return nil, err
			}
			media.CallID = callID
			return []Part{media}, nil
		}
	}
	if output == nil {
		return []Part{{Kind: ToolResult, CallID: callID}}, nil
	}
	text, err := stringifyOutput(output)
	if err != nil {
		return nil, err
	}
	return []Part{{Kind: ToolResult, CallID: callID, Text: text}}, nil
}

func (openCodeAdapter) Write(ctx context.Context, history *Session, opts WriteOptions) (result Result, err error) {
	idPart, err := shortID("")
	if err != nil {
		return Result{}, err
	}
	id := "ses_" + idPart
	canonical, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	result = Result{Agent: OpenCode, ID: id, Path: "OpenCode local database", Resume: []string{openCodeBinary(), "--session", id}, ExternalCLI: true}
	if opts.DryRun {
		return result, nil
	}
	if _, err := exec.LookPath(openCodeBinary()); err != nil {
		return Result{}, fmt.Errorf("OpenCode executable %q not found: %w", openCodeBinary(), err)
	}
	now := time.Now()
	created := now.UnixMilli()
	updated := created
	export := map[string]any{
		"info": map[string]any{
			"id": id, "slug": id, "projectID": "global", "directory": canonical,
			"title": safeTitle(history.Title, firstText(history)), "version": "1",
			"time": map[string]any{"created": created, "updated": updated},
			"metadata": map[string]any{"agentswap": map[string]any{
				"sourceAgent": history.Source, "sourceSessionID": history.SourceID, "sourceModel": history.Model,
				"sourceCreatedAt": history.CreatedAt, "sourceUpdatedAt": history.UpdatedAt,
			}},
		},
	}
	type recordedToolResult struct {
		part Part
		time time.Time
	}
	toolResults := make(map[string]recordedToolResult)
	for _, event := range history.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult || (part.Kind == Media && part.CallID != "") {
				toolResults[part.CallID] = recordedToolResult{part: part, time: event.Timestamp}
			}
		}
	}
	provider, model := openCodeModel(history)
	var messages []any
	var lastUser string
	for _, event := range history.Events {
		if event.Kind == Plan {
			event = Event{Kind: Message, Role: "assistant", Timestamp: event.Timestamp, Parts: []Part{{Kind: Text, Text: "<proposed_plan>\n" + event.PlanText + "\n</proposed_plan>"}}}
		}
		if event.Kind != Message {
			continue
		}
		var parts []any
		for _, part := range event.Parts {
			partID, err := shortID("prt_")
			if err != nil {
				return Result{}, err
			}
			switch part.Kind {
			case Text:
				parts = append(parts, map[string]any{"id": partID, "sessionID": id, "type": "text", "text": part.Text})
			case Reasoning:
				start := timestampOr(event.Timestamp, now).UnixMilli()
				parts = append(parts, map[string]any{"id": partID, "sessionID": id, "type": "reasoning", "text": part.Text, "time": map[string]any{"start": start, "end": start}})
			case Media:
				file := map[string]any{"id": partID, "sessionID": id, "type": "file", "mime": part.MediaType, "url": mediaDataURL(part)}
				if part.Filename != "" {
					file["filename"] = part.Filename
				}
				parts = append(parts, file)
			case ToolCall:
				stateTime := timestampOr(event.Timestamp, now).UnixMilli()
				input := map[string]any{}
				if err := json.Unmarshal(jsonObject(part.Data), &input); err != nil {
					return Result{}, err
				}
				state := map[string]any{"status": "error", "input": input, "error": "[Tool execution was interrupted before teleport]", "time": map[string]any{"start": stateTime, "end": stateTime}}
				if recorded, ok := toolResults[part.CallID]; ok {
					endTime := timestampOr(recorded.time, timestampOr(event.Timestamp, now)).UnixMilli()
					output := any(recorded.part.Text)
					if recorded.part.Kind == Media {
						output = map[string]any{"type": "image", "url": mediaDataURL(recorded.part), "mime": recorded.part.MediaType, "filename": recorded.part.Filename}
					}
					if recorded.part.Error {
						state = map[string]any{"status": "error", "input": input, "error": recorded.part.Text, "time": map[string]any{"start": stateTime, "end": endTime}}
					} else {
						state = map[string]any{"status": "completed", "input": input, "output": output, "title": part.ToolName, "metadata": map[string]any{}, "time": map[string]any{"start": stateTime, "end": endTime}}
					}
				}
				parts = append(parts, map[string]any{"id": partID, "sessionID": id, "type": "tool", "callID": part.CallID, "tool": part.ToolName, "state": state})
			case ToolResult:
				// OpenCode stores the result in its matching tool part.
			}
		}
		if len(parts) == 0 {
			continue
		}
		msgID, err := shortID("msg_")
		if err != nil {
			return Result{}, err
		}
		for _, rawPart := range parts {
			rawPart.(map[string]any)["messageID"] = msgID
		}
		ts := timestampOr(event.Timestamp, now).UnixMilli()
		role := event.Role
		if role == "tool" {
			continue
		}
		var info map[string]any
		if role == "user" {
			info = map[string]any{
				"id": msgID, "sessionID": id, "role": "user", "time": map[string]any{"created": ts},
				"agent": "build", "model": map[string]any{"providerID": provider, "modelID": model},
			}
		} else {
			parent := lastUser
			if parent == "" {
				parent, err = shortID("msg_root_")
				if err != nil {
					return Result{}, err
				}
			}
			info = map[string]any{
				"id": msgID, "sessionID": id, "role": "assistant", "time": map[string]any{"created": ts, "completed": ts},
				"parentID": parent, "modelID": model, "providerID": provider, "mode": "build", "agent": "build",
				"path": map[string]any{"cwd": canonical, "root": canonical}, "cost": 0,
				"tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
			}
		}
		messages = append(messages, map[string]any{"info": info, "parts": parts})
		if role == "user" {
			lastUser = msgID
		}
	}
	export["messages"] = messages
	if len(messages) == 0 {
		return Result{}, fmt.Errorf("canonical history produced no OpenCode messages")
	}
	tmp, err := os.CreateTemp("", "agentswap-opencode-*.json")
	if err != nil {
		return Result{}, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer removeIfExists(tmpPath)
	if err := writeJSONFile(tmpPath, export, 0o600); err != nil {
		return Result{}, err
	}
	stdout, stderr, err := runOpenCode(ctx, canonical, "import", tmpPath)
	if err != nil {
		rollbackOpenCode(canonical, id)
		return Result{}, fmt.Errorf("run `opencode import`: %w", err)
	}
	combined := string(stdout) + string(stderr)
	if !containsExactID(combined, id) || !strings.Contains(strings.ToLower(combined), "imported session") {
		rollbackOpenCode(canonical, id)
		return Result{}, fmt.Errorf("OpenCode import did not confirm session %s: %s", id, strings.TrimSpace(combined))
	}
	return result, nil
}

func containsExactID(text, id string) bool {
	for offset := 0; ; {
		index := strings.Index(text[offset:], id)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(id)
		isIDByte := func(b byte) bool {
			return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
		}
		if (index == 0 || !isIDByte(text[index-1])) && (end == len(text) || !isIDByte(text[end])) {
			return true
		}
		offset = index + 1
	}
}

func rollbackOpenCode(cwd, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _ = runOpenCode(ctx, cwd, "session", "delete", id)
}

func openCodeModel(history *Session) (provider, model string) {
	model = history.Model
	switch history.Source {
	case Claude:
		provider = "anthropic"
		if model == "" {
			model = "claude-sonnet-4-6"
		}
	case Kimi:
		provider = "moonshotai"
		if model == "" {
			model = "kimi-k2.5"
		}
	default:
		provider = "openai"
		if model == "" {
			model = "gpt-5.2"
		}
	}
	return provider, model
}
