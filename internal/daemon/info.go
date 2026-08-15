// Package daemon records where a running agentswap daemon can be reached, so
// the other commands do not have to guess.
//
// `agentswap serve --addr` and a config file that says something else are both
// legitimate, and without this every other command would look at the config,
// find nothing listening, and report a daemon that is running perfectly well
// as down.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const infoFile = "daemon.json"

// Info is what a running daemon publishes about itself.
type Info struct {
	Addr      string    `json:"addr"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// Path is where the info file lives inside the config directory.
func Path(configDir string) string { return filepath.Join(configDir, infoFile) }

// Write publishes the daemon's address. It is advisory: a failure here should
// never stop the daemon from serving, only make the other commands fall back
// to the configured address.
func Write(configDir string, i Info) error {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(Path(configDir), append(b, '\n'), 0o600)
}

// Read returns the recorded info, or nil if no daemon has published any. A
// file left behind by a daemon that was killed reads fine; callers find out it
// is stale by failing to reach the address, which they would have to handle
// anyway.
func Read(configDir string) (*Info, error) {
	b, err := os.ReadFile(Path(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var i Info
	if err := json.Unmarshal(b, &i); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(configDir), err)
	}
	if i.Addr == "" {
		return nil, nil
	}
	return &i, nil
}

// Clear removes the info file on a clean shutdown.
func Clear(configDir string) error {
	err := os.Remove(Path(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Addrs returns the addresses to try, in order: whatever a running daemon
// published, then the configured one. Callers probe them in turn.
func Addrs(configDir, configured string) []string {
	out := []string{}
	if info, err := Read(configDir); err == nil && info != nil {
		out = append(out, info.Addr)
	}
	for _, a := range out {
		if a == configured {
			return out
		}
	}
	return append(out, configured)
}
