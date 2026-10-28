package panel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
)

func TestLoadSkillCandidateTargetsSkipsDecisionSidecarAndLimit(t *testing.T) {
	home := t.TempDir()
	writeSkillReviewForPanelTest(t, home, "scr_a_available")
	writeSkillReviewForPanelTest(t, home, "scr_b_skipped")
	writeSkillReviewForPanelTest(t, home, "scr_c_available")
	writeJSONFileForPanelTest(t, filepath.Join(home, "reviews", "skill-candidates", "scr_b_skipped.decision_scr_b_skipped.decision.json"), map[string]any{"id": "sidecar"})

	got, err := LoadSkillCandidateTargets(home, 1)
	if err != nil {
		t.Fatalf("load skill targets: %v", err)
	}
	if len(got) != 1 || got[0].Ref != "scr_a_available" || got[0].Kind != "skill_candidate" || got[0].Visibility != "internal" {
		t.Fatalf("limited skill targets = %+v", got)
	}
	all, err := LoadSkillCandidateTargets(home, 0)
	if err != nil {
		t.Fatalf("load all skill targets: %v", err)
	}
	if refs := targetRefsForPanelTest(all); strings.Join(refs, ",") != "scr_a_available,scr_c_available" {
		t.Fatalf("skill target refs = %v", refs)
	}
	if !strings.Contains(all[0].Summary, "classification=skill_candidate") || !strings.Contains(all[0].Summary, "overlaps=skill:existing-reference") {
		t.Fatalf("skill summary = %q", all[0].Summary)
	}
}

func TestLoadDecisionCandidateTargetsFiltersNonParkAndLimit(t *testing.T) {
	home := t.TempDir()
	writeDecisionCandidateForPanelTest(t, home, "dc_a_available", candidates.Park)
	writeDecisionCandidateForPanelTest(t, home, "dc_b_rejected", candidates.Reject)
	writeDecisionCandidateForPanelTest(t, home, "dc_c_available", candidates.Park)

	got, err := LoadDecisionCandidateTargets(home, 1)
	if err != nil {
		t.Fatalf("load limited decision targets: %v", err)
	}
	if len(got) != 1 || got[0].Ref != "dc_a_available" || got[0].Kind != "decision_candidate" || got[0].Visibility != "internal" {
		t.Fatalf("limited decision targets = %+v", got)
	}

	all, err := LoadDecisionCandidateTargets(home, 0)
	if err != nil {
		t.Fatalf("load all decision targets: %v", err)
	}
	if refs := targetRefsForPanelTest(all); strings.Join(refs, ",") != "dc_a_available,dc_c_available" {
		t.Fatalf("decision target refs = %v", refs)
	}
	if !strings.Contains(all[0].Summary, "scope=routing") || !strings.Contains(all[0].Summary, "verdict=park") {
		t.Fatalf("decision summary = %q", all[0].Summary)
	}

	input, err := LoadTargetInput(home, "decision_candidate", "dc_a_available")
	if err != nil {
		t.Fatalf("load decision target input: %v", err)
	}
	if input.Ref != "dc_a_available" || !strings.Contains(string(input.Payload), `"falsifier"`) {
		t.Fatalf("decision target input = %+v payload=%s", input, string(input.Payload))
	}
}

func TestLoadCapabilityObservationTargetsFiltersVerdictsAndLimit(t *testing.T) {
	home := t.TempDir()
	writeCapabilityObservationForPanelTest(t, home, "cobs_a_pending", "")
	writeCapabilityObservationForPanelTest(t, home, "cobs_b_operationalized", candidates.Operationalize)
	writeCapabilityObservationForPanelTest(t, home, "cobs_c_parked", candidates.Park)
	writeCapabilityObservationForPanelTest(t, home, "cobs_d_pending", "")

	got, err := LoadCapabilityObservationTargets(home, 1)
	if err != nil {
		t.Fatalf("load capability targets: %v", err)
	}
	if len(got) != 1 || got[0].Ref != "cobs_a_pending" || got[0].Kind != "capability_observation" {
		t.Fatalf("limited capability targets = %+v", got)
	}
	all, err := LoadCapabilityObservationTargets(home, 0)
	if err != nil {
		t.Fatalf("load all capability targets: %v", err)
	}
	if refs := targetRefsForPanelTest(all); strings.Join(refs, ",") != "cobs_a_pending,cobs_d_pending" {
		t.Fatalf("capability refs = %v", refs)
	}
}

