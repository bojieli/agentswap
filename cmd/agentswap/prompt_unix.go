//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

// terminalSettings returns the current terminal settings, and an error when
// stdin is not a terminal at all.
//
// Reading them would normally be a dependency — golang.org/x/term — and this
// process holds live OAuth tokens, so it does not get to have dependencies.
// `stty` is in every POSIX base system and is what shell scripts have always
// used for exactly this.
func terminalSettings() (string, error) { return stty("-g") }

// hideEcho turns off terminal echo and returns the function that puts it back,
// along with whether it managed to.
func hideEcho() (restore func(), hidden bool, err error) {
	noop := func() {}

	saved, err := terminalSettings()
	if err != nil {
		return noop, false, errNotATerminal
	}
	if _, err := stty("-echo"); err != nil {
		// A terminal we cannot silence is still a terminal. The caller warns
		// that the input will be visible and carries on, which beats refusing
		// to accept a key at all.
		return noop, false, nil //nolint:nilerr // deliberate: not fatal, only less private
	}
	return func() { _, _ = stty(saved) }, true, nil
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
