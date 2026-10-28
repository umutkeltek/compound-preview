package harness

import (
	"strings"
	"testing"
)

// This file pins the oh_my_pi version pin to the binary that is actually
// installed and probed on this host. The doctor's harness_version_drift
// signal compares ExpectedVersion against ObservedVersion, whose verdict is
// the binary relation expected != observed (classifyDrift in
// internal/doctor/harness_version_drift.go — this package cannot import it,
// so the tests below restate that relation inline). A pin that lags the
// installed binary fires false DRIFT on every doctor run, which is the exact
// always-on signal the 9873b82a re-pin repaired.

// ohMyPiVersionOutput is the literal shape `omp --version` emits on this host
// (stdout, "omp/18.0.10" — no space, slash-separated). The fake runner below
// feeds it through the REAL parser, so the test fails if the pin stops
// round-tripping through parseSemverToken even when the raw strings agree.
const ohMyPiVersionOutput = "omp/18.1.17"

// TestOhMyPiVersionPinMatchesObservedRuntime is the match verdict: the pin
// MUST equal what the installed binary reports, or harness_version_drift
// fires on every doctor run. The hardcoded 18.0.10 is deliberate — it is the
// observed runtime this pin was raised to, and bumping the test means
// consciously re-proving the upgrade, not riding along with an edit.
func TestOhMyPiVersionPinMatchesObservedRuntime(t *testing.T) {
	h, ok := DefaultRegistry().Get("oh_my_pi")
	if !ok {
		t.Fatal("DefaultRegistry() missing oh_my_pi")
	}
	observed := ObservedVersionWithRunner("oh_my_pi", func(binary string) (string, error) {
		if binary != "omp" {
			t.Fatalf("runner called with binary %q, want omp (binaryForHarness row)", binary)
		}
		return ohMyPiVersionOutput, nil
	})
	pin := strings.TrimSpace(h.ExpectedVersion)
	if observed != pin {
		t.Fatalf("oh_my_pi pin %q != observed %q parsed from %q — harness_version_drift fires "+
			"on every doctor run; re-pin with the upgrade, not after it", pin, observed, ohMyPiVersionOutput)
	}
}

// TestOhMyPiVersionPinStillReportsDriftForOtherVersions is the drift verdict:
// any OTHER observed version — including 18.0.4, the superseded pin this
// table replaced — MUST still differ from the pin, so the signal can actually
// fire when the binary moves again. Without this the match test above could
// pass vacuously by comparing the runner's output to itself.
func TestOhMyPiVersionPinStillReportsDriftForOtherVersions(t *testing.T) {
	h, ok := DefaultRegistry().Get("oh_my_pi")
	if !ok {
		t.Fatal("DefaultRegistry() missing oh_my_pi")
	}
	pin := strings.TrimSpace(h.ExpectedVersion)
	for _, raw := range []string{
		"omp/18.0.4", // the superseded pin — drift now, and that is the point
		"omp/17.4.0", // the pin 9873b82a replaced
		"omp/18.1.18",
		"omp/18.0.4",
	} {
		observed := ObservedVersionWithRunner("oh_my_pi", func(string) (string, error) {
			return raw, nil
		})
		if observed == "" {
			t.Fatalf("parseSemverToken(%q) = \"\" — the drift case stopped parsing", raw)
		}
		if strings.EqualFold(pin, observed) {
			t.Fatalf("observed %q (from %q) equals the pin %q — the drift verdict cannot fire", observed, raw, pin)
		}
	}
}
