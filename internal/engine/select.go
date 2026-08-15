package engine

import (
	"time"

	"github.com/bojieli/agentswap/internal/store"
)

// candidate pairs an account with the health snapshot used to judge it.
type candidate struct {
	acct   *store.Account
	health store.Health
}

// selectAccount picks the account to try next.
//
// Preference order: the conversation's previous account (warm prompt cache),
// then the store's own ordering (subscriptions before API keys, then by
// priority). Accounts already tried for this request are skipped.
//
// Selection runs in two passes. The first excludes accounts whose observed
// utilization is above the drain threshold, so a nearly-spent account is
// retired before it starts returning 429s. If that leaves nothing, the second
// pass puts them back: a drained account that has not actually been refused is
// still better than parking the request.
func (e *Engine) selectAccount(laneID store.LaneID, convKey string, tried map[string]bool, now time.Time) *store.Account {
	accounts := e.store.Accounts(laneID)

	var usable []candidate
	for _, a := range accounts {
		if tried[a.ID] {
			continue
		}
		h := e.store.Health(a.ID)
		if !h.Available(now) {
			continue
		}
		usable = append(usable, candidate{acct: a, health: h})
	}
	if len(usable) == 0 {
		return nil
	}

	var fresh []candidate
	for _, c := range usable {
		if !e.drained(c.health, now) {
			fresh = append(fresh, c)
		}
	}
	pool := fresh
	if len(pool) == 0 {
		pool = usable
	}

	if e.cfg.Rotation.Sticky {
		if id, ok := e.sticky.get(convKey, now); ok {
			for _, c := range pool {
				if c.acct.ID == id {
					return c.acct
				}
			}
		}
	}
	return pool[0].acct
}

// drained reports whether any observed window is at or above the configured
// utilization threshold and has not yet reset.
func (e *Engine) drained(h store.Health, now time.Time) bool {
	for _, w := range h.Windows {
		if w.Utilization < e.cfg.Rotation.DrainAbove {
			continue
		}
		// A stale reading from a window that has since refilled says nothing
		// about the account's current headroom.
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			continue
		}
		return true
	}
	return false
}

// nextAvailable returns the soonest time any account in the lane recovers, and
// whether such a time exists. Accounts marked invalid never recover on their
// own and are excluded.
func (e *Engine) nextAvailable(laneID store.LaneID, now time.Time) (time.Time, bool) {
	var best time.Time
	for _, a := range e.store.Accounts(laneID) {
		h := e.store.Health(a.ID)
		if h.Available(now) {
			return now, true
		}
		t := h.NextAvailable(now)
		if t.IsZero() {
			continue
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return best, !best.IsZero()
}
