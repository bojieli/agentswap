package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestNativeBranchTransfers(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, from := range []Agent{Claude, Codex, Kimi} {
			for _, to := range []Agent{Claude, Codex, Kimi} {
				if from == to {
					continue
				}
				t.Run(string(from)+"-"+string(to)+map[bool]string{true: "-python", false: ""}[legacy], func(t *testing.T) {
					isolatedHomes(t)
					if legacy {
						t.Setenv("AGENTSWAP_KIMI_FORMAT", "legacy")
					}
					cwd := t.TempDir()
					history := sampleHistory(t, cwd)
					history.Source = from
					// Deliberately put the nested run first: native writers must order
					// parents before children without losing their links.
					bs := sampleBranches()
					history.Branches = []Branch{bs[1], bs[0]}
					manager := NewManager()
					opts := WriteOptions{CWD: cwd}
					source, err := manager.adapters[from].Write(context.Background(), history, opts)
					if err != nil {
						t.Fatal(err)
					}
					candidates, err := manager.adapters[from].Discover(context.Background(), cwd)
					if err != nil {
						t.Fatal(err)
					}
					candidate, err := Select(candidates, source.ID, false)
					if err != nil {
						t.Fatal(err)
					}
					result, original, err := manager.Teleport(context.Background(), candidate, to, TransferOptions{WriteOptions: opts})
					if err != nil {
						t.Fatal(err)
					}
					if len(original.Branches) != 2 {
						t.Fatalf("source branches = %d", len(original.Branches))
					}
					candidates, err = manager.adapters[to].Discover(context.Background(), cwd)
					if err != nil {
						t.Fatal(err)
					}
					candidate, err = Select(candidates, result.ID, false)
					if err != nil {
						t.Fatal(err)
					}
					got, err := manager.adapters[to].Read(context.Background(), candidate)
					if err != nil {
						t.Fatal(err)
					}
					if err := got.Validate(); err != nil {
						t.Fatal(err)
					}
					checkBranchTree(t, got)
					// Transfer back, exercising both reader and writer semantics.
					back, _, err := manager.Teleport(context.Background(), candidate, from, TransferOptions{WriteOptions: opts})
					if err != nil {
						t.Fatal(err)
					}
					candidates, _ = manager.adapters[from].Discover(context.Background(), cwd)
					candidate, err = Select(candidates, back.ID, false)
					if err != nil {
						t.Fatal(err)
					}
					got, err = manager.adapters[from].Read(context.Background(), candidate)
					if err != nil {
						t.Fatal(err)
					}
					checkBranchTree(t, got)
				})
			}
		}
	}
}

func checkBranchTree(t *testing.T, history *Session) {
	t.Helper()
	if len(history.Branches) != 2 {
		t.Fatalf("branches = %#v", history.Branches)
	}
	var parent, child Branch
	for _, b := range history.Branches {
		if b.ParentID == "" {
			parent = b
		} else {
			child = b
		}
	}
	if parent.ID == "" || child.ParentID != parent.ID || parent.CallID != "call-1" || child.CallID != "branch-call-1" {
		t.Fatalf("lost parent or call links: %#v", history.Branches)
	}
	for _, b := range history.Branches {
		if len(b.Events) == 0 {
			t.Fatalf("empty branch %s", b.ID)
		}
	}
	encoded, _ := json.Marshal(history.Events)
	if strings.Contains(string(encoded), "All twelve look fine.") {
		t.Fatal("branch text leaked into main context")
	}
	encoded, _ = json.Marshal(history.Branches)
	for _, text := range []string{"12 matches", "All twelve look fine."} {
		if !strings.Contains(string(encoded), text) {
			t.Fatalf("lost branch content %q", text)
		}
	}
}

