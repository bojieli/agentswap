package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bojieli/agentswap/internal/importer"
	"github.com/bojieli/agentswap/internal/store"
)

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	id := fs.String("id", "", "account id (default: derived from the lane)")
	label := fs.String("label", "", "human-readable label")
	dryRun := fs.Bool("dry-run", false, "show what would be imported without writing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap import [--id ID] [--label NAME]")
		fmt.Fprintln(os.Stderr, "\nAdopts the logins and active provider overrides stored by `claude` and `codex`.")
		fmt.Fprintln(os.Stderr, "To pool several accounts, log in as each one and run import again with a new --id.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	sources := []struct {
		lane store.LaneID
		load func() ([]*store.Account, error)
	}{
		{store.LaneAnthropic, importer.ImportClaudeAll},
		{store.LaneOpenAI, importer.ImportCodexAll},
	}

	var found []*store.Account
	for _, s := range sources {
		accounts, err := s.load()
		if err != nil {
			// A machine logged into only one of the two CLIs is the normal
			// case, not a failure.
			if errors.Is(err, importer.ErrNoCredentials) {
				fmt.Printf("  %-10s skipped: not logged in\n", s.lane)
				continue
			}
			return err
		}
		found = append(found, accounts...)
	}
	if len(found) == 0 {
		return errors.New("nothing to import; log in with `claude` or `codex login` first")
	}

	var st *store.Store
	var err error
	if *dryRun {
		st, _, err = openStoreReadOnly()
	} else {
		st, _, err = openStore()
	}
	if err != nil {
		return err
	}
	prepareImports(st, found, *id, *label)

	if *dryRun {
		for _, a := range found {
			kind := "subscription"
			if a.Kind == store.KindAPIKey {
				kind = "api key"
			}
			upstream := "vendor default"
			if a.BaseURL != "" {
				upstream = a.BaseURL
			}
			verb := "would import"
			if matchingAccount(st, a) != nil {
				verb = "would refresh"
			}
			fmt.Printf("  %-10s %s %q (%s, %s)\n", a.Lane, verb, a.ID, kind, upstream)
		}
		fmt.Println("\n(dry run — no pool files were written)")
		return nil
	}

	taken := takenIDs(st)

	for _, a := range found {
		// A credential already in the pool is the same login, not a second one.
		// Adding it again would show two accounts that are refused in the same
		// instant, which looks exactly like failover until you need it.
		if existing := matchingAccount(st, a); existing != nil {
			if err := st.Upsert(a); err != nil {
				return err
			}
			fmt.Printf("  %-10s refreshed %q — already in the pool, not added twice\n", a.Lane, a.ID)
			continue
		}
		if err := st.Upsert(a); err != nil {
			return err
		}
		verb := "imported"
		if taken[a.ID] {
			// Re-importing under an id that exists is how you replace a stale
			// credential, so say which it was rather than looking like a no-op.
			verb = "updated"
		}
		kind := "subscription"
		if a.Kind == store.KindAPIKey {
			kind = "api key"
		}
		fmt.Printf("  %-10s %s %q (%s)\n", a.Lane, verb, a.ID, kind)
	}

	notifyDaemon()
	fmt.Printf("\n%d account(s) in the pool. Next: agentswap install && agentswap serve\n", len(st.All()))
	reportEnvKeys(st)
	return nil
}

func prepareImports(st *store.Store, found []*store.Account, id, label string) {
	taken := takenIDs(st)
	// Reserve the identities of credentials already in the pool before naming
	// genuinely new discoveries. Otherwise importing a native subscription and
	// a provider override together can skip anthropic-2 merely because the
	// already-known subscription temporarily consumed that candidate name.
	for _, a := range found {
		if existing := matchingAccount(st, a); existing != nil {
			a.ID, a.Label = existing.ID, existing.Label
		}
	}
	nameImports(found, id, label, taken)
}

// matchingAccount finds an account already holding this credential.
func matchingAccount(st *store.Store, a *store.Account) *store.Account {
	for _, existing := range st.All() {
		if existing.SameCredentialAs(a) {
			return existing
		}
	}
	return nil
}

// reportEnvKeys mentions API keys sitting in the environment, without adopting
// them. Pooling a credential the user never named would be a surprise, but so
// is not knowing that the fallback you assumed was there is not.
func reportEnvKeys(st *store.Store) {
	pooled := map[string]bool{}
	for _, a := range st.All() {
		if a.APIKey != "" {
			pooled[a.APIKey] = true
		}
	}
	for _, env := range []struct {
		name string
		lane store.LaneID
	}{
		{"ANTHROPIC_API_KEY", store.LaneAnthropic},
		{"OPENAI_API_KEY", store.LaneOpenAI},
	} {
		key := os.Getenv(env.name)
		if key == "" || pooled[key] {
			continue
		}
		fmt.Printf("\n%s is set in your environment but not in the pool.\n", env.name)
		fmt.Printf("  Add it as a fallback with: %s | agentswap add-key %s --key -\n",
			"echo \"$"+env.name+"\"", env.lane)
	}
}

