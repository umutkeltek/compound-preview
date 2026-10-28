package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	homepkg "compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
)

type failingLensCaller struct {
	base   FixtureCaller
	lensID string
}

func (c failingLensCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	if req.LensID == c.lensID {
		return CallResult{}, fmt.Errorf("fixture lens failure for %s", req.LensID)
	}
	return c.base.Call(ctx, req)
}

type failingJudgeCaller struct {
	base FixtureCaller
}

func (c failingJudgeCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	if req.LensID == judgePromptID {
		return CallResult{}, fmt.Errorf("fixture judge failure for %s", req.LensID)
	}
	return c.base.Call(ctx, req)
}

type preflightBlockingCaller struct {
	err error
}

func (c preflightBlockingCaller) Preflight(ctx context.Context, run Run) error {
	return c.err
}

func (c preflightBlockingCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	return CallResult{}, fmt.Errorf("call should not run after preflight block")
}

func TestDecodeLensResponseStripsMarkdownFence(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "full fence",
			body: "```json\n{\"confidence\":0.8,\"findings\":[{\"claim\":\"ok\",\"severity\":\"low\",\"evidence_refs\":[\"trace://one\"]}]}\n```",
		},
		{
			name: "trailing closing fence",
			body: "{\"confidence\":0.8,\"findings\":[{\"claim\":\"ok\",\"severity\":\"low\",\"evidence_refs\":[\"trace://one\"]}]}\n```",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeLensResponse([]byte(tt.body))
			if err != nil {
				t.Fatalf("decode fenced lens response: %v", err)
			}
			if got.Confidence != 0.8 || len(got.Findings) != 1 || got.Findings[0].Claim != "ok" {
				t.Fatalf("decoded lens response = %+v", got)
			}
		})
	}
}

