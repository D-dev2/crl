//go:build darwin

package shell

import (
	"fmt"
	"syscall"
	"unsafe"
)

func terminalWidth(fd uintptr) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	return 80
}

func terminalHeight(fd uintptr) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno == 0 && ws.Row > 0 {
		return int(ws.Row)
	}
	return 24
}

func makeRaw(fd uintptr) (func(), error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno != 0 {
		return nil, fmt.Errorf("lecture du terminal : %v", errno)
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&raw)), 0, 0, 0); errno != 0 {
		return nil, fmt.Errorf("passage du terminal en mode brut : %v", errno)
	}
	return func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}, nil
}