// nameImports assigns ids and labels to the accounts one import turned up.
//
// A single credential takes --id verbatim: `agentswap import --id work` is the
// documented way to name the second account you pooled. Two credentials from
// one import cannot share an id, so each takes its lane as a prefix — which is
// also the only case where an unqualified name would be ambiguous.
func nameImports(found []*store.Account, userID, userLabel string, taken map[string]bool) {
	seen := map[string]bool{}
	for k := range taken {
		seen[k] = true
	}
	unnamed := 0
	for _, a := range found {
		if a.ID == "" {
			unnamed++
		}
	}
	for _, a := range found {
		if a.ID != "" {
			seen[a.ID] = true
			continue
		}
		switch {
		case userID == "":
			a.ID = nextFreeID(string(a.Lane), seen)
		case unnamed == 1:
			a.ID = userID
		default:
			base := fmt.Sprintf("%s-%s", a.Lane, userID)
			a.ID = base
			if seen[a.ID] {
				a.ID = nextFreeID(base, seen)
			}
		}
		seen[a.ID] = true

		switch {
		case userLabel == "":
			a.Label = a.ID
		case unnamed == 1:
			a.Label = userLabel
		default:
			a.Label = fmt.Sprintf("%s (%s)", userLabel, a.Lane)
		}
	}
}

func takenIDs(st *store.Store) map[string]bool {
	taken := map[string]bool{}
	for _, a := range st.All() {
		taken[a.ID] = true
	}
	return taken
}

// nextFreeID picks the first unused id of the form "<prefix>-N", so importing a
// second account never silently overwrites the first.
func nextFreeID(prefix string, taken map[string]bool) string {
	for i := 1; ; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if !taken[id] {
			return id
		}
	}
}

func cmdAddKey(args []string) error {
	fs := flag.NewFlagSet("add-key", flag.ExitOnError)
	key := fs.String("key", "", "API key; `-` reads it from stdin (also: AGENTSWAP_API_KEY, or a prompt)")
	baseURL := fs.String("base-url", "", "override the upstream, for same-protocol third-party providers")
	id := fs.String("id", "", "account id")
	label := fs.String("label", "", "human-readable label")
	priority := fs.Int("priority", 100, "lower is preferred; keys are tried after subscriptions regardless")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap add-key <anthropic|openai> [--key KEY] [flags]")
		fmt.Fprintln(os.Stderr, "\nThe key can come from anywhere that keeps it out of your shell history:")
		fmt.Fprintln(os.Stderr, "  agentswap add-key anthropic                      # prompts, with the echo off")
		fmt.Fprintln(os.Stderr, "  echo \"$ANTHROPIC_API_KEY\" | agentswap add-key anthropic --key -")
		fmt.Fprintln(os.Stderr, "  AGENTSWAP_API_KEY=sk-ant-... agentswap add-key anthropic")
		fmt.Fprintln(os.Stderr, "\nA third-party provider that speaks the same protocol needs --base-url.")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	// The lane is positional and must come off the front before parsing:
	// flag.Parse stops at the first non-flag argument, so `add-key anthropic
	// --key X` would otherwise silently ignore --key.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return errors.New("a lane is required: anthropic or openai")
	}
	laneArg, rest := args[0], args[1:]
	if err := fs.Parse(rest); err != nil {
		return err
	}

	laneID := store.LaneID(laneArg)
	if !laneID.Valid() {
		return fmt.Errorf("unknown lane %q; want anthropic or openai", laneID)
	}

	secret, err := readSecret(*key, "--key")
	if err != nil {
		return err
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}

	// The same key twice is two entries that fail together, which looks like a
	// fallback right up until it is needed.
	candidate := &store.Account{Lane: laneID, Kind: store.KindAPIKey, APIKey: secret}
	if existing := matchingAccount(st, candidate); existing != nil && (*id == "" || *id == existing.ID) {
		fmt.Printf("that key is already in the pool as %q\n", existing.ID)
		return nil
	}

	accountID := *id
	if accountID == "" {
		accountID = nextFreeID(string(laneID)+"-key", takenIDs(st))
	}
	lbl := *label
	if lbl == "" {
		lbl = accountID
	}
	replacing := false
	if prior, err := st.Get(accountID); err == nil {
		replacing = true
		if lbl == accountID && prior.Label != "" {
			lbl = prior.Label
		}
	}

	a := &store.Account{
		ID: accountID, Lane: laneID, Kind: store.KindAPIKey, Label: lbl,
		Priority: *priority, Enabled: true, APIKey: secret, BaseURL: *baseURL,
	}
	if err := st.Upsert(a); err != nil {
		return err
	}
	notifyDaemon()
	verb := "added"
	if replacing {
		verb = "replaced the key for"
	}
	fmt.Printf("%s %q (%s) in the %s lane\n", verb, accountID, maskKey(secret), laneID)
	if *baseURL != "" {
		fmt.Printf("  upstream: %s\n", *baseURL)
		warnAboutBaseURLPath(laneID, *baseURL)
	}
	return nil
}

