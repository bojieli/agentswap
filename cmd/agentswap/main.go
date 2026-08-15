// Command agentswap keeps Claude Code and Codex running across rate limits,
// quota exhaustion and upstream overload by pooling several accounts behind a
// local proxy.
package main

import (
	"fmt"
	"os"
	"strings"
)

// version is overridden at build time via -ldflags.
var version = "dev"

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

func main() {
	commands := []command{
		{"serve", "run the proxy daemon", cmdServe},
		{"import", "adopt the logins already on this machine", cmdImport},
		{"add-key", "add an API key to a lane", cmdAddKey},
		{"list", "list pooled accounts", cmdList},
		{"remove", "remove an account from the pool", cmdRemove},
		{"enable", "re-enable an account", cmdEnable},
		{"disable", "take an account out of rotation", cmdDisable},
		{"status", "show quota and health per account", cmdStatus},
		{"env", "print shell exports for the current shell", cmdEnv},
		{"install", "point Claude Code and Codex at agentswap", cmdInstall},
		{"uninstall", "undo install", cmdUninstall},
		{"doctor", "check that everything is wired up correctly", cmdDoctor},
		{"version", "print the version", cmdVersion},
	}

	if len(os.Args) < 2 {
		usage(commands)
		os.Exit(2)
	}

	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage(commands)
		return
	}

	for _, c := range commands {
		if c.name == name {
			if err := c.run(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "agentswap %s: %v\n", name, err)
				os.Exit(1)
			}
			return
		}
	}

	fmt.Fprintf(os.Stderr, "agentswap: unknown command %q\n\n", name)
	usage(commands)
	os.Exit(2)
}

func usage(commands []command) {
	var b strings.Builder
	b.WriteString("agentswap — keep your coding agent running across rate limits\n\n")
	b.WriteString("Usage:\n  agentswap <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-10s %s\n", c.name, c.summary)
	}
	b.WriteString("\nGetting started:\n")
	b.WriteString("  agentswap import          # adopt your current claude / codex logins\n")
	b.WriteString("  agentswap install         # point both CLIs at agentswap\n")
	b.WriteString("  agentswap serve           # run the daemon\n")
	b.WriteString("\nRun `agentswap <command> -h` for the flags of a single command.\n")
	fmt.Fprint(os.Stderr, b.String())
}

func cmdVersion([]string) error {
	fmt.Println("agentswap", version)
	return nil
}
