package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/bojieli/agentswap/internal/lane"
	"github.com/bojieli/agentswap/internal/store"
)

// refreshGroup coalesces concurrent token refreshes for one account.
//
// This is not an optimization. Both upstreams rotate the refresh token on use,
// so a second exchange presents a credential the server has already retired —
// which retires the *account*, not just the request. A single agent easily has
// several requests in flight (subagents, background compaction), and they all
// notice the same expired token in the same instant.
type refreshGroup struct {
	mu    sync.Mutex
	calls map[string]*refreshCall
}

type refreshCall struct {
	wg   sync.WaitGroup
	acct *store.Account
	err  error
}

func newRefreshGroup() *refreshGroup {
	return &refreshGroup{calls: map[string]*refreshCall{}}
}

// do runs fn for id unless an identical refresh is already in flight, in which
// case it waits for that one and returns its result.
func (g *refreshGroup) do(id string, fn func() (*store.Account, error)) (*store.Account, error) {
	g.mu.Lock()
	if c, ok := g.calls[id]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.acct, c.err
	}
	c := &refreshCall{}
	c.wg.Add(1)
	g.calls[id] = c
	g.mu.Unlock()

	c.acct, c.err = fn()

	g.mu.Lock()
	delete(g.calls, id)
	g.mu.Unlock()
	c.wg.Done()

	return c.acct, c.err
}

// refresh renews an account's token and returns the updated account.
//
// staleToken is the access token the caller was using. If the pool already
// holds a different one, someone else has refreshed since and the caller was
// simply holding a stale clone — so this returns the current credential
// without spending an exchange. That check is what makes a late arrival safe:
// coalescing only covers callers that overlap in time.
func (e *Engine) refresh(ctx context.Context, ln lane.Lane, id, staleToken string) (*store.Account, error) {
	return e.refreshes.do(id, func() (*store.Account, error) {
		current, err := e.store.Get(id)
		if err != nil {
			return nil, err
		}
		if current.AccessToken != "" && current.AccessToken != staleToken {
			return current, nil
		}

		if err := ln.Refresh(ctx, current); err != nil {
			return nil, err
		}

		// Write the renewed credential back into the pool's own record rather
		// than replacing it wholesale, so a concurrent enable/disable survives.
		if err := e.store.UpdateAccount(id, func(dst *store.Account) {
			dst.AccessToken = current.AccessToken
			dst.RefreshToken = current.RefreshToken
			dst.ExpiresAt = current.ExpiresAt
		}); err != nil {
			// The token in memory is good; only persistence failed. Failing the
			// request over that would turn a recoverable disk problem into an
			// outage, so carry on and let the next restart re-refresh.
			e.log.Warn("persist refreshed token", "account", current.Display(), "err", err)
		}
		return current, nil
	})
}

// redactSecrets removes an account's own credential from a message before it
// is stored, logged, or shown to anyone.
//
// Relaying the upstream's words is what makes a refusal diagnosable, but the
// upstream chooses those words: gateways do echo the key back — "Incorrect API
// key provided: nb_…" — and some echo more of it than others. That text lands
// in state.json, in the daemon's log, and in the client's error, so a proxy
// whose whole point is that credentials do not leak has to take its own
// credential back out of it.
func redactSecrets(msg string, a *store.Account) string {
	if msg == "" || a == nil {
		return msg
	}
	for _, secret := range []string{a.APIKey, a.AccessToken, a.RefreshToken} {
		// Short values are not credentials and would match half the message.
		if len(secret) < 8 {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	return msg
}
