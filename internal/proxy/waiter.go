package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bojieli/agentswap/internal/config"
)

// KeepaliveMode controls what, if anything, is written to the client while a
// request is parked waiting for quota.
type KeepaliveMode string

const (
	// KeepaliveSilent holds the connection with no bytes written until the
	// upstream actually responds.
	//
	// This is the default and it is the safe choice. The client is on loopback,
	// so there is no intermediary to time out an idle socket, and writing
	// nothing means we never commit to a status code we might have to take
	// back. The only limit is the client's own request timeout, which
	// `agentswap install` raises for exactly this reason.
	KeepaliveSilent KeepaliveMode = "silent"

	// KeepalivePing commits to a 200 text/event-stream and emits SSE ping
	// frames while waiting.
	//
	// Use only if a client insists on seeing bytes sooner. It is strictly
	// riskier: once the status line is sent it cannot be retracted, so a
	// subsequent failure has to be reported as an in-band SSE error event
	// rather than an HTTP status. Whether a ping *preceding* message_start is
	// accepted by every client parser is unverified.
	KeepalivePing KeepaliveMode = "ping"
)

// streamWaiter blocks while keeping the client connection usable.
type streamWaiter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	mode KeepaliveMode
	cfg  config.Park

	committed bool
}

func newStreamWaiter(w http.ResponseWriter, mode KeepaliveMode, cfg config.Park) *streamWaiter {
	return &streamWaiter{w: w, rc: http.NewResponseController(w), mode: mode, cfg: cfg}
}

// Wait blocks for d, or until ctx ends.
func (s *streamWaiter) Wait(ctx context.Context, d time.Duration, reason string) error {
	if d <= 0 {
		return nil
	}

	// The write deadline must outlast the wait, or the runtime severs the
	// connection we are deliberately holding open.
	_ = s.rc.SetWriteDeadline(time.Now().Add(d + time.Minute))

	if s.mode != KeepalivePing {
		return sleep(ctx, d)
	}

	if !s.committed {
		s.commit()
	}

	deadline := time.Now().Add(d)
	tick := s.cfg.KeepaliveInterval.D()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if remaining < tick {
			tick = remaining
		}
		if err := sleep(ctx, tick); err != nil {
			return err
		}
		if err := s.ping(reason); err != nil {
			// The client hung up. Stop burning quota on a response nobody will
			// read.
			return err
		}
	}
}

// Committed reports whether we have already sent a status line, which
// determines how a later failure has to be reported.
func (s *streamWaiter) Committed() bool { return s.committed }

func (s *streamWaiter) commit() {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	_ = s.rc.Flush()
	s.committed = true
}

func (s *streamWaiter) ping(_ string) error {
	if _, err := fmt.Fprint(s.w, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
		return err
	}
	return s.rc.Flush()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
