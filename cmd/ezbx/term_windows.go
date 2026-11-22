//go:build windows

package main

import (
	"os"
	"syscall"
)

// enableVirtualTerminal is ENABLE_VIRTUAL_TERMINAL_PROCESSING: the console
// reads colour codes instead of printing them.
const enableVirtualTerminal = 0x0004

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// isTerminal reports whether f is a console. Windows calls the NUL device a
// character device too, so only asking the console itself is reliable.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

// enableColor turns on colour codes for console f, as Windows Terminal has
// them already and older consoles need asking. It reports whether they work;
// if the console says no, the caller prints plain text.
func enableColor(f *os.File) bool {
	var mode uint32
	h := syscall.Handle(f.Fd())
	if syscall.GetConsoleMode(h, &mode) != nil {
		return false
	}
	if mode&enableVirtualTerminal != 0 {
		return true
	}
	ok, _, _ := setConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminal))
	return ok != 0
}
