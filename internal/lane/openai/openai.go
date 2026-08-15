// Package openai implements the Responses-API lane used by Codex.
//
// Codex speaks only wire_api = "responses", so this lane forwards request
// bodies untouched and never translates between protocols.
package openai

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

// Upstream roots. A ChatGPT subscription and a metered API key are served by
// different hosts, so the default depends on the credential kind.
const (
	SubscriptionBaseURL = "https://chatgpt.com/backend-api/codex"
	APIBaseURL          = "https://api.openai.com/v1"
)

// OAuth parameters for the Codex CLI public client.
var (
	TokenURL = "https://auth.openai.com/oauth/token"
	ClientID = "app_69a1d78e929881919bba0dbda1f6436d"
)

// hdrRateLimitType carries why a 429 was issued, which is the cheapest way to
// tell a short throttle from a spent plan.
const hdrRateLimitType = "X-Codex-Rate-Limit-Reached-Type"

// hdrAccountID identifies the ChatGPT workspace. Omitting it yields 401/403
// even when the bearer token is perfectly valid.
const hdrAccountID = "Chatgpt-Account-Id"

var timeNow = time.Now

// Lane implements lane.Lane for the OpenAI Responses API.
type Lane struct {
	HTTP *http.Client
}

func New(c *http.Client) *Lane {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &Lane{HTTP: c}
}

func (*Lane) ID() store.LaneID { return store.LaneOpenAI }

func (*Lane) Upstream(a *store.Account) (*url.URL, error) {
	raw := a.BaseURL
	if raw == "" {
		if a.Kind == store.KindOAuth {
			raw = SubscriptionBaseURL
		} else {
			raw = APIBaseURL
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("account %s: bad base_url %q: %w", a.ID, raw, err)
	}
	return u, nil
}

// Authorize replaces the client's credential with the pool's.
func (*Lane) Authorize(req *http.Request, a *store.Account) {
	req.Header.Del("Authorization")
	req.Header.Del(hdrAccountID)

	switch a.Kind {
	case store.KindOAuth:
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		if a.ChatGPTAccountID != "" {
			req.Header.Set(hdrAccountID, a.ChatGPTAccountID)
		}
	default:
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Refresh renews the ChatGPT access token in place.
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

// Observe reads what quota information Codex exposes in headers.
//
// Codex's richer snapshot (primary/secondary windows with used_percent and
// resets_at) travels inside the SSE response stream rather than in headers.
// Reading it would mean parsing a body this proxy otherwise forwards
// byte-for-byte, so this lane is reactive by default: it learns an account is
// spent from the 429 rather than before it. See the README for the tradeoff.
func (*Lane) Observe(resp *http.Response) []store.Window {
	var out []store.Window
	if v := resp.Header.Get("X-Codex-Primary-Used-Percent"); v != "" {
		if u, ok := lane.ParsePercent(v); ok {
			w := store.Window{Name: "primary", Utilization: u}
			if t, ok := lane.ParseTimestamp(resp.Header.Get("X-Codex-Primary-Reset-At")); ok {
				w.ResetAt = t
			}
			out = append(out, w)
		}
	}
	if v := resp.Header.Get("X-Codex-Secondary-Used-Percent"); v != "" {
		if u, ok := lane.ParsePercent(v); ok {
			w := store.Window{Name: "secondary", Utilization: u}
			if t, ok := lane.ParseTimestamp(resp.Header.Get("X-Codex-Secondary-Reset-At")); ok {
				w.ResetAt = t
			}
			out = append(out, w)
		}
	}
	return out
}

// errorBody covers both the OpenAI API error envelope and the plan-level
// errors the Codex backend returns.
type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`

		// The subscription backend reports when the plan window refills.
		ResetsInSeconds  float64 `json:"resets_in_seconds"`
		ResetAfterSecond float64 `json:"reset_after_seconds"`
	} `json:"error"`
	Type            string  `json:"type"`
	ResetsInSeconds float64 `json:"resets_in_seconds"`
}

func parseError(body []byte) errorBody {
	var e errorBody
	if len(body) > 0 {
		_ = json.Unmarshal(body, &e)
	}
	return e
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

// Classify decides what to do with a response.
func (*Lane) Classify(resp *http.Response, body []byte, cfg config.Retry, now time.Time) lane.Outcome {
	e := parseError(body)
	code := firstNonEmpty(e.Error.Code, e.Error.Type, e.Type)

	if code == "server_overloaded" {
		return lane.Outcome{Action: lane.ActionRetrySame, Overload: true, Reason: "upstream overloaded"}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return lane.Outcome{Action: lane.ActionRelay}

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return lane.Outcome{
			Action: lane.ActionRefreshAuth,
			Reason: fmt.Sprintf("upstream returned %d", resp.StatusCode),
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		return classify429(resp, e, code, now, cfg)

	case resp.StatusCode == 529:
		return lane.Outcome{Action: lane.ActionRetrySame, Overload: true, Reason: "upstream overloaded (529)"}

	case resp.StatusCode == http.StatusRequestTimeout:
		return lane.Outcome{Action: lane.ActionRetrySame, Overload: true, Reason: "upstream request timeout"}

	case resp.StatusCode >= 500:
		return lane.Outcome{
			Action:   lane.ActionRetrySame,
			Overload: true,
			Reason:   fmt.Sprintf("upstream %d", resp.StatusCode),
		}

	default:
		return lane.Outcome{
			Action: lane.ActionFatal,
			Reason: fmt.Sprintf("client error %d", resp.StatusCode),
		}
	}
}

func classify429(resp *http.Response, e errorBody, code string, now time.Time, cfg config.Retry) lane.Outcome {
	reachedType := strings.ToLower(strings.TrimSpace(resp.Header.Get(hdrRateLimitType)))

	// Prefer the server's own reset figure over any inference.
	resetIn := firstPositive(e.Error.ResetsInSeconds, e.Error.ResetAfterSecond, e.ResetsInSeconds)

	// A named plan-exhaustion code is decisive regardless of timing.
	if planExhausted[code] || strings.Contains(reachedType, "usage") || strings.Contains(reachedType, "plan") {
		at := now.Add(5 * time.Hour)
		if resetIn > 0 {
			at = now.Add(time.Duration(resetIn) * time.Second)
		} else if ra, ok := lane.RetryAfterHeader(resp.Header, now); ok {
			at = now.Add(ra)
		}
		return lane.Outcome{Action: lane.ActionRotate, ResetAt: at, Reason: "plan quota exhausted"}
	}

	wait := time.Duration(resetIn) * time.Second
	if wait == 0 {
		if ra, ok := lane.RetryAfterHeader(resp.Header, now); ok {
			wait = ra
		}
	}

	if wait > 0 {
		if wait <= cfg.BurstCutoff.D() {
			return lane.Outcome{
				Action:     lane.ActionRetrySame,
				RetryAfter: wait,
				Reason:     fmt.Sprintf("burst limit, retry in %s", wait.Round(time.Second)),
			}
		}
		return lane.Outcome{
			Action:  lane.ActionRotate,
			ResetAt: now.Add(wait),
			Reason:  fmt.Sprintf("quota exhausted, resets in %s", wait.Round(time.Second)),
		}
	}

	// No timing at all: rotate rather than guess, since we cannot see the
	// shape of the limit we just hit.
	return lane.Outcome{
		Action:  lane.ActionRotate,
		ResetAt: now.Add(5 * time.Hour),
		Reason:  "rate limited (no reset advertised)",
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
