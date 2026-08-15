package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/agentswap/internal/importer"
	"github.com/bojieli/agentswap/internal/store"
)

// loginPoll is how often the credential file is re-read while waiting. Fast
// enough to feel immediate, slow enough to be invisible.
const loginPoll = 500 * time.Millisecond

// cmdLogin adds an account, or replaces the credential of one that was
// rejected.
//
// agentswap has no OAuth flow of its own — only the CLIs can mint a
// credential — so this cannot be a single self-contained login. What it can do
// is remove every part of the dance that is not "sign in": it works out
// whether you even need to, tells you exactly what to run, waits for the
// credential to appear, and adopts it under the name you chose. Watching for
// the change rather than driving the CLI means it works however you sign in:
// another terminal, a browser, a device code.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	id := fs.String("id", "", "account id to add or replace (default: derived from the lane)")
	label := fs.String("label", "", "human-readable label")
	laneFlag := fs.String("lane", "", "which CLI to sign in to: anthropic (Claude Code) or openai (Codex)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the sign-in")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap login [--id NAME] [--lane anthropic|openai]")
		fmt.Fprintln(os.Stderr, "\nAdds an account to the pool, or replaces one whose credential was rejected.")
		fmt.Fprintln(os.Stderr, "Sign in with your CLI as usual; this waits for it and adopts the result.")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  agentswap login                 # pool another account")
		fmt.Fprintln(os.Stderr, "  agentswap login --id work       # replace the credential for \"work\"")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}

	lane, err := chooseLane(*laneFlag, st, *id)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Already signed in as somebody new? Then there is nothing to wait for.
	// This is the common case when someone signs in first and reaches for
	// agentswap afterwards.
	if acct, err := readCredential(lane); err == nil {
		if existing := matchingAccount(st, acct); existing == nil {
			return adopt(st, acct, lane, *id, *label, "found a new sign-in")
		} else if *id == "" || *id == existing.ID {
			fmt.Printf("%s is already signed in as %q.\n\n", cliName(lane), existing.ID)
			fmt.Printf("To pool a different account, sign out and back in as that one:\n")
			printSignInHint(lane)
			fmt.Printf("\nWaiting for a different account… (Ctrl-C to stop)\n")
		}
	} else if !errors.Is(err, importer.ErrNoCredentials) {
		return err
	} else {
		fmt.Printf("%s is not signed in.\n\n", cliName(lane))
		printSignInHint(lane)
		fmt.Printf("\nWaiting… (Ctrl-C to stop)\n")
	}

	acct, err := waitForNewCredential(ctx, st, lane, *timeout)
	if err != nil {
		return err
	}
	return adopt(st, acct, lane, *id, *label, "signed in")
}

// chooseLane works out which CLI is meant, asking only when it genuinely
// cannot tell.
//
// Having both CLIs installed is normal and says nothing about which one you
// just signed in to. The signals are ordered by how much they actually mean:
// an unpooled credential sitting there is someone who signed in a moment ago,
// which is the whole reason they are running this.
func chooseLane(flagValue string, st *store.Store, id string) (store.LaneID, error) {
	if flagValue != "" {
		lane := store.LaneID(flagValue)
		if !lane.Valid() {
			return "", fmt.Errorf("unknown lane %q; want anthropic or openai", flagValue)
		}
		return lane, nil
	}
	// A re-login is for the lane that account is already in.
	if id != "" {
		if existing, err := st.Get(id); err == nil {
			return existing.Lane, nil
		}
	}

	lanes := []store.LaneID{store.LaneAnthropic, store.LaneOpenAI}
	var fresh, credentialed, installed []store.LaneID
	for _, lane := range lanes {
		acct, err := readCredential(lane)
		if err == nil {
			credentialed = append(credentialed, lane)
			if matchingAccount(st, acct) == nil {
				fresh = append(fresh, lane)
			}
		}
		if _, err := exec.LookPath(cliBinary(lane)); err == nil {
			installed = append(installed, lane)
		}
	}

	for _, candidates := range [][]store.LaneID{fresh, credentialed, installed} {
		if len(candidates) == 1 {
			return candidates[0], nil
		}
	}
	if len(installed) == 0 && len(credentialed) == 0 {
		return "", errors.New("neither `claude` nor `codex` was found on this machine; " +
			"install one, or name the lane with --lane")
	}

	// Genuinely ambiguous: both are set up and neither has a new sign-in.
	answer, err := prompt("Sign in to (1) Claude Code or (2) Codex? [1] ")
	if err != nil {
		return "", errors.New("both CLIs are set up here, so say which you mean: " +
			"`agentswap login --lane anthropic` or `--lane openai`")
	}
	switch strings.TrimSpace(answer) {
	case "", "1", "claude", "anthropic":
		return store.LaneAnthropic, nil
	case "2", "codex", "openai":
		return store.LaneOpenAI, nil
	}
	return "", fmt.Errorf("did not understand %q", answer)
}

func cliBinary(lane store.LaneID) string {
	if lane == store.LaneOpenAI {
		return "codex"
	}
	return "claude"
}

func cliName(lane store.LaneID) string {
	if lane == store.LaneOpenAI {
		return "Codex"
	}
	return "Claude Code"
}

func printSignInHint(lane store.LaneID) {
	if lane == store.LaneOpenAI {
		fmt.Printf("  Run:  codex login\n")
		return
	}
	fmt.Printf("  Run:  claude /login\n")
	fmt.Printf("  (or `/login` inside a running claude session)\n")
}

// readCredential reads whatever the CLI currently holds, without storing it.
func readCredential(lane store.LaneID) (*store.Account, error) {
	if lane == store.LaneOpenAI {
		return importer.ImportCodex()
	}
	return importer.ImportClaude()
}

// waitForNewCredential blocks until the CLI holds a credential that is not
// already pooled.
func waitForNewCredential(ctx context.Context, st *store.Store, lane store.LaneID, timeout time.Duration) (*store.Account, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(loginPoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("cancelled")
		case <-ticker.C:
		}

		acct, err := readCredential(lane)
		if err == nil && matchingAccount(st, acct) == nil {
			return acct, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no new %s sign-in after %s; "+
				"run `agentswap login` again when you have signed in", lane, timeout)
		}
	}
}

// adopt stores a freshly signed-in credential.
func adopt(st *store.Store, acct *store.Account, lane store.LaneID, id, label, why string) error {
	replacing := false
	if id == "" {
		id = nextFreeID(string(lane), takenIDs(st))
	} else if prior, err := st.Get(id); err == nil {
		replacing = true
		if label == "" {
			label = prior.Label
		}
	}
	if label == "" {
		label = id
	}
	acct.ID, acct.Label = id, label

	if err := st.Upsert(acct); err != nil {
		return err
	}

	if replacing {
		fmt.Printf("\n%s: replaced the credential for %q.\n", why, id)
	} else {
		fmt.Printf("\n%s: added %q to the pool.\n", why, id)
	}

	// Health is keyed by id, and a rejected account has to stop being rejected
	// or the new credential would never be tried.
	st.MutateHealth(id, func(h *store.Health) {
		*h = store.Health{State: store.StateAvailable}
	})
	if err := st.FlushHealth(); err != nil {
		fmt.Printf("  (could not clear the old health record: %v)\n", err)
	}

	notifyDaemon(id)
	fmt.Printf("%d account(s) in the pool.\n", len(st.All()))
	if n := len(st.Accounts(lane)); n == 1 {
		fmt.Printf("\nOne account is not failover. Run `agentswap login` again, signed in as another.\n")
	}
	return nil
}
