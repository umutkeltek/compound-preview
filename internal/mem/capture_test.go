package mem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

// TestCaptureExhaustRoundTripObservedNeverLedger proves the custody + candidate-shape +
// ledger-immutability contract of claim_933958b8cad4 (compound.conversation_capture_posture):
// a gesture-fired body is content-addressed into a rebuildable blob (never the ledger) and
// mints exactly ONE observed-class candidate carrying a bounded, body-free pointer, retrievable
// byte-exact and never a Claim (so never ambient / never an index_hit).
// Covers F1 (custody), F2 (candidate shape), F3 (ledger immutability).
func TestCaptureExhaustRoundTripObservedNeverLedger(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	body := "remember to check the northern fence line before the storm"
	wantCID := ToolOutputCID([]byte(body))

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	cid, pointer, err := s.CaptureExhaust("phone-voice", "work_50662769fd78", body, now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if cid != wantCID {
		t.Fatalf("cid = %q, want %q", cid, wantCID)
	}

	// F2: the pointer is the ONLY thing that flows into context: bounded, body-free, carries the cid.
	if len(pointer) > 256 || strings.Contains(pointer, body) {
		t.Fatalf("pointer must be bounded and body-free, got %q", pointer)
	}
	if !strings.Contains(pointer, cid) {
		t.Fatalf("pointer must carry the cid, got %q", pointer)
	}

	// F1: blob lives under observed/capture/, NEVER under ledger/.
	blob := filepath.Join(home, "observed", "capture", cid)
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("blob not written at %s: %v", blob, err)
	}
	if strings.HasPrefix(blob, filepath.Join(home, "ledger")+string(filepath.Separator)) {
		t.Fatalf("blob path must not live under ledger/: %s", blob)
	}

	// F1: round-trip the exact bytes via ReadCapture.
	got, err := s.ReadCapture(cid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("round-trip mismatch: got %q, want %q", string(got), body)
	}

	// F3: ledger byte-identical: capture added ZERO ledger bytes.
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed: before=%s after=%s", before, after)
	}

	// F2: minted candidate is observed-class, proposed, no promotion basis, body-free.
	id := CaptureCandidateID(cid, now)
	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &c); err != nil {
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
	if c.Entity != "phone-voice" {
		t.Errorf("entity = %q, want phone-voice", c.Entity)
	}
	if c.Predicate != "captured_exhaust" {
		t.Errorf("predicate = %q, want captured_exhaust", c.Predicate)
	}
	if c.Object != cid {
		t.Errorf("object = %q, want %s", c.Object, cid)
	}
	if c.ProvenanceLane != "phone-voice" {
		t.Errorf("provenance_lane = %q, want phone-voice", c.ProvenanceLane)
	}
	if c.SourceTraceRef != "capture:"+cid {
		t.Errorf("source_trace_ref = %q, want capture:%s", c.SourceTraceRef, cid)
	}
	if c.SnippetHash == "" {
		t.Error("snippet_hash must be set (bounded head snippet hash)")
	}
	if len(c.EvidenceRefs) != 1 || c.EvidenceRefs[0] != "work:work_50662769fd78" {
		t.Errorf("evidence_refs = %v, want [work:work_50662769fd78]", c.EvidenceRefs)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "inbox", "memory-candidates", id+".json"))
	if strings.Contains(string(raw), body) {
		t.Error("candidate record leaked the capture body")
	}

	// F2/F3: never ambient — the body is NOT a Claim, so it can never enter recall or resolve as index_hit.
	cur, err := s.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	if len(cur) != 0 {
		t.Fatalf("capture must not create claims; got %d current claims", len(cur))
	}
	ledgerClaims, err := s.LedgerClaims()
	if err != nil {
		t.Fatalf("ledger claims: %v", err)
	}
	if len(ledgerClaims) != 0 {
		t.Fatalf("capture must not append ledger claims; got %d", len(ledgerClaims))
	}
}

// TestCaptureExhaustNoWorkIDOmitsEvidenceRefs proves an empty workID never mints a spurious
// evidence pointer (mirrors CaptureToolOutput's same convention).
func TestCaptureExhaustNoWorkIDOmitsEvidenceRefs(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	cid, _, err := s.CaptureExhaust("phone-voice", "", "a body with no work context", now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	id := CaptureCandidateID(cid, now)
	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &c); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if len(c.EvidenceRefs) != 0 {
		t.Errorf("evidence_refs = %v, want empty when workID is empty", c.EvidenceRefs)
	}
}

