// Package supervisor resumes a CLI session after a wait too long to hold a
// connection open for.
package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bojieli/agentswap/internal/store"
)

// Ticket records that a request was refused because the whole pool was spent,
// and when it is worth trying again. The proxy writes one; `agentswap run`
// consumes it.
type Ticket struct {
	Lane      store.LaneID `json:"lane"`
	Until     time.Time    `json:"until"`
	WrittenAt time.Time    `json:"written_at"`
}

// TicketDir is where pending tickets live inside the agentswap config dir.
func TicketDir(configDir string) string { return filepath.Join(configDir, "pending") }

func ticketPath(configDir string, lane store.LaneID) string {
	return filepath.Join(TicketDir(configDir), string(lane)+".json")
}

// WriteTicket records a handoff. Failure to write is not fatal to the request
// that triggered it — the user still gets their 503 — so callers log and move
// on rather than surfacing an error about an error.
func WriteTicket(configDir string, t Ticket) error {
	if err := os.MkdirAll(TicketDir(configDir), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ticketPath(configDir, t.Lane), append(b, '\n'), 0o600)
}

// ReadTicket returns the pending ticket for a lane, if any.
func ReadTicket(configDir string, lane store.LaneID) (*Ticket, error) {
	b, err := os.ReadFile(ticketPath(configDir, lane))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Ticket
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse ticket: %w", err)
	}
	return &t, nil
}

// ConsumeTicket removes a ticket once it has been acted on, so a later run
// does not wait for a reset that has already been served.
func ConsumeTicket(configDir string, lane store.LaneID) error {
	err := os.Remove(ticketPath(configDir, lane))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// freshnessGrace widens the window for deciding a ticket belongs to this run.
//
// Ticket timestamps can be written at second granularity while the caller's
// start time carries sub-second precision, so a ticket written in the same
// second as the launch would otherwise be judged stale and the resume silently
// skipped. Erring a few seconds wide costs nothing: a ticket from just before
// this run started is one we would want to act on anyway.
const freshnessGrace = 5 * time.Second

// PendingSince returns the soonest ticket across lanes that belongs to this run
// and whose deadline is still ahead. Tickets left over from an earlier run are
// ignored: resuming on one would wait out a reset that has already passed.
func PendingSince(configDir string, since time.Time) (*Ticket, error) {
	var best *Ticket
	for _, lane := range []store.LaneID{store.LaneAnthropic, store.LaneOpenAI} {
		t, err := ReadTicket(configDir, lane)
		if err != nil || t == nil {
			continue
		}
		if t.WrittenAt.Before(since.Add(-freshnessGrace)) {
			continue
		}
		if !t.Until.After(time.Now()) {
			continue
		}
		if best == nil || t.Until.Before(best.Until) {
			best = t
		}
	}
	return best, nil
}
