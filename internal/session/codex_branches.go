package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (codexAdapter) Read(ctx context.Context, candidate Candidate) (*Session, error) {
	history, err := readCodexThread(candidate)
	if err != nil {
		return nil, err
	}
	type thread struct {
		meta codexMeta
		path string
	}
	children := map[string][]thread{}
	paths := map[string]string{candidate.ID: candidate.Path}
	err = filepath.WalkDir(filepath.Join(codexRoot(), "sessions"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".agentswap-") {
			return filepath.SkipDir
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		meta, err := readCodexMeta(path)
		if err != nil {
			return nil
		} // Unrelated malformed rollouts do not strand this session.
		if meta.ParentID != "" {
			children[meta.ParentID] = append(children[meta.ParentID], thread{meta, path})
			paths[meta.ID] = path
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{candidate.ID: true}
	var load func(string, string, []Event) error
	load = func(nativeParent, parent string, events []Event) error {
		links, err := codexSpawnLinks(events, paths[nativeParent])
		if err != nil {
			return err
		}
		for _, child := range children[nativeParent] {
			if seen[child.meta.ID] {
				return fmt.Errorf("duplicate or cyclic Codex child thread %s", child.meta.ID)
			}
			seen[child.meta.ID] = true
			part, err := readCodexThread(Candidate{Agent: Codex, ID: child.meta.ID, CWD: child.meta.CWD, Path: child.path})
			if err != nil {
				return fmt.Errorf("read Codex child %s: %w", child.meta.ID, err)
			}
			if len(part.Events) == 0 {
				history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("Codex child %s recorded no messages; it was skipped", child.meta.ID))
				continue
			}
			b := Branch{ID: child.meta.ID, CWD: child.meta.CWD, ParentID: parent, Name: child.meta.Role, Description: child.meta.Nickname,
				Model: part.Model, CreatedAt: part.CreatedAt, UpdatedAt: part.UpdatedAt, Status: "unknown", Events: part.Events}
			if child.meta.Branch != nil {
				b = *child.meta.Branch
				b.ID = child.meta.ID
				b.ParentID = parent
				b.CWD = child.meta.CWD
				b.Events = part.Events
			}
			if link, ok := links[child.meta.ID]; ok {
				b.CallID = link.CallID
				if b.Description == "" {
					b.Description = link.Description
				}
				if b.Status == "unknown" {
					b.Status = link.Status
				}
			}
			if b.CallID == "" {
				history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("Codex child %s has no recorded spawning call; it moved unattached", b.ID))
			}
			history.Branches = append(history.Branches, b)
			history.Warnings = append(history.Warnings, part.Warnings...)
			if err := load(child.meta.ID, b.ID, b.Events); err != nil {
				return err
			}
		}
		for id := range links {
			if !seen[id] {
				history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("Codex delegated thread %s has no local child rollout", id))
			}
		}
		return nil
	}
	if err := load(candidate.ID, "", history.Events); err != nil {
		return nil, err
	}
	return history, nil
}

func codexSpawnLinks(events []Event, path string) (map[string]Branch, error) {
	links := map[string]Branch{}
	delegation := map[string]Part{}
	for _, e := range events {
		for _, p := range e.Parts {
			if p.Kind == ToolCall && isDelegation(p.ToolName) {
				delegation[p.CallID] = p
			}
			if p.Kind == ToolResult {
				if call, ok := delegation[p.CallID]; ok {
					var result map[string]any
					if json.Unmarshal([]byte(p.Text), &result) == nil {
						id := stringValue(result["agent_id"])
						if id == "" {
							id = stringValue(result["thread_id"])
						}
						if id != "" {
							var args map[string]any
							_ = json.Unmarshal(call.Data, &args)
							links[id] = Branch{CallID: p.CallID, Description: stringValue(args["message"]), Status: "unknown"}
						}
					}
				}
			}
		}
	}
	if path == "" {
		return links, nil
	}
	err := readJSONL(path, func(_ int, raw json.RawMessage) error {
		var r struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Type == "event_msg" && stringValue(r.Payload["type"]) == "collab_agent_spawn_end" {
			id := stringValue(r.Payload["new_thread_id"])
			if id != "" {
				status := stringValue(r.Payload["status"])
				if obj, ok := r.Payload["status"].(map[string]any); ok {
					for k := range obj {
						status = k
					}
				}
				links[id] = Branch{CallID: stringValue(r.Payload["call_id"]), Description: stringValue(r.Payload["prompt"]), Status: status}
			}
		}
		return nil
	})
	return links, err
}

