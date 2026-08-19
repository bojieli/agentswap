package session

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type kimiAdapter struct{}

func newKimiAdapter() Adapter    { return kimiAdapter{} }
func (kimiAdapter) Agent() Agent { return Kimi }

func kimiCodeRoot() string   { return envDir("KIMI_CODE_HOME", filepath.Join(homeDir(), ".kimi-code")) }
func kimiLegacyRoot() string { return envDir("KIMI_SHARE_DIR", filepath.Join(homeDir(), ".kimi")) }

func (kimiAdapter) Discover(_ context.Context, cwd string) ([]Candidate, error) {
	canonical, err := canonicalPath(cwd)
	if err != nil {
		return nil, err
	}
	modern, modernErr := discoverKimiCode(canonical)
	legacy, legacyErr := discoverKimiLegacy(canonical)
	out := append(modern, legacy...)
	if len(out) == 0 {
		switch {
		case modernErr != nil && legacyErr != nil:
			return nil, fmt.Errorf("Kimi Code: %v; legacy Kimi CLI: %v", modernErr, legacyErr)
		case modernErr != nil:
			return nil, fmt.Errorf("Kimi Code: %w", modernErr)
		case legacyErr != nil:
			return nil, fmt.Errorf("legacy Kimi CLI: %w", legacyErr)
		}
	}
	SortCandidates(out)
	return out, nil
}

func discoverKimiCode(cwd string) ([]Candidate, error) {
	root := filepath.Join(kimiCodeRoot(), "sessions")
	workdirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, workdir := range workdirs {
		if !workdir.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(root, workdir.Name()))
		if err != nil {
			continue
		}
		for _, entry := range sessions {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session_") {
				continue
			}
			dir := filepath.Join(root, workdir.Name(), entry.Name())
			statePath := filepath.Join(dir, "state.json")
			b, err := os.ReadFile(statePath)
			if err != nil {
				continue
			}
			var state struct {
				ID        string `json:"id"`
				CWD       string `json:"cwd"`
				WorkDir   string `json:"workDir"`
				Title     string `json:"title"`
				CreatedAt any    `json:"createdAt"`
				UpdatedAt any    `json:"updatedAt"`
				Archived  bool   `json:"archived"`
			}
			if json.Unmarshal(b, &state) != nil || state.Archived {
				continue
			}
			if state.ID == "" {
				state.ID = entry.Name()
			}
			if state.CWD == "" {
				state.CWD = state.WorkDir
			}
			if state.ID != entry.Name() || state.CWD == "" || !samePath(state.CWD, cwd) {
				continue
			}
			info, _ := os.Stat(statePath)
			updated := parseFlexibleTime(state.UpdatedAt)
			if updated.IsZero() && info != nil {
				updated = info.ModTime()
			}
			out = append(out, Candidate{Agent: Kimi, ID: state.ID, CWD: state.CWD, Path: dir, Format: "kimi-code-wire", Title: state.Title, UpdatedAt: updated})
		}
	}
	return out, nil
}

func discoverKimiLegacy(cwd string) ([]Candidate, error) {
	metadataPath := filepath.Join(kimiLegacyRoot(), "kimi.json")
	b, err := os.ReadFile(metadataPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var metadata struct {
		WorkDirs []struct {
			Path string `json:"path"`
			KAOS string `json:"kaos"`
		} `json:"work_dirs"`
	}
	if err := json.Unmarshal(b, &metadata); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metadataPath, err)
	}
	seenDirs := make(map[string]bool)
	var matched []struct{ path, kaos string }
	for _, workdir := range metadata.WorkDirs {
		if samePath(workdir.Path, cwd) {
			matched = append(matched, struct{ path, kaos string }{workdir.Path, workdir.KAOS})
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	var out []Candidate
	for _, workdir := range matched {
		hash := md5.Sum([]byte(workdir.path)) // legacy Kimi intentionally uses MD5 as a directory key.
		key := hex.EncodeToString(hash[:])
		if workdir.kaos != "" && workdir.kaos != "local" {
			if filepath.Base(workdir.kaos) != workdir.kaos || workdir.kaos == "." || workdir.kaos == ".." {
				return nil, fmt.Errorf("unsafe legacy Kimi KAOS name %q", workdir.kaos)
			}
			key = workdir.kaos + "_" + key
		}
		dir := filepath.Join(kimiLegacyRoot(), "sessions", key)
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			full := filepath.Join(dir, entry.Name())
			wire := filepath.Join(full, "wire.jsonl")
			info, err := os.Stat(wire)
			if err != nil {
				continue
			}
			out = append(out, Candidate{Agent: Kimi, ID: entry.Name(), CWD: workdir.path, Path: full, Format: "kimi-cli-v1", UpdatedAt: info.ModTime(), Size: info.Size()})
		}
	}
	return out, nil
}