// readSecret resolves the key from wherever the user put it, preferring the
// ways that keep it out of shell history.
//
// A secret on the command line ends up in ~/.zsh_history and in the process
// list, so `--key -` reads it from a pipe, an unset key falls back to the
// environment, and an interactive terminal is prompted with the echo off.
func readSecret(flagValue, flagName string) (string, error) {
	if flagValue == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read the key from stdin: %w", err)
		}
		if key := strings.TrimSpace(string(b)); key != "" {
			return key, nil
		}
		return "", errors.New("nothing arrived on stdin")
	}
	if flagValue != "" {
		return flagValue, nil
	}
	if key := os.Getenv("AGENTSWAP_API_KEY"); key != "" {
		return key, nil
	}
	if key, err := promptSecret(strings.TrimPrefix(flagName, "--") + ": "); err == nil && key != "" {
		return key, nil
	}
	// Naming the flag this command actually takes: being told to use --key by
	// a command that has no --key is its own small maze.
	return "", fmt.Errorf("nothing given. Pipe it in with `%s -`, set AGENTSWAP_API_KEY, "+
		"or run this from a terminal to be prompted", flagName)
}

// maskKey renders a key as something a human can tell apart from another key
// without it being a secret on screen or in a scrollback buffer.
func maskKey(key string) string {
	const tail = 4
	if len(key) <= tail {
		return "…"
	}
	prefix := ""
	// The vendor prefixes are the useful part: they say what kind of key it is.
	for _, p := range []string{"sk-ant-api", "sk-ant-oat", "sk-ant", "sk-proj", "sk-"} {
		if strings.HasPrefix(key, p) {
			prefix = p
			break
		}
	}
	return prefix + "…" + key[len(key)-tail:]
}

func cmdList([]string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	all := st.All()
	if len(all) == 0 {
		fmt.Println("no accounts yet — run `agentswap import`")
		return nil
	}
	fmt.Printf("%-18s %-10s %-14s %-4s %-10s %s\n", "ID", "LANE", "KIND", "PRI", "STATE", "CREDENTIAL")
	for _, a := range all {
		kind := "subscription"
		if a.Kind == store.KindAPIKey {
			kind = "api key"
		}
		state := string(st.Health(a.ID).State)
		if state == "" {
			state = string(store.StateAvailable)
		}
		if !a.Enabled {
			state = "disabled"
		}
		if a.Problem() != "" {
			// It cannot serve a request, whatever its health says.
			state = "unusable"
		}
		fmt.Printf("%-18s %-10s %-14s %-4d %-10s %s\n",
			truncate(a.ID, 18), a.Lane, kind, a.Priority, state, credentialSummary(a))
	}
	return nil
}

// credentialSummary says which credential an entry holds, without printing it.
//
// Someone with three keys — one official, one from a gateway, one from a
// colleague — cannot otherwise tell which row is which, and "remove the wrong
// one" is an unpleasant way to find out.
func credentialSummary(a *store.Account) string {
	if problem := a.Problem(); problem != "" {
		return problem
	}
	var parts []string
	if a.Kind == store.KindAPIKey && a.APIKey != "" {
		parts = append(parts, maskKey(a.APIKey))
	}
	if a.Kind == store.KindOAuth && a.RefreshToken == "" && a.AccessToken != "" {
		// A credential with nothing to refresh is a long-lived token, and that
		// is the useful thing to know about it: it does not go stale when the
		// CLI renews its own session.
		parts = append(parts, "long-lived "+maskKey(a.AccessToken))
	}
	if a.SubscriptionType != "" {
		parts = append(parts, a.SubscriptionType)
	}
	if a.BaseURL != "" {
		parts = append(parts, "→ "+hostOf(a.BaseURL))
	}
	return strings.Join(parts, "  ")
}

// hostOf is the readable part of a base URL: the scheme and path are noise in
// a table whose job is telling two providers apart.
func hostOf(raw string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

func cmdRemove(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: agentswap remove <account-id>")
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	if err := st.Remove(args[0]); err != nil {
		return err
	}
	notifyDaemon()
	fmt.Printf("removed %q\n", args[0])
	return nil
}

func cmdEnable(args []string) error  { return setEnabled(args, true) }
func cmdDisable(args []string) error { return setEnabled(args, false) }

func setEnabled(args []string, enabled bool) error {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: agentswap %s <account-id>", verb)
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	a, err := st.Get(args[0])
	if err != nil {
		return err
	}
	a.Enabled = enabled
	if err := st.Upsert(a); err != nil {
		return err
	}
	notifyDaemon()
	fmt.Printf("%sd %q\n", verb, a.ID)
	return nil
}
