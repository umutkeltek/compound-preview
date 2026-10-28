package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/mem"
	autonomyproof "compound/internal/routeevidence/autonomy"
)

func ensureJobHealthAutonomyPlan(t *testing.T, s *Store) autonomyproof.RoutePlanSnapshot {
	t.Helper()
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, "registries", "model-capabilities.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	return autonomyproof.RoutePlanSnapshot{
		ID:               autonomyproof.RoutePlanID("judge-critique", false, path, revision),
		AuthorityPath:    path,
		SourceRevision:   revision,
		CapabilityClass:  "judge-critique",
		PrivacySensitive: false,
		Chain: []autonomyproof.LaneSnapshot{
			{ModelID: "auto/judge", ProviderRoute: "auto/judge", Resolution: autonomyproof.LaneResolution{Mode: "adaptive"}, Tier: "cheap", ClaimLevel: "primary", GateRole: "eligible"},
			{ModelID: "deepseek/deepseek-v4-pro", ProviderRoute: "deepseek/deepseek-v4-pro", Resolution: autonomyproof.LaneResolution{Mode: "direct", Targets: []autonomyproof.ResolvedTarget{{Provider: "deepseek", Model: "deepseek/deepseek-v4-pro"}}}, Tier: "cheap", ClaimLevel: "primary", GateRole: "eligible"},
			{ModelID: "scout/chat", ProviderRoute: "scout/chat", Resolution: autonomyproof.LaneResolution{Mode: "direct", Targets: []autonomyproof.ResolvedTarget{{Provider: "scout", Model: "scout/chat"}}}, Tier: "free", ClaimLevel: "capable", GateRole: "tripwire_only"},
		},
	}
}

func typedJobHealthEligibility(t *testing.T, s *Store, now time.Time) AutoAdmitInput {
	t.Helper()
	plan := ensureJobHealthAutonomyPlan(t, s)
	grantID := "job-health-autonomy-test"
	seedDecisionClaim(t, s, "grant", grantID, map[string]string{
		"scope":                "autonomy-evidence-threshold",
		"auto_admit_class":     "job_health",
		"min_terminal_samples": "2",
		"min_window_days":      "1",
		"max_failures":         "0",
	}, now.Add(-72*time.Hour))
	memStore, err := mem.Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := memStore.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	claimID := ""
	for _, claim := range claims {
		if claim.Qualifiers["decision_id"] == grantID {
			claimID = claim.ID
		}
	}
	if claimID == "" {
		t.Fatal("typed autonomy grant claim missing")
	}
	for index, offset := range []time.Duration{-48 * time.Hour, -24 * time.Hour} {
		seedTypedAutonomySample(t, s, plan, index, now.Add(offset))
	}
	artifact, err := autonomyproof.Compile(s.home, autonomyproof.GrantSnapshot{
		DecisionID:         grantID,
		ClaimID:            claimID,
		GrantRef:           "grant:" + grantID,
		ClaimRef:           "claim:" + claimID,
		ReviewedBy:         "operator:test",
		Class:              "job_health",
		MinTerminalSamples: 2,
		MinWindowDays:      1,
		MaxFailures:        0,
	}, plan, now)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Status != "eligible" {
		t.Fatalf("typed test eligibility is not eligible: %+v", artifact)
	}
	if _, err := autonomyproof.Record(s.home, artifact); err != nil {
		t.Fatal(err)
	}
	return AutoAdmitInput{By: "director", EligibilityRef: artifact.Ref()}
}

func seedTypedAutonomySample(t *testing.T, s *Store, plan autonomyproof.RoutePlanSnapshot, index int, closedAt time.Time) {
	t.Helper()
	id := idFor("work_sample_", plan.ID, string(rune('a'+index)), closedAt.UTC().Format(time.RFC3339))
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-authority",
		Actor:            "operator-test",
		SuccessPredicate: "typed autonomy historical sample",
		EvidenceRefs: []string{
			"proposed_by:director",
			"director_trigger:job_health:" + id,
			"route_plan:" + plan.ID,
		},
		RouteDecisionID: plan.ID,
		State:           "proposed",
	}, closedAt.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{Actor: "operator-test", To: "ready"}, closedAt.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "sample-executor", closedAt.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{Actor: "sample-executor", To: "running"}, closedAt.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	routeID := idFor("route_", w.ID)
	row := map[string]any{
		"schema_version": "compound.execution_route_evidence.v0.1",
		"id":             routeID,
		"created_at":     closedAt.Add(-time.Minute).UTC().Format(time.RFC3339),
		"request_id":     "request-" + w.ID,
		"work_id":        w.ID,
		"client_profile": "compound.panel.judge",
		"protocol":       "panel-lane",
		"recorded_via":   "trusted_panel_runtime",
		"selected_target": map[string]any{
			"provider": "compound-route-plan",
			"model":    "auto/judge",
			"protocol": "panel-lane",
		},
		"resolved_provider":  "deepseek",
		"resolved_model":     "deepseek/deepseek-v4-pro",
		"resolution_source":  "gateway_route",
		"gateway_request_id": "gateway-" + w.ID,
		"status":             "completed",
		"evidence_refs":      []string{"compound://registries/model-router"},
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(s.home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "sample-executor",
		Summary:   "typed autonomy sample completed",
		Changed:   true,
		ProofRefs: []string{routeID, "check:job-health-state-delta:" + w.ID},
	}, closedAt); err != nil {
		t.Fatal(err)
	}
}
