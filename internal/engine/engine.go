// Package engine turns a pool of credentials into a single reliable endpoint.
// It owns account selection, retry, rotation and parking; everything
// protocol-specific lives behind lane.Lane.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/store"
)

// errorBodyLimit caps how much of an error response we buffer to classify it.
// Error envelopes are small; anything larger is not one.
const errorBodyLimit = 64 << 10

// ErrNoAccounts means the lane has no enabled accounts at all.
var ErrNoAccounts = errors.New("no accounts configured for this lane")

// ErrParkTooLong means every account is spent for longer than park.max_hold.
// The caller should hand off to the supervisor, which can resume the session
// after the reset rather than holding a connection open for hours.
type ErrParkTooLong struct {
	Until time.Time
}

func (e *ErrParkTooLong) Error() string {
	return fmt.Sprintf("all accounts exhausted until %s, beyond max hold", e.Until.Format(time.RFC3339))
}

// Waiter blocks while keeping the client connection alive. The proxy supplies
// an implementation that emits SSE keepalives; tests and non-streaming callers
// can just sleep.
type Waiter interface {
	Wait(ctx context.Context, d time.Duration, reason string) error
}

// SleepWaiter is a Waiter that does nothing but wait.
type SleepWaiter struct{}

func (SleepWaiter) Wait(ctx context.Context, d time.Duration, _ string) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Engine executes requests against the credential pool.
type Engine struct {
	cfg       config.Config
	store     *store.Store
	lanes     map[store.LaneID]lane.Lane
	client    *http.Client
	log       *slog.Logger
	sticky    *stickyMap
	refreshes *refreshGroup

	now func() time.Time
	// jitter returns a factor in [0.5, 1.5) used to spread retries. Swappable
	// so tests are deterministic.
	jitter func() float64
}

