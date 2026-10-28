//go:build !darwin

package browser

import (
	"fmt"
	"os"
)

// Run refuses loudly off darwin: the projection observes macOS process
// surfaces (ps + agent-browser chrome instances) and there is nothing to
// observe on other platforms — an empty projection would read as "no browser
// instances". The command registry references browser.Run unconditionally,
// so this stub keeps non-darwin builds compiling (pattern: the parked
// darwin_surface_stub.go preserved in archive commit 40af455b).
func Run(args []string) {
	if handled, code := runSelfHealCLI(args, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if handled, err := runPortableBrowser(args, os.Stdout); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "compound browser is only supported on darwin (macOS process projection)")
	os.Exit(2)
}
