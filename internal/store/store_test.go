package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st, dir
}

func seed(t *testing.T, st *Store, a *Account) {
	t.Helper()
	if err := st.Upsert(a); err != nil {
		t.Fatalf("Upsert(%s): %v", a.ID, err)
	}
}

func TestAccountsRoundTripAcrossRestart(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{
		ID: "personal", Lane: LaneAnthropic, Kind: KindOAuth, Label: "Personal",
		Enabled: true, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1789000000000,
		Scopes: []string{"user:inference"},
	})

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get("personal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" {
		t.Errorf("tokens did not survive a restart: %+v", got)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "user:inference" {
		t.Errorf("scopes = %v", got.Scopes)
	}
}

// The pool holds live OAuth tokens. World-readable is not an option, and the
// directory matters as much as the file.
func TestCredentialFilesAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits do not apply")
	}
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "secret"})
	st.MutateHealth("a", func(h *Health) { h.Requests = 1 })
	if err := st.FlushHealth(); err != nil {
		t.Fatalf("FlushHealth: %v", err)
	}

	if info, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config dir mode = %o, want no group or world access", perm)
	}
	for _, name := range []string{accountsFile, stateFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group or world access", name, perm)
		}
	}
}

func TestUpsertReplacesByID(t *testing.T) {
	st, _ := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "old"})
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "new"})

	if n := len(st.All()); n != 1 {
		t.Errorf("pool holds %d accounts, want 1", n)
	}
	got, _ := st.Get("a")
	if got.AccessToken != "new" {
		t.Errorf("access token = %q, want the replacement", got.AccessToken)
	}
}

func TestUpsertRejectsIncompleteAccounts(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.Upsert(&Account{Lane: LaneAnthropic}); err == nil {
		t.Error("an account with no id was accepted")
	}
	if err := st.Upsert(&Account{ID: "a", Lane: "gemini"}); err == nil {
		t.Error("an account in an unknown lane was accepted")
	}
	if err := st.Upsert(nil); err == nil {
		t.Error("a nil account was accepted")
	}
}

func TestRemove(t *testing.T) {
	st, _ := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "t"})
	st.MutateHealth("a", func(h *Health) { h.State = StateExhausted })

	if err := st.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := st.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Remove: %v, want ErrNotFound", err)
	}
	// A new account reusing the id must not inherit the old one's health.
	if h := st.Health("a"); h.State != StateAvailable {
		t.Errorf("health survived removal: %+v", h)
	}
	if err := st.Remove("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing twice: %v, want ErrNotFound", err)
	}
}

// Selection order is a policy: subscriptions are already paid for, so they are
// spent before metered keys.
func TestAccountsOrdering(t *testing.T) {
	st, _ := openTemp(t)
	seed(t, st, &Account{ID: "key-1", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, Priority: 0, APIKey: "k"})
	seed(t, st, &Account{ID: "sub-b", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, Priority: 5, AccessToken: "t"})
	seed(t, st, &Account{ID: "sub-a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, Priority: 5, AccessToken: "t"})
	seed(t, st, &Account{ID: "sub-first", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, Priority: 1, AccessToken: "t"})
	seed(t, st, &Account{ID: "off", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: false, AccessToken: "t"})
	seed(t, st, &Account{ID: "other-lane", Lane: LaneOpenAI, Kind: KindOAuth, Enabled: true, AccessToken: "t"})

	var got []string
	for _, a := range st.Accounts(LaneAnthropic) {
		got = append(got, a.ID)
	}
	want := []string{"sub-first", "sub-a", "sub-b", "key-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (subscriptions first, then priority, then id)", got, want)
	}
}

func TestHealthDefaultsToAvailable(t *testing.T) {
	st, _ := openTemp(t)
	// A brand new account has no health entry, and needing an initialization
	// step before first use would be a trap.
	if h := st.Health("never-seen"); !h.Available(time.Now()) {
		t.Errorf("unknown account health = %+v, want available", h)
	}
}

func TestHealthSurvivesRestart(t *testing.T) {
	st, dir := openTemp(t)
	reset := time.Now().Add(time.Hour).Round(time.Second)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "t"})
	st.MutateHealth("a", func(h *Health) {
		h.State = StateExhausted
		h.ResetAt = reset
	})
	if err := st.FlushHealth(); err != nil {
		t.Fatalf("FlushHealth: %v", err)
	}

	// Re-probing an account we already know is spent would waste a request and
	// re-trigger the limit.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	h := reopened.Health("a")
	if h.State != StateExhausted || !h.ResetAt.Equal(reset) {
		t.Errorf("health after restart = %+v, want exhausted until %v", h, reset)
	}
}

