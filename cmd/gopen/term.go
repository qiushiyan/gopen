//go:build darwin || linux

package main

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether fd is a terminal: the termios query succeeds only
// on a tty, unlike a character-device check, which /dev/null also passes.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
