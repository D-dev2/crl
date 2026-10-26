//go:build linux

package termstyle

import (
	"syscall"
	"unsafe"
)

// IsTerminal indique si le descripteur est un terminal.
func IsTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}
