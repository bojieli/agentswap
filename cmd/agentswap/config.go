package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/daemon"
	"github.com/bojieli/agentswap/internal/install"
	"github.com/bojieli/agentswap/internal/store"
)

// cmdConfig shows where agentswap keeps things and what settings are actually
// in effect.
//
// Every field has a default, which is good for a first run and bad for
// answering "what is it doing": an absent config.json tells you nothing about
// the values behind it. `--json` prints the effective settings in full, and
// `--write` saves them, so editing starts from a complete file rather than a
// blank one to guess at.
func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the effective settings as JSON, and nothing else")
	write := fs.Bool("write", false, "save the effective settings to config.json, ready to edit")
	force := fs.Bool("force", false, "with --write, replace a config.json that already exists")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap config [--json] [--write [--force]]")
		fmt.Fprintln(os.Stderr, "\nShows where agentswap keeps things and what settings are in effect.")
		fmt.Fprintln(os.Stderr, "\nTo start editing from a complete file rather than a blank one:")
		fmt.Fprintln(os.Stderr, "  agentswap config --write")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}

	if *write {
		return writeConfigFile(dir, cfg, *force)
	}

	if *asJSON {
		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	fmt.Println("Files")
	fmt.Printf("  %-12s %s\n", "settings", describe(filepath.Join(dir, "config.json"), "not present — the defaults below are in use"))
	fmt.Printf("  %-12s %s\n", "accounts", describeAccounts(dir))
	fmt.Printf("  %-12s %s\n", "quota", describe(filepath.Join(dir, "state.json"), "nothing observed yet"))
	fmt.Printf("  %-12s %s\n", "daemon", describeDaemon(dir, cfg))
	if src := configDirSource(); src != "" {
		fmt.Printf("\n  This directory comes from %s.\n", src)
	}

	fmt.Println("\nThe CLIs' own files, which `agentswap install` edits")
	if path, err := install.ClaudeSettingsPath(); err == nil {
		fmt.Printf("  %-12s %s\n", "claude", describe(path, "not present"))
	}
	if path, err := install.CodexConfigPath(); err == nil {
		fmt.Printf("  %-12s %s\n", "codex", describe(path, "not present"))
	}
	if path, err := install.CodexProfilePath(); err == nil {
		fmt.Printf("  %-12s %s\n", "codex profile", describe(path, "not present"))
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("\nEffective settings\n%s\n", indent(string(out), "  "))
	fmt.Println("\nEdit them starting from a complete file:")
	fmt.Println("  agentswap config --write")
	return nil
}

// writeConfigFile saves the effective settings where agentswap reads them.
//
// This exists rather than telling people to redirect `--json` into the file:
// the shell truncates the target before agentswap starts, so the command would
// read the empty file it had just created.
func writeConfigFile(dir string, cfg config.Config, force bool) error {
	path := filepath.Join(dir, "config.json")
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to replace it "+
			"(it is your settings, and this would overwrite them)", path)
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Written through a temp file so an interrupted write cannot leave the
	// empty config.json this command exists to avoid.
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", path)
	fmt.Println("Every value in it is the default, so edit what you want and leave the rest.")
	return nil
}

// describe reports a path and whether it is there, since "where is it" and "is
// it there" are the same question when something is not working.
func describe(path, absent string) string {
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("%s  (%s)", path, absent)
	}
	return path
}

func describeAccounts(dir string) string {
	path := filepath.Join(dir, "accounts.json")
	st, err := store.Open(dir)
	if err != nil {
		return fmt.Sprintf("%s  (unreadable: %v)", path, err)
	}
	n := len(st.All())
	if n == 0 {
		return fmt.Sprintf("%s  (empty — run `agentswap import` or `agentswap login`)", path)
	}
	return fmt.Sprintf("%s  (%d; see `agentswap list`)", path, n)
}

func describeDaemon(dir string, cfg config.Config) string {
	addr, up := probeDaemon(daemon.Addrs(dir, cfg.Addr))
	if up {
		return fmt.Sprintf("running on %s", addr)
	}
	return fmt.Sprintf("not running  (would listen on %s)", cfg.Addr)
}

// configDirSource names the environment variable that moved the config
// directory, so a surprising location is explained where it is shown.
func configDirSource() string {
	if os.Getenv("AGENTSWAP_HOME") != "" {
		return "AGENTSWAP_HOME"
	}
	if os.Getenv("XDG_CONFIG_HOME") != "" {
		return "XDG_CONFIG_HOME"
	}
	return ""
}

func indent(s, with string) string {
	out := with
	for i, r := range s {
		out += string(r)
		if r == '\n' && i != len(s)-1 {
			out += with
		}
	}
	return out
}
