package harness

import (
	"testing"
)

// TestBuildInstructionPackObservation_F1_SilenceOnClean mirrors
// TestBuildPermissionPostureObservationSilentOnCleanReport: a non-drifted
// report must NEVER hand the cmd layer a usable observation. The cmd
// bridge already gates on Drifted, but the package-level silence-on-match
// is non-negotiable — silence is the falsifier, not the cmd bridge.
func TestBuildInstructionPackObservation_F1_SilenceOnClean(t *testing.T) {
	report := InstructionPackDriftReport{
		Harness:        "claude_code",
		Path:           InstructionPackPath("", "claude_code", ""),
		Drifted:        false,
		ExpectedDigest: "sha256:abc",
		ObservedDigest: "sha256:abc",
	}
	obs, ok := BuildInstructionPackObservation(report)
	if ok {
		t.Fatalf("clean report must not produce an observation, got %+v", obs)
	}
	if obs.ObservationID != "" || obs.Title != "" {
		t.Fatalf("clean report must zero the observation, got %+v", obs)
	}
}

// TestBuildInstructionPackObservation_F2_DriftedStableId mirrors
// TestBuildPermissionPostureObservationStableIdentity at the instruction-
// pack seam. Same drift state ⇒ same observation id (so the intake
// dedupes to one card); genuinely different drift state ⇒ different id
// (so a distinct divergence card is minted). The "different drift state"
// here is a DIFFERENT ObservedDigest, which mirrors the way permission
// posture changes its id when the live allow list shrinks.
func TestBuildInstructionPackObservation_F2_DriftedStableId(t *testing.T) {
	report := InstructionPackDriftReport{
		Harness:        "claude_code",
		Path:           InstructionPackPath("", "claude_code", ""),
		Drifted:        true,
		ExpectedDigest: "sha256:expected",
		ObservedDigest: "sha256:observed",
		Details:        []string{"policy_core drifted"},
	}
	first, ok := BuildInstructionPackObservation(report)
	if !ok {
		t.Fatal("drifted report must produce an observation")
	}
	if first.ObservationID == "" {
		t.Fatal("observation id must not be empty")
	}
	if first.Title == "" {
		t.Fatal("title must not be empty")
	}
	if first.ExpectedState == first.ObservedState {
		t.Fatalf("expected/observed states must differ on a drifted report: %q",
			first.ExpectedState)
	}
	if !contains(first.EvidenceRefs, report.Path) {
		t.Fatalf("evidence refs must include the pack path %q, got %+v",
			report.Path, first.EvidenceRefs)
	}

	// Re-derive on identical inputs (a re-run of the bridge against the
	// SAME drift state, as a user re-invoking `compound harness
	// instruction-pack --drift --emit-divergence` would).
	second, ok := BuildInstructionPackObservation(report)
	if !ok {
		t.Fatal("re-derivation must produce an observation on the same drift state")
	}
	if first.ObservationID != second.ObservationID {
		t.Fatalf("same drift state must reproduce the same observation id: %q vs %q",
			first.ObservationID, second.ObservationID)
	}

	// Now mutate to a genuinely different drift state and confirm the id
	// changes.
	changed := report
	changed.ObservedDigest = "sha256:different"
	changed.Details = []string{"effective_contract layer drifted"}
	third, ok := BuildInstructionPackObservation(changed)
	if !ok {
		t.Fatal("a different drift state must produce an observation")
	}
	if third.ObservationID == first.ObservationID {
		t.Fatalf("a different drift state must mint a different observation id, got identical: %q",
			third.ObservationID)
	}
}

// TestBuildInstructionPackObservation_F3_MissingOnDiskMirrorsColdStart
// closes the S5 deviation loop: a missing on-disk pack (cold-start case)
// must surface as a drifted observation whose observed state is the
// literal "no_on_disk_pack" sentinel, not as silence. Without this, the
// cold-start case stays invisible to the intake seam — the seam would
// never see a card for a never-rendered harness, which is the exact
// observability gap the packet is closing.
func TestBuildInstructionPackObservation_F3_MissingOnDiskMirrorsColdStart(t *testing.T) {
	report := InstructionPackDriftReport{
		Harness:        "claude_code",
		Path:           InstructionPackPath("", "claude_code", ""),
		Drifted:        true,
		ExpectedDigest: "sha256:expected",
		ObservedDigest: "", // file missing
		Details:        []string{"on-disk latest.json is missing"},
	}
	obs, ok := BuildInstructionPackObservation(report)
	if !ok {
		t.Fatal("missing-on-disk drift must produce an observation, not silence")
	}
	if obs.ObservedState == obs.ExpectedState {
		t.Fatalf("missing-on-disk observed state must differ from expected, got both=%q",
			obs.ExpectedState)
	}
	if obs.ObservedState == "" {
		t.Fatal("missing-on-disk observed state must be a stable sentinel, not empty")
	}
}

// TestBuildInstructionPackObservation_F4_DriftedReportNeverHasEqualStates
// is the defensive mirror of permission-posture's "expected == observed
// but Drifted=true" guard: a drifted report whose computed digests STILL
// collide must hand back silence so the intake seam never drops a card
// it thinks it just minted.
func TestBuildInstructionPackObservation_F4_DriftedReportNeverHasEqualStates(t *testing.T) {
	report := InstructionPackDriftReport{
		Harness:        "claude_code",
		Path:           InstructionPackPath("", "claude_code", ""),
		Drifted:        true,
		ExpectedDigest: "sha256:same",
		ObservedDigest: "sha256:same",
	}
	obs, ok := BuildInstructionPackObservation(report)
	if ok {
		t.Fatalf("a drifted report with colliding digests must not emit, got %+v", obs)
	}
	if obs.ObservationID != "" {
		t.Fatalf("a colliding-digests drift must zero the observation, got id=%q",
			obs.ObservationID)
	}
}

// contains is a tiny string-slice membership helper used by the
// evidence-refs checks; kept local to the test file so we don't reach for
// a slices dependency that this package may not import.
func contains(needle []string, hay string) bool {
	for _, s := range needle {
		if s == hay {
			return true
		}
	}
	return false
}
