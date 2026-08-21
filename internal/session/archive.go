package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An archive is the other half of a compaction: everything the abridged
// transcript no longer carries, written where the target can read it without
// agentswap's help. That last point decides the format. A target harness can
// always open a file in its own working process; it cannot always be given
// permission to run a new command, and asking it to page through one very large
// JSON document is worse than handing it named text files. So the archive is a
// directory of plain text, with the machine-readable session beside it.

const archiveSchemaVersion = 1

// DefaultArchiveDirName is the project-local directory archives go in. Keeping
// them beside the code rather than in agentswap's own configuration directory
// is what makes them readable at all: a coding agent is confined to its working
// directory, so an archive outside it cannot be opened without the user
// granting access, and a non-interactive resume has nobody to ask.
const DefaultArchiveDirName = ".agentswap"

// NewArchiveDir allocates the directory one compaction will occupy under root.
// The id is the archive's own, not the target session's, because the markers
// naming this path are written before any target id exists.
func NewArchiveDir(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("archive root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, id), nil
}

// Endpoint names one side of a transfer in the manifest.
type Endpoint struct {
	Agent     Agent  `json:"agent"`
	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Path      string `json:"path,omitempty"`
}

// Shard describes one file in the archive and the content it stands in for.
type Shard struct {
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Event  int    `json:"event"`
	Role   string `json:"role,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Bytes  int    `json:"bytes"`
	Lines  int    `json:"lines,omitempty"`
	SHA256 string `json:"sha256"`
}

// Manifest is the archive's record of itself. It is versioned so a later
// agentswap can read an archive this one wrote.
type Manifest struct {
	SchemaVersion    int              `json:"schema_version"`
	AgentswapVersion string           `json:"agentswap_version,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	Source           Endpoint         `json:"source"`
	Target           *Endpoint        `json:"target,omitempty"`
	Compaction       CompactionReport `json:"compaction"`
	Shards           []Shard          `json:"shards,omitempty"`
}

// Archive is a compaction's output, held in memory until it is written. Nothing
// touches the filesystem during a dry run.
type Archive struct {
	Dir      string
	Manifest Manifest

	history *Session
	files   []archiveFile
}

type archiveFile struct {
	name string
	data []byte
}

type archiveBuilder struct {
	dir     string
	history *Session
	opts    CompactOptions
	files   []archiveFile
	shards  []Shard
	used    map[string]int
}

func newArchiveBuilder(dir string, history *Session, opts CompactOptions) *archiveBuilder {
	return &archiveBuilder{dir: dir, history: history, opts: opts, used: make(map[string]int)}
}

// add stores one shard and returns the absolute path a marker will name.
func (b *archiveBuilder) add(name string, data []byte, shard Shard) string {
	// Two tool calls can share a name once sanitized, and a call can be
	// answered more than once, so a collision gets a counter rather than
	// silently overwriting the earlier payload.
	if n := b.used[name]; n > 0 {
		ext := filepath.Ext(name)
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n+1, ext)
	}
	b.used[name]++
	shard.File = name
	shard.Bytes = len(data)
	shard.SHA256 = hashHex(data)
	b.files = append(b.files, archiveFile{name: name, data: data})
	b.shards = append(b.shards, shard)
	return filepath.Join(b.dir, name)
}

func (b *archiveBuilder) addToolResult(eventIndex int, part Part) string {
	name := fmt.Sprintf("events/%04d-tool-result-%s.txt", eventIndex, sanitizeName(part.CallID))
	return b.add(name, []byte(part.Text), Shard{
		Kind: "tool_result", Event: eventIndex, CallID: part.CallID,
		Lines: strings.Count(part.Text, "\n") + 1,
	})
}

func (b *archiveBuilder) addText(eventIndex int, role string, part Part) string {
	name := fmt.Sprintf("events/%04d-%s-message.txt", eventIndex, sanitizeName(role))
	return b.add(name, []byte(part.Text), Shard{
		Kind: "message", Event: eventIndex, Role: role,
		Lines: strings.Count(part.Text, "\n") + 1,
	})
}

func (b *archiveBuilder) addMedia(eventIndex int, part Part) string {
	data, err := decodeMediaBase64(part.MediaData)
	if err != nil || part.MediaData == "" {
		// A media part that is only a remote URL has no payload to keep, so the
		// archive records the reference instead.
		body := []byte("source URL: " + part.MediaURL + "\n")
		name := fmt.Sprintf("media/%04d-%s.txt", eventIndex, sanitizeName(part.Filename))
		return b.add(name, body, Shard{Kind: "media_reference", Event: eventIndex, CallID: part.CallID})
	}
	base := sanitizeName(part.Filename)
	if base == "unnamed" {
		base = "attachment"
	}
	name := fmt.Sprintf("media/%04d-%s%s", eventIndex, base, mediaExtension(part.MediaType, part.Filename))
	return b.add(name, data, Shard{Kind: "media", Event: eventIndex, CallID: part.CallID})
}

func (b *archiveBuilder) addCollapsed(from, to int, events []Event) string {
	text := renderEvents(events)
	name := fmt.Sprintf("events/collapsed-%04d-%04d.txt", from, to-1)
	return b.add(name, []byte(text), Shard{
		Kind: "collapsed_turns", Event: from, Lines: strings.Count(text, "\n") + 1,
	})
}

