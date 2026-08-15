package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/supervisor"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	maxResumes := fs.Int("max-resumes", 10, "how many times to resume after a quota wait")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap run -- <command> [args...]")
		fmt.Fprintln(os.Stderr, "\nRuns a CLI against agentswap and, if the whole pool runs dry for longer")
		fmt.Fprintln(os.Stderr, "than park.max_hold, waits for the reset and resumes the session.")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  agentswap run -- claude \"refactor the parser\"")
		fmt.Fprintln(os.Stderr, "  agentswap run -- codex exec \"fix the failing tests\"")
		fs.PrintDefaults()
	}

	// Everything after `--` belongs to the child, so split before parsing.
	var flagArgs, childArgs []string
	if i := indexOfArg(args, "--"); i >= 0 {
		flagArgs, childArgs = args[:i], args[i+1:]
	} else {
		flagArgs = args
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(childArgs) == 0 {
		childArgs = fs.Args()
	}
	if len(childArgs) == 0 {
		fs.Usage()
		return errors.New("no command given")
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return supervisor.Run(ctx, supervisor.Options{
		ConfigDir:  dir,
		Addr:       cfg.Addr,
		Args:       childArgs,
		MaxResumes: *maxResumes,
		Out:        os.Stderr,
	})
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
