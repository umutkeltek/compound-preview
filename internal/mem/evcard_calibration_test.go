package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/routeev"
)

func marshalCalibration(t *testing.T, cal routeev.CalibrationResult) []byte {
	t.Helper()
	b, err := json.Marshal(cal)
	if err != nil {
		t.Fatalf("marshal calibration result: %v", err)
	}
	return b
}

// TestObserveEVCardCalibrationRoundTripObservedNeverLedger proves the custody + candidate-shape +
// ledger-immutability contract for calibration exhaust (sibling of
// TestObserveWorkSurpriseRoundTripObservedNeverLedger): a diverged routeev.CalibrationResult is
// content-addressed into a rebuildable blob (never the ledger) and mints exactly ONE
// observed-class candidate carrying a plain-language statement naming both the verdict and the
// actual close outcome.
func TestObserveEVCardCalibrationRoundTripObservedNeverLedger(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	cal := routeev.Calibrate(routeev.Card{Verdict: routeev.VerdictPricedRight}, routeev.ActualOutcomeFailed)
	if !cal.Diverged {
		t.Fatalf("fixture must be a diverged calibration, got %+v", cal)
	}
	payload := marshalCalibration(t, cal)

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	cid, candID, err := s.ObserveEVCardCalibration("work_evcard_demo", "rec_evcard_demo", "card_evcard_demo", payload, now)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if cid == "" || candID == "" {
		t.Fatalf("cid/candidate id must be non-empty on a diverged observation: cid=%q candID=%q", cid, candID)
	}

	// Blob lives under observed/evcard-calibration/, NEVER under ledger/.
	blob := filepath.Join(home, "observed", "evcard-calibration", cid)
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
	if c.SourceTraceRef != "evcard-calibration:"+cid {
		t.Errorf("source_trace_ref = %q, want evcard-calibration:%s", c.SourceTraceRef, cid)
	}
	wantRefs := []string{"work:work_evcard_demo", "receipt:rec_evcard_demo", "card:card_evcard_demo"}
	if len(c.EvidenceRefs) != len(wantRefs) {
		t.Fatalf("evidence_refs = %v, want %v", c.EvidenceRefs, wantRefs)
	}
	for i, want := range wantRefs {
		if c.EvidenceRefs[i] != want {
			t.Errorf("evidence_refs[%d] = %q, want %q", i, c.EvidenceRefs[i], want)
		}
	}
	// The statement names both the verdict and the actual outcome in plain language.
	if !strings.Contains(c.Object, "work_evcard_demo") {
		t.Errorf("statement must name the work id: %q", c.Object)
	}
	if !strings.Contains(c.Object, routeev.VerdictPricedRight) {
		t.Errorf("statement must name the verdict: %q", c.Object)
	}
	if !strings.Contains(c.Object, routeev.ActualOutcomeFailed) {
		t.Errorf("statement must name the actual outcome: %q", c.Object)
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

// TestObserveEVCardCalibrationNonDivergedMintsNothing proves a vindicated card is not exhaust: no
// blob, no candidate, and the ledger stays untouched, for every non-diverged cell of the
// divergence table (priced_right+completed, overpaid+failed, dominated+cancelled) AND for
// unpriceable, which can never diverge regardless of outcome.
func TestObserveEVCardCalibrationNonDivergedMintsNothing(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		outcome string
	}{
		{"priced_right+completed", routeev.VerdictPricedRight, routeev.ActualOutcomeCompleted},
		{"overpaid+failed", routeev.VerdictOverpaid, routeev.ActualOutcomeFailed},
		{"overpaid+cancelled", routeev.VerdictOverpaid, routeev.ActualOutcomeCancelled},
		{"dominated+failed", routeev.VerdictDominated, routeev.ActualOutcomeFailed},
		{"dominated+cancelled", routeev.VerdictDominated, routeev.ActualOutcomeCancelled},
		{"unpriceable+completed", routeev.VerdictUnpriceable, routeev.ActualOutcomeCompleted},
		{"unpriceable+failed", routeev.VerdictUnpriceable, routeev.ActualOutcomeFailed},
		{"unpriceable+cancelled", routeev.VerdictUnpriceable, routeev.ActualOutcomeCancelled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			s, err := Open(home)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
			cal := routeev.Calibrate(routeev.Card{Verdict: tc.verdict}, tc.outcome)
			if cal.Diverged {
				t.Fatalf("fixture must be non-diverged, got %+v", cal)
			}
			payload := marshalCalibration(t, cal)

			before, err := s.LedgerDigest()
			if err != nil {
				t.Fatalf("ledger digest before: %v", err)
			}

			cid, candID, err := s.ObserveEVCardCalibration("work_vindicated", "rec_vindicated", "card_vindicated", payload, now)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if cid != "" || candID != "" {
				t.Fatalf("non-diverged observation must mint nothing: cid=%q candID=%q", cid, candID)
			}

			// No blob directory contents at all.
			dir := filepath.Join(home, "observed", "evcard-calibration")
			if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
				t.Fatalf("non-diverged observation wrote a blob: %v", entries)
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read blob dir: %v", err)
			}

			after, err := s.LedgerDigest()
			if err != nil {
				t.Fatalf("ledger digest after: %v", err)
			}
			if before != after {
				t.Fatalf("ledger digest changed on a non-diverged observation: before=%s after=%s", before, after)
			}
		})
	}
}

