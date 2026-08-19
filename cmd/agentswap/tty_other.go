//go:build !linux && !darwin && !freebsd && !windows

package main

import "os"

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
