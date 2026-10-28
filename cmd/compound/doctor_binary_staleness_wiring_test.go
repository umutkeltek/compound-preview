package main

import (
	"strings"
	"testing"

	"compound/internal/home"
)

// The binary-staleness signal answers the question projection freshness never asks: the
// projections check asks "is this projection as new as the RUNNING binary", the staleness
// signal asks "is the running binary as new as MAIN". That gap is the 2026-08-17 evidence —
// doctor reported all-clear while the installed build sat five commits behind main.
//
// This falsifier pins the WIRING, not the verdict. It fails when the computation is reachable
// only on the projections check's failure path (the shape that shipped: block 8 nested inside
// the else of home.RunningBinaryBuiltAt), because then every HEALTHY run — the run the signal
// exists to serve — leaves doctorReport.BinaryStaleness at its zero value and prints the
// "could not determine" fallback to the operator. A signal that only appears when the binary
// cannot be resolved is dead on arrival: when RunningBinaryBuiltAt fails there is no installed
// SHA to compare in the first place.
//
// Deliberately unasserted: which relation comes back. Equal/ancestor/descendant/unknown all
// depend on the checkout and the installed binary under test; the wiring does not.
func TestDoctorComputesBinaryStalenessOnHealthyRun(t *testing.T) {
	t.Parallel()
	// The premise this test rests on: under `go test` the running executable resolves, so
	// doctor takes the HEALTHY projections branch. Asserted rather than assumed — if it ever
	// stopped holding, the test below would exercise the failure path and pass while proving
	// nothing.
	if _, err := home.RunningBinaryBuiltAt(); err != nil {
		t.Fatalf("premise broken: RunningBinaryBuiltAt must succeed under test, got %v", err)
	}

	homeDir := t.TempDir()
	base := t.TempDir()

	rep := runDoctorReport(t, homeDir, base, "/usr/local/bin/compound")
	if rep.BinaryStaleness.Relation == "" {
		t.Fatalf("healthy doctor run left binary staleness uncomputed: %+v", rep.BinaryStaleness)
	}

	text := runDoctorText(t, homeDir, base, "/usr/local/bin/compound")
	if strings.Contains(text, "could not determine installed SHA") {
		t.Fatalf("healthy doctor run printed the uncomputed-signal fallback:\n%s", text)
	}
}