// Health is derived data: one request per account rebuilds it. Refusing to
// start over a corrupt copy of it would be a bad trade.
func TestCorruptHealthDoesNotBlockStartup(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "t"})
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with a corrupt state file: %v", err)
	}
	if h := reopened.Health("a"); !h.Available(time.Now()) {
		t.Errorf("health = %+v, want a clean slate", h)
	}
}

// Credentials are not derived data. Silently starting with an empty pool would
// look like every account had vanished.
func TestCorruptAccountsFileIsAnError(t *testing.T) {
	_, dir := openTemp(t)
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("want an error rather than a silently empty pool")
	}
}

func TestUnknownLaneOnDiskIsAnError(t *testing.T) {
	_, dir := openTemp(t)
	body := `[{"id":"a","lane":"gemini","kind":"oauth","enabled":true}]`
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("want an error naming the unknown lane")
	}
}

func TestFlushHealthSkipsCleanState(t *testing.T) {
	st, dir := openTemp(t)
	if err := st.FlushHealth(); err != nil {
		t.Fatalf("FlushHealth: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file written with nothing to save: %v", err)
	}
}

// The hot path mutates health from every request goroutine while the flusher
// serializes it on a ticker.
func TestConcurrentHealthMutationAndFlush(t *testing.T) {
	st, _ := openTemp(t)
	for _, id := range []string{"a", "b", "c"} {
		seed(t, st, &Account{ID: id, Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "t"})
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := string(rune('a' + j%3))
				st.MutateHealth(id, func(h *Health) { h.Requests++ })
				_ = st.Health(id)
				_ = st.Accounts(LaneAnthropic)
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := st.FlushHealth(); err != nil {
					t.Errorf("FlushHealth: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := st.Health("a").Requests; got == 0 {
		t.Error("no requests recorded")
	}
}

func TestUpdateAccount(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "old"})

	if err := st.UpdateAccount("a", func(a *Account) { a.AccessToken = "new" }); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	got, _ := st.Get("a")
	if got.AccessToken != "new" {
		t.Errorf("access token = %q, want the update applied", got.AccessToken)
	}

	// It has to reach disk, or the next start presents a token the upstream
	// has already retired.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, _ := reopened.Get("a"); got.AccessToken != "new" {
		t.Errorf("persisted access token = %q, want the update", got.AccessToken)
	}

	if err := st.UpdateAccount("absent", func(*Account) {}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAccount on a missing id = %v, want ErrNotFound", err)
	}
}

// A refresh landing at the same moment as `agentswap disable` must not undo it.
func TestUpdateAccountDoesNotClobberConcurrentEdits(t *testing.T) {
	st, _ := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true, AccessToken: "old"})

	// A stale clone, as a request holds for its whole lifetime.
	stale, _ := st.Get("a")

	if err := st.UpdateAccount("a", func(a *Account) { a.Enabled = false }); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := st.UpdateAccount("a", func(a *Account) { a.AccessToken = "refreshed" }); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got, _ := st.Get("a")
	if got.Enabled {
		t.Error("the refresh re-enabled a disabled account")
	}
	if got.AccessToken != "refreshed" {
		t.Errorf("access token = %q, want the refreshed one", got.AccessToken)
	}
	if stale.AccessToken != "old" {
		t.Errorf("the caller's clone was mutated behind its back: %q", stale.AccessToken)
	}
}

