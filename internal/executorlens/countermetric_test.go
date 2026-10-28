package executorlens

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"compound/internal/behaviorallens"
	"compound/internal/candidates"
	"compound/internal/distiller"
)

// Synthetic close-summary text only (no personal/product names): the repo is destined public.
const (
	unpairedCounterMetricSummary = "Landed the change: attention 13-26s → 3-5s, 2.9x per-engineer throughput."
	pairedCounterMetricSummary   = "Landed the change: attention 13-26s → 3-5s, 2.9x per-engineer throughput; " +
		"reversion rate flat, review latency held."
	noClaimCounterMetricSummary = "Recorded the decision and parked the intake for later adjudication."
)

// failurePatternFiles lists the (non-directory) entries under home/inbox/failure-patterns, using
// candidates.InboxDir as the one source of truth for the path (never a hardcoded string).
func failurePatternFiles(t *testing.T, home string) []string {
	t.Helper()
	dir := filepath.Join(home, candidates.InboxDir(candidates.FailurePattern))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read failure-pattern inbox: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// FALSIFIER 4 (counter-metric, collector level) — the load-bearing mint proof. A receipt whose close
// summary claims an improvement with NO counter-metric named produces a counter-metric public_safe
// signal at the negative pole, and --record direct-mints exactly ONE proposed failure_pattern
// candidate (promotion_basis empty, derivation observed) that round-trips
// distiller.BuildFailureRulePlan. Re-running the SAME day with --record mints no second candidate
// (day-salted dedup).
func TestCounterMetricFalsifier_UnpairedClaimMintsOneFailurePattern(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_unpaired01", "codex-native", "codex", "work_alpha", "completed",
		unpairedCounterMetricSummary, "2026-07-17T10:00:00Z")

	report, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var cm *behaviorallens.InferredSignal
	for i := range report.PublicSignals {
		s := report.PublicSignals[i]
		if s.LensID == behaviorallens.CounterMetricLensID {
			cm = &report.PublicSignals[i]
		}
	}
	if cm == nil {
		t.Fatalf("expected a counter-metric public_safe signal; got %+v", report.PublicSignals)
	}
	if cm.Value >= 0 {
		t.Fatalf("unpaired claim must score the negative pole, got %.3f", cm.Value)
	}
	if cm.Visibility != behaviorallens.VisibilityPublicSafe {
		t.Fatalf("counter-metric signal must be public_safe, got %q", cm.Visibility)
	}

	if len(report.MintErrors) != 0 {
		t.Fatalf("mint must not error: %v", report.MintErrors)
	}
	if len(report.FailurePatternCandidates) != 1 {
		t.Fatalf("expected exactly 1 minted failure_pattern candidate, got %d: %v", len(report.FailurePatternCandidates), report.FailurePatternCandidates)
	}
	candID := report.FailurePatternCandidates[0]

	files := failurePatternFiles(t, home)
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 file in inbox/failure-patterns, got %d: %v", len(files), files)
	}
	blobs, err := os.ReadDir(filepath.Join(home, filepath.FromSlash(counterMetricFailureDir)))
	if err != nil {
		t.Fatalf("read content-addressed observations: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("expected exactly 1 content-addressed observation, got %d", len(blobs))
	}
	blobRef := "counter-metric:" + blobs[0].Name()

	var c struct {
		ID             string   `json:"id"`
		Kind           string   `json:"kind"`
		Derivation     string   `json:"derivation"`
		PromotionBasis string   `json:"promotion_basis"`
		State          string   `json:"state"`
		EvidenceRefs   []string `json:"evidence_refs"`
	}
	if err := candidates.ReadCandidate(home, candidates.FailurePattern, candID, &c); err != nil {
		t.Fatalf("read minted candidate: %v", err)
	}
	if c.Kind != string(candidates.FailurePattern) {
		t.Fatalf("candidate kind = %q, want %q", c.Kind, candidates.FailurePattern)
	}
	if c.Derivation != "observed" {
		t.Fatalf("candidate derivation = %q, want observed", c.Derivation)
	}
	if c.PromotionBasis != "" {
		t.Fatalf("candidate promotion_basis must be empty (never auto-promotable), got %q", c.PromotionBasis)
	}
	if c.State != "proposed" {
		t.Fatalf("candidate state = %q, want proposed", c.State)
	}
	hasRef := func(want string) bool {
		for _, ref := range c.EvidenceRefs {
			if ref == want {
				return true
			}
		}
		return false
	}
	if !hasRef("compound://events/rec_unpaired01") || !hasRef(blobRef) {
		t.Fatalf("candidate must cite receipt and content-addressed observation: refs=%v want_blob=%s", c.EvidenceRefs, blobRef)
	}

	// Round-trips distiller.BuildFailureRulePlan — the reader requires the distiller.Candidate JSON
	// field shape. This round-trip IS the falsifier; the import stays test-only (internal/distiller
	// is never imported from this package's non-test source).
	plan, err := distiller.BuildFailureRulePlan(home, candID, "", fixedNow)
	if err != nil {
		t.Fatalf("BuildFailureRulePlan: %v", err)
	}
	if plan.CandidateID != candID {
		t.Fatalf("plan candidate id = %q, want %q", plan.CandidateID, candID)
	}
	if plan.Statement == "" {
		t.Fatal("plan statement must round-trip from the minted candidate")
	}

	// re-run the SAME day with --record: day-salted dedup skips a second candidate file.
	report2, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run (re-run): %v", err)
	}
	if len(report2.FailurePatternCandidates) != 1 || report2.FailurePatternCandidates[0] != candID {
		t.Fatalf("re-run same day must resolve to the SAME single candidate id, got %v want [%s]", report2.FailurePatternCandidates, candID)
	}
	if filesAfter := failurePatternFiles(t, home); len(filesAfter) != 1 {
		t.Fatalf("re-run same day must NOT mint a second candidate file, got %d: %v", len(filesAfter), filesAfter)
	}

	// THE dedup invariant that actually matters (content-addressing alone would make a blind
	// same-content rewrite look identical): skip-if-exists must never reset an IN-FLIGHT
	// adjudication verdict. Simulate an adjudicator having moved the candidate on, then prove a
	// same-day re-run leaves that state untouched.
	c.State = "operationalize"
	if err := candidates.WriteCandidate(home, candidates.FailurePattern, candID, &c); err != nil {
		t.Fatalf("simulate adjudication write: %v", err)
	}
	if _, err := Run(home, true, fixedNow); err != nil {
		t.Fatalf("Run (post-adjudication re-run): %v", err)
	}
	var afterAdjudication struct {
		State string `json:"state"`
	}
	if err := candidates.ReadCandidate(home, candidates.FailurePattern, candID, &afterAdjudication); err != nil {
		t.Fatalf("read post-adjudication candidate: %v", err)
	}
	if afterAdjudication.State != "operationalize" {
		t.Fatalf("a same-day re-run must NOT reset an in-flight adjudication verdict, got state=%q", afterAdjudication.State)
	}
}

