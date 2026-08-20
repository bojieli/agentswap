// Package lane defines the protocol-specific seam. A lane is a wire protocol
// plus the rules for authorizing against it and reading its rate-limit
// signals; the engine above it is protocol-agnostic.
package lane

import (
	"context"
	"fmt"
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
	// non-2xx responses, and a bounded head sample of the body for 2xx ones:
	// a successful status line is a claim, not proof, and some upstreams
	// deliver a terminal failure in-band — a JSON error envelope, or a
	// standard terminal stream event. Nothing has reached the client at this
	// point, so an in-band failure classifies exactly like its HTTP-status
	// equivalent.
	//
	// now is passed in rather than read from the clock so that every deadline
	// the engine acts on comes from one consistent source of time.
	Classify(resp *http.Response, body []byte, cfg config.Retry, now time.Time) Outcome
}

// overloadSignals are the standard error types and codes that mean "the
// server is at capacity", in whichever words a vendor or gateway uses. They
// are retried without a bound for the same reason 529s are: the condition is
// temporary, and surfacing it is what strands the agent.
var overloadSignals = map[string]bool{
	"overloaded_error":          true,
	"service_unavailable_error": true,
	"server_error":              true,
	"api_error":                 true,
	"internal_error":            true,
	"internal_server_error":     true,
	"server_overloaded":         true,
	"server_is_overloaded":      true,
	"overloaded":                true,
}

// planExhausted lists the error codes that mean "this account is out of
// quota", as opposed to "you are going too fast".
var planExhausted = map[string]bool{
	"usage_limit_reached":                  true,
	"usage_limit_exceeded":                 true,
	"workspace_owner_usage_limit_reached":  true,
	"workspace_member_usage_limit_reached": true,
	"workspace_member_credits_depleted":    true,
	"insufficient_quota":                   true,
}

// IsPlanExhausted reports whether code names a spent quota window rather than
// a momentary throttle.
func IsPlanExhausted(code string) bool { return planExhausted[code] }

// authSignals are the standard error types and codes that mean the credential
// itself was refused.
var authSignals = map[string]bool{
	"authentication_error": true,
	"permission_error":     true,
	"invalid_api_key":      true,
	"unauthorized":         true,
}

// ClassifyInBand maps a failure delivered inside a response the status line
// called successful onto the same action its HTTP-status equivalent would
// produce. The standard terminal stream events and error envelopes carry the
// same taxonomy as the status-code errors — both lanes' clients understand
// them natively — so the rules are shared rather than per-lane.
func ClassifyInBand(e InBandError, cfg config.Retry, now time.Time) Outcome {
	switch {
	case overloadSignals[e.Type] || overloadSignals[e.Code]:
		return Outcome{Action: ActionRetrySame, Overload: true, Reason: "upstream overloaded (in-stream)"}

	case planExhausted[e.Code] || planExhausted[e.Type]:
		at := now.Add(5 * time.Hour)
		if e.ResetsInSeconds > 0 {
			at = now.Add(floatSeconds(e.ResetsInSeconds))
		}
		return Outcome{Action: ActionRotate, ResetAt: at, Reason: "plan quota exhausted (in-stream)"}

	case e.Type == "rate_limit_error" || e.Code == "rate_limit_exceeded":
		wait := floatSeconds(e.ResetsInSeconds)
		if wait > 0 && wait <= cfg.BurstCutoff.D() {
			return Outcome{
				Action:     ActionRetrySame,
				RetryAfter: wait,
				Reason:     fmt.Sprintf("burst limit, retry in %s (in-stream)", wait.Round(time.Second)),
			}
		}
		at := now.Add(5 * time.Hour)
		if wait > 0 {
			at = now.Add(wait)
		}
		return Outcome{Action: ActionRotate, ResetAt: at, Reason: "rate limited (in-stream)"}

	case authSignals[e.Type] || authSignals[e.Code]:
		reason := "credential refused (in-stream)"
		if e.Message != "" {
			reason += ": " + e.Message
		}
		return Outcome{Action: ActionRefreshAuth, Reason: reason}

	default:
		// Anything else is the client's own error, which fails identically
		// anywhere — hand it back. On a 2xx the engine relays the original
		// stream, so the client receives the standard event it understands.
		reason := e.Message
		if reason == "" {
			reason = e.Type
		}
		if reason == "" {
			reason = e.Code
		}
		if reason == "" {
			reason = "in-stream error"
		}
		return Outcome{Action: ActionFatal, Reason: reason}
	}
}

// floatSeconds converts an upstream figure to a duration without dropping its
// fraction: a sub-second burst limit is the shortest wait there is, and
// truncating it to zero reads as "the upstream said nothing about timing".
func floatSeconds(v float64) time.Duration {
	return time.Duration(v * float64(time.Second))
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
