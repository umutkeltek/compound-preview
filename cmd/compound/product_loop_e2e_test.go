package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/mem"
	"compound/internal/observation"
	"compound/internal/product"
	"compound/internal/work"
)

func TestObservationProductLoopActionToWorkEnvelopeProof(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	applyOut := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/gsc-product-metric.json", "--apply-product-signals", "--json"})
	})
	var observeReport observation.Report
	if err := json.Unmarshal([]byte(applyOut), &observeReport); err != nil {
		t.Fatalf("parse observe-batch report: %v\n%s", err, applyOut)
	}
	if len(observeReport.ProductApplyResults) != 1 || len(observeReport.ProductApplyResults[0].SignalIDs) != 1 {
		t.Fatalf("observe report = %+v", observeReport)
	}

	model, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:focus-notes", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Products) != 1 {
		t.Fatalf("model products = %+v", model.Products)
	}
	card := productLoopActionCard(model.Products[0], "product_signal_hypothesis_review")
	if card.ID == "" {
		t.Fatalf("observation-created signal should expose reviewed action card: %+v", model.Products[0].ActionCards)
	}

	actionPlan, err := product.BuildActionPlan(homePath, "product:focus-notes", card.ID, "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := product.RecordActionPlan(homePath, actionPlan)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := readTestFile(t, paths.ReviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_decision_propose_work_park_reject", "propose_work")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "observation signal needs a reviewed work envelope before any product action")
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
	actionReport, err := product.ApplyActionReview(homePath, actionReview, reviewPath, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if actionReport.Decision != "propose_work" || actionReport.ReviewedPath == "" {
		t.Fatalf("action review report = %+v", actionReport)
	}

	workPlanOut := captureStdout(t, func() {
		cmdProduct([]string{"action-work-plan", "--reviewed", actionReport.ReviewedPath, "--actor", "codex", "--json"})
	})
	var workPlanReport struct {
		SchemaVersion string                  `json:"schema_version"`
		ProductAction product.ActionWorkDraft `json:"product_action"`
		WorkPlan      work.CreatePlan         `json:"work_plan"`
	}
	if err := json.Unmarshal([]byte(workPlanOut), &workPlanReport); err != nil {
		t.Fatalf("parse action-work-plan: %v\n%s", err, workPlanOut)
	}
	if workPlanReport.WorkPlan.WorkID == "" || workPlanReport.WorkPlan.ActionContext == nil {
		t.Fatalf("work plan should carry reviewed action context: %+v", workPlanReport)
	}
	if !stringSliceContains(workPlanReport.WorkPlan.Work.EvidenceRefs, "product_action_card:"+card.ID) {
		t.Fatalf("work plan evidence should cite action card: %+v", workPlanReport.WorkPlan.Work.EvidenceRefs)
	}

	admitOut := captureStdout(t, func() {
		cmdWork([]string{"admit-plan", "--plan", workPlanReport.WorkPlan.PlanPath, "--by", "operator-test", "--json"})
	})
	var admitReport work.AdmitCreatePlanReport
	if err := json.Unmarshal([]byte(admitOut), &admitReport); err != nil {
		t.Fatalf("parse admit-plan: %v\n%s", err, admitOut)
	}
	if admitReport.WorkID != workPlanReport.WorkPlan.WorkID || !admitReport.PolicyAllowed {
		t.Fatalf("admit report = %+v", admitReport)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Advance(workPlanReport.WorkPlan.WorkID, work.AdvanceInput{To: "ready", Actor: "operator-test"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Claim(workPlanReport.WorkPlan.WorkID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Advance(workPlanReport.WorkPlan.WorkID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	closed, err := workStore.Close(workPlanReport.WorkPlan.WorkID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "reviewed product-loop action completed in synthetic fixture",
		Changed:   true,
		ProofRefs: []string{"product_action_card:" + card.ID, "product_signal:" + observeReport.ProductApplyResults[0].SignalIDs[0]},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Transition.ID == "" || closed.Receipt.ID == "" {
		t.Fatalf("closed work missing receipt/transition: %+v", closed)
	}

	after, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:focus-notes", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	afterGroup := productLoopSignalGap(after.Products[0], card.ID)
	if !afterGroup.Consumed || !stringSliceContains(afterGroup.ConsumedByWorkIDs, workPlanReport.WorkPlan.WorkID) {
		t.Fatalf("loop should mark reviewed action consumed after terminal work: %+v", afterGroup)
	}
	if !stringSliceContains(afterGroup.ConsumedEvidenceRefs, "transition:"+closed.Transition.ID) {
		t.Fatalf("consumed evidence refs should include transition proof: %+v", afterGroup.ConsumedEvidenceRefs)
	}
}

func productLoopActionCard(loop product.ProductLoop, kind string) product.LoopActionCard {
	for _, card := range loop.ActionCards {
		if card.Kind == kind {
			return card
		}
	}
	return product.LoopActionCard{}
}

func productLoopSignalGap(loop product.ProductLoop, cardID string) product.LoopSignalHypothesisGap {
	for _, group := range loop.SignalsWithoutHypothesis {
		for _, workID := range group.ConsumedByWorkIDs {
			if strings.TrimSpace(workID) != "" && group.Consumed {
				return group
			}
		}
		for _, card := range group.ActionCards {
			if card.ID == cardID {
				return group
			}
		}
	}
	return product.LoopSignalHypothesisGap{}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
