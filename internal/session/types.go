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
	Warnings  []string  `json:"warnings,omitempty"`
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
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	toolResults := make(map[string]struct{})
	toolResultEvents := make(map[string]int)
	for ei, event := range s.Events {
		switch event.Kind {
		case Message:
			if event.Role != "user" && event.Role != "assistant" && event.Role != "tool" {
				return fmt.Errorf("event %d has unsupported role %q", ei+1, event.Role)
			}
			if len(event.Parts) == 0 {
				return fmt.Errorf("event %d has no content", ei+1)
			}
		case Plan:
			if strings.TrimSpace(event.PlanText) == "" {
				return fmt.Errorf("event %d has an empty plan", ei+1)
			}
		default:
			return fmt.Errorf("event %d has unsupported kind %q", ei+1, event.Kind)
		}
		for pi, part := range event.Parts {
			switch part.Kind {
			case Text, Reasoning:
				if part.Text == "" {
					return fmt.Errorf("event %d part %d has empty %s content", ei+1, pi+1, part.Kind)
				}
				if event.Role == "tool" {
					return fmt.Errorf("event %d has %s content in a tool message", ei+1, part.Kind)
				}
				if part.Kind == Reasoning && event.Role != "assistant" {
					return fmt.Errorf("event %d has reasoning in a %s message", ei+1, event.Role)
				}
			case ToolCall:
				if event.Role != "assistant" {
					return fmt.Errorf("event %d has a tool call in a %s message", ei+1, event.Role)
				}
				if part.CallID == "" || part.ToolName == "" {
					return fmt.Errorf("event %d part %d has an incomplete tool call", ei+1, pi+1)
				}
				if _, exists := calls[part.CallID]; exists {
					return fmt.Errorf("duplicate tool call id %q", part.CallID)
				}
				if len(part.Data) == 0 || !json.Valid(part.Data) {
					return fmt.Errorf("tool call %q has invalid JSON input", part.CallID)
				}
				calls[part.CallID] = struct{}{}
			case ToolResult:
				if event.Role == "assistant" {
					return fmt.Errorf("event %d has a tool result in an assistant message", ei+1)
				}
				if part.CallID == "" {
					return fmt.Errorf("event %d part %d has a tool result without a call id", ei+1, pi+1)
				}
				if _, exists := calls[part.CallID]; !exists {
					return fmt.Errorf("tool result %q appears before its matching call", part.CallID)
				}
				if previous, exists := toolResultEvents[part.CallID]; exists && previous != ei {
					return fmt.Errorf("duplicate result for tool call %q", part.CallID)
				}
				toolResults[part.CallID] = struct{}{}
				toolResultEvents[part.CallID] = ei
				results[part.CallID] = struct{}{}
			case Media:
				if event.Role == "tool" && part.CallID == "" {
					return fmt.Errorf("event %d part %d has media in a tool message without a call id", ei+1, pi+1)
				}
				if part.MediaData == "" && part.MediaURL == "" {
					return fmt.Errorf("event %d part %d has media without data or URL", ei+1, pi+1)
				}
				if part.MediaData != "" {
					if _, err := decodeMediaBase64(part.MediaData); err != nil {
						return fmt.Errorf("event %d part %d has invalid base64 media: %w", ei+1, pi+1, err)
					}
				}
				if strings.TrimSpace(part.MediaType) == "" {
					return fmt.Errorf("event %d part %d has media without a MIME type", ei+1, pi+1)
				}
				if part.CallID != "" {
					if _, exists := calls[part.CallID]; !exists {
						return fmt.Errorf("media %q appears before its matching call", part.CallID)
					}
					results[part.CallID] = struct{}{}
				}
			default:
				return fmt.Errorf("event %d part %d has unsupported kind %q", ei+1, pi+1, part.Kind)
			}
		}
	}
	for id := range calls {
		if _, exists := results[id]; !exists {
			s.Warnings = appendUnique(s.Warnings, fmt.Sprintf("tool call %s has no recorded result; the target will mark it interrupted", id))
		}
	}
	return nil
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