// TestObserveEVCardCalibrationDaySaltedDedupAcrossWorks proves the content address folds in
// work_id/receipt_id/card_ref, so two different works observing an IDENTICAL divergence (same
// verdict, same actual outcome, same reason) never collide on the same cid, while the SAME
// (work, receipt, card, payload) triple re-observed the same day skips the candidate (never
// resetting an in-flight adjudication verdict) and a different day mints a fresh one.
func TestObserveEVCardCalibrationDaySaltedDedupAcrossWorks(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cal := routeev.Calibrate(routeev.Card{Verdict: routeev.VerdictOverpaid}, routeev.ActualOutcomeCompleted)
	payload := marshalCalibration(t, cal)
	day1 := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	day1Later := time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 22, 0, 5, 0, 0, time.UTC)

	cid1, candID1, err := s.ObserveEVCardCalibration("work_alpha", "rec_alpha", "card_alpha", payload, day1)
	if err != nil {
		t.Fatalf("observe work_alpha: %v", err)
	}

	// Set a pre-existing adjudication verdict, then re-observe the identical (work, receipt,
	// card, payload) tuple later the SAME day: must skip, never resetting the verdict.
	var c1 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID1, &c1); err != nil {
		t.Fatalf("read candidate 1: %v", err)
	}
	c1.State = "parked"
	if err := candidates.WriteCandidate(home, candidates.Memory, candID1, &c1); err != nil {
		t.Fatalf("rewrite candidate 1: %v", err)
	}
	cid1Again, candID1Again, err := s.ObserveEVCardCalibration("work_alpha", "rec_alpha", "card_alpha", payload, day1Later)
	if err != nil {
		t.Fatalf("same-day re-observe: %v", err)
	}
	if cid1Again != cid1 || candID1Again != candID1 {
		t.Fatalf("same-day re-observe of the identical tuple must be stable: cid %q->%q candidate %q->%q", cid1, cid1Again, candID1, candID1Again)
	}
	var c1After Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candID1, &c1After); err != nil {
		t.Fatalf("read candidate 1 after re-observe: %v", err)
	}
	if c1After.State != "parked" {
		t.Fatalf("same-day re-observe reset adjudication verdict: state = %q, want parked (skip-if-exists)", c1After.State)
	}

	// A DIFFERENT work, byte-identical calibration payload, same day: DIFFERENT cid (content
	// address folds in work_id/receipt_id/card_ref) and DIFFERENT candidate id -- two works must
	// never collide/dedup against each other even on an identical divergence.
	cid2, candID2, err := s.ObserveEVCardCalibration("work_beta", "rec_beta", "card_beta", payload, day1)
	if err != nil {
		t.Fatalf("observe work_beta: %v", err)
	}
	if cid2 == cid1 {
		t.Fatalf("a different work must not collide on the same content-addressed cid: %q", cid2)
	}
	if candID2 == candID1 {
		t.Fatalf("a different work must mint a different candidate id: %q", candID2)
	}

	// The SAME work+receipt+card+signal on a DIFFERENT day mints a fresh candidate (new day, new
	// evidence, never a duplicate of yesterday's), with a stable cid (content address does not
	// depend on the observation clock).
	cid3, candID3, err := s.ObserveEVCardCalibration("work_alpha", "rec_alpha", "card_alpha", payload, day2)
	if err != nil {
		t.Fatalf("next-day observe: %v", err)
	}
	if cid3 != cid1 {
		t.Fatalf("cid is content-addressed on (work, receipt, card, payload) alone, stable across days: got %q, want %q", cid3, cid1)
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
