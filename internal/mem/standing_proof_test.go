package mem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

func standingProofCandidateCount(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "inbox", "memory-candidates"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read inbox: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestOfferStandingProofGraduationMintsOneProposedNeverLedger proves the standing-proof graduation
// door mints EXACTLY ONE observed-class PROPOSED candidate carrying the work_id, the verbatim proof
// command, the close receipt ref, and a legible graduation statement -- and adds ZERO ledger bytes
// (the offer accumulates for adjudication; it never writes memory or a standing proof itself).
func TestOfferStandingProofGraduationMintsOneProposedNeverLedger(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	workID := "work_50662769fd78"
	receiptID := "rec_abc123def456"
	command := "go test ./internal/work/..."

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	cid, candidateID, err := s.OfferStandingProofGraduation(workID, receiptID, command, now)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if candidateID != StandingProofCandidateID(workID, command) {
		t.Fatalf("candidate id = %q, want %q", candidateID, StandingProofCandidateID(workID, command))
	}

	// Exactly one candidate exists.
	if n := standingProofCandidateCount(t, home); n != 1 {
		t.Fatalf("candidate count = %d, want exactly 1", n)
	}

	// Blob lives under observed/standing-proof/, NEVER under ledger/.
	blob := filepath.Join(home, "observed", "standing-proof", cid)
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("blob not written at %s: %v", blob, err)
	}
	if strings.HasPrefix(blob, filepath.Join(home, "ledger")+string(filepath.Separator)) {
		t.Fatalf("blob path must not live under ledger/: %s", blob)
	}

	// Ledger byte-identical: the offer added ZERO ledger bytes.
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed: before=%s after=%s", before, after)
	}

	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, candidateID, &c); err != nil {
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
	if c.Entity != workID {
		t.Errorf("entity = %q, want %q", c.Entity, workID)
	}
	if c.Predicate != "standing_proof_offered" {
		t.Errorf("predicate = %q, want standing_proof_offered", c.Predicate)
	}
	// The candidate body carries the verbatim proof command...
	if !strings.Contains(c.Object, command) {
		t.Errorf("object must carry the verbatim command %q, got %q", command, c.Object)
	}
	// ...and the graduation statement makes promotion's meaning legible.
	if !strings.Contains(c.Object, "standing re-verified proof") || !strings.Contains(c.Object, "adjudication") {
		t.Errorf("object must state that promotion graduates a standing re-verified proof via adjudication, got %q", c.Object)
	}
	// ...and both the work and receipt refs are attached as evidence.
	if len(c.EvidenceRefs) != 2 || c.EvidenceRefs[0] != "work:"+workID || c.EvidenceRefs[1] != "receipt:"+receiptID {
		t.Errorf("evidence_refs = %v, want [work:%s receipt:%s]", c.EvidenceRefs, workID, receiptID)
	}
	if c.SourceTraceRef != "standing-proof:"+cid {
		t.Errorf("source_trace_ref = %q, want standing-proof:%s", c.SourceTraceRef, cid)
	}
	if c.SnippetHash == "" {
		t.Error("snippet_hash must be set")
	}

	// The offer never becomes a Claim: not ambient, never an index_hit.
	cur, err := s.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	if len(cur) != 0 {
		t.Fatalf("offer must not create claims; got %d current claims", len(cur))
	}
}

// TestOfferStandingProofGraduationDedupOnReclose proves an idempotent re-close of the SAME work
// offering the SAME proof command never mints a second candidate and never resets an in-flight
// adjudication verdict -- even across day boundaries (the deliberate contrast with the day-salted
// capture/surprise doors). A genuinely different proof command on the same work is a distinct
// offer and mints its own candidate.
func TestOfferStandingProofGraduationDedupOnReclose(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	workID := "work_dedup01"
	command := "compound proof-check --ci"
	day1 := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC) // a LATER day -- must still dedup

	_, id1, err := s.OfferStandingProofGraduation(workID, "rec_first", command, day1)
	if err != nil {
		t.Fatalf("first offer: %v", err)
	}

	// Simulate an in-flight adjudication verdict, then re-close the same work on a later day.
	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id1, &c); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	c.State = "parked"
	c.Reason = "operator kept aside pending review"
	if err := candidates.WriteCandidate(home, candidates.Memory, id1, &c); err != nil {
		t.Fatalf("stamp verdict: %v", err)
	}

	_, id2, err := s.OfferStandingProofGraduation(workID, "rec_second", command, day2)
	if err != nil {
		t.Fatalf("re-close offer: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("re-close minted a NEW candidate id %q, want dedup to %q", id2, id1)
	}
	if n := standingProofCandidateCount(t, home); n != 1 {
		t.Fatalf("candidate count after re-close = %d, want exactly 1 (no duplicate)", n)
	}
	// The in-flight verdict is untouched -- the re-close never reset it.
	if err := candidates.ReadCandidate(home, candidates.Memory, id1, &c); err != nil {
		t.Fatalf("re-read candidate: %v", err)
	}
	if c.State != "parked" || c.Reason != "operator kept aside pending review" {
		t.Fatalf("re-close reset the adjudication verdict: state=%q reason=%q", c.State, c.Reason)
	}

	// A different proof command on the same work is a different offer -> a second candidate.
	_, id3, err := s.OfferStandingProofGraduation(workID, "rec_third", "bun test", day2)
	if err != nil {
		t.Fatalf("distinct-command offer: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("a different command must mint a distinct candidate, got %q == %q", id3, id1)
	}
	if n := standingProofCandidateCount(t, home); n != 2 {
		t.Fatalf("candidate count after distinct command = %d, want 2", n)
	}
}
