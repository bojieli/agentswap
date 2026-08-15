// Package service keeps the daemon running across logins and reboots.
//
// Everything else here assumes `agentswap serve` is up. Leaving that to a
// terminal the user must not close is the difference between a tool that works
// and one that works until they restart. Each platform has exactly one right
// answer for a per-user background process, and this writes that file.
//
// Per-user, never system-wide: the daemon holds one person's credentials and
// reads their config directory, so it has no business running as root.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label is the service identifier, in each platform's convention.
const (
	LaunchdLabel = "com.github.bojieli.agentswap"
	SystemdUnit  = "agentswap.service"
)

// ErrUnsupported means this platform has no per-user service manager that
// agentswap knows how to write for.
var ErrUnsupported = errors.New("no supported service manager on this platform")

// Config is what the service needs to run the same daemon the CLI talks to.
type Config struct {
	// Binary is the absolute path to agentswap. A service referring to a
	// binary that has moved fails at boot, long after anyone would connect it
	// to the move.
	Binary string

	// ConfigDir is passed through as AGENTSWAP_HOME, so the service reads the
	// same pool the CLI edits. A service manager's environment is not the
	// shell's, so leaving it to resolve the default can mean two different
	// directories.
	ConfigDir string

	// LogDir is where a launchd service writes stdout and stderr. systemd
	// keeps them in the journal instead.
	LogDir string
}

// Manager is one platform's way of running a background process for a user.
type Manager interface {
	// Name is what to call it when talking to the user.
	Name() string
	// Path is the file that defines the service.
	Path() (string, error)
	// Render is the file's content, so it can be shown before it is written.
	Render(Config) (string, error)
	// Install writes the file and starts the service.
	Install(Config) error
	// Uninstall stops the service and removes the file.
	Uninstall() error
	// Running reports whether the service is loaded and up.
	Running() (bool, error)
	// Logs describes how to read its output, which differs per platform.
	Logs(Config) string
}

// For returns the manager for this platform.
func For() (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return launchd{}, nil
	case "linux":
		return systemd{}, nil
	default:
		return nil, ErrUnsupported
	}
}

// ResolveBinary returns the absolute, symlink-free path to the running
// agentswap.
//
// A service file has to name a real path: `go run` builds into a temp
// directory that is deleted immediately, and a relative path means whatever
// the service manager's working directory happens to be.
func ResolveBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// IsTemporary reports whether a path will not survive: `go run` and `go test`
// build into a temp directory that is deleted on exit, so a service pointing
// there starts failing at the next boot, long after anyone would connect it to
// how they installed it.
//
// Reported separately from resolving, because showing what *would* be written
// is still useful from a throwaway build; only writing it for real is not.
func IsTemporary(path string) bool {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	return strings.HasPrefix(path, tmp+string(os.PathSeparator)) ||
		strings.Contains(path, string(os.PathSeparator)+"go-build")
}

// run executes a service-manager command, folding its output into the error so
// the user sees what the tool said rather than just an exit code.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

// writeFile writes a service definition, creating its directory.
func writeFile(path, content string) error {
	// 0755: launchd and systemd read these as themselves, and both of these
	// directories are conventionally world-readable anyway.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the service manager has to read it
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644) //nolint:gosec // a service definition is not a secret, and the manager must read it
}
