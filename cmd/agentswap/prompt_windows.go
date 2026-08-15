//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// The console API, reached through the standard library rather than
// golang.org/x/sys: this process holds live OAuth tokens and does not get to
// have dependencies.
var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableEchoInput is ENABLE_ECHO_INPUT from wincon.h.
const enableEchoInput = 0x0004

// terminalSettings returns the console mode, and an error when stdin is not a
// console. GetConsoleMode is the reliable test: NUL and a redirected file are
// character devices too, so the mode bits would call them terminals.
func terminalSettings() (uint32, error) {
	var mode uint32
	r, _, err := procGetConsoleMode.Call(
		uintptr(syscall.Handle(os.Stdin.Fd())),
		uintptr(unsafe.Pointer(&mode)),
	)
	if r == 0 {
		return 0, err
	}
	return mode, nil
}

// hideEcho clears ENABLE_ECHO_INPUT for the length of a prompt.
func hideEcho() (restore func(), hidden bool, err error) {
	noop := func() {}

	mode, err := terminalSettings()
	if err != nil {
		return noop, false, errNotATerminal
	}
	if err := setConsoleMode(mode &^ enableEchoInput); err != nil {
		// A console we cannot silence is still a console; the caller warns and
		// carries on rather than refusing to accept a key.
		return noop, false, nil //nolint:nilerr // deliberate: not fatal, only less private
	}
	return func() { _ = setConsoleMode(mode) }, true, nil
}

func setConsoleMode(mode uint32) error {
	r, _, err := procSetConsoleMode.Call(uintptr(syscall.Handle(os.Stdin.Fd())), uintptr(mode))
	if r == 0 {
		return err
	}
	return nil
}
