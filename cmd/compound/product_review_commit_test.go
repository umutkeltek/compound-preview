package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/product"
	"compound/internal/reviewartifact"
)

// The public binary creates the plan, applies its review, reports a real
// postcommit filesystem failure, and recovers the exact operation in another
// process. The fixture has no provider, credential or live product dependency.
func TestProductReviewPublicCommitRecoveryAndConsumers(t *testing.T) {
	h := t.TempDir()
	userHome := isolatedTestUserHome(t)
	bin := compoundTestBinary(t)
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "COMPOUND_HOME="+h, "HOME="+userHome)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	fixture, err := filepath.Abs("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := run("home", "init"); err != nil {
		t.Fatalf("home init: %v\n%s\n%s", err, out, stderr)
	}
	if out, stderr, err := run("product", "import", fixture); err != nil {
		t.Fatalf("import: %v\n%s\n%s", err, out, stderr)
	}
	const hypothesisID = "hyp_focus_visuals_conversion"
	const signalID = "sig_focus_screenshot_20260629"
	out, stderr, err := run("product", "review-plan", "--hypothesis", hypothesisID, "--signal", signalID)
	if err != nil {
		t.Fatalf("review plan: %v\n%s\n%s", err, out, stderr)
	}
	field := func(key string) string {
		for _, line := range strings.Split(out, "\n") {
			if value, ok := strings.CutPrefix(line, "- "+key+": "); ok {
				return value
			}
		}
		t.Fatalf("missing %s in plan output:\n%s", key, out)
		return ""
	}
	planPath, reviewPath := field("plan"), field("review_yaml")
	reviewID := field("review_id")
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewBytes, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	draft := strings.ReplaceAll(string(reviewBytes), "TODO_result_supported_refuted_or_inconclusive", "supported")
	draft = strings.ReplaceAll(draft, "rationale: \"\"", "rationale: \"Synthetic evidence reviewed with confounders retained.\"")
	draft = strings.ReplaceAll(draft, "confidence: -1", "confidence: 0.8")
	draft = strings.ReplaceAll(draft, "TODO_reviewed_by_actor", "synthetic-reviewer")
	if err := os.WriteFile(reviewPath, []byte(draft), 0o600); err != nil {
		t.Fatal(err)
	}
	// Failure occurs after exact candidate CAS and committed history, while
	// updating the existing plan projection. Preserve the broken file for replay.
	if err := os.WriteFile(planPath, []byte("invalid plan JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = run("product", "review", reviewPath, "--reviewed-by", "synthetic-reviewer", "--json")
	if err == nil {
		t.Fatal("postcommit artifact failure reported success")
	}
	var failed product.ReviewReport
	if err := json.Unmarshal([]byte(out), &failed); err != nil {
		t.Fatalf("CLI discarded typed partial result: %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	if failed.Status != "recovery_required" || failed.CandidateOutcome != "committed" || failed.ReviewID != reviewID || failed.TransitionID == "" {
		t.Fatalf("untruthful partial result: %+v", failed)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, hypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// A different byte revision cannot complete the partial operation, even if
	// its parsed fields are identical. Restoring A must remain recoverable.
	if err := os.WriteFile(reviewPath, []byte(draft+"\n# replacement source revision\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = run("product", "review", reviewPath, "--reviewed-by", "synthetic-reviewer", "--json")
	if err == nil || !strings.Contains(stderr, "source bytes changed") {
		t.Fatalf("changed source revision completed old operation: %v\n%s\n%s", err, out, stderr)
	}
	if err := os.WriteFile(reviewPath, []byte(draft), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = run("product", "review", reviewPath, "--reviewed-by", "synthetic-reviewer", "--json")
	if err != nil {
		t.Fatalf("public recovery: %v\n%s\n%s", err, out, stderr)
	}
	var committed product.ReviewReport
	if err := json.Unmarshal([]byte(out), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Status != "updated" || committed.CandidateOutcome != "committed" || committed.TransitionID != failed.TransitionID || committed.EvidenceID != failed.EvidenceID {
		t.Fatalf("recovery changed operation: failed=%+v committed=%+v", failed, committed)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, hypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatal("recovery rewrote candidate")
	}
	if out, stderr, err := run("product", "review-plan", "--hypothesis", hypothesisID, "--signal", signalID); err == nil || !strings.Contains(stderr, "already has result") {
		t.Fatalf("review plan forgot committed result: %v\n%s\n%s", err, out, stderr)
	}
	out, stderr, err = run("product", "loops", "--product", "product:focus-notes", "--json")
	if err != nil || !strings.Contains(out, reviewID) {
		t.Fatalf("loops lost committed review: %v\n%s\n%s", err, out, stderr)
	}
	out, stderr, err = run("reviews", "--area", "product-reviews", "--json")
	if err != nil {
		t.Fatalf("review queue: %v\n%s\n%s", err, out, stderr)
	}
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &queue); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range queue.Items {
		if item.JSONPath == planPath {
			found = true
			if item.Status != "recorded" || item.Readiness != "no_action" {
				t.Errorf("queue contradicts committed review: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("queue omitted plan: %s", out)
	}
	out, stderr, err = run("product", "review", reviewPath, "--reviewed-by", "another-reviewer", "--json")
	if err == nil {
		t.Fatal("CLI reused review ID with another reviewer")
	}
	var refused product.ReviewReport
	if err := json.Unmarshal([]byte(out), &refused); err != nil || refused.Status != "refused" || refused.ReviewID != reviewID {
		t.Fatalf("CLI lost refusal: %v %+v\n%s", err, refused, stderr)
	}
}
