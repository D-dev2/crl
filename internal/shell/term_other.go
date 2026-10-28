//go:build !linux && !darwin

package shell

import "fmt"

func makeRaw(fd uintptr) (func(), error) {
	return nil, fmt.Errorf("édition interactive non prise en charge sur cette plateforme")
}

func terminalWidth(fd uintptr) int { return 80 }
