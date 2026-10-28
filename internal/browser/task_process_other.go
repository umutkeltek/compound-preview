//go:build !darwin && !linux

package browser

import (
	"errors"
	"os/exec"
)

func jevProcessSupported() bool       { return false }
func configureJevProcess(_ *exec.Cmd) {}
func newJevSession() (string, error) {
	return "", errors.New("Jev sessions are unsupported on this OS")
}
func cleanupJevSession(string) error { return nil }
