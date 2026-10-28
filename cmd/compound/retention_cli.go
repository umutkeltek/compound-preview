package main

// `compound retention` — the read-only surface a storage controller calls.
// The controller supplies the consumer's published thresholds. Compound inventories declared
// artifact classes and returns a plan; it never reclaims anything itself.
//
// The body lives in internal/policy.RetentionCLI so the cmd-compound-budget
// gate can ratchet DOWN — verb bodies move to existing internal/ organs,
// not raise the ceiling. The wrapper here is intentionally thin: it just
// delegates the argv + exit code through to the policy package and exits.

import (
	"os"

	"compound/internal/policy"
)

func cmdRetention(args []string) {
	os.Exit(policy.RetentionCLI(args, os.Stdout, os.Stderr))
}
