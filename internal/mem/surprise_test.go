package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

func marshalSurprise(t *testing.T, sig envelope.SurpriseSignal) []byte {
	t.Helper()
	b, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("marshal surprise signal: %v", err)
	}
	return b
}

// TestObserveWorkSurpriseRoundTripObservedNeverLedger proves the custody + candidate-shape +
// ledger-immutability contract for close-time prediction-error exhaust (sibling of
// TestCaptureExhaustRoundTripObservedNeverLedger, claim_933958b8cad4's machine-exhaust posture): a
// diverged Receipt.Surprise is content-addressed into a rebuildable blob (never the ledger) and
// mints exactly ONE observed-class candidate carrying a plain-language statement naming both the
// predicted and actual tuples.
func TestObserveWorkSurpriseRoundTripObservedNeverLedger(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	sig := envelope.SurpriseSignal{
		PredictedOutcome:  "completed",
		PredictedMoveType: "rule_delta",
		ActualOutcome:     "completed",
		ActualMoveType:    "control_plane_repair",
		Diverged:          true,
	}
	payload := marshalSurprise(t, sig)

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	cid, candID, err := s.ObserveWorkSurprise("work_prediction_demo", "rec_prediction_demo", payload, now)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if cid == "" || candID == "" {
		t.Fatalf("cid/candidate id must be non-empty: cid=%q candID=%q", cid, candID)
	}

	// Blob lives under observed/work-surprise/, NEVER under ledger/.
	blob := filepath.Join(home, "observed", "work-surprise", cid)
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("blob not written at %s: %v", blob, err)
	}
	if strings.HasPrefix(blob, filepath.Join(home, "ledger")+string(filepath.Separator)) {
		t.Fatalf("blob path must not live under ledger/: %s", blob)
	}

	// Ledger byte-identical: the exhaust added ZERO ledger bytes (memory-doctor invariant).
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed: before=%s after=%s", before, after)
	}

	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID, &c); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if c.Derivation != DerivationObserved {
		t.Errorf("derivation = %q, want %q", c.Derivation, DerivationObserved)
	}
	if c.State != "proposed" {
		t.Errorf("state = %q, want proposed", c.State)
	}
	if c.PromotionBasis != "" {
		t.Errorf("promotion_basis = %q, want empty (never auto-promotes)", c.PromotionBasis)
	}
	if c.SourceTraceRef != "work-surprise:"+cid {
		t.Errorf("source_trace_ref = %q, want work-surprise:%s", c.SourceTraceRef, cid)
	}
	if len(c.EvidenceRefs) != 2 || c.EvidenceRefs[0] != "work:work_prediction_demo" || c.EvidenceRefs[1] != "receipt:rec_prediction_demo" {
		t.Errorf("evidence_refs = %v, want [work:work_prediction_demo receipt:rec_prediction_demo]", c.EvidenceRefs)
	}
	// The statement names BOTH the predicted and actual tuples in plain language.
	if !strings.Contains(c.Object, "work_prediction_demo") {
		t.Errorf("statement must name the work id: %q", c.Object)
	}
	if !strings.Contains(c.Object, "completed/rule_delta") {
		t.Errorf("statement must name the predicted tuple: %q", c.Object)
	}
	if !strings.Contains(c.Object, "completed/control_plane_repair") {
		t.Errorf("statement must name the actual tuple: %q", c.Object)
	}

	// Never ambient: no Claim was ever written, so this can never resolve as an index_hit.
	cur, err := s.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	if len(cur) != 0 {
		t.Fatalf("observe must not create claims; got %d current claims", len(cur))
	}
}

// TestObserveWorkSurpriseDaySaltedDedupAcrossWorks proves F5's "cids differ" requirement: the
// identical SurpriseSignal payload observed for a SECOND, DIFFERENT work on the SAME day mints a
// DIFFERENT blob and candidate (the content address folds in work_id/receipt_id, so two different
// works never collide on the same cid even with byte-identical predicted/actual tuples); the
// SAME work+receipt+signal re-observed the SAME day skips the candidate (never resets an
// in-flight adjudication verdict), while a DIFFERENT day mints a fresh one.
func TestObserveWorkSurpriseDaySaltedDedupAcrossWorks(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sig := envelope.SurpriseSignal{PredictedOutcome: "completed", ActualOutcome: "failed", Diverged: true}
	payload := marshalSurprise(t, sig)
	day1 := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	day1Later := time.Date(2026, 7, 15, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 16, 0, 5, 0, 0, time.UTC)

	cid1, candID1, err := s.ObserveWorkSurprise("work_alpha", "rec_alpha", payload, day1)
	if err != nil {
		t.Fatalf("observe work_alpha: %v", err)
	}

	// Set a pre-existing adjudication verdict, then re-observe the identical (work, receipt,
	// payload) triple later the SAME day: must skip, never resetting the verdict.
	var c1 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID1, &c1); err != nil {
		t.Fatalf("read candidate 1: %v", err)
	}
	c1.State = "parked"
	if err := candidates.WriteCandidate(home, candidates.Memory, candID1, &c1); err != nil {
		t.Fatalf("rewrite candidate 1: %v", err)
	}
	cid1Again, candID1Again, err := s.ObserveWorkSurprise("work_alpha", "rec_alpha", payload, day1Later)
	if err != nil {
		t.Fatalf("same-day re-observe: %v", err)
	}
	if cid1Again != cid1 || candID1Again != candID1 {
		t.Fatalf("same-day re-observe of the identical triple must be stable: cid %q->%q candidate %q->%q", cid1, cid1Again, candID1, candID1Again)
	}
	var c1After Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID1, &c1After); err != nil {
		t.Fatalf("read candidate 1 after re-observe: %v", err)
	}
	if c1After.State != "parked" {
		t.Fatalf("same-day re-observe reset adjudication verdict: state = %q, want parked (skip-if-exists)", c1After.State)
	}

	// A DIFFERENT work, same signal, same day: DIFFERENT cid (content address folds in work_id)
	// and DIFFERENT candidate id -- two works must never collide/dedup against each other.
	cid2, candID2, err := s.ObserveWorkSurprise("work_beta", "rec_beta", payload, day1)
	if err != nil {
		t.Fatalf("observe work_beta: %v", err)
	}
	if cid2 == cid1 {
		t.Fatalf("a different work must not collide on the same content-addressed cid: %q", cid2)
	}
	if candID2 == candID1 {
		t.Fatalf("a different work must mint a different candidate id: %q", candID2)
	}

	// The SAME work+receipt+signal on a DIFFERENT day mints a fresh candidate (new day, new
	// evidence, never a duplicate of yesterday's).
	cid3, candID3, err := s.ObserveWorkSurprise("work_alpha", "rec_alpha", payload, day2)
	if err != nil {
		t.Fatalf("next-day observe: %v", err)
	}
	if cid3 != cid1 {
		t.Fatalf("cid is content-addressed on (work, receipt, payload) alone, stable across days: got %q, want %q", cid3, cid1)
	}
	if candID3 == candID1 {
		t.Fatalf("candidate id must differ across a day boundary, got %q for both", candID1)
	}
	var c3 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID3, &c3); err != nil {
		t.Fatalf("read candidate 3: %v", err)
	}
	if c3.State != "proposed" {
		t.Fatalf("next-day candidate state = %q, want proposed (a fresh mint)", c3.State)
	}
}
