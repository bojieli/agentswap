// Package session discovers and translates local coding-agent sessions.
//
// Teleportation is deliberately offline and user initiated. It never copies
// credentials, chooses a destination, or asks one agent to summarize another
// agent's history.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Agent identifies a supported coding-agent harness.
type Agent string

const (
	Claude   Agent = "claude"
	Codex    Agent = "codex"
	OpenCode Agent = "opencode"
	Kimi     Agent = "kimi"
)

var allAgents = []Agent{Claude, Codex, OpenCode, Kimi}

func Agents() []Agent { return append([]Agent(nil), allAgents...) }

func ParseAgent(s string) (Agent, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "claude", "claude-code", "claudecode":
		return Claude, nil
	case "codex", "openai-codex":
		return Codex, nil
	case "opencode", "open-code", "open_code":
		return OpenCode, nil
	case "kimi", "kimi-code", "kimi-code-cli", "kimicode":
		return Kimi, nil
	default:
		return "", fmt.Errorf("unknown agent %q (want claude, codex, opencode, or kimi)", s)
	}
}

func (a Agent) Display() string {
	switch a {
	case Claude:
		return "Claude Code"
	case Codex:
		return "Codex"
	case OpenCode:
		return "OpenCode"
	case Kimi:
		return "Kimi Code"
	default:
		return string(a)
	}
}

type PartKind string

const (
	Text       PartKind = "text"
	Reasoning  PartKind = "reasoning"
	ToolCall   PartKind = "tool_call"
	ToolResult PartKind = "tool_result"
	// Media is inline binary content such as an image. MediaData stores the
	// base64 payload without a data-URL prefix; MediaURL is used for a remote
	// URL (or a source URL that cannot be decoded locally).
	Media PartKind = "media"
)

// Part is one observable, ordered unit inside a message. Data contains a tool
// input as JSON. Text contains text, recorded reasoning, or a tool result.
// Media carries an inline binary payload or a source URL.
type Part struct {
	Kind      PartKind        `json:"kind"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     bool            `json:"error,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	MediaData string          `json:"media_data,omitempty"`
	MediaURL  string          `json:"media_url,omitempty"`
	Filename  string          `json:"filename,omitempty"`
}

type EventKind string

const (
	Message EventKind = "message"
	Plan    EventKind = "plan"
)

// Event preserves a native message boundary when the source has one. Tool
// calls and results stay separate parts so their IDs and ordering survive even
// when a destination stores results inside an assistant message.
type Event struct {
	Kind      EventKind `json:"kind"`
	ID        string    `json:"id,omitempty"`
	ParentID  string    `json:"parent_id,omitempty"`
	Role      string    `json:"role,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	Parts     []Part    `json:"parts,omitempty"`
	PlanText  string    `json:"plan_text,omitempty"`
}

type Session struct {
	Source    Agent     `json:"source"`
	SourceID  string    `json:"source_id"`
	CWD       string    `json:"cwd"`
	Title     string    `json:"title,omitempty"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Events    []Event   `json:"events"`
	Branches  []Branch  `json:"branches,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Branch is one delegated agent run recorded beside the main thread. The main
// model never observed these events: it saw only the delegating tool call and
// whatever result the harness recorded for that call. A branch therefore
// travels as archived detail anchored to CallID, never as resumable
// main-thread context, and a destination without a native place to keep one
// reports the loss instead of folding it into the conversation.
//
// ParentID names the branch that spawned this one; an empty ParentID means the
// main thread. CallID is the tool call in that parent which opened the branch.
type Branch struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id,omitempty"`
	CallID      string    `json:"call_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Model       string    `json:"model,omitempty"`
	Status      string    `json:"status,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	Events      []Event   `json:"events"`
}

type Candidate struct {
	Agent     Agent
	ID        string
	CWD       string
	Path      string
	Format    string
	Title     string
	UpdatedAt time.Time
	Size      int64
}

type WriteOptions struct {
	CWD    string
	DryRun bool
}

type Result struct {
	Agent       Agent
	ID          string
	Path        string
	Resume      []string
	Warnings    []string
	Files       []string
	ExternalCLI bool

	// ArchivePath and Compaction are filled by the manager, not by a writer.
	// They are empty on a transfer that carried the whole history.
	ArchivePath string
	Compaction  *CompactionReport
}

type Adapter interface {
	Agent() Agent
	Discover(context.Context, string) ([]Candidate, error)
	Read(context.Context, Candidate) (*Session, error)
	Write(context.Context, *Session, WriteOptions) (Result, error)
}

