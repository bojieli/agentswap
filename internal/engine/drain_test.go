package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/store"
)

// Predictive rotation is what the quota headers buy: an account is retired at
// the drain threshold rather than after it starts refusing. Without it the
// first request of every window is spent discovering the limit.
func TestDrainedAccountIsSkippedBeforeItRefuses(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	// "a" reported 99% utilization on its last successful response.
	h.engine.store.MutateHealth("a", func(hl *store.Health) {
		hl.Windows = []store.Window{{Name: "5h", Utilization: 99, ResetAt: base.Add(2 * time.Hour)}}
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if got := h.attempts(); len(got) != 1 || got[0] != "b" {
		t.Errorf("attempts = %v, want a single call on b: a was drained", got)
	}
}

// A reading from a window that has since refilled says nothing about the
// account's headroom now. Treating it as current would retire an account for
// the rest of its life on one stale observation.
func TestDrainReadingExpiresWithItsWindow(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	h.engine.store.MutateHealth("a", func(hl *store.Health) {
		hl.Windows = []store.Window{{Name: "5h", Utilization: 100, ResetAt: base.Add(-time.Minute)}}
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if got := h.attempts(); len(got) != 1 || got[0] != "a" {
		t.Errorf("attempts = %v, want a: its window has already reset", got)
	}
}

// Draining is a preference, not a prohibition. When every account is above the
// threshold but none has actually been refused, sending the request is better
// than parking it.
func TestEveryAccountDrainedStillServesTheRequest(t *testing.T) {
	h := newHarness(t, []string{"a", "b"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	for _, id := range []string{"a", "b"} {
		h.engine.store.MutateHealth(id, func(hl *store.Health) {
			hl.Windows = []store.Window{{Name: "5h", Utilization: 100, ResetAt: base.Add(time.Hour)}}
		})
	}

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the request served rather than parked", res.Response.StatusCode)
	}
	if h.waiter.count() != 0 {
		t.Errorf("waited %d times, want none: nothing had actually been refused", h.waiter.count())
	}
}

// Hop-by-hop headers describe one connection. Forwarding them upstream is a
// protocol error, and forwarding the upstream's back can corrupt the response
// the client sees.
func TestHopByHopHeadersAreStripped(t *testing.T) {
	var got http.Header
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	req := post(`{"model":"claude"}`)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "Basic sniffed")
	req.Header.Set("Anthropic-Version", "2023-06-01")

	res, err := h.engine.Execute(context.Background(), store.LaneAnthropic, req, []byte(`{"model":"claude"}`), h.waiter)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	for _, k := range []string{"Keep-Alive", "Proxy-Authorization"} {
		if v := got.Get(k); v != "" {
			t.Errorf("%s reached the upstream as %q", k, v)
		}
	}
	// Everything else is the client's business and must survive.
	if v := got.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want it forwarded", v)
	}
}

func TestCopyHeaderSkipsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "text/event-stream")
	src.Set("Transfer-Encoding", "chunked")
	src.Add("X-Multi", "one")
	src.Add("X-Multi", "two")

	dst := http.Header{}
	CopyHeader(dst, src)

	if dst.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", dst.Get("Content-Type"))
	}
	if dst.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding was copied; it describes the upstream connection, not ours")
	}
	if got := dst.Values("X-Multi"); len(got) != 2 {
		t.Errorf("X-Multi = %v, want both values", got)
	}
}

// A window that genuinely refills in seconds should be waited out for seconds.
// Rounding every reset up to the livelock guard idles the account — and with
// one account in the pool, idles the user.
func TestShortResetIsWaitedOutNotRoundedUp(t *testing.T) {
	var refused bool
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		if !refused {
			refused = true
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
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

	// Four seconds of reset, plus a skew allowance capped at the wait itself.
	if got := h.waiter.total(); got > 10*time.Second {
		t.Errorf("waited %v for a four-second window, want it honoured", got)
	}
	if got := h.waiter.total(); got < 4*time.Second {
		t.Errorf("waited %v, want at least the four seconds the upstream asked for", got)
	}
}

// The guard still has to hold: a reset time that has already passed would
// otherwise spin against an account that keeps refusing.
func TestResetInThePastFallsBackToTheCooldown(t *testing.T) {
	var calls int
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 3 {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		// A reset an hour in the past: a misread header, or a clock behind ours.
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", base.Add(-time.Hour).Format(time.RFC3339))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	res, err := run(t, h, `{"model":"claude"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Response.Body.Close()

	// Each refusal must cost a real pause rather than an immediate retry.
	if got := h.waiter.total(); got < minCooldown {
		t.Errorf("total wait %v, want at least one cooldown of %v", got, minCooldown)
	}
	if calls > 8 {
		t.Errorf("made %d attempts: a past reset time is spinning", calls)
	}
}

// The skew allowance is insurance against clock drift, not a tax on short
// waits.
func TestSkewAllowanceIsCappedByTheWait(t *testing.T) {
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	e := h.engine

	cases := []struct {
		wait time.Duration
		want time.Duration
	}{
		{5 * time.Hour, e.cfg.Park.Buffer.D()}, // full allowance, negligible
		{time.Second, time.Second},             // capped: never more than doubles
		{0, 0},
		{-time.Second, 0},
	}
	for _, c := range cases {
		if got := e.skewAllowance(c.wait); got != c.want {
			t.Errorf("skewAllowance(%v) = %v, want %v", c.wait, got, c.want)
		}
	}
}

// A key is not a token: there is no exchange that turns a refused one into a
// working one. Attempting it replaced the upstream's explanation with an
// internal detail about the account not being OAuth, and recommended a sign-in
// that cannot apply.
func TestRefusedAPIKeyIsNotRefreshed(t *testing.T) {
	var calls int
	h := newHarness(t, []string{"a"}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"forbidden","message":"your token is not authorized for any channel serving this model"}}`)
	})

	_, err := run(t, h, `{"model":"claude"}`)
	if err == nil {
		t.Fatal("want an error")
	}

	var rejected *ErrCredentialsRejected
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want ErrCredentialsRejected", err)
	}
	// The upstream's own words are the diagnosis; ours would be a guess.
	if got := rejected.Rejected[0].Reason; !strings.Contains(got, "not authorized for any channel") {
		t.Errorf("reason = %q, want the upstream's message", got)
	}
	if rejected.Rejected[0].Kind != store.KindAPIKey {
		t.Errorf("kind = %q, want it carried so the remedy can match", rejected.Rejected[0].Kind)
	}
	// One attempt: no refresh, no retry of a credential that cannot change.
	if calls != 1 {
		t.Errorf("upstream saw %d requests, want 1", calls)
	}
}
