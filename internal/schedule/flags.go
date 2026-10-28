package schedule

// Flag validation for the schedule surface, platform-neutral so the darwin
// CLI and the not-darwin stub enforce the SAME dialect (see run_notdarwin.go):
// an unknown flag must be refused identically everywhere, or the
// flag-validation ratchet census splits across platforms (CI follow-up, 2026-09-19).

import (
	"fmt"
	"strings"
)

const scheduleUsage = "usage: compound schedule list [--json] [--source user|system|crontab|all] [--namespace N] [--include-env-values]\n" +
	"       compound schedule show <label|path> [--json]\n" +
	"       compound schedule doctor [--json] [--stale-days N]\n" +
	"  Read-only list/show/doctor; repair plan|review|apply|rollback is exact and governed."

// table-driven and one-source-of-truth so adding a new flag means one row
// here plus the consume site.
var scheduleFlagSets = map[string]map[string]bool{
	"list": {
		"--json":               true,
		"--source":             true,
		"--namespace":          true,
		"--include-env-values": true,
	},
	"show": {
		"--json": true,
	},
	"doctor": {
		"--json":       true,
		"--stale-days": true,
	},
}

// validateScheduleFlags fails closed on unknown flags and on too many
// positional args. Centralized so list/show/doctor speak the same dialect;
// doctor callers that want to add a flag touch exactly two places (here
// + the consume site).
func validateScheduleFlags(sub string, args []string, maxPos int) error {
	known, ok := scheduleFlagSets[sub]
	if !ok {
		return fmt.Errorf("unknown subcommand %q", sub)
	}
	positional := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			positional++
			continue
		}
		name := a
		if eq := strings.Index(a, "="); eq >= 0 {
			name = a[:eq]
		}
		if !known[name] {
			return fmt.Errorf("unknown flag %q", a)
		}
		// Value-requiring flags: consume the next arg if it doesn't look
		// like another flag. The list of value-requiring flags lives here
		// too — keeps the validation authoritative.
		if (name == "--source" || name == "--namespace" || name == "--stale-days") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
		}
	}
	if positional > maxPos {
		return fmt.Errorf("too many positional args (got %d, max %d)", positional, maxPos)
	}
	return nil
}
