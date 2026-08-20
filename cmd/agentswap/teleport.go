package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bojieli/agentswap/internal/session"
)

func cmdTeleport(args []string) error {
	return cmdTransfer(args, false)
}

func cmdHandoff(args []string) error {
	return cmdTransfer(args, true)
}

// cmdTransfer implements both user-facing forms. Teleport stops after creating
// the target session; handoff continues by running the target's exact native
// resume command. Keeping the pipeline shared prevents a convenience command
// from acquiring weaker validation or selection rules than teleport itself.
func cmdTransfer(args []string, handoff bool) error {
	name := "teleport"
	if handoff {
		name = "handoff"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fromValue := fs.String("from", "", "deprecated: source agent for the old target-first syntax")
	sessionID := fs.String("session", "", "exact source session id")
	cwdValue := fs.String("cwd", "", "project directory (default: current directory)")
	latest := fs.Bool("latest", false, "deprecated: the newest source session is now selected by default")
	dryRun := fs.Bool("dry-run", false, "validate and show the migration without writing it")
	launch := fs.Bool("launch", false, "deprecated: use agentswap handoff to launch the target")
	fs.Usage = func() {
		if handoff {
			fmt.Fprintln(os.Stderr, "Usage: agentswap handoff <source> <target> [handoff flags] [target args...]")
		} else {
			fmt.Fprintln(os.Stderr, "Usage: agentswap teleport <source> <target> [flags]")
		}
		if handoff {
			fmt.Fprintln(os.Stderr, "\nTeleports the newest source session into a native target session, then")
			fmt.Fprintln(os.Stderr, "launches the target coding agent with that exact new session id.")
		} else {
			fmt.Fprintln(os.Stderr, "\nCopies the newest source session into a new native target session without")
			fmt.Fprintln(os.Stderr, "launching it. The source is never modified.")
		}
		fmt.Fprintln(os.Stderr, "Discovery is scoped to the current working directory unless --cwd is given.")
		fmt.Fprintln(os.Stderr, "\nAgents: claude, codex, opencode, kimi")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		if handoff {
			fmt.Fprintln(os.Stderr, "  agentswap handoff claude codex")
			fmt.Fprintln(os.Stderr, "  agentswap handoff codex claude --session <id> --dangerously-skip-permissions")
			fmt.Fprintln(os.Stderr, "  agentswap handoff claude codex --dangerously-bypass-approvals-and-sandbox")
		} else {
			fmt.Fprintln(os.Stderr, "  agentswap teleport claude codex")
			fmt.Fprintln(os.Stderr, "  agentswap teleport codex claude --session <id>")
			fmt.Fprintln(os.Stderr, "  agentswap teleport claude opencode --dry-run")
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fmt.Fprintln(os.Stderr, "  --cwd path     project directory (default: current directory)")
		fmt.Fprintln(os.Stderr, "  --session id   exact source session id (default: newest source session)")
		if handoff {
			fmt.Fprintln(os.Stderr, "\nOther arguments are passed unchanged to the target CLI. Codex profile/provider")
			fmt.Fprintln(os.Stderr, "overrides are refused so its managed agentswap provider cannot be bypassed.")
			fmt.Fprintln(os.Stderr, "Use -- before target arguments when the target itself needs a --cwd or")
			fmt.Fprintln(os.Stderr, "--session option.")
		} else {
			fmt.Fprintln(os.Stderr, "  --dry-run      validate and show the migration without writing it")
			fmt.Fprintln(os.Stderr, "\nDeprecated compatibility flags: --from, --latest, --launch")
		}
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			return errors.New("source and target agents are required")
		}
		return nil
	}

	// The current form has two positional agents. For one compatibility window,
	// continue accepting `teleport <target> --from <source>` so scripts can move
	// without an abrupt break; target-only discovery is intentionally gone.
	var sourceValue, targetValue string
	var targetArgs []string
	legacy := len(args) < 2 || strings.HasPrefix(args[1], "-")
	flagArgs := args[1:]
	if !legacy {
		sourceValue, targetValue = args[0], args[1]
		flagArgs = args[2:]
	} else {
		targetValue = args[0]
	}
	if handoff {
		if legacy {
			return errors.New("source and target agents are required; usage: agentswap handoff <source> <target> [target args...]")
		}
		var parseErr error
		*sessionID, *cwdValue, targetArgs, parseErr = parseHandoffArgs(flagArgs)
		if parseErr != nil {
			return parseErr
		}
	} else {
		if err := fs.Parse(flagArgs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
	}
	if !handoff && *launch && *dryRun {
		return errors.New("--launch cannot be combined with --dry-run")
	}
	if legacy {
		if *fromValue == "" {
			return errors.New("source agent is required; usage: agentswap teleport <source> <target>")
		}
		sourceValue = *fromValue
		fmt.Fprintln(os.Stderr, "warning: `teleport <target> --from <source>` is deprecated; use `teleport <source> <target>`")
	} else if *fromValue != "" {
		return errors.New("--from cannot be combined with positional source and target agents")
	}
	if *latest {
		fmt.Fprintln(os.Stderr, "warning: --latest is deprecated because the newest source session is selected by default")
	}
	if !handoff && *launch {
		fmt.Fprintln(os.Stderr, "warning: teleport --launch is deprecated; use agentswap handoff <source> <target>")
	}

	from, err := session.ParseAgent(sourceValue)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	target, err := session.ParseAgent(targetValue)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if from == target {
		return errors.New("source and target agents are the same")
	}
	if err := validateTargetArgs(target, targetArgs); err != nil {
		return err
	}
	cwd := *cwdValue
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", cwd)
	}
	chosenID := *sessionID
	if chosenID == "" {
		chosenID = activeSessionID(from)
	}
	ctx := context.Background()
	manager := session.NewManager()
	candidates, err := manager.Discover(ctx, session.DiscoverOptions{Target: target, From: from, CWD: cwd})
	if err != nil {
		return err
	}
	selected, err := session.Select(candidates, chosenID, true)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s %s -> %s (%s)\n", selected.Agent.Display(), selected.ID, target.Display(), cwd)
	result, history, err := manager.Teleport(ctx, selected, target, session.WriteOptions{CWD: cwd, DryRun: *dryRun})
	if err != nil {
		return err
	}
	if handoff {
		result.Resume = append(result.Resume, targetArgs...)
	}
	if *dryRun {
		fmt.Fprintf(os.Stdout, "Dry run succeeded: %d events can be teleported to %s.\n", len(history.Events), target.Display())
		fmt.Fprintln(os.Stdout, "Nothing was written.")
	} else {
		fmt.Fprintf(os.Stdout, "Created %s session %s\n", target.Display(), result.ID)
		if result.Path != "" {
			fmt.Fprintf(os.Stdout, "Location: %s\n", result.Path)
		}
	}
	if len(result.Resume) > 0 {
		fmt.Fprintf(os.Stdout, "Resume: %s\n", shellJoin(result.Resume))
	}
	for _, warning := range uniqueStrings(result.Warnings) {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	if handoff || *launch {
		fmt.Fprintf(os.Stdout, "Launching: %s\n", shellJoin(result.Resume))
		return launchTarget(result.Resume, cwd)
	}
	return nil
}

// parseHandoffArgs consumes the two options owned by agentswap and leaves all
// other tokens byte-for-byte for the target CLI. `--` is supported when the
// target itself has a --cwd or --session flag that must not be consumed here.
func parseHandoffArgs(args []string) (sessionID, cwd string, targetArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			targetArgs = append(targetArgs, args[i+1:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--session", "-session", "--cwd", "-cwd":
			if !hasValue {
				if i+1 >= len(args) {
					return "", "", nil, fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if value == "" {
				return "", "", nil, fmt.Errorf("%s requires a non-empty value", name)
			}
			if name == "--session" || name == "-session" {
				sessionID = value
			} else {
				cwd = value
			}
		default:
			targetArgs = append(targetArgs, arg)
		}
	}
	return sessionID, cwd, targetArgs, nil
}

func validateTargetArgs(target session.Agent, args []string) error {
	if target != session.Codex {
		return nil
	}
	for i, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		if name == "--profile" || name == "-p" || strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "-p") && len(arg) > 2 {
			return errors.New("Codex handoff always uses --profile agentswap; remove the target --profile option")
		}
		if name == "--oss" || name == "--local-provider" {
			return errors.New("Codex handoff always uses the agentswap provider; remove the target provider option")
		}
		var configValue string
		switch {
		case name == "--config" || name == "-c":
			if _, value, hasValue := strings.Cut(arg, "="); hasValue {
				configValue = value
			} else if i+1 < len(args) {
				configValue = args[i+1]
			}
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			configValue = strings.TrimPrefix(arg[2:], "=")
		}
		if key, _, ok := strings.Cut(configValue, "="); ok {
			key = strings.TrimSpace(key)
			if key == "model_provider" || key == "model_providers" || strings.HasPrefix(key, "model_providers.") {
				return errors.New("Codex handoff always uses the agentswap provider; remove the target model-provider config override")
			}
		}
	}
	return nil
}

func activeSessionID(from session.Agent) string {
	if value := os.Getenv("AGENTSWAP_SESSION_ID"); value != "" {
		return value
	}
	keys := map[session.Agent][]string{
		session.Claude:   {"CLAUDE_SESSION_ID"},
		session.Codex:    {"CODEX_THREAD_ID", "CODEX_SESSION_ID"},
		session.OpenCode: {"OPENCODE_SESSION_ID"},
		session.Kimi:     {"KIMI_SESSION_ID"},
	}
	if from == "" {
		return ""
	}
	for _, key := range keys[from] {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func launchTarget(args []string, cwd string) error {
	if len(args) == 0 {
		return errors.New("target adapter did not provide a resume command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launch %s (the teleported session was kept): %w", args[0], err)
	}
	return nil
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._/:", r))
		}) == -1 {
			quoted[i] = arg
		} else {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
	}
	return strings.Join(quoted, " ")
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
