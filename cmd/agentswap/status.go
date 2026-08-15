package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/daemon"
	"github.com/bojieli/agentswap/internal/proxy"
	"github.com/bojieli/agentswap/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	watch := fs.Duration("watch", 0, "refresh on an interval, e.g. --watch 5s")
	addr := fs.String("addr", "", "daemon address (default: wherever the running daemon says it is)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *watch <= 0 {
		return printStatus(*addr)
	}
	for {
		fmt.Print("\033[H\033[2J") // clear
		if err := printStatus(*addr); err != nil {
			return err
		}
		time.Sleep(*watch)
	}
}

func printStatus(addrOverride string) error {
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	all := st.All()
	if len(all) == 0 {
		fmt.Println("no accounts yet — run `agentswap import`")
		return nil
	}

	// Prefer the daemon's live view. Health is only flushed to disk every few
	// seconds, so reading the file alone reports quota that is seconds to
	// minutes stale — and stale quota is the one number nobody can act on.
	//
	// The daemon loaded its pool at startup, so an account added since is
	// missing from its answer. Falling back per account keeps that row honest
	// instead of printing it blank.
	health := func(id string) store.Health { return st.Health(id) }
	live, err := fetchLiveStatus(daemonAddrs(cfg.Addr, addrOverride))
	if err == nil {
		health = func(id string) store.Health {
			if h, ok := live[id]; ok {
				return h
			}
			return st.Health(id)
		}
	} else {
		fmt.Println("(daemon not running — showing last saved state)")
		fmt.Println()
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Lane != all[j].Lane {
			return all[i].Lane < all[j].Lane
		}
		return all[i].Priority < all[j].Priority
	})

	now := time.Now()
	fmt.Printf("%-18s %-10s %-11s %-7s %-7s %-12s\n", "ACCOUNT", "LANE", "STATE", "5H/PRI", "7D/SEC", "RECOVERS")
	for _, a := range all {
		h := health(a.ID)

		state := string(h.State)
		switch {
		case !a.Enabled:
			state = "disabled"
		case h.State == "":
			// An account nothing has used yet has no recorded state.
			state = string(store.StateAvailable)
		case h.Available(now) && h.State != store.StateAvailable:
			// The stored state is stale: its deadline has already passed.
			state = string(store.StateAvailable)
		}

		primary, secondary := "-", "-"
		if len(h.Windows) > 0 {
			primary = fmt.Sprintf("%.0f%%", h.Windows[0].Utilization)
		}
		if len(h.Windows) > 1 {
			secondary = fmt.Sprintf("%.0f%%", h.Windows[1].Utilization)
		}

		recovers := "-"
		if t := h.NextAvailable(now); !t.IsZero() {
			recovers = humanUntil(t.Sub(now))
		} else if h.State == store.StateInvalid {
			recovers = "needs login"
		}

		fmt.Printf("%-18s %-10s %-11s %-7s %-7s %-12s\n",
			truncate(a.Display(), 18), a.Lane, state, primary, secondary, recovers)
	}

	if line := poolSummary(st, health, now); line != "" {
		fmt.Printf("\n%s\n", line)
	}
	printRejected(all, health)
	return nil
}

// printRejected calls out the one state that will not fix itself.
//
// Everything else in this table recovers on its own given time, so a user can
// reasonably read it and wait. A rejected credential never comes back, and the
// table alone does not say what to do about it.
func printRejected(all []*store.Account, health func(string) store.Health) {
	var rejected []*store.Account
	for _, a := range all {
		if a.Enabled && health(a.ID).State == store.StateInvalid {
			rejected = append(rejected, a)
		}
	}
	if len(rejected) == 0 {
		return
	}
	fmt.Println()
	for _, a := range rejected {
		if reason := health(a.ID).LastError; reason != "" {
			fmt.Printf("%s was rejected: %s\n", a.Display(), reason)
		} else {
			fmt.Printf("%s was rejected.\n", a.Display())
		}
		switch {
		case a.Kind == store.KindAPIKey:
			fmt.Printf("  replace the key:  agentswap set %s --key -\n", a.ID)
		case a.RefreshToken == "":
			fmt.Printf("  issue a new token:  claude setup-token, then agentswap add-token %s --id %s\n",
				a.Lane, a.ID)
		default:
			fmt.Printf("  sign in again:    agentswap login --id %s\n", a.ID)
		}
	}
}

// poolSummary reports, per lane, whether anything is actually usable right
// now — the question a user opening this table is trying to answer.
func poolSummary(st *store.Store, health func(string) store.Health, now time.Time) string {
	var parts []string
	for _, l := range []store.LaneID{store.LaneAnthropic, store.LaneOpenAI} {
		accounts := st.Accounts(l)
		if len(accounts) == 0 {
			continue
		}
		ready := 0
		var soonest time.Time
		for _, a := range accounts {
			h := health(a.ID)
			if h.Available(now) {
				ready++
				continue
			}
			if t := h.NextAvailable(now); !t.IsZero() && (soonest.IsZero() || t.Before(soonest)) {
				soonest = t
			}
		}
		switch {
		case ready > 0:
			parts = append(parts, fmt.Sprintf("%s: %d/%d ready", l, ready, len(accounts)))
		case !soonest.IsZero():
			parts = append(parts, fmt.Sprintf("%s: all spent, next in %s", l, humanUntil(soonest.Sub(now))))
		default:
			parts = append(parts, fmt.Sprintf("%s: no usable account", l))
		}
	}
	return strings.Join(parts, "   ")
}

func humanUntil(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// daemonAddrs is where a running daemon might be, most authoritative first: an
// explicit --addr, then whatever the daemon published on startup, then the
// configured address. `serve --addr` is legitimate, and without this every
// other command would report a healthy daemon as down.
func daemonAddrs(configured, override string) []string {
	if override != "" {
		return []string{override}
	}
	dir, err := config.Dir()
	if err != nil {
		return []string{configured}
	}
	return daemon.Addrs(dir, configured)
}

// fetchLiveStatus asks a running daemon for its in-memory health, which is
// always fresher than the periodically flushed state file.
func fetchLiveStatus(addrs []string) (map[string]store.Health, error) {
	var err error
	for _, addr := range addrs {
		var m map[string]store.Health
		if m, err = fetchLiveStatusFrom(addr); err == nil {
			return m, nil
		}
	}
	if err == nil {
		err = errors.New("no daemon address to try")
	}
	return nil, err
}

func fetchLiveStatusFrom(addr string) (map[string]store.Health, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/_agentswap/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %s", resp.Status)
	}

	var out struct {
		Accounts []proxy.AccountStatus `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	m := make(map[string]store.Health, len(out.Accounts))
	for _, a := range out.Accounts {
		m[a.ID] = a.Health
	}
	return m, nil
}
