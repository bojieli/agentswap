package session

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type kimiAdapter struct{}

func newKimiAdapter() Adapter    { return kimiAdapter{} }
func (kimiAdapter) Agent() Agent { return Kimi }

func kimiCodeRoot() string   { return envDir("KIMI_CODE_HOME", filepath.Join(homeDir(), ".kimi-code")) }
func kimiLegacyRoot() string { return envDir("KIMI_SHARE_DIR", filepath.Join(homeDir(), ".kimi")) }

func kimiResumeModel() (string, error) {
	if model := strings.TrimSpace(os.Getenv("AGENTSWAP_KIMI_MODEL")); model != "" {
		return model, nil
	}
	path := filepath.Join(kimiCodeRoot(), "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Kimi default model from %s: %w (or set AGENTSWAP_KIMI_MODEL)", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "default_model" {
			continue
		}
		raw = strings.TrimSpace(trimKimiTOMLComment(raw))
		var model string
		switch {
		case len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"':
			if err := json.Unmarshal([]byte(raw), &model); err != nil {
				return "", fmt.Errorf("parse Kimi default_model in %s: %w", path, err)
			}
		case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'':
			model = raw[1 : len(raw)-1]
		default:
			return "", fmt.Errorf("Kimi default_model in %s must be a quoted string", path)
		}
		if model = strings.TrimSpace(model); model != "" {
			return model, nil
		}
		return "", fmt.Errorf("Kimi default_model in %s is empty (or set AGENTSWAP_KIMI_MODEL)", path)
	}
	return "", fmt.Errorf("Kimi default_model is not configured in %s (or set AGENTSWAP_KIMI_MODEL)", path)
}

func trimKimiTOMLComment(value string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote == 0 {
			switch c {
			case '#':
				return value[:i]
			case '\'', '"':
				quote = c
			}
			continue
		}
		if quote == '"' && c == '\\' && !escaped {
			escaped = true
			continue
		}
		if c == quote && !escaped {
			quote = 0
		}
		escaped = false
	}
	return value
}

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
		ID        string                   `json:"id"`
		Version   int                      `json:"version"`
		CWD       string                   `json:"cwd"`
		WorkDir   string                   `json:"workDir"`
		Title     string                   `json:"title"`
		CreatedAt any                      `json:"createdAt"`
		UpdatedAt any                      `json:"updatedAt"`
		Custom    map[string]any           `json:"custom"`
		Agents    map[string]kimiAgentNode `json:"agents"`
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
	agentsDir := filepath.Join(candidate.Path, "agents")
	agentHome := filepath.Join(agentsDir, "main")
	main, err := parseKimiWire(filepath.Join(agentHome, "wire.jsonl"), agentHome)
	if err != nil {
		return nil, err
	}
	if !main.metadata {
		return nil, fmt.Errorf("Kimi wire log has no metadata header")
	}
	history.Events = main.events
	if main.model != "" {
		history.Model = main.model
	}
	history.Warnings = append(history.Warnings, main.warnings...)
	branches, warnings, err := readKimiBranches(agentsDir, state.Agents, main.tasks, main.events)
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

