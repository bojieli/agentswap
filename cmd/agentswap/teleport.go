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
	"time"

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
	compact := fs.Bool("compact", false, "abridge the history to fit the target, archiving the full session")
	budget := fs.String("budget", "", "token budget for the abridged transcript (implies --compact)")
	archiveDir := fs.String("archive-dir", "", "parent directory for the archive (default: <project>/"+session.DefaultArchiveDirName+")")
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
			fmt.Fprintln(os.Stderr, "  agentswap handoff opencode claude --session <id> --dangerously-skip-permissions")
			fmt.Fprintln(os.Stderr, "  agentswap handoff kimi codex --dangerously-bypass-approvals-and-sandbox")
		} else {
			fmt.Fprintln(os.Stderr, "  agentswap teleport claude codex")
			fmt.Fprintln(os.Stderr, "  agentswap teleport kimi claude --session <id>")
			fmt.Fprintln(os.Stderr, "  agentswap teleport opencode codex --dry-run")
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fmt.Fprintln(os.Stderr, "  --cwd path     project directory (default: current directory)")
		fmt.Fprintln(os.Stderr, "  --session id   exact source session id (default: newest source session)")
		fmt.Fprintln(os.Stderr, "  --compact      abridge the history to fit the target's context window,")
		fmt.Fprintln(os.Stderr, "                 archiving the complete session where the target can read it")
		fmt.Fprintln(os.Stderr, "  --budget n     token budget for the abridged transcript, such as 120k;")
		fmt.Fprintln(os.Stderr, "                 implies --compact (default: a per-target budget)")
		fmt.Fprintln(os.Stderr, "  --archive-dir path")
		fmt.Fprintln(os.Stderr, "                 parent directory for the archive, which defaults to")
		fmt.Fprintln(os.Stderr, "                 <project>/"+session.DefaultArchiveDirName+" so the target can read it")
		if handoff {
			fmt.Fprintln(os.Stderr, "\nOther arguments are passed unchanged to the target CLI. Use -- before")
			fmt.Fprintln(os.Stderr, "target arguments when the target itself needs a --cwd or --session option.")
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
		owned, parseErr := parseHandoffArgs(flagArgs)
		if parseErr != nil {
			return parseErr
		}
		*sessionID, *cwdValue, *compact, *budget = owned.sessionID, owned.cwd, owned.compact, owned.budget
		*archiveDir = owned.archiveDir
		targetArgs = owned.target
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
	compactOptions, err := compactOptions(*compact, *budget, *archiveDir)
	if err != nil {
		return err
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
	result, history, err := manager.Teleport(ctx, selected, target, session.TransferOptions{
		WriteOptions: session.WriteOptions{CWD: cwd, DryRun: *dryRun},
		Compact:      compactOptions,
	})
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
	if report := result.Compaction; report != nil {
		fmt.Fprintf(os.Stdout, "Compacted: %s\n", report.Summary())
		if detail := report.Detail(); detail != "" {
			fmt.Fprintf(os.Stdout, "  %s\n", detail)
		}
		if result.ArchivePath == "" {
			// Nothing was removed, so there is no archive to point at.
		} else if *dryRun {
			fmt.Fprintf(os.Stdout, "Archive would be written to: %s\n", result.ArchivePath)
		} else {
			fmt.Fprintf(os.Stdout, "Archive: %s\n", result.ArchivePath)
		}
		if hint := archiveReachHint(result.ArchivePath, cwd); hint != "" {
			fmt.Fprintln(os.Stdout, hint)
		}
	}
	if len(history.Branches) > 0 {
		fmt.Fprintf(os.Stdout, "Delegated agent runs: %d (%s)\n", len(history.Branches), branchSummary(history.Branches))
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

// handoffArgs are the options agentswap owns on a handoff command line.
// Everything else is left byte-for-byte for the target CLI.
type handoffArgs struct {
	sessionID  string
	cwd        string
	compact    bool
	budget     string
	archiveDir string
	target     []string
}

// parseHandoffArgs consumes only the options agentswap owns and leaves all
// other tokens untouched for the target CLI. `--` is supported when the target
// itself has an option that must not be consumed here.
func parseHandoffArgs(args []string) (handoffArgs, error) {
	var out handoffArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.target = append(out.target, args[i+1:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--compact", "-compact":
			// --compact takes no value, so a budget written as --compact=120k
			// is a mistake worth naming rather than passing to the target.
			if hasValue {
				return handoffArgs{}, fmt.Errorf("%s takes no value; use --budget %s", name, value)
			}
			out.compact = true
		case "--session", "-session", "--cwd", "-cwd", "--budget", "-budget", "--archive-dir", "-archive-dir":
			if !hasValue {
				if i+1 >= len(args) {
					return handoffArgs{}, fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if value == "" {
				return handoffArgs{}, fmt.Errorf("%s requires a non-empty value", name)
			}
			switch name {
			case "--session", "-session":
				out.sessionID = value
			case "--cwd", "-cwd":
				out.cwd = value
			case "--budget", "-budget":
				out.budget = value
			default:
				out.archiveDir = value
			}
		default:
			out.target = append(out.target, arg)
		}
	}
	return out, nil
}

// archiveReachHint warns when --archive-dir has put the archive outside the
// project the target will run in. A coding agent is normally confined to its
// working directory, so it has to be granted access before it can follow an
// elision marker — and a non-interactive resume has nobody to ask.
func archiveReachHint(archive, cwd string) string {
	if archive == "" || cwd == "" {
		return ""
	}
	rel, err := filepath.Rel(cwd, archive)
	if err == nil && !strings.HasPrefix(rel, "..") {
		return ""
	}
	return "  This archive is outside the project, so the target must be granted access\n" +
		"  before it can read it. Drop --archive-dir to keep it inside the project."
}

// compactOptions turns the user-facing flags into a compaction request. Either
// of --budget and --archive-dir implies --compact on its own, because naming a
// size or a destination is already saying the history should be abridged.
func compactOptions(compact bool, budget, archiveDir string) (*session.CompactOptions, error) {
	if !compact && budget == "" && archiveDir == "" {
		return nil, nil
	}
	tokens := 0
	if budget != "" {
		parsed, err := session.ParseBudget(budget)
		if err != nil {
			return nil, err
		}
		tokens = parsed
	}
	if archiveDir != "" {
		abs, err := filepath.Abs(archiveDir)
		if err != nil {
			return nil, fmt.Errorf("archive directory: %w", err)
		}
		archiveDir = abs
	}
	return &session.CompactOptions{Budget: tokens, ArchiveRoot: archiveDir, Version: version, Now: time.Now()}, nil
}

// validateTargetArgs is retained for compatibility with older package-level
// callers. Handoff itself passes target arguments unchanged.
func validateTargetArgs(_ session.Agent, _ []string) error { return nil }

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

// branchSummary counts the delegated runs by the event totals that matter to a
// reader deciding whether the transfer captured the work: how many messages the
// branches hold, and how many of them are still linked to the call that spawned
// them. A branch that lost its link is readable but will not appear under a
// tool call in the target.
func branchSummary(branches []session.Branch) string {
	events, attached := 0, 0
	for _, branch := range branches {
		events += len(branch.Events)
		if branch.CallID != "" {
			attached++
		}
	}
	summary := fmt.Sprintf("%d events", events)
	if attached < len(branches) {
		summary += fmt.Sprintf(", %d of %d linked to a tool call", attached, len(branches))
	}
	return summary
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
