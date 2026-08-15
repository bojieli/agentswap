package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/lane/anthropic"
	"github.com/bojieli/agentswap/internal/store"
)

var base = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// fakeClock lets parking tests cover multi-hour waits instantly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeWaiter advances the clock instead of sleeping, and records what it was
// asked to wait for so tests can assert on the schedule.
type fakeWaiter struct {
	clock *fakeClock
	mu    sync.Mutex
	waits []time.Duration
}

func (w *fakeWaiter) Wait(ctx context.Context, d time.Duration, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	w.clock.advance(d)
	return nil
}

func (w *fakeWaiter) total() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	var t time.Duration
	for _, d := range w.waits {
		t += d
	}
	return t
}

func (w *fakeWaiter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waits)
}

type harness struct {
	engine *Engine
	clock  *fakeClock
	waiter *fakeWaiter
	// calls records the account label used for each upstream attempt.
	calls []string
	mu    sync.Mutex
}

func (h *harness) record(label string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, label)
}

func (h *harness) attempts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// newHarness wires an engine to a scripted upstream. The handler receives the
// calling account's api key as its label.
func newHarness(t *testing.T, accountIDs []string, handler func(label string, w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()

	h := &harness{
		clock:  &fakeClock{now: base},
		waiter: nil,
	}
	h.waiter = &fakeWaiter{clock: h.clock}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := r.Header.Get("X-Api-Key")
		h.record(label)
		handler(label, w, r)
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i, id := range accountIDs {
		if err := st.Upsert(&store.Account{
			ID: id, Lane: store.LaneAnthropic, Kind: store.KindAPIKey,
			Label: id, Priority: i, Enabled: true,
			APIKey: id, BaseURL: srv.URL,
		}); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}

	cfg := config.Default()
	// Keep backoff short so overload tests stay readable.
	cfg.Retry.OverloadInitial = config.Duration(time.Second)
	cfg.Retry.OverloadMax = config.Duration(8 * time.Second)

	lanes := map[store.LaneID]lane.Lane{store.LaneAnthropic: anthropic.New(srv.Client())}
	e := New(cfg, st, lanes, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = h.clock.Now
	e.jitter = func() float64 { return 1.0 } // deterministic backoff

	h.engine = e
	return h
}

func post(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
}

func run(t *testing.T, h *harness, body string) (*Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.engine.Execute(ctx, store.LaneAnthropic, post(body), []byte(body), h.waiter)
}

// Feature 3: an overloaded upstream must be absorbed, not surfaced.
func TestOverloadRetriesUntilSuccess(t *testing.T) {
	var n int
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		n++
		if n < 4 {
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	res, err := run(t, h, `{"model":"claude","messages":[]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Response.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.Response.StatusCode)
	}
	if res.Attempts != 4 {
		t.Errorf("attempts = %d, want 4", res.Attempts)
	}
	// Exponential backoff: 1s, 2s, 4s.
	if got, want := h.waiter.total(), 7*time.Second; got != want {
		t.Errorf("total wait = %v, want %v", got, want)
	}
}

// Feature 1: a spent account rotates to the next one, and the request still
// succeeds without the caller ever seeing the 429.
func TestQuotaExhaustionRotatesAccount(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(label string, w http.ResponseWriter, _ *http.Request) {
		if label == "a" {
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Account.ID != "b" {
		t.Errorf("served by %q, want b", res.Account.ID)
	}
	if got := h.attempts(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("attempts = %v, want [a b]", got)
	}
	// Rotation must be immediate; waiting for a reset we can route around
	// would be the whole bug this tool exists to fix.
	if h.waiter.count() != 0 {
		t.Errorf("waited %d times, want 0", h.waiter.count())
	}
	if st := h.engine.store.Health("a").State; st != store.StateExhausted {
		t.Errorf("account a state = %v, want exhausted", st)
	}
}

// Feature 2: with everything spent, the request parks and resumes after the
// reset plus the configured buffer.
func TestParksUntilResetThenSucceeds(t *testing.T) {
	resetAt := base.Add(10 * time.Minute)
	var served bool
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		if served {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		served = true
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", resetAt.Format(time.RFC3339))
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Response.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.Response.StatusCode)
	}
	// 10 minutes to the reset, plus the 60s skew buffer.
	if got, want := h.waiter.total(), 11*time.Minute; got != want {
		t.Errorf("parked for %v, want %v", got, want)
	}
}

// A reset further out than max_hold must hand off rather than hold a socket
// open for hours.
func TestParkBeyondMaxHoldReturnsHandoff(t *testing.T) {
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "18000") // 5h
		w.WriteHeader(429)
	})

	_, err := run(t, h, `{"model":"claude"}`)
	var tooLong *ErrParkTooLong
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want ErrParkTooLong", err)
	}
	if !tooLong.Until.After(base.Add(4 * time.Hour)) {
		t.Errorf("Until = %v, want ~5h out", tooLong.Until)
	}
}

// A short throttle should keep the same account, because rotating would throw
// away a warm prompt cache to dodge a wait measured in seconds.
func TestBurstLimitStaysOnSameAccount(t *testing.T) {
	var n int
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "20")
			w.WriteHeader(429)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Account.ID != "a" {
		t.Errorf("served by %q, want a (cache affinity)", res.Account.ID)
	}
	if got := h.attempts(); len(got) != 2 || got[0] != "a" || got[1] != "a" {
		t.Errorf("attempts = %v, want [a a]", got)
	}
	if got, want := h.waiter.total(), 20*time.Second; got != want {
		t.Errorf("waited %v, want %v", got, want)
	}
}

// Persistent overload on one account should eventually try another, in case
// the fault is account-scoped after all.
func TestPersistentOverloadEventuallyRotates(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(label string, w http.ResponseWriter, _ *http.Request) {
		if label == "a" {
			w.WriteHeader(529)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Account.ID != "b" {
		t.Errorf("served by %q, want b", res.Account.ID)
	}
	// rotate_after defaults to 3, so account a is tried three times first.
	got := h.attempts()
	if len(got) != 4 || got[0] != "a" || got[3] != "b" {
		t.Errorf("attempts = %v, want three on a then b", got)
	}
}

// A malformed request is the client's fault; retrying it elsewhere would fail
// identically while spending another account's quota.
func TestFatalErrorPassesThroughWithoutRotating(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad model"}}`)
	})

	res, err := run(t, h, `{"model":"nope"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Response.StatusCode != 400 {
		t.Errorf("status = %d, want 400", res.Response.StatusCode)
	}
	if len(h.attempts()) != 1 {
		t.Errorf("attempts = %v, want exactly one", h.attempts())
	}
	body, _ := io.ReadAll(res.Response.Body)
	if !strings.Contains(string(body), "bad model") {
		t.Errorf("client lost the upstream error detail: %q", body)
	}
}

// Successive turns of one conversation should return to the same account.
func TestStickyRoutingKeepsConversationOnOneAccount(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	const convo = `{"model":"claude","system":"you are a helpful assistant","messages":[{"role":"user"}]}`
	for i := 0; i < 3; i++ {
		res, err := run(t, h, convo+strings.Repeat(" ", i)) // body grows, prefix stable
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		res.Response.Body.Close()
	}
	for i, label := range h.attempts() {
		if label != "a" {
			t.Errorf("turn %d served by %q, want a", i, label)
		}
	}
}

// An upstream whose clock runs behind ours can advertise a reset that has
// already passed. Without a cooldown floor the engine would spin, re-trying an
// account that keeps refusing and burning a request every iteration.
func TestResetInThePastDoesNotLivelock(t *testing.T) {
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", base.Add(-time.Hour).Format(time.RFC3339))
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	// Short: the loop is bounded by the context, and one second is plenty to
	// observe the retry cadence.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := h.engine.Execute(ctx, store.LaneAnthropic, post("{}"), []byte("{}"), h.waiter)
	if err == nil {
		t.Fatal("expected an error, got success")
	}

	// The guard is the cooldown floor: every retry round must be separated by
	// a real wait. Attempt *count* is not the thing to assert here, because
	// the test waiter advances a fake clock instantly and so makes waiting
	// free; in production those same waits are wall-clock seconds.
	h.waiter.mu.Lock()
	waits := append([]time.Duration(nil), h.waiter.waits...)
	h.waiter.mu.Unlock()

	if len(waits) == 0 {
		t.Fatal("engine retried without ever waiting: that is the hot loop this guards against")
	}
	for i, d := range waits {
		if d < minCooldown {
			t.Fatalf("wait %d was %v, shorter than the %v cooldown floor", i, d, minCooldown)
		}
	}
	hh := h.engine.store.Health("a")
	if !hh.ResetAt.After(base) {
		t.Errorf("resetAt = %v, want it clamped to after %v", hh.ResetAt, base)
	}
}

func TestNoAccountsIsAnError(t *testing.T) {
	h := newHarness(t, nil, func(_ string, w http.ResponseWriter, _ *http.Request) {})
	_, err := run(t, h, `{}`)
	if !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("err = %v, want ErrNoAccounts", err)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {})
	e := h.engine
	for i := 1; i <= 12; i++ {
		if got := e.backoff(i); got > e.cfg.Retry.OverloadMax.D() {
			t.Fatalf("backoff(%d) = %v, exceeds cap %v", i, got, e.cfg.Retry.OverloadMax.D())
		}
	}
	if got, want := e.backoff(1), time.Second; got != want {
		t.Errorf("backoff(1) = %v, want %v", got, want)
	}
	if got, want := e.backoff(3), 4*time.Second; got != want {
		t.Errorf("backoff(3) = %v, want %v", got, want)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.engine.Execute(ctx, store.LaneAnthropic, post("{}"), []byte("{}"), h.waiter)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

var _ = fmt.Sprintf