func TestExecuteDryRunEndToEndWritesArtifactsEvidenceAndDoesNotMutateInbox(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")
	before := readTestFile(t, filepath.Join(home, "inbox", "memory-candidates", "mcand_example_api_clean.json"))
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, Budget{})

	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if got.Status != "verdicts_recorded" || len(got.Findings) != 2 {
		t.Fatalf("run result = %+v", got)
	}
	for _, finding := range got.Findings {
		if finding.Synthesis.SchemaVersion != SynthesisSchemaVersion || finding.Synthesis.JudgeRouteEvidenceRef == "" {
			t.Fatalf("synthesis not populated: %+v", finding.Synthesis)
		}
		if _, err := os.Stat(finding.VerdictArtifact); err != nil {
			t.Fatalf("missing verdict json %s: %v", finding.VerdictArtifact, err)
		}
		mdPath := strings.TrimSuffix(finding.VerdictArtifact, ".json") + ".md"
		if _, err := os.Stat(mdPath); err != nil {
			t.Fatalf("missing verdict markdown %s: %v", mdPath, err)
		}
	}
	if rows := routeEvidenceRows(t, home); rows == 0 {
		t.Fatalf("route evidence ledger empty")
	}
	after := readTestFile(t, filepath.Join(home, "inbox", "memory-candidates", "mcand_example_api_clean.json"))
	if string(before) != string(after) {
		t.Fatalf("memory candidate changed during dry run")
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(got.ID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.Status != "verdicts_recorded" || len(loaded.Findings) != 2 {
		t.Fatalf("saved run = %+v", loaded)
	}
}

func TestExecuteDryRunFailSoftLens(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	got, err := ExecuteDryRun(context.Background(), home, run, failingLensCaller{base: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, lensID: "lens:duplicate-risk"}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if got.Status != "verdicts_recorded" || len(got.Findings) != 1 {
		t.Fatalf("run result = %+v", got)
	}
	foundFailed := false
	for _, lens := range got.Findings[0].LensFindings {
		if lens.LensRef == "lens:duplicate-risk" && lens.Status == "failed" && strings.Contains(lens.Error, "fixture lens failure") {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf("failed lens finding not recorded: %+v", got.Findings[0].LensFindings)
	}
}

func TestExecuteDryRunTerminalizesJudgeFailure(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	got, err := ExecuteDryRun(context.Background(), home, run, failingJudgeCaller{base: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}, fixedPanelNow)
	if err == nil {
		t.Fatalf("execute dry run succeeded; want judge failure")
	}
	if got.Status != "failed" || !strings.Contains(got.StoppedReason, "target mcand_example_api_clean") || !strings.Contains(got.StoppedReason, "fixture judge failure") {
		t.Fatalf("failed run state = status %q reason %q", got.Status, got.StoppedReason)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.Status != got.Status || loaded.StoppedReason != got.StoppedReason {
		t.Fatalf("saved failed run = status %q reason %q, want status %q reason %q", loaded.Status, loaded.StoppedReason, got.Status, got.StoppedReason)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("judge failure must not record incomplete target finding: %+v", got.Findings)
	}
	if rows := routeEvidenceRows(t, home); rows == 0 {
		t.Fatalf("judge failure should still leave route evidence")
	}
}

func TestExecuteDryRunMalformedJudgeNeedsHuman(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_malformed")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_malformed"}, Budget{})
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	synthesis := got.Findings[0].Synthesis
	if synthesis.Decision != "needs_human" || !containsString(synthesis.GuardrailsApplied, "judge_json_malformed") {
		t.Fatalf("synthesis = %+v, want needs_human with malformed guardrail", synthesis)
	}
	if rows := routeEvidenceRows(t, home); rows < 8 {
		t.Fatalf("route evidence rows = %d, want lens calls plus judge retry", rows)
	}
}

func TestExecuteDryRunBudgetStop(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, Budget{MaxItems: 1})
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if got.Status != "stopped_budget" || got.StoppedReason != "budget:max_items" || len(got.Findings) != 1 {
		t.Fatalf("budget run = %+v", got)
	}
}

func TestExecuteDryRunStoppedBudgetCannotResume(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, Budget{MaxItems: 1})
	first, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "stopped_budget" || len(first.Findings) != 1 {
		t.Fatalf("first run = %+v", first)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	resumed, err := ExecuteDryRun(context.Background(), home, first, caller, fixedPanelNow)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "stopped_budget" || resumed.StoppedReason != "budget:max_items" || len(resumed.Findings) != 1 {
		t.Fatalf("stopped run resumed past its original bound: %+v", resumed)
	}
	if caller.calls != 0 {
		t.Fatalf("stopped run spent %d additional provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunChargesPersistedRouteTokensBeforeResume(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 7})
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     requestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.lens",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.LensLane.ModelID,
		Usage:         routeevidence.Usage{TotalTokens: 7},
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	got, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped_budget" || got.StoppedReason != "budget:max_total_tokens" {
		t.Fatalf("persisted token spend did not stop resume: %+v", got)
	}
	if caller.calls != 0 {
		t.Fatalf("persisted token spend was charged only after %d new provider call(s)", caller.calls)
	}
}

func TestBuildPlanRejectsNegativeBudgetFields(t *testing.T) {
	tests := []struct {
		name   string
		budget Budget
	}{
		{name: "items", budget: Budget{MaxItems: -1}},
		{name: "tokens", budget: Budget{MaxTotalTokens: -1}},
		{name: "usd", budget: Budget{MaxUSDMicros: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := setupPanelPlanHome(t)
			writePanelFixtureTarget(t, home, "mcand_example_api_clean")
			if _, err := BuildPlan(home, PlanInput{
				TargetKind:            "memory_candidate",
				TargetRefs:            []string{"mcand_example_api_clean"},
				States:                []string{"parked"},
				PrivacyOverrideReason: "synthetic fixtures",
				Budget:                tc.budget,
			}, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "must be non-negative") {
				t.Fatalf("negative %s budget was accepted: %v", tc.name, err)
			}
		})
	}
}

func TestExecuteDryRunRejectsPersistedNegativeBudgetBeforeSpend(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	validInvocation := run
	run.Budget = Budget{MaxUSDMicros: 1, MaxTotalTokens: -1}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "panel-runs", run.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     requestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.lens",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.LensLane.ModelID,
		Usage:         routeevidence.Usage{TotalTokens: 7},
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, validInvocation, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "must be non-negative") {
		t.Fatalf("persisted negative budget did not fail validation: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("persisted negative budget was rejected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunRejectsLegacyUnknownSpendBeforeResume(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{
		MaxTotalTokens: 4000,
		MaxUSDMicros:   1_000_000,
	})
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     requestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.lens",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.LensLane.ModelID,
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "unknown token usage") {
		t.Fatalf("legacy route row with unknown spend did not fail closed: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("legacy unknown spend was detected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunRejectsLaterUnknownSpendBeforeAnyProviderCall(t *testing.T) {
	home := setupPanelPlanHome(t)
	const targetRef = "mcand_example_api_clean"
	writePanelFixtureTarget(t, home, targetRef)
	run := buildRunnablePlan(t, home, []string{targetRef}, Budget{})
	requestID := "panel:" + run.ID + ":" + targetDigest(targetRef) + ":" + judgePromptID
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     requestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.judge",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.JudgeLane.ModelID,
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "unknown token usage") {
		t.Fatalf("later judge row with unknown spend did not fail closed: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("later unknown spend was detected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunBlocksUnresolvedCallReservationBeforeSpend(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxItems: 2, MaxTotalTokens: 4000})
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + run.LensSet.Included[0].Ref
	writePanelCallAttemptFixture(t, home, run, "mcand_example_api_clean", requestID)

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	got, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
	if err == nil || !strings.Contains(err.Error(), "unresolved route attempt") {
		t.Fatalf("unresolved reservation did not fail closed: run=%+v err=%v", got, err)
	}
	if caller.calls != 0 {
		t.Fatalf("unresolved reservation was detected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunRejectsLaterUnresolvedJudgeReservationBeforeAnyProviderCall(t *testing.T) {
	home := setupPanelPlanHome(t)
	const targetRef = "mcand_example_api_clean"
	writePanelFixtureTarget(t, home, targetRef)
	run := buildRunnablePlan(t, home, []string{targetRef}, Budget{MaxTotalTokens: 4000})
	if _, err := reservePanelTargetAttempt(home, run, targetRef, fixedPanelNow()); err != nil {
		t.Fatal(err)
	}

	lensRef := run.LensSet.Included[0].Ref
	lensRequestID := "panel:" + run.ID + ":" + targetDigest(targetRef) + ":" + lensRef
	if err := reservePanelCallAttempt(home, run, targetRef, lensRequestID, "compound.panel.lens", lensRef, run.Route.LensLane, fixedPanelNow()); err != nil {
		t.Fatal(err)
	}
	lensRoute, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     lensRequestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.lens",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.LensLane.ModelID,
		Usage:         routeevidence.Usage{TotalTokens: 7},
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := routeevidence.RecordJoined(home, lensRoute)
	if err != nil {
		t.Fatal(err)
	}
	if err := completePanelCallAttempt(home, run, targetRef, lensRequestID, "compound.panel.lens", lensRef, run.Route.LensLane, joined.Evidence.ID, true, fixedPanelNow()); err != nil {
		t.Fatal(err)
	}

	judgeRequestID := "panel:" + run.ID + ":" + targetDigest(targetRef) + ":" + judgePromptID
	if err := reservePanelCallAttempt(home, run, targetRef, judgeRequestID, "compound.panel.judge", judgePromptID, run.Route.JudgeLane, fixedPanelNow()); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "unresolved route attempt") {
		t.Fatalf("later unresolved judge reservation did not fail closed: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("later unresolved judge reservation was detected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunGenericRouteCannotResolveCallReservation(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	writePanelCallAttemptFixture(t, home, run, "mcand_example_api_clean", requestID)
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     fixedPanelNow(),
		RequestID:     requestID,
		WorkID:        run.WorkID,
		ClientProfile: "compound.panel.lens",
		Protocol:      "panel-lane",
		Provider:      "compound-route-plan",
		Model:         run.Route.LensLane.ModelID,
		Usage:         routeevidence.Usage{TotalTokens: 7},
		Status:        "completed",
		EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "unresolved route attempt") {
		t.Fatalf("generic route evidence resolved a panel-owned reservation: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("generic route evidence permitted %d additional provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunRejectsIncompleteCallReservationIdentity(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + run.LensSet.Included[0].Ref
	attemptDir := filepath.Join(home, "panel-runs", "attempts", targetDigest(run.ID))
	if err := os.MkdirAll(attemptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	attempt, err := json.Marshal(map[string]string{
		"schema_version": panelAttemptSchemaVersion,
		"kind":           "call",
		"run_id":         run.ID,
		"target_ref":     "mcand_example_api_clean",
		"request_id":     requestID,
		"created_at":     fixedPanelNow().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "call-"+targetDigest(requestID)+".json"), append(attempt, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "incomplete panel call attempt identity") {
		t.Fatalf("incomplete reservation did not fail closed: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("incomplete reservation was rejected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunRejectsAmbiguousRouteRowsBeforeSpend(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	build := func(createdAt time.Time) routeevidence.Evidence {
		ev, err := routeevidence.Build(routeevidence.BuildInput{
			CreatedAt:     createdAt,
			RequestID:     requestID,
			WorkID:        run.WorkID,
			ClientProfile: "compound.panel.lens",
			Protocol:      "panel-lane",
			Provider:      "compound-route-plan",
			Model:         run.Route.LensLane.ModelID,
			Usage:         routeevidence.Usage{TotalTokens: 7},
			Status:        "completed",
			EvidenceRefs:  []string{routeevidence.ModelCapabilitiesAuthorityRef},
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	first := build(fixedPanelNow())
	if _, err := routeevidence.RecordJoined(home, first); err != nil {
		t.Fatal(err)
	}
	second := build(fixedPanelNow().Add(time.Second))
	data, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := os.OpenFile(filepath.Join(home, "ledger", "execution-route-evidence.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Write(append(data, '\n')); err != nil {
		_ = ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "ambiguous panel request") {
		t.Fatalf("duplicate request rows were not rejected: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("ambiguous rows were rejected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunFailsClosedWhenBoundedCallUsageIsUnknown(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	caller := &countingLensCaller{
		delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")},
		decorate: func(_ CallRequest, result *CallResult) {
			result.TotalTokens = 0
		},
	}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "unknown token usage") {
		t.Fatalf("bounded call with unknown usage did not fail closed: %v", err)
	}
	if caller.calls != 1 {
		t.Fatalf("unknown usage was detected after %d provider calls, want 1", caller.calls)
	}
}

func TestExecuteDryRunRejectsNonCanonicalRunIDBeforeLocking(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	run.ID += " "
	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "invalid panel run id") {
		t.Fatalf("non-canonical run id was accepted: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("non-canonical run id reached %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunSerializesConcurrentExecutors(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	caller := &synchronizedCountingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}

	type result struct {
		run Run
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			got, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
			results <- result{run: got, err: err}
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil || got.run.Status != "verdicts_recorded" {
			t.Fatalf("concurrent executor %d: status=%q err=%v", i, got.run.Status, got.err)
		}
	}
	wantCalls := len(run.LensSet.Included) + 1
	if got := caller.CallCount(); got != wantCalls {
		t.Fatalf("concurrent executors made %d provider calls, want one run's %d", got, wantCalls)
	}
}

func TestExecuteDryRunResumeSkipsExistingVerdictArtifact(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, Budget{MaxTotalTokens: 4000})
	if _, err := reviewartifact.Write(home, "panel-verdicts", verdictArtifactID(run.ID, "mcand_example_api_clean"), map[string]string{"id": "preexisting"}, " ", "preexisting\n"); err != nil {
		t.Fatalf("write preexisting verdict: %v", err)
	}
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if got.Status != "verdicts_recorded" || len(got.Findings) != 2 {
		t.Fatalf("resume run = %+v", got)
	}
	if got.Findings[0].TargetRef != "mcand_example_api_clean" || len(got.Findings[0].LensFindings) == 0 || got.Findings[0].LensFindings[0].Status != "skipped" {
		t.Fatalf("first finding not skipped: %+v", got.Findings[0])
	}
}

func TestExecuteDryRunReJudgesTargetFromDifferentRun(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	if _, err := reviewartifact.Write(home, "panel-verdicts", verdictArtifactID("prun_other_run", "mcand_example_api_clean"), map[string]string{"id": "stale"}, " ", "stale\n"); err != nil {
		t.Fatalf("write stale verdict: %v", err)
	}
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].Synthesis.Decision == "" || got.Findings[0].LensFindings[0].Status == "skipped" {
		t.Fatalf("target was not re-judged despite artifact from another run: %+v", got.Findings)
	}
}

func TestExecuteDryRunSelfJudgmentGuardrail(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	run.Route.JudgeLane.ModelID = run.Route.LensLane.ModelID
	saveRunnablePlan(t, home, run)
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	syn := got.Findings[0].Synthesis
	if syn.Decision == "promote" {
		t.Fatalf("self-judgment must not promote: %+v", syn)
	}
	found := false
	for _, g := range syn.GuardrailsApplied {
		if g == "self_judgment" {
			found = true
		}
	}
	if !found {
		t.Fatalf("self_judgment guardrail missing: %+v", syn.GuardrailsApplied)
	}
}

func TestTotalRunTokensIncludesJudgeUsage(t *testing.T) {
	run := Run{Findings: []TargetFinding{{
		LensFindings: []LensFinding{{Usage: Usage{TotalTokens: 10}}, {Usage: Usage{TotalTokens: 5}}},
		Synthesis:    Synthesis{JudgeUsage: Usage{TotalTokens: 7}},
	}}}
	if got := totalRunTokens(run); got != 22 {
		t.Fatalf("totalRunTokens = %d, want 22 (judge usage must count)", got)
	}
}

func buildRunnablePlan(t *testing.T, home string, refs []string, budget Budget) Run {
	t.Helper()
	run, err := BuildPlan(home, PlanInput{TargetKind: "memory_candidate", TargetRefs: refs, States: []string{"parked"}, PrivacyOverrideReason: "synthetic fixtures", Budget: budget}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build runnable plan: %v", err)
	}
	return run
}

func saveRunnablePlan(t *testing.T, home string, run Run) {
	t.Helper()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
}

func writePanelCallAttemptFixture(t *testing.T, home string, run Run, targetRef, requestID string) {
	t.Helper()
	lane := run.Route.LensLane
	attempt, err := json.Marshal(panelAttempt{
		SchemaVersion: panelAttemptSchemaVersion,
		Kind:          "call",
		RunID:         run.ID,
		WorkID:        panelStringRef(run.WorkID),
		TargetRef:     targetRef,
		RequestID:     requestID,
		ClientProfile: "compound.panel.lens",
		CallRole:      run.LensSet.Included[0].Ref,
		Lane:          &lane,
		CreatedAt:     fixedPanelNow().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := panelCallAttemptPath(home, run.ID, requestID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(attempt, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixedPanelNow() time.Time { return panelTestNow }

func routeEvidenceRows(t *testing.T, home string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func TestRecordPanelRouteEvidenceIncludesGatewayMetadata(t *testing.T) {
	home := t.TempDir()
	lane := LaneRef{ModelID: "auto/judge", Tier: "cheap"}
	claim, err := claimPanelRouteRequest(home, "work_1", "panel:req_1", "compound.panel.judge", lane)
	if err != nil {
		t.Fatalf("claim route request: %v", err)
	}
	_, err = recordPanelRouteEvidence(home, fixedPanelNow(), "work_1", "panel:req_1", "compound.panel.judge", lane, claim, CallResult{
		TotalTokens:      7,
		LatencyMS:        12,
		ResolvedProvider: "nvidia",
		ResolvedModel:    "nvidia/minimaxai/minimax-m3",
		ResolutionSource: "gateway_route",
		GatewayRequestID: "req_switchback_1",
	}, "completed")
	if err != nil {
		t.Fatalf("record route evidence: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))
	if err != nil {
		t.Fatalf("read route evidence ledger: %v", err)
	}
	var row routeevidence.Evidence
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &row); err != nil {
		t.Fatalf("parse route evidence row: %v", err)
	}
	if row.SelectedTarget.Model != "auto/judge" {
		t.Fatalf("selected target model = %q, want requested alias", row.SelectedTarget.Model)
	}
	if row.ResolvedProvider != "nvidia" || row.ResolvedModel != "nvidia/minimaxai/minimax-m3" || row.ResolutionSource != "gateway_route" || row.GatewayRequestID != "req_switchback_1" {
		t.Fatalf("gateway metadata = provider:%q resolved:%q source:%q request:%q", row.ResolvedProvider, row.ResolvedModel, row.ResolutionSource, row.GatewayRequestID)
	}
	if row.RecordedVia != routeevidence.RecorderGeneric {
		t.Fatalf("recorded_via = %q, want generic until an attested gateway adapter exists", row.RecordedVia)
	}
}

func TestExecuteDryRunBlocksAndPersistsPreflightFailure(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{})
	got, err := ExecuteDryRun(context.Background(), home, run, preflightBlockingCaller{err: fmt.Errorf("lm studio context preflight: model %q loaded_context_length=8192 below max_context_length=262144", "ornith-1.0-35b")}, fixedPanelNow)
	if err == nil {
		t.Fatal("expected preflight error")
	}
	if got.Status != "blocked" || !strings.Contains(got.StoppedReason, "loaded_context_length=8192") || !strings.Contains(got.NextAction, "lms ps") || len(got.Findings) != 0 {
		t.Fatalf("blocked run = %+v", got)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.Status != got.Status || loaded.StoppedReason != got.StoppedReason || loaded.NextAction != got.NextAction {
		t.Fatalf("saved run = %+v, want %+v", loaded, got)
	}
	if rows := routeEvidenceRows(t, home); rows != 0 {
		t.Fatalf("route evidence rows = %d, want no model call before preflight passes", rows)
	}
}

func TestExecuteDryRunRecordsCostUnknownForUSDCapWithoutCostMetadata(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	unbounded := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxUSDMicros: 1})
	if _, err := ExecuteDryRun(context.Background(), home, unbounded, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow); err == nil {
		t.Fatal("usd-only budget must fail closed without cost data")
	}

	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxUSDMicros: 1, MaxItems: 1})
	got, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if !containsString(got.EvidenceRefs, "cost_unknown") {
		t.Fatalf("run = %+v, want cost_unknown evidence when usd cap is bounded by items", got)
	}
}

func TestExecuteDryRunBlocksGatewayMeteredLaneWithoutUSDBudget(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxItems: 1})
	got, err := ExecuteDryRun(context.Background(), home, run, GatewayCaller{BaseURL: "http://127.0.0.1:1/v1"}, fixedPanelNow)
	if err == nil {
		t.Fatalf("execute dry run succeeded; want metered budget block")
	}
	if got.Status != "blocked" || got.StoppedReason != "budget:metered_lane_requires_max_usd_micros" {
		t.Fatalf("blocked run = status %q reason %q", got.Status, got.StoppedReason)
	}
	if !strings.Contains(got.NextAction, "--max-usd-micros") || !strings.Contains(got.NextAction, "--max-items or --max-tokens") {
		t.Fatalf("blocked run next_action = %q", got.NextAction)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.Status != got.Status || loaded.StoppedReason != got.StoppedReason || loaded.NextAction != got.NextAction {
		t.Fatalf("saved blocked run = status %q reason %q next %q, want status %q reason %q next %q", loaded.Status, loaded.StoppedReason, loaded.NextAction, got.Status, got.StoppedReason, got.NextAction)
	}
	if rows := routeEvidenceRows(t, home); rows != 0 {
		t.Fatalf("metered budget block should happen before route calls; route evidence rows = %d", rows)
	}
}

func TestExecuteDryRunRecordsANewRouteAttemptOnResume(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        fixedPanelNow(),
		RequestID:        requestID,
		WorkID:           run.WorkID,
		ClientProfile:    "compound.panel.lens",
		Protocol:         "panel-lane",
		Provider:         "compound-route-plan",
		Model:            run.Route.LensLane.ModelID,
		ResolvedProvider: "deepseek",
		ResolvedModel:    "deepseek/deepseek-v4-pro",
		ResolutionSource: "gateway_route",
		GatewayRequestID: "gateway-before-crash",
		Usage:            routeevidence.Usage{TotalTokens: 7},
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatalf("build route evidence: %v", err)
	}
	// Legacy rows predate recorded_via. They remain generic evidence and must
	// still allocate a fresh :resumeN request instead of being rejected.
	ev.RecordedVia = ""
	legacyRow, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, append(legacyRow, '\n'), 0o644); err != nil {
		t.Fatalf("preseed legacy route evidence: %v", err)
	}

	caller := &countingLensCaller{
		delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")},
		decorate: func(req CallRequest, result *CallResult) {
			result.ResolvedProvider = "deepseek"
			result.ResolvedModel = "deepseek/deepseek-v4-pro"
			result.ResolutionSource = "gateway_route"
			result.GatewayRequestID = "gateway-resume-" + req.LensID
		},
	}
	got, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute dry run: %v", err)
	}
	if got.Status != "verdicts_recorded" {
		t.Fatalf("run status = %s", got.Status)
	}
	if rows := routeEvidenceRequestRows(t, home, requestID); rows != 1 {
		t.Fatalf("request_id rows = %d, want 1", rows)
	}
	if rows := routeEvidenceRequestRows(t, home, requestID+":resume1"); rows != 1 {
		t.Fatalf("resume request_id rows = %d, want 1", rows)
	}
	if got.Findings[0].LensFindings[0].RouteEvidenceRef == ev.ID {
		t.Fatalf("resume reused stale route evidence %q for a new provider call", ev.ID)
	}
	if caller.calls == 0 {
		t.Fatal("resume did not execute the missing model output")
	}
}

func TestExecuteDryRunRejectsImportedRequestIDShadow(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	run.WorkID = "work_panel_import_shadow"
	saveRunnablePlan(t, home, run)
	lensRef := run.LensSet.Included[0].Ref
	requestID := "panel:" + run.ID + ":" + targetDigest("mcand_example_api_clean") + ":" + lensRef
	forged, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        fixedPanelNow(),
		RequestID:        requestID,
		WorkID:           run.WorkID,
		ClientProfile:    "compound.panel.lens",
		Protocol:         "panel-lane",
		Provider:         "compound-route-plan",
		Model:            run.Route.LensLane.ModelID,
		ResolvedProvider: "deepseek",
		ResolvedModel:    "deepseek/deepseek-v4-pro",
		ResolutionSource: "gateway_route",
		GatewayRequestID: "forged-gateway-request",
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "forged-panel-route.json")
	data, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportFile(home, path); err != nil {
		t.Fatal(err)
	}

	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "untrusted route evidence") {
		t.Fatalf("imported request-id shadow did not fail closed: %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("imported shadow was detected only after %d provider call(s)", caller.calls)
	}
}

func TestExecuteDryRunDoesNotSpendWhenRouteWriterWinsAfterCallAttemptReservation(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean"}, Budget{MaxTotalTokens: 4000})
	run.WorkID = "work_panel_cross_writer"
	saveRunnablePlan(t, home, run)

	lensRef := run.LensSet.Included[0].Ref
	targetRef := "mcand_example_api_clean"
	requestID := "panel:" + run.ID + ":" + targetDigest(targetRef) + ":" + lensRef
	caller := &gatedCountingLensCaller{
		delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")},
		release:  make(chan struct{}),
	}
	type executeResult struct {
		err error
	}
	done := make(chan executeResult, 1)

	err := homepkg.WithAutonomyAdmissionLock(home, func() error {
		go func() {
			_, executeErr := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
			done <- executeResult{err: executeErr}
		}()

		waitForPanelAttemptFile(t, panelCallAttemptPath(home, run.ID, requestID))
		return writeCanonicalCompetingRouteRow(home, run, targetRef, requestID, lensRef)
	})
	if err != nil {
		t.Fatalf("write competing route row under request-id authority: %v", err)
	}
	close(caller.release)
	result := <-done
	if got := caller.CallCount(); got != 0 {
		t.Fatalf("competing route writer was detected only after %d provider call(s): %v", got, result.err)
	}
	if result.err == nil {
		t.Fatal("competing route writer did not block panel execution")
	}
}

func waitForPanelAttemptFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect panel attempt %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("panel call attempt was not reserved at %s", path)
		}
		runtime.Gosched()
	}
}

func writeCanonicalCompetingRouteRow(home string, run Run, targetRef, requestID, lensRef string) error {
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        fixedPanelNow(),
		RequestID:        requestID,
		WorkID:           run.WorkID,
		ClientProfile:    "compound.panel.lens",
		Protocol:         "panel-lane",
		Provider:         "compound-route-plan",
		Model:            run.Route.LensLane.ModelID,
		ResolvedProvider: "deepseek",
		ResolvedModel:    "deepseek/deepseek-v4-pro",
		ResolutionSource: "gateway_route",
		GatewayRequestID: "competing-gateway-request-" + targetDigest(targetRef),
		Usage:            routeevidence.Usage{TotalTokens: 37},
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef, "lens:" + lensRef},
	})
	if err != nil {
		return err
	}
	ev.RecordedVia = routeevidence.RecorderExternalImport
	ev, _, err = routeevidence.JoinTransition(ev)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

type countingLensCaller struct {
	delegate LensCaller
	decorate func(CallRequest, *CallResult)
	calls    int
}

type synchronizedCountingLensCaller struct {
	delegate LensCaller
	mu       sync.Mutex
	calls    int
}

type gatedCountingLensCaller struct {
	delegate LensCaller
	release  chan struct{}
	mu       sync.Mutex
	calls    int
}

func (c *gatedCountingLensCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	select {
	case <-c.release:
	case <-ctx.Done():
		return CallResult{}, ctx.Err()
	}
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.delegate.Call(ctx, req)
}

func (c *gatedCountingLensCaller) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *synchronizedCountingLensCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.delegate.Call(ctx, req)
}

func (c *synchronizedCountingLensCaller) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingLensCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	c.calls++
	result, err := c.delegate.Call(ctx, req)
	if c.decorate != nil {
		c.decorate(req, &result)
	}
	return result, err
}

func routeEvidenceRequestRows(t *testing.T, home, requestID string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))
	if err != nil {
		t.Fatalf("read route evidence ledger: %v", err)
	}
	rows := 0
	for i, raw := range strings.Split(string(data), string([]byte{10})) {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var row struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse route evidence line %d: %v", i+1, err)
		}
		if row.RequestID == requestID {
			rows++
		}
	}
	return rows
}