// kimiTask is the lifecycle record Kimi writes for a delegated run. It is the
// only place the wire log links a subagent directory back to the tool call in
// its parent that spawned it.
type kimiTask struct {
	TaskID       string `json:"taskId"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	Kind         string `json:"kind"`
	AgentID      string `json:"agentId"`
	SubagentType string `json:"subagentType"`
	ParentCallID string `json:"parentToolCallId"`
	Model        string `json:"model"`
	StartedAt    any    `json:"startedAt"`
	EndedAt      any    `json:"endedAt"`
}

// kimiWire is one parsed Kimi wire log: the main thread or a single subagent.
type kimiWire struct {
	events   []Event
	model    string
	warnings []string
	tasks    []kimiTask
	metadata bool
}

// parseKimiWire reads one agent's wire log into canonical events. agentHome
// anchors plan-path resolution to the agent that recorded the revision, so a
// subagent's plan cannot be read out of the main agent's directory.
func parseKimiWire(wirePath, agentHome string) (*kimiWire, error) {
	wire := &kimiWire{}
	seenPlans := make(map[string]bool)
	err := readJSONL(wirePath, func(_ int, raw json.RawMessage) error {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		kind := stringValue(record["type"])
		ts := parseFlexibleTime(record["time"])
		switch kind {
		case "metadata":
			wire.metadata = true
		case "profile.bind":
			if model := stringValue(record["modelAlias"]); model != "" {
				wire.model = model
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
				switch blockType {
				case "text":
					if text := stringValue(block["text"]); text != "" {
						event.Parts = append(event.Parts, Part{Kind: Text, Text: text})
					}
				case "image", "input_image":
					media, err := mediaPartFromValue(block["url"], stringValue(block["mime"]), stringValue(block["filename"]))
					if err != nil {
						media, err = mediaPartFromValue(block["image_url"], stringValue(block["mime"]), stringValue(block["filename"]))
					}
					if err != nil {
						return fmt.Errorf("Kimi media block: %w", err)
					}
					event.Parts = append(event.Parts, media)
				default:
					return fmt.Errorf("unsupported Kimi context block %q", blockType)
				}
			}
			if len(event.Parts) > 0 {
				wire.events = append(wire.events, event)
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
						wire.events = append(wire.events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: Text, ID: stringValue(event["uuid"]), Text: text}}})
					}
				case "think":
					if text := stringValue(part["think"]); text != "" {
						wire.events = append(wire.events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: Reasoning, ID: stringValue(event["uuid"]), Text: text}}})
					}
				case "image":
					media, err := mediaPartFromValue(part["url"], stringValue(part["mime"]), stringValue(part["filename"]))
					if err != nil {
						media, err = mediaPartFromValue(part["image_url"], stringValue(part["mime"]), stringValue(part["filename"]))
					}
					if err != nil {
						return fmt.Errorf("Kimi media part: %w", err)
					}
					wire.events = append(wire.events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{media}})
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
				wire.events = append(wire.events, Event{Kind: Message, Role: "assistant", Timestamp: ts, Parts: []Part{{Kind: ToolCall, ID: stringValue(event["uuid"]), CallID: stringValue(event["toolCallId"]), ToolName: stringValue(event["name"]), Data: jsonObject(input)}}})
			case "tool.result":
				result, ok := event["result"].(map[string]any)
				if !ok {
					return fmt.Errorf("tool.result has no result")
				}
				parts, err := kimiOutputParts(result["output"], stringValue(event["toolCallId"]))
				if err != nil {
					return err
				}
				isError, _ := result["isError"].(bool)
				for i := range parts {
					parts[i].Error = isError
				}
				wire.events = append(wire.events, Event{Kind: Message, Role: "tool", Timestamp: ts, Parts: parts})
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
			wire.events = append(wire.events, Event{Kind: Plan, Role: "assistant", Timestamp: ts, PlanText: string(plan)})
		case "context.apply_compaction", "full_compaction.begin", "full_compaction.complete":
			wire.warnings = appendUnique(wire.warnings, "source contains Kimi compaction records; original observable wire events were retained")
		case "task.started", "task.terminated":
			// Lifecycle only for the main thread, but the record carries the
			// metadata that links a subagent directory to the call that spawned it.
			info, ok := record["info"].(map[string]any)
			if !ok {
				return fmt.Errorf("%s has no info object", kind)
			}
			raw, err := json.Marshal(info)
			if err != nil {
				return err
			}
			var task kimiTask
			if err := json.Unmarshal(raw, &task); err != nil {
				return fmt.Errorf("parse Kimi %s info: %w", kind, err)
			}
			if task.Kind == "agent" && task.AgentID != "" {
				wire.tasks = append(wire.tasks, task)
			}
		case "config.update", "goal.clear", "goal.create", "goal.update", "interaction.request", "interaction.resolved", "interruptionReminder.recorded", "llm.request", "llm.tools_snapshot", "permission.record_approval_result", "permission.set_mode", "plan_mode.enter", "plan_mode.exit", "plugin.session_start", "prompt.accepted", "runtime.set_binding", "staleGuard.recorded", "swarm_mode.enter", "swarm_mode.exit", "token_counting.measured", "token_counting.turn_recorded", "tools.set_active_tools", "tools.update_store", "turn.cancel", "turn.ended", "turn.prompt", "turn.steer", "usage.record":
			// UI, permission, usage and profile records do not add model
			// conversation content.
		default:
			return fmt.Errorf("unsupported Kimi wire record %q", kind)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
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

// kimiAgentNode is one entry of the agent tree Kimi keeps in state.json.
type kimiAgentNode struct {
	Type          string         `json:"type"`
	ParentAgentID string         `json:"parentAgentId"`
	Labels        map[string]any `json:"labels"`
}

// readKimiBranches reads every subagent directory beside the main thread.
// mainTasks seeds the lifecycle metadata; each subagent's own log is parsed
// too, both for its events and because a nested delegation is only recorded in
// the log of the agent that made it.
func readKimiBranches(agentsDir string, agents map[string]kimiAgentNode, mainTasks []kimiTask, mainEvents []Event) ([]Branch, []string, error) {
	entries, err := os.ReadDir(agentsDir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	type parsed struct {
		name string
		wire *kimiWire
	}
	var wires []parsed
	tasks := make(map[string]kimiTask)
	recordTasks := func(list []kimiTask) {
		for _, task := range list {
			// A terminated record supersedes the started one it repeats.
			if existing, ok := tasks[task.AgentID]; ok && task.EndedAt == nil && existing.EndedAt != nil {
				continue
			}
			tasks[task.AgentID] = task
		}
	}
	recordTasks(mainTasks)
	var warnings []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == "main" {
			continue
		}
		if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
			return nil, nil, fmt.Errorf("unsafe Kimi agent directory %q", name)
		}
		home := filepath.Join(agentsDir, name)
		wire, err := parseKimiWire(filepath.Join(home, "wire.jsonl"), home)
		if os.IsNotExist(err) {
			warnings = appendUnique(warnings, fmt.Sprintf("Kimi subagent %s has no wire log; it was skipped", name))
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read Kimi subagent %s: %w", name, err)
		}
		recordTasks(wire.tasks)
		wires = append(wires, parsed{name: name, wire: wire})
	}
	// Kimi also persists each task as a file under the agent that spawned it,
	// and that copy holds the final state, so it supersedes the wire records.
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		fileTasks, err := readKimiTaskFiles(filepath.Join(agentsDir, entry.Name(), "tasks"))
		if err != nil {
			return nil, nil, err
		}
		for _, task := range fileTasks {
			tasks[task.AgentID] = task
		}
	}
	sort.SliceStable(wires, func(i, j int) bool { return naturalLess(wires[i].name, wires[j].name) })
	var branches []Branch
	swarmItems := make(map[string]string)
	for _, item := range wires {
		if len(item.wire.events) == 0 {
			// Kimi creates the directory and binds a profile before the first
			// turn, so an agent that never ran leaves a log with no content.
			warnings = appendUnique(warnings, fmt.Sprintf("Kimi subagent %s recorded no messages (it never started or failed immediately); it was skipped", item.name))
			continue
		}
		task := tasks[item.name]
		node := agents[item.name]
		parent := node.ParentAgentID
		if parent == "main" {
			parent = ""
		}
		branch := Branch{
			ID: item.name, ParentID: parent, CallID: task.ParentCallID,
			Name: task.SubagentType, Description: task.Description, Status: task.Status,
			Model: task.Model, CreatedAt: parseFlexibleTime(task.StartedAt),
			UpdatedAt: parseFlexibleTime(task.EndedAt), Events: item.wire.events,
		}
		if branch.Model == "" {
			branch.Model = item.wire.model
		}
		// A swarm member is labelled by its item rather than by a task record.
		if label := stringValue(node.Labels["swarmItem"]); label != "" {
			swarmItems[branch.ID] = label
			if branch.Description == "" {
				branch.Description = "swarm item " + label
			}
		}
		if branch.Status == "" {
			branch.Status = "unknown"
		}
		for _, warning := range item.wire.warnings {
			warnings = appendUnique(warnings, warning)
		}
		branches = append(branches, branch)
	}
	streams := make([][]Event, 0, len(branches)+1)
	streams = append(streams, mainEvents)
	for _, branch := range branches {
		streams = append(streams, branch.Events)
	}
	attachKimiBranches(branches, streams, swarmItems)
	linkBranchParents(branches)
	var unattached []string
	for _, branch := range branches {
		if branch.CallID == "" {
			unattached = append(unattached, branch.ID)
		}
	}
	if len(unattached) > 0 {
		warnings = appendUnique(warnings, fmt.Sprintf("Kimi recorded no spawning tool call for subagent %s; %s moved as unattached branches", strings.Join(unattached, ", "), plural(len(unattached), "it", "they")))
	}
	return branches, warnings, nil
}

// attachKimiBranches links a branch to the call that launched it when Kimi
// recorded no parentToolCallId for it. Kimi writes a task record only for a
// detached run, and never for a swarm member, so two other signals are used:
// the delegating tool's own result names the agent ids it started, and a swarm
// member carries the item label that appears in the AgentSwarm call's items
// argument. A signal claimed by more than one call is ambiguous and is left
// alone, so a branch is only ever linked to the one call that can own it.
//
// streams must include the main thread and every branch, because a nested
// delegation is recorded in the branch that made it.
func attachKimiBranches(branches []Branch, streams [][]Event, swarmItems map[string]string) {
	delegating := make(map[string]bool)
	described := make(map[string]string)
	owners := make(map[string][]string)
	claim := func(key, callID string) {
		if key != "" {
			owners[key] = appendUnique(owners[key], callID)
		}
	}
	for _, events := range streams {
		for _, event := range events {
			for _, part := range event.Parts {
				if part.Kind != ToolCall || (part.ToolName != "Agent" && part.ToolName != "AgentSwarm") {
					continue
				}
				delegating[part.CallID] = true
				var args struct {
					Items       []string `json:"items"`
					Description string   `json:"description"`
				}
				if json.Unmarshal(part.Data, &args) == nil {
					described[part.CallID] = args.Description
					for _, item := range args.Items {
						claim(item, part.CallID)
					}
				}
			}
		}
	}
	for _, events := range streams {
		for _, event := range events {
			for _, part := range event.Parts {
				if part.Kind != ToolResult || !delegating[part.CallID] {
					continue
				}
				for _, id := range kimiResultAgentIDs(part.Text) {
					claim(id, part.CallID)
				}
			}
		}
	}
	for i := range branches {
		if branches[i].CallID != "" {
			continue
		}
		for _, key := range []string{branches[i].ID, swarmItems[branches[i].ID]} {
			if callIDs := owners[key]; len(callIDs) == 1 {
				branches[i].CallID = callIDs[0]
				break
			}
		}
	}
	// Without a task record there is no description either, but the delegating
	// call carried one; a swarm's shared call describes the swarm, not a member.
	for i := range branches {
		if branches[i].Description == "" && swarmItems[branches[i].ID] == "" {
			branches[i].Description = described[branches[i].CallID]
		}
	}
}

// kimiResultAgentIDs pulls the subagent ids out of a delegating tool's result.
// The Agent tool prints `agent_id: <id>` on its own line; AgentSwarm reports
// one `agent_id="<id>"` attribute per member.
func kimiResultAgentIDs(text string) []string {
	if !strings.Contains(text, "agent_id") {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "agent_id:")
		if !ok {
			continue
		}
		if id := strings.TrimSpace(value); id != "" {
			out = appendUnique(out, id)
		}
	}
	const attribute = `agent_id="`
	for rest := text; ; {
		index := strings.Index(rest, attribute)
		if index < 0 {
			return out
		}
		rest = rest[index+len(attribute):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return out
		}
		if id := strings.TrimSpace(rest[:end]); id != "" {
			out = appendUnique(out, id)
		}
		rest = rest[end+1:]
	}
}

