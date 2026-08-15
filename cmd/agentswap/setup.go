package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/install"
	"github.com/bojieli/agentswap/internal/store"
)

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: eval \"$(agentswap env)\"")
		fmt.Fprintln(os.Stderr, "\nPrints exports that point this shell at agentswap, without touching any config file.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, cfg, err := openStore()
	if err != nil {
		return err
	}
	for k, v := range install.ClaudeEnv(cfg.Addr, cfg.Park.MaxHold.D()) {
		fmt.Printf("export %s=%q\n", k, v)
	}
	fmt.Printf("# Codex reads config.toml, not the environment. Run `agentswap install`,\n")
	fmt.Printf("# then start it with: codex --profile %s\n", install.ProfileName)
	return nil
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "show what would change without writing")
	only := fs.String("only", "", "limit to one CLI: claude or codex")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, cfg, err := openStore()
	if err != nil {
		return err
	}

	if *only == "" || *only == "claude" {
		plan, err := install.InstallClaude(cfg.Addr, cfg.Park.MaxHold.D(), *dryRun)
		if err != nil {
			return err
		}
		report("Claude Code", plan, *dryRun)
	}
	if *only == "" || *only == "codex" {
		plan, err := install.InstallCodex(cfg.Addr, *dryRun)
		if err != nil {
			return err
		}
		report("Codex", plan, *dryRun)
	}

	if *dryRun {
		fmt.Println("\n(dry run — nothing was written)")
		return nil
	}
	fmt.Printf("\nDone. Start the daemon with `agentswap serve`, then:\n")
	fmt.Printf("  %-28s # picks up the settings automatically\n", "claude")
	fmt.Printf("  %-28s # Codex needs the profile flag\n", "codex --profile "+install.ProfileName)
	return nil
}

func report(name string, plan *install.Plan, dryRun bool) {
	verb := map[bool]string{true: "would " + plan.Action, false: plan.Action + "d"}[dryRun]
	fmt.Printf("%s: %s %s\n", name, verb, plan.Path)
	if dryRun {
		for _, line := range strings.Split(strings.TrimRight(plan.Preview, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, cfg, err := openStore()
	if err != nil {
		return err
	}
	if err := install.UninstallClaude(cfg.Addr); err != nil {
		return err
	}
	if err := install.UninstallCodex(); err != nil {
		return err
	}
	fmt.Println("removed agentswap's settings from Claude Code and Codex")
	return nil
}

// cmdDoctor checks each link in the chain in the order a request travels it,
// so the first failure reported is the first thing to fix.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	addrFlag := fs.String("addr", "", "daemon address (default: wherever the running daemon says it is)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	var problems int
	check := func(ok bool, label, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Printf("[%s] %s\n", mark, label)
		if detail != "" {
			fmt.Printf("       %s\n", detail)
		}
	}
	// note reports something true but not wrong. Most people use one of the two
	// CLIs, and reporting the other as a fault makes the exit code meaningless
	// — which makes the whole command meaningless.
	note := func(label, detail string) {
		fmt.Printf("[    ] %s\n", label)
		if detail != "" {
			fmt.Printf("       %s\n", detail)
		}
	}

	// 1. Is there anything to route to? An empty lane only matters when every
	// lane is empty.
	counts := map[store.LaneID]int{}
	total := 0
	lanes := []store.LaneID{store.LaneAnthropic, store.LaneOpenAI}
	for _, l := range lanes {
		counts[l] = len(st.Accounts(l))
		total += counts[l]
	}
	if total == 0 {
		check(false, "the pool has accounts (0)",
			"run `agentswap import`, or `agentswap add-key anthropic --key ...`")
	}
	for _, l := range lanes {
		switch {
		case counts[l] > 0:
			check(true, fmt.Sprintf("%s lane has accounts (%d)", l, counts[l]), "")
		case total > 0:
			note(fmt.Sprintf("%s lane has no accounts", l),
				fmt.Sprintf("fine if you do not use it; otherwise `agentswap add-key %s --key ...`", l))
		}
	}

	// 2. Is the daemon up? It may have been started on another address.
	addr, daemonUp := probeDaemon(daemonAddrs(cfg.Addr, *addrFlag))
	if daemonUp {
		check(true, "daemon is listening on "+addr, "")
	} else {
		addr = cfg.Addr
		check(false, "daemon is listening on "+addr, "run `agentswap serve`")
	}

	// 3. Are the CLIs actually pointed at it? A daemon nothing talks to is the
	// most confusing failure mode of all, because everything looks healthy.
	//
	// Being pointed at the wrong address is its own diagnosis: telling someone
	// to run `install` when they already have would rewrite the same value and
	// change nothing.
	claudePath, _ := install.ClaudeSettingsPath()
	claudeAddr := wiredAddr(claudePath, "/anthropic", addr, cfg.Addr)
	codexPath, _ := install.CodexConfigPath()
	codexAddr := wiredAddr(codexPath, "/openai", addr, cfg.Addr)

	if claudeAddr == "" && codexAddr == "" {
		check(false, "a CLI is pointed at agentswap", "run `agentswap install`")
	}
	reportWiring := func(name, wired, fixOnly string) {
		switch {
		case wired == addr:
			check(true, name+" is pointed at agentswap", "")
		case wired != "":
			check(false, fmt.Sprintf("%s is pointed at %s, but the daemon is on %s", name, wired, addr),
				"start the daemon without --addr, or set \"addr\" in config.json and re-run `agentswap install`")
		default:
			note(name+" is not pointed at agentswap",
				"fine if you do not use it; otherwise `agentswap install --only "+fixOnly+"`")
		}
	}
	reportWiring("Claude Code", claudeAddr, "claude")
	reportWiring("Codex", codexAddr, "codex")
	if codexAddr == addr {
		// Codex has no equivalent of Claude Code's automatic pickup, so a
		// correct config still needs the flag at the call site.
		fmt.Printf("       start Codex with: codex --profile %s\n", install.ProfileName)
	}

	// 4. Report anything the pool already knows is broken.
	for _, a := range st.All() {
		if h := st.Health(a.ID); h.State == store.StateInvalid {
			check(false, fmt.Sprintf("account %q was rejected", a.Display()),
				h.LastError+" — re-import or remove it")
		}
	}

	fmt.Println()
	if problems == 0 {
		fmt.Println("Everything checks out.")
		return nil
	}
	return fmt.Errorf("%d problem(s) found", problems)
}

// probeDaemon tries each candidate address and reports the first that answers.
func probeDaemon(addrs []string) (string, bool) {
	for _, addr := range addrs {
		if probeDaemonAt(addr) {
			return addr, true
		}
	}
	return "", false
}

func probeDaemonAt(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/_agentswap/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
	}
	return json.NewDecoder(resp.Body).Decode(&out) == nil && out.Status == "ok"
}

// wiredAddr reports which of the candidate addresses a CLI's config file points
// agentswap at, or "" if none of them do. Distinguishing "wired elsewhere" from
// "not wired" is what lets doctor give advice that would actually help.
func wiredAddr(path, suffix string, candidates ...string) string {
	for _, addr := range candidates {
		if addr != "" && fileContains(path, "http://"+addr+suffix) {
			return addr
		}
	}
	return ""
}

func fileContains(path, needle string) bool {
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), needle)
}
