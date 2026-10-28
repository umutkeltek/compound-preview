package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

var invObsNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// invObserve runs the door body against a test home and returns exit code + captured streams.
func invObserve(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("COMPOUND_HOME", home)
	var stdout, stderr bytes.Buffer
	code := runSourceInventoryObserve(args, &stdout, &stderr, invObsNow)
	return code, stdout.String(), stderr.String()
}

// writeInvIntake writes an inventory.json capture (whose exact bytes are the preserved original) and
// an intake JSON referencing it, returning the intake path. captureBody is the raw inventory.json.
func writeInvIntake(t *testing.T, dir, captureBody, generatedAt string, projects []sourceobs.InventoryRepo) string {
	t.Helper()
	capturePath := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(capturePath, []byte(captureBody), 0o644); err != nil {
		t.Fatal(err)
	}
	intake := sourceobs.InventoryIntake{
		SchemaVersion: sourceobs.InventoryIntakeSchemaVersion,
		SourceCapture: capturePath,
		GeneratedAt:   generatedAt,
		Projects:      projects,
	}
	b, _ := json.Marshal(intake)
	intakePath := filepath.Join(dir, "intake.json")
	if err := os.WriteFile(intakePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return intakePath
}

func invCountFiles(t *testing.T, dir string) int {
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

func invCountLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			n++
		}
	}
	return n
}

func boolp(b bool) *bool { return &b }

func twoRepos() []sourceobs.InventoryRepo {
	return []sourceobs.InventoryRepo{
		{Slug: "products-notekeep", Name: "notekeep", Domain: "products", RelativePath: "products/notekeep", Git: false, MarkerFiles: []string{"CLAUDE.md", "AGENTS.md"}, ProjectStrength: "weak"},
		{Slug: "notekeep-android", Name: "android_app", Domain: "products", RelativePath: "products/notekeep/android_app", Git: true, Branch: "main", Dirty: boolp(false), RecentCommit: "d62f43f prepare release", MarkerFiles: []string{"AGENTS.md"}, ProjectStrength: "strong"},
	}
}