// Validate rejects corrupt or unrepresentable canonical histories before any
// destination is touched. Dangling calls are allowed but called out: native
// writers synthesize an interrupted result when their API requires pairing.
// Branches are checked as independent streams, because a delegated run has its
// own tool-call namespace and never shares ids with the thread that spawned it.
func (s *Session) Validate() error {
	if s == nil {
		return errors.New("nil session")
	}
	if s.SourceID == "" {
		return errors.New("source session has no id")
	}
	if s.CWD == "" {
		return errors.New("source session has no working directory")
	}
	if len(s.Events) == 0 {
		return errors.New("source session has no transferable messages")
	}
	calls, dangling, err := validateEvents(s.Events, "")
	if err != nil {
		return err
	}
	for _, id := range dangling {
		s.Warnings = appendUnique(s.Warnings, fmt.Sprintf("tool call %s has no recorded result; the target will mark it interrupted", id))
	}
	// A branch may be spawned by another branch, so every stream's calls are
	// collected before any parent link is resolved.
	spawnable := map[string]struct{}{"": {}}
	for id := range calls {
		spawnable[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(s.Branches))
	for i, branch := range s.Branches {
		if branch.ID == "" {
			return fmt.Errorf("branch %d has no id", i+1)
		}
		if _, exists := seen[branch.ID]; exists {
			return fmt.Errorf("duplicate branch id %q", branch.ID)
		}
		seen[branch.ID] = struct{}{}
		label := fmt.Sprintf("branch %q: ", branch.ID)
		if len(branch.Events) == 0 {
			return fmt.Errorf("%shas no transferable messages", label)
		}
		branchCalls, branchDangling, err := validateEvents(branch.Events, label)
		if err != nil {
			return err
		}
		for _, id := range branchDangling {
			s.Warnings = appendUnique(s.Warnings, fmt.Sprintf("branch %s ended with tool call %s unanswered; the target will mark it interrupted", branch.ID, id))
		}
		for id := range branchCalls {
			spawnable[id] = struct{}{}
		}
	}
	for _, branch := range s.Branches {
		if branch.ParentID != "" {
			if _, exists := seen[branch.ParentID]; !exists {
				return fmt.Errorf("branch %q names unknown parent branch %q", branch.ID, branch.ParentID)
			}
		}
		if _, exists := spawnable[branch.CallID]; !exists {
			s.Warnings = appendUnique(s.Warnings, fmt.Sprintf("branch %s names tool call %s, which no transferred thread contains; it moved unattached", branch.ID, branch.CallID))
		}
	}
	return nil
}

// validateEvents checks one ordered event stream — the main thread or a single
// branch — and reports the tool calls it opened plus, in recorded order, the
// ones that never received a result. label prefixes every error so a failure
// inside a branch names the branch it came from.
func validateEvents(events []Event, label string) (map[string]struct{}, []string, error) {
	// The label carries a branch id from the source, so any verb inside it must
	// not be read as one.
	label = strings.ReplaceAll(label, "%", "%%")
	fail := func(format string, args ...any) error {
		return fmt.Errorf(label+format, args...)
	}
	calls := make(map[string]struct{})
	var callOrder []string
	results := make(map[string]struct{})
	toolResultEvents := make(map[string]int)
	for ei, event := range events {
		switch event.Kind {
		case Message:
			if event.Role != "user" && event.Role != "assistant" && event.Role != "tool" {
				return nil, nil, fail("event %d has unsupported role %q", ei+1, event.Role)
			}
			if len(event.Parts) == 0 {
				return nil, nil, fail("event %d has no content", ei+1)
			}
		case Plan:
			if strings.TrimSpace(event.PlanText) == "" {
				return nil, nil, fail("event %d has an empty plan", ei+1)
			}
		default:
			return nil, nil, fail("event %d has unsupported kind %q", ei+1, event.Kind)
		}
		for pi, part := range event.Parts {
			switch part.Kind {
			case Text, Reasoning:
				if part.Text == "" {
					return nil, nil, fail("event %d part %d has empty %s content", ei+1, pi+1, part.Kind)
				}
				if event.Role == "tool" {
					return nil, nil, fail("event %d has %s content in a tool message", ei+1, part.Kind)
				}
				if part.Kind == Reasoning && event.Role != "assistant" {
					return nil, nil, fail("event %d has reasoning in a %s message", ei+1, event.Role)
				}
			case ToolCall:
				if event.Role != "assistant" {
					return nil, nil, fail("event %d has a tool call in a %s message", ei+1, event.Role)
				}
				if part.CallID == "" || part.ToolName == "" {
					return nil, nil, fail("event %d part %d has an incomplete tool call", ei+1, pi+1)
				}
				if _, exists := calls[part.CallID]; exists {
					return nil, nil, fail("duplicate tool call id %q", part.CallID)
				}
				if len(part.Data) == 0 || !json.Valid(part.Data) {
					return nil, nil, fail("tool call %q has invalid JSON input", part.CallID)
				}
				calls[part.CallID] = struct{}{}
				callOrder = append(callOrder, part.CallID)
			case ToolResult:
				if event.Role == "assistant" {
					return nil, nil, fail("event %d has a tool result in an assistant message", ei+1)
				}
				if part.CallID == "" {
					return nil, nil, fail("event %d part %d has a tool result without a call id", ei+1, pi+1)
				}
				if _, exists := calls[part.CallID]; !exists {
					return nil, nil, fail("tool result %q appears before its matching call", part.CallID)
				}
				if previous, exists := toolResultEvents[part.CallID]; exists && previous != ei {
					return nil, nil, fail("duplicate result for tool call %q", part.CallID)
				}
				toolResultEvents[part.CallID] = ei
				results[part.CallID] = struct{}{}
			case Media:
				if event.Role == "tool" && part.CallID == "" {
					return nil, nil, fail("event %d part %d has media in a tool message without a call id", ei+1, pi+1)
				}
				if part.MediaData == "" && part.MediaURL == "" {
					return nil, nil, fail("event %d part %d has media without data or URL", ei+1, pi+1)
				}
				if part.MediaData != "" {
					if _, err := decodeMediaBase64(part.MediaData); err != nil {
						return nil, nil, fail("event %d part %d has invalid base64 media: %w", ei+1, pi+1, err)
					}
				}
				if strings.TrimSpace(part.MediaType) == "" {
					return nil, nil, fail("event %d part %d has media without a MIME type", ei+1, pi+1)
				}
				if part.CallID != "" {
					if _, exists := calls[part.CallID]; !exists {
						return nil, nil, fail("media %q appears before its matching call", part.CallID)
					}
					results[part.CallID] = struct{}{}
				}
			default:
				return nil, nil, fail("event %d part %d has unsupported kind %q", ei+1, pi+1, part.Kind)
			}
		}
	}
	var dangling []string
	for _, id := range callOrder {
		if _, exists := results[id]; !exists {
			dangling = append(dangling, id)
		}
	}
	return calls, dangling, nil
}

// branchesNotTransferred names the delegated runs a destination has no native
// place to keep. The conversation itself is complete either way — the main
// thread still carries the delegating call and the result the model saw — so
// what is reported here is the loss of each run's own transcript.
func branchesNotTransferred(target string, branches []Branch) string {
	const listed = 5
	names := make([]string, 0, listed+1)
	for i, branch := range branches {
		if i == listed {
			names = append(names, fmt.Sprintf("and %d more", len(branches)-listed))
			break
		}
		label := branch.ID
		if branch.Description != "" {
			label += " (" + branch.Description + ")"
		}
		names = append(names, label)
	}
	return fmt.Sprintf("%s has no representation for a delegated agent run, so %d %s did not move: %s. The main thread kept every delegating tool call and the result it recorded", target, len(branches), plural(len(branches), "transcript", "transcripts"), strings.Join(names, "; "))
}

// sortBranches orders branches the way the thread spawned them, so a
// destination lists a delegated run next to the call it came from. Branches
// with no recorded call sort last. The sort is stable, so branches sharing one
// call — a swarm launched by a single call spawns many — keep the order the
// reader produced.
func sortBranches(branches []Branch, events []Event) {
	position := make(map[string]int)
	next := 0
	for _, event := range events {
		for _, part := range event.Parts {
			if part.Kind == ToolCall {
				position[part.CallID] = next
				next++
			}
		}
	}
	sort.SliceStable(branches, func(i, j int) bool {
		a, aok := position[branches[i].CallID]
		b, bok := position[branches[j].CallID]
		if aok != bok {
			return aok
		}
		return aok && a < b
	})
}

// naturalLess orders ids such as agent-2 before agent-10, which a plain string
// comparison reverses.
func naturalLess(a, b string) bool {
	for len(a) > 0 && len(b) > 0 {
		aDigit, bDigit := a[0] >= '0' && a[0] <= '9', b[0] >= '0' && b[0] <= '9'
		if aDigit && bDigit {
			ai, bi := 0, 0
			for ai < len(a) && a[ai] >= '0' && a[ai] <= '9' {
				ai++
			}
			for bi < len(b) && b[bi] >= '0' && b[bi] <= '9' {
				bi++
			}
			an, _ := strconv.Atoi(a[:ai])
			bn, _ := strconv.Atoi(b[:bi])
			if an != bn {
				return an < bn
			}
			a, b = a[ai:], b[bi:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// linkBranchParents fills in the branch that spawned each branch, for sources
// that record the spawning call but not the agent tree. A call id belongs to
// exactly one stream, so the branch whose events contain it is the parent.
func linkBranchParents(branches []Branch) {
	owner := make(map[string]string)
	for _, branch := range branches {
		for _, event := range branch.Events {
			for _, part := range event.Parts {
				if part.Kind == ToolCall {
					owner[part.CallID] = branch.ID
				}
			}
		}
	}
	for i := range branches {
		if branches[i].ParentID != "" {
			continue
		}
		if parent := owner[branches[i].CallID]; parent != "" && parent != branches[i].ID {
			branches[i].ParentID = parent
		}
	}
}

func appendUnique(in []string, value string) []string {
	for _, old := range in {
		if old == value {
			return in
		}
	}
	return append(in, value)
}

func SortCandidates(in []Candidate) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].UpdatedAt.Equal(in[j].UpdatedAt) {
			if in[i].Agent == in[j].Agent {
				return in[i].ID < in[j].ID
			}
			return in[i].Agent < in[j].Agent
		}
		return in[i].UpdatedAt.After(in[j].UpdatedAt)
	})
}
