package session

import (
	"context"
	"errors"
	"fmt"
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
			return Candidate{}, fmt.Errorf("session id %q is ambiguous across agents; add --from", sessionID)
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

func (m *Manager) Teleport(ctx context.Context, source Candidate, target Agent, opts WriteOptions) (Result, *Session, error) {
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
	if err := history.Validate(); err != nil {
		return Result{}, history, fmt.Errorf("source session is not safely transferable: %w", err)
	}
	result, err := writer.Write(ctx, history, opts)
	if err != nil {
		return Result{}, history, fmt.Errorf("write %s session: %w", target.Display(), err)
	}
	result.Warnings = append(result.Warnings, history.Warnings...)
	return result, history, nil
}
