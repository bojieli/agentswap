package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/session"
	"github.com/bojieli/agentswap/internal/store"
	"github.com/bojieli/agentswap/internal/supervisor"
)

func cmdTeleport(args []string) error {
	fs := flag.NewFlagSet("teleport", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fromValue := fs.String("from", "", "source agent (claude, codex, opencode, or kimi)")
	sessionID := fs.String("session", "", "exact source session id")
	cwdValue := fs.String("cwd", "", "project directory (default: current directory)")
	latest := fs.Bool("latest", false, "choose the newest matching source session without prompting")
	dryRun := fs.Bool("dry-run", false, "validate and show the migration without writing it")
	launch := fs.Bool("launch", false, "launch the target CLI after a successful teleport")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap teleport <target> [flags]")
		fmt.Fprintln(os.Stderr, "\nCopies the complete observable session structure into a new native session")
		fmt.Fprintln(os.Stderr, "for the target agent. The source is never modified. Discovery is scoped")
		fmt.Fprintln(os.Stderr, "to the current working directory unless --cwd is given.")
		fmt.Fprintln(os.Stderr, "\nTargets: claude, codex, opencode, kimi")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  agentswap teleport codex")
		fmt.Fprintln(os.Stderr, "  agentswap teleport claude --from codex --latest")
		fmt.Fprintln(os.Stderr, "  agentswap teleport opencode --session <id> --dry-run")
		fmt.Fprintln(os.Stderr, "  agentswap teleport kimi --launch")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			return errors.New("target agent is required")
		}
		return nil
	}
	// The target comes first by design: it keeps the common command short and
	// also lets the standard flag parser accept flags after it.
	target, err := session.ParseAgent(args[0])
	if err != nil {
		return err
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *launch && *dryRun {
		return errors.New("--launch cannot be combined with --dry-run")
	}
	var from session.Agent
	if *fromValue != "" {
		from, err = session.ParseAgent(*fromValue)
		if err != nil {
			return fmt.Errorf("--from: %w", err)
		}
		if from == target {
			return errors.New("source and target agents are the same")
		}
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
	if from == "" && chosenID == "" {
		from = freshTicketSource(target)
	}
	ctx := context.Background()
	manager := session.NewManager()
	candidates, err := manager.Discover(ctx, session.DiscoverOptions{Target: target, From: from, CWD: cwd})
	if err != nil {
		return err
	}
	selected, err := session.Select(candidates, chosenID, *latest)
	if err != nil {
		var ambiguous *session.AmbiguousError
		if !errors.As(err, &ambiguous) {
			return err
		}
		selected, err = chooseSession(ambiguous.Candidates)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%s %s -> %s (%s)\n", selected.Agent.Display(), selected.ID, target.Display(), cwd)
	result, history, err := manager.Teleport(ctx, selected, target, session.WriteOptions{CWD: cwd, DryRun: *dryRun})
	if err != nil {
		return err
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
	if *launch {
		return launchTarget(result.Resume, cwd)
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

func freshTicketSource(target session.Agent) session.Agent {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	// A ticket is a hint only while it plausibly belongs to the just-failed
	// session. It identifies the provider lane, never a target harness.
	ticket, err := supervisor.PendingSince(dir, time.Now().Add(-2*time.Minute))
	if err != nil || ticket == nil {
		return ""
	}
	var source session.Agent
	switch ticket.Lane {
	case store.LaneAnthropic:
		source = session.Claude
	case store.LaneOpenAI:
		source = session.Codex
	}
	if source == target {
		return ""
	}
	return source
}

func chooseSession(candidates []session.Candidate) (session.Candidate, error) {
	if len(candidates) == 0 {
		return session.Candidate{}, errors.New("no source sessions found")
	}
	fmt.Fprintln(os.Stderr, "Several sessions match this directory:")
	for i, candidate := range candidates {
		title := strings.ReplaceAll(strings.TrimSpace(candidate.Title), "\n", " ")
		if runes := []rune(title); len(runes) > 60 {
			title = string(runes[:60]) + "…"
		}
		age := candidate.UpdatedAt.Local().Format("2006-01-02 15:04")
		if candidate.UpdatedAt.IsZero() {
			age = "unknown time"
		}
		if title != "" {
			fmt.Fprintf(os.Stderr, "  %d) %-10s %s  %s  %q\n", i+1, candidate.Agent, candidate.ID, age, title)
		} else {
			fmt.Fprintf(os.Stderr, "  %d) %-10s %s  %s\n", i+1, candidate.Agent, candidate.ID, age)
		}
	}
	if !stdinIsTerminal() {
		return session.Candidate{}, errors.New("selection is ambiguous; rerun with --session <id>, --from <agent>, or --latest")
	}
	fmt.Fprintf(os.Stderr, "Choose [1-%d]: ", len(candidates))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return session.Candidate{}, fmt.Errorf("read selection: %w", err)
	}
	index, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || index < 1 || index > len(candidates) {
		return session.Candidate{}, errors.New("invalid session selection")
	}
	return candidates[index-1], nil
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
