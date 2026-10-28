package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/routeev"
)

// calibrationCandidateFiles returns the ids of calibration-kind candidates in the memory-
// candidates inbox (ProvenanceLane "evcard-calibration"), never counting any surprise-kind
// candidate (ProvenanceLane "work-close") that the same lifecycle might also mint if a
// prediction were staked. None of the tests below stake a prediction, so this is belt-and-braces
// per the task note: count candidates by kind, not just by directory listing size.
func calibrationCandidateFiles(t *testing.T, homePath string) []string {
	t.Helper()
	dir := filepath.Join(homePath, "inbox", "memory-candidates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read candidates dir: %v", err)
	}
	var ids []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		var cand mem.Candidate
		if err := candidates.ReadCandidate(homePath, candidates.Memory, id, &cand); err != nil {
			t.Fatalf("read candidate %s: %v", id, err)
		}
		if cand.ProvenanceLane == "evcard-calibration" {
			ids = append(ids, id)
		}
	}
	return ids
}

// runEVCardDeviationLifecycle runs a full CLI lifecycle (create->claim->advance->advance->close)
// for a work whose RouteDeviation cites cardRef (may be empty), closing with the given outcome.
// No prediction is staked (mirrors the task note: avoid staking a prediction in calibration
// tests so the work-surprise door's own candidate never mixes into these counts). --deviation-
// risk tidy is used throughout so a prediction is never REQUIRED for admission either (only
// judge-grade deviations require one).
func runEVCardDeviationLifecycle(t *testing.T, predicate, cardRef, outcome string) string {
	t.Helper()
	createArgs := []string{
		"create", "--owner", "compound-core", "--predicate", predicate,
		"--deviation-lane", "scout/chat", "--deviation-class", "judge-critique", "--deviation-risk", "tidy",
	}
	if cardRef != "" {
		createArgs = append(createArgs, "--deviation-card", cardRef)
	}
	out := captureStdout(t, func() { cmdWork(createArgs) })
	created := firstJSONObject(t, out)
	var workID string
	if err := json.Unmarshal(created["id"], &workID); err != nil || workID == "" {
		t.Fatalf("create output missing id:\n%s", out)
	}
	captureStdout(t, func() { cmdWork([]string{"claim", workID, "--by", "claude_code"}) })
	captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "running", "--by", "claude_code"}) })
	captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "verifying", "--by", "claude_code"}) })
	captureStdout(t, func() {
		cmdWork([]string{
			"close", workID, "--outcome", outcome, "--move-type", "control_plane_repair",
			"--by", "claude_code", "--proof", "check:proof",
		})
	})
	return workID
}

// TestWorkCloseCLIMintsEVCardCalibrationCandidateOnDivergedOutcome covers S4/S5's close-path
// stitch (spec docs_private/plan/ev-card-spec.md §5): a route-deviation work citing a
// priced_right-verdict EV card, closed --outcome failed, diverges from what the card staked
// (priced_right + failed -> priced_deviation_did_not_pay). This mints exactly ONE calibration
// candidate (empty PromotionBasis) and writes exactly one blob under
// observed/evcard-calibration/, and the ledger digest is byte-identical before and after (same
// EngineDoctor-green-by-construction guarantee as work-surprise).
func TestWorkCloseCLIMintsEVCardCalibrationCandidateOnDivergedOutcome(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	cardRef, _, err := routeev.Store(homePath, routeev.Card{
		SchemaVersion:   routeev.SchemaVersion,
		CapabilityClass: "judge-critique",
		Verdict:         routeev.VerdictPricedRight,
	})
	if err != nil {
		t.Fatalf("store ev card: %v", err)
	}

	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open mem store: %v", err)
	}
	before, err := memStore.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	workID := runEVCardDeviationLifecycle(t, "priced_right deviation closes failed", cardRef, "failed")

	cids := calibrationCandidateFiles(t, homePath)
	if len(cids) != 1 {
		t.Fatalf("want exactly 1 calibration candidate after a diverged close, got %d: %v", len(cids), cids)
	}

	var cand mem.Candidate
	if err := candidates.ReadCandidate(homePath, candidates.Memory, cids[0], &cand); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if cand.PromotionBasis != "" {
		t.Errorf("promotion_basis = %q, want empty", cand.PromotionBasis)
	}
	if cand.Derivation != mem.DerivationObserved {
		t.Errorf("derivation = %q, want %q", cand.Derivation, mem.DerivationObserved)
	}
	if !strings.Contains(cand.Object, workID) {
		t.Errorf("statement must name the work id %s: %q", workID, cand.Object)
	}
	if !strings.Contains(cand.Object, "priced_right") || !strings.Contains(cand.Object, "failed") {
		t.Errorf("statement must name the verdict and actual outcome: %q", cand.Object)
	}

	blobDir := filepath.Join(homePath, "observed", "evcard-calibration")
	blobEntries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("read blob dir %s: %v", blobDir, err)
	}
	if len(blobEntries) != 1 {
		t.Fatalf("want exactly 1 blob under observed/evcard-calibration/, got %d", len(blobEntries))
	}

	after, err := memStore.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed across a diverged calibration close: before=%s after=%s", before, after)
	}
}