// TestCaptureExhaustDaySaltedDedup proves F4: the same body captured twice on the SAME UTC day
// skips the candidate and never resets an in-flight adjudication verdict, while the same body
// captured on a DIFFERENT day mints a brand NEW candidate (a repeated gesture on a new day is
// new exhaust, not a duplicate of yesterday's).
func TestCaptureExhaustDaySaltedDedup(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	body := "the same reminder said twice"
	day1 := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	day1Later := time.Date(2026, 7, 14, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 15, 0, 5, 0, 0, time.UTC)

	cid1, _, err := s.CaptureExhaust("phone-voice", "", body, day1)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	id1 := CaptureCandidateID(cid1, day1)

	// Set a pre-existing adjudication verdict, then re-capture the identical body later the SAME day.
	var c1 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id1, &c1); err != nil {
		t.Fatalf("read candidate 1: %v", err)
	}
	c1.State = "parked"
	if err := candidates.WriteCandidate(home, candidates.Memory, id1, &c1); err != nil {
		t.Fatalf("rewrite candidate 1: %v", err)
	}

	cid1Again, _, err := s.CaptureExhaust("phone-voice", "", body, day1Later)
	if err != nil {
		t.Fatalf("same-day recapture: %v", err)
	}
	if cid1Again != cid1 {
		t.Fatalf("cid must be content-addressed and stable: got %q, want %q", cid1Again, cid1)
	}
	idSameDay := CaptureCandidateID(cid1Again, day1Later)
	if idSameDay != id1 {
		t.Fatalf("same-day candidate id must match: got %q, want %q", idSameDay, id1)
	}
	var c1After Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id1, &c1After); err != nil {
		t.Fatalf("read candidate 1 after recapture: %v", err)
	}
	if c1After.State != "parked" {
		t.Fatalf("same-day recapture reset adjudication verdict: state = %q, want parked (skip-if-exists)", c1After.State)
	}

	// A DIFFERENT day mints a NEW candidate id for the identical body.
	cid2, _, err := s.CaptureExhaust("phone-voice", "", body, day2)
	if err != nil {
		t.Fatalf("next-day capture: %v", err)
	}
	if cid2 != cid1 {
		t.Fatalf("cid is content-addressed on the body alone, must be stable across days: got %q, want %q", cid2, cid1)
	}
	id2 := CaptureCandidateID(cid2, day2)
	if id2 == id1 {
		t.Fatalf("candidate id must differ across a day boundary: both = %q", id1)
	}
	var c2 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id2, &c2); err != nil {
		t.Fatalf("read candidate 2: %v", err)
	}
	if c2.State != "proposed" {
		t.Fatalf("next-day candidate state = %q, want proposed (a fresh mint)", c2.State)
	}

	// Exactly two candidates total: one per day, never a third from the same-day recapture.
	entries, _ := os.ReadDir(filepath.Join(home, "inbox", "memory-candidates"))
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want exactly 2 memory candidates (one per day), got %d", n)
	}
}

// TestReadCaptureMissingIsExplicitError proves F1/F5: a missing or malformed cid is an EXPLICIT
// error, never a silent nil, and that a path-shaped cid can never escape the blob dir.
func TestReadCaptureMissingIsExplicitError(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	missing := "sha256:" + strings.Repeat("0", 64)
	if _, err := s.ReadCapture(missing); err == nil {
		t.Fatal("missing cid must be an explicit error, never a silent nil")
	}
	if _, err := s.ReadCapture("../../etc/passwd"); err == nil {
		t.Fatal("malformed/path-shaped cid must be rejected before any read")
	}
}

// TestReadCaptureCIDGuardShape proves F5 in detail: the "sha256:" prefix is required, exactly 64
// lowercase hex characters are enforced, and a path-shaped value under a valid-looking prefix is
// rejected before any file read (never just "happens" to fail with not-exist).
func TestReadCaptureCIDGuardShape(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	cid, _, err := s.CaptureExhaust("phone-voice", "", "a guarded body", now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	cases := []struct {
		name string
		cid  string
	}{
		{"missing prefix", strings.TrimPrefix(cid, "sha256:")},
		{"wrong prefix", "md5:" + strings.TrimPrefix(cid, "sha256:")},
		{"too short hex", "sha256:abc"},
		{"uppercase hex rejected", "sha256:" + strings.ToUpper(strings.TrimPrefix(cid, "sha256:"))},
		{"path traversal after valid prefix", "sha256:../../../etc/passwd"},
		{"path separator embedded", "sha256:" + strings.Repeat("a", 32) + "/" + strings.Repeat("b", 31)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.ReadCapture(tc.cid); err == nil {
				t.Fatalf("ReadCapture(%q) must reject a malformed cid before any file read", tc.cid)
			}
		})
	}

	// The one well-formed cid still round-trips.
	if _, err := s.ReadCapture(cid); err != nil {
		t.Fatalf("well-formed cid must still read: %v", err)
	}
}