// New builds an Engine. client should have no timeout: model responses stream
// for minutes and a client-level deadline would sever them mid-answer.
func New(cfg config.Config, st *store.Store, lanes map[store.LaneID]lane.Lane, client *http.Client, log *slog.Logger) *Engine {
	if client == nil {
		client = &http.Client{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:       cfg,
		store:     st,
		lanes:     lanes,
		client:    client,
		log:       log,
		sticky:    newStickyMap(cfg.Rotation.StickyTTL.D()),
		refreshes: newRefreshGroup(),
		now:       time.Now,
		jitter:    func() float64 { return 0.5 + rand.Float64() },
	}
}

// Result is a successful upstream response plus the account that produced it.
// Body is still open and streaming; the caller must close it.
type Result struct {
	Response *http.Response
	Account  *store.Account
	Attempts int
}

// Execute sends the request against the pool, retrying, rotating and parking
// until it gets a response worth returning to the client.
//
// A "response worth returning" is a success or a fatal client error. Rate
// limits, quota exhaustion and upstream overload are all absorbed here, which
// is the entire point: the agent upstream of this call never sees them.
func (e *Engine) Execute(ctx context.Context, laneID store.LaneID, req *http.Request, body []byte, w Waiter) (*Result, error) {
	ln, ok := e.lanes[laneID]
	if !ok {
		return nil, fmt.Errorf("unknown lane %q", laneID)
	}
	if len(e.store.Accounts(laneID)) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoAccounts, laneID)
	}
	if w == nil {
		w = SleepWaiter{}
	}

	convKey := ConversationKey(body)
	tried := map[string]bool{}
	attempts := 0
	overloadStreak := 0
	authAttempts := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := e.now()

		acct := e.selectAccount(laneID, convKey, tried, now)
		if acct == nil {
			// Everything is either spent or already tried this round.
			waited, err := e.park(ctx, laneID, w, now)
			if err != nil {
				return nil, err
			}
			if !waited {
				return nil, fmt.Errorf("%w: all accounts unavailable", ErrNoAccounts)
			}
			// A fresh round: accounts that were skipped may have recovered.
			tried = map[string]bool{}
			overloadStreak = 0
			continue
		}

		attempts++
		fresh, err := e.ensureFresh(ctx, ln, acct, now)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			e.log.Warn("token refresh failed", "account", acct.Display(), "err", err)
			e.markInvalid(acct, err)
			tried[acct.ID] = true
			continue
		}
		acct = fresh

		resp, err := e.send(ctx, ln, acct, req, body)
		if err != nil {
			// A transport failure is indistinguishable from an overloaded
			// upstream from here, and both want the same treatment.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			overloadStreak++
			e.log.Warn("upstream request failed", "account", acct.Display(), "err", err, "streak", overloadStreak)
			if overloadStreak >= e.cfg.Retry.RotateAfter {
				tried[acct.ID] = true
				overloadStreak = 0
				continue
			}
			if err := w.Wait(ctx, e.backoff(overloadStreak), "upstream unreachable"); err != nil {
				return nil, err
			}
			continue
		}

		e.observe(ln, acct, resp)

		var errBody []byte
		if resp.StatusCode >= 300 {
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
			resp.Body.Close()
		}

		outcome := ln.Classify(resp, errBody, e.cfg.Retry, now)

		switch outcome.Action {
		case lane.ActionRelay:
			e.markSuccess(acct, convKey)
			overloadStreak = 0
			return &Result{Response: resp, Account: acct, Attempts: attempts}, nil

		case lane.ActionFatal:
			// Hand the client its own error back, body included.
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			return &Result{Response: resp, Account: acct, Attempts: attempts}, nil

		case lane.ActionRefreshAuth:
			authAttempts++
			if authAttempts > e.cfg.Retry.AuthRefreshAttempts {
				e.markInvalid(acct, errors.New(outcome.Reason))
				tried[acct.ID] = true
				authAttempts = 0
				continue
			}
			renewed, err := e.refresh(ctx, ln, acct.ID, acct.AccessToken)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				e.markInvalid(acct, err)
				tried[acct.ID] = true
				continue
			}
			acct = renewed
			continue

		case lane.ActionRetrySame:
			if outcome.Overload {
				overloadStreak++
				// Overload is rarely account-specific, so absorb a few on the
				// same account before spending another one's quota on it.
				if overloadStreak >= e.cfg.Retry.RotateAfter && len(e.store.Accounts(laneID)) > 1 {
					e.log.Info("overload persists, trying another account",
						"account", acct.Display(), "streak", overloadStreak)
					tried[acct.ID] = true
					overloadStreak = 0
					continue
				}
				d := e.backoff(overloadStreak)
				e.log.Info("upstream overloaded, retrying",
					"account", acct.Display(), "in", d.Round(time.Millisecond), "reason", outcome.Reason)
				if err := w.Wait(ctx, d, outcome.Reason); err != nil {
					return nil, err
				}
				continue
			}
			e.markCooling(acct, now.Add(outcome.RetryAfter), outcome.Reason)
			e.log.Info("throttled, holding the same account",
				"account", acct.Display(), "in", outcome.RetryAfter.Round(time.Second))
			if err := w.Wait(ctx, outcome.RetryAfter, outcome.Reason); err != nil {
				return nil, err
			}
			continue

		case lane.ActionRotate:
			e.markExhausted(acct, outcome.ResetAt, outcome.Reason)
			e.log.Info("account exhausted, rotating",
				"account", acct.Display(), "resets", outcome.ResetAt.Format(time.RFC3339), "reason", outcome.Reason)
			tried[acct.ID] = true
			overloadStreak = 0
			continue

		default:
			return nil, fmt.Errorf("unhandled outcome %v", outcome.Action)
		}
	}
}

// park waits until an account recovers. It reports whether it actually waited;
// false means there is nothing to wait for.
func (e *Engine) park(ctx context.Context, laneID store.LaneID, w Waiter, now time.Time) (bool, error) {
	until, ok := e.nextAvailable(laneID, now)
	if !ok {
		// Every account is invalid, not merely spent. Waiting cannot fix a
		// rejected credential.
		return false, nil
	}
	if !e.cfg.Park.Enabled {
		return false, nil
	}

	// The buffer absorbs clock skew between us and the upstream. Retrying one
	// second early wastes the entire wait.
	deadline := until.Add(e.cfg.Park.Buffer.D())
	d := deadline.Sub(now)
	if d <= 0 {
		return true, nil
	}
	if d > e.cfg.Park.MaxHold.D() {
		return false, &ErrParkTooLong{Until: deadline}
	}

	e.log.Info("all accounts exhausted, parking request",
		"lane", laneID, "until", deadline.Format(time.RFC3339), "for", d.Round(time.Second))
	if err := w.Wait(ctx, d, "waiting for quota reset"); err != nil {
		return false, err
	}
	return true, nil
}

