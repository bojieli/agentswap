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
	for k, v := range install.ClaudeEnv(cfg.Addr) {
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
		plan, err := install.InstallClaude(cfg.Addr, *dryRun)
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

	// 1. Is there anything to route to?
	for _, l := range []store.LaneID{store.LaneAnthropic, store.LaneOpenAI} {
		n := len(st.Accounts(l))
		check(n > 0, fmt.Sprintf("%s lane has accounts (%d)", l, n),
			map[bool]string{true: "", false: "run `agentswap import`"}[n > 0])
	}

	// 2. Is the daemon up?
	daemonUp := probeDaemon(cfg.Addr)
	check(daemonUp, "daemon is listening on "+cfg.Addr,
		map[bool]string{true: "", false: "run `agentswap serve`"}[daemonUp])

	// 3. Are the CLIs actually pointed at it? A daemon nothing talks to is the
	// most confusing failure mode of all, because everything looks healthy.
	claudePath, _ := install.ClaudeSettingsPath()
	claudeWired := fileContains(claudePath, "http://"+cfg.Addr+"/anthropic")
	check(claudeWired, "Claude Code is pointed at agentswap",
		map[bool]string{true: "", false: "run `agentswap install`"}[claudeWired])

	codexPath, _ := install.CodexConfigPath()
	codexWired := fileContains(codexPath, "http://"+cfg.Addr+"/openai")
	detail := "run `agentswap install`"
	if codexWired {
		detail = "start Codex with: codex --profile " + install.ProfileName
	}
	check(codexWired, "Codex has an agentswap profile", detail)

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

func probeDaemon(addr string) bool {
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
