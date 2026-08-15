package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when an account id is not in the pool.
var ErrNotFound = errors.New("account not found")

const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
)

// Store is the credential pool plus its health state. It is safe for
// concurrent use; the proxy hot path reads accounts and mutates health from
// many goroutines at once.
type Store struct {
	dir string

	mu       sync.RWMutex
	accounts []*Account
	health   map[string]*Health

	healthDirty bool
}

// Open loads the pool from dir, creating it if absent.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone, so a config dir
	// created by hand or by an older version can still be world-readable. The
	// files inside are 0600, but the listing is a map of which accounts exist
	// and the directory being writable is worse than that. Best effort: on
	// Windows, or a directory we do not own, there is nothing to do.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, 0o700)
	}
	s := &Store{dir: dir, health: map[string]*Health{}}
	if err := s.loadAccounts(); err != nil {
		return nil, err
	}
	if err := s.loadHealth(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadAccounts() error {
	b, err := os.ReadFile(filepath.Join(s.dir, accountsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read accounts: %w", err)
	}
	var accts []*Account
	if err := json.Unmarshal(b, &accts); err != nil {
		return fmt.Errorf("parse accounts.json: %w", err)
	}
	for _, a := range accts {
		if !a.Lane.Valid() {
			return fmt.Errorf("account %q: unknown lane %q", a.ID, a.Lane)
		}
	}
	s.accounts = accts
	return nil
}

func (s *Store) loadHealth() error {
	b, err := os.ReadFile(filepath.Join(s.dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	// A corrupt health file must never block startup: health is derived data
	// and re-observing quota costs one request per account.
	var h map[string]*Health
	if err := json.Unmarshal(b, &h); err != nil {
		return nil
	}
	s.health = h
	return nil
}

// Accounts returns the enabled accounts in lane, ordered by selection
// preference: subscriptions before API keys, then by ascending Priority, then
// by id so the order is stable across restarts.
//
// The accounts are clones. Callers hold one for the length of a request while
// another goroutine may be refreshing its token, so handing out the pool's own
// pointers would be a data race.
func (s *Store) Accounts(lane LaneID) []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*Account
	for _, a := range s.accounts {
		if a.Lane == lane && a.Enabled {
			out = append(out, a.Clone())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		if (ai.Kind == KindOAuth) != (aj.Kind == KindOAuth) {
			return ai.Kind == KindOAuth
		}
		if ai.Priority != aj.Priority {
			return ai.Priority < aj.Priority
		}
		return ai.ID < aj.ID
	})
	return out
}

// All returns a clone of every account, enabled or not, for `agentswap status`.
func (s *Store) All() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.Clone())
	}
	return out
}

// Get returns a clone of the account with the given id.
func (s *Store) Get(id string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a.Clone(), nil
		}
	}
	return nil, fmt.Errorf("%q: %w", id, ErrNotFound)
}

// Upsert adds an account or replaces the one with a matching id, then persists
// accounts.json. The account is cloned on the way in, so a caller that keeps
// mutating its copy afterwards cannot reach into the pool.
func (s *Store) Upsert(a *Account) error {
	if a == nil {
		return errors.New("account required")
	}
	if a.ID == "" {
		return errors.New("account id required")
	}
	if !a.Lane.Valid() {
		return fmt.Errorf("unknown lane %q", a.Lane)
	}
	stored := a.Clone()

	s.mu.Lock()
	replaced := false
	for i, existing := range s.accounts {
		if existing.ID == a.ID {
			s.accounts[i] = stored
			replaced = true
			break
		}
	}
	if !replaced {
		s.accounts = append(s.accounts, stored)
	}
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return writeJSONAtomic(filepath.Join(s.dir, accountsFile), snapshot)
}

// UpdateAccount applies fn to the pool's own copy of an account under the
// lock, then persists.
//
// Token refresh has to be a read-modify-write against the canonical record: a
// caller that renews the token on a clone and Upserts it back would overwrite
// whatever else changed meanwhile — an `agentswap disable` landing during a
// long stream, say.
func (s *Store) UpdateAccount(id string, fn func(*Account)) error {
	s.mu.Lock()
	var found *Account
	for _, a := range s.accounts {
		if a.ID == id {
			found = a
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		return fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	fn(found)
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return writeJSONAtomic(filepath.Join(s.dir, accountsFile), snapshot)
}

// snapshotLocked clones the pool for serialization. Cloning matters as much as
// copying the slice: the write happens outside the lock, and marshalling a
// struct another goroutine may be writing to is a race.
func (s *Store) snapshotLocked() []*Account {
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.Clone())
	}
	return out
}

// Remove deletes an account and its health entry.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	idx := -1
	for i, a := range s.accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	s.accounts = append(s.accounts[:idx], s.accounts[idx+1:]...)
	delete(s.health, id)
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return writeJSONAtomic(filepath.Join(s.dir, accountsFile), snapshot)
}

// Health returns a copy of the account's health. A missing entry reads as a
// fresh available account rather than an error, so a new account needs no
// initialization step.
func (s *Store) Health(id string) Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if h, ok := s.health[id]; ok {
		return *h
	}
	return Health{State: StateAvailable}
}

// MutateHealth applies fn to the account's health under lock. Health is only
// flushed to disk by FlushHealth, so the hot path never blocks on I/O.
func (s *Store) MutateHealth(id string, fn func(*Health)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.health[id]
	if !ok {
		h = &Health{State: StateAvailable}
		s.health[id] = h
	}
	fn(h)
	s.healthDirty = true
}

// FlushHealth persists health if it changed since the last flush.
func (s *Store) FlushHealth() error {
	s.mu.Lock()
	if !s.healthDirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]Health, len(s.health))
	for k, v := range s.health {
		snapshot[k] = *v
	}
	s.healthDirty = false
	s.mu.Unlock()

	return writeJSONAtomic(filepath.Join(s.dir, stateFile), snapshot)
}

// RunHealthFlusher periodically persists health until done is closed.
func (s *Store) RunHealthFlusher(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			_ = s.FlushHealth()
			return
		case <-t.C:
			_ = s.FlushHealth()
		}
	}
}

// writeJSONAtomic writes v as indented JSON to path via a temp file in the same
// directory, fsynced and renamed. Mode is 0600 throughout: these files hold
// live credentials.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
