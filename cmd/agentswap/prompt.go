package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// errNotATerminal means there is nobody at the keyboard to ask.
var errNotATerminal = errors.New("stdin is not a terminal")

// promptSecret reads a secret from the terminal without echoing it.
//
// Where echo cannot be turned off, the user is told the input will be visible
// rather than having it silently displayed. See hideEcho for how each platform
// does it — without golang.org/x/term, because this process holds live OAuth
// tokens and does not get to have dependencies.
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
// The mode bits alone are not enough: /dev/null and NUL are character devices
// too, so `cmd < /dev/null` would look interactive and the prompt would sit
// there waiting for a keystroke that cannot arrive. Each platform has to ask
// the terminal itself, which is what terminalSettings does.
func isTerminal() bool {
	_, err := terminalSettings()
	return err == nil
}
