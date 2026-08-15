package store

import "time"

// State is an account's availability as of the last observation.
type State string

const (
	StateAvailable State = "available" // usable now
	StateCooling   State = "cooling"   // short burst limit; will recover on its own
	StateExhausted State = "exhausted" // quota spent until ResetAt
	StateInvalid   State = "invalid"   // credential rejected; needs human attention
)

// Window is one observed rate-limit window (Anthropic 5h/7d, Codex
// primary/secondary). Utilization is a percentage in [0,100].
type Window struct {
	Name        string    `json:"name"`
	Utilization float64   `json:"utilization"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
}

// Health is the tracked state of one account. It is persisted so that a daemon
// restart does not re-probe accounts we already know are exhausted.
type Health struct {
	State State `json:"state"`

	// Until is when a cooling account becomes available again; ResetAt is when
	// an exhausted account's quota refills. Both are absolute times.
	Until   time.Time `json:"until,omitempty"`
	ResetAt time.Time `json:"reset_at,omitempty"`

	Windows []Window `json:"windows,omitempty"`

	LastUsed    time.Time `json:"last_used,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Requests    int64     `json:"requests"`
	Rotations   int64     `json:"rotations"`
	ConsecFails int       `json:"consec_fails"`
}

// Available reports whether the account can be tried at time now. Cooling and
// exhausted accounts self-heal once their deadline passes, so this is a pure
// function of the stored deadlines rather than a background sweeper.
func (h *Health) Available(now time.Time) bool {
	switch h.State {
	case StateInvalid:
		return false
	case StateCooling:
		return !now.Before(h.Until)
	case StateExhausted:
		return !now.Before(h.ResetAt)
	default:
		return true
	}
}

// NextAvailable returns when this account is expected to become usable. Zero
// means "now". Used to decide how long to park when everything is spent.
func (h *Health) NextAvailable(now time.Time) time.Time {
	if h.Available(now) {
		return time.Time{}
	}
	switch h.State {
	case StateCooling:
		return h.Until
	case StateExhausted:
		return h.ResetAt
	default:
		return time.Time{} // invalid never recovers on its own
	}
}