func (kimiAdapter) Read(_ context.Context, candidate Candidate) (*Session, error) {
	switch candidate.Format {
	case "kimi-code-wire", "kimi-code-v2":
		return readKimiCode(candidate)
	case "kimi-cli-v1":
		return readKimiLegacy(candidate)
	default:
		return nil, fmt.Errorf("unsupported Kimi session format %q", candidate.Format)
	}
}

func readKimiCode(candidate Candidate) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(candidate.Path, "state.json"))
	if err != nil {
		return nil, err
	}
	var state struct {
		ID        string         `json:"id"`
		Version   int            `json:"version"`
		CWD       string         `json:"cwd"`
		WorkDir   string         `json:"workDir"`
		Title     string         `json:"title"`
		CreatedAt any            `json:"createdAt"`
		UpdatedAt any            `json:"updatedAt"`
		Custom    map[string]any `json:"custom"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	if state.Version != 0 && state.Version != 2 {
		return nil, fmt.Errorf("unexpected modern Kimi state version %d", state.Version)
	}
	if state.ID == "" {
		state.ID = filepath.Base(candidate.Path)
	}
	if state.CWD == "" {
		state.CWD = state.WorkDir
	}
	if state.ID != candidate.ID || state.CWD == "" || !samePath(state.CWD, candidate.CWD) {
		return nil, fmt.Errorf("Kimi state id or cwd does not match discovery metadata")
	}
	history := &Session{
		Source: Kimi, SourceID: state.ID, CWD: state.CWD, Title: state.Title,
		CreatedAt: parseFlexibleTime(state.CreatedAt), UpdatedAt: parseFlexibleTime(state.UpdatedAt),
	}
	if metadata, ok := state.Custom["agentswap"].(map[string]any); ok {
		history.Model = stringValue(metadata["sourceModel"])
	}
	agentHome := filepath.Join(candidate.Path, "agents", "main")
	if err := rejectKimiSubagents(filepath.Join(candidate.Path, "agents")); err != nil {
		return nil, err
	}
	wire := filepath.Join(agentHome, "wire.jsonl")
	seenMetadata := false
	seenPlans := make(map[string]bool)
	err = readJSONL(wire, func(_ int, raw json.RawMessage) error {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		kind := stringValue(record["type"])
		ts := parseFlexibleTime(record["time"])
		switch kind {
		case "metadata":
			seenMetadata = true
		case "profile.bind":
			if model := stringValue(record["modelAlias"]); model != "" {
				history.Model = model
			}
		case "context.append_message":
			message, ok := record["message"].(map[string]any)
			if !ok {
				return fmt.Errorf("context.append_message has no message")
			}
			role := stringValue(message["role"])
			if role != "user" && role != "assistant" {
				return fmt.Errorf("unsupported Kimi context role %q", role)
			}
			event := Event{Kind: Message, ID: stringValue(message["id"]), Role: role, Timestamp: ts}
			content, ok := message["content"].([]any)
			if !ok {
				return fmt.Errorf("Kimi context content is not an array")
			}
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("Kimi context contains a non-object block")
				}
				blockType := stringValue(block["type"])
				if blockType != "text" {
					return fmt.Errorf("unsupported Kimi context block %q", blockType)
				}
				if text := stringValue(block["text"]); text != "" {
					event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
				}
			}
			if len(event.Parts) > 0 {
				history.Events = append(history.Events, event)
			}
		case "context.append_loop_event":
			event, ok := record["event"].(map[string]any)
			if !ok {
				return fmt.Errorf("context.append_loop_event has no event")
			}
			switch eventType := stringValue(event["type"]); eventType {
			case "content.part":
				part, ok := event["part"].(map[string]any)
				if !ok {
					return fmt.Errorf("content.part has no part")
				}
				switch partType := stringValue(part["type"]); partType {
				case "text":
					if text := stringValue(part["text"]); text != "" {
						history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: Text, ID: stringValue(event["uuid"]), Text: text}}})
					}
				case "think":
					if text := stringValue(part["think"]); text != "" {
						history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: Reasoning, ID: stringValue(event["uuid"]), Text: text}}})
					}
				default:
					return fmt.Errorf("unsupported Kimi content part %q", partType)
				}
			case "tool.call":
				input, err := json.Marshal(event["args"])
				if err != nil {
					return err
				}
				if string(input) == "null" {
					input = []byte(`{}`)
				}
				history.Events = append(history.Events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: ToolCall, ID: stringValue(event["uuid"]), CallID: stringValue(event["toolCallId"]), ToolName: stringValue(event["name"]), Data: jsonObject(input)}}})
			case "tool.result":
				result, ok := event["result"].(map[string]any)
				if !ok {
					return fmt.Errorf("tool.result has no result")
				}
				text, err := kimiOutputText(result["output"])
				if err != nil {
					return err
				}
				isError, _ := result["isError"].(bool)
				history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Timestamp: ts, Parts: []Part{{Kind: ToolResult, CallID: stringValue(event["toolCallId"]), Text: text, Error: isError}}})
			case "step.begin", "step.end":
			default:
				return fmt.Errorf("unsupported Kimi loop event %q", eventType)
			}
		case "plan.revision":
			path := stringValue(record["path"])
			if path == "" || seenPlans[path] {
				break
			}
			seenPlans[path] = true
			full, err := resolveKimiPlanPath(agentHome, path)
			if err != nil {
				return err
			}
			plan, err := os.ReadFile(full)
			if err != nil {
				return fmt.Errorf("read Kimi plan %s: %w", path, err)
			}
			if expected := stringValue(record["sha256"]); expected != "" && hashHex(plan) != expected {
				return fmt.Errorf("Kimi plan %s failed its SHA-256 check", path)
			}
			history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: ts, PlanText: string(plan)})
		case "context.apply_compaction", "full_compaction.begin", "full_compaction.complete":
			history.Warnings = appendUnique(history.Warnings, "source contains Kimi compaction records; original observable wire events were retained")
		case "config.update", "goal.clear", "goal.create", "goal.update", "interaction.request", "interaction.resolved", "interruptionReminder.recorded", "llm.request", "llm.tools_snapshot", "permission.record_approval_result", "permission.set_mode", "plan_mode.enter", "plan_mode.exit", "plugin.session_start", "prompt.accepted", "runtime.set_binding", "swarm_mode.enter", "swarm_mode.exit", "task.started", "task.terminated", "token_counting.measured", "tools.set_active_tools", "tools.update_store", "turn.cancel", "turn.ended", "turn.prompt", "turn.steer", "usage.record":
			// UI, permission, usage, profile and task lifecycle records do not
			// add model conversation content.
		default:
			return fmt.Errorf("unsupported Kimi wire record %q", kind)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !seenMetadata {
		return nil, fmt.Errorf("Kimi wire log has no metadata header")
	}
	if history.Title == "" {
		history.Title = firstText(history)
	}
	return history, nil
}

func safeChildPath(root, child string) (string, error) {
	if filepath.IsAbs(child) {
		return "", fmt.Errorf("unsafe absolute child path %q", child)
	}
	clean := filepath.Clean(child)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe child path %q", child)
	}
	return filepath.Join(root, clean), nil
}

func resolveKimiPlanPath(agentHome, recorded string) (string, error) {
	var candidates []string
	if filepath.IsAbs(recorded) {
		candidates = append(candidates, filepath.Clean(recorded))
	} else {
		for _, root := range []string{agentHome, kimiCodeRoot()} {
			path, err := safeChildPath(root, recorded)
			if err != nil {
				return "", err
			}
			candidates = append(candidates, path)
		}
	}
	canonicalAgent, err := canonicalPath(agentHome)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		canonicalCandidate, err := canonicalPath(candidate)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(canonicalAgent, canonicalCandidate)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Kimi plan path %q is missing or outside the main agent directory", recorded)
}

func rejectKimiSubagents(agentsDir string) error {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "main" {
			return fmt.Errorf("Kimi session contains subagent %q; branched agent histories are not safely transferable yet", entry.Name())
		}
	}
	return nil
}

func kimiOutputText(output any) (string, error) {
	if text, ok := output.(string); ok {
		return text, nil
	}
	if blocks, ok := output.([]any); ok {
		return claudeResultText(blocks)
	}
	return stringifyOutput(output)
}

func readKimiLegacy(candidate Candidate) (*Session, error) {
	contextPath := filepath.Join(candidate.Path, "context.jsonl")
	history := &Session{Source: Kimi, SourceID: candidate.ID, CWD: candidate.CWD, UpdatedAt: candidate.UpdatedAt}
	err := readJSONL(contextPath, func(_ int, raw json.RawMessage) error {
		var message map[string]any
		if err := json.Unmarshal(raw, &message); err != nil {
			return err
		}
		role := stringValue(message["role"])
		switch role {
		case "user", "assistant":
			event := Event{Kind: Message, Role: role}
			switch content := message["content"].(type) {
			case string:
				if content != "" {
					event.Parts = append(event.Parts, Part{Kind: Text, Text: content})
				}
			case []any:
				for _, item := range content {
					block, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("legacy Kimi content contains a non-object block")
					}
					switch stringValue(block["type"]) {
					case "text":
						if text := stringValue(block["text"]); text != "" {
							event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
						}
					case "think":
						if text := stringValue(block["think"]); text != "" {
							event.Parts = append(event.Parts, Part{Kind: Reasoning, Text: text})
						}
					default:
						return fmt.Errorf("unsupported legacy Kimi content block %q", stringValue(block["type"]))
					}
				}
			case nil:
			default:
				return fmt.Errorf("unsupported legacy Kimi content type %T", content)
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, item := range calls {
					call, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("legacy Kimi tool call is not an object")
					}
					fn, _ := call["function"].(map[string]any)
					input, err := codexInputJSON(fn["arguments"])
					if err != nil {
						return err
					}
					id := stringValue(call["id"])
					event.Parts = append(event.Parts, Part{Kind: ToolCall, CallID: id, ID: id, ToolName: stringValue(fn["name"]), Data: input})
				}
			}
			if len(event.Parts) > 0 {
				history.Events = append(history.Events, event)
			}
		case "tool":
			text, err := kimiOutputText(message["content"])
			if err != nil {
				return err
			}
			history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Parts: []Part{{Kind: ToolResult, CallID: stringValue(message["tool_call_id"]), Text: text}}})
		default:
			return fmt.Errorf("unsupported legacy Kimi role %q", role)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	wirePath := filepath.Join(candidate.Path, "wire.jsonl")
	toolErrors := make(map[string]bool)
	err = readJSONL(wirePath, func(_ int, raw json.RawMessage) error {
		var record struct {
			Timestamp any `json:"timestamp"`
			Message   struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			} `json:"message"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.Type == "metadata" {
			return nil
		}
		switch record.Message.Type {
		case "PlanDisplay":
			text := stringValue(record.Message.Payload["content"])
			if strings.TrimSpace(text) != "" {
				history.Events = append(history.Events, Event{Kind: Plan, Role: "assistant", Timestamp: parseFlexibleTime(record.Timestamp), PlanText: text})
			}
		case "ToolResult":
			callID := stringValue(record.Message.Payload["tool_call_id"])
			if value, ok := record.Message.Payload["return_value"].(map[string]any); ok {
				isError, _ := value["is_error"].(bool)
				toolErrors[callID] = isError
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for ei := range history.Events {
		for pi := range history.Events[ei].Parts {
			part := &history.Events[ei].Parts[pi]
			if part.Kind == ToolResult && toolErrors[part.CallID] {
				part.Error = true
			}
		}
	}
	history.Title = firstText(history)
	return history, nil
}

func (kimiAdapter) Write(_ context.Context, history *Session, opts WriteOptions) (Result, error) {
	format := strings.ToLower(strings.TrimSpace(os.Getenv("AGENTSWAP_KIMI_FORMAT")))
	if format != "" && format != "modern" && format != "legacy" {
		return Result{}, fmt.Errorf("AGENTSWAP_KIMI_FORMAT must be modern or legacy, got %q", format)
	}
	if format == "legacy" {
		return writeKimiLegacy(history, opts)
	}
	return writeKimiCode(history, opts)
}

func writeKimiCode(history *Session, opts WriteOptions) (result Result, err error) {
	uuid, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	id := "session_" + uuid
	canonical, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	workdir := filepath.Join(kimiCodeRoot(), "sessions", kimiWorkdirKey(canonical))
	final := filepath.Join(workdir, id)
	agentHome := filepath.Join(final, "agents", "main")
	result = Result{Agent: Kimi, ID: id, Path: final, Resume: []string{"kimi", "--session", id}, Files: []string{filepath.Join(final, "state.json"), filepath.Join(agentHome, "wire.jsonl")}}
	if opts.DryRun {
		return result, nil
	}
	if err := ensureDir(workdir); err != nil {
		return Result{}, err
	}
	stage := filepath.Join(workdir, ".agentswap-"+id+".tmp")
	committed := false
	defer func() {
		if !committed {
			removeIfExists(stage)
		}
	}()
	if err := os.Mkdir(stage, 0o700); err != nil {
		return Result{}, err
	}
	stageAgent := filepath.Join(stage, "agents", "main")
	if err := ensureDir(stageAgent); err != nil {
		return Result{}, err
	}
	now := time.Now()
	created := now.UnixMilli()
	state := map[string]any{
		"createdAt": now.UTC().Format(time.RFC3339Nano), "updatedAt": now.UTC().Format(time.RFC3339Nano),
		"agents": map[string]any{"main": map[string]any{"homedir": filepath.Join(final, "agents", "main"), "type": "main", "parentAgentId": nil}},
		"custom": map[string]any{"agentswap": map[string]any{
			"sourceAgent": history.Source, "sourceSessionID": history.SourceID, "sourceModel": history.Model,
			"sourceCreatedAt": history.CreatedAt, "sourceUpdatedAt": history.UpdatedAt,
		}},
		"lastPrompt": lastUserText(history), "title": safeTitle(history.Title, firstText(history)),
		"workDir": canonical, "isCustomTitle": true,
	}
	if err := writeJSONFile(filepath.Join(stage, "state.json"), state, 0o600); err != nil {
		return Result{}, err
	}
	wirePath := filepath.Join(stageAgent, "wire.jsonl")
	f, err := os.OpenFile(wirePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if err := writeJSONLine(f, map[string]any{"type": "metadata", "protocol_version": "1.5", "created_at": created}); err != nil {
		return Result{}, err
	}
	callParents := make(map[string]string)
	resultIDs := make(map[string]bool)
	for _, event := range history.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult {
				resultIDs[part.CallID] = true
			}
		}
	}
	turnID, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	emitToolResult := func(part Part, ts int64) error {
		parent := callParents[part.CallID]
		if parent == "" {
			return fmt.Errorf("Kimi tool result %s has no emitted call", part.CallID)
		}
		resultValue := map[string]any{"output": part.Text}
		if part.Error {
			resultValue["isError"] = true
		}
		loop := map[string]any{"type": "tool.result", "parentUuid": parent, "toolCallId": part.CallID, "result": resultValue}
		return writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": loop, "time": ts})
	}
	step := 0
	planIndex := 0
	for _, event := range history.Events {
		ts := timestampOr(event.Timestamp, now).UnixMilli()
		if event.Kind == Plan {
			planIndex++
			planID := fmt.Sprintf("agentswap-import-%d", planIndex)
			content := []byte(event.PlanText)
			planPath := filepath.Join("plan", planID, "v1.md")
			full := filepath.Join(stageAgent, planPath)
			if err := ensureDir(filepath.Dir(full)); err != nil {
				return Result{}, err
			}
			if err := os.WriteFile(full, content, 0o600); err != nil {
				return Result{}, err
			}
			if err := ensureDir(filepath.Join(stageAgent, "plans")); err != nil {
				return Result{}, err
			}
			if err := os.WriteFile(filepath.Join(stageAgent, "plans", planID+".md"), content, 0o600); err != nil {
				return Result{}, err
			}
			publishedPlan := filepath.Join(agentHome, planPath)
			recordedPath, err := filepath.Rel(kimiCodeRoot(), publishedPlan)
			if err != nil {
				return Result{}, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "plan.revision", "id": planID, "version": 1, "path": filepath.ToSlash(recordedPath), "sha256": hashHex(content), "bytes": len(content), "time": ts}); err != nil {
				return Result{}, err
			}
			result.Files = append(result.Files, publishedPlan)
			continue
		}
		if event.Role == "user" {
			var content []any
			flushUser := func() error {
				if len(content) == 0 {
					return nil
				}
				turnID, err = newUUID()
				if err != nil {
					return err
				}
				messageID, err := shortID("msg_")
				if err != nil {
					return err
				}
				message := map[string]any{"role": "user", "content": content, "toolCalls": []any{}, "origin": map[string]any{"kind": "user"}, "id": messageID}
				if err := writeJSONLine(f, map[string]any{"type": "context.append_message", "message": message, "time": ts}); err != nil {
					return err
				}
				if err := writeJSONLine(f, map[string]any{"type": "turn.prompt", "input": content, "origin": map[string]any{"kind": "user"}, "time": ts}); err != nil {
					return err
				}
				content = nil
				step = 0
				return nil
			}
			for _, part := range event.Parts {
				switch part.Kind {
				case Text:
					content = append(content, map[string]any{"type": "text", "text": part.Text})
				case ToolResult:
					if err := flushUser(); err != nil {
						return Result{}, err
					}
					if err := emitToolResult(part, ts); err != nil {
						return Result{}, err
					}
				default:
					return Result{}, fmt.Errorf("cannot write %s part in a Kimi user message", part.Kind)
				}
			}
			if err := flushUser(); err != nil {
				return Result{}, err
			}
			continue
		}
		if event.Role == "assistant" {
			stepUUID, err := newUUID()
			if err != nil {
				return Result{}, err
			}
			beginUUID, err := newUUID()
			if err != nil {
				return Result{}, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": map[string]any{"type": "step.begin", "uuid": beginUUID, "turnId": turnID, "step": step}, "time": ts}); err != nil {
				return Result{}, err
			}
			var dangling []Part
			for _, part := range event.Parts {
				eventUUID, err := newUUID()
				if err != nil {
					return Result{}, err
				}
				var loop map[string]any
				switch part.Kind {
				case Text:
					loop = map[string]any{"type": "content.part", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "part": map[string]any{"type": "text", "text": part.Text}}
				case Reasoning:
					loop = map[string]any{"type": "content.part", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "part": map[string]any{"type": "think", "think": part.Text}}
				case ToolCall:
					var args map[string]any
					if err := json.Unmarshal(jsonObject(part.Data), &args); err != nil {
						return Result{}, err
					}
					loop = map[string]any{"type": "tool.call", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "toolCallId": part.CallID, "name": part.ToolName, "args": args}
					callParents[part.CallID] = eventUUID
					if !resultIDs[part.CallID] {
						dangling = append(dangling, Part{Kind: ToolResult, CallID: part.CallID, Text: "[Tool execution was interrupted before teleport]", Error: true})
					}
				case ToolResult:
					return Result{}, fmt.Errorf("tool result appeared in an assistant event")
				}
				if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": loop, "time": ts}); err != nil {
					return Result{}, err
				}
			}
			endUUID, err := newUUID()
			if err != nil {
				return Result{}, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": map[string]any{"type": "step.end", "uuid": endUUID, "turnId": turnID, "step": step}, "time": ts}); err != nil {
				return Result{}, err
			}
			step++
			for _, part := range dangling {
				if err := emitToolResult(part, ts); err != nil {
					return Result{}, err
				}
			}
			continue
		}
		if event.Role == "tool" {
			for _, part := range event.Parts {
				if part.Kind != ToolResult {
					return Result{}, fmt.Errorf("non-result part appeared in a tool event")
				}
				if err := emitToolResult(part, ts); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if err := f.Sync(); err != nil {
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(stage, final); err != nil {
		return Result{}, err
	}
	committed = true
	return result, nil
}

func kimiWorkdirKey(cwd string) string {
	base := strings.ToLower(filepath.Base(cwd))
	base = strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, base), "-")
	if base == "" {
		base = "workspace"
	}
	return "wd_" + base + "_" + hash12(cwd)
}

func lastUserText(history *Session) string {
	for i := len(history.Events) - 1; i >= 0; i-- {
		if history.Events[i].Kind != Message || history.Events[i].Role != "user" {
			continue
		}
		for j := len(history.Events[i].Parts) - 1; j >= 0; j-- {
			if history.Events[i].Parts[j].Kind == Text {
				return history.Events[i].Parts[j].Text
			}
		}
	}
	return ""
}

func writeKimiLegacy(history *Session, opts WriteOptions) (result Result, err error) {
	id, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	canonical, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	hash := md5.Sum([]byte(canonical))
	workdir := filepath.Join(kimiLegacyRoot(), "sessions", hex.EncodeToString(hash[:]))
	final := filepath.Join(workdir, id)
	result = Result{
		Agent: Kimi, ID: id, Path: final, Resume: []string{"kimi", "-r", id},
		Files: []string{filepath.Join(final, "context.jsonl"), filepath.Join(final, "wire.jsonl"), filepath.Join(final, "state.json")},
	}
	if opts.DryRun {
		return result, nil
	}
	if err := ensureDir(workdir); err != nil {
		return Result{}, err
	}
	stage := filepath.Join(workdir, ".agentswap-"+id+".tmp")
	if err := os.Mkdir(stage, 0o700); err != nil {
		return Result{}, err
	}
	committed := false
	published := false
	defer func() {
		if !committed {
			removeIfExists(stage)
			if published {
				removeIfExists(final)
			}
		}
	}()
	contextFile, err := os.OpenFile(filepath.Join(stage, "context.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	wireFile, err := os.OpenFile(filepath.Join(stage, "wire.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = contextFile.Close()
		return Result{}, err
	}
	defer contextFile.Close()
	defer wireFile.Close()
	if err := writeJSONLine(wireFile, map[string]any{"type": "metadata", "protocol_version": "1.10"}); err != nil {
		return Result{}, err
	}
	wire := func(ts time.Time, messageType string, payload map[string]any) error {
		when := timestampOr(ts, time.Now())
		return writeJSONLine(wireFile, map[string]any{
			"timestamp": float64(when.UnixNano()) / 1e9,
			"message":   map[string]any{"type": messageType, "payload": payload},
		})
	}
	resultIDs := make(map[string]bool)
	for _, event := range history.Events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult {
				resultIDs[part.CallID] = true
			}
		}
	}
	emitToolResult := func(part Part, ts time.Time) error {
		if err := writeJSONLine(contextFile, map[string]any{"role": "tool", "content": part.Text, "tool_call_id": part.CallID}); err != nil {
			return err
		}
		payload := map[string]any{
			"tool_call_id": part.CallID,
			"return_value": map[string]any{"is_error": part.Error, "output": part.Text, "message": "", "display": []any{}},
		}
		return wire(ts, "ToolResult", payload)
	}
	step := 0
	planIndex := 0
	for _, event := range history.Events {
		if event.Kind == Plan {
			planIndex++
			planDir := filepath.Join(stage, "plans")
			if err := ensureDir(planDir); err != nil {
				return Result{}, err
			}
			planName := fmt.Sprintf("agentswap-import-%d.md", planIndex)
			planPath := filepath.Join(planDir, planName)
			publishedPlan := filepath.Join(final, "plans", planName)
			if err := os.WriteFile(planPath, []byte(event.PlanText), 0o600); err != nil {
				return Result{}, err
			}
			if err := wire(event.Timestamp, "PlanDisplay", map[string]any{"content": event.PlanText, "file_path": publishedPlan}); err != nil {
				return Result{}, err
			}
			result.Files = append(result.Files, publishedPlan)
			continue
		}
		if event.Role == "user" {
			var blocks []any
			var texts []string
			flushUser := func() error {
				if len(blocks) == 0 {
					return nil
				}
				content := any(strings.Join(texts, "\n"))
				if len(blocks) > 1 {
					content = blocks
				}
				if err := writeJSONLine(contextFile, map[string]any{"role": "user", "content": content}); err != nil {
					return err
				}
				if err := wire(event.Timestamp, "TurnBegin", map[string]any{"user_input": content}); err != nil {
					return err
				}
				blocks = nil
				texts = nil
				step = 0
				return nil
			}
			for _, part := range event.Parts {
				switch part.Kind {
				case Text:
					blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
					texts = append(texts, part.Text)
				case ToolResult:
					if err := flushUser(); err != nil {
						return Result{}, err
					}
					if err := emitToolResult(part, event.Timestamp); err != nil {
						return Result{}, err
					}
				default:
					return Result{}, fmt.Errorf("cannot write %s part in a legacy Kimi user message", part.Kind)
				}
			}
			if err := flushUser(); err != nil {
				return Result{}, err
			}
			continue
		}
		if event.Role == "assistant" {
			if err := wire(event.Timestamp, "StepBegin", map[string]any{"n": step}); err != nil {
				return Result{}, err
			}
			var content []any
			var calls []any
			var dangling []Part
			for _, part := range event.Parts {
				switch part.Kind {
				case Text:
					block := map[string]any{"type": "text", "text": part.Text}
					content = append(content, block)
					if err := wire(event.Timestamp, "TextPart", block); err != nil {
						return Result{}, err
					}
				case Reasoning:
					block := map[string]any{"type": "think", "think": part.Text, "encrypted": nil}
					content = append(content, block)
					if err := wire(event.Timestamp, "ThinkPart", block); err != nil {
						return Result{}, err
					}
				case ToolCall:
					call := map[string]any{"type": "function", "id": part.CallID, "function": map[string]any{"name": part.ToolName, "arguments": string(jsonObject(part.Data))}}
					calls = append(calls, call)
					if err := wire(event.Timestamp, "ToolCall", call); err != nil {
						return Result{}, err
					}
					if !resultIDs[part.CallID] {
						dangling = append(dangling, Part{Kind: ToolResult, CallID: part.CallID, Text: "[Tool execution was interrupted before teleport]", Error: true})
					}
				case ToolResult:
					return Result{}, fmt.Errorf("tool result appeared in an assistant event")
				}
			}
			message := map[string]any{"role": "assistant", "content": content}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			if err := writeJSONLine(contextFile, message); err != nil {
				return Result{}, err
			}
			step++
			for _, part := range dangling {
				if err := emitToolResult(part, event.Timestamp); err != nil {
					return Result{}, err
				}
			}
			continue
		}
		if event.Role == "tool" {
			for _, part := range event.Parts {
				if part.Kind != ToolResult {
					return Result{}, fmt.Errorf("non-result part appeared in a legacy Kimi tool message")
				}
				if err := emitToolResult(part, event.Timestamp); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if err := wireFile.Sync(); err != nil {
		return Result{}, err
	}
	if err := contextFile.Sync(); err != nil {
		return Result{}, err
	}
	if err := wireFile.Close(); err != nil {
		return Result{}, err
	}
	if err := contextFile.Close(); err != nil {
		return Result{}, err
	}
	state := map[string]any{
		"version": 1, "approval": map[string]any{"yolo": false, "afk": false, "auto_approve_actions": []any{}},
		"additional_dirs": []any{}, "custom_title": safeTitle(history.Title, firstText(history)),
		"title_generated": false, "title_generate_attempts": 0, "plan_mode": false,
		"archived": false, "auto_archive_exempt": false, "todos": []any{},
	}
	if err := writeJSONFile(filepath.Join(stage, "state.json"), state, 0o600); err != nil {
		return Result{}, err
	}
	if err := os.Rename(stage, final); err != nil {
		return Result{}, err
	}
	published = true
	if err := updateKimiLegacyMetadata(canonical, id); err != nil {
		return Result{}, err
	}
	committed = true
	return result, nil
}

func updateKimiLegacyMetadata(cwd, sessionID string) error {
	path := filepath.Join(kimiLegacyRoot(), "kimi.json")
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	metadata := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &metadata); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var workdirs []any
	if existing, ok := metadata["work_dirs"].([]any); ok {
		workdirs = existing
	}
	updated := false
	for _, item := range workdirs {
		workdir, ok := item.(map[string]any)
		if !ok || filepath.Clean(stringValue(workdir["path"])) != filepath.Clean(cwd) || stringValue(workdir["kaos"]) != "" && stringValue(workdir["kaos"]) != "local" {
			continue
		}
		workdir["path"] = cwd
		workdir["last_session_id"] = sessionID
		if _, ok := workdir["kaos"]; !ok {
			workdir["kaos"] = "local"
		}
		updated = true
		break
	}
	if !updated {
		workdirs = append(workdirs, map[string]any{"path": cwd, "kaos": "local", "last_session_id": sessionID})
	}
	metadata["work_dirs"] = workdirs
	tmp := path + ".agentswap.tmp"
	removeIfExists(tmp)
	if err := writeJSONFile(tmp, metadata, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		removeIfExists(tmp)
		return err
	}
	return nil
}
