package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type Manager struct {
	adapters map[Agent]Adapter
}

func NewManager() *Manager {
	adapters := []Adapter{
		newClaudeAdapter(),
		newCodexAdapter(),
		newOpenCodeAdapter(),
		newKimiAdapter(),
	}
	m := &Manager{adapters: make(map[Agent]Adapter, len(adapters))}
	for _, adapter := range adapters {
		m.adapters[adapter.Agent()] = adapter
	}
	return m
}

func NewManagerWith(adapters ...Adapter) *Manager {
	m := &Manager{adapters: make(map[Agent]Adapter, len(adapters))}
	for _, adapter := range adapters {
		m.adapters[adapter.Agent()] = adapter
	}
	return m
}

type DiscoverOptions struct {
	Target Agent
	From   Agent
	CWD    string
}

func (m *Manager) Discover(ctx context.Context, opts DiscoverOptions) ([]Candidate, error) {
	var agents []Agent
	if opts.From != "" {
		if opts.From == opts.Target {
			return nil, errors.New("source and target agents are the same")
		}
		agents = []Agent{opts.From}
	} else {
		for _, agent := range allAgents {
			if agent != opts.Target {
				agents = append(agents, agent)
			}
		}
	}
	var candidates []Candidate
	var failures []string
	for _, agent := range agents {
		adapter := m.adapters[agent]
		if adapter == nil {
			continue
		}
		found, err := adapter.Discover(ctx, opts.CWD)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", agent.Display(), err))
			continue
		}
		candidates = append(candidates, found...)
	}
	SortCandidates(candidates)
	if len(candidates) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("session discovery failed: %s", strings.Join(failures, "; "))
	}
	return candidates, nil
}

func Select(candidates []Candidate, sessionID string, latest bool) (Candidate, error) {
	if sessionID != "" {
		var matches []Candidate
		for _, candidate := range candidates {
			if candidate.ID == sessionID || strings.TrimPrefix(candidate.ID, "session_") == strings.TrimPrefix(sessionID, "session_") {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			return Candidate{}, fmt.Errorf("session %q was not found in the current working directory", sessionID)
		}
		if len(matches) > 1 {
			return Candidate{}, fmt.Errorf("session id %q matches more than one source session", sessionID)
		}
		return matches[0], nil
	}
	if len(candidates) == 0 {
		return Candidate{}, errors.New("no source sessions found for the current working directory")
	}
	if len(candidates) == 1 || latest {
		return candidates[0], nil
	}
	return Candidate{}, &AmbiguousError{Candidates: candidates}
}

type AmbiguousError struct{ Candidates []Candidate }

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%d source sessions match the current working directory", len(e.Candidates))
}

// TransferOptions carries what a transfer decides on top of what a writer
// needs. Compaction is a manager-level concern: it happens once, between
// validation and the write, so every adapter gets it without any adapter
// knowing about it.
type TransferOptions struct {
	WriteOptions
	Compact *CompactOptions
}

func (m *Manager) Teleport(ctx context.Context, source Candidate, target Agent, opts TransferOptions) (Result, *Session, error) {
	reader := m.adapters[source.Agent]
	writer := m.adapters[target]
	if reader == nil || writer == nil {
		return Result{}, nil, errors.New("source or target adapter is unavailable")
	}
	if source.Agent == target {
		return Result{}, nil, errors.New("source and target agents are the same")
	}
	history, err := reader.Read(ctx, source)
	if err != nil {
		return Result{}, nil, fmt.Errorf("read %s session %s: %w", source.Agent.Display(), source.ID, err)
	}
	if !samePath(history.CWD, opts.CWD) {
		return Result{}, nil, fmt.Errorf("source session cwd %q no longer matches target cwd %q", history.CWD, opts.CWD)
	}
	// The complete history is validated before anything else happens, exactly
	// as it is without compaction. Compaction reduces a session that is already
	// known to be transferable; it never rescues one that is not.
	if err := history.Validate(); err != nil {
		return Result{}, history, fmt.Errorf("source session is not safely transferable: %w", err)
	}
	transferred, archive, report, err := m.compact(history, target, opts)
	if err != nil {
		return Result{}, history, err
	}
	result, err := writer.Write(ctx, transferred, opts.WriteOptions)
	if err != nil {
		archive.Remove()
		return Result{}, history, fmt.Errorf("write %s session: %w", target.Display(), err)
	}
	if archive != nil && !opts.DryRun {
		if err := archive.Finalize(result); err != nil {
			result.Warnings = appendUnique(result.Warnings, fmt.Sprintf("the archive was written but its manifest could not record the new session: %v", err))
		}
	}
	if opts.Compact != nil {
		result.Compaction = &report
	}
	if archive != nil {
		result.ArchivePath = archive.Dir
	}
	result.Warnings = append(result.Warnings, transferred.Warnings...)
	return result, history, nil
}

// compact runs the reduction when the user asked for one, and otherwise only
// measures. The measurement is why a plain transfer can warn: agentswap knows
// the size of what it is about to hand over, and staying silent about a session
// that will not fit is how a transfer looks successful and is not.
func (m *Manager) compact(history *Session, target Agent, opts TransferOptions) (*Session, *Archive, CompactionReport, error) {
	if opts.Compact == nil {
		if estimate := EstimateTokens(history); estimate > DefaultBudget(target) {
			history.Warnings = appendUnique(history.Warnings, fmt.Sprintf(
				"the transferred thread is about %s tokens, which is more than %s is likely to hold; re-run with --compact to abridge it and archive the full history",
				humanCount(estimate), target.Display()))
		}
		return history, nil, CompactionReport{}, nil
	}
	settings := *opts.Compact
	if settings.Budget <= 0 {
		settings.Budget = DefaultBudget(target)
	}
	if settings.ArchiveDir == "" {
		root := settings.ArchiveRoot
		if root == "" {
			root = filepath.Join(opts.CWD, DefaultArchiveDirName)
		}
		dir, err := NewArchiveDir(root)
		if err != nil {
			return nil, nil, CompactionReport{}, fmt.Errorf("locate the archive directory: %w", err)
		}
		settings.ArchiveDir = dir
	}
	transferred, archive, report, err := Compact(history, settings)
	if err != nil {
		return nil, nil, CompactionReport{}, err
	}
	if !opts.DryRun {
		if err := archive.Write(); err != nil {
			return nil, nil, report, fmt.Errorf("write the session archive: %w", err)
		}
	}
	return transferred, archive, report, nil
}
