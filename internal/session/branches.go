package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// orderedBranches validates parentage and orders parents before their children.
// Tool call IDs are scoped to the parent stream, never to the whole session.
func orderedBranches(branches []Branch) ([]Branch, error) {
	byID := make(map[string]Branch, len(branches))
	for _, b := range branches {
		if b.ID == "" {
			return nil, fmt.Errorf("branch has no id")
		}
		if _, ok := byID[b.ID]; ok {
			return nil, fmt.Errorf("duplicate branch id %q", b.ID)
		}
		byID[b.ID] = b
	}
	state := map[string]int{}
	var out []Branch
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("branch parent cycle at %q", id)
		}
		b, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown parent branch %q", id)
		}
		state[id] = 1
		if b.ParentID != "" {
			if err := visit(b.ParentID); err != nil {
				return err
			}
		}
		state[id] = 2
		out = append(out, b)
		return nil
	}
	for _, b := range branches {
		if err := visit(b.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func branchHistory(main *Session, b Branch) *Session {
	return &Session{Source: main.Source, SourceID: b.ID, CWD: main.CWD,
		Title: safeTitle(b.Description, b.Name), Model: b.Model, CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt, Events: b.Events}
}

func branchMetadata(b Branch) Branch { b.Events = nil; return b }

func isDelegation(name string) bool {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case "task", "agent", "agentswarm", "spawn_agent", "send_input", "resume_agent", "wait", "wait_agent", "close_agent":
		return true
	}
	return false
}

// Rewrite only delegation references, never ordinary messages or shell commands.
// Native IDs are regenerated to avoid collisions with existing target sessions.
func remapDelegation(events []Event, ids map[string]string) []Event {
	out := append([]Event(nil), events...)
	calls := map[string]bool{}
	for _, e := range events {
		for _, p := range e.Parts {
			if p.Kind == ToolCall && isDelegation(p.ToolName) {
				calls[p.CallID] = true
			}
		}
	}
	var rewrite func(any) any
	rewrite = func(v any) any {
		switch v := v.(type) {
		case string:
			return remapIDText(v, ids)
		case []any:
			for i := range v {
				v[i] = rewrite(v[i])
			}
			return v
		case map[string]any:
			n := map[string]any{}
			for k, x := range v {
				if id, ok := ids[k]; ok {
					k = id
				}
				n[k] = rewrite(x)
			}
			return n
		default:
			return v
		}
	}
	for i := range out {
		out[i].Parts = append([]Part(nil), out[i].Parts...)
		for j := range out[i].Parts {
			p := &out[i].Parts[j]
			if !calls[p.CallID] {
				continue
			}
			switch p.Kind {
			case ToolCall:
				var v any
				if json.Unmarshal(p.Data, &v) == nil {
					p.Data, _ = json.Marshal(rewrite(v))
				}
			case ToolResult:
				p.Text = remapIDText(p.Text, ids)
			}
		}
	}
	return out
}

func remapIDText(s string, ids map[string]string) string {
	// Scan once: a replacement must never be rewritten as another source ID.
	isID := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		if !isID(s[i]) {
			out.WriteByte(s[i])
			i++
			continue
		}
		end := i + 1
		for end < len(s) && isID(s[end]) {
			end++
		}
		token := s[i:end]
		if id, ok := ids[token]; ok {
			token = id
		}
		out.WriteString(token)
		i = end
	}
	return out.String()
}

func branchReferenceIDs(source Agent, branches []Branch, ids map[string]string) map[string]string {
	out := make(map[string]string, len(ids))
	for k, v := range ids {
		if k != "" {
			out[k] = v
		}
	}
	if source == Claude {
		for _, b := range branches {
			if strings.HasPrefix(b.ID, "agent-") {
				out[strings.TrimPrefix(b.ID, "agent-")] = ids[b.ID]
			}
		}
	}
	return out
}

// Older Claude transcripts omit the spawning-call sidecar but include agentId
// in a Task/Agent result. Recover only a unique parent/call association.
func linkBranchesFromResults(branches []Branch, main []Event, source Agent) {
	ids := map[string]string{}
	for _, b := range branches {
		ids[b.ID] = b.ID
	}
	aliases := branchReferenceIDs(source, branches, ids)
	type link struct{ parent, call string }
	links := map[string]link{}
	ambiguous := map[string]bool{}
	streams := append([][]Event{main}, branchEventStreams(branches)...)
	for si, events := range streams {
		parent := ""
		if si > 0 {
			parent = branches[si-1].ID
		}
		calls := map[string]bool{}
		for _, e := range events {
			for _, p := range e.Parts {
				if p.Kind == ToolCall && isDelegation(p.ToolName) {
					calls[p.CallID] = true
				}
			}
		}
		for _, e := range events {
			for _, p := range e.Parts {
				if p.Kind != ToolResult || !calls[p.CallID] {
					continue
				}
				// Tokenize by the same boundaries used for reference rewriting.
				for _, token := range strings.FieldsFunc(p.Text, func(r rune) bool {
					return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
				}) {
					id, ok := aliases[token]
					if !ok || id == parent {
						continue
					}
					next := link{parent, p.CallID}
					if old, ok := links[id]; ok && old != next {
						ambiguous[id] = true
					} else {
						links[id] = next
					}
				}
			}
		}
	}
	for i := range branches {
		b := &branches[i]
		if l, ok := links[b.ID]; ok && b.CallID == "" && !ambiguous[b.ID] {
			b.CallID = l.call
			b.ParentID = l.parent
		}
	}
}

func sortBranchesByID(branches []Branch) {
	sort.Slice(branches, func(i, j int) bool { return branches[i].ID < branches[j].ID })
}