// readKimiTaskFiles reads the delegated-run records one agent recorded. Shell
// tasks live here too and are skipped; only an agent task describes a branch.
func readKimiTaskFiles(dir string) ([]kimiTask, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []kimiTask
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var task kimiTask
		if err := json.Unmarshal(b, &task); err != nil {
			return nil, fmt.Errorf("parse Kimi task %s: %w", entry.Name(), err)
		}
		if task.Kind == "agent" && task.AgentID != "" {
			out = append(out, task)
		}
	}
	return out, nil
}

func kimiOutputParts(output any, callID string) ([]Part, error) {
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
	if blocks, ok := output.([]any); ok {
		var parts []Part
		for _, block := range blocks {
			obj, ok := block.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("Kimi tool output contains a non-object block")
			}
			if stringValue(obj["type"]) == "image" || stringValue(obj["type"]) == "input_image" {
				media, err := kimiOutputParts(obj, callID)
				if err != nil {
					return nil, err
				}
				parts = append(parts, media...)
			} else {
				parts = append(parts, Part{Kind: ToolResult, CallID: callID, Text: stringValue(obj["text"])})
			}
		}
		if len(parts) > 0 {
			return parts, nil
		}
	}
	text, err := stringifyOutput(output)
	if err != nil {
		return nil, err
	}
	return []Part{{Kind: ToolResult, CallID: callID, Text: text}}, nil
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
		case "_system_prompt", "_checkpoint", "_usage":
			// Python-era Kimi materializes its runtime prompt and bookkeeping
			// records in context.jsonl when a native session is resumed. They
			// are not conversation messages and cannot be reused by a different
			// harness. Keep unknown roles fail-closed below.
			return nil
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
					case "image", "input_image":
						media, err := mediaPartFromValue(block["url"], stringValue(block["mime"]), stringValue(block["filename"]))
						if err != nil {
							media, err = mediaPartFromValue(block["image_url"], stringValue(block["mime"]), stringValue(block["filename"]))
						}
						if err != nil {
							return fmt.Errorf("legacy Kimi media block: %w", err)
						}
						event.Parts = append(event.Parts, media)
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
			parts, err := kimiOutputParts(message["content"], stringValue(message["tool_call_id"]))
			if err != nil {
				return err
			}
			history.Events = append(history.Events, Event{Kind: Message, Role: "tool", Parts: parts})
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

