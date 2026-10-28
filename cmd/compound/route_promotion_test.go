package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
)

func TestRoutePromotionCLIHappyPath(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	importOut := captureStdout(t, func() {
		cmdRoute([]string{"import-promotion-criteria", filepath.Join("..", "..", "fixtures", "route", "promotion-criteria.yaml")})
	})
	if !strings.Contains(importOut, "# route import-promotion-criteria") || !strings.Contains(importOut, "- entries: 3") || !strings.Contains(importOut, "- verdict: compound_primary") {
		t.Fatalf("bad import output:\n%s", importOut)
	}
	laneID := "panel/free-judge-tripwire"
	recordCLIPromotionRows(t, homePath, laneID, 20, 4*time.Hour)
	writeCLIPromotionComparisonEvidence(t, homePath, "cmp_cli_win", laneID, "prefer_candidate")
	writeCLIPromotionComparisonEvidence(t, homePath, "cmp_cli_loss", laneID, "prefer_baseline")

	out := captureStdout(t, func() {
		cmdRoute([]string{"promotion-review", "--lane", laneID, "--json"})
	})
	var reviews []routeevidence.PromotionReview
	if err := json.Unmarshal([]byte(out), &reviews); err != nil {
		t.Fatalf("parse promotion review JSON: %v\n%s", err, out)
	}
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v, want one", reviews)
	}
	review := reviews[0]
	if review.LaneID != laneID || review.CurrentClaimLevel != "capable" || review.Recommendation != "promote" || review.RecommendedClaimLevel != "primary" || review.Tallies.Samples != 20 {
		t.Fatalf("bad promotion review: %+v", review)
	}

	human := captureStdout(t, func() {
		cmdRoute([]string{"promotion-review", "--lane", laneID})
	})
	for _, want := range []string{"# route promotion-review", "claim=capable", "samples=20", "recommendation=promote", "comparison_wins=1", "comparison_losses=1", "review_artifacts_only_no_authority_mutation"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output missing %q:\n%s", want, human)
		}
	}
}

func TestRoutePromotionRecordHold(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	paths, err := reviewartifact.Write(homePath, "route-promotions", "route_promotion_hold_cli", map[string]any{
		"schema_version":                   "compound.route_promotion_review.v0.1",
		"id":                               "route_promotion_hold_cli",
		"status":                           "review_required",
		"recommendation":                   "hold_insufficient_evidence",
		"reason":                           "samples 2 below min_samples 10",
		"lane_id":                          "auto/judge",
		"capability_class":                 "classify-extract",
		"current_claim_level":              "assumed",
		"model_capabilities_authority_ref": routeevidence.ModelCapabilitiesAuthorityRef,
		"criteria_authority_ref":           routeevidence.PromotionCriteriaAuthorityRef,
		"evidence_refs":                    []string{"route_a", "route_b"},
	}, " ", "# Route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdRoute([]string{"promotion-record", "--review", paths.PlanPath, "--rationale", "owner reviewed current samples; keep held until criteria are met", "--json"})
	})
	var report routeevidence.PromotionHoldReviewReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse promotion-record JSON: %v\n%s", err, out)
	}
	if report.Status != "recorded" || report.Decision != "hold_recorded" || report.ReviewID != "route_promotion_hold_cli" {
		t.Fatalf("bad promotion hold report: %+v", report)
	}
	if report.ReviewPath == "" || !strings.HasSuffix(report.ReviewPath, "route_promotion_hold_cli.reviewed.json") {
		t.Fatalf("review path not recorded: %+v", report)
	}
	if len(report.Boundaries) == 0 || !strings.Contains(strings.Join(report.Boundaries, " "), "no lane promotion") {
		t.Fatalf("report should preserve no-mutation boundaries: %+v", report)
	}
}

func recordCLIPromotionRows(t *testing.T, homePath, laneID string, count int, spacing time.Duration) {
	t.Helper()
	base := cliTestNow
	for i := 0; i < count; i++ {
		ev, err := routeevidence.Build(routeevidence.BuildInput{
			CreatedAt:     base.Add(time.Duration(i) * spacing),
			RequestID:     fmt.Sprintf("cli-promotion:%s:%d", strings.ReplaceAll(laneID, "/", "_"), i),
			ClientProfile: "compound.panel.judge",
			Protocol:      "panel-lane",
			Provider:      "compound-route-plan",
			Model:         laneID,
			Status:        "completed",
			EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
		})
		if err != nil {
			t.Fatalf("build route evidence: %v", err)
		}
		if err := routeevidence.RecordEvidence(homePath, ev); err != nil {
			t.Fatalf("record route evidence: %v", err)
		}
	}
}

func writeCLIPromotionComparisonEvidence(t *testing.T, homePath, comparisonID, candidateLane, decision string) {
	t.Helper()
	path := filepath.Join(homePath, "ledger", "comparison-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir comparison evidence: %v", err)
	}
	row := map[string]any{
		"comparison_id":    comparisonID,
		"capability_class": "judge-critique",
		"baseline_lane":    "panel/baseline",
		"candidate_lane":   candidateLane,
		"decision":         decision,
		"confidence":       0.88,
		"run_id":           "prun_" + comparisonID,
		"verdict_artifact": "reviews/panel-verdicts/" + comparisonID + ".json",
		"created_at":       cliTestNow.Format(time.RFC3339),
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal comparison evidence: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open comparison evidence: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatalf("write comparison evidence: %v", err)
	}
}
