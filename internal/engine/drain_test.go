package engine

import (
	"context"
	"io"
	"net/http"
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
