package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/daemon"
)

// notifyDaemon tells a running daemon that the pool has changed.
//
// The daemon reads accounts.json once, at startup. Without this, replacing a
// rejected credential — the fix that `status` and the client's own error both
// recommend — changes nothing until somebody restarts the daemon, and nothing
// anywhere says so. The user does the right thing and watches it fail again.
//
// Best effort by design: no daemon running is the normal case for these
// commands, and a failure here must not make an otherwise successful change
// look like it failed.
func notifyDaemon(retry ...string) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return
	}

	// Naming the accounts is what carries the intent: "I have just fixed this
	// one, try it again" cannot be inferred from the file, since a credential
	// re-entered unchanged looks identical to one never touched.
	payload, err := json.Marshal(map[string][]string{"retry": retry})
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, addr := range daemon.Addrs(dir, cfg.Addr) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+addr+"/_agentswap/reload", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
	}
}
