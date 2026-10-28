package panel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestApplyVerdictMappingAndProofChain(t *testing.T) {
	home := t.TempDir()
	writePolicyHoldFixture(t, home, "mcand_policy_hold")
	cases := []struct {
		id          string
		decision    string
		basis       string
		wantMapped  string
		wantState   string
		wantApplied bool
	}{
		{id: "mcand_promote_valid", decision: "promote", basis: candidates.BasisDecisionRecord, wantMapped: "promote", wantState: "promoted", wantApplied: true},
		{id: "mcand_promote_no_basis", decision: "promote", wantMapped: "park", wantState: "parked", wantApplied: true},
		{id: "mcand_park", decision: "park", basis: candidates.BasisRuntimeProof, wantMapped: "park", wantState: "parked", wantApplied: true},
		{id: "mcand_reject", decision: "reject", basis: candidates.BasisRuntimeProof, wantMapped: "reject", wantState: "rejected", wantApplied: true},
		{id: "mcand_merge", decision: "merge_existing", basis: candidates.BasisRuntimeProof, wantMapped: "merge_existing", wantState: "rejected", wantApplied: true},
		{id: "mcand_needs_human", decision: "needs_human", basis: candidates.BasisRuntimeProof, wantMapped: "needs_human", wantState: "parked", wantApplied: false},
		{id: "mcand_needs_trial", decision: "needs_trial", basis: candidates.BasisRuntimeProof, wantMapped: "needs_trial", wantState: "parked", wantApplied: false},
		{id: "mcand_policy_hold", decision: "park", basis: candidates.BasisRuntimeProof, wantMapped: "park", wantState: "parked", wantApplied: false},
	}
	findings := make([]TargetFinding, 0, len(cases))
	targets := make([]Target, 0, len(cases))
	for _, tc := range cases {
		writeMemoryCandidateRecord(t, home, tc.id, map[string]any{
			"id":               tc.id,
			"entity":           "service:panel-apply",
			"predicate":        "has_" + strings.TrimPrefix(tc.id, "mcand_"),
			"object":           "true",
			"claim_type":       "status",
			"confidence":       0.91,
			"promotion_basis":  tc.basis,
			"state":            "parked",
			"visibility":       "contextual",
			"derivation":       "deterministic",
			"source_trace_ref": "trace://" + tc.id,
			"snippet_hash":     "sha256:" + tc.id,
			"observation_date": "2026-07-01T00:00:00Z",
			"created_at":       "2026-07-01T10:00:00Z",
		})
		artifact := writeApplyVerdictArtifact(t, home, tc.id, tc.decision)
		findings = append(findings, TargetFinding{
			TargetRef:       tc.id,
			VerdictArtifact: artifact,
			Synthesis:       Synthesis{SchemaVersion: SynthesisSchemaVersion, Decision: tc.decision, Confidence: 0.9, Rationale: "synthetic " + tc.decision},
			LensFindings:    []LensFinding{{LensRef: "lens:test", Status: "completed"}},
		})
		targets = append(targets, Target{Ref: tc.id, Privacy: "contextual", StateBefore: "parked"})
	}
	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            "prun_20260701_apply",
		CreatedAt:     panelTestNow.Format(time.RFC3339),
		Status:        "verdicts_recorded",
		TargetKind:    "memory_candidate",
		Supervised:    true,
		Supervision:   "operator",
		Targets:       targets,
		Findings:      findings,
		CountsBefore:  map[string]int{"parked": len(cases)},
		CountsAfter:   map[string]int{"parked": len(cases)},
		EvidenceRefs:  []string{},
	}
	beforeClaims := countLines(t, filepath.Join(home, "ledger", "claims.jsonl"))
	now := incrementingPanelNow()
	got, err := Apply(home, run, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Status != "applied" || got.WorkID == "" || got.ReceiptID == "" || got.TransitionID == "" {
		t.Fatalf("apply proof refs not populated: %+v", got)
	}
	if got.CountsAfter["promoted"] != 1 || got.CountsAfter["rejected"] != 2 || got.CountsAfter["parked"] != 5 {
		t.Fatalf("counts_after = %+v", got.CountsAfter)
	}
	if countLines(t, filepath.Join(home, "ledger", "claims.jsonl")) != beforeClaims+1 {
		t.Fatalf("claims log did not grow by one")
	}
	if got.CountsAfter["parked"] != 5 {
		t.Fatalf("parked count = %d, want 5", got.CountsAfter["parked"])
	}
	if lines := countLines(t, filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); lines != 3 {
		t.Fatalf("no-promote rows = %d, want 3", lines)
	}
	byID := map[string]TargetFinding{}
	for _, finding := range got.Findings {
		byID[finding.TargetRef] = finding
	}
	for _, tc := range cases {
		finding := byID[tc.id]
		if finding.Application.MappedVerdict != tc.wantMapped || finding.Application.Applied != tc.wantApplied {
			t.Fatalf("%s application = %+v, want mapped=%s applied=%t", tc.id, finding.Application, tc.wantMapped, tc.wantApplied)
		}
		cand := readCandidateForTest(t, home, tc.id)
		if cand.State != tc.wantState {
			t.Fatalf("%s state = %q, want %q", tc.id, cand.State, tc.wantState)
		}
	}
	if byID["mcand_promote_valid"].Application.ClaimRef == "" || byID["mcand_promote_valid"].Application.TransitionRef == "" {
		t.Fatalf("promote application missing claim/transition: %+v", byID["mcand_promote_valid"].Application)
	}
	for _, id := range []string{"mcand_promote_no_basis", "mcand_park", "mcand_merge"} {
		if byID[id].Application.NoPromoteRef == "" {
			t.Fatalf("%s missing no-promote ref: %+v", id, byID[id].Application)
		}
	}
	if !strings.Contains(byID["mcand_policy_hold"].Application.Note, "policy") {
		t.Fatalf("policy hold note = %q", byID["mcand_policy_hold"].Application.Note)
	}
	assertApplyWorkProof(t, home, got, byID["mcand_promote_valid"].Application.TransitionRef)

	again, err := Apply(home, got, now)
	if err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if countLines(t, filepath.Join(home, "ledger", "claims.jsonl")) != beforeClaims+1 {
		t.Fatalf("reapply appended duplicate claim")
	}
	if lines := countLines(t, filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); lines != 3 {
		t.Fatalf("reapply no-promote rows = %d, want 3", lines)
	}
	if again.Status != "applied" || again.ReceiptID == "" || again.TransitionID == "" {
		t.Fatalf("reapply run = %+v", again)
	}
}

