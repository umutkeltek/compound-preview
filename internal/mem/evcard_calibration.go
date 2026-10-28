package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/candidates"
	"compound/internal/routeev"
)

// evcardCalibrationDir is the rebuildable, content-addressed home for EV-card calibration
// exhaust: COMPOUND_HOME/observed/evcard-calibration. Same custody posture as work-surprise (see
// surprise.go, this door's sibling and pattern of record) -- deliberately OUTSIDE
// COMPOUND_HOME/ledger, so externalizing a diverged routeev.CalibrationResult never adds a ledger
// byte, which keeps EngineDoctor's ledger-immutability invariant intact by construction.
//
// FOLLOW-UP (not built here, by design): this directory has no size cap or GC, the same posture
// as its siblings -- a retention/GC pass is a documented follow-up, not this slice.
const evcardCalibrationDir = "observed/evcard-calibration"

// evcardCalibrationBlob is the exact on-disk, hashed shape: the calibration verdict-vs-outcome
// signal plus the work, receipt, and card it closed against. Folding work_id/receipt_id/card_ref
// into the hashed content (not just the bare CalibrationResult) is deliberate: two DIFFERENT
// works (or the same work pricing two different lanes) can diverge identically -- same verdict,
// same actual outcome, same reason -- and they must never collide on the same content-addressed
// cid.
type evcardCalibrationBlob struct {
	WorkID      string          `json:"work_id"`
	ReceiptID   string          `json:"receipt_id"`
	CardRef     string          `json:"card_ref"`
	Calibration json.RawMessage `json:"calibration"`
}

// ObserveEVCardCalibration externalizes a routeev.Calibrate result into a content-addressed,
// rebuildable blob and mints exactly ONE observed-class memory candidate carrying a plain-
// language statement that names the card's verdict and the actual close outcome, mirroring
// ObserveWorkSurprise's custody posture (surprise.go): it writes ONLY through two rebuildable
// doors -- the blob dir and the candidate inbox -- and NEVER a ledger append, so LedgerDigest
// stays byte-identical (EngineDoctor green by construction). The candidate can never auto-promote
// (PromotionBasis empty): calibration evidence accumulates here for governed adjudication, the
// same inbox work-surprise feeds.
//
// Deliberate difference from ObserveWorkSurprise: that door trusts its caller to have already
// checked SurpriseSignal.Diverged before calling. This door checks payload's Diverged bit
// ITSELF and mints NOTHING -- no blob, no candidate -- when it is false, because a vindicated or
// unpriceable card is not exhaust (spec §5: "calibration exhaust... mints ONE observed-class
// candidate" only on divergence) and this is the one place that guarantee can be enforced for
// every caller, present or future.
//
// Dedup is day-salted like CaptureExhaust/ObserveWorkSurprise: the identical (work, receipt,
// card, payload) tuple observed again the SAME UTC day skips the candidate (never resetting an
// in-flight adjudication verdict); a DIFFERENT day mints a fresh candidate id.
func (s *Store) ObserveEVCardCalibration(workID, receiptID, cardRef string, payload []byte, now time.Time) (cid, candidateID string, err error) {
	var cal routeev.CalibrationResult
	if err = json.Unmarshal(payload, &cal); err != nil {
		return "", "", fmt.Errorf("decode calibration payload: %w", err)
	}
	if !cal.Diverged {
		// Vindicated (or unpriceable, which is never diverged) is not exhaust -- mint nothing.
		return "", "", nil
	}

	blob, err := json.Marshal(evcardCalibrationBlob{WorkID: workID, ReceiptID: receiptID, CardRef: cardRef, Calibration: json.RawMessage(payload)})
	if err != nil {
		return "", "", err
	}
	cid = ToolOutputCID(blob) // shares the sha256:<hex> content-id shape with tool-output/capture/surprise

	dir := filepath.Join(s.home, evcardCalibrationDir)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(filepath.Join(dir, cid), blob, 0o644); err != nil {
		return "", "", err
	}

	candidateID = EVCardCalibrationCandidateID(cid, now)
	var existing Candidate
	if rerr := candidates.ReadCandidate(s.home, candidates.Memory, candidateID, &existing); rerr == nil {
		return cid, candidateID, nil // skip-if-exists: never reset an adjudication verdict
	} else if !os.IsNotExist(rerr) {
		return "", "", rerr
	}

	c := Candidate{
		ID:             candidateID,
		Entity:         workID,
		Predicate:      "evcard_calibration_diverged",
		Object:         evcardCalibrationStatement(workID, cal),
		Derivation:     DerivationObserved,
		ProvenanceLane: "evcard-calibration",
		SourceTraceRef: "evcard-calibration:" + cid,
		PromotionBasis: "", // no basis -- an observed calibration-divergence pointer can never auto-promote
		State:          "proposed",
		SnippetHash:    shortHash(string(blob)),
		CreatedAt:      rfc3339(now),
		EvidenceRefs:   []string{"work:" + workID, "receipt:" + receiptID, "card:" + cardRef},
	}
	if err = candidates.WriteCandidate(s.home, candidates.Memory, candidateID, &c); err != nil {
		return "", "", err
	}
	return cid, candidateID, nil
}

// EVCardCalibrationCandidateID returns the day-salted candidate id ObserveEVCardCalibration mints
// (or skips) for a given blob cid and observation time -- exported so a caller/test that needs to
// know proposed-vs-skipped-existing can check candidate existence without re-deriving the
// day-salt formula (mirrors WorkSurpriseCandidateID).
func EVCardCalibrationCandidateID(cid string, now time.Time) string {
	return idFor("cand_", "evcard-calibration", cid+"|"+now.UTC().Format("2006-01-02"))
}

// evcardCalibrationStatement renders the plain-language divergence statement naming both the
// card's verdict and the actual close outcome, so the candidate is legible in the adjudication
// inbox without decoding the blob.
func evcardCalibrationStatement(workID string, cal routeev.CalibrationResult) string {
	return fmt.Sprintf(
		"Work %s's EV card verdict %s diverged from its actual close outcome %s (%s).",
		workID, cal.Verdict, cal.ActualOutcome, cal.Reason,
	)
}
