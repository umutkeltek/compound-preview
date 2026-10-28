//go:build !darwin

package schedule

import (
	"fmt"
	"os"
)

// Run refuses loudly off darwin: the projection observes launchd/crontab and
// there is nothing to observe on other platforms — an empty projection would
// read as "nothing scheduled". The command registry references schedule.Run
// unconditionally, so this stub keeps non-darwin builds compiling (pattern:
// the parked darwin_surface_stub.go preserved in archive commit 40af455b).
//
// Known subcommands validate their flags BEFORE the platform refusal so the
// flag dialect is identical on every platform: without this, linux builds
// "refuse" unknown flags only incidentally (via the exit-2 platform error)
// while darwin silently accepted them — a split the flag-validation ratchet
// census cannot pin (CI follow-up, 2026-09-19).
func Run(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "repair":
			os.Exit(runScheduleRepair(args[1:]))
		case "list", "show", "doctor":
			if err := validateScheduleFlags(args[0], args[1:], 0); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				fmt.Fprintln(os.Stderr, scheduleUsage)
				os.Exit(2)
			}
		}
	}
	fmt.Fprintln(os.Stderr, "compound schedule is only supported on darwin (launchd/crontab projection)")
	os.Exit(2)
}
