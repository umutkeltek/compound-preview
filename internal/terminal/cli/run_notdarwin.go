//go:build !darwin

package cli

import (
	"fmt"
	"os"
)

// Run refuses loudly on non-darwin platforms: the terminal surface observes
// macOS terminal state (AppleScript bridges, IPC sockets), so there is no
// meaningful implementation elsewhere. The registry entry still resolves so
// `compound terminal` fails closed with a clear exit code instead of a
// confusing empty surface.
func Run(args []string) {
	fmt.Fprintln(os.Stderr, "error: terminal surface requires macOS (darwin)")
	os.Exit(2)
}