// F1: each inventory row parks a product_hypothesis candidate no-promote, records a no-promote
// decision, and content-addresses the raw inventory body. RED if a row promoted or the body were not
// preserved.
func TestInventoryObserveRowsParkProductHypothesisNoPromote(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeInvIntake(t, dir, `{"generated_at":"2026-07-19T09:00:00+03:00","projects":[{"slug":"products-notekeep"}]}`, "2026-07-19T09:00:00+03:00", twoRepos())
	code, stdout, stderr := invObserve(t, home, "--intake", intake, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		CaptureSHA string `json:"capture_sha"`
		ContentSHA string `json:"content_sha"`
		BodyRef    string `json:"body_ref"`
		Report     struct {
			CandidateIDs []string `json:"candidate_ids"`
			NoPromoteIDs []string `json:"no_promote_ids"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout)
	}
	if len(out.Report.CandidateIDs) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(out.Report.CandidateIDs))
	}
	if len(out.Report.NoPromoteIDs) != 2 {
		t.Fatalf("want 2 no-promote decisions, got %d", len(out.Report.NoPromoteIDs))
	}
	if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 2 {
		t.Fatalf("want 2 product-hypothesis files, got %d", got)
	}
	if got := invCountFiles(t, filepath.Join(home, "artifacts", "sha256")); got != 1 {
		t.Fatalf("want 1 preserved inventory body, got %d", got)
	}
	if out.BodyRef == "" || out.CaptureSHA == "" || out.ContentSHA == "" {
		t.Fatalf("want body_ref + capture_sha + content_sha, got %q %q %q", out.BodyRef, out.CaptureSHA, out.ContentSHA)
	}
}

// F2 (crux): an idempotent re-run over an UNCHANGED workspace must not spray duplicate candidates or
// no-promote decisions, EVEN when the raw body and generated_at differ (the door keys on stable
// content, not wall-clock time). RED if candidate ids were time/nonce-based or the ledger double-wrote.
func TestInventoryObserveIdempotentReRunAcrossTimestamps(t *testing.T) {
	home := t.TempDir()
	// Same capture path across runs (the production cron always writes the same inventory.json),
	// but different body bytes + generated_at between runs — only the wall-clock differs, the
	// workspace is unchanged.
	dir := t.TempDir()
	intake1 := writeInvIntake(t, dir, `{"generated_at":"2026-07-19T09:00:00+03:00","projects":[1]}`, "2026-07-19T09:00:00+03:00", twoRepos())
	code1, out1, err1 := invObserve(t, home, "--intake", intake1, "--json")
	if code1 != 0 {
		t.Fatalf("run1 exit %d: %s", code1, err1)
	}
	intake2 := writeInvIntake(t, dir, `{"generated_at":"2026-07-19T23:59:59+03:00","projects":[2]}`, "2026-07-19T23:59:59+03:00", twoRepos())
	code2, out2, err2 := invObserve(t, home, "--intake", intake2, "--json")
	if code2 != 0 {
		t.Fatalf("run2 exit %d: %s", code2, err2)
	}
	var r1, r2 struct {
		ObservationID string `json:"observation_id"`
	}
	json.Unmarshal([]byte(out1), &r1)
	json.Unmarshal([]byte(out2), &r2)
	if r1.ObservationID == "" || r1.ObservationID != r2.ObservationID {
		t.Fatalf("observation id not stable across timestamped re-runs: %q vs %q", r1.ObservationID, r2.ObservationID)
	}
	if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 2 {
		t.Fatalf("re-run sprayed candidates: want 2, got %d", got)
	}
	if got := invCountLines(t, filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); got != 2 {
		t.Fatalf("re-run double-wrote the no-promote ledger: want 2, got %d", got)
	}
}

// F3: malformed input fails closed (exit 1) and parks NO candidates — bad JSON, a phantom capture,
// or a row with no identity. RED if any wrote a candidate or exited 0.
func TestInventoryObserveMalformedFailsClosed(t *testing.T) {
	t.Run("bad-json", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.json")
		os.WriteFile(bad, []byte("{not json"), 0o644)
		code, _, _ := invObserve(t, home, "--intake", bad)
		if code != 1 {
			t.Fatalf("want exit 1, got %d", code)
		}
		if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
			t.Fatalf("bad json parked %d candidates", got)
		}
	})
	t.Run("phantom-capture", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		intake := sourceobs.InventoryIntake{SchemaVersion: sourceobs.InventoryIntakeSchemaVersion, SourceCapture: filepath.Join(dir, "does-not-exist.json"), Projects: twoRepos()}
		b, _ := json.Marshal(intake)
		p := filepath.Join(dir, "intake.json")
		os.WriteFile(p, b, 0o644)
		code, _, _ := invObserve(t, home, "--intake", p)
		if code != 1 {
			t.Fatalf("want exit 1 missing capture, got %d", code)
		}
		if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
			t.Fatalf("phantom capture parked %d candidates", got)
		}
	})
	t.Run("identity-less-row", func(t *testing.T) {
		home := t.TempDir()
		dir := t.TempDir()
		intake := writeInvIntake(t, dir, `{"projects":[1]}`, "2026-07-19T09:00:00+03:00", []sourceobs.InventoryRepo{{Domain: "products"}})
		code, _, _ := invObserve(t, home, "--intake", intake)
		if code != 1 {
			t.Fatalf("want exit 1 on identity-less row, got %d", code)
		}
		if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
			t.Fatalf("identity-less row parked %d candidates", got)
		}
	})
}

// F4: preserve-only (a census with zero rows) content-addresses the body and parks NOTHING. The
// capture-time write: the original artifact flows into COMPOUND_HOME with no derived candidates. RED
// if preserve-only errored or parked a candidate.
func TestInventoryObservePreserveOnly(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeInvIntake(t, dir, `{"generated_at":"2026-07-19","projects":[]}`, "2026-07-19", nil)
	code, stdout, stderr := invObserve(t, home, "--intake", intake, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		BodyRef  string `json:"body_ref"`
		Projects int    `json:"projects"`
		Mode     string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout)
	}
	if out.Mode != "preserve_only" || out.Projects != 0 || out.BodyRef == "" {
		t.Fatalf("bad preserve-only report: %+v", out)
	}
	if got := invCountFiles(t, filepath.Join(home, "artifacts", "sha256")); got != 1 {
		t.Fatalf("preserve-only did not content-address the body: %d", got)
	}
	if got := invCountFiles(t, filepath.Join(home, "inbox", "product-hypotheses")); got != 0 {
		t.Fatalf("preserve-only parked %d candidates", got)
	}
}

// F5: the parked candidate is a no-promote pointer that resolves back to the preserved body — verdict
// no_promote and evidence carries the content-addressed body ref (keep-all-data lossless source
// graph: candidate -> observation -> preserved original). RED if the link were lost or promotable.
func TestInventoryObserveCandidateLinksPreservedBody(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	intake := writeInvIntake(t, dir, `{"projects":[{"slug":"shiftworth"}]}`, "2026-07-19T09:00:00+03:00", []sourceobs.InventoryRepo{
		{Slug: "shiftworth", Name: "ShiftWorthRN", Domain: "products", RelativePath: "products/shiftworth", Git: true, Branch: "main", Dirty: boolp(true), ProjectStrength: "strong"},
	})
	code, stdout, stderr := invObserve(t, home, "--intake", intake, "--json")
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
	if len(out.Report.CandidateIDs) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(out.Report.CandidateIDs))
	}
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
