package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when an account id is not in the pool.
var ErrNotFound = errors.New("account not found")

const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"

	// tempPrefix marks the scratch file an atomic write renames into place.
	tempPrefix = ".tmp-"
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

	// writeMu serializes writes to the files. The data lock is deliberately
	// released before writing, so readers are never blocked on disk I/O — which
	// also means two goroutines can reach the write at once. On Windows two
	// concurrent replaces of one path fail with a sharing violation instead of
	// one simply winning.
	writeMu sync.Mutex
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
		_ = os.Chmod(dir, 0o700) //nolint:gosec // 0700 is correct for a directory; the rule is about files
	}
	sweepStaleTemps(dir)

	s := &Store{dir: dir, health: map[string]*Health{}}
	if err := s.loadAccounts(); err != nil {
		return nil, err
	}
	if err := s.loadHealth(); err != nil {
		return nil, err
	}
	return s, nil
}

// tempStaleAfter is how old a temp file must be before it is assumed abandoned.
// Another agentswap may be mid-write in this directory right now, and deleting
// its temp file would fail its rename; nothing legitimate takes an hour.
const tempStaleAfter = time.Hour

// sweepStaleTemps removes leftovers from a write that was killed between
// creating the temp file and renaming it. Best effort throughout: a config
// directory slowly filling with debris is untidy, but not a reason to refuse
// to start.
func sweepStaleTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempStaleAfter)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
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
	if err := validateAccounts(accts); err != nil {
		return err
	}
	s.accounts = accts
	return nil
}

// validateAccounts rejects a pool that cannot be addressed unambiguously.
//
// Only that. A single unusable account must not stop the pool loading, or the
// commands needed to see and fix it — `list`, `set`, `remove` — would be the
// ones that no longer run. Those problems are reported per account instead;
// see Account.Problem.
//
// An id, by contrast, is how everything addresses an account: two accounts
// sharing one silently halve the pool, because every command and the health
// record would only ever reach the first.
func validateAccounts(accts []*Account) error {
	seen := map[string]bool{}
	for i, a := range accts {
		where := fmt.Sprintf("account %d in accounts.json", i+1)
		if a.ID != "" {
			where = fmt.Sprintf("account %q", a.ID)
		}

		switch {
		case a.ID == "":
			return fmt.Errorf("%s has no id", where)
		case seen[a.ID]:
			return fmt.Errorf("accounts.json has two accounts with the id %q; "+
				"an id addresses one account, so they need different names", a.ID)
		case !a.Lane.Valid():
			return fmt.Errorf("%s: unknown lane %q; want anthropic or openai", where, a.Lane)
		}
		seen[a.ID] = true
	}
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
		return nil //nolint:nilerr // deliberate: health is derived data, and refusing to start over a corrupt copy of it would be a worse failure than rebuilding it
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
		// Problem() accounts are excluded here rather than at load: they are
		// visible to `list` and `doctor`, which is where they can be fixed,
		// but sending a request with a credential we know is missing only
		// spends a round trip to be told so.
		if a.Lane == lane && a.Enabled && a.Problem() == "" {
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

	return s.writeJSON(accountsFile, snapshot)
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

	return s.writeJSON(accountsFile, snapshot)
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

	return s.writeJSON(accountsFile, snapshot)
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

	return s.writeJSON(stateFile, snapshot)
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

// writeJSON persists one of the store's files, one write at a time.
func (s *Store) writeJSON(name string, v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeJSONAtomic(filepath.Join(s.dir, name), v)
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
	f, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmp := f.Name()
	// No-op once the rename succeeds; nothing useful to do if it fails.
	defer func() { _ = os.Remove(tmp) }()

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameWithRetry(tmp, path)
}

// renameWithRetry replaces path, retrying briefly.
//
// A rename onto a path some other handle has open fails on Windows with a
// sharing violation — another agentswap process writing the same file, or an
// antivirus scanner that opened what we just wrote, is enough. Elsewhere the
// first attempt always succeeds, so this costs nothing.
func renameWithRetry(tmp, path string) error {
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

// renameAttempts bounds the retry at roughly half a second in total, which is
// far longer than a scanner holds a small file and short enough that a real
// permissions problem still surfaces promptly.
const renameAttempts = 10
