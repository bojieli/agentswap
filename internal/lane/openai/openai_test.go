package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/store"
)

var testNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func init() { timeNow = func() time.Time { return testNow } }

func resp(status int, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func TestClassify(t *testing.T) {
	cfg := config.Default().Retry

	tests := []struct {
		name        string
		resp        *http.Response
		body        string
		want        lane.Action
		overload    bool
		wantResetIn time.Duration
		wantRetry   time.Duration
	}{
		{name: "success relays", resp: resp(200, nil), want: lane.ActionRelay},
		{name: "401 refreshes", resp: resp(401, nil), want: lane.ActionRefreshAuth},
		{
			name:        "usage_limit_reached rotates using the advertised reset",
			resp:        resp(429, nil),
			body:        `{"error":{"code":"usage_limit_reached","resets_in_seconds":7200}}`,
			want:        lane.ActionRotate,
			wantResetIn: 2 * time.Hour,
		},
		{
			name:        "plan exhaustion via header type rotates",
			resp:        resp(429, map[string]string{"X-Codex-Rate-Limit-Reached-Type": "usage_limit"}),
			want:        lane.ActionRotate,
			wantResetIn: 5 * time.Hour,
		},
		{
			name:      "short rate_limit_exceeded is a burst, stays put",
			resp:      resp(429, map[string]string{"Retry-After": "20"}),
			body:      `{"error":{"code":"rate_limit_exceeded"}}`,
			want:      lane.ActionRetrySame,
			wantRetry: 20 * time.Second,
		},
		{
			name:        "long throttle without a plan code still rotates",
			resp:        resp(429, map[string]string{"Retry-After": "1800"}),
			want:        lane.ActionRotate,
			wantResetIn: 30 * time.Minute,
		},
		{
			name:     "server_overloaded retries the same account",
			resp:     resp(500, nil),
			body:     `{"error":{"code":"server_overloaded"}}`,
			want:     lane.ActionRetrySame,
			overload: true,
		},
		{name: "529 retries as overload", resp: resp(529, nil), want: lane.ActionRetrySame, overload: true},
		{name: "502 retries as overload", resp: resp(502, nil), want: lane.ActionRetrySame, overload: true},
		{name: "400 is fatal", resp: resp(400, nil), want: lane.ActionFatal},
		{
			name:        "credits depleted rotates",
			resp:        resp(429, nil),
			body:        `{"error":{"code":"workspace_member_credits_depleted"}}`,
			want:        lane.ActionRotate,
			wantResetIn: 5 * time.Hour,
		},
	}

	var l Lane
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := l.Classify(tc.resp, []byte(tc.body), cfg, testNow)
			if got.Action != tc.want {
				t.Errorf("action = %v, want %v (reason: %s)", got.Action, tc.want, got.Reason)
			}
			if got.Overload != tc.overload {
				t.Errorf("overload = %v, want %v", got.Overload, tc.overload)
			}
			if tc.wantRetry != 0 && got.RetryAfter != tc.wantRetry {
				t.Errorf("retryAfter = %v, want %v", got.RetryAfter, tc.wantRetry)
			}
			if tc.wantResetIn != 0 {
				want := testNow.Add(tc.wantResetIn)
				if !got.ResetAt.Equal(want) {
					t.Errorf("resetAt = %v, want %v", got.ResetAt, want)
				}
			}
		})
	}
}

func TestUpstreamDependsOnCredentialKind(t *testing.T) {
	var l Lane
	// A subscription and an API key are served by different hosts; picking the
	// wrong one yields a 404 that looks like an outage.
	sub, err := l.Upstream(&store.Account{Kind: store.KindOAuth})
	if err != nil || sub.String() != SubscriptionBaseURL {
		t.Errorf("oauth upstream = %v, %v", sub, err)
	}
	key, err := l.Upstream(&store.Account{Kind: store.KindAPIKey})
	if err != nil || key.String() != APIBaseURL {
		t.Errorf("api key upstream = %v, %v", key, err)
	}
	override, err := l.Upstream(&store.Account{Kind: store.KindAPIKey, BaseURL: "https://example.test/v1"})
	if err != nil || override.String() != "https://example.test/v1" {
		t.Errorf("override upstream = %v, %v", override, err)
	}
}

func TestAuthorizeSetsAccountHeader(t *testing.T) {
	var l Lane
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	req.Header.Set("Authorization", "Bearer stale-from-client")

	l.Authorize(req, &store.Account{
		Kind:             store.KindOAuth,
		AccessToken:      "tok",
		ChatGPTAccountID: "acct-123",
	})

	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	// Dropping this header yields 401/403 even with a valid bearer token.
	if got := req.Header.Get(hdrAccountID); got != "acct-123" {
		t.Errorf("%s = %q, want acct-123", hdrAccountID, got)
	}
}