// An interrupted write must leave the previous pool intact rather than a
// half-written file, and must not leave temp files behind either.
func TestWritesAreAtomic(t *testing.T) {
	st, dir := openTemp(t)
	for i := 0; i < 5; i++ {
		seed(t, st, &Account{ID: string(rune('a' + i)), Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatal(err)
	}
	var accounts []*Account
	if err := json.Unmarshal(b, &accounts); err != nil {
		t.Fatalf("accounts.json is not valid json: %v", err)
	}
	if len(accounts) != 5 {
		t.Errorf("accounts.json holds %d entries, want 5", len(accounts))
	}
}

func TestLaneIDValid(t *testing.T) {
	for _, l := range []LaneID{LaneAnthropic, LaneOpenAI} {
		if !l.Valid() {
			t.Errorf("%s is not valid", l)
		}
	}
	for _, l := range []LaneID{"", "gemini", "Anthropic"} {
		if l.Valid() {
			t.Errorf("%q was accepted as a lane", l)
		}
	}
}

// accounts.json is documented as hand-editable, so an entry that omits the
// optional keys has to behave the way someone writing it would expect.
func TestHandWrittenAccountDefaultsToEnabled(t *testing.T) {
	_, dir := openTemp(t)
	body := `[{"id":"mine","lane":"anthropic","kind":"api_key","api_key":"sk-ant"}]`
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := len(st.Accounts(LaneAnthropic)); n != 1 {
		t.Errorf("the lane has %d usable accounts, want 1: an absent \"enabled\" made it inert", n)
	}
}

func TestOpenReadOnlyDoesNotCreateAnAbsentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent", "agentswap")
	st, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if len(st.All()) != 0 {
		t.Errorf("fresh read-only store has %d accounts", len(st.All()))
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenReadOnly created or touched %s: %v", dir, err)
	}
}

// Disabling is the deliberate act and has to survive a round trip.
func TestExplicitlyDisabledSurvives(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "off", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: false, APIKey: "k"})

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := len(reopened.Accounts(LaneAnthropic)); n != 0 {
		t.Errorf("the lane has %d usable accounts, want 0", n)
	}
	got, err := reopened.Get("off")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Error("a disabled account came back enabled")
	}
}