func (codexAdapter) Write(ctx context.Context, history *Session, opts WriteOptions) (Result, error) {
	branches, err := orderedBranches(history.Branches)
	if err != nil {
		return Result{}, err
	}
	now := time.Now().UTC()
	rootID, err := newUUID7(now)
	if err != nil {
		return Result{}, err
	}
	ids := map[string]string{"": rootID}
	depth := map[string]int{}
	for _, b := range branches {
		id, err := newUUID7(now)
		if err != nil {
			return Result{}, err
		}
		ids[b.ID] = id
		depth[b.ID] = depth[b.ParentID] + 1
	}
	dir := filepath.Join(codexRoot(), "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	stage := dir
	if !opts.DryRun {
		if err := ensureDir(dir); err != nil {
			return Result{}, err
		}
		stage, err = os.MkdirTemp(dir, ".agentswap-tree-")
		if err != nil {
			return Result{}, err
		}
		defer removeIfExists(stage)
	}
	refs := branchReferenceIDs(history.Source, branches, ids)
	main := *history
	main.Events = remapDelegation(history.Events, refs)
	spawnEvents := map[string][]map[string]any{}
	for _, b := range branches {
		if b.CallID == "" {
			continue
		}
		spawnEvents[b.ParentID] = append(spawnEvents[b.ParentID], map[string]any{
			"type": "collab_agent_spawn_end", "call_id": b.CallID, "sender_thread_id": ids[b.ParentID],
			"new_thread_id": ids[b.ID], "new_agent_role": b.Name, "new_agent_nickname": b.Description,
			"prompt": b.Description, "model": b.Model, "reasoning_effort": "medium",
			"status": transferredCodexStatus(b),
		})
	}
	root, err := writeCodexThread(&main, opts, rootID, stage, map[string]any{"agentswap_spawn_events": spawnEvents[""]})
	if err != nil {
		return Result{}, err
	}
	root.Path = filepath.Join(dir, filepath.Base(root.Path))
	root.Files = []string{root.Path}
	var staged []string
	// Publish the root last: discovery cannot see an incomplete tree.
	for _, b := range branches {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		sub := branchHistory(history, b)
		sub.Events = remapDelegation(b.Events, refs)
		meta := branchMetadata(b)
		meta.ID = ids[b.ID]
		meta.ParentID = ids[b.ParentID]
		if b.ParentID == "" {
			meta.ParentID = ""
		}
		extra := map[string]any{"agentswap_spawn_events": spawnEvents[b.ID], "thread_source": "subagent", "session_id": rootID, "parent_thread_id": ids[b.ParentID], "agent_role": b.Name, "agent_nickname": b.Description, "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{
			"parent_thread_id": ids[b.ParentID], "depth": depth[b.ID], "agent_role": b.Name, "agent_nickname": b.Description}}}, "agentswap_branch": meta}
		childOpts := opts
		if b.CWD != "" {
			childOpts.CWD = b.CWD
		}
		r, err := writeCodexThread(sub, childOpts, ids[b.ID], stage, extra)
		if err != nil {
			return Result{}, err
		}
		staged = append(staged, r.Path)
		root.Files = append(root.Files, filepath.Join(dir, filepath.Base(r.Path)))
		for _, warning := range r.Warnings {
			root.Warnings = appendUnique(root.Warnings, warning)
		}
	}
	if len(branches) > 0 {
		root.Warnings = appendUnique(root.Warnings, fmt.Sprintf("%d delegated agent runs moved as Codex child rollouts; live execution was not transferred", len(branches)))
	}
	if opts.DryRun {
		return root, nil
	}
	staged = append(staged, filepath.Join(stage, filepath.Base(root.Path)))
	var published []string
	for _, path := range staged {
		final := filepath.Join(dir, filepath.Base(path))
		if err := os.Rename(path, final); err != nil {
			for _, p := range published {
				removeIfExists(p)
			}
			return Result{}, err
		}
		published = append(published, final)
	}
	return root, nil
}

func transferredCodexStatus(b Branch) any {
	switch b.Status {
	case "completed":
		for i := len(b.Events) - 1; i >= 0; i-- {
			if b.Events[i].Role == "assistant" {
				for _, p := range b.Events[i].Parts {
					if p.Kind == Text {
						return map[string]any{"completed": p.Text}
					}
				}
			}
		}
		return map[string]any{"completed": nil}
	case "failed", "errored":
		return map[string]any{"errored": "Imported failed run; no live process was transferred"}
	case "killed", "shutdown":
		return "shutdown"
	default:
		return "interrupted"
	}
}