func writePolicyHoldFixture(t *testing.T, home, candidateID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("forbidden_paths:\n  - '**/" + candidateID + ".json'\n")
	if err := os.WriteFile(filepath.Join(home, "config", "policy.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeApplyVerdictArtifact(t *testing.T, home, targetID, decision string) string {
	t.Helper()
	path := filepath.Join(home, "reviews", "panel-verdicts", targetID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"target": targetID, "decision": decision}
	data, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertApplyWorkProof(t *testing.T, home string, run Run, memoryTransitionID string) {
	t.Helper()
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Get(run.WorkID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if w.State != "completed" || w.IDs.TransitionID != run.TransitionID {
		t.Fatalf("work proof = state %s transition %s, run transition %s", w.State, w.IDs.TransitionID, run.TransitionID)
	}
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{run.ReceiptID, run.TransitionID, memoryTransitionID} {
		if exists, err := events.Exists(id); err != nil || !exists {
			t.Fatalf("missing event envelope %s: exists=%v err=%v", id, exists, err)
		}
	}
	var receipt envelope.Receipt
	readJSONForTest(t, filepath.Join(home, "events", run.ReceiptID+".json"), &receipt)
	if receipt.MoveType != "memory_delta" || !containsString(receipt.Memory.Promoted, "mcand_promote_valid") || !containsString(receipt.Memory.Parked, "mcand_park") || !containsString(receipt.Memory.Rejected, "mcand_reject") {
		t.Fatalf("receipt memory delta = %+v move=%s", receipt.Memory, receipt.MoveType)
	}
}

func TestPanelApplyProofRefsIncludeRouteEvidence(t *testing.T) {
	run := Run{
		ID: "prun_route_refs",
		Findings: []TargetFinding{{
			VerdictArtifact: "panel_verdict_route_refs.json",
			LensFindings: []LensFinding{
				{RouteEvidenceRef: "route_lens_one"},
				{RouteEvidenceRef: "route_lens_two"},
				{RouteEvidenceRef: "route_lens_one"},
			},
			Synthesis: Synthesis{JudgeRouteEvidenceRef: "route_judge"},
		}},
	}

	got := panelApplyProofRefs(run, "/tmp/panel-run.json")
	for _, want := range []string{"/tmp/panel-run.json", "panel_verdict_route_refs.json", "route_lens_one", "route_lens_two", "route_judge"} {
		if !containsString(got, want) {
			t.Fatalf("proof refs = %v, missing %q", got, want)
		}
	}
}

func readCandidateForTest(t *testing.T, home, id string) mem.Candidate {
	t.Helper()
	var cand mem.Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &cand); err != nil {
		t.Fatal(err)
	}
	return cand
}

func readJSONForTest(t *testing.T, path string, target any) {
	t.Helper()
	var data []byte
	if filepath.Base(filepath.Dir(path)) == "events" && strings.HasSuffix(path, ".json") {
		store, err := eventstore.Open(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			t.Fatal(err)
		}
		data = record.Bytes
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func incrementingPanelNow() func() time.Time {
	var tick int
	return func() time.Time {
		tick++
		return panelTestNow.Add(time.Duration(tick) * time.Second)
	}
}

func TestApplyFailSoftTerminalizesEnvelopeOnTargetFailure(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")
	run := buildRunnablePlan(t, home, []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, Budget{})
	run, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// Break the second target after verdicts: apply must fail soft, not strand the run.
	if err := os.Remove(filepath.Join(home, "inbox", "memory-candidates", "mcand_example_api_dissent.json")); err != nil {
		t.Fatalf("remove candidate: %v", err)
	}
	got, err := Apply(home, run, fixedPanelNow)
	if err == nil || !strings.Contains(err.Error(), "target failure") {
		t.Fatalf("want completed-with-failures error, got %v", err)
	}
	if got.Status != "applied" {
		t.Fatalf("run must not strand at running: status=%q", got.Status)
	}
	if got.ReceiptID == "" || got.TransitionID == "" {
		t.Fatalf("work envelope must terminalize: receipt=%q transition=%q", got.ReceiptID, got.TransitionID)
	}
	var appliedOK, failedNoted bool
	for _, f := range got.Findings {
		if f.TargetRef == "mcand_example_api_clean" && f.Application.Applied {
			appliedOK = true
		}
		if f.TargetRef == "mcand_example_api_dissent" && !f.Application.Applied && f.Application.Note != "" {
			failedNoted = true
		}
	}
	if !appliedOK || !failedNoted {
		t.Fatalf("fail-soft accounting wrong: %+v", got.Findings)
	}
}

func TestApplyRetriesFailedComparisonApplications(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeComparisonCandidateForPanelTest(t, home, "cmp_retry_me", "awaiting_verdict", "lane/base", "lane/cand", 120, 80, 340, 220)
	run, err := BuildPlan(home, PlanInput{TargetKind: "comparison", TargetRefs: []string{"cmp_retry_me"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	// Simulate the stranded state observed live 2026-07-02: a prior apply
	// failed on every target yet the run was persisted as "applied" with a
	// closed work chain. Re-apply must retry the failed application instead
	// of skipping it forever.
	run.Status = "applied"
	run.Findings = []TargetFinding{{
		TargetRef:       "cmp_retry_me",
		VerdictArtifact: filepath.Join(home, "reviews", "panel-verdicts", "panel_verdict_retry_stub.json"),
		Synthesis:       Synthesis{SchemaVersion: "compound.panel_synthesis.v0.1", Decision: "promote", Confidence: 0.95, Rationale: "candidate strictly better"},
		Application:     Application{Applied: false, Note: "apply failed: invalid character '{' after top-level value"},
	}}
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatalf("save stranded run: %v", err)
	}

	got, err := Apply(home, run, fixedPanelNow)
	if err != nil {
		t.Fatalf("re-apply must succeed for retryable failed applications: %v", err)
	}
	if len(got.Findings) != 1 || !got.Findings[0].Application.Applied {
		t.Fatalf("failed application was not retried: %+v", got.Findings)
	}
	if got.Findings[0].Application.MappedVerdict != "prefer_candidate" {
		t.Fatalf("mapped verdict = %q, want prefer_candidate", got.Findings[0].Application.MappedVerdict)
	}
	exists, err := comparisonEvidenceRowExists(comparisonEvidenceLedgerPath(home), "cmp_retry_me", got.ID)
	if err != nil {
		t.Fatalf("ledger lookup: %v", err)
	}
	if !exists {
		t.Fatalf("comparison evidence row missing after retried apply")
	}
	// Second re-apply is a no-op: everything is genuinely applied now.
	again, err := Apply(home, got, fixedPanelNow)
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if !again.Findings[0].Application.Applied || again.Findings[0].Application.Note != "already applied; skipped" {
		t.Fatalf("second re-apply not idempotent: %+v", again.Findings[0].Application)
	}
}

// TestApplyRecordsRunAttemptChain proves panel.Apply mints exactly one RunAttempt per apply,
// routes it through Advance{RunAttemptIDs}, binds it to the authorizing ActionIntent, and
// terminalizes it succeeded on a clean apply. The Mint→Advance→AttachActionIntent→Complete
// chain is otherwise covered only indirectly (work.advance tests + runattempt package tests).
// Failed-status semantics are unit-covered by runattempt.TestStoreAttachActionIntentRejects and
// TestStoreComplete; this test pins the happy path end-to-end on disk.
func TestApplyRecordsRunAttemptChain(t *testing.T) {
	home := t.TempDir()
	candID := "mcand_runattempt_chain"
	writeMemoryCandidateRecord(t, home, candID, map[string]any{
		"id":               candID,
		"entity":           "service:panel-apply",
		"predicate":        "has_runattempt_chain",
		"object":           "true",
		"claim_type":       "status",
		"confidence":       0.91,
		"promotion_basis":  candidates.BasisDecisionRecord,
		"state":            "parked",
		"visibility":       "contextual",
		"derivation":       "deterministic",
		"source_trace_ref": "trace://" + candID,
		"snippet_hash":     "sha256:" + candID,
		"observation_date": "2026-07-01T00:00:00Z",
		"created_at":       "2026-07-01T10:00:00Z",
	})
	run := newMemoryPanelApplyRunForTest("runattempt_chain", candID)
	run.Findings = []TargetFinding{writeMemoryPanelFindingForTest(t, home, run, candID, "promote")}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.WorkID == "" {
		t.Fatalf("apply did not bind a work envelope")
	}

	// C1: claim mints one RunAttempt (pending); panel.Apply mints its own
	// execution RunAttempt (succeeded). Two entries total. Find the
	// execution one by status.
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatalf("read run_attempts: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("run_attempts entries = %d, want 2 (claim + execution)", len(entries))
	}
	raStore := runattempt.Open(home)
	var ra runattempt.RunAttempt
	for _, e := range entries {
		candidate, err := raStore.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			t.Fatalf("Get runattempt %s: %v", e.Name(), err)
		}
		if candidate.Status == runattempt.StatusSucceeded {
			ra = candidate
			break
		}
	}
	if ra.ID == "" {
		t.Fatal("no succeeded RunAttempt found among entries")
	}
	if ra.WorkID != got.WorkID {
		t.Fatalf("runattempt work_id = %q, want %q", ra.WorkID, got.WorkID)
	}
	if ra.Status != runattempt.StatusSucceeded {
		t.Fatalf("runattempt status = %q, want succeeded", ra.Status)
	}
	if ra.Harness != runattempt.HarnessPlaceholder {
		t.Fatalf("runattempt harness = %q, want %q", ra.Harness, runattempt.HarnessPlaceholder)
	}
	if !strings.HasPrefix(ra.ActionIntentID, "act_") {
		t.Fatalf("runattempt action_intent_id = %q, want a non-empty act_ id (audit chain unbound)", ra.ActionIntentID)
	}
	// S2 fields populated by panel.Apply as honest values from the apply context.
	if ra.Workspace != home {
		t.Fatalf("runattempt workspace = %q, want %q (home path)", ra.Workspace, home)
	}
	if ra.Command != "compound panel apply" {
		t.Fatalf("runattempt command = %q, want %q", ra.Command, "compound panel apply")
	}
	if ra.TranscriptRef == "" {
		t.Fatalf("runattempt transcript_ref must be set after a clean apply (the panel run record is the transcript)")
	}
	if len(ra.ProofRefs) == 0 {
		t.Fatalf("runattempt proof_refs must include at least the panel run record and the receipt/transition ids")
	}
	if !slices.Contains(ra.ProofRefs, got.ReceiptID) || !slices.Contains(ra.ProofRefs, got.TransitionID) {
		t.Fatalf("runattempt proof_refs = %v, want close proof %s and %s", ra.ProofRefs, got.ReceiptID, got.TransitionID)
	}
	if ra.TerminalizationIntent == nil || ra.TerminalizationIntent.Owner != runattempt.TerminalizationOwnerPanelApply || ra.TerminalizationIntent.TargetStatus != runattempt.StatusSucceeded {
		t.Fatalf("runattempt exact prepared terminalization = %+v, want panel-owned succeeded intent", ra.TerminalizationIntent)
	}
	if ra.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply || ra.TerminalizationReceiptID != got.ReceiptID || ra.TerminalizationTransitionID != got.TransitionID {
		t.Fatalf("runattempt terminalization proof = owner %q receipt %q transition %q, want panel-owned close proof", ra.TerminalizationOwner, ra.TerminalizationReceiptID, ra.TerminalizationTransitionID)
	}
	// F2: panel apply IS the execution, so its attempt is stamped
	// harness_execution immediately after Mint.
	if ra.Origin != runattempt.OriginHarnessExecution {
		t.Fatalf("runattempt origin = %q, want %q (stamped immediately after Mint)", ra.Origin, runattempt.OriginHarnessExecution)
	}
	// F2 twin: attaching the harness_execution attempt at the running-advance
	// supersedes the claim_reservation placeholder minted by Claim -- it must
	// not linger pending for the sweeper.
	for _, e := range entries {
		candidate, err := raStore.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			t.Fatalf("Get runattempt %s: %v", e.Name(), err)
		}
		if candidate.Origin == runattempt.OriginClaimReservation && candidate.Status != runattempt.StatusSuperseded {
			t.Fatalf("claim reservation status = %q, want superseded once the execution attempt attached", candidate.Status)
		}
	}
}

// TestApplyRecordsRunAttemptChainFailed is the failed-path companion to
// TestApplyRecordsRunAttemptChain: when a target fails soft, the RunAttempt is still minted (Mint
// runs before the apply loop) but terminalizes failed, and Apply returns the target-failure error.
// Together the two tests pin the Mint→Advance→AttachActionIntent→Complete chain for both outcomes.
func TestApplyRecordsRunAttemptChainFailed(t *testing.T) {
	home := t.TempDir()
	candID := "mcand_runattempt_fail"
	writeMemoryCandidateRecord(t, home, candID, map[string]any{
		"id":               candID,
		"entity":           "service:panel-apply",
		"predicate":        "has_runattempt_fail",
		"object":           "true",
		"claim_type":       "status",
		"confidence":       0.91,
		"promotion_basis":  candidates.BasisDecisionRecord,
		"state":            "parked",
		"visibility":       "contextual",
		"derivation":       "deterministic",
		"source_trace_ref": "trace://" + candID,
		"snippet_hash":     "sha256:" + candID,
		"observation_date": "2026-07-01T00:00:00Z",
		"created_at":       "2026-07-01T10:00:00Z",
	})
	run := newMemoryPanelApplyRunForTest("runattempt_fail", candID)
	run.Findings = []TargetFinding{writeMemoryPanelFindingForTest(t, home, run, candID, "promote")}
	// Remove the candidate after the finding is built so apply fails soft at readMemoryCandidate.
	if err := os.Remove(filepath.Join(home, "inbox", "memory-candidates", candID+".json")); err != nil {
		t.Fatalf("remove candidate: %v", err)
	}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err == nil || !strings.Contains(err.Error(), "target failure") {
		t.Fatalf("want completed-with-failures error, got %v", err)
	}
	if got.WorkID == "" {
		t.Fatalf("apply did not bind a work envelope even on failure")
	}

	// C1: claim mints one RunAttempt (pending); panel.Apply mints its own
	// execution RunAttempt (failed). Two entries total. Find the execution
	// one by status.
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatalf("read run_attempts: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("run_attempts entries = %d, want 2 (claim + execution)", len(entries))
	}
	raStore := runattempt.Open(home)
	var ra runattempt.RunAttempt
	for _, e := range entries {
		candidate, err := raStore.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			t.Fatalf("Get runattempt %s: %v", e.Name(), err)
		}
		if candidate.Status == runattempt.StatusFailed {
			ra = candidate
			break
		}
	}
	if ra.ID == "" {
		t.Fatal("no failed RunAttempt found among entries")
	}
	if ra.WorkID != got.WorkID {
		t.Fatalf("runattempt work_id = %q, want %q", ra.WorkID, got.WorkID)
	}
	if ra.Status != runattempt.StatusFailed {
		t.Fatalf("runattempt status = %q, want failed", ra.Status)
	}
	if ra.TerminalizationIntent == nil || ra.TerminalizationIntent.Owner != runattempt.TerminalizationOwnerPanelApply || ra.TerminalizationIntent.TargetStatus != runattempt.StatusFailed {
		t.Fatalf("failed runattempt exact prepared terminalization = %+v, want panel-owned failed intent", ra.TerminalizationIntent)
	}
	if ra.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply || ra.TerminalizationReceiptID != got.ReceiptID || ra.TerminalizationTransitionID != got.TransitionID {
		t.Fatalf("failed runattempt terminalization proof = owner %q receipt %q transition %q, want panel-owned close proof", ra.TerminalizationOwner, ra.TerminalizationReceiptID, ra.TerminalizationTransitionID)
	}
	if !strings.Contains(ra.TerminalizationReason, "1 target failure") {
		t.Fatalf("failed runattempt terminalization reason = %q, want target-failure count", ra.TerminalizationReason)
	}
	if !slices.Contains(ra.ProofRefs, got.ReceiptID) || !slices.Contains(ra.ProofRefs, got.TransitionID) {
		t.Fatalf("failed runattempt proof_refs = %v, want close proof %s and %s", ra.ProofRefs, got.ReceiptID, got.TransitionID)
	}
}

// TestApplyAbortTerminalizesAttemptAndClosesEnvelope pins the deferred
// failTerm-style guard: an error return AFTER Mint and the running-advance
// must neither strand the minted RunAttempt pending nor leave the
// WorkEnvelope stuck in running. The trigger is a TargetKind that
// countPanelTargetStates rejects -- a deterministic error return late in
// Apply (after every store write up to the apply loop succeeded) that needs
// no fault-injection seam. Falsifier: before the guard, this exact path left
// the attempt pending forever and the envelope running.
func TestApplyAbortTerminalizesAttemptAndClosesEnvelope(t *testing.T) {
	home := t.TempDir()
	// decision_candidate passes Apply's early target-kind validation but has no
	// countPanelTargetStates case, so it deterministically errors late — after
	// Mint and the running-advance — which is exactly the path the guard owns.
	run := newPanelApplyRunForTest("decision_candidate")

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err == nil || !strings.Contains(err.Error(), "unsupported target kind") {
		t.Fatalf("want unsupported-target-kind abort, got %v", err)
	}
	if got.WorkID == "" {
		t.Fatalf("the abort path must have created and claimed a work envelope before failing")
	}

	// The execution attempt is terminal failed with the abort named as the
	// reason -- never stranded pending. The claim reservation is superseded
	// (F2). Two attempts total.
	raStore := runattempt.Open(home)
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatalf("read run_attempts: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("run_attempts entries = %d, want 2 (claim reservation + execution)", len(entries))
	}
	execFound := false
	for _, e := range entries {
		candidate, err := raStore.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			t.Fatalf("Get runattempt %s: %v", e.Name(), err)
		}
		switch candidate.Origin {
		case runattempt.OriginHarnessExecution:
			execFound = true
			if candidate.Status != runattempt.StatusFailed {
				t.Fatalf("execution attempt status = %q, want failed (the deferred guard terminalized the abort)", candidate.Status)
			}
			if candidate.TerminalizationIntent == nil || candidate.TerminalizationIntent.Owner != runattempt.TerminalizationOwnerPanelApply || candidate.TerminalizationIntent.TargetStatus != runattempt.StatusFailed {
				t.Fatalf("execution attempt exact prepared terminalization = %+v, want panel-owned failed intent", candidate.TerminalizationIntent)
			}
			if candidate.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply {
				t.Fatalf("execution attempt terminalization owner = %q, want %q", candidate.TerminalizationOwner, runattempt.TerminalizationOwnerPanelApply)
			}
			if !strings.Contains(candidate.TerminalizationReason, "panel apply aborted") {
				t.Fatalf("execution attempt reason = %q, want it to name the abort", candidate.TerminalizationReason)
			}
			if candidate.TerminalizationReceiptID == "" || candidate.TerminalizationTransitionID == "" ||
				!slices.Contains(candidate.ProofRefs, candidate.TerminalizationReceiptID) || !slices.Contains(candidate.ProofRefs, candidate.TerminalizationTransitionID) {
				t.Fatalf("execution attempt proof refs = %v, receipt=%q transition=%q", candidate.ProofRefs, candidate.TerminalizationReceiptID, candidate.TerminalizationTransitionID)
			}
		case runattempt.OriginClaimReservation:
			if candidate.Status != runattempt.StatusSuperseded {
				t.Fatalf("claim reservation status = %q, want superseded once the execution attempt attached", candidate.Status)
			}
		default:
			t.Fatalf("unexpected attempt origin %q; every mint must be stamped (F2)", candidate.Origin)
		}
	}
	if !execFound {
		t.Fatal("no harness_execution attempt found; panel apply must stamp SetOrigin right after Mint")
	}

	// The envelope fail-closed (audit_only) instead of stranding in running,
	// mirroring the governed execution path's failed-run close.
	workStore, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(got.WorkID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if w.State != "failed" {
		t.Fatalf("work envelope state = %q, want failed (fail-closed by the deferred guard, not stuck running)", w.State)
	}
	if w.IDs.TransitionID == "" {
		t.Fatalf("a fail-closed envelope emits a Transition; transition_id is empty")
	}
}

func TestApplyKnowledgeClassNeedsHumanAnnotatesCandidateAndClosesQueue(t *testing.T) {
	home := t.TempDir()
	candID := "mcand_knowledge_needs_human"
	writeMemoryCandidateRecord(t, home, candID, map[string]any{
		"id":               candID,
		"entity":           "knowledge:paper:alpha",
		"predicate":        "finding",
		"object":           "route findings can be useful without becoming memory",
		"claim_type":       "knowledge",
		"confidence":       0.82,
		"promotion_basis":  candidates.BasisRuntimeProof,
		"state":            "parked",
		"visibility":       "contextual",
		"derivation":       "panel_test",
		"source_trace_ref": "trace://knowledge-alpha",
		"snippet_hash":     "sha256:knowledge-alpha",
	})

	run := newMemoryPanelApplyRunForTest("knowledge_needs_human", candID)
	finding := writeMemoryPanelFindingForTest(t, home, run, candID, "needs_human")
	run.Findings = []TargetFinding{finding}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	applied := got.Findings[0].Application
	if !applied.Applied || applied.MappedVerdict != "annotate" {
		t.Fatalf("knowledge application = %+v, want applied annotate", applied)
	}

	cand := readCandidateForTest(t, home, candID)
	if cand.State != "parked" || len(cand.Annotations) != 1 {
		t.Fatalf("candidate after apply = state %q annotations %d", cand.State, len(cand.Annotations))
	}
	ann := cand.Annotations[0]
	if ann.Decision != "annotate" || ann.OriginalDecision != "needs_human" || ann.RunID != got.ID || ann.VerdictArtifact != finding.VerdictArtifact {
		t.Fatalf("annotation = %+v", ann)
	}
	if ann.Confidence != finding.Synthesis.Confidence || ann.Rationale != finding.Synthesis.Rationale {
		t.Fatalf("annotation synthesis fields = %+v", ann)
	}
	if len(ann.LensFindings) != 1 || ann.LensFindings[0].LensRef != "lens:test-memory" || ann.LensFindings[0].Findings[0].Claim == "" {
		t.Fatalf("annotation lens findings = %+v", ann.LensFindings)
	}

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defaultRecall, err := store.Recall("knowledge:paper:alpha", mem.RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultRecall.Claims) != 0 || len(defaultRecall.EvidenceCandidates) != 0 {
		t.Fatalf("default recall must stay promoted-only, got claims=%d evidence=%d", len(defaultRecall.Claims), len(defaultRecall.EvidenceCandidates))
	}
	withEvidence, err := store.Recall("knowledge:paper:alpha", mem.RecallOpts{IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvidence.Claims) != 0 || len(withEvidence.EvidenceCandidates) != 1 {
		t.Fatalf("include-evidence recall = claims %d evidence %d", len(withEvidence.Claims), len(withEvidence.EvidenceCandidates))
	}

	again, err := Apply(home, got, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if !again.Findings[0].Application.Applied || again.Findings[0].Application.Note != "already applied; skipped" {
		t.Fatalf("re-apply application = %+v", again.Findings[0].Application)
	}
	if cand := readCandidateForTest(t, home, candID); len(cand.Annotations) != 1 {
		t.Fatalf("re-apply annotations = %d, want 1", len(cand.Annotations))
	}

	report := annotatedPanelVerdictQueueForTest(t, home)
	if report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 || report.Counts.ByStatus["applied"] != 1 {
		t.Fatalf("queue counts = %+v", report.Counts)
	}
	if got := report.Items[0].NextAction; !strings.Contains(got, "mapped=annotate") || strings.Contains(got, "owner review required") {
		t.Fatalf("queue next action = %q", got)
	}
}

func TestApplyOrdinaryNeedsHumanStillRequiresReview(t *testing.T) {
	home := t.TempDir()
	candID := "mcand_ordinary_needs_human"
	writeMemoryCandidateRecord(t, home, candID, map[string]any{
		"id":               candID,
		"entity":           "service:example-api",
		"predicate":        "uses_runtime",
		"object":           "go-http",
		"claim_type":       "architecture",
		"confidence":       0.82,
		"promotion_basis":  candidates.BasisRuntimeProof,
		"state":            "parked",
		"visibility":       "contextual",
		"derivation":       "panel_test",
		"source_trace_ref": "trace://ordinary",
		"snippet_hash":     "sha256:ordinary",
	})

	run := newMemoryPanelApplyRunForTest("ordinary_needs_human", candID)
	run.Findings = []TargetFinding{writeMemoryPanelFindingForTest(t, home, run, candID, "needs_human")}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	app := got.Findings[0].Application
	if app.Applied || app.MappedVerdict != "needs_human" {
		t.Fatalf("ordinary application = %+v, want unapplied needs_human", app)
	}
	cand := readCandidateForTest(t, home, candID)
	if cand.State != "parked" || len(cand.Annotations) != 0 {
		t.Fatalf("ordinary candidate = state %q annotations %d", cand.State, len(cand.Annotations))
	}

	report := annotatedPanelVerdictQueueForTest(t, home)
	if report.Counts.ReviewRequired != 1 || report.Counts.OK != 0 {
		t.Fatalf("queue counts = %+v", report.Counts)
	}
	if got := report.Items[0].NextAction; !strings.Contains(got, "panel requested human decision") {
		t.Fatalf("queue next action = %q", got)
	}
}

func TestApplyKnowledgeClassPromoteStillRequiresPromotionBasis(t *testing.T) {
	home := t.TempDir()
	candID := "mcand_knowledge_promote_no_basis"
	writeMemoryCandidateRecord(t, home, candID, map[string]any{
		"id":               candID,
		"entity":           "knowledge:paper:beta",
		"predicate":        "finding",
		"object":           "promotion still needs evidence basis",
		"claim_type":       "knowledge",
		"confidence":       0.91,
		"promotion_basis":  "",
		"state":            "parked",
		"visibility":       "contextual",
		"derivation":       "panel_test",
		"source_trace_ref": "trace://knowledge-beta",
		"snippet_hash":     "sha256:knowledge-beta",
	})

	run := newMemoryPanelApplyRunForTest("knowledge_promote_no_basis", candID)
	run.Findings = []TargetFinding{writeMemoryPanelFindingForTest(t, home, run, candID, "promote")}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	app := got.Findings[0].Application
	if !app.Applied || app.MappedVerdict != "park" || app.NoPromoteRef == "" {
		t.Fatalf("promotion guard application = %+v", app)
	}
	cand := readCandidateForTest(t, home, candID)
	if cand.State != "parked" || len(cand.Annotations) != 0 {
		t.Fatalf("candidate after guarded promote = state %q annotations %d", cand.State, len(cand.Annotations))
	}
	if countLines(t, filepath.Join(home, "ledger", "claims.jsonl")) != 0 {
		t.Fatalf("guarded knowledge promote wrote a claim")
	}

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	recall, err := store.Recall("knowledge:paper:beta", mem.RecallOpts{IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall.Claims) != 0 || len(recall.EvidenceCandidates) != 0 {
		t.Fatalf("guarded promote recall = claims %d evidence %d", len(recall.Claims), len(recall.EvidenceCandidates))
	}
}

func newMemoryPanelApplyRunForTest(suffix, candID string) Run {
	run := newPanelApplyRunForTest("memory_candidate")
	run.ID = "prun_20260701T000000Z_" + suffix
	run.Targets = []Target{{Ref: candID, Privacy: "contextual", StateBefore: "parked"}}
	run.CountsBefore = map[string]int{"parked": 1}
	return run
}

func writeMemoryPanelFindingForTest(t *testing.T, home string, run Run, candID, decision string) TargetFinding {
	t.Helper()
	lensFindings := []LensFinding{{
		LensRef:          "lens:test-memory",
		RouteEvidenceRef: "routeev_lens_" + candID,
		Confidence:       0.73,
		Status:           "completed",
		Findings: []Finding{{
			Claim:        "candidate is useful evidence but not ambient memory",
			Severity:     "info",
			EvidenceRefs: []string{"trace://" + candID},
		}},
	}}
	synthesis := Synthesis{
		SchemaVersion:         SynthesisSchemaVersion,
		Decision:              decision,
		Confidence:            0.87,
		Rationale:             "panel fixture " + decision,
		JudgeRouteEvidenceRef: "routeev_judge_" + candID,
	}
	target := TargetInput{
		Kind:       "memory_candidate",
		Ref:        candID,
		Visibility: "contextual",
		Summary:    "memory candidate " + candID,
	}
	artifact, err := writeVerdictArtifact(home, run, target, lensFindings, synthesis, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return TargetFinding{
		TargetRef:       candID,
		LensFindings:    lensFindings,
		Synthesis:       synthesis,
		VerdictArtifact: artifact,
	}
}

func annotatedPanelVerdictQueueForTest(t *testing.T, home string) reviewartifact.QueueReport {
	t.Helper()
	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateVerdictQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	return report
}
