package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/daemon"
	"github.com/bojieli/agentswap/internal/service"
)

// cmdService keeps the daemon running without a terminal held open for it.
//
// Everything agentswap does assumes `serve` is up. Leaving that to a window
// the user must not close is the difference between a tool that works and one
// that works until they reboot.
func cmdService(args []string) error {
	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap service <install|uninstall|status>")
		fmt.Fprintln(os.Stderr, "\nRuns the daemon in the background, starting it again at login.")
		fmt.Fprintln(os.Stderr, "  install    write and start the service for your user")
		fmt.Fprintln(os.Stderr, "  uninstall  stop it and remove the file")
		fmt.Fprintln(os.Stderr, "  status     where it is defined, and whether it is up")
		fmt.Fprintln(os.Stderr, "\n  --dry-run  with install: print the file instead of writing it")
	}
	if len(args) == 0 {
		usage()
		return errors.New("say what to do: install, uninstall or status")
	}

	fs := flag.NewFlagSet("service", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "with install: print the service file instead of writing it")
	fs.Usage = usage
	action, rest := args[0], args[1:]
	if err := fs.Parse(rest); err != nil {
		return err
	}

	mgr, err := service.For()
	if err != nil {
		return unsupportedPlatform()
	}
	cfg, err := serviceConfig()
	if err != nil {
		return err
	}

	switch action {
	case "install":
		return serviceInstall(mgr, cfg, *dryRun)
	case "uninstall":
		if err := mgr.Uninstall(); err != nil {
			return err
		}
		fmt.Println("service removed. The daemon is no longer started at login.")
		return nil
	case "status":
		return serviceStatus(mgr, cfg)
	default:
		usage()
		return fmt.Errorf("unknown action %q", action)
	}
}

func serviceConfig() (service.Config, error) {
	binary, err := service.ResolveBinary()
	if err != nil {
		return service.Config{}, err
	}
	dir, err := config.Dir()
	if err != nil {
		return service.Config{}, err
	}

	// The directory is always named, never left to the service to resolve. A
	// service manager's environment is not the shell's, so "the default" can
	// mean two different directories — and a daemon serving a different pool
	// from the one the CLI edits is the worst kind of working.
	return service.Config{
		Binary:    binary,
		ConfigDir: dir,
		LogDir:    filepath.Join(dir, "logs"),
	}, nil
}

func serviceInstall(mgr service.Manager, cfg service.Config, dryRun bool) error {
	path, err := mgr.Path()
	if err != nil {
		return err
	}
	body, err := mgr.Render(cfg)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("# %s would be written to %s\n\n%s", mgr.Name(), path, body)
		if service.IsTemporary(cfg.Binary) {
			fmt.Printf("\n# note: %s is a temporary build and will be deleted.\n", cfg.Binary)
			fmt.Printf("#       Install agentswap properly before installing the service.\n")
		}
		return nil
	}

	// A service pointing at a binary that is about to be deleted fails at the
	// next boot, with nothing to connect it to how it was installed.
	if service.IsTemporary(cfg.Binary) {
		return fmt.Errorf("this agentswap is a temporary build at %s and will be deleted; "+
			"install it first (`go install ./cmd/agentswap`), then run this again", cfg.Binary)
	}

	// A service pointing at an address the CLIs are not using is the confusing
	// failure this command could otherwise create.
	if err := mgr.Install(cfg); err != nil {
		return err
	}

	fmt.Printf("installed the %s service at %s\n", mgr.Name(), path)
	fmt.Printf("  runs:  %s serve\n", cfg.Binary)
	if cfg.ConfigDir != "" {
		fmt.Printf("  pool:  %s\n", cfg.ConfigDir)
	}
	fmt.Printf("  logs:  %s\n", mgr.Logs(cfg))
	if runtime.GOOS == "linux" {
		// Without lingering a user unit stops at logout, which looks exactly
		// like the service having failed.
		fmt.Printf("\nTo keep it running when you are not logged in:\n")
		fmt.Printf("  sudo loginctl enable-linger %s\n", os.Getenv("USER"))
	}
	fmt.Println("\nCheck it with `agentswap doctor`.")
	return nil
}

func serviceStatus(mgr service.Manager, cfg service.Config) error {
	path, err := mgr.Path()
	if err != nil {
		return err
	}

	installed := false
	if _, err := os.Stat(path); err == nil {
		installed = true
	}
	running, _ := mgr.Running()

	fmt.Printf("manager    %s\n", mgr.Name())
	fmt.Printf("file       %s%s\n", path, map[bool]string{true: "", false: "  (not installed)"}[installed])
	fmt.Printf("running    %v\n", running)
	fmt.Printf("logs       %s\n", mgr.Logs(cfg))

	// The service and a hand-started daemon are different processes, and the
	// distinction matters when one is answering and the other is not.
	dir, err := config.Dir()
	if err == nil {
		if info, err := daemon.Read(dir); err == nil && info != nil {
			fmt.Printf("daemon     listening on %s (pid %d)\n", info.Addr, info.PID)
		} else {
			fmt.Printf("daemon     nothing has published an address\n")
		}
	}

	if !installed {
		fmt.Println("\nInstall it with `agentswap service install`.")
	}
	return nil
}

// unsupportedPlatform explains what to do where there is no per-user service
// manager to write for, rather than leaving the user with an error.
func unsupportedPlatform() error {
	if runtime.GOOS != "windows" {
		return service.ErrUnsupported
	}
	fmt.Fprintln(os.Stderr, "Windows has no per-user service manager that agentswap writes for.")
	fmt.Fprintln(os.Stderr, "\nTo run the daemon in the background, either:")
	fmt.Fprintln(os.Stderr, "  * Task Scheduler: create a task that runs `agentswap serve` at log on, or")
	fmt.Fprintln(os.Stderr, "  * put a shortcut to `agentswap serve` in your Startup folder:")
	fmt.Fprintln(os.Stderr, "      shell:startup")
	fmt.Fprintln(os.Stderr, "\nEither way it is an ordinary program: it needs no console once started.")
	return service.ErrUnsupported
}
