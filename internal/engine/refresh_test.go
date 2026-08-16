package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/lane/anthropic"
	"github.com/bojieli/agentswap/internal/store"
)

// refreshHarness wires an engine to a fake token endpoint and a fake upstream
// that only accepts a token the token endpoint actually issued.
type refreshHarness struct {
	engine    *Engine
	store     *store.Store
	exchanges *atomic.Int64
}

func newRefreshHarness(t *testing.T, rotate bool) *refreshHarness {
	t.Helper()

	var exchanges atomic.Int64
	var mu sync.Mutex
	presented := map[string]bool{}
	issued := map[string]bool{}

	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		reused := presented[req.RefreshToken]
		presented[req.RefreshToken] = true
		mu.Unlock()

		if reused && rotate {
			// What a real upstream does with an already-rotated token.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		n := exchanges.Add(1)
		access := fmt.Sprintf("fresh-%d", n)
		mu.Lock()
		issued[access] = true
		mu.Unlock()

		out := map[string]any{"access_token": access, "expires_in": 3600}
		if rotate {
			out["refresh_token"] = fmt.Sprintf("rotated-%d", n)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(auth.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		ok := issued[token]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	restore := anthropic.TokenURL
	anthropic.TokenURL = auth.URL
	t.Cleanup(func() { anthropic.TokenURL = restore })

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Upsert(&store.Account{
		ID: "a", Lane: store.LaneAnthropic, Kind: store.KindOAuth, Label: "a", Enabled: true,
		AccessToken: "stale", RefreshToken: "original",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
		BaseURL:   upstream.URL,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	lanes := map[store.LaneID]lane.Lane{store.LaneAnthropic: anthropic.New(auth.Client())}
	e := New(config.Default(), st, lanes, upstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	return &refreshHarness{engine: e, store: st, exchanges: &exchanges}
}

// TestConcurrentRefreshHappensOnce covers the failure mode that costs an
// account rather than a request: several in-flight requests all noticing an
// expired token and all posting the same refresh token.
//
// Anthropic rotates the refresh token on use, so a second exchange presents a
// token the server has already retired — which retires the account instead of
// renewing it.
func TestConcurrentRefreshHappensOnce(t *testing.T) {
	h := newRefreshHarness(t, true)

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	codes := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf(`{"model":"claude","n":%d}`, i))
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			res, err := h.engine.Execute(context.Background(), store.LaneAnthropic, req, body, SleepWaiter{})
			errs[i] = err
			if res != nil {
				codes[i] = res.Response.StatusCode
				res.Response.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
			continue
		}
		if codes[i] != http.StatusOK {
			t.Errorf("caller %d: status = %d, want 200", i, codes[i])
		}
	}
	if got := h.exchanges.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want exactly 1: concurrent requests must coalesce", got)
	}
}

// TestRefreshPersistsRotatedToken makes sure the renewed credential survives a
// restart. Losing it means the next refresh presents a token the server has
// already retired.
func TestRefreshPersistsRotatedToken(t *testing.T) {
	h := newRefreshHarness(t, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	res, err := h.engine.Execute(context.Background(), store.LaneAnthropic, req, []byte(`{"model":"claude"}`), SleepWaiter{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res.Response.Body.Close()

	got, err := h.store.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != "fresh-1" {
		t.Errorf("stored access token = %q, want the refreshed one", got.AccessToken)
	}
	if got.RefreshToken != "rotated-1" {
		t.Errorf("stored refresh token = %q, want the rotated one", got.RefreshToken)
	}
	if !time.UnixMilli(got.ExpiresAt).After(time.Now()) {
		t.Errorf("stored expiry = %v, want a future time", time.UnixMilli(got.ExpiresAt))
	}
}

// TestAccountsAreCopies pins the invariant the concurrency fix rests on: a
// caller holding an account cannot mutate the pool's copy of it behind the
// store's lock.
func TestAccountsAreCopies(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Upsert(&store.Account{
		ID: "a", Lane: store.LaneAnthropic, Kind: store.KindOAuth, Enabled: true,
		AccessToken: "original", Scopes: []string{"user:inference"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	first, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	first.AccessToken = "tampered"
	first.Scopes[0] = "tampered"

	second, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if second.AccessToken != "original" {
		t.Errorf("access token = %q, want the store to be unaffected by a caller's mutation", second.AccessToken)
	}
	if second.Scopes[0] != "user:inference" {
		t.Errorf("scopes = %v, want the store to be unaffected by a caller's mutation", second.Scopes)
	}
}

// TestAuthRefreshBudgetIsPerAccount covers the account that never gets its
// turn.
//
// auth_refresh_attempts asks whether renewing *this* credential is still worth
// trying, so spending it on the first account condemns the second one
// unrefreshed — recorded as "rejected", which is the one verdict that does not
// heal on its own and the one that sends the user to sign in again. An
// imported credential the CLI stored without an expires_at is exactly this
// case: nothing knows the token is stale until the 401 arrives, so every
// account in the pool needs its own first refresh.
func TestAuthRefreshBudgetIsPerAccount(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": strings.Replace(req.RefreshToken, "refresh-", "fresh-", 1),
			"expires_in":   3600,
		})
	}))
	defer auth.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "fresh-a":
			// a renews fine and is then simply out of quota, which is what
			// moves the request on to b.
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-Reset",
				time.Now().Add(10*time.Hour).Format(time.RFC3339))
			w.WriteHeader(http.StatusTooManyRequests)
		case "fresh-b":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
		}
	}))
	defer upstream.Close()

	restore := anthropic.TokenURL
	anthropic.TokenURL = auth.URL
	defer func() { anthropic.TokenURL = restore }()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i, id := range []string{"a", "b"} {
		if err := st.Upsert(&store.Account{
			ID: id, Lane: store.LaneAnthropic, Kind: store.KindOAuth, Label: id,
			Priority: i, Enabled: true,
			AccessToken: "stale-" + id, RefreshToken: "refresh-" + id,
			BaseURL: upstream.URL,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	cfg := config.Default()
	// Parking would only postpone the question; this test is about which
	// account answers it.
	cfg.Park.Enabled = false
	lanes := map[store.LaneID]lane.Lane{store.LaneAnthropic: anthropic.New(auth.Client())}
	e := New(cfg, st, lanes, upstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := []byte(`{"model":"claude"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	res, err := e.Execute(context.Background(), store.LaneAnthropic, req, body, SleepWaiter{})
	if err != nil {
		t.Fatalf("Execute: %v (health of b: %+v)", err, st.Health("b"))
	}
	defer res.Response.Body.Close()

	if res.Account.ID != "b" {
		t.Errorf("served by %q, want b", res.Account.ID)
	}
	if h := st.Health("b"); h.State == store.StateInvalid {
		t.Errorf("b was marked rejected without a refresh being tried: %q", h.LastError)
	}
}
