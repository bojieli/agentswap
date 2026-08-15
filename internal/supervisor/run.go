package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/install"
)

// Kind is which CLI is being supervised, which decides how to resume it.
type Kind int

const (
	KindUnknown Kind = iota
	KindClaude
	KindCodex
)

// DetectKind identifies the CLI from the command name.
func DetectKind(argv0 string) Kind {
	switch strings.TrimSuffix(filepath.Base(argv0), ".exe") {
	case "claude":
		return KindClaude
	case "codex":
		return KindCodex
	}
	return KindUnknown
}

// Options configures a supervised run.
type Options struct {
	ConfigDir  string
	Addr       string
	Args       []string
	MaxResumes int
	Out        io.Writer
}

// Run executes the command, and after a handoff waits for the reset and
// resumes the session rather than leaving the user to notice and restart.
func Run(ctx context.Context, opts Options) error {
	if len(opts.Args) == 0 {
		return errors.New("nothing to run")
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}

	kind := DetectKind(opts.Args[0])
	args := opts.Args
	if kind == KindCodex {
		args = ensureCodexProfile(args)
	}

	for attempt := 0; ; attempt++ {
		started := time.Now()

		if err := runChild(ctx, args, opts); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err
			}
			// A non-zero exit is normal when the pool ran dry; whether to
			// resume is decided by the ticket, not the exit code.
		}

		if kind == KindUnknown {
			return nil // nothing we know how to resume
		}
		if attempt >= opts.MaxResumes {
			fmt.Fprintf(opts.Out, "agentswap: reached the resume limit (%d)\n", opts.MaxResumes)
			return nil
		}

		ticket, err := PendingSince(opts.ConfigDir, started)
		if err != nil || ticket == nil {
			return nil // exited for some other reason; leave it alone
		}

		wait := time.Until(ticket.Until)
		fmt.Fprintf(opts.Out, "\nagentswap: %s quota is spent. Resuming at %s (in %s).\n",
			ticket.Lane, ticket.Until.Format(time.Kitchen), wait.Round(time.Minute))
		fmt.Fprintf(opts.Out, "agentswap: press Ctrl-C to stop waiting.\n")

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if err := ConsumeTicket(opts.ConfigDir, ticket.Lane); err != nil {
			fmt.Fprintf(opts.Out, "agentswap: could not clear ticket: %v\n", err)
		}

		// Rewrite the args we actually ran, not the user's original: the
		// agentswap profile is added to the first invocation, and resuming from
		// the original would drop it and quietly bypass the proxy from the
		// second attempt on.
		args = resumeArgs(kind, args)
		fmt.Fprintf(opts.Out, "agentswap: resuming %s\n\n", strings.Join(args, " "))
	}
}

// resumeArgs rewrites the command to continue the previous session rather than
// start a new one, discarding any prompt argument so the agent picks up where
// it stopped instead of repeating the original instruction.
//
// What it must keep is whichever flag made the original run non-interactive.
// A supervised run is usually unattended — that is the whole reason for it —
// and resuming interactively leaves the CLI waiting on a terminal nobody is
// watching, which looks exactly like a hang.
func resumeArgs(kind Kind, original []string) []string {
	switch kind {
	case KindClaude:
		out := []string{original[0]}
		if flag, ok := firstOf(original, "-p", "--print"); ok {
			out = append(out, flag)
		}
		return append(out, "--continue")

	case KindCodex:
		out := []string{original[0]}
		// `codex exec` and `codex` are separate session kinds with separate
		// resume subcommands; resuming an exec session with the interactive
		// form finds nothing to resume.
		//
		// Matching anywhere in the argv would misfire only on a prompt that is
		// the bare word "exec", and the cost of that is resuming in exec mode,
		// which is what an unattended run wants anyway.
		if indexOf(original, "exec") >= 0 {
			out = append(out, "exec")
		}
		out = append(out, "resume", "--last")
		if flag, value, ok := codexProfile(original); ok {
			out = append(out, flag, value)
		}
		return out
	}
	return original
}

// firstOf returns whichever of names appears in args, preserving the spelling
// the user chose.
func firstOf(args []string, names ...string) (string, bool) {
	for _, n := range names {
		if indexOf(args, n) >= 0 {
			return n, true
		}
	}
	return "", false
}

// ensureCodexProfile adds the agentswap profile when the user has not named
// one. Codex has no equivalent of Claude Code's automatic settings pickup, so
// without this the run silently bypasses the proxy entirely.
func ensureCodexProfile(args []string) []string {
	if _, _, ok := codexProfile(args); ok {
		return args
	}
	out := append([]string{}, args...)
	return append(out, "--profile", install.ProfileName)
}

// codexProfile finds an explicitly named profile and the spelling it was given
// in, so a resume can carry it over unchanged.
func codexProfile(args []string) (flag, value string, ok bool) {
	for _, name := range []string{"--profile", "-p"} {
		if i := indexOf(args, name); i >= 0 && i+1 < len(args) {
			return name, args[i+1], true
		}
	}
	return "", "", false
}

func indexOf(hay []string, needle string) int {
	for i, v := range hay {
		if v == needle {
			return i
		}
	}
	return -1
}

// runChild launches the CLI with agentswap's environment, wired to this
// terminal so the session behaves exactly as an unsupervised one would.
func runChild(ctx context.Context, args []string, opts Options) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), envPairs(opts.Addr)...)
	return cmd.Run()
}

// envPairs is the environment that points Claude Code at the proxy. It is
// applied even when `agentswap install` has already written the same values,
// so a supervised run works on a machine that was never installed to.
func envPairs(addr string) []string {
	var out []string
	for k, v := range install.ClaudeEnv(addr) {
		out = append(out, k+"="+v)
	}
	return out
}