func (b *archiveBuilder) archive() *Archive {
	return &Archive{
		Dir:     b.dir,
		history: b.history,
		files:   b.files,
		Manifest: Manifest{
			SchemaVersion:    archiveSchemaVersion,
			AgentswapVersion: b.opts.Version,
			CreatedAt:        b.opts.Now,
			Source: Endpoint{
				Agent: b.history.Source, SessionID: b.history.SourceID,
				Model: b.history.Model, CWD: b.history.CWD,
			},
			Shards: b.shards,
		},
	}
}

// Write commits the archive. It stages the whole directory beside its final
// name and renames it into place, so a target is never pointed at a partial
// archive by a run that failed halfway.
func (a *Archive) Write() error {
	if a == nil {
		return nil
	}
	staging := a.Dir + ".tmp"
	removeIfExists(staging)
	committed := false
	defer func() {
		if !committed {
			removeIfExists(staging)
		}
	}()
	if err := ensureDir(staging); err != nil {
		return err
	}
	for _, file := range a.files {
		path := filepath.Join(staging, filepath.FromSlash(file.name))
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return err
		}
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			return err
		}
	}
	history, err := json.MarshalIndent(a.history, "", "  ")
	if err != nil {
		return fmt.Errorf("encode archived history: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "history.json"), append(history, '\n'), 0o600); err != nil {
		return err
	}
	transcript := renderEvents(a.history.Events)
	if err := os.WriteFile(filepath.Join(staging, "transcript.txt"), []byte(transcript), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "INDEX.md"), []byte(a.index()), 0o600); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(staging, "manifest.json"), a.Manifest, 0o600); err != nil {
		return err
	}
	// An archive normally lands inside the user's project now, and it is a
	// complete copy of a session. A .gitignore matching everything — itself
	// included — keeps the whole directory out of version control without
	// agentswap editing a .gitignore the user maintains.
	if err := os.WriteFile(filepath.Join(staging, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(a.Dir)); err != nil {
		return err
	}
	removeIfExists(a.Dir)
	if err := os.Rename(staging, a.Dir); err != nil {
		return err
	}
	committed = true
	return nil
}

// Remove deletes a written archive. A transfer whose target write failed leaves
// nothing behind, matching how the writers themselves roll back.
func (a *Archive) Remove() {
	if a != nil {
		removeIfExists(a.Dir)
	}
}

// Finalize records the session the archive was created for, which is only known
// once the target has been written.
func (a *Archive) Finalize(result Result) error {
	if a == nil {
		return nil
	}
	a.Manifest.Target = &Endpoint{
		Agent: result.Agent, SessionID: result.ID, Path: result.Path,
	}
	return writeJSONFile(filepath.Join(a.Dir, "manifest.json"), a.Manifest, 0o600)
}

// index is the first file a resuming agent should open: what this archive is,
// and which file holds each thing the transcript no longer carries.
func (a *Archive) index() string {
	var b strings.Builder
	b.WriteString("# Archived session history\n\n")
	fmt.Fprintf(&b, "This directory holds the complete history of %s session `%s`, which\n",
		a.Manifest.Source.Agent.Display(), a.Manifest.Source.SessionID)
	b.WriteString("agentswap abridged when it transferred the session into another coding agent.\n")
	b.WriteString("Wherever content was removed, the transferred conversation carries an inline\n")
	b.WriteString("marker saying `agentswap:elided` and ending with `full text: <absolute path>`.\n")
	b.WriteString("Each of those paths is one of the files listed below.\n\n")

	b.WriteString("## Start here\n\n")
	b.WriteString("| File | What it holds |\n| --- | --- |\n")
	b.WriteString("| `transcript.txt` | The entire original conversation as plain text. Grep this first. |\n")
	b.WriteString("| `history.json` | The same conversation, machine readable, with every tool call and result. |\n")
	b.WriteString("| `manifest.json` | What was removed, and the checksum of every file here. |\n\n")

	c := a.Manifest.Compaction
	fmt.Fprintf(&b, "The transfer went from about %s tokens to about %s, against a %s token budget.\n\n",
		humanCount(c.Before), humanCount(c.After), humanCount(c.Budget))

	if len(a.Manifest.Shards) == 0 {
		return b.String()
	}
	b.WriteString("## Elided content\n\n")
	b.WriteString("| File | Kind | Event | Size |\n| --- | --- | --- | --- |\n")
	for _, shard := range a.Manifest.Shards {
		detail := shard.CallID
		if detail == "" {
			detail = shard.Role
		}
		kind := shard.Kind
		if detail != "" {
			kind = fmt.Sprintf("%s (`%s`)", shard.Kind, detail)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s bytes |\n", shard.File, kind, shard.Event, humanCount(shard.Bytes))
	}
	return b.String()
}

// sanitizeName keeps an id or a role usable as a file name on every platform
// agentswap runs on, without letting a source-controlled string escape the
// archive directory.
func sanitizeName(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, value)
	cleaned = strings.Trim(cleaned, "-")
	if cleaned == "" {
		return "unnamed"
	}
	if len(cleaned) > 60 {
		cleaned = cleaned[:60]
	}
	return cleaned
}

func mediaExtension(mediaType, filename string) string {
	if ext := filepath.Ext(filename); ext != "" && len(ext) <= 6 {
		return strings.ToLower(ext)
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	default:
		return ".bin"
	}
}
