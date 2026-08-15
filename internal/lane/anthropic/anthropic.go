// Package anthropic implements the Messages-API lane used by Claude Code.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/store"
)

// DefaultBaseURL is Anthropic's API root. Per-account BaseURL overrides it,
// which is how same-protocol third-party providers are supported.
const DefaultBaseURL = "https://api.anthropic.com"

// OAuth parameters. These are endpoints and a *public* client id — the values
// Claude Code itself ships — not secrets. They are variables so that a
// change upstream can be worked around by config without a new release.
var (
	TokenURL = "https://console.anthropic.com/v1/oauth/token"
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// oauthBeta must accompany a subscription bearer token. Claude Code sends it
// natively, but a request that reached us in API-key mode will not have it, so
// the lane adds it rather than trusting the client.
const oauthBeta = "oauth-2025-04-20"

// Rate-limit headers, as emitted by Claude Code's upstream. These arrive on
// successful responses too, which is what makes rotating before a 429 possible.
const (
	hdrStatus        = "Anthropic-Ratelimit-Unified-Status"
	hdrReset         = "Anthropic-Ratelimit-Unified-Reset"
	hdr5hReset       = "Anthropic-Ratelimit-Unified-5h-Reset"
	hdr5hUtilization = "Anthropic-Ratelimit-Unified-5h-Utilization"
	hdr7dReset       = "Anthropic-Ratelimit-Unified-7d-Reset"
	hdr7dUtilization = "Anthropic-Ratelimit-Unified-7d-Utilization"
	statusRejected   = "rejected"
)

// timeNow is swappable in tests.
var timeNow = time.Now

// Lane implements lane.Lane for the Anthropic Messages API.
type Lane struct {
	HTTP *http.Client
}

func New(c *http.Client) *Lane {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &Lane{HTTP: c}
}

func (*Lane) ID() store.LaneID { return store.LaneAnthropic }

func (*Lane) Upstream(a *store.Account) (*url.URL, error) {
	raw := a.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("account %s: bad base_url %q: %w", a.ID, raw, err)
	}
	return u, nil
}

// Authorize strips whatever credential arrived and installs the pool's. Both
// header families are cleared first: leaving a stale x-api-key alongside a
// bearer token makes the upstream pick for us, and it may not pick ours.
func (*Lane) Authorize(req *http.Request, a *store.Account) {
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")

	betas := betaFlags(req.Header)

	switch a.Kind {
	case store.KindOAuth:
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		req.Header.Set("Anthropic-Beta", withBeta(betas, oauthBeta))
	default:
		req.Header.Set("X-Api-Key", a.APIKey)
		// A subscription beta flag on an API-key request is at best ignored and
		// at worst rejected, so drop it when we are not using a bearer token.
		if b := removeBeta(betas, oauthBeta); b != "" {
			req.Header.Set("Anthropic-Beta", b)
		} else {
			req.Header.Del("Anthropic-Beta")
		}
	}
}

// betaFlags collects every beta the client asked for.
//
// The header is legal in both forms — repeated lines, or one comma-joined
// value — and clients use both. Reading only the first line and then Set-ting
// over it would silently drop whichever features the client requested in the
// rest, which surfaces later as a capability mysteriously not working through
// the proxy.
func betaFlags(h http.Header) string {
	return strings.Join(h.Values("Anthropic-Beta"), ",")
}

