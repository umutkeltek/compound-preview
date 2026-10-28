// Helpers for the `compound work list` CLI. The cmd handler lives in
// cmd/compound/work_cli.go; the canonical name of every accepted flag lives
// here so the usage string, the validation helper, and the error message
// cannot drift. Adding a flag means one entry in workListFlags plus the
// switch arms in cmdWork's list case (failure mode: validate accepts a value
// the handler silently drops, which is exactly the silent-ignore bug
// work-list-flags-20260817 exists to prevent).
package work

import (
	"fmt"
	"strings"
)

// workListFlags is the canonical set of flags accepted by `compound work list`.
// Listed here, not in the cmd switch, so the usage string, the validator, and
// the handler all reference the same table.
var workListFlags = []string{"--json"}

// WorkListUsage is the usage string for `compound work list`. Rendered by
// `compound work list --help` and by the validator when it rejects an
// unknown flag, so the operator can see what they should have typed.
const WorkListUsage = "usage: compound work list [--json]"

// ValidateWorkListFlags returns nil when args contains only flags from the
// canonical set (or is empty — the default human output is legal), and an
// error naming the canonical set otherwise. The validator fails closed so a
// typo cannot silently produce the fixed-width table when the caller
// expected JSON, and so a stray positional argument (work show's contract
// takes <work_id>, work list does not) is rejected instead of silently
// dropped by the handler.
func ValidateWorkListFlags(args []string) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown argument %q; valid flags: %s", a, strings.Join(workListFlags, ", "))
		}
		ok := false
		for _, f := range workListFlags {
			if a == f {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unknown flag %q; valid flags: %s", a, strings.Join(workListFlags, ", "))
		}
	}
	return nil
}
