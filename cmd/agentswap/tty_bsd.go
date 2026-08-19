//go:build darwin || freebsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func stdinIsTerminal() bool {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, os.Stdin.Fd(), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	return errno == 0
}
