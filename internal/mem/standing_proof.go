package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/candidates"
)

// standingProofDir is the rebuildable, content-addressed home for standing-proof graduation
// exhaust: COMPOUND_HOME/observed/standing-proof. Same custody posture as its close-exhaust
// siblings (capture.go, tool_output.go, surprise.go) -- deliberately OUTSIDE COMPOUND_HOME/ledger,
// so externalizing a graduation offer never adds a ledger byte, which keeps EngineDoctor's
// ledger-immutability invariant intact by construction.
//
// FOLLOW-UP (not built here, by design): this directory has no size cap or GC, the same posture
// as its siblings -- a retention/GC pass is a documented follow-up, not this slice.
const standingProofDir = "observed/standing-proof"

// standingProofBlob is the exact on-disk, hashed shape: the machine-checkable proof command
// verbatim, plus the work it closed and the receipt that recorded that close. Folding work_id and
// receipt_id into the hashed content (not just the bare command) is deliberate -- two different
// works can close on an identical proof command, and they must never collide on one cid.
type standingProofBlob struct {
	WorkID    string `json:"work_id"`
	ReceiptID string `json:"receipt_id"`
	Command   string `json:"command"`
	Statement string `json:"statement"`
}

// OfferStandingProofGraduation externalizes a successful close's machine-checkable success
// predicate / re-runnable proof command into a content-addressed, rebuildable blob and mints
// exactly ONE observed-class memory candidate offering to graduate that command into a STANDING
// re-verified proof (a job-roster entry or a proof-check gate) via adjudication. It attacks the
// "landed != working" failure class: today a close is point-in-time evidence and nothing re-runs
// the proof afterward; this offer is the door to a standing re-verification, but only adjudication
// (never this automated caller) may walk through it.
//
// It writes ONLY through two rebuildable doors -- the blob dir and the candidate inbox -- and
// NEVER a ledger append, so LedgerDigest stays byte-identical (EngineDoctor green by
// construction). The candidate can never auto-promote (PromotionBasis empty): a graduation offer
// accumulates here for governed adjudication; it never writes memory or a standing proof on its
// own.
//
// Dedup is keyed on work_id + command, NOT day-salted (the deliberate deviation from
// ObserveWorkSurprise/CaptureExhaust): re-closing the same work -- even on a later day -- offers to
// graduate the SAME proof, so a repeat close must be idempotent and never mint a second candidate
// (never resetting an in-flight adjudication verdict). A genuinely different proof command on the
// same work is a different offer and mints its own candidate.
func (s *Store) OfferStandingProofGraduation(workID, receiptID, command string, now time.Time) (cid, candidateID string, err error) {
	statement := standingProofStatement(workID, command)
	blob, err := json.Marshal(standingProofBlob{WorkID: workID, ReceiptID: receiptID, Command: command, Statement: statement})
	if err != nil {
		return "", "", err
	}
	cid = ToolOutputCID(blob) // shares the sha256:<hex> content-id shape with tool-output/capture/surprise

	dir := filepath.Join(s.home, standingProofDir)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(filepath.Join(dir, cid), blob, 0o644); err != nil {
		return "", "", err
	}

	candidateID = StandingProofCandidateID(workID, command)
	var existing Candidate
	if rerr := candidates.ReadCandidate(s.home, candidates.Memory, candidateID, &existing); rerr == nil {
		return cid, candidateID, nil // skip-if-exists: an idempotent re-close never mints a duplicate
	} else if !os.IsNotExist(rerr) {
		return "", "", rerr
	}

	c := Candidate{
		ID:             candidateID,
		Entity:         workID,
		Predicate:      "standing_proof_offered",
		Object:         statement,
		Derivation:     DerivationObserved,
		ProvenanceLane: "work-close",
		SourceTraceRef: "standing-proof:" + cid,
		PromotionBasis: "", // no basis -- a graduation offer can never auto-promote
		State:          "proposed",
		SnippetHash:    shortHash(command),
		CreatedAt:      rfc3339(now),
		EvidenceRefs:   []string{"work:" + workID, "receipt:" + receiptID},
	}
	if err = candidates.WriteCandidate(s.home, candidates.Memory, candidateID, &c); err != nil {
		return "", "", err
	}
	return cid, candidateID, nil
}

// StandingProofCandidateID returns the candidate id OfferStandingProofGraduation mints (or skips)
// for a given work and proof command. Keyed on work_id + command (NOT day-salted): a re-close of
// the same work offering the same proof resolves to the same id, so the skip-if-exists path makes
// re-close idempotent. Exported so a caller/test can check proposed-vs-skipped-existing without
// re-deriving the key (mirrors CaptureCandidateID / WorkSurpriseCandidateID).
func StandingProofCandidateID(workID, command string) string {
	return idFor("cand_", "standing-proof", workID+"|"+command)
}

// standingProofStatement renders the plain-language offer, naming the work, the verbatim proof
// command, and what promotion means, so the candidate is legible in the adjudication inbox without
// decoding the blob.
func standingProofStatement(workID, command string) string {
	return fmt.Sprintf(
		"Work %s closed completed carrying machine-checkable proof command %q; promote to graduate it into a standing re-verified proof (job-roster entry or proof-check gate) via adjudication.",
		workID, command,
	)
}
