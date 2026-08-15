package proxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentswap/internal/config"
)

func parkConfig(interval time.Duration) config.Park {
	p := config.Default().Park
	p.KeepaliveInterval = config.Duration(interval)
	return p
}

// Silence is the default because it commits to nothing: no status line has
// been sent, so a failure can still be reported as an HTTP status.
func TestSilentWaiterWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newStreamWaiter(rec, KeepaliveSilent, parkConfig(10*time.Millisecond))

	if err := w.Wait(context.Background(), 30*time.Millisecond, "waiting for quota"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if w.Committed() {
		t.Error("the silent waiter committed to a status line")
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("wrote %q while parked, want nothing", body)
	}
}

func TestPingWaiterEmitsKeepalives(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newStreamWaiter(rec, KeepalivePing, parkConfig(10*time.Millisecond))

	if err := w.Wait(context.Background(), 55*time.Millisecond, "waiting for quota"); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if !w.Committed() {
		t.Error("ping mode did not commit to a status line")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if n := strings.Count(rec.Body.String(), "event: ping"); n < 2 {
		t.Errorf("emitted %d pings over five intervals, want several", n)
	}
	// Well-formed SSE: every frame ends with a blank line.
	if !strings.HasSuffix(rec.Body.String(), "\n\n") {
		t.Errorf("last frame is not terminated: %q", rec.Body.String())
	}
}

// Once the status line is spent it cannot be taken back, so committing is the
// one thing the waiter must not do speculatively.
func TestPingWaiterDoesNotCommitForAZeroWait(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newStreamWaiter(rec, KeepalivePing, parkConfig(10*time.Millisecond))

	if err := w.Wait(context.Background(), 0, "no wait at all"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if w.Committed() {
		t.Error("committed without ever needing to wait")
	}
}

func TestWaiterStopsWhenTheClientGoesAway(t *testing.T) {
	for _, mode := range []KeepaliveMode{KeepaliveSilent, KeepalivePing} {
		t.Run(string(mode), func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := newStreamWaiter(rec, mode, parkConfig(10*time.Millisecond))

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			// Burning a five-hour wait on a connection nobody is listening to
			// keeps an account pinned for a request that can never be answered.
			done := make(chan error, 1)
			go func() { done <- w.Wait(ctx, 5*time.Hour, "waiting for quota") }()

			select {
			case err := <-done:
				if err == nil {
					t.Error("Wait returned nil for a cancelled request")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Wait ignored cancellation")
			}
		})
	}
}