// A state file from a newer version must not stop an older one starting.
func TestUnknownFieldsAreTolerated(t *testing.T) {
	_, dir := openTemp(t)
	accounts := `[{"id":"a","lane":"anthropic","kind":"api_key","api_key":"k","future_field":42}]`
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte(accounts), 0o600); err != nil {
		t.Fatal(err)
	}
	state := `{"a":{"state":"available","future_field":"whatever"}}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := len(st.Accounts(LaneAnthropic)); n != 1 {
		t.Errorf("the lane has %d accounts, want 1", n)
	}
}

func TestSweepStaleTemps(t *testing.T) {
	_, dir := openTemp(t)

	stale := filepath.Join(dir, tempPrefix+"abandoned")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	// Another agentswap could be mid-write in this directory right now, and
	// removing its temp file would fail its rename.
	fresh := filepath.Join(dir, tempPrefix+"inflight")
	if err := os.WriteFile(fresh, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an abandoned temp file survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a temp file that may still be in use was removed: %v", err)
	}
}

// Concurrent writes to the same file must not collide. On Unix a rename simply
// wins; on Windows two replaces of one path fail with a sharing violation, so
// the store serializes its own writes rather than relying on the platform.
func TestConcurrentWritesToBothFiles(t *testing.T) {
	st, dir := openTemp(t)
	for _, id := range []string{"a", "b", "c"} {
		seed(t, st, &Account{ID: id, Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "k"})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Health flushes, token refreshes and enable/disable all land on disk, and
	// on a busy daemon they overlap.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				id := string(rune('a' + j%3))
				st.MutateHealth(id, func(h *Health) { h.Requests++ })
				if err := st.FlushHealth(); err != nil {
					errs <- err
					return
				}
				if err := st.UpdateAccount(id, func(a *Account) {
					a.AccessToken = "refreshed"
				}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write failed under concurrency: %v", err)
	}

	// Whatever the interleaving, what landed has to be readable.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after concurrent writes: %v", err)
	}
	if n := len(reopened.Accounts(LaneAnthropic)); n != 3 {
		t.Errorf("pool holds %d accounts after concurrent writes, want 3", n)
	}
}

// Pooling one login twice is worse than pooling it once: the pool looks like
// failover and both entries are refused in the same instant.
func TestSameCredentialAs(t *testing.T) {
	oauth := func(access, refresh, chatgpt string) *Account {
		return &Account{
			Lane: LaneAnthropic, Kind: KindOAuth,
			AccessToken: access, RefreshToken: refresh, ChatGPTAccountID: chatgpt,
		}
	}

	t.Run("same tokens are the same login", func(t *testing.T) {
		if !oauth("a", "r", "").SameCredentialAs(oauth("a", "r", "")) {
			t.Error("identical credentials read as different accounts")
		}
	})

	t.Run("a refreshed access token is still the same login", func(t *testing.T) {
		// Refresh rotates the access token constantly, so it cannot be the only
		// handle on identity.
		if !oauth("old", "shared-refresh", "").SameCredentialAs(oauth("new", "shared-refresh", "")) {
			t.Error("a rotated access token made one account look like two")
		}
	})

	t.Run("the workspace id decides when there is one", func(t *testing.T) {
		a := &Account{Lane: LaneOpenAI, Kind: KindOAuth, AccessToken: "x", ChatGPTAccountID: "acct-1"}
		b := &Account{Lane: LaneOpenAI, Kind: KindOAuth, AccessToken: "y", ChatGPTAccountID: "acct-1"}
		if !a.SameCredentialAs(b) {
			t.Error("the same workspace read as two accounts")
		}
		c := &Account{Lane: LaneOpenAI, Kind: KindOAuth, AccessToken: "x", ChatGPTAccountID: "acct-2"}
		if a.SameCredentialAs(c) {
			t.Error("two workspaces read as one account")
		}
	})

	t.Run("different logins are different", func(t *testing.T) {
		if oauth("a", "r1", "").SameCredentialAs(oauth("b", "r2", "")) {
			t.Error("two separate logins read as one")
		}
	})

	t.Run("api keys compare by key", func(t *testing.T) {
		a := &Account{Lane: LaneAnthropic, Kind: KindAPIKey, APIKey: "sk-1"}
		if !a.SameCredentialAs(&Account{Lane: LaneAnthropic, Kind: KindAPIKey, APIKey: "sk-1"}) {
			t.Error("the same key read as two accounts")
		}
		if a.SameCredentialAs(&Account{Lane: LaneAnthropic, Kind: KindAPIKey, APIKey: "sk-2"}) {
			t.Error("two keys read as one")
		}
	})

	t.Run("lanes and kinds never mix", func(t *testing.T) {
		a := &Account{Lane: LaneAnthropic, Kind: KindAPIKey, APIKey: "same"}
		if a.SameCredentialAs(&Account{Lane: LaneOpenAI, Kind: KindAPIKey, APIKey: "same"}) {
			t.Error("a key matched across lanes")
		}
		if a.SameCredentialAs(&Account{Lane: LaneAnthropic, Kind: KindOAuth, AccessToken: "same"}) {
			t.Error("a key matched a subscription")
		}
	})

	t.Run("empty credentials never match", func(t *testing.T) {
		// Two half-written entries are not evidence of anything.
		if (&Account{Lane: LaneAnthropic, Kind: KindAPIKey}).SameCredentialAs(
			&Account{Lane: LaneAnthropic, Kind: KindAPIKey}) {
			t.Error("two empty api keys matched")
		}
		if (&Account{Lane: LaneAnthropic, Kind: KindOAuth}).SameCredentialAs(
			&Account{Lane: LaneAnthropic, Kind: KindOAuth}) {
			t.Error("two empty token sets matched")
		}
	})

	if (&Account{}).SameCredentialAs(nil) {
		t.Error("nil matched")
	}
}

// The daemon reads the pool once, at startup. Without a reload, every fix made
// from the CLI is invisible to it until a restart — which is the whole
// recovery path silently not working.
func TestReloadAdoptsChangesFromDisk(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "old"})

	// Another process — the CLI — replaces the key and adds an account.
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, other, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "new"})
	seed(t, other, &Account{ID: "b", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "second"})

	// Until it reloads, the daemon is still serving the old pool.
	if got, _ := st.Get("a"); got.AccessToken == "new" {
		t.Fatal("the test cannot observe the bug it is checking")
	}

	changed, err := st.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got, _ := st.Get("a"); got.APIKey != "new" {
		t.Errorf("key = %q, want the replacement", got.APIKey)
	}
	if len(st.All()) != 2 {
		t.Errorf("pool holds %d accounts, want the new one adopted", len(st.All()))
	}
	// Both are worth resetting: one credential was replaced, the other is new.
	if len(changed) != 2 {
		t.Errorf("changed = %v, want both accounts reported", changed)
	}
}

// A credential that has not changed is not news, and resetting its health
// would forget a quota window that is still true.
func TestReloadReportsOnlyRealChanges(t *testing.T) {
	st, dir := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "same"})

	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A label change touches the file without touching the credential.
	if err := other.UpdateAccount("a", func(a *Account) { a.Label = "renamed" }); err != nil {
		t.Fatal(err)
	}

	changed, err := st.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none: the credential is the same", changed)
	}
	if got, _ := st.Get("a"); got.Label != "renamed" {
		t.Errorf("label = %q, want the change adopted anyway", got.Label)
	}
}

// A reload arriving while a token is being renewed must not put the old
// credential back.
//
// The two overlap in real use: everything that changes the pool tells the
// daemon to reload, and the daemon renews tokens whenever one expires, so
// `agentswap login` during a busy session is enough. Reverting is not a lost
// write — the upstream retires the old refresh token the moment it issues the
// new one, so a daemon left holding the old copy would present a retired
// credential, be refused, and mark a healthy account rejected.
func TestReloadDoesNotRevertATokenBeingRefreshed(t *testing.T) {
	st, dir := openTemp(t)

	for i := 0; i < 200; i++ {
		seed(t, st, &Account{
			ID: "a", Lane: LaneAnthropic, Kind: KindOAuth, Enabled: true,
			AccessToken: "v0", RefreshToken: "r0",
		})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := st.UpdateAccount("a", func(a *Account) {
				a.AccessToken, a.RefreshToken = "v1", "r1"
			}); err != nil {
				t.Errorf("UpdateAccount: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := st.Reload(); err != nil {
				t.Errorf("Reload: %v", err)
			}
		}()
		wg.Wait()

		b, err := os.ReadFile(filepath.Join(dir, accountsFile))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk []*Account
		if err := json.Unmarshal(b, &onDisk); err != nil {
			t.Fatal(err)
		}
		mem, err := st.Get("a")
		if err != nil {
			t.Fatal(err)
		}
		if len(onDisk) != 1 || mem.RefreshToken != onDisk[0].RefreshToken {
			t.Fatalf("run %d: the daemon holds refresh token %q while the file holds %q",
				i, mem.RefreshToken, onDisk[0].RefreshToken)
		}
	}
}

func TestResetHealth(t *testing.T) {
	st, _ := openTemp(t)
	seed(t, st, &Account{ID: "a", Lane: LaneAnthropic, Kind: KindAPIKey, Enabled: true, APIKey: "k"})
	st.MutateHealth("a", func(h *Health) {
		h.State = StateInvalid
		h.LastError = "refused"
	})

	st.ResetHealth("a")
	if h := st.Health("a"); h.State != StateAvailable || h.LastError != "" {
		t.Errorf("health = %+v, want a clean slate", h)
	}
}
