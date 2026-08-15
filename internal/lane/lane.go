// Package lane defines the protocol-specific seam. A lane is a wire protocol
// plus the rules for authorizing against it and reading its rate-limit
// signals; the engine above it is protocol-agnostic.
package lane

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/store"
)

// Action is what the engine should do with a response.
type Action int

const (
	// ActionRelay passes the response through to the client unchanged.
	ActionRelay Action = iota

	// ActionRetrySame waits RetryAfter and retries on the same account. Used
	// for short throttles and server overload, where switching accounts would
	// throw away a warm prompt cache without helping.
	ActionRetrySame

	// ActionRotate marks the account spent until ResetAt and moves to the next
	// one.
	ActionRotate

	// ActionRefreshAuth renews the token and retries on the same account.
	ActionRefreshAuth

	// ActionFatal relays the response to the client. The request itself is
	// bad, so retrying it anywhere would fail identically.
	ActionFatal
)

func (a Action) String() string {
	switch a {
	case ActionRelay:
		return "relay"
	case ActionRetrySame:
		return "retry-same"
	case ActionRotate:
		return "rotate"
	case ActionRefreshAuth:
		return "refresh-auth"
	case ActionFatal:
		return "fatal"
	}
	return "unknown"
}

// Outcome is the classifier's verdict on one response.
type Outcome struct {
	Action     Action
	RetryAfter time.Duration
	ResetAt    time.Time

	// Overload marks a server-side capacity failure rather than anything to do
	// with this account's quota. These retry without a bound, because the
	// condition is temporary and surfacing it is what strands the agent.
	Overload bool

	Reason string
}

// Lane is one protocol adapter.
type Lane interface {
	ID() store.LaneID

	// Upstream is the base URL for a, honoring any per-account override.
	Upstream(a *store.Account) (*url.URL, error)

	// Authorize replaces whatever credential the client sent with a's. The
	// incoming credential is always discarded: the CLI may have decided to use
	// a token we know nothing about, and we route on our own pool regardless.
	Authorize(req *http.Request, a *store.Account)

	// Refresh renews a's OAuth token in place.
	Refresh(ctx context.Context, a *store.Account) error

	// Observe reads quota windows off any response, including successful ones.
	// Seeing utilization climb on 200s is what allows rotating before a 429
	// rather than after.
	Observe(resp *http.Response) []store.Window

	// Classify decides what to do. body is the already-read error body for
	// non-2xx responses and nil for 2xx, whose body may be an open stream.
	//
	// now is passed in rather than read from the clock so that every deadline
	// the engine acts on comes from one consistent source of time.
	Classify(resp *http.Response, body []byte, cfg config.Retry, now time.Time) Outcome
}

// RetryAfterHeader reads a Retry-After header in either of its legal forms: delay
// seconds, or an HTTP date.
func RetryAfterHeader(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := parseFloat(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
