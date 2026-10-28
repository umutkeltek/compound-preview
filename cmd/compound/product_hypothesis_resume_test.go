package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/product"
)

func TestProductHypothesisResumeCLIReportsOriginalApprovalAndCurrentMeasurement(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_HYPOTHESIS_RESUME_CLI") == "1" {
		cmdProduct([]string{"hypothesis-approve", "hyp_cli_resume", "--resume-approval", os.Getenv("COMPOUND_TEST_HYPOTHESIS_APPROVAL_ID")})
		return
	}
	homePath := t.TempDir()
	candidate := product.HypothesisCandidate{
		SchemaVersion: product.ProductHypothesisCandidate, ID: "hyp_cli_resume", Kind: string(candidates.ProductHypothesis),
		ProductID: "product:resume-fixture", ExpectedEffect: product.HypothesisDraftExpectedEffectPlaceholderPrefix,
		Metrics: []string{"activation"}, ObservationWindow: "fourteen days", Result: "unknown", NextAction: "review sentence", Verdict: candidates.Park,
	}
	if err := candidates.WriteCandidate(homePath, candidates.ProductHypothesis, candidate.ID, candidate); err != nil {
		t.Fatal(err)
	}
	approved, err := product.HypothesisApprove(homePath, product.HypothesisApproveInput{CandidateID: candidate.ID, ExpectedEffect: "Activation increases.", ReviewedBy: "synthetic-operator"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.ApplyReview(homePath, memory, product.Review{
		SchemaVersion: product.ReviewSchemaVersion, ID: "review_cli_measurement", ProductID: candidate.ProductID, HypothesisID: candidate.ID,
		Result: "supported", Rationale: "synthetic measurement", Confidence: 0.8, NextAction: "retain measured result",
		EvidenceRefs: []string{"product_signal:synthetic_cli_signal"}, ReviewedBy: "synthetic-operator",
	}, "", cliTestNow.Add(14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProductHypothesisResumeCLIReportsOriginalApprovalAndCurrentMeasurement$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_HYPOTHESIS_RESUME_CLI=1", "COMPOUND_HOME="+homePath, "COMPOUND_TEST_HYPOTHESIS_APPROVAL_ID="+approved.ApprovalID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resume CLI failed: %v\n%s", err, out)
	}
	for _, want := range []string{"approval: " + approved.ApprovalID, "status: committed", "candidate_result: supported", "candidate_expected_effect:", "transition: " + approved.TransitionID} {
		if !strings.Contains(string(out), want) {
			t.Errorf("CLI omitted truthful approval/measurement field %q:\n%s", want, out)
		}
	}
	after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidate.ID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatalf("CLI resume overwrote the newer measurement: %v", err)
	}
}
