package mem

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
)

// captureDir is the rebuildable, content-addressed home for gesture-fired capture-exhaust
// bodies: COMPOUND_HOME/observed/capture. Deliberately outside COMPOUND_HOME/ledger for the same
// reason as observed/tool-output — externalizing a captured body must never add a ledger byte,
// which keeps EngineDoctor's ledger-immutability invariant intact by construction.
//
// Governing ruling: claim_933958b8cad4 (compound.conversation_capture_posture) — capture is
// GESTURE-TRIGGERED only, never ambient; the body is custodied here, never the ledger; and the
// candidate this mints is observed-class and can NEVER auto-promote on its own.
//
// FOLLOW-UP (not built here, by design): this directory has no size cap or GC, the same posture
// as observed/tool-output — a retention/GC pass is a documented follow-up, not this slice.
const captureDir = "observed/capture"

// CaptureExhaust externalizes a gesture-fired text body (e.g. a phone-voice capture) into a
// content-addressed, rebuildable blob and mints exactly ONE observed-class memory candidate
// carrying only a compact pointer. It writes ONLY through two rebuildable doors — the blob dir
// and the candidate inbox — and NEVER a ledger append, so LedgerDigest stays byte-identical
// (EngineDoctor green by construction). The body never becomes a Claim, so it can never enter
// ambient recall or resolve as an index_hit; it is retrievable only via ReadCapture /
// `compound capture show`. PromotionBasis is always empty: an observed capture pointer can never
// auto-promote — promotion stays a governed, separately-evidenced adjudication act.
//
// Dedup is content-addressed on the body AND day-salted on the UTC calendar day: cid is stable
// for a given body (same body -> same cid -> same blob path, rewritten idempotently), but the
// candidate id additionally folds in the day, so re-capturing an identical body the SAME day
// SKIPS the candidate (never resetting an in-flight adjudication verdict — the same skip-if-exists
// contract as CaptureToolOutput), while the same body captured again on a DIFFERENT day mints a
// NEW candidate: a repeated gesture on a new day is new exhaust, not yesterday's duplicate.
func (s *Store) CaptureExhaust(source, workID, body string, now time.Time) (cid, pointer string, err error) {
	raw := []byte(body)
	cid = ToolOutputCID(raw) // shares the sha256:<hex> content-id shape with tool-output

	dir := filepath.Join(s.home, captureDir)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(filepath.Join(dir, cid), raw, 0o644); err != nil {
		return "", "", err
	}
	pointer = fmt.Sprintf("capture:%s source=%s bytes=%d", cid, source, len(raw))

	id := CaptureCandidateID(cid, now)
	var existing Candidate
	if rerr := candidates.ReadCandidate(s.home, candidates.Memory, id, &existing); rerr == nil {
		return cid, pointer, nil // skip-if-exists: never reset an adjudication verdict
	} else if !os.IsNotExist(rerr) {
		return "", "", rerr
	}

	head := raw
	if len(head) > toolOutputSnippetHeadBytes {
		head = head[:toolOutputSnippetHeadBytes]
	}
	c := Candidate{
		ID:             id,
		Entity:         source,
		Predicate:      "captured_exhaust",
		Object:         cid,
		Derivation:     DerivationObserved,
		ProvenanceLane: source,
		SourceTraceRef: "capture:" + cid,
		PromotionBasis: "", // no basis — an observed capture pointer can never auto-promote
		State:          "proposed",
		SnippetHash:    shortHash(string(head)),
		CreatedAt:      rfc3339(now),
	}
	if strings.TrimSpace(workID) != "" {
		c.EvidenceRefs = []string{"work:" + workID}
	}
	if err = candidates.WriteCandidate(s.home, candidates.Memory, id, &c); err != nil {
		return "", "", err
	}
	return cid, pointer, nil
}

// CaptureCandidateID returns the day-salted candidate id CaptureExhaust mints (or skips) for a
// given cid and capture time. Exported so callers that need to know proposed-vs-skipped-existing
// (the CLI's success line) can check candidate existence around the CaptureExhaust call without
// re-deriving the day-salt formula.
func CaptureCandidateID(cid string, now time.Time) string {
	return idFor("cand_", "capture", cid+"|"+now.UTC().Format("2006-01-02"))
}

// ReadCapture returns the exact original bytes for a captured cid. A missing cid is an EXPLICIT
// error (never a silent nil): the content-addressed body either rebuilds byte-identically or the
// caller is told it is gone. The cid is validated to the strict "sha256:<64 hex>" shape before any
// read, so a path-shaped value can never escape the blob dir.
func (s *Store) ReadCapture(cid string) ([]byte, error) {
	if err := validSHA256CID(cid); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.home, captureDir, cid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("capture %s not found (rebuildable blob missing)", cid)
		}
		return nil, err
	}
	return b, nil
}
