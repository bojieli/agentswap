package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// errNotATerminal means there is nobody at the keyboard to ask.
var errNotATerminal = errors.New("stdin is not a terminal")

// promptSecret reads a secret from the terminal without echoing it.
//
// Turning echo off would normally be a dependency — golang.org/x/term — and
// this process holds live OAuth tokens, so it does not get to have
// dependencies. `stty` is in every POSIX base system and is what shell scripts
// have always used for this. Where it is unavailable the user is told the
// input will be visible, rather than having it silently displayed.
func promptSecret(question string) (string, error) {
	restore, hidden, err := hideEcho()
	if err != nil {
		return "", err
	}
	defer restore()

	if !hidden {
		fmt.Fprintln(os.Stderr, "(this terminal will echo what you type)")
	}
	fmt.Fprint(os.Stderr, question)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if hidden {
		// The newline the user typed was swallowed along with the echo.
		fmt.Fprintln(os.Stderr)
	}
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// prompt asks for something that is not a secret.
func prompt(question string) (string, error) {
	if !isTerminal() {
		return "", errNotATerminal
	}
	fmt.Fprint(os.Stderr, question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// isTerminal reports whether stdin is something a person is typing at.
//
// The mode bits alone are not enough: /dev/null is a character device too, so
// `cmd < /dev/null` would look interactive and the prompt would sit there
// waiting for a keystroke that cannot arrive. Asking stty for the terminal
// settings only succeeds on a real one.
func isTerminal() bool {
	if runtime.GOOS == "windows" {
		info, err := os.Stdin.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0
	}
	_, err := stty("-g")
	return err == nil
}

// hideEcho turns off terminal echo and returns the function that puts it back,
// along with whether it managed to.
func hideEcho() (restore func(), hidden bool, err error) {
	noop := func() {}
	if runtime.GOOS == "windows" {
		if !isTerminal() {
			return noop, false, errNotATerminal
		}
		return noop, false, nil
	}

	saved, err := stty("-g")
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