// TestWorkCloseCLIVindicatedEVCardMintsNoCalibrationCandidate covers the vindicated arm: the
// SAME priced_right-verdict card, closed --outcome completed this time, matches what the card
// staked (priced_right + completed -> card_vindicated, never a divergence). Zero calibration
// candidates, no blob dir at all.
func TestWorkCloseCLIVindicatedEVCardMintsNoCalibrationCandidate(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	cardRef, _, err := routeev.Store(homePath, routeev.Card{
		SchemaVersion:   routeev.SchemaVersion,
		CapabilityClass: "judge-critique",
		Verdict:         routeev.VerdictPricedRight,
	})
	if err != nil {
		t.Fatalf("store ev card: %v", err)
	}

	runEVCardDeviationLifecycle(t, "priced_right deviation closes completed (vindicated)", cardRef, "completed")

	cids := calibrationCandidateFiles(t, homePath)
	if len(cids) != 0 {
		t.Fatalf("want 0 calibration candidates on a vindicated close, got %d: %v", len(cids), cids)
	}
	if _, err := os.Stat(filepath.Join(homePath, "observed", "evcard-calibration")); !os.IsNotExist(err) {
		t.Fatalf("want no observed/evcard-calibration/ dir on a vindicated close, stat err=%v", err)
	}
}

// TestWorkCloseCLIMissingEVCardBlobSkipsCalibrationWithoutFailingClose covers the fail-open
// mechanical note in the task: a route-deviation work whose card_ref is well-formed
// (sha256:<64 hex>) but was never actually stored (a missing/corrupt blob) must NOT fail the
// close -- the close proceeds (exit 0, envelope lands completed) and calibration is silently
// skipped: zero candidates, no blob dir. Calibration is exhaust, never a gate.
func TestWorkCloseCLIMissingEVCardBlobSkipsCalibrationWithoutFailingClose(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	// A well-formed content-id that was never written through routeev.Store -- the blob is
	// simply absent from observed/route-evcards/.
	missingRef := routeev.CardRef([]byte("never persisted ev card body"))

	workID := runEVCardDeviationLifecycle(t, "deviation cites a missing ev card blob", missingRef, "completed")

	showOut := captureStdout(t, func() { cmdWork([]string{"show", workID, "--json"}) })
	shown := firstJSONObject(t, showOut)
	var state string
	if err := json.Unmarshal(shown["state"], &state); err != nil || state != "completed" {
		t.Fatalf("close with a missing ev card blob must still land the work completed, got state=%q err=%v\n%s", state, err, showOut)
	}

	cids := calibrationCandidateFiles(t, homePath)
	if len(cids) != 0 {
		t.Fatalf("want 0 calibration candidates when the ev card blob is missing, got %d: %v", len(cids), cids)
	}
	if _, err := os.Stat(filepath.Join(homePath, "observed", "evcard-calibration")); !os.IsNotExist(err) {
		t.Fatalf("want no observed/evcard-calibration/ dir when the ev card blob is missing, stat err=%v", err)
	}
}

// TestWorkCloseCLINoCardRefSkipsCalibration covers the "nothing to calibrate" case: a
// RouteDeviation with NO card ref at all (--deviation-card never supplied) closes cleanly and
// mints zero calibration output -- there is no card to read, so observeEVCardCalibrationOnClose
// no-ops before ever touching the routeev store.
func TestWorkCloseCLINoCardRefSkipsCalibration(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	runEVCardDeviationLifecycle(t, "deviation with no card ref at all", "", "completed")

	cids := calibrationCandidateFiles(t, homePath)
	if len(cids) != 0 {
		t.Fatalf("want 0 calibration candidates when the deviation carries no card_ref, got %d: %v", len(cids), cids)
	}
	if _, err := os.Stat(filepath.Join(homePath, "observed", "evcard-calibration")); !os.IsNotExist(err) {
		t.Fatalf("want no observed/evcard-calibration/ dir when the deviation carries no card_ref, stat err=%v", err)
	}
}