// writeKimiBranches gives every delegated run its own agent directory, the way
// Kimi records one natively: a wire log beside the main thread, an entry in the
// session's agent tree, and a task record under the agent that spawned it so
// the run stays linked to its call. Destination agent ids are renumbered from
// zero because they are positional in Kimi and the source's numbering may not
// survive skipped branches; the source id is kept in the task record.
func writeKimiBranches(branches []Branch, agents map[string]any, stage, final string, now time.Time) (files []string, warnings []string, err error) {
	if len(branches) == 0 {
		return nil, nil, nil
	}
	destination := make(map[string]string, len(branches))
	for i, branch := range branches {
		destination[branch.ID] = fmt.Sprintf("agent-%d", i)
	}
	var unfinished []string
	for _, branch := range branches {
		name := destination[branch.ID]
		parent := "main"
		if name, ok := destination[branch.ParentID]; ok {
			parent = name
		}
		stageAgent := filepath.Join(stage, "agents", name)
		if err := ensureDir(stageAgent); err != nil {
			return nil, nil, err
		}
		publishedAgent := filepath.Join(final, "agents", name)
		planFiles, err := writeKimiWireLog(branch.Events, stageAgent, publishedAgent, now)
		if err != nil {
			return nil, nil, fmt.Errorf("write Kimi subagent %s: %w", branch.ID, err)
		}
		files = append(files, filepath.Join(publishedAgent, "wire.jsonl"))
		files = append(files, planFiles...)
		entry := map[string]any{"homedir": publishedAgent, "type": "sub", "parentAgentId": parent}
		labels := map[string]any{"parentAgentId": parent, "agentswapSourceAgentId": branch.ID}
		entry["labels"] = labels
		agents[name] = entry

		// Kimi polls a task it still believes is running, and nothing in the new
		// session can service that. A run that never reached a terminal state is
		// therefore recorded as failed rather than resurrected.
		status := branch.Status
		if status != "completed" && status != "failed" {
			if status != "" && status != "unknown" {
				unfinished = append(unfinished, branch.ID)
			}
			status = "failed"
		}
		taskSuffix, err := shortID("")
		if err != nil {
			return nil, nil, err
		}
		task := map[string]any{
			"taskId": "agent-" + taskSuffix, "kind": "agent", "agentId": name,
			"description": branch.Description, "status": status, "detached": false,
			"parentToolCallId": branch.CallID, "subagentType": branch.Name,
			"model": branch.Model, "agentswapSourceAgentId": branch.ID,
		}
		if !branch.CreatedAt.IsZero() {
			task["startedAt"] = branch.CreatedAt.UnixMilli()
		}
		if !branch.UpdatedAt.IsZero() {
			task["endedAt"] = branch.UpdatedAt.UnixMilli()
		}
		tasksDir := filepath.Join(stage, "agents", parent, "tasks")
		if err := ensureDir(tasksDir); err != nil {
			return nil, nil, err
		}
		if err := writeJSONFile(filepath.Join(tasksDir, stringValue(task["taskId"])+".json"), task, 0o600); err != nil {
			return nil, nil, err
		}
		files = append(files, filepath.Join(final, "agents", parent, "tasks", stringValue(task["taskId"])+".json"))
	}
	warnings = append(warnings, fmt.Sprintf("%d delegated agent %s moved into Kimi subagent %s; the transcripts are readable but a subagent cannot be resumed in the new session", len(branches), plural(len(branches), "run", "runs"), plural(len(branches), "directory", "directories")))
	if len(unfinished) > 0 {
		warnings = append(warnings, fmt.Sprintf("delegated %s %s had not finished; %s recorded as failed because the new session has no process to attach to", plural(len(unfinished), "run", "runs"), strings.Join(unfinished, ", "), plural(len(unfinished), "it was", "they were")))
	}
	return files, warnings, nil
}

