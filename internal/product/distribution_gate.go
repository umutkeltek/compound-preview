package product

import (
	"fmt"
	"os"
	"strings"
)

// distribution_gate.go marks the distribution loop as EXPERIMENTAL at its doors.
//
// The v0.2 path proves one synthetic package-to-measurement vertical. Live
// transport enrollment and external provider acceptance remain explicit gates.
const DistributionExperimentEnv = "COMPOUND_EXPERIMENTAL_DISTRIBUTION"

// KnownDistributionGaps are the remaining support limits at the opt-in door.
// The exact package, durable attempt and typed observation contracts are owned
// by docs/architecture/contracts/content-transport.md.
var KnownDistributionGaps = []string{
	"live adapters require explicit v0.2 enrollment and operator acceptance",
	"provider idempotency and observation semantics require real contract verification",
	"automatic cancellation, replacement attempts and transport migration are not supported",
	"synthetic media acceptance does not prove every provider media limit",
}

// RequireDistributionExperiment refuses a distribution door unless the operator has
// explicitly opted in. The refusal names the gaps rather than just the flag: an
// opt-in nobody understands is not consent.
func RequireDistributionExperiment(verb string) error {
	if strings.TrimSpace(os.Getenv(DistributionExperimentEnv)) == "1" {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is part of the EXPERIMENTAL distribution loop and is not a supported surface.\n", verb)
	fmt.Fprintf(&b, "Set %s=1 to use it. Known gaps you would be accepting:\n", DistributionExperimentEnv)
	for _, gap := range KnownDistributionGaps {
		fmt.Fprintf(&b, "  - %s\n", gap)
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}