func withBeta(existing, want string) string {
	for _, p := range strings.Split(existing, ",") {
		if strings.TrimSpace(p) == want {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return want
	}
	return existing + "," + want
}

func removeBeta(existing, drop string) string {
	var keep []string
	for _, p := range strings.Split(existing, ",") {
		if t := strings.TrimSpace(p); t != "" && t != drop {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, ",")
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Refresh exchanges the refresh token for a new access token, updating a in
// place. The refresh token is rotated when the server returns a new one;
// dropping that would strand the account at the next refresh.
func (l *Lane) Refresh(ctx context.Context, a *store.Account) error {
	if a.Kind != store.KindOAuth {
		return fmt.Errorf("account %s: not an oauth account", a.ID)
	}
	if a.RefreshToken == "" {
		return fmt.Errorf("account %s: no refresh token; re-run `agentswap login`", a.ID)
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": a.RefreshToken,
		"client_id":     ClientID,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("account %s: refresh request: %w", a.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("account %s: refresh failed with %s", a.ID, resp.Status)
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("account %s: decode refresh response: %w", a.ID, err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("account %s: refresh returned no access token", a.ID)
	}

	a.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		a.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		a.ExpiresAt = timeNow().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	}
	return nil
}

// Observe extracts the 5h and 7d windows. Absent headers yield no window
// rather than a zero-valued one, so "unknown" is never mistaken for "empty".
func (*Lane) Observe(resp *http.Response) []store.Window {
	var out []store.Window
	add := func(name, utilHdr, resetHdr string) {
		u, okU := lane.ParsePercent(resp.Header.Get(utilHdr))
		r, okR := lane.ParseTimestamp(resp.Header.Get(resetHdr))
		if !okU && !okR {
			return
		}
		w := store.Window{Name: name}
		if okU {
			w.Utilization = u
		}
		if okR {
			w.ResetAt = r
		}
		out = append(out, w)
	}
	add("5h", hdr5hUtilization, hdr5hReset)
	add("7d", hdr7dUtilization, hdr7dReset)
	return out
}

// resetTime picks the soonest reset the response advertises. The unified reset
// header wins when present; otherwise the earliest window reset is used.
func resetTime(resp *http.Response) (time.Time, bool) {
	if t, ok := lane.ParseTimestamp(resp.Header.Get(hdrReset)); ok {
		return t, true
	}
	var best time.Time
	for _, h := range []string{hdr5hReset, hdr7dReset} {
		if t, ok := lane.ParseTimestamp(resp.Header.Get(h)); ok {
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
	}
	return best, !best.IsZero()
}

// errorBody is the shape of an Anthropic API error.
type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func errorType(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error.Type
}

// Classify decides what to do with a response.
func (*Lane) Classify(resp *http.Response, body []byte, cfg config.Retry, now time.Time) lane.Outcome {
	etype := errorType(body)

	// An overloaded upstream sometimes arrives dressed as a 500 rather than a
	// 529, so trust the body's type when it says so.
	if etype == "overloaded_error" {
		return lane.Outcome{
			Action:   lane.ActionRetrySame,
			Overload: true,
			Reason:   "upstream overloaded",
		}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return lane.Outcome{Action: lane.ActionRelay}

	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Handed straight back. Following it ourselves would send the pool's
		// credential to a host nobody configured.
		return lane.Outcome{
			Action: lane.ActionFatal,
			Reason: fmt.Sprintf("upstream redirect (%d)", resp.StatusCode),
		}

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return lane.Outcome{
			Action: lane.ActionRefreshAuth,
			Reason: fmt.Sprintf("upstream returned %d", resp.StatusCode),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		return classify429(resp, now, cfg)

	case resp.StatusCode == 529:
		return lane.Outcome{
			Action:   lane.ActionRetrySame,
			Overload: true,
			Reason:   "upstream overloaded (529)",
		}

	case resp.StatusCode == http.StatusRequestTimeout:
		return lane.Outcome{
			Action:   lane.ActionRetrySame,
			Overload: true,
			Reason:   "upstream request timeout",
		}

	case resp.StatusCode >= 500:
		return lane.Outcome{
			Action:   lane.ActionRetrySame,
			Overload: true,
			Reason:   fmt.Sprintf("upstream %d", resp.StatusCode),
		}

	default:
		// Any other 4xx is a problem with the request itself. Retrying it on a
		// different account would fail identically and burn quota doing it.
		return lane.Outcome{
			Action: lane.ActionFatal,
			Reason: fmt.Sprintf("client error %d", resp.StatusCode),
		}
	}
}

// classify429 separates a short throttle from a spent quota. Getting this
// distinction right is what protects the prompt cache: a burst limit lifts in
// seconds and should reuse the same account, while a spent 5h window must
// rotate immediately.
func classify429(resp *http.Response, now time.Time, cfg config.Retry) lane.Outcome {
	reset, hasReset := resetTime(resp)
	ra, hasRA := lane.RetryAfterHeader(resp.Header, now)
	status := strings.ToLower(strings.TrimSpace(resp.Header.Get(hdrStatus)))

	// An explicit rejection is unambiguous: this account's window is spent.
	// Take the reset time from whatever the server actually told us, in order
	// of precision. Falling back to the conservative guess when a real figure
	// is available would park the request for hours longer than necessary.
	if status == statusRejected {
		switch {
		case hasReset:
			return lane.Outcome{Action: lane.ActionRotate, ResetAt: reset, Reason: "quota exhausted (status=rejected)"}
		case hasRA:
			return lane.Outcome{Action: lane.ActionRotate, ResetAt: now.Add(ra), Reason: "quota exhausted (status=rejected)"}
		default:
			return rotate(time.Time{}, false, now, cfg, "quota exhausted (status=rejected)")
		}
	}

	// Otherwise infer from how long we are told to wait. A short delay is a
	// per-minute throttle; a long one means the window itself has to refill.
	if hasRA {
		if ra <= cfg.BurstCutoff.D() {
			return lane.Outcome{
				Action:     lane.ActionRetrySame,
				RetryAfter: ra,
				Reason:     fmt.Sprintf("burst limit, retry in %s", ra.Round(time.Second)),
			}
		}
		at := now.Add(ra)
		if hasReset && reset.After(at) {
			at = reset
		}
		return lane.Outcome{
			Action:  lane.ActionRotate,
			ResetAt: at,
			Reason:  fmt.Sprintf("quota exhausted, resets in %s", ra.Round(time.Second)),
		}
	}

	if hasReset {
		if d := reset.Sub(now); d <= cfg.BurstCutoff.D() {
			return lane.Outcome{
				Action:     lane.ActionRetrySame,
				RetryAfter: d,
				Reason:     fmt.Sprintf("burst limit, window resets in %s", d.Round(time.Second)),
			}
		}
		return lane.Outcome{
			Action:  lane.ActionRotate,
			ResetAt: reset,
			Reason:  "quota exhausted",
		}
	}

	// A 429 with no timing information at all. Rotating is the safer guess:
	// staying put risks hammering a limit we cannot see the shape of.
	return rotate(time.Time{}, false, now, cfg, "rate limited (no reset advertised)")
}

func rotate(reset time.Time, hasReset bool, now time.Time, cfg config.Retry, reason string) lane.Outcome {
	if !hasReset || reset.Before(now) {
		// Assume a 5h window when the server does not say. Overestimating
		// merely delays a retry; underestimating re-triggers the limit.
		reset = now.Add(5 * time.Hour)
	}
	return lane.Outcome{Action: lane.ActionRotate, ResetAt: reset, Reason: reason}
}