// writeKimiWireLog renders one canonical event stream as a Kimi wire log for a
// single agent. Files are produced under stageAgent so an aborted write leaves
// nothing behind, but plan records must store the location those files will
// have after publication, which is what publishedAgent supplies. The returned
// paths are the published plan files.
func writeKimiWireLog(events []Event, stageAgent, publishedAgent string, now time.Time) (files []string, err error) {
	wirePath := filepath.Join(stageAgent, "wire.jsonl")
	f, err := os.OpenFile(wirePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := writeJSONLine(f, map[string]any{"type": "metadata", "protocol_version": "1.5", "created_at": now.UnixMilli()}); err != nil {
		return nil, err
	}
	callParents := make(map[string]string)
	resultIDs := make(map[string]bool)
	for _, event := range events {
		for _, part := range event.Parts {
			if part.Kind == ToolResult || (part.Kind == Media && part.CallID != "") {
				resultIDs[part.CallID] = true
			}
		}
	}
	turnID, err := newUUID()
	if err != nil {
		return nil, err
	}
	emitToolResult := func(part Part, ts int64) error {
		parent := callParents[part.CallID]
		if parent == "" {
			return fmt.Errorf("Kimi tool result %s has no emitted call", part.CallID)
		}
		output := any(part.Text)
		if part.Kind == Media {
			output = map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename}
		}
		resultValue := map[string]any{"output": output}
		if part.Error {
			resultValue["isError"] = true
		}
		loop := map[string]any{"type": "tool.result", "parentUuid": parent, "toolCallId": part.CallID, "result": resultValue}
		return writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": loop, "time": ts})
	}
	step := 0
	planIndex := 0
	for _, event := range events {
		ts := timestampOr(event.Timestamp, now).UnixMilli()
		if event.Kind == Plan {
			planIndex++
			planID := fmt.Sprintf("agentswap-import-%d", planIndex)
			content := []byte(event.PlanText)
			planPath := filepath.Join("plan", planID, "v1.md")
			full := filepath.Join(stageAgent, planPath)
			if err := ensureDir(filepath.Dir(full)); err != nil {
				return nil, err
			}
			if err := os.WriteFile(full, content, 0o600); err != nil {
				return nil, err
			}
			if err := ensureDir(filepath.Join(stageAgent, "plans")); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(stageAgent, "plans", planID+".md"), content, 0o600); err != nil {
				return nil, err
			}
			publishedPlan := filepath.Join(publishedAgent, planPath)
			recordedPath, err := filepath.Rel(kimiCodeRoot(), publishedPlan)
			if err != nil {
				return nil, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "plan.revision", "id": planID, "version": 1, "path": filepath.ToSlash(recordedPath), "sha256": hashHex(content), "bytes": len(content), "time": ts}); err != nil {
				return nil, err
			}
			files = append(files, publishedPlan)
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
				case Media:
					content = append(content, map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename})
				case ToolResult:
					if err := flushUser(); err != nil {
						return nil, err
					}
					if err := emitToolResult(part, ts); err != nil {
						return nil, err
					}
				default:
					return nil, fmt.Errorf("cannot write %s part in a Kimi user message", part.Kind)
				}
			}
			if err := flushUser(); err != nil {
				return nil, err
			}
			continue
		}
		if event.Role == "assistant" {
			stepUUID, err := newUUID()
			if err != nil {
				return nil, err
			}
			beginUUID, err := newUUID()
			if err != nil {
				return nil, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": map[string]any{"type": "step.begin", "uuid": beginUUID, "turnId": turnID, "step": step}, "time": ts}); err != nil {
				return nil, err
			}
			var dangling []Part
			for _, part := range event.Parts {
				eventUUID, err := newUUID()
				if err != nil {
					return nil, err
				}
				var loop map[string]any
				switch part.Kind {
				case Text:
					loop = map[string]any{"type": "content.part", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "part": map[string]any{"type": "text", "text": part.Text}}
				case Reasoning:
					loop = map[string]any{"type": "content.part", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "part": map[string]any{"type": "think", "think": part.Text}}
				case Media:
					loop = map[string]any{"type": "content.part", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "part": map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename}}
				case ToolCall:
					var args map[string]any
					if err := json.Unmarshal(jsonObject(part.Data), &args); err != nil {
						return nil, err
					}
					loop = map[string]any{"type": "tool.call", "uuid": eventUUID, "turnId": turnID, "step": step, "stepUuid": stepUUID, "toolCallId": part.CallID, "name": part.ToolName, "args": args}
					callParents[part.CallID] = eventUUID
					if !resultIDs[part.CallID] {
						dangling = append(dangling, Part{Kind: ToolResult, CallID: part.CallID, Text: "[Tool execution was interrupted before teleport]", Error: true})
					}
				case ToolResult:
					return nil, fmt.Errorf("tool result appeared in an assistant event")
				}
				if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": loop, "time": ts}); err != nil {
					return nil, err
				}
			}
			endUUID, err := newUUID()
			if err != nil {
				return nil, err
			}
			if err := writeJSONLine(f, map[string]any{"type": "context.append_loop_event", "event": map[string]any{"type": "step.end", "uuid": endUUID, "turnId": turnID, "step": step}, "time": ts}); err != nil {
				return nil, err
			}
			step++
			for _, part := range dangling {
				if err := emitToolResult(part, ts); err != nil {
					return nil, err
				}
			}
			continue
		}
		if event.Role == "tool" {
			for _, part := range event.Parts {
				if part.Kind != ToolResult && part.Kind != Media {
					return nil, fmt.Errorf("non-result part appeared in a tool event")
				}
				if err := emitToolResult(part, ts); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return files, f.Close()
}

func writeKimiCode(history *Session, opts WriteOptions) (result Result, err error) {
	resumeModel, err := kimiResumeModel()
	if err != nil {
		return Result{}, err
	}
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
	result = Result{Agent: Kimi, ID: id, Path: final, Resume: []string{"kimi", "--session", id, "--model", resumeModel}, Files: []string{filepath.Join(final, "state.json"), filepath.Join(agentHome, "wire.jsonl")}}
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
	agents := map[string]any{"main": map[string]any{"homedir": filepath.Join(final, "agents", "main"), "type": "main", "parentAgentId": nil}}
	state := map[string]any{
		"createdAt": now.UTC().Format(time.RFC3339Nano), "updatedAt": now.UTC().Format(time.RFC3339Nano),
		"agents": agents,
		"custom": map[string]any{"agentswap": map[string]any{
			"sourceAgent": history.Source, "sourceSessionID": history.SourceID, "sourceModel": history.Model,
			"sourceCreatedAt": history.CreatedAt, "sourceUpdatedAt": history.UpdatedAt,
		}},
		"lastPrompt": lastUserText(history), "title": safeTitle(history.Title, firstText(history)),
		"workDir": canonical, "isCustomTitle": true,
	}
	planFiles, err := writeKimiWireLog(history.Events, stageAgent, agentHome, now)
	if err != nil {
		return Result{}, err
	}
	result.Files = append(result.Files, planFiles...)
	branchFiles, branchWarnings, err := writeKimiBranches(history.Branches, agents, stage, final, now)
	if err != nil {
		return Result{}, err
	}
	result.Files = append(result.Files, branchFiles...)
	result.Warnings = append(result.Warnings, branchWarnings...)
	if err := writeJSONFile(filepath.Join(stage, "state.json"), state, 0o600); err != nil {
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
	if len(history.Branches) > 0 {
		result.Warnings = append(result.Warnings, branchesNotTransferred("The Python-era Kimi layout", history.Branches))
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
			if part.Kind == ToolResult || (part.Kind == Media && part.CallID != "") {
				resultIDs[part.CallID] = true
			}
		}
	}
	emitToolResult := func(part Part, ts time.Time) error {
		content := any(part.Text)
		if part.Kind == Media {
			content = []any{map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename}}
		}
		if err := writeJSONLine(contextFile, map[string]any{"role": "tool", "content": content, "tool_call_id": part.CallID}); err != nil {
			return err
		}
		payload := map[string]any{
			"tool_call_id": part.CallID,
			"return_value": map[string]any{"is_error": part.Error, "output": content, "message": "", "display": []any{}},
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
				case Media:
					blocks = append(blocks, map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename})
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
				case Media:
					block := map[string]any{"type": "image", "url": mediaDataURL(part), "mime": part.MediaType, "filename": part.Filename}
					content = append(content, block)
					if err := wire(event.Timestamp, "ImagePart", block); err != nil {
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
				if part.Kind != ToolResult && part.Kind != Media {
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
