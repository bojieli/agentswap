package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/agentswap/internal/importer"
	"github.com/bojieli/agentswap/internal/store"
)

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	id := fs.String("id", "", "account id (default: derived from the lane)")
	label := fs.String("label", "", "human-readable label")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap import [--id ID] [--label NAME]")
		fmt.Fprintln(os.Stderr, "\nAdopts the credentials already stored by `claude` and `codex`.")
		fmt.Fprintln(os.Stderr, "To pool several accounts, log in as each one and run import again with a new --id.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}

	sources := []struct {
		lane store.LaneID
		load func() (*store.Account, error)
	}{
		{store.LaneAnthropic, importer.ImportClaude},
		{store.LaneOpenAI, importer.ImportCodex},
	}

	var found []*store.Account
	for _, s := range sources {
		a, err := s.load()
		if err != nil {
			// A machine logged into only one of the two CLIs is the normal
			// case, not a failure.
			if errors.Is(err, importer.ErrNoCredentials) {
				fmt.Printf("  %-10s skipped: not logged in\n", s.lane)
				continue
			}
			return err
		}
		found = append(found, a)
	}
	if len(found) == 0 {
		return errors.New("nothing to import; log in with `claude` or `codex login` first")
	}

	taken := takenIDs(st)
	nameImports(found, *id, *label, taken)

	for _, a := range found {
		verb := "imported"
		if taken[a.ID] {
			// Re-importing the same id is how you refresh a credential that has
			// gone stale, so say which it was rather than looking like a no-op.
			verb = "updated"
		}
		if err := st.Upsert(a); err != nil {
			return err
		}
		kind := "subscription"
		if a.Kind == store.KindAPIKey {
			kind = "api key"
		}
		fmt.Printf("  %-10s %s %q (%s)\n", a.Lane, verb, a.ID, kind)
	}

	fmt.Printf("\n%d account(s) in the pool. Next: agentswap install && agentswap serve\n", len(st.All()))
	return nil
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
	for _, a := range found {
		switch {
		case userID == "":
			a.ID = nextFreeID(string(a.Lane), seen)
		case len(found) == 1:
			a.ID = userID
		default:
			a.ID = fmt.Sprintf("%s-%s", a.Lane, userID)
		}
		seen[a.ID] = true

		switch {
		case userLabel == "":
			a.Label = a.ID
		case len(found) == 1:
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
	key := fs.String("key", "", "API key (required; or set AGENTSWAP_API_KEY)")
	baseURL := fs.String("base-url", "", "override the upstream, for same-protocol third-party providers")
	id := fs.String("id", "", "account id")
	label := fs.String("label", "", "human-readable label")
	priority := fs.Int("priority", 100, "lower is preferred; keys are tried after subscriptions regardless")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap add-key <anthropic|openai> --key KEY [flags]")
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

	secret := *key
	if secret == "" {
		secret = os.Getenv("AGENTSWAP_API_KEY")
	}
	if secret == "" {
		return errors.New("--key is required (or set AGENTSWAP_API_KEY to keep it out of your shell history)")
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	accountID := *id
	if accountID == "" {
		accountID = nextFreeID(string(laneID)+"-key", takenIDs(st))
	}
	lbl := *label
	if lbl == "" {
		lbl = accountID
	}

	a := &store.Account{
		ID: accountID, Lane: laneID, Kind: store.KindAPIKey, Label: lbl,
		Priority: *priority, Enabled: true, APIKey: secret, BaseURL: *baseURL,
	}
	if err := st.Upsert(a); err != nil {
		return err
	}
	fmt.Printf("added %q to the %s lane\n", accountID, laneID)
	return nil
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
	fmt.Printf("%-18s %-10s %-14s %-4s %s\n", "ID", "LANE", "KIND", "PRI", "STATE")
	for _, a := range all {
		kind := "subscription"
		if a.Kind == store.KindAPIKey {
			kind = "api key"
		}
		state := string(st.Health(a.ID).State)
		if !a.Enabled {
			state = "disabled"
		}
		fmt.Printf("%-18s %-10s %-14s %-4d %s\n", a.ID, a.Lane, kind, a.Priority, state)
	}
	return nil
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
	fmt.Printf("%sd %q\n", verb, a.ID)
	return nil
}
