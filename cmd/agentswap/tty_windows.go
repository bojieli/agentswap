//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func stdinIsTerminal() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	var mode uint32
	ok, _, _ := getConsoleMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&mode)))
	return ok != 0
}
