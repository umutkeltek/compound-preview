package main

import (
	"compound/internal/negatives"
)

// cmdNegatives is the thin dispatch onto the negatives organ's CLI body
// (internal/negatives.RunCLI). The body lives in the organ — mirroring the
// promotedriver pattern — so cmd/compound stays a thin dispatch layer under
// its LOC budget ratchet (internal/arch.checkCmdCompoundBudget).
func cmdNegatives(args []string) {
	negatives.RunCLI(args)
}
