package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

var expSmokeNow = time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

// importExpSmokeReviewGrant seeds the standing grant the governed review-finding
// door consumes, so the emit path is allowed deterministically (mirrors
// TestReviewsRecordFindingConsumesStandingGrantAndRecordsWorkAction).
func importExpSmokeReviewGrant(t *testing.T, homePath string) {
	t.Helper()
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
      - review.summary
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, expSmokeNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
}

func writeExpSmokeBaseline(t *testing.T, root, baselinePath string) {
	t.Helper()
	rep, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, WriteBaseline: true, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if rep.Decision != "baseline-written" {
		t.Fatalf("baseline decision = %q, want baseline-written", rep.Decision)
	}
}

// hashTree fingerprints every file (relpath + content) under root so a byte-diff
// of the real COMPOUND_HOME is provable before/after a replay.
func hashTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var files []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Falsifier (a): a regression-seeded replay produces a review card that NAMES
// the regression (summary + typed friction receipts), through the governed door.
func TestExperientialSmokeRegressionEmitsReviewCardNamingRegression(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", realHome)
	if err := home.Init(realHome, "test", expSmokeNow); err != nil {
		t.Fatal(err)
	}
	importExpSmokeReviewGrant(t, realHome)

	root := t.TempDir()
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")
	writeExpSmokeBaseline(t, root, baselinePath)

	rep, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, InjectRegression: true, Emit: true, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rep.Regressed {
		t.Fatal("expected regression on a regression-seeded replay")
	}
	if rep.Decision != "review-card-emitted" {
		t.Fatalf("decision = %q, want review-card-emitted", rep.Decision)
	}
	if rep.ReviewFinding == nil {
		t.Fatal("expected a governed review card, got none")
	}
	if _, err := os.Stat(rep.ReviewFinding.ArtifactPath); err != nil {
		t.Fatalf("review card artifact missing on disk: %v", err)
	}
	if rep.ReviewFinding.PolicyOutcome != "allow_with_obligations" {
		t.Fatalf("review card not governed-allowed: outcome %q", rep.ReviewFinding.PolicyOutcome)
	}
	// The card must NAME every regressed dimension in its summary.
	if len(rep.RegressedDims) == 0 {
		t.Fatal("no regressed dimensions recorded")
	}
	for _, d := range rep.RegressedDims {
		if !strings.Contains(rep.ReviewFinding.Summary, d.Dimension) {
			t.Fatalf("card summary does not name regressed dimension %q:\n%s", d.Dimension, rep.ReviewFinding.Summary)
		}
	}
	// One typed friction receipt per regressed dimension, covering all classes.
	if len(rep.Receipts) != len(rep.RegressedDims) {
		t.Fatalf("receipts=%d != regressed dims=%d", len(rep.Receipts), len(rep.RegressedDims))
	}
	wantKinds := map[string]bool{
		"interaction_cost_regression": false,
		"belief_ledger_divergence":    false,
		"comprehension_miss":          false,
	}
	for _, r := range rep.Receipts {
		if _, ok := wantKinds[r.Kind]; ok {
			wantKinds[r.Kind] = true
		}
	}
	for kind, seen := range wantKinds {
		if !seen {
			t.Fatalf("missing typed friction receipt kind %q", kind)
		}
	}
	// The grant-check stub ran and honestly reported no ux_friction grant.
	if rep.UxFrictionGrantActive {
		t.Fatal("no ux_friction grant was minted; stub must report inactive")
	}
	if rep.AutoFileStub == "" {
		t.Fatal("expected the auto-file stub note documenting the TODO")
	}
}

// Falsifier (b): a no-regression replay produces NO card — silence is the default.
func TestExperientialSmokeNoRegressionStaysSilent(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", realHome)
	if err := home.Init(realHome, "test", expSmokeNow); err != nil {
		t.Fatal(err)
	}
	importExpSmokeReviewGrant(t, realHome)

	root := t.TempDir()
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")
	writeExpSmokeBaseline(t, root, baselinePath)

	// Emit is ON, but with no regression there is nothing to emit.
	rep, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, Emit: true, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Regressed {
		t.Fatalf("clean replay reported a regression: %+v", rep.RegressedDims)
	}
	if rep.Decision != "silent" {
		t.Fatalf("decision = %q, want silent", rep.Decision)
	}
	if rep.ReviewFinding != nil {
		t.Fatalf("silence default violated: card %s emitted with no regression", rep.ReviewFinding.ID)
	}
	if len(rep.Receipts) != 0 {
		t.Fatalf("silence default violated: %d friction receipts on a clean run", len(rep.Receipts))
	}
	// A silent run writes no state/runs evidence dir.
	if _, err := os.Stat(filepath.Join(root, "state", "runs")); !os.IsNotExist(err) {
		t.Fatalf("silent run wrote a state/runs dir (err=%v)", err)
	}
}

// Falsifier (c): the replay NEVER mutates the real COMPOUND_HOME — proven by a
// ledger byte-diff. Only the deliberate governed card (under --emit) touches it.
func TestExperientialSmokeNeverMutatesRealHome(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", realHome)
	if err := home.Init(realHome, "test", expSmokeNow); err != nil {
		t.Fatal(err)
	}
	importExpSmokeReviewGrant(t, realHome)

	root := t.TempDir()
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")
	writeExpSmokeBaseline(t, root, baselinePath)

	// (i) no-regression + emit: nothing is emitted → real home byte-identical.
	before := hashTree(t, realHome)
	rep, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, Emit: true, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Decision != "silent" {
		t.Fatalf("decision = %q, want silent", rep.Decision)
	}
	if after := hashTree(t, realHome); after != before {
		t.Fatalf("no-regression replay mutated the real home:\n before=%s\n after =%s", before, after)
	}

	// (ii) regression WITHOUT emit: seed+project+grade still must not touch it.
	before2 := hashTree(t, realHome)
	rep2, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, InjectRegression: true, Emit: false, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Regressed || rep2.Decision != "regression-detected" {
		t.Fatalf("want regression-detected; got regressed=%t decision=%q", rep2.Regressed, rep2.Decision)
	}
	if after2 := hashTree(t, realHome); after2 != before2 {
		t.Fatalf("regression replay (no emit) mutated the real home:\n before=%s\n after =%s", before2, after2)
	}

	// (iii) sanity: regression WITH emit DOES record the governed card in the
	//       real home — the single intended real-home mutation.
	before3 := hashTree(t, realHome)
	rep3, err := runExperientialSmoke(experientialSmokeOptions{
		Root: root, BaselinePath: baselinePath, InjectRegression: true, Emit: true, Now: expSmokeNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Decision != "review-card-emitted" {
		t.Fatalf("decision = %q, want review-card-emitted", rep3.Decision)
	}
	if after3 := hashTree(t, realHome); after3 == before3 {
		t.Fatal("expected the governed review card to mutate the real home, but it was byte-identical")
	}
}