func TestCounterMetricFailureCandidateDedupIgnoresClassifierDetailDrift(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_classifier_drift", "codex-native", "codex", "work_delta", "completed",
		unpairedCounterMetricSummary, "2026-07-17T10:00:00Z")

	report, err := Run(home, false, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var original *behaviorallens.InferredSignal
	for i := range report.PublicSignals {
		if report.PublicSignals[i].LensID == behaviorallens.CounterMetricLensID {
			original = &report.PublicSignals[i]
			break
		}
	}
	if original == nil {
		t.Fatalf("expected counter-metric signal, got %+v", report.PublicSignals)
	}

	firstID, err := mintCounterMetricFailurePattern(home, *original, fixedNow)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	var adjudicated failurePatternCandidate
	if err := candidates.ReadCandidate(home, candidates.FailurePattern, firstID, &adjudicated); err != nil {
		t.Fatalf("read first candidate: %v", err)
	}
	adjudicated.State = candidates.Operationalize
	if err := candidates.WriteCandidate(home, candidates.FailurePattern, firstID, &adjudicated); err != nil {
		t.Fatalf("simulate adjudication write: %v", err)
	}
	drifted := *original
	drifted.Value = -0.25
	drifted.Detail = "classifier wording changed while the logical receipt and lens stayed fixed"
	secondID, err := mintCounterMetricFailurePattern(home, drifted, fixedNow)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if secondID != firstID {
		t.Fatalf("same receipt/lens/day must keep one candidate id across classifier drift: first=%s second=%s", firstID, secondID)
	}
	if files := failurePatternFiles(t, home); len(files) != 1 {
		t.Fatalf("classifier drift must not mint a second candidate, got %d: %v", len(files), files)
	}
	blobs, err := os.ReadDir(filepath.Join(home, filepath.FromSlash(counterMetricFailureDir)))
	if err != nil {
		t.Fatalf("read content-addressed observations: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("same-day classifier drift must not persist an orphan blob, got %d blobs", len(blobs))
	}
	var preserved failurePatternCandidate
	if err := candidates.ReadCandidate(home, candidates.FailurePattern, firstID, &preserved); err != nil {
		t.Fatalf("read deduplicated candidate: %v", err)
	}
	if preserved.State != candidates.Operationalize {
		t.Fatalf("classifier drift must preserve adjudicated state, got %q", preserved.State)
	}
	for _, blob := range blobs {
		want := "counter-metric:" + blob.Name()
		if !slices.Contains(preserved.EvidenceRefs, want) {
			t.Fatalf("stored classifier evidence is orphaned: candidate refs=%v missing=%s", preserved.EvidenceRefs, want)
		}
	}
}

// FALSIFIER (counter-metric, collector level) — the SAME improvement claim, paired with
// counter-metric text, scores the positive pole and mints NO failure_pattern candidate.
func TestCounterMetricFalsifier_PairedClaimMintsNothing(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_paired01", "codex-native", "codex", "work_beta", "completed",
		pairedCounterMetricSummary, "2026-07-17T10:00:00Z")

	report, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var cm *behaviorallens.InferredSignal
	for i := range report.PublicSignals {
		s := report.PublicSignals[i]
		if s.LensID == behaviorallens.CounterMetricLensID {
			cm = &report.PublicSignals[i]
		}
	}
	if cm == nil {
		t.Fatalf("expected a counter-metric public_safe signal; got %+v", report.PublicSignals)
	}
	if cm.Value <= 0 {
		t.Fatalf("paired claim must score the positive pole, got %.3f", cm.Value)
	}
	if len(report.FailurePatternCandidates) != 0 {
		t.Fatalf("a paired claim must mint NO failure_pattern candidate, got %v", report.FailurePatternCandidates)
	}
	if got := failurePatternFiles(t, home); len(got) != 0 {
		t.Fatalf("a paired claim must leave inbox/failure-patterns empty, got %v", got)
	}
}

