package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/bojieli/agentswap/internal/store"
)

// cmdSet changes one property of an account already in the pool.
//
// Without this, the only way to move a key to a different gateway or reorder
// two accounts was to add them again with the credential re-supplied, or to
// hand-edit accounts.json — a file that the daemon rewrites underneath you
// whenever a token is refreshed.
func cmdSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	baseURL := fs.String("base-url", "", "upstream for this account; pass \"\" to go back to the default")
	priority := fs.Int("priority", 0, "lower is preferred within a lane")
	label := fs.String("label", "", "human-readable name shown in status")
	key := fs.String("key", "", "replace the API key; `-` reads it from stdin")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap set <account-id> [--base-url URL] [--priority N] [--label NAME] [--key -]")
		fmt.Fprintln(os.Stderr, "\nChanges an account already in the pool. Its id stays the same, so the")
		fmt.Fprintln(os.Stderr, "observed quota and the conversations pinned to it are kept.")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  agentswap set gateway --base-url https://llm.corp.example.com/v1")
		fmt.Fprintln(os.Stderr, "  agentswap set backup --priority 200      # try it last")
		fmt.Fprintln(os.Stderr, "  agentswap set gateway --base-url \"\"       # back to the vendor's own API")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}

	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return errors.New("an account id is required")
	}
	id, rest := args[0], args[1:]
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// Which flags were actually given, so that `--base-url ""` can mean "clear
	// it" while an absent --base-url means "leave it alone".
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if len(given) == 0 {
		fs.Usage()
		return errors.New("nothing to change")
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	account, err := st.Get(id)
	if err != nil {
		return err
	}

	if given["base-url"] && *baseURL != "" {
		if err := validateBaseURL(*baseURL); err != nil {
			return err
		}
	}

	var secret string
	if given["key"] {
		if account.Kind != store.KindAPIKey {
			return fmt.Errorf("%q is a subscription, not an API key; "+
				"replace its credential with `agentswap login --id %s`", id, id)
		}
		if secret, err = readSecret(*key); err != nil {
			return err
		}
	}

	var changes []string
	if err := st.UpdateAccount(id, func(a *store.Account) {
		if given["base-url"] {
			a.BaseURL = *baseURL
			if *baseURL == "" {
				changes = append(changes, "upstream: back to the default")
			} else {
				changes = append(changes, "upstream: "+*baseURL)
			}
		}
		if given["priority"] {
			a.Priority = *priority
			changes = append(changes, fmt.Sprintf("priority: %d", *priority))
		}
		if given["label"] {
			a.Label = *label
			changes = append(changes, "label: "+*label)
		}
		if given["key"] {
			a.APIKey = secret
			changes = append(changes, "key: "+maskKey(secret))
		}
	}); err != nil {
		return err
	}

	// A rejection is evidence about a credential and an upstream. Change
	// either and the evidence is stale — without this, the fix that `status`
	// and the client's error both recommend leaves the account exactly as
	// unusable as it was, which is worse than no advice at all.
	if given["key"] || given["base-url"] {
		st.MutateHealth(id, func(h *store.Health) { *h = store.Health{State: store.StateAvailable} })
		if err := st.FlushHealth(); err != nil {
			fmt.Printf("  (could not clear the old health record: %v)\n", err)
		}
		changes = append(changes, "back in rotation")
	}

	notifyDaemon(id)

	fmt.Printf("%s\n", id)
	for _, c := range changes {
		fmt.Printf("  %s\n", c)
	}
	if given["base-url"] {
		warnAboutBaseURLPath(account.Lane, *baseURL)
	}
	return nil
}

// validateBaseURL rejects an upstream that would fail at the first request,
// when the mistake is still attached to the command that made it.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad base URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base URL %q needs an http:// or https:// scheme", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("base URL %q has no host", raw)
	}
	return nil
}

// warnAboutBaseURLPath flags an upstream that will produce a doubled path.
//
// Claude Code sends `/v1/messages`, and agentswap joins that onto the base
// URL, so an anthropic base ending in `/v1` reaches the provider as
// `/v1/v1/messages` and 404s. Gateways document their endpoint both ways, so
// this is easy to get wrong and hard to diagnose — the request simply fails
// with nothing in it about the extra segment.
//
// A warning rather than an error: a gateway is free to mount its API anywhere,
// and refusing would make agentswap wrong about somebody else's URL scheme.
func warnAboutBaseURLPath(lane store.LaneID, raw string) {
	if raw == "" || lane != store.LaneAnthropic {
		// The openai lane's own default ends in /v1, because Codex sends bare
		// `/responses`. There it is right.
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if !strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/v1") {
		return
	}
	fmt.Printf("\n  note: this ends in /v1, and the client already sends /v1/messages,\n")
	fmt.Printf("        so the provider will see /v1/v1/messages. If it 404s, try:\n")
	fmt.Printf("          %s\n", strings.TrimSuffix(strings.TrimSuffix(raw, "/"), "/v1"))
}
