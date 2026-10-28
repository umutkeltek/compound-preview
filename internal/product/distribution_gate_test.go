package product

import (
	"strings"
	"testing"
)

// TestDistributionGateRefusesWithoutExplicitOptIn pins that the distribution loop
// cannot be reached by accident. The audit's point was that a comment saying
// "experimental" gates nothing — only a refusal does.
func TestDistributionGateRefusesWithoutExplicitOptIn(t *testing.T) {
	t.Setenv(DistributionExperimentEnv, "")
	err := RequireDistributionExperiment("content-draft record --generate")
	if err == nil {
		t.Fatal("a distribution door must refuse without an explicit opt-in")
	}
	if !strings.Contains(err.Error(), DistributionExperimentEnv) {
		t.Fatalf("refusal must name the opt-in, got: %v", err)
	}
	// An opt-in nobody understands is not consent: the refusal must show what is
	// still broken, not merely how to bypass itself.
	for _, gap := range KnownDistributionGaps {
		if !strings.Contains(err.Error(), gap) {
			t.Fatalf("refusal must disclose known gap %q, got: %v", gap, err)
		}
	}
}

// TestDistributionGateAllowsExplicitOptIn keeps the work reachable — the decision
// was to gate the distribution loop, not to archive it.
func TestDistributionGateAllowsExplicitOptIn(t *testing.T) {
	t.Setenv(DistributionExperimentEnv, "1")
	if err := RequireDistributionExperiment("content-draft record --generate"); err != nil {
		t.Fatalf("an explicit opt-in must be honoured: %v", err)
	}
}

// TestDistributionGateRejectsNearMisses pins that only an exact "1" opts in, so a
// stray or truthy-looking value cannot silently enable an unsupported surface.
func TestDistributionGateRejectsNearMisses(t *testing.T) {
	for _, value := range []string{"0", "true", "yes", "2", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(DistributionExperimentEnv, value)
			if err := RequireDistributionExperiment("verb"); err == nil {
				t.Fatalf("%q must not count as an opt-in", value)
			}
		})
	}
}
