package main

// CLI falsifiers for the product-review provenance gate (packet
// 2026-07-10-product-review-provenance-gate). These use the re-exec child-arm
// pattern (see director_cli_boundary_test.go) so the parent can observe the
// real os.Exit code from a die()/usage refusal without terminating the runner.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/mem"
	"compound/internal/product"
)

const productReviewProvenanceSubEnv = "COMPOUND_PRODUCT_REVIEW_PROVENANCE_SUB"

// seedFocusHome imports the market-loop fixture into home so the hypothesis
// referenced by fixtures/product/review.yaml exists and a bare
// `product review <yaml>` would (pre-gate) succeed.
func seedFocusHome(t *testing.T, home string) {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
}

// Falsifier (a): `compound product review <review.yaml>` without --reviewed-by
// must refuse (exit 2) with a usage line naming the flag. Red today: it
// succeeds and writes an unattributed review record.
func TestProductReviewRequiresReviewedByFlag(t *testing.T) {
	if os.Getenv(productReviewProvenanceSubEnv) == "review-no-flag" {
		cmdProduct([]string{"review", os.Getenv("COMPOUND_PRP_REVIEW_PATH")})
		return // if cmdProduct returns, exit 0 fails the parent's exit-2 assertion
	}
	home := t.TempDir()
	seedFocusHome(t, home)
	reviewPath, err := filepath.Abs("../../fixtures/product/review.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProductReviewRequiresReviewedByFlag$")
	cmd.Env = append(os.Environ(),
		productReviewProvenanceSubEnv+"=review-no-flag",
		"COMPOUND_HOME="+home,
		"COMPOUND_PRP_REVIEW_PATH="+reviewPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("product review without --reviewed-by should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("product review without --reviewed-by should exit 2 (usage), got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "--reviewed-by") {
		t.Fatalf("usage must name --reviewed-by, got %s", output)
	}
}

// seedReviewedActionReport runs the observation -> action-plan -> action-review
// chain into home and returns the reviewed action-review report path.
func seedReviewedActionReport(t *testing.T, home string) string {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", home)
	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/gsc-product-metric.json", "--apply-product-signals", "--json"})
	})
	model, err := product.BuildLoopModel(home, product.LoopOptions{ProductID: "product:focus-notes", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Products) != 1 {
		t.Fatalf("expected one product loop, got %+v", model.Products)
	}
	card := productLoopActionCard(model.Products[0], "product_signal_hypothesis_review")
	if card.ID == "" {
		t.Fatalf("expected a reviewed action card, got %+v", model.Products[0].ActionCards)
	}
	actionPlan, err := product.BuildActionPlan(home, "product:focus-notes", card.ID, "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := product.RecordActionPlan(home, actionPlan)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := readTestFile(t, paths.ReviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_decision_propose_work_park_reject", "propose_work")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "reviewed action card before work admission")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.75")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_reviewed_by_actor", "operator")
	reviewPath := filepath.Join(t.TempDir(), "product-action.review.yaml")
	if err := os.WriteFile(reviewPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	actionReview, err := product.LoadActionReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := product.ApplyActionReview(home, actionReview, reviewPath, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedPath == "" {
		t.Fatalf("action review report missing reviewed path: %+v", report)
	}
	return report.ReviewedPath
}

// Falsifier (c): `product action-work-plan --reviewed <r>` with no --actor must
// refuse. Red today: it silently records actor "codex".
func TestProductActionWorkPlanRequiresActor(t *testing.T) {
	if os.Getenv(productReviewProvenanceSubEnv) == "workplan-no-actor" {
		cmdProduct([]string{"action-work-plan", "--reviewed", os.Getenv("COMPOUND_PRP_REVIEWED_PATH")})
		return
	}
	home := t.TempDir()
	reviewedPath := seedReviewedActionReport(t, home)
	cmd := exec.Command(os.Args[0], "-test.run=^TestProductActionWorkPlanRequiresActor$")
	cmd.Env = append(os.Environ(),
		productReviewProvenanceSubEnv+"=workplan-no-actor",
		"COMPOUND_HOME="+home,
		"COMPOUND_PRP_REVIEWED_PATH="+reviewedPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("action-work-plan without --actor should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("action-work-plan without --actor should exit 2 (usage), got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "--actor") {
		t.Fatalf("usage must name --actor, got %s", output)
	}
}