// FALSIFIER (counter-metric, collector level) — a receipt with no improvement claim at all produces
// no counter-metric signal, is a legible Skipped note (not a fatal Run error), and mints nothing.
func TestCounterMetricFalsifier_NoClaimProducesNoSignal(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_none01", "codex-native", "codex", "work_gamma", "completed",
		noClaimCounterMetricSummary, "2026-07-17T10:00:00Z")

	report, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range report.PublicSignals {
		if s.LensID == behaviorallens.CounterMetricLensID {
			t.Fatalf("a receipt with no improvement claim must produce no counter-metric signal, got %+v", s)
		}
	}
	var skippedCounterMetric bool
	for _, sk := range report.Skipped {
		if sk.LensID == behaviorallens.CounterMetricLensID {
			skippedCounterMetric = true
		}
	}
	if !skippedCounterMetric {
		t.Fatalf("expected the counter-metric lens to be a legible skip, got %+v", report.Skipped)
	}
	if len(report.FailurePatternCandidates) != 0 {
		t.Fatalf("no claim -> no mint, got %v", report.FailurePatternCandidates)
	}
}

// A non-comparative percentage is ordinary verification evidence, not an improvement claim. The
// public collector seam must therefore skip the counter-metric lens and leave the failure-pattern
// inbox untouched even when recording is enabled.
func TestCounterMetricFalsifier_NonComparativePercentageMintsNothing(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_passrate01", "codex-native", "codex", "work_delta", "completed",
		"Verification completed with 100% of tests passing.", "2026-07-17T10:00:00Z")

	report, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var counterMetricSignals int
	for _, signal := range report.PublicSignals {
		if signal.LensID == behaviorallens.CounterMetricLensID {
			counterMetricSignals++
		}
	}
	files := failurePatternFiles(t, home)
	if counterMetricSignals != 0 || len(report.FailurePatternCandidates) != 0 || len(files) != 0 {
		t.Fatalf("non-comparative pass rate produced signals=%d candidates=%v files=%v",
			counterMetricSignals, report.FailurePatternCandidates, files)
	}
}

// The counter-metric Observation the collector builds carries the receipt's canonical logical event
// reference (mirrors TestExecutorObservationsUseLogicalEventReferences for hedging).
func TestCounterMetricObservationsUseLogicalEventReferences(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_logical02", "codex-native", "codex", "work_x", "completed",
		unpairedCounterMetricSummary, "2026-07-17T10:00:00Z")
	col, err := CollectObservations(home)
	if err != nil {
		t.Fatalf("CollectObservations: %v", err)
	}
	if len(col.CounterMetric) != 1 {
		t.Fatalf("counter-metric observations = %d, want 1", len(col.CounterMetric))
	}
	refs := col.CounterMetric[0].EvidenceRefs
	if len(refs) != 1 || refs[0] != "compound://events/rec_logical02" {
		t.Fatalf("evidence refs = %v, want canonical logical event ref", refs)
	}
}