func TestAuthorizeAPIKeyClearsStaleAccountHeader(t *testing.T) {
	var l Lane
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	req.Header.Set(hdrAccountID, "acct-from-a-previous-account")

	l.Authorize(req, &store.Account{Kind: store.KindAPIKey, APIKey: "sk-proj-x"})

	if got := req.Header.Get("Authorization"); got != "Bearer sk-proj-x" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get(hdrAccountID); got != "" {
		t.Errorf("stale account header survived rotation: %q", got)
	}
}

func TestObserve(t *testing.T) {
	l := New(nil)

	windows := l.Observe(resp(200, map[string]string{
		"X-Codex-Primary-Used-Percent":   "64.5",
		"X-Codex-Primary-Reset-At":       "1786806000",
		"X-Codex-Secondary-Used-Percent": "12",
	}))
	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want primary and secondary", windows)
	}
	if windows[0].Name != "primary" || windows[0].Utilization != 64.5 {
		t.Errorf("primary = %+v", windows[0])
	}
	if windows[0].ResetAt.IsZero() {
		t.Error("primary reset was not read")
	}
	if windows[1].Name != "secondary" || windows[1].Utilization != 12 {
		t.Errorf("secondary = %+v", windows[1])
	}
	// A window with no reset header is still a usable utilization reading.
	if !windows[1].ResetAt.IsZero() {
		t.Errorf("secondary reset = %v, want zero", windows[1].ResetAt)
	}
}

// "Unknown" and "empty" are different: a zero-valued window would look like a
// completely fresh account and defeat the drain threshold.
func TestObserveAbsentHeadersYieldNoWindows(t *testing.T) {
	if got := New(nil).Observe(resp(200, nil)); len(got) != 0 {
		t.Errorf("windows = %+v, want none", got)
	}
	// Codex reports its real quota inside the SSE stream, so most responses
	// carry nothing usable here at all.
	if got := New(nil).Observe(resp(200, map[string]string{"X-Codex-Primary-Used-Percent": "nonsense"})); len(got) != 0 {
		t.Errorf("windows = %+v, want none for an unparseable value", got)
	}
}

func TestID(t *testing.T) {
	if got := New(nil).ID(); got != store.LaneOpenAI {
		t.Errorf("ID() = %q, want %q", got, store.LaneOpenAI)
	}
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer srv.Close()

	restore := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = restore }()

	a := &store.Account{ID: "a", Kind: store.KindOAuth, AccessToken: "old", RefreshToken: "old-refresh"}
	if err := New(srv.Client()).Refresh(context.Background(), a); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if gotBody["grant_type"] != "refresh_token" || gotBody["refresh_token"] != "old-refresh" {
		t.Errorf("request body = %v", gotBody)
	}
	if a.AccessToken != "new-access" {
		t.Errorf("access token = %q", a.AccessToken)
	}
	// Dropping a rotated refresh token strands the account at the next refresh.
	if a.RefreshToken != "new-refresh" {
		t.Errorf("refresh token = %q, want the rotated one", a.RefreshToken)
	}
	if want := testNow.Add(time.Hour).UnixMilli(); a.ExpiresAt != want {
		t.Errorf("expires_at = %d, want %d", a.ExpiresAt, want)
	}
}

func TestRefreshFailures(t *testing.T) {
	t.Run("api key account", func(t *testing.T) {
		a := &store.Account{ID: "a", Kind: store.KindAPIKey, APIKey: "sk-proj"}
		if err := New(nil).Refresh(context.Background(), a); err == nil {
			t.Error("want an error: an api key has nothing to refresh")
		}
	})

	t.Run("no refresh token", func(t *testing.T) {
		a := &store.Account{ID: "a", Kind: store.KindOAuth, AccessToken: "old"}
		if err := New(nil).Refresh(context.Background(), a); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("upstream rejects", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		restore := TokenURL
		TokenURL = srv.URL
		defer func() { TokenURL = restore }()

		a := &store.Account{ID: "a", Kind: store.KindOAuth, RefreshToken: "expired"}
		if err := New(srv.Client()).Refresh(context.Background(), a); err == nil {
			t.Error("want an error")
		}
		// A failed refresh must not leave a half-updated credential behind.
		if a.AccessToken != "" {
			t.Errorf("access token = %q, want it untouched", a.AccessToken)
		}
	})

	t.Run("response without a token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"expires_in":3600}`)
		}))
		defer srv.Close()
		restore := TokenURL
		TokenURL = srv.URL
		defer func() { TokenURL = restore }()

		a := &store.Account{ID: "a", Kind: store.KindOAuth, RefreshToken: "rt"}
		if err := New(srv.Client()).Refresh(context.Background(), a); err == nil {
			t.Error("want an error rather than an account with an empty token")
		}
	})
}
