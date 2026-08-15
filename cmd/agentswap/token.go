package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/agentswap/internal/store"
)

// cmdAddToken pools a long-lived bearer token.
//
// This is the answer to an imported subscription going stale. Importing copies
// the credential your CLI is *currently* using, and both upstreams rotate the
// refresh token when it is used — so whichever side renews first retires the
// other's copy. The CLI renews roughly every eight hours, which puts a ceiling
// on how long an imported credential lasts no matter what agentswap does.
//
// A long-lived token is a separate credential. The CLI never renews it,
// because it is not the CLI's session, so there is nothing to race. Get one
// with `claude setup-token`.
func cmdAddToken(args []string) error {
	fs := flag.NewFlagSet("add-token", flag.ExitOnError)
	token := fs.String("token", "", "the token; `-` reads it from stdin (also: AGENTSWAP_API_KEY, or a prompt)")
	id := fs.String("id", "", "account id")
	label := fs.String("label", "", "human-readable label")
	baseURL := fs.String("base-url", "", "override the upstream")
	priority := fs.Int("priority", 0, "lower is preferred within a lane")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap add-token <anthropic|openai> [--token TOKEN] [flags]")
		fmt.Fprintln(os.Stderr, "\nPools a long-lived token, which is not tied to your CLI's session and so")
		fmt.Fprintln(os.Stderr, "does not go stale when the CLI renews its own credential.")
		fmt.Fprintln(os.Stderr, "\n  claude setup-token                      # issue one")
		fmt.Fprintln(os.Stderr, "  agentswap add-token anthropic           # then paste it here")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}

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
	secret, err := readSecret(*token, "--token")
	if err != nil {
		return err
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}

	// Stored as a subscription credential, because that is what it is: sent as
	// a bearer token, spent before metered keys, and not something to refresh.
	candidate := &store.Account{
		Lane: laneID, Kind: store.KindOAuth, Enabled: true,
		AccessToken: secret, BaseURL: *baseURL, Priority: *priority,
	}
	if existing := matchingAccount(st, candidate); existing != nil && (*id == "" || *id == existing.ID) {
		fmt.Printf("that token is already in the pool as %q\n", existing.ID)
		return nil
	}

	accountID := *id
	if accountID == "" {
		accountID = nextFreeID(string(laneID)+"-token", takenIDs(st))
	}
	replacing := false
	if prior, err := st.Get(accountID); err == nil {
		replacing = true
		if *label == "" {
			*label = prior.Label
		}
	}
	candidate.ID = accountID
	candidate.Label = *label
	if candidate.Label == "" {
		candidate.Label = accountID
	}

	if err := st.Upsert(candidate); err != nil {
		return err
	}
	// A replaced credential deserves a fresh verdict, and a running daemon has
	// to hear about it or the fix does nothing until it restarts.
	st.ResetHealth(accountID)
	if err := st.FlushHealth(); err != nil {
		fmt.Printf("  (could not clear the old health record: %v)\n", err)
	}
	notifyDaemon(accountID)

	verb := "added"
	if replacing {
		verb = "replaced the token for"
	}
	fmt.Printf("%s %q (%s) in the %s lane\n", verb, accountID, maskKey(secret), laneID)
	fmt.Println("This one is not tied to your CLI's session, so it will not go stale when the CLI renews.")
	return nil
}
