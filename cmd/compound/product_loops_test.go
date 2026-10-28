package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/mem"
	"compound/internal/product"
)

func TestProductLoopsJSONDecodesModel(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	captureStdout(t, func() {
		cmdProduct([]string{
			"signal",
			"--gsc-report", filepath.Join("..", "..", "fixtures", "product", "gsc-report.json"),
			"--product", "product:cli-loop",
			"--site", "example.com",
			"--metric", "clicks",
		})
	})
	out := captureStdout(t, func() {
		cmdProduct([]string{"loops", "--json", "--product", "product:cli-loop", "--recent", "2", "--stale-days", "14"})
	})
	var model product.LoopModel
	if err := json.Unmarshal([]byte(out), &model); err != nil {
		t.Fatalf("decode loops JSON: %v\n%s", err, out)
	}
	if err := model.Validate(); err != nil {
		t.Fatalf("validate loops JSON: %v\n%s", err, out)
	}
	if len(model.Products) != 1 || model.Products[0].ProductID != "product:cli-loop" {
		t.Fatalf("model products = %+v", model.Products)
	}
	signals := model.Products[0].SignalsByMetric["gsc_clicks"]
	if len(signals) != 1 || signals[0].Provider != "google_search_console" {
		t.Fatalf("loop signals = %+v", signals)
	}
}

func TestProductLoopsTextPrintsOpenHypothesisContract(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = product.Apply(homePath, store, product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:loop-contract",
			Name:           "Loop Contract",
			OwnerEntity:    "project:loop-contract",
			Surfaces:       []product.Surface{{ID: "app-store", Kind: "store", Ref: "fixture://store"}},
			Channels:       []string{"app-store"},
			Positioning:    "Synthetic product loop contract.",
			PrivacyPosture: "test_fixture",
		},
		Hypotheses: []product.Hypothesis{{
			ID:                "hyp_loop_contract",
			ProductID:         "product:loop-contract",
			ChangeEvent:       "Prepared outbound distribution dry-run drafts.",
			ExpectedEffect:    "reviewed outbound drafts improve product-message clarity before public distribution",
			Metrics:           []string{"distribution_draft_quality"},
			ObservationWindow: "14d",
			Result:            "unknown",
			NextAction:        "wait for real distribution signal evidence before review",
			EvidenceRefs:      []string{"outbound_dry_run_review:fixture"},
		}},
		EvidenceRefs: []string{"fixture:loop-contract"},
	}, "fixture://loop-contract.yaml", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{"loops", "--product", "product:loop-contract"})
	})
	for _, want := range []string{
		"- open_hypotheses:",
		"hyp_loop_contract",
		"metrics=distribution_draft_quality",
		"window=14d",
		"next=wait for real distribution signal evidence before review",
		"- open_hypotheses_by_missing_signal_source:",
		"unknown hypotheses=hyp_loop_contract metrics=distribution_draft_quality next=wait for real distribution signal evidence before review",
		"- action_cards:",
		"product_signal_import",
		"command=compound product signal <signal.yaml|source-export.yaml>",
		"boundary=local ProductSignal import only",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("product loops text missing %q:\n%s", want, out)
		}
	}
}