func TestBuildPlanNewTargetKindsPrivacyDefaults(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeSkillReviewForPanelTest(t, home, "scr_panel_synthetic_promote")
	writeCapabilityObservationFixtureForPanelTest(t, home, "cobs_panel_synthetic_park")
	writeDecisionCandidateForPanelTest(t, home, "dc_panel_review", candidates.Park)

	skillRun, err := BuildPlan(home, PlanInput{TargetKind: "skill_candidate", TargetRefs: []string{"scr_panel_synthetic_promote"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build skill plan: %v", err)
	}
	if !containsString(skillRun.EvidenceRefs, "privacy_default:not_sensitive:skill_candidate") {
		t.Fatalf("skill privacy evidence = %v", skillRun.EvidenceRefs)
	}
	if len(skillRun.LensSet.Included) != 5 {
		t.Fatalf("skill lenses = %d, want 5: %+v", len(skillRun.LensSet.Included), skillRun.LensSet.Included)
	}

	capRun, err := BuildPlan(home, PlanInput{TargetKind: "capability_observation", TargetRefs: []string{"cobs_panel_synthetic_park"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build capability plan: %v", err)
	}
	if !containsString(capRun.EvidenceRefs, "privacy_default:not_sensitive:capability_observation") {
		t.Fatalf("capability privacy evidence = %v", capRun.EvidenceRefs)
	}
	if len(capRun.LensSet.Included) != 4 || capRun.CountsBefore["awaiting_verdict"] != 1 {
		t.Fatalf("capability plan lenses/counts = %d/%v", len(capRun.LensSet.Included), capRun.CountsBefore)
	}

	decisionRun, err := BuildPlan(home, PlanInput{TargetKind: "decision_candidate", TargetRefs: []string{"dc_panel_review"}, Domain: "reasoning-lenses", Task: "review falsifier and second-order decision effects"}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build decision plan: %v", err)
	}
	if !containsString(decisionRun.EvidenceRefs, "privacy_default:not_sensitive:decision_candidate") {
		t.Fatalf("decision privacy evidence = %v", decisionRun.EvidenceRefs)
	}
	if len(decisionRun.Targets) != 1 || decisionRun.Targets[0].StateBefore != candidates.Park {
		t.Fatalf("decision targets = %+v", decisionRun.Targets)
	}
	if len(decisionRun.LensSet.Included) != 7 || !includedLensRefsForPanelTest(decisionRun.LensSet.Included)["lens:decision-falsifier-review"] {
		t.Fatalf("decision plan lenses = %d/%+v", len(decisionRun.LensSet.Included), decisionRun.LensSet.Included)
	}
}

func TestApplySkillCandidateMappingAndFailSoft(t *testing.T) {
	home := setupPanelPlanHome(t)
	cases := []struct {
		id          string
		decision    string
		rationale   string
		wantMapped  string
		wantApplied bool
		wantSidecar bool
	}{
		{id: "scr_skill_promote", decision: "promote", rationale: "create candidate", wantMapped: "create_candidate", wantApplied: true, wantSidecar: true},
		{id: "scr_skill_merge", decision: "merge_existing", rationale: "merge existing", wantMapped: "merge_existing", wantApplied: true, wantSidecar: true},
		{id: "scr_skill_reject", decision: "reject", rationale: "reject candidate", wantMapped: "reject", wantApplied: true, wantSidecar: true},
		{id: "scr_skill_park", decision: "park", rationale: "keep needs review", wantMapped: "keep_needs_review", wantApplied: true, wantSidecar: true},
		{id: "scr_skill_needs_human", decision: "needs_human", rationale: "human needed", wantMapped: "needs_human", wantApplied: false},
		{id: "scr_skill_fail_soft", decision: "promote", rationale: "", wantMapped: "create_candidate", wantApplied: false},
	}
	run := newPanelApplyRunForTest("skill_candidate")
	for _, tc := range cases {
		writeSkillReviewForPanelTest(t, home, tc.id)
		run.Targets = append(run.Targets, Target{Ref: tc.id, Privacy: "internal", StateBefore: "review_required"})
		run.Findings = append(run.Findings, TargetFinding{TargetRef: tc.id, VerdictArtifact: filepath.Join(home, "reviews", "panel-verdicts", tc.id+".json"), Synthesis: Synthesis{SchemaVersion: SynthesisSchemaVersion, Decision: tc.decision, Confidence: 0.86, Rationale: tc.rationale}})
	}
	now := incrementingPanelNowForTest()
	got, err := Apply(home, run, now)
	if err != nil {
		t.Fatalf("apply skill candidates: %v", err)
	}
	byID := findingsByRefForPanelTest(got.Findings)
	for _, tc := range cases {
		app := byID[tc.id].Application
		if app.MappedVerdict != tc.wantMapped || app.Applied != tc.wantApplied {
			t.Fatalf("%s app = %+v, want mapped=%s applied=%t", tc.id, app, tc.wantMapped, tc.wantApplied)
		}
		if hasDecisionSidecarForPanelTest(home, tc.id) != tc.wantSidecar {
			t.Fatalf("%s sidecar existence mismatch", tc.id)
		}
	}
	if _, err := os.Stat(filepath.Join(home, candidates.InboxDir(candidates.Skill), "skill:panel-synthetic-review.json")); err != nil {
		t.Fatalf("promote did not create skill candidate: %v", err)
	}
	if !strings.Contains(byID["scr_skill_fail_soft"].Application.Note, "record skill candidate decision failed") {
		t.Fatalf("fail-soft note = %q", byID["scr_skill_fail_soft"].Application.Note)
	}
	again, err := Apply(home, got, now)
	if err != nil {
		t.Fatalf("reapply skill candidates: %v", err)
	}
	if again.Status != "applied" {
		t.Fatalf("reapply status = %s", again.Status)
	}
}

func TestApplyCapabilityObservationMapping(t *testing.T) {
	home := setupPanelPlanHome(t)
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{ID: "skill:cap_promote", Kind: "skill", Name: "Cap Promote", Triggers: []string{"panel"}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id          string
		decision    string
		subject     string
		wantVerdict string
		wantApplied bool
	}{
		{id: "cobs_cap_promote", decision: "promote", subject: "skill:cap_promote", wantVerdict: candidates.Operationalize, wantApplied: true},
		{id: "cobs_cap_park", decision: "park", subject: "skill:cap_park", wantVerdict: candidates.Park, wantApplied: true},
		{id: "cobs_cap_reject", decision: "reject", subject: "skill:cap_reject", wantVerdict: candidates.Park, wantApplied: true},
		{id: "cobs_cap_needs_human", decision: "needs_human", subject: "skill:cap_human", wantVerdict: "", wantApplied: false},
	}
	run := newPanelApplyRunForTest("capability_observation")
	for _, tc := range cases {
		writeCapabilityObservationForPanelTest(t, home, tc.id, "", tc.subject)
		run.Targets = append(run.Targets, Target{Ref: tc.id, Privacy: "internal", StateBefore: "awaiting_verdict"})
		run.Findings = append(run.Findings, TargetFinding{TargetRef: tc.id, VerdictArtifact: filepath.Join(home, "reviews", "panel-verdicts", tc.id+".json"), Synthesis: Synthesis{SchemaVersion: SynthesisSchemaVersion, Decision: tc.decision, Confidence: 0.86, Rationale: "capability mapping"}})
	}
	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply capability observations: %v", err)
	}
	byID := findingsByRefForPanelTest(got.Findings)
	for _, tc := range cases {
		app := byID[tc.id].Application
		if app.MappedVerdict != tc.decision || app.Applied != tc.wantApplied {
			t.Fatalf("%s app = %+v", tc.id, app)
		}
		var obs capabilityObservationTarget
		if err := candidates.ReadCandidate(home, candidates.CapabilityObs, tc.id, &obs); err != nil {
			t.Fatal(err)
		}
		if obs.Verdict != tc.wantVerdict {
			t.Fatalf("%s verdict = %q, want %q", tc.id, obs.Verdict, tc.wantVerdict)
		}
	}
	if edges := readPanelCapabilityEdges(t, home); !strings.Contains(string(edges), "skill:cap_promote") {
		t.Fatalf("capability edge not consumed:\n%s", string(edges))
	}
	if lines := countLines(t, filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); lines != 2 {
		t.Fatalf("no-promote rows = %d, want 2", lines)
	}
}

func TestPanelFixtureE2ESkillCandidate(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeSkillReviewFixtureForPanelTest(t, home, "scr_panel_synthetic_promote")
	run, err := BuildPlan(home, PlanInput{TargetKind: "skill_candidate", TargetRefs: []string{"scr_panel_synthetic_promote"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build skill plan: %v", err)
	}
	completed, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("dry-run skill: %v", err)
	}
	applied, err := Apply(home, completed, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply skill: %v", err)
	}
	if applied.Status != "applied" || !hasDecisionSidecarForPanelTest(home, "scr_panel_synthetic_promote") {
		t.Fatalf("skill e2e not applied: status=%s", applied.Status)
	}
	again, err := Apply(home, applied, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("reapply skill e2e: %v", err)
	}
	if again.Findings[0].Application.Note != "already applied; skipped" {
		t.Fatalf("reapply note = %q", again.Findings[0].Application.Note)
	}
}

func TestPanelFixtureE2ECapabilityObservation(t *testing.T) {
	home := setupPanelPlanHome(t)
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{ID: "skill:panel-capability-promote", Kind: "skill", Name: "Panel Capability Promote", Triggers: []string{"panel"}}); err != nil {
		t.Fatal(err)
	}
	writeCapabilityObservationFixtureForPanelTest(t, home, "cobs_panel_synthetic_promote")
	run, err := BuildPlan(home, PlanInput{TargetKind: "capability_observation", TargetRefs: []string{"cobs_panel_synthetic_promote"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build capability plan: %v", err)
	}
	completed, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("dry-run capability: %v", err)
	}
	applied, err := Apply(home, completed, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply capability: %v", err)
	}
	if applied.Status != "applied" || !strings.Contains(string(readPanelCapabilityEdges(t, home)), "skill:panel-capability-promote") {
		t.Fatalf("capability e2e did not consume edge")
	}
}

func TestSynthesizeDefaultQuorumCapsAtUsableLensCount(t *testing.T) {
	outputs := []LensOutput{
		{Lens: Lens{NodeID: "lens:duplicate-risk"}, Confidence: 0.8, Findings: []Finding{{Claim: "ok", Severity: "low"}}},
		{Lens: Lens{NodeID: "lens:evidence-strength"}, Confidence: 0.82, Findings: []Finding{{Claim: "ok", Severity: "low"}}},
		{Lens: Lens{NodeID: "lens:fossilization-risk"}, Err: os.ErrNotExist},
	}
	got := Synthesize(outputs, JudgeOutput{Decision: "promote", Confidence: 0.9, Rationale: "ok"}, Guardrails{})
	if got.Decision != "promote" || containsString(got.GuardrailsApplied, "lens_quorum_not_met") {
		t.Fatalf("default quorum over-parked: %+v", got)
	}
	memoryOutputs := successfulLensOutputs(0.8, 0.81, 0.82, 0.83, 0.84, 0.85)
	got = Synthesize(memoryOutputs, JudgeOutput{Decision: "promote", Confidence: 0.9, Rationale: "ok"}, Guardrails{})
	if got.Decision != "promote" {
		t.Fatalf("six-lens default should keep quorum 3 and allow promote: %+v", got)
	}
}

func writeSkillReviewFixtureForPanelTest(t *testing.T, home, id string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "panel", "targets", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "reviews", "skill-candidates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSkillReviewForPanelTest(t *testing.T, home, id string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "panel", "targets", "scr_panel_synthetic_promote.json"))
	if err != nil {
		t.Fatal(err)
	}
	var review map[string]any
	if err := json.Unmarshal(data, &review); err != nil {
		t.Fatal(err)
	}
	review["id"] = id
	review["candidate_id"] = "skill:panel-synthetic-review"
	ext := review["external"].(map[string]any)
	ext["id"] = "external." + id
	ext["name"] = "Panel Synthetic " + id
	writeJSONFileForPanelTest(t, filepath.Join(home, "reviews", "skill-candidates", id+".json"), review)
}

func writeCapabilityObservationFixtureForPanelTest(t *testing.T, home, id string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "panel", "targets", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var obs capabilityObservationTarget
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatal(err)
	}
	if err := candidates.WriteCandidate(home, candidates.CapabilityObs, id, obs); err != nil {
		t.Fatal(err)
	}
}

func writeCapabilityObservationForPanelTest(t *testing.T, home, id, verdict string, subjectOverride ...string) {
	t.Helper()
	subject := "skill:" + id
	if len(subjectOverride) > 0 {
		subject = subjectOverride[0]
	}
	obs := capabilityObservationTarget{ID: id, Kind: string(candidates.CapabilityObs), Subject: subject, Statement: "panel test context", Detail: "missed_when", Confidence: 0.8, EvidenceRefs: []string{"trace://panel/test"}, Verdict: verdict}
	if err := candidates.WriteCandidate(home, candidates.CapabilityObs, id, obs); err != nil {
		t.Fatal(err)
	}
}

func writeDecisionCandidateForPanelTest(t *testing.T, home, id, verdict string) {
	t.Helper()
	target := decisionCandidateTarget{
		ID:         id,
		Kind:       string(candidates.Decision),
		Subject:    "decision:routing-frontier-judge",
		Statement:  "Use paid frontier lanes only for final judgment when cheaper lanes already produced evidence.",
		Detail:     "Keeps model cost bounded while preserving quality gates.",
		Confidence: 0.84,
		Qualifiers: map[string]string{
			"scope":          "routing",
			"owner":          "compound",
			"falsifier":      "cheap lanes repeatedly miss material defects",
			"review_trigger": "after three route-evidence regressions",
		},
		EvidenceRefs: []string{"trace://panel/decision", "receipt://panel/decision"},
		Verdict:      verdict,
	}
	if err := candidates.WriteCandidate(home, candidates.Decision, id, target); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFileForPanelTest(t *testing.T, path string, payload any) {
	t.Helper()
	data, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, byte(10)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func targetRefsForPanelTest(inputs []TargetInput) []string {
	refs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		refs = append(refs, input.Ref)
	}
	return refs
}

func includedLensRefsForPanelTest(included []IncludedLens) map[string]bool {
	refs := make(map[string]bool, len(included))
	for _, lens := range included {
		refs[lens.Ref] = true
	}
	return refs
}

func newPanelApplyRunForTest(targetKind string) Run {
	return Run{SchemaVersion: RunSchemaVersion, ID: "prun_20260701T000000Z_" + targetKind, CreatedAt: "2026-07-01T00:00:00Z", Status: "verdicts_recorded", TargetKind: targetKind, Supervised: true, Supervision: "operator", Targets: []Target{}, LensSet: LensSet{Included: []IncludedLens{}}, Budget: Budget{}, Findings: []TargetFinding{}, CountsBefore: map[string]int{}, CountsAfter: map[string]int{}, EvidenceRefs: []string{"test"}}
}

func incrementingPanelNowForTest() func() time.Time {
	tick := 0
	return func() time.Time {
		tick++
		return time.Date(2026, 7, 1, 0, 0, tick, 0, time.UTC)
	}
}

func findingsByRefForPanelTest(findings []TargetFinding) map[string]TargetFinding {
	out := map[string]TargetFinding{}
	for _, finding := range findings {
		out[finding.TargetRef] = finding
	}
	return out
}

func hasDecisionSidecarForPanelTest(home, id string) bool {
	matches, err := filepath.Glob(filepath.Join(home, "reviews", "skill-candidates", id+".*.decision.json"))
	return err == nil && len(matches) > 0
}

func readPanelCapabilityEdges(t *testing.T, home string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "registries", "capability-edges.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
