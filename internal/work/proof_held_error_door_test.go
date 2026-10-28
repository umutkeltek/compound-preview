package work

import (
	"strings"
	"testing"
	"time"
)

// TestProofResourceHeldErrorNamesHolderExpiryAndReleaseDoor asserts that the
// refusal emission for an ALREADY-HELD proof/land reservation names every gate
// the keeper needs to break the 50-minute-stale block: (1) the holder ref,
// (2) the exact RFC3339 expiry, and (3) the governed release door expressed
// as the --terminated-run RATT_ID / --termination-evidence ARTIFACT_REF flag
// pair. Omitting the door leaves the keeper blocked; the test refuses to
// tolerate that omission.
func TestProofResourceHeldErrorNamesHolderExpiryAndReleaseDoor(t *testing.T) {
	t.Parallel()
	holder := "ratt-keep-alpha"
	reservationID := "pfr_test-keep-001"
	workID := "work-keep-001"
	expiresAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)

	err := &ProofResourceHeldError{
		WorkID:        workID,
		ReservationID: reservationID,
		Holder:        holder,
		ExpiresAt:     expiresAt,
		Remaining:     47 * 60,
	}

	msg := err.Error()

	// (1) Holder ref must be present, exact.
	if !strings.Contains(msg, holder) {
		t.Fatalf("held refusal missing holder ref %q: %s", holder, msg)
	}

	// (2) Exact RFC3339 expiry must be present, verbatim.
	if !strings.Contains(msg, expiresAt) {
		t.Fatalf("held refusal missing RFC3339 expiry %q: %s", expiresAt, msg)
	}

	// (3) Governed release door: both flags must be present, in the same message.
	if !strings.Contains(msg, "--terminated-run") {
		t.Fatalf("held refusal missing --terminated-run release door flag: %s", msg)
	}
	if !strings.Contains(msg, "--termination-evidence") {
		t.Fatalf("held refusal missing --termination-evidence release door flag: %s", msg)
	}
}