func TestCodexNativeSpawnDiscovery(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	history := sampleHistory(t, cwd)
	history.Branches = sampleBranches()
	r, err := (codexAdapter{}).Write(context.Background(), history, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	// Remove all AgentSwap metadata. The native source.parent_thread_id and
	// collab_agent_spawn_end event alone must suffice to discover a child.
	var childID string
	for _, path := range r.Files[1:] {
		var records []map[string]any
		if err := readJSONL(path, func(_ int, raw json.RawMessage) error {
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			records = append(records, record)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		meta := records[0]["payload"].(map[string]any)
		if stringValue(meta["parent_thread_id"]) == r.ID {
			childID = stringValue(meta["id"])
		}
		delete(meta, "agentswap_branch")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if err := writeJSONLine(f, record); err != nil {
				t.Fatal(err)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(r.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONLine(f, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "collab_agent_spawn_end", "call_id": "call-1", "new_thread_id": childID, "prompt": "survey", "status": "pending_init"}}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := (codexAdapter{}).Read(context.Background(), Candidate{ID: r.ID, Path: r.Path, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Branches) != 2 {
		t.Fatalf("branches = %#v", got.Branches)
	}
	for _, b := range got.Branches {
		if b.ID == childID && b.CallID != "call-1" {
			t.Fatalf("spawn link = %#v", b)
		}
	}
	candidates, err := (codexAdapter{}).Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("child polluted root picker: %d, %v", len(candidates), err)
	}
}

func TestBranchCyclesAndScopedCallLinks(t *testing.T) {
	history := sampleHistory(t, t.TempDir())
	history.Branches = sampleBranches()
	history.Branches[0].ParentID = history.Branches[1].ID
	if err := history.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle validation = %v", err)
	}
}

func TestClaudeBranchLinksWithoutSidecars(t *testing.T) {
	branches := []Branch{{ID: "agent-abc", Events: []Event{{Kind: Message, Role: "user", Parts: []Part{{Kind: Text, Text: "inspect"}}}}}}
	main := []Event{{Kind: Message, Role: "assistant", Parts: []Part{{Kind: ToolCall, ToolName: "Agent", CallID: "spawn", Data: json.RawMessage(`{}`)}}},
		{Kind: Message, Role: "tool", Parts: []Part{{Kind: ToolResult, CallID: "spawn", Text: "agentId: abc (for resuming)"}}}}
	linkBranchesFromResults(branches, main, Claude)
	if branches[0].CallID != "spawn" || branches[0].ParentID != "" {
		t.Fatalf("link = %#v", branches[0])
	}
	refs := branchReferenceIDs(Claude, branches, map[string]string{"agent-abc": "codex-child"})
	got := remapDelegation(main, refs)
	if !strings.Contains(got[1].Parts[0].Text, "codex-child") {
		t.Fatalf("native Claude ID was not rewritten: %#v", got)
	}
}

func TestCodexWorktreeBranchTransfer(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	worktree := t.TempDir()
	history := sampleHistory(t, cwd)
	history.Branches = sampleBranches()
	for i := range history.Branches {
		history.Branches[i].CWD = worktree
	}
	ctx := context.Background()
	r, err := (codexAdapter{}).Write(ctx, history, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	got, err := (codexAdapter{}).Read(ctx, Candidate{Agent: Codex, ID: r.ID, Path: r.Path, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Branches) != 2 {
		t.Fatalf("worktree children lost: %#v", got.Branches)
	}
	for _, b := range got.Branches {
		if !samePath(b.CWD, worktree) {
			t.Fatalf("child cwd = %s", b.CWD)
		}
	}
	r, err = (claudeAdapter{}).Write(ctx, got, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	got, err = (claudeAdapter{}).Read(ctx, Candidate{Agent: Claude, ID: r.ID, Path: r.Path, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got.Branches {
		if !samePath(b.CWD, worktree) {
			t.Fatalf("Claude child cwd = %s", b.CWD)
		}
	}
}

func TestCodexDiscoveryIgnoresStagedTrees(t *testing.T) {
	isolatedHomes(t)
	cwd := t.TempDir()
	h := sampleHistory(t, cwd)
	r, err := (codexAdapter{}).Write(context.Background(), h, WriteOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(filepath.Dir(r.Path), ".agentswap-tree-pending")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "pending.jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := (codexAdapter{}).Discover(context.Background(), cwd)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("staged tree entered picker: %v %v", candidates, err)
	}
}

func TestDelegationIDRemapping(t *testing.T) {
	events := []Event{{Kind: Message, Role: "assistant", Parts: []Part{
		{Kind: Text, Text: "agent-0 should stay in prose"},
		{Kind: ToolCall, CallID: "spawn", ToolName: "functions.spawn_agent", Data: json.RawMessage(`{"target":"agent-0"}`)},
		{Kind: ToolCall, CallID: "shell", ToolName: "Bash", Data: json.RawMessage(`{"command":"echo agent-0"}`)},
	}}, {Kind: Message, Role: "tool", Parts: []Part{{Kind: ToolResult, CallID: "spawn", Text: `{"agent_id":"agent-0","other":"agent-01"}`}}}}
	before, _ := json.Marshal(events)
	got := remapDelegation(events, map[string]string{"agent-0": "new-id"})
	after, _ := json.Marshal(events)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source mutated")
	}
	if got[0].Parts[0].Text != events[0].Parts[0].Text || string(got[0].Parts[2].Data) != string(events[0].Parts[2].Data) {
		t.Fatal("non-delegation content rewritten")
	}
	if !strings.Contains(got[1].Parts[0].Text, "new-id") || !strings.Contains(got[1].Parts[0].Text, "agent-01") {
		t.Fatalf("result = %s", got[1].Parts[0].Text)
	}
}

func fakeOpenCodeTreeCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture executable is a shell script")
	}
	root := isolatedHomes(t)
	store := filepath.Join(root, "oc")
	os.MkdirAll(store, 0o700)
	path := filepath.Join(root, "opencode")
	// Capture each export independently; this models OpenCode's CLI boundary
	// without coupling the test to its database schema.
	script := `#!/bin/sh
set -eu
if [ "$1" = import ]; then
 id=$(sed -n 's/.*"id": "\(ses_[^"]*\)".*/\1/p' "$2" | head -1)
 cp "$2" "$OC_STORE/$id.json"
 if [ "${OC_FAIL:-}" = "$id" ] || [ "${OC_FAIL_CHILD:-}" = 1 ] && rg -q '"parentID": "ses_' "$2"; then echo unconfirmed; exit 0; fi
 echo "Imported session: $id"; exit 0
fi
if [ "$1" = export ]; then cat "$OC_STORE/$2.json"; exit 0; fi
if [ "$1 $2" = 'session delete' ]; then rm -f "$OC_STORE/$3.json"; exit 0; fi
exit 2
`
	// Avoid requiring ripgrep in the test subprocess (Windows is skipped).
	script = strings.ReplaceAll(script, "rg -q", "grep -q")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTSWAP_OPENCODE_BIN", path)
	t.Setenv("OC_STORE", store)
	return store
}

func TestOpenCodeBranchTransfersAndRollback(t *testing.T) {
	for _, target := range []Agent{Claude, Codex, Kimi} {
		t.Run(string(target), func(t *testing.T) {
			fakeOpenCodeTreeCLI(t)
			cwd := t.TempDir()
			history := sampleHistory(t, cwd)
			history.Branches = sampleBranches()
			manager := NewManager()
			r, err := manager.adapters[OpenCode].Write(context.Background(), history, WriteOptions{CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			candidate := Candidate{Agent: OpenCode, ID: r.ID, CWD: cwd}
			got, err := manager.adapters[OpenCode].Read(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			checkBranchTree(t, got)
			result, _, err := manager.Teleport(context.Background(), candidate, target, TransferOptions{WriteOptions: WriteOptions{CWD: cwd}})
			if err != nil {
				t.Fatal(err)
			}
			candidates, _ := manager.adapters[target].Discover(context.Background(), cwd)
			candidate, err = Select(candidates, result.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			back, _, err := manager.Teleport(context.Background(), candidate, OpenCode, TransferOptions{WriteOptions: WriteOptions{CWD: cwd}})
			if err != nil {
				t.Fatal(err)
			}
			got, err = manager.adapters[OpenCode].Read(context.Background(), Candidate{Agent: OpenCode, ID: back.ID, CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			checkBranchTree(t, got)
		})
	}
	t.Run("rollback child failure", func(t *testing.T) {
		store := fakeOpenCodeTreeCLI(t)
		cwd := t.TempDir()
		history := sampleHistory(t, cwd)
		history.Branches = sampleBranches()
		t.Setenv("OC_FAIL_CHILD", "1")
		_, err := (openCodeAdapter{}).Write(context.Background(), history, WriteOptions{CWD: cwd})
		if err == nil {
			t.Fatal("expected child import failure")
		}
		entries, _ := os.ReadDir(store)
		if len(entries) != 0 {
			t.Fatalf("partial tree left behind: %v", entries)
		}
	})
}

func TestBranchDryRunsLeaveNoArtifacts(t *testing.T) {
	for _, agent := range Agents() {
		t.Run(string(agent), func(t *testing.T) {
			root := isolatedHomes(t)
			cwd := t.TempDir()
			history := sampleHistory(t, cwd)
			history.Branches = sampleBranches()
			_, err := NewManager().adapters[agent].Write(context.Background(), history, WriteOptions{CWD: cwd, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatalf("dry run wrote %v", entries)
			}
		})
	}
}
