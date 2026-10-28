package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

var csObsNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// csObserve runs the door body against a test home and returns exit code + captured stdout/stderr.
func csObserve(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("COMPOUND_HOME", home)
	var stdout, stderr bytes.Buffer
	code := runSourceChangeStreamObserve(args, &stdout, &stderr, csObsNow)
	return code, stdout.String(), stderr.String()
}

// writeIntake writes a capture file + an intake JSON referencing it, returns the intake path.
func writeIntake(t *testing.T, dir string, captureBody string, cands []sourceobs.ChangeStreamCandidate) string {
	t.Helper()
	capturePath := filepath.Join(dir, "capture.json")
	if err := os.WriteFile(capturePath, []byte(captureBody), 0o644); err != nil {
		t.Fatal(err)
	}
	intake := sourceobs.ChangeStreamIntake{
		SchemaVersion: sourceobs.ChangeStreamSchemaVersion,
		SourceCapture: capturePath,
		GeneratedAt:   "2026-07-19T09:00:00Z",
		Candidates:    cands,
	}
	b, _ := json.Marshal(intake)
	intakePath := filepath.Join(dir, "intake.json")
	if err := os.WriteFile(intakePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return intakePath
}

func csCountFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// F1: a repo-signal drifting in parks a product_hypothesis candidate no-promote, records a
// no-promote decision, and content-addresses the raw capture body. RED without the door.
func TestChangeStreamObserveRepoSignalParksProductHypothesisNoPromote(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeIntake(t, dir, `{"hot_repos":[{"name":"notekeep"}]}`, []sourceobs.ChangeStreamCandidate{
		{Type: "repo-signal", EntityHint: "notekeep", Surface: "App Store metadata/ASO", Summary: "notekeep on main: dirty 5", Score: 6, EvidenceRefs: []string{"repo:notekeep"}},
	})
	code, stdout, stderr := csObserve(t, home, "--intake", intake, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		CaptureSHA string           `json:"capture_sha"`
		BodyRef    string           `json:"body_ref"`
		Report     sourceobs.Report `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("parse stdout: %v\n%s", err, stdout)
	}
	if len(out.Report.CandidateIDs) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(out.Report.CandidateIDs))
	}
	if len(out.Report.NoPromoteIDs) != 1 {
		t.Fatalf("want 1 no_promote decision, got %d", len(out.Report.NoPromoteIDs))
	}
	// candidate lands in the product-hypotheses inbox.
	if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 1 {
		t.Fatalf("want 1 product-hypothesis candidate file, got %d", got)
	}
	// the raw capture body is content-addressed under artifacts/sha256.
	if got := csCountFiles(t, filepath.Join(home, "artifacts", "sha256")); got != 1 {
		t.Fatalf("want 1 preserved capture body, got %d", got)
	}
	if out.BodyRef == "" || out.CaptureSHA == "" {
		t.Fatalf("want body_ref + capture_sha, got %q %q", out.BodyRef, out.CaptureSHA)
	}
}

// F2: canon-file-signal maps to a capability_observation candidate (the kind canon-observe parks),
// NOT product_hypothesis. RED if the door mis-routed the kind.
func TestChangeStreamObserveCanonSignalMapsToCapabilityObs(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeIntake(t, dir, `{"harness_canon":[{"path":"~/.codex/config.toml","changed":true}]}`, []sourceobs.ChangeStreamCandidate{
		{Type: "canon-file-signal", EntityHint: "~/.codex/config.toml", Surface: "harness-governance", Summary: "harness canon file changed", Score: 4},
	})
	code, _, stderr := csObserve(t, home, "--intake", intake)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got := csCountFiles(t, filepath.Join(home, "inbox", "capability-observations")); got != 1 {
		t.Fatalf("want 1 capability-observation candidate, got %d", got)
	}
	if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
		t.Fatalf("canon signal must NOT land in product-hypotheses, got %d", got)
	}
}

// F3: an idempotent re-run of the identical intake must not spray duplicate candidates or bodies
// (deterministic content-addressed ids overwrite). RED if ids were time/nonce-based.
func TestChangeStreamObserveIdempotentReRun(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeIntake(t, dir, `{"hot_repos":[{"name":"postready"}]}`, []sourceobs.ChangeStreamCandidate{
		{Type: "repo-signal", EntityHint: "postready", Surface: "runtime/product", Summary: "postready dirty", Score: 5},
		{Type: "agent-history-row", EntityHint: "postready", Surface: "runtime/product", Summary: "session touched postready", Score: 4},
	})
	code1, out1, _ := csObserve(t, home, "--intake", intake, "--json")
	code2, out2, _ := csObserve(t, home, "--intake", intake, "--json")
	if code1 != 0 || code2 != 0 {
		t.Fatalf("exits %d %d", code1, code2)
	}
	if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 2 {
		t.Fatalf("re-run duplicated candidates: want 2, got %d", got)
	}
	if got := csCountFiles(t, filepath.Join(home, "artifacts", "sha256")); got != 1 {
		t.Fatalf("re-run duplicated preserved body: want 1, got %d", got)
	}
	// same observation id both runs (content-addressed).
	var r1, r2 struct {
		ObservationID string `json:"observation_id"`
	}
	json.Unmarshal([]byte(out1), &r1)
	json.Unmarshal([]byte(out2), &r2)
	if r1.ObservationID == "" || r1.ObservationID != r2.ObservationID {
		t.Fatalf("observation id not stable: %q vs %q", r1.ObservationID, r2.ObservationID)
	}
}

// F4: malformed input fails closed (exit 1) and parks NO candidates — bad JSON, a phantom capture,
// and an unknown candidate type. RED if any wrote a candidate or exited 0.
func TestChangeStreamObserveMalformedFailsClosed(t *testing.T) {
	t.Run("bad-json", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.json")
		os.WriteFile(bad, []byte("{not json"), 0o644)
		code, _, _ := csObserve(t, home, "--intake", bad)
		if code != 1 {
			t.Fatalf("want exit 1, got %d", code)
		}
		if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
			t.Fatalf("bad json parked %d candidates", got)
		}
	})
	t.Run("phantom-capture", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		intake := sourceobs.ChangeStreamIntake{SchemaVersion: sourceobs.ChangeStreamSchemaVersion, SourceCapture: filepath.Join(dir, "does-not-exist.json"), Candidates: []sourceobs.ChangeStreamCandidate{{Type: "repo-signal", EntityHint: "x"}}}
		b, _ := json.Marshal(intake)
		p := filepath.Join(dir, "intake.json")
		os.WriteFile(p, b, 0o644)
		code, _, _ := csObserve(t, home, "--intake", p)
		if code != 1 {
			t.Fatalf("want exit 1 for missing capture, got %d", code)
		}
	})
	t.Run("unknown-type", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		intake := writeIntake(t, dir, `{"x":1}`, []sourceobs.ChangeStreamCandidate{{Type: "bogus-kind", EntityHint: "x", Summary: "y"}})
		code, _, stderr := csObserve(t, home, "--intake", intake)
		if code != 1 {
			t.Fatalf("want exit 1 for unknown type, got %d (%s)", code, stderr)
		}
		if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
			t.Fatalf("unknown type parked %d candidates", got)
		}
		if got := csCountFiles(t, filepath.Join(home, "inbox", "capability-observations")); got != 0 {
			t.Fatalf("unknown type parked %d capability candidates", got)
		}
	})
}

// F5: a preserve-only call (no candidates) content-addresses the body and parks NOTHING (exit 0).
// This is the capture-time write: real data (the original artifact) flows into COMPOUND_HOME. RED
// if preserve-only errored or parked a candidate.
func TestChangeStreamObservePreserveOnly(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeIntake(t, dir, `{"generated_at":"2026-07-19","hot_repos":[]}`, nil)
	code, stdout, stderr := csObserve(t, home, "--intake", intake, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		BodyRef    string `json:"body_ref"`
		Candidates int    `json:"candidates"`
		Mode       string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout)
	}
	if out.Mode != "preserve_only" || out.Candidates != 0 || out.BodyRef == "" {
		t.Fatalf("bad preserve-only report: %+v", out)
	}
	if got := csCountFiles(t, filepath.Join(home, "artifacts", "sha256")); got != 1 {
		t.Fatalf("preserve-only did not content-address the body: %d", got)
	}
	if got := csCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
		t.Fatalf("preserve-only parked %d candidates", got)
	}
}

// F6: the parked candidate is a no-promote pointer that resolves back to the preserved body — its
// verdict is no_promote and its evidence carries the content-addressed body ref (keep-all-data
// lossless source graph: candidate -> observation -> preserved original). RED if the link were lost.
func TestChangeStreamObserveCandidateLinksPreservedBody(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeIntake(t, dir, `{"hot_repos":[{"name":"shiftworth"}]}`, []sourceobs.ChangeStreamCandidate{
		{Type: "repo-signal", EntityHint: "shiftworth", Surface: "pricing/paywall/IAP", Summary: "shiftworth pricing", Score: 6},
	})
	code, stdout, stderr := csObserve(t, home, "--intake", intake, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var out struct {
		BodyRef string `json:"body_ref"`
		Report  struct {
			CandidateIDs []string `json:"candidate_ids"`
		} `json:"report"`
	}
	json.Unmarshal([]byte(stdout), &out)
	candPath := filepath.Join(home, "inbox", "product-hypotheses", out.Report.CandidateIDs[0]+".json")
	raw, err := os.ReadFile(candPath)
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	var cand sourceobs.CandidateRecord
	if err := json.Unmarshal(raw, &cand); err != nil {
		t.Fatal(err)
	}
	if cand.Verdict != "no_promote" {
		t.Fatalf("want verdict no_promote, got %q", cand.Verdict)
	}
	if cand.PromoteAllowed {
		t.Fatalf("candidate must be promote_allowed=false")
	}
	foundBody := false
	for _, ref := range cand.EvidenceRefs {
		if ref == out.BodyRef {
			foundBody = true
		}
	}
	if !foundBody {
		t.Fatalf("candidate evidence %v missing preserved body ref %q", cand.EvidenceRefs, out.BodyRef)
	}
}
