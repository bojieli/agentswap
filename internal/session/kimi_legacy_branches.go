package session

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func writeKimiLegacy(history *Session, opts WriteOptions) (Result, error) {
	branches, err := orderedBranches(history.Branches)
	if err != nil {
		return Result{}, err
	}
	id, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	cwd, err := canonicalPath(opts.CWD)
	if err != nil {
		return Result{}, err
	}
	hash := md5.Sum([]byte(cwd))
	final := filepath.Join(kimiLegacyRoot(), "sessions", hex.EncodeToString(hash[:]), id)
	ids := map[string]string{}
	for i, b := range branches {
		ids[b.ID] = fmt.Sprintf("agent-%d", i)
	}
	refs := branchReferenceIDs(history.Source, branches, ids)
	copied := *history
	copied.Events = remapDelegation(history.Events, refs)
	copied.Branches = append([]Branch(nil), branches...)
	for i := range copied.Branches {
		b := &copied.Branches[i]
		b.Events = remapDelegation(b.Events, refs)
		b.ID = ids[b.ID]
		if b.ParentID != "" {
			b.ParentID = ids[b.ParentID]
		}
	}
	r, err := writeKimiLegacyAt(&copied, opts, final, id, true)
	if err == nil && len(branches) > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d delegated agent runs moved into Kimi Python subagent directories; unfinished runs were marked failed", len(branches)))
	}
	return r, err
}

func writeKimiLegacyChildren(history *Session, opts WriteOptions, stage, final string) ([]string, error) {
	var files []string
	for _, b := range history.Branches {
		subDir := filepath.Join(stage, "subagents", b.ID)
		result, err := writeKimiLegacyAt(branchHistory(history, b), opts, subDir, b.ID, false)
		if err != nil {
			return nil, err
		}
		for _, path := range result.Files {
			rel, err := filepath.Rel(stage, path)
			if err != nil {
				return nil, err
			}
			files = append(files, filepath.Join(final, rel))
		}
		status := b.Status
		if status != "completed" && status != "failed" && status != "killed" {
			status = "failed"
		}
		now := time.Now()
		created := timestampOr(b.CreatedAt, now)
		updated := timestampOr(b.UpdatedAt, now)
		launch := map[string]any{"agent_id": b.ID, "subagent_type": b.Name, "model_override": nil, "effective_model": nullableString(b.Model), "created_at": float64(created.UnixMilli()) / 1000}
		meta := map[string]any{"agent_id": b.ID, "subagent_type": b.Name, "status": status, "description": b.Description,
			"created_at": float64(created.UnixMilli()) / 1000, "updated_at": float64(updated.UnixMilli()) / 1000, "last_task_id": nil, "launch_spec": launch,
			"parent_tool_call_id": b.CallID, "parent_agent_id": b.ParentID, "agentswap_cwd": b.CWD}
		if err := writeJSONFile(filepath.Join(subDir, "meta.json"), meta, 0o600); err != nil {
			return nil, err
		}
		for _, name := range []string{"prompt.txt", "output"} {
			if err := os.WriteFile(filepath.Join(subDir, name), nil, 0o600); err != nil {
				return nil, err
			}
			files = append(files, filepath.Join(final, "subagents", b.ID, name))
		}
		files = append(files, filepath.Join(final, "subagents", b.ID, "meta.json"))
	}
	return files, nil
}

func readKimiLegacy(candidate Candidate) (*Session, error) {
	history, err := readKimiLegacyThread(candidate)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(candidate.Path, "subagents")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return history, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		var meta map[string]any
		data, err := os.ReadFile(filepath.Join(path, "meta.json"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, err
			}
		}
		part, err := readKimiLegacyThread(Candidate{Agent: Kimi, ID: entry.Name(), Path: path, CWD: candidate.CWD})
		if err != nil {
			return nil, fmt.Errorf("read Kimi Python child %s: %w", entry.Name(), err)
		}
		if len(part.Events) == 0 {
			history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("Kimi Python child %s recorded no messages", entry.Name()))
			continue
		}
		launch, _ := meta["launch_spec"].(map[string]any)
		b := Branch{ID: entry.Name(), CWD: stringValue(meta["agentswap_cwd"]), ParentID: stringValue(meta["parent_agent_id"]), CallID: stringValue(meta["parent_tool_call_id"]),
			Name: stringValue(meta["subagent_type"]), Description: stringValue(meta["description"]), Status: stringValue(meta["status"]),
			Model: stringValue(launch["effective_model"]), CreatedAt: parseFlexibleTime(meta["created_at"]), UpdatedAt: parseFlexibleTime(meta["updated_at"]), Events: part.Events}
		history.Branches = append(history.Branches, b)
		history.Warnings = append(history.Warnings, part.Warnings...)
	}
	// Python releases without parent_tool_call_id can still be linked from the
	// recorded Agent result, which contains agent_id.
	streams := append([][]Event{history.Events}, branchEventStreams(history.Branches)...)
	for i := range history.Branches {
		b := &history.Branches[i]
		if b.CallID != "" {
			continue
		}
		for si, events := range streams {
			links, _ := codexSpawnLinks(events, "")
			if link, ok := links[b.ID]; ok {
				b.CallID = link.CallID
				if si > 0 {
					b.ParentID = history.Branches[si-1].ID
				}
			}
		}
		if b.CallID == "" {
			history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("Kimi Python child %s has no recorded spawning call; it moved unattached", b.ID))
		}
	}
	return history, nil
}

func branchEventStreams(branches []Branch) [][]Event {
	out := make([][]Event, len(branches))
	for i, b := range branches {
		out[i] = b.Events
	}
	return out
}
