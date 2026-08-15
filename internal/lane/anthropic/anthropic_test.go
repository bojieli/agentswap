package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		name     string
		resp     *http.Response
		body     string
		want     lane.Action
		overload bool
		// wantResetIn, when non-zero, asserts the rotation deadline.
		wantResetIn time.Duration
		wantRetry   time.Duration
	}{
		{
			name: "success relays",
			resp: resp(200, nil),
			want: lane.ActionRelay,
		},
		{
			name: "401 triggers refresh",
			resp: resp(401, nil),
			want: lane.ActionRefreshAuth,
		},
		{
			name: "403 triggers refresh",
			resp: resp(403, nil),
			want: lane.ActionRefreshAuth,
		},
		{
			name:      "429 with short retry-after is a burst limit, stays put",
			resp:      resp(429, map[string]string{"Retry-After": "30"}),
			want:      lane.ActionRetrySame,
			wantRetry: 30 * time.Second,
		},
		{
			name:        "429 with long retry-after is quota exhaustion, rotates",
			resp:        resp(429, map[string]string{"Retry-After": "3600"}),
			want:        lane.ActionRotate,
			wantResetIn: time.Hour,
		},
		{
			name: "429 with explicit rejected status rotates even on a short retry-after",
			resp: resp(429, map[string]string{
				"Retry-After":                          "5",
				"Anthropic-Ratelimit-Unified-Status":   "rejected",
				"Anthropic-Ratelimit-Unified-5h-Reset": testNow.Add(2 * time.Hour).Format(time.RFC3339),
			}),
			want:        lane.ActionRotate,
			wantResetIn: 2 * time.Hour,
		},
		{
			name: "rejected status uses Retry-After rather than the conservative guess",
			resp: resp(429, map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "rejected",
				"Retry-After":                        "7200",
			}),
			want:        lane.ActionRotate,
			wantResetIn: 2 * time.Hour,
		},
		{
			name:        "429 with no timing information rotates on a conservative 5h guess",
			resp:        resp(429, nil),
			want:        lane.ActionRotate,
			wantResetIn: 5 * time.Hour,
		},
		{
			name: "429 with only a near reset header is treated as a burst limit",
			resp: resp(429, map[string]string{
				"Anthropic-Ratelimit-Unified-Reset": testNow.Add(45 * time.Second).Format(time.RFC3339),
			}),
			want:      lane.ActionRetrySame,
			wantRetry: 45 * time.Second,
		},
		{
			name:     "529 retries the same account as an overload",
			resp:     resp(529, nil),
			want:     lane.ActionRetrySame,
			overload: true,
		},
		{
			name:     "503 retries as an overload",
			resp:     resp(503, nil),
			want:     lane.ActionRetrySame,
			overload: true,
		},
		{
			name:     "500 carrying overloaded_error is treated as an overload",
			resp:     resp(500, nil),
			body:     `{"error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:     lane.ActionRetrySame,
			overload: true,
		},
		{
			name:     "408 retries as an overload",
			resp:     resp(408, nil),
			want:     lane.ActionRetrySame,
			overload: true,
		},
		{
			name: "400 is fatal and must not burn another account",
			resp: resp(400, nil),
			body: `{"error":{"type":"invalid_request_error","message":"bad"}}`,
			want: lane.ActionFatal,
		},
		{
			name: "404 is fatal",
			resp: resp(404, nil),
			want: lane.ActionFatal,
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

func TestClassifyBurstCutoffBoundary(t *testing.T) {
	cfg := config.Default().Retry // BurstCutoff = 120s
	var l Lane

	// Exactly at the cutoff must stay on the same account: the boundary is
	// inclusive, so a 120s throttle does not needlessly discard a warm cache.
	got := l.Classify(resp(429, map[string]string{"Retry-After": "120"}), nil, cfg, testNow)
	if got.Action != lane.ActionRetrySame {
		t.Errorf("at cutoff: action = %v, want retry-same", got.Action)
	}
	// One second past it is quota exhaustion.
	got = l.Classify(resp(429, map[string]string{"Retry-After": "121"}), nil, cfg, testNow)
	if got.Action != lane.ActionRotate {
		t.Errorf("past cutoff: action = %v, want rotate", got.Action)
	}
}

func TestObserve(t *testing.T) {
	var l Lane
	reset5h := testNow.Add(90 * time.Minute)
	got := l.Observe(resp(200, map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "61",
		"Anthropic-Ratelimit-Unified-5h-Reset":       reset5h.Format(time.RFC3339),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "38.5",
	}))
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2: %+v", len(got), got)
	}
	if got[0].Name != "5h" || got[0].Utilization != 61 || !got[0].ResetAt.Equal(reset5h) {
		t.Errorf("5h window = %+v", got[0])
	}
	if got[1].Name != "7d" || got[1].Utilization != 38.5 {
		t.Errorf("7d window = %+v", got[1])
	}
}

func TestObserveAbsentHeadersYieldNoWindows(t *testing.T) {
	var l Lane
	// "Unknown" must not be recorded as "0% used", which would look like a
	// completely fresh account and defeat predictive rotation.
	if got := l.Observe(resp(200, nil)); len(got) != 0 {
		t.Errorf("got %d windows, want 0: %+v", len(got), got)
	}
}

func TestAuthorizeOAuth(t *testing.T) {
	var l Lane
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Api-Key", "sk-stale-from-client")
	req.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")

	l.Authorize(req, &store.Account{Kind: store.KindOAuth, AccessToken: "tok-abc"})

	if got := req.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("stale client api key survived: %q", got)
	}
	beta := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(beta, oauthBeta) {
		t.Errorf("Anthropic-Beta = %q, want it to include %q", beta, oauthBeta)
	}
	if !strings.Contains(beta, "context-1m-2025-08-07") {
		t.Errorf("Anthropic-Beta = %q, dropped the client's own beta flag", beta)
	}
}

func TestAuthorizeAPIKeyDropsOAuthBeta(t *testing.T) {
	var l Lane
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer stale")
	req.Header.Set("Anthropic-Beta", oauthBeta+",context-1m-2025-08-07")

	l.Authorize(req, &store.Account{Kind: store.KindAPIKey, APIKey: "sk-ant-real"})

	if got := req.Header.Get("X-Api-Key"); got != "sk-ant-real" {
		t.Errorf("X-Api-Key = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("stale bearer survived: %q", got)
	}
	beta := req.Header.Get("Anthropic-Beta")
	if strings.Contains(beta, oauthBeta) {
		t.Errorf("oauth beta must not accompany an api key: %q", beta)
	}
	if !strings.Contains(beta, "context-1m-2025-08-07") {
		t.Errorf("unrelated beta flag was dropped: %q", beta)
	}
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	a := &store.Account{ID: "x", Kind: store.KindOAuth, RefreshToken: "old-refresh"}
	l := New(srv.Client())
	if err := l.Refresh(context.Background(), a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "new-access" {
		t.Errorf("access token = %q", a.AccessToken)
	}
	// Losing a rotated refresh token strands the account at the next refresh.
	if a.RefreshToken != "new-refresh" {
		t.Errorf("refresh token = %q, want the rotated value", a.RefreshToken)
	}
	if want := testNow.Add(time.Hour).UnixMilli(); a.ExpiresAt != want {
		t.Errorf("expiresAt = %d, want %d", a.ExpiresAt, want)
	}
}

func TestRefreshKeepsOldRefreshTokenWhenServerOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a2", "expires_in": 60})
	}))
	defer srv.Close()
	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	a := &store.Account{ID: "x", Kind: store.KindOAuth, RefreshToken: "keep-me"}
	if err := New(srv.Client()).Refresh(context.Background(), a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.RefreshToken != "keep-me" {
		t.Errorf("refresh token = %q, want it preserved", a.RefreshToken)
	}
}

// The beta header is legal repeated as well as comma-joined, and clients use
// both. Dropping the extra lines silently disables whichever features the
// client asked for there.
func TestAuthorizeKeepsEveryBetaFlag(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Add("Anthropic-Beta", "context-1m-2025-08-07")
	req.Header.Add("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")

	(&Lane{}).Authorize(req, &store.Account{Kind: store.KindOAuth, AccessToken: "t"})

	got := req.Header.Get("Anthropic-Beta")
	for _, want := range []string{
		"context-1m-2025-08-07",
		"fine-grained-tool-streaming-2025-05-14",
		oauthBeta,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Anthropic-Beta = %q, want it to include %q", got, want)
		}
	}
}

func TestAuthorizeAPIKeyKeepsEveryBetaFlagButOurs(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Add("Anthropic-Beta", "context-1m-2025-08-07")
	req.Header.Add("Anthropic-Beta", oauthBeta)
	req.Header.Add("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")

	(&Lane{}).Authorize(req, &store.Account{Kind: store.KindAPIKey, APIKey: "sk-ant"})

	got := req.Header.Get("Anthropic-Beta")
	if strings.Contains(got, oauthBeta) {
		t.Errorf("Anthropic-Beta = %q, want the subscription flag dropped", got)
	}
	for _, want := range []string{"context-1m-2025-08-07", "fine-grained-tool-streaming-2025-05-14"} {
		if !strings.Contains(got, want) {
			t.Errorf("Anthropic-Beta = %q, want it to include %q", got, want)
		}
	}
	// One header, not three, is what the upstream sees.
	if n := len(req.Header.Values("Anthropic-Beta")); n != 1 {
		t.Errorf("Anthropic-Beta has %d values, want them merged into one", n)
	}
}
