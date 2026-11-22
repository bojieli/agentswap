package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func openCodeTreePath(id string) string {
	root := envDir("AGENTSWAP_HOME", filepath.Join(homeDir(), ".agentswap"))
	// The session ID is untrusted when reading an export.
	return filepath.Join(root, "session-trees", "opencode", hash12(id)+".json")
}

type openCodeTree struct {
	Version  int      `json:"version"`
	ID       string   `json:"session_id"`
	CWD      string   `json:"cwd"`
	Branches []Branch `json:"branches"`
}

func (openCodeAdapter) Read(ctx context.Context, candidate Candidate) (*Session, error) {
	history, raw, err := readOpenCodeThread(ctx, candidate, false)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{candidate.ID: true}
	var archived openCodeTree
	if b, err := os.ReadFile(openCodeTreePath(candidate.ID)); err == nil {
		if err := json.Unmarshal(b, &archived); err != nil {
			return nil, fmt.Errorf("parse OpenCode tree manifest: %w", err)
		}
		if archived.Version != 1 || archived.ID != candidate.ID || !samePath(archived.CWD, candidate.CWD) {
			return nil, fmt.Errorf("OpenCode tree manifest does not match the session")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var load func(map[string]any, string) error
	load = func(export map[string]any, parent string) error {
		links := openCodeChildLinks(export)
		for _, b := range archived.Branches {
			if b.ParentID == parent {
				if _, exists := links[b.ID]; !exists {
					links[b.ID] = b
				}
			}
		}
		// Map iteration must not decide the ordering of transferred branches.
		ordered := make([]Branch, 0, len(links))
		for _, b := range links {
			ordered = append(ordered, b)
		}
		sortBranchesByID(ordered)
		for _, b := range ordered {
			if seen[b.ID] {
				continue
			} // An agent can be invoked repeatedly.
			if len(seen) >= 4096 {
				return fmt.Errorf("OpenCode tree exceeds 4096 sessions")
			}
			seen[b.ID] = true
			part, childRaw, err := readOpenCodeThread(ctx, Candidate{Agent: OpenCode, ID: b.ID, CWD: candidate.CWD}, true)
			if err != nil {
				history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("OpenCode child %s could not be exported: %v", b.ID, err))
				continue
			}
			info, _ := childRaw["info"].(map[string]any)
			wantParent := candidate.ID
			if parent != "" {
				wantParent = parent
			}
			if declared := stringValue(info["parentID"]); declared != "" && declared != wantParent {
				return fmt.Errorf("OpenCode child %s declares a different parent %s", b.ID, declared)
			}
			b.ParentID = parent
			b.CWD = part.CWD
			b.Events = part.Events
			if b.Model == "" {
				b.Model = part.Model
			}
			if b.CreatedAt.IsZero() {
				b.CreatedAt = part.CreatedAt
			}
			if b.UpdatedAt.IsZero() {
				b.UpdatedAt = part.UpdatedAt
			}
			if len(b.Events) > 0 {
				history.Branches = append(history.Branches, b)
			} else {
				history.Warnings = appendUnique(history.Warnings, fmt.Sprintf("OpenCode child %s recorded no messages", b.ID))
			}
			history.Warnings = append(history.Warnings, part.Warnings...)
			if err := load(childRaw, b.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := load(raw, ""); err != nil {
		return nil, err
	}
	return history, nil
}

func openCodeChildLinks(export map[string]any) map[string]Branch {
	links := map[string]Branch{}
	messages, _ := export["messages"].([]any)
	for _, msg := range messages {
		m, _ := msg.(map[string]any)
		parts, _ := m["parts"].([]any)
		for _, raw := range parts {
			p, _ := raw.(map[string]any)
			state, _ := p["state"].(map[string]any)
			meta, _ := state["metadata"].(map[string]any)
			callID := stringValue(p["callID"])
			if values, ok := meta["agentswapBranches"]; ok {
				b, _ := json.Marshal(values)
				var branches []Branch
				if json.Unmarshal(b, &branches) == nil {
					for _, branch := range branches {
						links[branch.ID] = branch
					}
				}
			}
			id := stringValue(meta["sessionId"])
			if id == "" {
				id = stringValue(meta["sessionID"])
			}
			if id == "" && strings.EqualFold(stringValue(p["tool"]), "task") {
				// Older task versions return session_id in their textual result.
				output := stringValue(state["output"])
				for _, line := range strings.Split(output, "\n") {
					if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "session_id" {
						id = strings.TrimSpace(value)
					}
				}
			}
			if id != "" {
				if _, exists := links[id]; !exists {
					input, _ := state["input"].(map[string]any)
					links[id] = Branch{ID: id, CallID: callID, Name: stringValue(input["subagent_type"]), Description: stringValue(input["description"]), Status: stringValue(state["status"])}
				}
			}
		}
	}
	return links
}

func (openCodeAdapter) Write(ctx context.Context, history *Session, opts WriteOptions) (result Result, err error) {
	branches, err := orderedBranches(history.Branches)
	if err != nil {
		return Result{}, err
	}
	id, err := shortID("ses_")
	if err != nil {
		return Result{}, err
	}
	ids := map[string]string{"": id}
	for _, b := range branches {
		subID, err := shortID("ses_")
		if err != nil {
			return Result{}, err
		}
		ids[b.ID] = subID
	}
	links := map[string]map[string][]Branch{}
	var metadata []Branch
	for _, b := range branches {
		meta := branchMetadata(b)
		meta.ID = ids[b.ID]
		if b.ParentID != "" {
			meta.ParentID = ids[b.ParentID]
		}
		metadata = append(metadata, meta)
		if links[b.ParentID] == nil {
			links[b.ParentID] = map[string][]Branch{}
		}
		links[b.ParentID][b.CallID] = append(links[b.ParentID][b.CallID], meta)
	}
	var imported []string
	manifest := openCodeTreePath(id)
	committed := false
	defer func() {
		if !committed && !opts.DryRun {
			for i := len(imported) - 1; i >= 0; i-- {
				rollbackOpenCode(opts.CWD, imported[i])
			}
			if len(branches) > 0 {
				removeIfExists(manifest)
			}
		}
	}()
	refs := branchReferenceIDs(history.Source, branches, ids)
	main := *history
	main.Events = remapDelegation(main.Events, refs)
	result, err = writeOpenCodeThread(ctx, &main, opts, id, "", links[""])
	if err != nil {
		return Result{}, err
	}
	if !opts.DryRun {
		imported = append(imported, id)
	}
	for _, b := range branches {
		sub := branchHistory(history, b)
		sub.Events = remapDelegation(sub.Events, refs)
		childOpts := opts
		if b.CWD != "" {
			childOpts.CWD = b.CWD
		}
		r, err := writeOpenCodeThread(ctx, sub, childOpts, ids[b.ID], ids[b.ParentID], links[b.ID])
		if err != nil {
			return Result{}, err
		}
		if !opts.DryRun {
			imported = append(imported, r.ID)
		}
		for _, warning := range r.Warnings {
			result.Warnings = appendUnique(result.Warnings, warning)
		}
	}
	if len(branches) > 0 {
		result.Files = append(result.Files, manifest)
		if !opts.DryRun {
			if err := ensureDir(filepath.Dir(manifest)); err != nil {
				return Result{}, err
			}
			if err := writeJSONFile(manifest, openCodeTree{1, id, opts.CWD, metadata}, 0o600); err != nil {
				return Result{}, err
			}
		}
		result.Warnings = appendUnique(result.Warnings, fmt.Sprintf("%d delegated agent runs moved as OpenCode child sessions; live execution was not transferred", len(branches)))
	}
	committed = true
	return result, nil
}