// backoff returns exponential backoff with full jitter, capped by config.
// Jitter matters when several agents share one pool: without it they retry in
// lockstep and re-overload the upstream the instant it recovers.
func (e *Engine) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := e.cfg.Retry.OverloadInitial.D()
	for i := 1; i < attempt && d < e.cfg.Retry.OverloadMax.D(); i++ {
		d *= 2
	}
	if d > e.cfg.Retry.OverloadMax.D() {
		d = e.cfg.Retry.OverloadMax.D()
	}
	return time.Duration(float64(d) * e.jitter())
}

// refreshSkew is how far ahead of expiry a token is renewed. It has to exceed
// the longest plausible request setup time, or a token that passes the check
// expires while the request is in flight.
const refreshSkew = 60 * time.Second

// ensureFresh refreshes an OAuth token that is expired or nearly so, rather
// than spending a request to discover it. It returns the account to use, which
// is a different value once a refresh has happened.
func (e *Engine) ensureFresh(ctx context.Context, ln lane.Lane, a *store.Account, now time.Time) (*store.Account, error) {
	if !a.TokenExpiredAt(now, refreshSkew) {
		return a, nil
	}
	return e.refresh(ctx, ln, a.ID, a.AccessToken)
}

// send builds and dispatches one upstream attempt.
func (e *Engine) send(ctx context.Context, ln lane.Lane, a *store.Account, orig *http.Request, body []byte) (*http.Response, error) {
	base, err := ln.Upstream(a)
	if err != nil {
		return nil, err
	}

	target := *base
	target.Path = joinPath(base.Path, orig.URL.Path)
	target.RawQuery = orig.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, orig.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Copy client headers verbatim, then let the lane overwrite the credential
	// ones. Hop-by-hop headers are dropped: they describe this connection, not
	// the upstream one.
	for k, vs := range orig.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Del("Accept-Encoding") // let Go negotiate and transparently decode
	req.Host = target.Host
	req.Header.Del("Host")
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
	}

	ln.Authorize(req, a)
	return e.client.Do(req)
}

func (e *Engine) observe(ln lane.Lane, a *store.Account, resp *http.Response) {
	windows := ln.Observe(resp)
	if len(windows) == 0 {
		return
	}
	e.store.MutateHealth(a.ID, func(h *store.Health) { h.Windows = windows })
}

func (e *Engine) markSuccess(a *store.Account, convKey string) {
	now := e.now()
	e.sticky.set(convKey, a.ID, now)
	e.store.MutateHealth(a.ID, func(h *store.Health) {
		h.State = store.StateAvailable
		h.LastUsed = now
		h.Requests++
		h.ConsecFails = 0
		h.LastError = ""
	})
}

func (e *Engine) markCooling(a *store.Account, until time.Time, reason string) {
	e.store.MutateHealth(a.ID, func(h *store.Health) {
		h.State = store.StateCooling
		h.Until = until
		h.LastError = reason
	})
}

// minCooldown is the shortest time an exhausted account is kept out of
// rotation. An upstream whose clock runs behind ours — or a reset header we
// misread — can otherwise name a reset time already in the past, which would
// put the engine in a tight loop re-trying an account that keeps refusing.
const minCooldown = 60 * time.Second

func (e *Engine) markExhausted(a *store.Account, resetAt time.Time, reason string) {
	if floor := e.now().Add(minCooldown); resetAt.Before(floor) {
		resetAt = floor
	}
	e.store.MutateHealth(a.ID, func(h *store.Health) {
		h.State = store.StateExhausted
		h.ResetAt = resetAt
		h.Rotations++
		h.LastError = reason
	})
}

func (e *Engine) markInvalid(a *store.Account, err error) {
	e.store.MutateHealth(a.ID, func(h *store.Health) {
		h.State = store.StateInvalid
		h.ConsecFails++
		h.LastError = err.Error()
	})
}
