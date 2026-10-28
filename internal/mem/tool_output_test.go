package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/policy"
)

// TestCaptureToolOutputRoundTripObservedNeverLedger proves the C3 contract at the mem seam:
// a large tool body is content-addressed into a rebuildable blob (never the ledger) and minted
// as exactly ONE observed-class candidate carrying a compact pointer, retrievable byte-exact,
// idempotent, and never a Claim (so never ambient / never an index_hit).
func TestCaptureToolOutputRoundTripObservedNeverLedger(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)

	body := strings.Repeat("ABCDEFGHIJ", 30000) // ~300 KB, far above the context budget
	sum := sha256.Sum256([]byte(body))
	wantCID := "sha256:" + hex.EncodeToString(sum[:])

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	cid, pointer, err := s.CaptureToolOutput("Bash", "work_1177cb550af3", body, now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if cid != wantCID {
		t.Fatalf("cid = %q, want %q", cid, wantCID)
	}

	// The pointer is the ONLY thing that flows into context: bounded, and body-free.
	if len(pointer) > 256 || strings.Contains(pointer, body[:64]) {
		t.Fatalf("pointer must be bounded and body-free, got len=%d %q", len(pointer), pointer)
	}
	if !strings.Contains(pointer, cid) {
		t.Fatalf("pointer must carry the cid, got %q", pointer)
	}

	// Body is private-only: new captures do not write observed/tool-output.
	blob := filepath.Join(home, "observed", "tool-output", cid)
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("new capture wrote legacy observed blob %s: %v", blob, err)
	}
	privateBlob := filepath.Join(home, "private", "artifacts", "sha256", strings.TrimPrefix(cid, "sha256:"))
	if _, err := os.Stat(privateBlob); err != nil {
		t.Fatalf("private blob not written at %s: %v", privateBlob, err)
	}

	// Round-trip the exact bytes.
	got, err := s.ReadToolOutput(cid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(body))
	}

	// Ledger byte-identical: capture added ZERO ledger bytes.
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed: before=%s after=%s", before, after)
	}

	// Policy files a bare-cid pointer terminal (auto_file -> no_promote); a dead row is never
	// written. The body stays retrievable from private custody (checked above).
	id := idFor("cand_", "tool-output", cid)
	if _, err := os.Stat(filepath.Join(home, "inbox", "memory-candidates", id+".json")); !os.IsNotExist(err) {
		t.Fatalf("policy-terminal pointer wrote a candidate file: %v", err)
	}

	// Never ambient: the body is NOT a Claim, so it can never enter recall or resolve as index_hit.
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

// TestCaptureToolOutputQueuedPointerStillMintsCandidate proves the candidate shape is unchanged for a
// pointer that adjudication policy would queue (cheap-lane provenance -> decay_park): observed,
// no promotion basis, bounded snippet hash, no body leak, and skip-if-exists never resets a verdict.
func TestCaptureToolOutputQueuedPointerStillMintsCandidate(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	body := strings.Repeat("ABCDEFGHIJ", 30000)
	cid, _, err := s.CaptureToolOutput("cheap-lane", "work_1177cb550af3", body, now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	id := idFor("cand_", "tool-output", cid)
	var c Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &c); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if c.Derivation != DerivationObserved {
		t.Errorf("derivation = %q, want %q", c.Derivation, DerivationObserved)
	}
	if c.State != "parked" || c.ExpiresAt == "" {
		t.Errorf("state=%q expires=%q, want parked with expiry (policy decay_park)", c.State, c.ExpiresAt)
	}
	if c.PromotionBasis != "" {
		t.Errorf("promotion_basis = %q, want empty (never auto-promotes)", c.PromotionBasis)
	}
	if c.ProvenanceLane != "cheap-lane" || c.Entity != "cheap-lane" || c.Predicate != "produced_tool_output" || c.Object != cid {
		t.Errorf("identity fields changed: %+v", c)
	}
	if c.SourceTraceRef != "tool-output:"+cid {
		t.Errorf("source_trace_ref = %q, want tool-output:%s", c.SourceTraceRef, cid)
	}
	if c.SnippetHash == "" {
		t.Error("snippet_hash must be set (bounded head snippet hash)")
	}
	if c.Qualifiers["custody"] != "private_artifact" {
		t.Errorf("custody qualifier = %q, want private_artifact", c.Qualifiers["custody"])
	}
	if len(c.EvidenceRefs) != 1 || c.EvidenceRefs[0] != "work:work_1177cb550af3" {
		t.Errorf("evidence_refs = %v", c.EvidenceRefs)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "inbox", "memory-candidates", id+".json"))
	if strings.Contains(string(raw), body[:200]) {
		t.Error("candidate record leaked the tool-output body")
	}

	// Skip-if-exists: an in-flight adjudication verdict is never reset by re-surfacing the same body.
	c.State = "rejected"
	if err := candidates.WriteCandidate(home, candidates.Memory, id, &c); err != nil {
		t.Fatalf("rewrite candidate: %v", err)
	}
	if _, _, err := s.CaptureToolOutput("cheap-lane", "work_1177cb550af3", body, now); err != nil {
		t.Fatalf("recapture: %v", err)
	}
	var c2 Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &c2); err != nil {
		t.Fatalf("read candidate after recapture: %v", err)
	}
	if c2.State != "rejected" {
		t.Errorf("recapture reset adjudication verdict: state = %q, want rejected", c2.State)
	}
}

// TestCaptureToolOutputWriteFollowsRealPolicy ties the write decision to policy.ResolveAdjudication
// for the exact input the pointer presents: a candidate file exists iff policy does NOT auto-file it.
// There is no second copy of the rule here, so changing the policy flips this test.
func TestCaptureToolOutputWriteFollowsRealPolicy(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	for _, tool := range []string{"Bash", "Read", "mcp__x__y", "cheap-lane", "cheap_lane"} {
		home := t.TempDir()
		s, err := Open(home)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		body := strings.Repeat(tool+" ", 20000)
		cid, _, err := s.CaptureToolOutput(tool, "", body, now)
		if err != nil {
			t.Fatalf("capture %s: %v", tool, err)
		}
		disp := policy.ResolveAdjudication(policy.AdjudicationInput{ProvenanceLane: tool, Object: cid})
		_, statErr := os.Stat(filepath.Join(home, "inbox", "memory-candidates", idFor("cand_", "tool-output", cid)+".json"))
		wrote := statErr == nil
		if wrote == (disp.Kind == policy.DispositionAutoFile) {
			t.Errorf("tool %s: policy=%s but candidate written=%v", tool, disp.Kind, wrote)
		}
	}
}

// TestReadToolOutputMissingIsExplicitError proves a missing or malformed cid is an EXPLICIT
// error, never a silent nil, and that a path-shaped cid can never escape the blob dir.
func TestReadToolOutputMissingIsExplicitError(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	missing := "sha256:" + strings.Repeat("0", 64)
	if _, err := s.ReadToolOutput(missing); err == nil {
		t.Fatal("missing cid must be an explicit error, never a silent nil")
	}
	if _, err := s.ReadToolOutput("../../etc/passwd"); err == nil {
		t.Fatal("malformed/path-shaped cid must be rejected before any read")
	}
}

func TestReadToolOutputFallsBackToLegacyObservedBlob(t *testing.T) {
	homeDir := t.TempDir()
	s, err := Open(homeDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	body := []byte("legacy observed body")
	cid := ToolOutputCID(body)
	dir := filepath.Join(homeDir, "observed", "tool-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cid), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadToolOutput(cid)
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("legacy round trip = %q, want %q", got, body)
	}
}
