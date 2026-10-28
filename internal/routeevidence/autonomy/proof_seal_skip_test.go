package autonomy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	autonomy "compound/internal/routeevidence/autonomy"
)

// Falsifier: one envelope whose recorded custody seal disagrees with its
// bytes (the legacy unsealed-sweep-mutation shape — 71 envelopes on
// 2026-08-28, each of which vetoed a whole hourly `director propose --apply`
// pass) must be SKIPPED with its id reported on the artifact, never allowed
// as a sample, and must not fail the compilation for the good envelopes in
// the same cohort.
//
// The damage cannot be produced through the public write doors anymore
// (sweep-stale re-seals since e5ecfd35), so the fixture hand-mutates the
// envelope file after close — deliberately bypassing the write door to
// reproduce the exact on-disk shape the incident left behind.
func TestCompileSkipsSealMismatchEnvelopeInsteadOfFailingCohort(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	good1 := seedSample(t, home, plan, "seal-skip-good-1", proofNow.Add(-72*time.Hour), sampleOptions{})
	good2 := seedSample(t, home, plan, "seal-skip-good-2", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "seal-skip-victim", proofNow.Add(-24*time.Hour), sampleOptions{})

	// Hand-mutate the victim envelope after its sealed close: the recorded
	// terminal seal now describes bytes that no longer exist.
	envelopePath := filepath.Join(home, "work", "work", victim.WorkID+".json")
	raw, err := os.ReadFile(envelopePath)
	if err != nil {
		t.Fatal(err)
	}
	var work envelope.WorkEnvelope
	if err := json.Unmarshal(raw, &work); err != nil {
		t.Fatal(err)
	}
	work.DesiredOutcome = "mutated after terminal seal"
	body, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envelopePath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Baseline falsifier check: with TWO good samples plus the damaged one,
	// compilation must complete. Pre-fix behavior was a global error
	// "recent terminal work <id> snapshot hash does not match terminal event"
	// that vetoed the whole cohort.
	artifact := compile(t, home, plan, 2)

	// The damaged envelope is reported and never a sample.
	found := false
	for _, id := range artifact.ExcludedSealMismatchIDs {
		if id == victim.WorkID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seal-mismatch victim %s not reported on excluded_seal_mismatch_ids: %+v", victim.WorkID, artifact.ExcludedSealMismatchIDs)
	}
	for _, sample := range artifact.Samples {
		if sample.WorkID == victim.WorkID {
			t.Fatalf("seal-mismatch victim %s must never be a sample: %+v", victim.WorkID, sample)
		}
	}
	// The good envelopes still form the cohort: both must be sampled.
	sampled := map[string]bool{}
	for _, sample := range artifact.Samples {
		sampled[sample.WorkID] = true
	}
	if !sampled[good1.WorkID] || !sampled[good2.WorkID] {
		t.Fatalf("good envelopes must still be sampled after skipping the damaged one, samples=%+v ids=%+v/%+v", artifact.Samples, good1, good2)
	}
	// The skip is visible, not silent: a reason naming the victim must be
	// derivable from the artifact itself.
	if artifact.ExcludedUnverifiableCount < 1 {
		t.Fatalf("skipped seal mismatch must count toward excluded_unverifiable_count, got %d", artifact.ExcludedUnverifiableCount)
	}
	if err := autonomy.Validate(artifact); err != nil {
		t.Fatalf("artifact with seal skip must validate: %v", err)
	}
	if strings.Join(artifact.ExcludedSealMismatchIDs, ",") != victim.WorkID {
		t.Fatalf("excluded_seal_mismatch_ids must stay sorted and unique: %+v", artifact.ExcludedSealMismatchIDs)
	}
}
