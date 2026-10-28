package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

// workSurpriseDir is the rebuildable, content-addressed home for close-time prediction-error
// exhaust: COMPOUND_HOME/observed/work-surprise. Same custody posture as capture/tool-output
// (siblings, see capture.go / tool_output.go) -- deliberately OUTSIDE COMPOUND_HOME/ledger, so
// externalizing a Receipt.Surprise never adds a ledger byte, which keeps EngineDoctor's
// ledger-immutability invariant intact by construction.
//
// FOLLOW-UP (not built here, by design): this directory has no size cap or GC, the same posture
// as its siblings -- a retention/GC pass is a documented follow-up, not this slice.
const workSurpriseDir = "observed/work-surprise"

// surpriseBlob is the exact on-disk, hashed shape: the staked-vs-actual signal plus the work and
// receipt it closed. Folding work_id/receipt_id into the hashed content (not just the bare
// signal) is deliberate: two DIFFERENT works can stake and diverge identically (same predicted
// and actual tuples), and they must never collide on the same content-addressed cid.
type surpriseBlob struct {
	WorkID    string          `json:"work_id"`
	ReceiptID string          `json:"receipt_id"`
	Surprise  json.RawMessage `json:"surprise"`
}

// ObserveWorkSurprise externalizes a close-time Receipt.Surprise (predicted vs. actual close, see
// envelope.SurpriseSignal) into a content-addressed, rebuildable blob and mints exactly ONE
// observed-class memory candidate carrying a plain-language statement that names both the
// predicted and actual tuples. It writes ONLY through two rebuildable doors -- the blob dir and
// the candidate inbox -- and NEVER a ledger append, so LedgerDigest stays byte-identical
// (EngineDoctor green by construction). The candidate can never auto-promote (PromotionBasis
// empty): prediction-error evidence accumulates here for governed adjudication, the
// ACE/injection-observation weekly compile, and the canonized value-function Revisit trigger --
// it never writes memory on its own.
//
// Dedup is day-salted like CaptureExhaust: the identical (work, receipt, payload) triple observed
// again the SAME UTC day skips the candidate (never resetting an in-flight adjudication verdict);
// a DIFFERENT day mints a fresh candidate id. Because the blob's content address folds in
// work_id and receipt_id, two different works can never collide on the same cid even when their
// staked-vs-actual tuples are byte-identical -- a caller must call this once per diverged close,
// keyed on the work it closed.
func (s *Store) ObserveWorkSurprise(workID, receiptID string, payload []byte, now time.Time) (cid, candidateID string, err error) {
	var sig envelope.SurpriseSignal
	if err = json.Unmarshal(payload, &sig); err != nil {
		return "", "", fmt.Errorf("decode surprise payload: %w", err)
	}
	blob, err := json.Marshal(surpriseBlob{WorkID: workID, ReceiptID: receiptID, Surprise: json.RawMessage(payload)})
	if err != nil {
		return "", "", err
	}
	cid = ToolOutputCID(blob) // shares the sha256:<hex> content-id shape with tool-output/capture

	dir := filepath.Join(s.home, workSurpriseDir)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(filepath.Join(dir, cid), blob, 0o644); err != nil {
		return "", "", err
	}

	candidateID = WorkSurpriseCandidateID(cid, now)
	var existing Candidate
	if rerr := candidates.ReadCandidate(s.home, candidates.Memory, candidateID, &existing); rerr == nil {
		return cid, candidateID, nil // skip-if-exists: never reset an adjudication verdict
	} else if !os.IsNotExist(rerr) {
		return "", "", rerr
	}

	c := Candidate{
		ID:             candidateID,
		Entity:         workID,
		Predicate:      "work_surprise_diverged",
		Object:         workSurpriseStatement(workID, sig),
		Derivation:     DerivationObserved,
		ProvenanceLane: "work-close",
		SourceTraceRef: "work-surprise:" + cid,
		PromotionBasis: "", // no basis -- an observed prediction-error pointer can never auto-promote
		State:          "proposed",
		SnippetHash:    shortHash(string(blob)),
		CreatedAt:      rfc3339(now),
		EvidenceRefs:   []string{"work:" + workID, "receipt:" + receiptID},
	}
	if err = candidates.WriteCandidate(s.home, candidates.Memory, candidateID, &c); err != nil {
		return "", "", err
	}
	return cid, candidateID, nil
}

// WorkSurpriseCandidateID returns the day-salted candidate id ObserveWorkSurprise mints (or
// skips) for a given blob cid and observation time -- exported so a caller/test that needs to
// know proposed-vs-skipped-existing can check candidate existence without re-deriving the
// day-salt formula (mirrors CaptureCandidateID).
func WorkSurpriseCandidateID(cid string, now time.Time) string {
	return idFor("cand_", "work-surprise", cid+"|"+now.UTC().Format("2006-01-02"))
}

// workSurpriseStatement renders the plain-language divergence statement naming both the staked
// prediction and the actual close, so the candidate is legible in the adjudication inbox without
// decoding the blob.
func workSurpriseStatement(workID string, sig envelope.SurpriseSignal) string {
	predicted := sig.PredictedOutcome
	if sig.PredictedMoveType != "" {
		predicted += "/" + sig.PredictedMoveType
	}
	actual := sig.ActualOutcome
	if sig.ActualMoveType != "" {
		actual += "/" + sig.ActualMoveType
	}
	return fmt.Sprintf("Work %s closed diverged from its staked prediction: predicted %s, actual %s.", workID, predicted, actual)
}
