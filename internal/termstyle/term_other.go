//go:build !linux && !darwin

package termstyle

// IsTerminal retourne false sur les plateformes non prises en charge.
func IsTerminal(fd uintptr) bool { return false }
