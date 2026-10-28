package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

func runEventStoreCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runEventStore(args, &out, &errb)
	return out.String(), errb.String(), code
}

func seedLooseCLIRecord(t *testing.T, homePath, id, body string) {
	t.Helper()
	eventsDir := filepath.Join(homePath, "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := `{"schema_version":"compound.receipt.v0.1","id":"` + id + `","created_at":"2026-07-15T00:00:00Z","value":"` + body + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(eventsDir, id+".json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEventStoreMigrateVerbEndToEnd drives the wired CLI verb across all three
// modes on one seeded home: dry-run must not mutate, apply must close the gate
// and archive originals, verify must confirm and exit 0.
func TestEventStoreMigrateVerbEndToEnd(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	for _, id := range []string{"rec_cli_a", "rec_cli_b"} {
		seedLooseCLIRecord(t, homePath, id, "body-"+id)
	}

	out, errStr, code := runEventStoreCLI(t, "migrate", "--dry-run", "--home", homePath)
	if code != 0 {
		t.Fatalf("dry-run exit=%d stderr=%s", code, errStr)
	}
	if !strings.Contains(out, "would_migrate: 2") {
		t.Fatalf("dry-run did not project 2 migrations:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "migration", "plan.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote a plan (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "rec_cli_a.json")); err != nil {
		t.Fatalf("dry-run archived a record: %v", err)
	}

	out, errStr, code = runEventStoreCLI(t, "migrate", "--apply", "--home", homePath)
	if code != 0 {
		t.Fatalf("apply exit=%d stderr=%s\n%s", code, errStr, out)
	}
	if !strings.Contains(out, "complete: true") || !strings.Contains(out, "migrated: 2") {
		t.Fatalf("apply did not close the gate:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "migration", "completion.json")); err != nil {
		t.Fatalf("apply did not write completion.json: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(homePath, "events", "*.json")); len(matches) != 0 {
		t.Fatalf("apply left loose records: %v", matches)
	}

	_, errStr, code = runEventStoreCLI(t, "migrate", "--verify", "--home", homePath)
	if code != 0 {
		t.Fatalf("verify exit=%d stderr=%s", code, errStr)
	}
}

// TestEventStoreMigrateVerifyFlagsLegacyWriter proves the post-completion status
// check exits non-zero and names the degraded state when a loose file reappears.
func TestEventStoreMigrateVerifyFlagsLegacyWriter(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	seedLooseCLIRecord(t, homePath, "rec_cli_x", "x")
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("apply exit=%d", code)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_late", "late")

	out, _, code := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath)
	if code == 0 {
		t.Fatalf("verify exited 0 despite a reappearing legacy writer:\n%s", out)
	}
	if !strings.Contains(out, "degraded:legacy_writer_active") {
		t.Fatalf("verify did not surface degraded:legacy_writer_active:\n%s", out)
	}
}

func TestEventStoreMigrateRequiresExactlyOneMode(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	if _, _, code := runEventStoreCLI(t, "migrate", "--home", homePath); code != 2 {
		t.Fatalf("missing mode exit=%d, want 2", code)
	}
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--verify", "--home", homePath); code != 2 {
		t.Fatalf("two modes exit=%d, want 2", code)
	}
	if _, _, code := runEventStoreCLI(t, "unknown-sub"); code != 2 {
		t.Fatalf("unknown subcommand exit=%d, want 2", code)
	}
}

// TestEventStoreMigrateUsageLineIsRegisteredAtTopLevel mirrors the capture/act
// verbs: a new top-level verb must be discoverable in the top-level help text.
func TestEventStoreMigrateUsageLineIsRegisteredAtTopLevel(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(data), "compound event-store migrate ") {
		t.Fatalf("top-level usage() must document `compound event-store migrate`")
	}
	if !strings.Contains(string(data), "compound event-store rebaseline ") {
		t.Fatalf("top-level usage() must document `compound event-store rebaseline`")
	}
	if !strings.Contains(string(data), "compound event-store repair ") {
		t.Fatalf("top-level usage() must document `compound event-store repair`")
	}
}

type eventStoreRepairStatusTestReport struct {
	Needed                    bool     `json:"needed"`
	LooseAfterCompletion      int      `json:"loose_after_completion"`
	PlanID                    string   `json:"plan_id"`
	PlanStatus                string   `json:"plan_status"`
	ReviewStatus              string   `json:"review_status"`
	ApplyStatus               string   `json:"apply_status"`
	ProofStatus               string   `json:"proof_status"`
	RootAnchorConsistent      bool     `json:"root_anchor_consistent"`
	SupersessionStatus        string   `json:"supersession_status"`
	SupersessionSuccessorPlan string   `json:"supersession_successor_plan_id"`
	ActivePlanIDs             []string `json:"active_plan_ids"`
	NextAction                string   `json:"next_action"`
	NextCommand               string   `json:"next_command"`
	NextCommandArgv           []string `json:"next_command_argv"`
	NextCommandRequirements   []string `json:"next_command_requirements"`
}

// FALSIFIER: status is a projection, never an event-store operation. The
// helper snapshots the recursive file list and a SHA-256 of every file before
// and after each invocation; any created, removed, or byte-changed file fails.
func TestEventStoreRepairStatusNeverMutatesHomeAcrossStages(t *testing.T) {
	healthyHome := setupEventStoreRepairHealthyCLI(t)
	assertEventStoreRepairStatusNoMutation(t, healthyHome)

	homePath, workID, actor := setupEventStoreRepairCLI(t)
	assertEventStoreRepairStatusNoMutation(t, homePath)

	planned := planEventStoreRepairCLI(t, homePath)
	assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)

	_, reviewPath := planAndReviewExistingEventStoreRepairCLI(t, homePath, planned)
	assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)

	if out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json"); code != 0 {
		t.Fatalf("apply code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)
}

// FALSIFIER: the projection follows the real artifact progression: no loose
// source, immutable ready plan, authorize review, pending/result apply, then
// proof-backed finalization. A repair-needed or in-flight status still exits 0.
func TestEventStoreRepairStatusStageProgression(t *testing.T) {
	healthy := assertEventStoreRepairStatusNoMutation(t, setupEventStoreRepairHealthyCLI(t))
	if healthy.Needed || healthy.LooseAfterCompletion != 0 || healthy.PlanStatus != "not_created" || healthy.NextAction != "none" || healthy.NextCommand != "" {
		t.Fatalf("healthy status = %+v", healthy)
	}

	homePath, workID, actor := setupEventStoreRepairCLI(t)
	unplanned := assertEventStoreRepairStatusNoMutation(t, homePath)
	if !unplanned.Needed || unplanned.LooseAfterCompletion != 1 || unplanned.NextAction != "plan" || !strings.Contains(unplanned.NextCommand, "event-store repair plan") {
		t.Fatalf("unplanned status = %+v", unplanned)
	}

	planned := planEventStoreRepairCLI(t, homePath)
	ready := assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)
	if !ready.Needed || ready.PlanID != planned.Plan.ID || ready.PlanStatus != "ready" || ready.ReviewStatus != "not_recorded" || ready.ApplyStatus != "not_started" || ready.ProofStatus != "not_recorded" || !ready.RootAnchorConsistent || ready.SupersessionStatus != "not_recorded" || ready.NextAction != "review" || !strings.Contains(ready.NextCommand, "--attest-writer-stopped") {
		t.Fatalf("planned status = %+v", ready)
	}

	_, reviewPath := planAndReviewExistingEventStoreRepairCLI(t, homePath, planned)
	reviewed := assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)
	if !reviewed.Needed || reviewed.ReviewStatus != "authorize" || reviewed.ApplyStatus != "not_started" || reviewed.NextAction != "apply" || !strings.Contains(reviewed.NextCommand, "--work") || !strings.Contains(reviewed.NextCommand, "--actor") {
		t.Fatalf("reviewed status = %+v", reviewed)
	}

	if out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json"); code != 0 {
		t.Fatalf("apply code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	finalized := assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)
	if finalized.Needed || finalized.LooseAfterCompletion != 0 || finalized.PlanStatus != "ready" || finalized.ReviewStatus != "authorize" || finalized.ApplyStatus != "finalized" || finalized.ProofStatus != "recorded" || !finalized.RootAnchorConsistent || finalized.NextAction != "complete" || finalized.NextCommand != "" {
		t.Fatalf("finalized status = %+v", finalized)
	}
}

func TestEventStoreRepairStatusReportsEveryActivePlanInsteadOfPickingOne(t *testing.T) {
	homePath, _, _ := setupEventStoreRepairCLI(t)
	first := planEventStoreRepairCLI(t, homePath)
	seedLooseCLIRecord(t, homePath, "rec_cli_post_cutover_second", "later")
	second := planEventStoreRepairCLI(t, homePath)
	if first.Plan.ID == second.Plan.ID {
		t.Fatal("fixture did not create two distinct active plans")
	}

	report := assertEventStoreRepairStatusNoMutation(t, homePath)
	if report.PlanID != "" || report.NextAction != "inspect_active_repairs" || len(report.ActivePlanIDs) != 2 {
		t.Fatalf("multiple-active status = %+v", report)
	}
	for _, planID := range []string{first.Plan.ID, second.Plan.ID} {
		if !containsString(report.ActivePlanIDs, planID) || !strings.Contains(report.NextCommand, "--plan "+planID) {
			t.Fatalf("multiple-active status omitted %s: %+v", planID, report)
		}
	}
}

// FALSIFIER: each emitted argv is checked against the corresponding verb's
// actual parser shape. This prevents status from advertising a confident but
// nonexistent flag while keeping the test independent of shell quoting.
func TestEventStoreRepairStatusNextCommandFlagsMatchVerbParsers(t *testing.T) {
	homePath, _, _ := setupEventStoreRepairCLI(t)
	unplanned := assertEventStoreRepairStatusNoMutation(t, homePath)
	planned := planEventStoreRepairCLI(t, homePath)
	ready := assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)
	planAndReviewExistingEventStoreRepairCLI(t, homePath, planned)
	reviewed := assertEventStoreRepairStatusNoMutation(t, homePath, "--plan", planned.Plan.ID)

	for _, tc := range []struct {
		name   string
		verb   string
		report eventStoreRepairStatusTestReport
	}{
		{name: "plan", verb: "plan", report: unplanned},
		{name: "review", verb: "review", report: ready},
		{name: "apply", verb: "apply", report: reviewed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEventStoreRepairNextCommandParses(t, tc.report, tc.verb)
		})
	}

	supHome, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	injected := errors.New("interrupt after terminal supersession for status")
	previous := eventStoreRepairSupersessionAfterStage
	eventStoreRepairSupersessionAfterStage = func(stage string) error {
		if stage == "terminal" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { eventStoreRepairSupersessionAfterStage = previous })
	if _, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", supHome, "--json"); code == 0 || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("supersession interruption code=%d stderr=%s", code, stderr)
	}
	superseded := assertEventStoreRepairStatusNoMutation(t, supHome, "--plan", oldPlan.Plan.ID)
	if superseded.SupersessionStatus != "recorded" || superseded.SupersessionSuccessorPlan != broaderPlan.Plan.ID || superseded.NextAction != "finalize_supersession" {
		t.Fatalf("supersession status = %+v", superseded)
	}
	assertEventStoreRepairNextCommandParses(t, superseded, "supersede")
}

func TestEventStoreRepairStatusRejectsBadPlanID(t *testing.T) {
	t.Parallel()

	homePath := setupEventStoreRepairHealthyCLI(t)
	for _, planID := range []string{"not-a-plan", "esrp_aaaaaaaaaaaa"} {
		if out, stderr, code := runEventStoreCLI(t, "repair", "status", "--home", homePath, "--plan", planID, "--json"); code == 0 {
			t.Fatalf("bad plan %q exited 0: stdout=%s stderr=%s", planID, out, stderr)
		}
	}
}

func TestEventStoreRepairCLIPlanPolicyApplyThroughRunningWorkAndReplay(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)

	out, errStr, code := runEventStoreCLI(t, "repair", "plan", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%s stdout=%s", code, errStr, out)
	}
	var planned eventstore.PostCompletionRepairPlanRecord
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, out)
	}
	if planned.Plan.Total != 1 || planned.Plan.Counts[eventstore.RepairImportAbsent] != 1 || planned.PlanPath == "" {
		t.Fatalf("plan = %+v", planned)
	}
	reviewOut, reviewErr, reviewCode := runEventStoreCLI(t, "repair", "review", planned.PlanPath,
		"--reviewer", "operator:test", "--decision", "authorize", "--attest-writer-stopped", "--home", homePath, "--json")
	if reviewCode != 0 {
		t.Fatalf("review code=%d stderr=%s stdout=%s", reviewCode, reviewErr, reviewOut)
	}
	reviewPath := filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".review.json")

	applyStartedAt := time.Now().UTC()
	out, errStr, code = runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("apply code=%d stderr=%s stdout=%s", code, errStr, out)
	}
	var applied eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("decode apply: %v\n%s", err, out)
	}
	if !applied.Changed || applied.Replayed || applied.WorkID != workID || applied.ActionIntentID == "" || applied.PolicyDecisionID == "" || applied.ReceiptID == "" || applied.TransitionID == "" || applied.RepairProofDigest == "" || !applied.PolicyAuthority.Current || !applied.PolicyAuthority.Allowed || applied.PolicyAuthority.ExactGate != policy.AuthorityGatePolicyAllow {
		t.Fatalf("apply = %+v", applied)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "rec_cli_post_cutover.json")); !os.IsNotExist(err) {
		t.Fatalf("repair left hot loose record: %v", err)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := events.Read("rec_cli_post_cutover")
	if err != nil || !strings.Contains(string(record.Bytes), "late") {
		t.Fatalf("canonical repaired record = %s err=%v", record.Bytes, err)
	}
	for _, id := range []string{applied.ActionIntentID, applied.PolicyDecisionID, applied.ReceiptID, applied.TransitionID} {
		if !testEventExists(t, homePath, id) {
			t.Fatalf("governed work chain event %s missing", id)
		}
	}
	var action envelope.ActionIntent
	readTestEventJSON(t, homePath, applied.ActionIntentID, &action)
	var decision envelope.PolicyDecision
	readTestEventJSON(t, homePath, applied.PolicyDecisionID, &decision)
	if action.IDs.WorkID != workID || action.Capability != eventStoreRepairCapability || action.Action != "repair_post_completion_loose_records" || action.MutationClass != "local_reversible" || action.Reversibility != "manual" || action.BlastRadius != "bounded_event_store" || !containsString(action.EvidenceRefs, "eventstore-repair-review:"+applied.Result.Authorization.ReviewDigest) || decision.ActionIntentID != action.ID || !policy.Allowed(decision) {
		t.Fatalf("action/decision = %+v / %+v", action, decision)
	}
	var review eventstore.PostCompletionRepairReview
	reviewBytes, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reviewBytes, &review); err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	readTestEventJSON(t, homePath, applied.ReceiptID, &receipt)
	var transition envelope.Transition
	readTestEventJSON(t, homePath, applied.TransitionID, &transition)
	reviewAt := mustEventStoreRepairTime(t, review.CreatedAt)
	actionAt := mustEventStoreRepairTime(t, action.CreatedAt)
	decisionAt := mustEventStoreRepairTime(t, decision.CreatedAt)
	authorizedAt := mustEventStoreRepairTime(t, applied.Result.Authorization.AuthorizedAt)
	resultAt := mustEventStoreRepairTime(t, applied.Result.CompletedAt)
	receiptAt := mustEventStoreRepairTime(t, receipt.CreatedAt)
	transitionAt := mustEventStoreRepairTime(t, transition.CreatedAt)
	if actionAt.Before(reviewAt) || decisionAt.Before(actionAt) || authorizedAt.Before(decisionAt) || resultAt.Before(authorizedAt) || receiptAt.Before(resultAt) || transitionAt.Before(receiptAt) {
		t.Fatalf("repair execution is not causally ordered: review=%s action=%s decision=%s authorized=%s result=%s receipt=%s transition=%s", reviewAt, actionAt, decisionAt, authorizedAt, resultAt, receiptAt, transitionAt)
	}
	if decisionAt.Before(applyStartedAt) || !authorizedAt.Equal(decisionAt) {
		t.Fatalf("repair authority is backdated before current evaluation: apply_started=%s decision=%s authorized=%s", applyStartedAt, decisionAt, authorizedAt)
	}

	out, errStr, code = runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("replay code=%d stderr=%s stdout=%s", code, errStr, out)
	}
	var replay eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Changed || !replay.Replayed || replay.ActionIntentID != applied.ActionIntentID || replay.ReceiptID != applied.ReceiptID {
		t.Fatalf("replay=%+v first=%+v", replay, applied)
	}
	if !replay.Result.Changed || replay.Result.Replayed || !reflect.DeepEqual(replay.Result, applied.Result) {
		t.Fatalf("replay changed sealed nested result:\nreplay=%+v\nfirst=%+v", replay.Result, applied.Result)
	}
	persistedResult, exists, err := events.ReadPostCompletionRepairResult(planned.Plan.ID)
	if err != nil || !exists || persistedResult.ID != replay.Result.ID || persistedResult.CompletedAt != replay.Result.CompletedAt || persistedResult.ResultDigest != replay.Result.ResultDigest || persistedResult.Changed != replay.Result.Changed || persistedResult.Replayed != replay.Result.Replayed {
		t.Fatalf("replay nested result does not match persisted sealed result: exists=%v err=%v\npersisted=%+v\nreported=%+v", exists, err, persistedResult, replay.Result)
	}

	verifyOut, verifyErr, verifyCode := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath, "--json")
	if verifyCode != 0 {
		t.Fatalf("verify code=%d stderr=%s stdout=%s", verifyCode, verifyErr, verifyOut)
	}
	var verified eventstore.MigrateReport
	if err := json.Unmarshal([]byte(verifyOut), &verified); err != nil {
		t.Fatal(err)
	}
	if !verified.Completion.Complete || verified.Completion.RemainingLoose != 0 || verified.Completion.LegacyWriterActive || verified.Completion.RepairManifests != 1 || verified.Completion.RepairedRecords != 1 {
		t.Fatalf("verify = %+v", verified.Completion)
	}
}

func TestEventStoreRepairCLIPlanAndReviewTimestampsFollowFinalObservation(t *testing.T) {
	homePath, _, _ := setupEventStoreRepairCLI(t)
	const observation = 40 * time.Millisecond
	eventStoreRepairObservationInterval = observation

	planStartedAt := time.Now().UTC()
	out, stderr, code := runEventStoreCLI(t, "repair", "plan", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var planned eventstore.PostCompletionRepairPlanRecord
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatal(err)
	}
	planCreatedAt := mustEventStoreRepairTime(t, planned.Plan.CreatedAt)
	fenceCompletedAt := mustEventStoreRepairTime(t, planned.Plan.WriterFence.CompletedAt)
	if planCreatedAt.Before(planStartedAt.Add(observation)) || !planCreatedAt.Equal(fenceCompletedAt) {
		t.Fatalf("plan timestamp precedes final observation: started=%s created=%s fence_completed=%s", planStartedAt, planCreatedAt, fenceCompletedAt)
	}

	reviewStartedAt := time.Now().UTC()
	out, stderr, code = runEventStoreCLI(t, "repair", "review", planned.PlanPath,
		"--reviewer", "operator:test", "--decision", "authorize", "--attest-writer-stopped", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("review code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var review eventstore.PostCompletionRepairReview
	if err := json.Unmarshal([]byte(out), &review); err != nil {
		t.Fatal(err)
	}
	reviewCreatedAt := mustEventStoreRepairTime(t, review.CreatedAt)
	if reviewCreatedAt.Before(reviewStartedAt.Add(observation)) {
		t.Fatalf("review timestamp precedes final observation: started=%s created=%s", reviewStartedAt, reviewCreatedAt)
	}
}

func TestEventStoreRepairCLIInitialApplyRejectsReviewCreatedAfterExecutionTime(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned := planEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	futureReviewAt := time.Now().UTC().Add(time.Hour)
	reviewed, err := events.ReviewPostCompletionRepair(planned.PlanPath, "operator:test", "authorize", true, futureReviewAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(homePath, "events", "rec_cli_post_cutover.json")
	looseBefore, err := os.ReadFile(loosePath)
	if err != nil {
		t.Fatal(err)
	}
	homeBefore := snapshotHomeFiles(t, homePath)

	_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewed.ReviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, "review created_at is after current execution time") {
		t.Fatalf("future review was not rejected: code=%d stderr=%s", code, stderr)
	}
	looseAfter, readErr := os.ReadFile(loosePath)
	if readErr != nil || !bytes.Equal(looseAfter, looseBefore) {
		t.Fatalf("future review refusal changed loose source: err=%v", readErr)
	}
	if _, exists, readErr := events.ReadPostCompletionRepairPending(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("future review refusal wrote pending repair: exists=%v err=%v", exists, readErr)
	}
	if _, exists, readErr := events.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("future review refusal wrote repair result: exists=%v err=%v", exists, readErr)
	}
	if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
		t.Fatalf("future review refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
	}
}

func TestEventStoreRepairCLIInitialApplyRejectsMalformedReviewCreatedAtWithoutMutation(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	reviewBytes, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	var review eventstore.PostCompletionRepairReview
	if err := json.Unmarshal(reviewBytes, &review); err != nil {
		t.Fatal(err)
	}
	review.CreatedAt = "not-a-time"
	reviewBytes, err = json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, append(reviewBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	homeBefore := snapshotHomeFiles(t, homePath)

	_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, "review created_at invalid") {
		t.Fatalf("malformed review time was not rejected: code=%d stderr=%s", code, stderr)
	}
	if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
		t.Fatalf("malformed review refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
	}
}

func mustEventStoreRepairTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse event-store repair time %q: %v", value, err)
	}
	return parsed
}

func TestEventStoreRepairActionTimeFollowsLatestCausalPredecessor(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 18, 8, 0, 0, 0, time.UTC)
	reviewAt := base.Add(4*time.Minute + 250*time.Millisecond)

	actionAt, err := eventStoreRepairActionTime(
		reviewAt.Format(time.RFC3339Nano),
		base.Add(5*time.Minute),
		base,
		base.Add(time.Minute),
		base.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := reviewAt
	if !actionAt.Equal(want) {
		t.Fatalf("action_at=%s want exact causal maximum %s", actionAt, want)
	}
}

func TestEvaluateEventStoreRepairCurrentAuthorityRejectsBackwardClock(t *testing.T) {
	t.Parallel()

	evaluatedAt := time.Date(2026, time.July, 18, 8, 0, 0, 123, time.UTC)
	_, err := evaluateEventStoreRepairCurrentAuthority(
		filepath.Join(t.TempDir(), "missing-home"),
		envelope.ActionIntent{},
		eventstore.PostCompletionRepairPlan{},
		evaluatedAt,
		evaluatedAt.Add(-time.Nanosecond),
	)
	if err == nil || !strings.Contains(err.Error(), "check time precedes evaluation time") {
		t.Fatalf("backward clock was not rejected before authority evaluation: %v", err)
	}
}

type eventStoreRepairValidatorInputs struct {
	homePath   string
	events     *eventstore.Store
	planned    eventstore.PostCompletionRepairPlanRecord
	reviewPath string
	action     envelope.ActionIntent
	actionAt   time.Time
	evaluation policy.AuthorityEvaluation
}

// setupEventStoreRepairValidatorInputs builds a reserved authority tuple without
// starting any physical repair, so the store carries no pending progress.
func setupEventStoreRepairValidatorInputs(t *testing.T) eventStoreRepairValidatorInputs {
	t.Helper()
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, err := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	workAt, err := time.Parse(time.RFC3339Nano, w.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	evaluation, err := evaluateEventStoreRepairCurrentAuthority(homePath, action, planned.Plan, actionAt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return eventStoreRepairValidatorInputs{
		homePath: homePath, events: events, planned: planned, reviewPath: reviewPath,
		action: action, actionAt: actionAt, evaluation: evaluation,
	}
}

// FALSIFIER (decision binding): a reserved tuple whose authority revision is
// unchanged but whose decision ID no longer matches the current evaluation must
// be refused. A revision-only check accepts it and lets a drifted decision mutate.
func TestValidateEventStoreRepairMutationAuthorityBindsExactDecisionID(t *testing.T) {
	in := setupEventStoreRepairValidatorInputs(t)
	if err := validateEventStoreRepairMutationAuthority(in.events, in.homePath, in.planned.PlanPath, in.reviewPath, in.action, in.planned.Plan, in.actionAt, in.evaluation.Decision.ID, in.evaluation.Authority.Revision, true); err != nil {
		t.Fatalf("exact reserved authority tuple was refused: %v", err)
	}
	staleDecisionID := in.evaluation.Decision.ID + "_stale"
	err := validateEventStoreRepairMutationAuthority(in.events, in.homePath, in.planned.PlanPath, in.reviewPath, in.action, in.planned.Plan, in.actionAt, staleDecisionID, in.evaluation.Authority.Revision, true)
	if err == nil || !strings.Contains(err.Error(), "policy authority changed before mutation") {
		t.Fatalf("drifted decision id under unchanged authority revision was not refused: %v", err)
	}
}

// FALSIFIER (progress guard): a fresh reservation (allowProgress=false) must be
// refused once physical repair progress exists, while a mutation revalidation
// (allowProgress=true) legitimately proceeds over the same in-flight progress.
func TestValidateEventStoreRepairMutationAuthorityRejectsProgressWhenDisallowed(t *testing.T) {
	fresh := setupEventStoreRepairValidatorInputs(t)
	if err := validateEventStoreRepairMutationAuthority(fresh.events, fresh.homePath, fresh.planned.PlanPath, fresh.reviewPath, fresh.action, fresh.planned.Plan, fresh.actionAt, fresh.evaluation.Decision.ID, fresh.evaluation.Authority.Revision, false); err != nil {
		t.Fatalf("fresh reservation with no repair progress was refused: %v", err)
	}

	fixture := setupEventStoreRepairCLIReauthorizationFixture(t)
	if _, present, err := fixture.events.ReadPostCompletionRepairPending(fixture.planned.Plan.ID); err != nil || !present {
		t.Fatalf("fixture did not leave physical repair progress: present=%t err=%v", present, err)
	}
	if err := validateEventStoreRepairMutationAuthority(fixture.events, fixture.homePath, fixture.planned.PlanPath, fixture.reviewPath, fixture.action, fixture.planned.Plan, fixture.actionAt, fixture.initialDecision.ID, fixture.initialAuthorization.AuthorityRevision, true); err != nil {
		t.Fatalf("mutation revalidation refused legitimate in-flight progress: %v", err)
	}
	err := validateEventStoreRepairMutationAuthority(fixture.events, fixture.homePath, fixture.planned.PlanPath, fixture.reviewPath, fixture.action, fixture.planned.Plan, fixture.actionAt, fixture.initialDecision.ID, fixture.initialAuthorization.AuthorityRevision, false)
	if err == nil || !strings.Contains(err.Error(), "already progressed before reservation") {
		t.Fatalf("reservation over existing repair progress was not refused: %v", err)
	}
}

func TestEventStoreRepairCLISupersessionCausallyFollowsExactReview(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	oldPlan := planEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := events.ReviewPostCompletionRepair(oldPlan.PlanPath, "operator:test", "authorize", true, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_after_old_plan", "broader")
	reviewAt := mustEventStoreRepairTime(t, reviewed.Review.CreatedAt)
	broaderPlan, err := events.CreatePostCompletionRepairPlan(reviewAt.Add(-time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}

	supersessionStartedAt := time.Now().UTC()
	out, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewed.ReviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("supersede code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	var action envelope.ActionIntent
	readTestEventJSON(t, homePath, report.ActionIntentID, &action)
	var decision envelope.PolicyDecision
	readTestEventJSON(t, homePath, report.PolicyDecisionID, &decision)
	var receipt envelope.Receipt
	readTestEventJSON(t, homePath, report.ReceiptID, &receipt)
	var transition envelope.Transition
	readTestEventJSON(t, homePath, report.TransitionID, &transition)
	supersession, present, err := events.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
	if err != nil || !present {
		t.Fatalf("read supersession: present=%t err=%v", present, err)
	}

	actionAt := mustEventStoreRepairTime(t, action.CreatedAt)
	decisionAt := mustEventStoreRepairTime(t, decision.CreatedAt)
	authorizedAt := mustEventStoreRepairTime(t, supersession.Authorization.AuthorizedAt)
	supersessionAt := mustEventStoreRepairTime(t, supersession.CreatedAt)
	receiptAt := mustEventStoreRepairTime(t, receipt.CreatedAt)
	transitionAt := mustEventStoreRepairTime(t, transition.CreatedAt)
	if actionAt.Before(reviewAt) || decisionAt.Before(supersessionStartedAt) || !authorizedAt.Equal(decisionAt) || supersessionAt.Before(authorizedAt) || receiptAt.Before(supersessionAt) || transitionAt.Before(receiptAt) {
		t.Fatalf("supersession is not causally ordered: review=%s action=%s decision=%s authorized=%s supersession=%s receipt=%s transition=%s", reviewAt, actionAt, decisionAt, authorizedAt, supersessionAt, receiptAt, transitionAt)
	}
}

func TestEventStoreRepairCLISupersessionRejectsFutureReviewWithoutMutation(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	oldPlan := planEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := events.ReviewPostCompletionRepair(oldPlan.PlanPath, "operator:test", "authorize", true, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_after_old_plan", "broader")
	broaderPlan := planEventStoreRepairCLI(t, homePath)
	homeBefore := snapshotHomeFiles(t, homePath)

	_, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewed.ReviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, "review created_at is after current execution time") {
		t.Fatalf("future review was not rejected: code=%d stderr=%s", code, stderr)
	}
	if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
		t.Fatalf("future-review refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
	}
}

func TestEventStoreRepairCLIRejectsFutureCausalPredecessorsWithoutMutation(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		events, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		planned, err := events.CreatePostCompletionRepairPlan(time.Now().UTC().Add(time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		reviewed, err := events.ReviewPostCompletionRepair(planned.PlanPath, "operator:test", "authorize", true, time.Now().UTC(), 0)
		if err != nil {
			t.Fatal(err)
		}
		homeBefore := snapshotHomeFiles(t, homePath)

		_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewed.ReviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
		if code == 0 || !strings.Contains(stderr, "causal predecessor is after current execution time") {
			t.Fatalf("future plan was not rejected: code=%d stderr=%s", code, stderr)
		}
		if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
			t.Fatalf("future-plan refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
		}
	})

	t.Run("work", func(t *testing.T) {
		homePath, _, actor := setupEventStoreRepairCLI(t)
		planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		futureActor := actor + "-future"
		futureWorkID := createRunningEventStoreRepairWorkAt(t, homePath, futureActor, time.Now().UTC().Add(time.Hour))
		homeBefore := snapshotHomeFiles(t, homePath)

		_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", futureWorkID, "--actor", futureActor, "--home", homePath, "--json")
		if code == 0 || !strings.Contains(stderr, "causal predecessor is after current execution time") {
			t.Fatalf("future work was not rejected: code=%d stderr=%s", code, stderr)
		}
		if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
			t.Fatalf("future-work refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
		}
	})

	t.Run("successor", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		oldPlan, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		seedLooseCLIRecord(t, homePath, "rec_cli_future_successor", "broader")
		events, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		successor, err := events.CreatePostCompletionRepairPlan(time.Now().UTC().Add(time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		homeBefore := snapshotHomeFiles(t, homePath)

		_, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
			"--successor", successor.PlanPath, "--review", reviewPath,
			"--work", workID, "--actor", actor, "--home", homePath, "--json")
		if code == 0 || !strings.Contains(stderr, "causal predecessor is after current execution time") {
			t.Fatalf("future successor was not rejected: code=%d stderr=%s", code, stderr)
		}
		if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
			t.Fatalf("future-successor refusal changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
		}
	})
}

func TestEventStoreRepairCLIZeroProgressSupersessionUsesRunningWorkReceipt(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	oldPlan, oldReviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	seedLooseCLIRecord(t, homePath, "rec_cli_after_old_plan", "broader")
	broaderPlan := planEventStoreRepairCLI(t, homePath)

	out, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", oldReviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("supersede code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var superseded struct {
		Status           string `json:"status"`
		WorkID           string `json:"work_id"`
		PlanID           string `json:"plan_id"`
		SuccessorPlanID  string `json:"successor_plan_id"`
		ActionIntentID   string `json:"action_intent_id"`
		PolicyDecisionID string `json:"policy_decision_id"`
		ReceiptID        string `json:"receipt_id"`
		TransitionID     string `json:"transition_id"`
	}
	if err := json.Unmarshal([]byte(out), &superseded); err != nil {
		t.Fatalf("decode supersession: %v\n%s", err, out)
	}
	if superseded.Status != "completed" || superseded.WorkID != workID || superseded.PlanID != oldPlan.Plan.ID || superseded.SuccessorPlanID != broaderPlan.Plan.ID || superseded.ActionIntentID == "" || superseded.PolicyDecisionID == "" || superseded.ReceiptID == "" || superseded.TransitionID == "" {
		t.Fatalf("supersession did not bind work/action/policy/receipt/transition: %+v", superseded)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(superseded.ReceiptID); err != nil {
		t.Fatalf("supersession receipt is not durable: %v", err)
	}

	_, reviewErr, reviewCode := runEventStoreCLI(t, "repair", "review", broaderPlan.PlanPath,
		"--reviewer", "operator:test", "--decision", "authorize", "--attest-writer-stopped", "--home", homePath, "--json")
	if reviewCode != 0 {
		t.Fatalf("broader review code=%d stderr=%s", reviewCode, reviewErr)
	}
	broaderReviewPath := filepath.Join(filepath.Dir(broaderPlan.PlanPath), broaderPlan.Plan.ID+".review.json")
	applyOut, applyErr, applyCode := runEventStoreCLI(t, "repair", "apply", broaderPlan.PlanPath,
		"--review", broaderReviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if applyCode != 0 {
		t.Fatalf("broader apply code=%d stderr=%s stdout=%s", applyCode, applyErr, applyOut)
	}
	verifyOut, verifyErr, verifyCode := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath, "--json")
	if verifyCode != 0 {
		t.Fatalf("verify after supersession code=%d stderr=%s stdout=%s", verifyCode, verifyErr, verifyOut)
	}
}

func TestEventStoreRepairCLICompletedSupersessionSecondInvocationIsNoOp(t *testing.T) {
	homePath, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	firstOut, firstErr, firstCode := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if firstCode != 0 {
		t.Fatalf("initial supersession code=%d stderr=%s stdout=%s", firstCode, firstErr, firstOut)
	}
	var first eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(firstOut), &first); err != nil {
		t.Fatal(err)
	}
	homeBefore := snapshotHomeFiles(t, homePath)

	secondOut, secondErr, secondCode := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if secondCode != 0 {
		t.Fatalf("completed supersession replay code=%d stderr=%s stdout=%s", secondCode, secondErr, secondOut)
	}
	var second eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(secondOut), &second); err != nil {
		t.Fatal(err)
	}
	secondAuthority := second.PolicyAuthority
	first.PolicyAuthority = policy.AuthorityFace{}
	second.PolicyAuthority = policy.AuthorityFace{}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("completed supersession replay changed proof identity:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if secondAuthority.Current || secondAuthority.ActionIntentID != second.ActionIntentID || secondAuthority.PolicyDecisionID != second.PolicyDecisionID || secondAuthority.Revision == "" {
		t.Fatalf("completed supersession replay did not expose sealed historical authority: %+v", secondAuthority)
	}
	if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
		t.Fatalf("completed supersession replay mutated home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
	}
}

func TestEventStoreRepairCLISupersessionVerificationStaysRedUntilProof(t *testing.T) {
	homePath, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	injected := errors.New("interrupt after terminal supersession")
	previous := eventStoreRepairSupersessionAfterStage
	eventStoreRepairSupersessionAfterStage = func(stage string) error {
		if stage == "terminal" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { eventStoreRepairSupersessionAfterStage = previous })
	_, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("terminal interruption code=%d stderr=%s", code, stderr)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := events.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID); err != nil || exists {
		t.Fatalf("terminal supersession unexpectedly had proof: exists=%v err=%v", exists, err)
	}
	verified, err := events.Migrate(eventstore.MigrateOptions{Mode: eventstore.ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !eventStoreRepairReasonContains(verified.Completion.Reasons, "supersession work proof missing") {
		t.Fatalf("verification treated unproved supersession as terminal: %+v", verified.Completion)
	}
}

func TestEventStoreRepairCLIRetriesTerminalSupersessionBeforeReceiptWithIdenticalProofChain(t *testing.T) {
	homePath, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	injected := errors.New("interrupt after terminal supersession")
	previous := eventStoreRepairSupersessionAfterStage
	eventStoreRepairSupersessionAfterStage = func(stage string) error {
		if stage == "terminal" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { eventStoreRepairSupersessionAfterStage = previous })
	_, _, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 {
		t.Fatal("terminal interruption unexpectedly succeeded")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	supersession, exists, err := events.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
	if err != nil || !exists {
		t.Fatalf("terminal supersession missing: exists=%v err=%v", exists, err)
	}
	reserved := readEventStoreRepairReceiptedIdentity(t, filepath.Join(homePath, "work", "receipted-action-reservations", workID+".json"))
	eventStoreRepairSupersessionAfterStage = previous
	out, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("terminal retry code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.ActionIntentID != supersession.Authorization.ActionIntentID || report.PolicyDecisionID != supersession.Authorization.PolicyDecisionID || report.ActionIntentID != reserved.ActionIntentID || report.PolicyDecisionID != reserved.PolicyDecisionID || report.ReceiptID != reserved.ReceiptID || report.TransitionID != reserved.TransitionID || report.SupersessionProofDigest == "" {
		t.Fatalf("terminal retry changed transaction identity: supersession=%+v reservation=%+v report=%+v", supersession, reserved, report)
	}
	proof, exists, err := events.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID)
	if err != nil || !exists || proof.ProofDigest != report.SupersessionProofDigest || proof.ReceiptID != report.ReceiptID || proof.TransitionID != report.TransitionID {
		t.Fatalf("terminal retry proof did not converge: exists=%v proof=%+v err=%v", exists, proof, err)
	}
}

func TestEventStoreRepairCLIRetriesDurableSupersessionRootGapBeforeReceipt(t *testing.T) {
	homePath, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	repairRoot := filepath.Dir(oldPlan.PlanPath)
	rootPath := filepath.Join(repairRoot, "root.json")
	anchorPath := filepath.Join(filepath.Dir(repairRoot), "post-completion-repair-anchor.json")
	rootBefore, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	anchorBefore, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("interrupt after rooted terminal before receipt")
	previous := eventStoreRepairSupersessionAfterStage
	eventStoreRepairSupersessionAfterStage = func(stage string) error {
		if stage == "terminal" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { eventStoreRepairSupersessionAfterStage = previous })
	_, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("terminal interruption code=%d stderr=%s", code, stderr)
	}

	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	supersession, exists, err := events.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
	if err != nil || !exists {
		t.Fatalf("durable supersession missing after interruption: exists=%v err=%v", exists, err)
	}
	reserved := readEventStoreRepairReceiptedIdentity(t, filepath.Join(homePath, "work", "receipted-action-reservations", workID+".json"))
	completionPath := filepath.Join(homePath, "work", "receipted-action-completions", workID, reserved.ActionIntentID+".json")
	if _, statErr := os.Stat(completionPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("terminal interruption unexpectedly recorded work proof: %v", statErr)
	}

	// Recreate the exact crash window: immutable terminal bytes survived, but
	// their root+anchor ownership update did not. The CLI must repair ownership
	// before it is allowed to replay the reserved Receipt/Transition.
	if err := os.WriteFile(rootPath, rootBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorPath, anchorBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	eventStoreRepairSupersessionAfterStage = previous
	out, retryErr, retryCode := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if retryCode != 0 {
		if _, statErr := os.Stat(completionPath); statErr == nil {
			t.Fatalf("retry recorded work proof before repairing supersession root: stderr=%s", retryErr)
		}
		t.Fatalf("root-gap retry code=%d stderr=%s stdout=%s", retryCode, retryErr, out)
	}

	rootBody, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	var root eventstore.PostCompletionRepairRoot
	if err := json.Unmarshal(rootBody, &root); err != nil {
		t.Fatal(err)
	}
	rooted := false
	for _, artifact := range root.Artifacts {
		if artifact.Kind == "supersession" && artifact.ID == oldPlan.Plan.ID {
			rooted = true
			break
		}
	}
	if !rooted {
		t.Fatalf("successful retry did not restore supersession root ownership: %+v", root.Artifacts)
	}
	if _, err := os.Stat(completionPath); err != nil {
		t.Fatalf("rooted retry did not record reserved work proof: %v", err)
	}
	var report eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	proof, exists, err := events.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID)
	if err != nil || !exists || proof.SupersessionDigest != supersession.SupersessionDigest || proof.ReceiptID != report.ReceiptID || proof.TransitionID != report.TransitionID {
		t.Fatalf("root-gap retry proof did not converge: exists=%v proof=%+v report=%+v err=%v", exists, proof, report, err)
	}
}

func TestEventStoreRepairCLIRetriesReceiptBeforeResponseWithIdenticalProofChain(t *testing.T) {
	homePath, workID, actor, oldPlan, reviewPath, broaderPlan := setupEventStoreRepairSupersessionCLI(t)
	injected := errors.New("interrupt after supersession receipt")
	previous := eventStoreRepairSupersessionAfterStage
	eventStoreRepairSupersessionAfterStage = func(stage string) error {
		if stage == "receipt" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { eventStoreRepairSupersessionAfterStage = previous })
	_, stderr, code := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code == 0 || !strings.Contains(stderr, injected.Error()) {
		t.Fatalf("receipt interruption code=%d stderr=%s", code, stderr)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	supersession, exists, err := events.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
	if err != nil || !exists {
		t.Fatalf("terminal supersession missing after receipt: exists=%v err=%v", exists, err)
	}
	completed := readEventStoreRepairReceiptedIdentity(t, filepath.Join(homePath, "work", "receipted-action-completions", workID, supersession.Authorization.ActionIntentID+".json"))
	if _, exists, err := events.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID); err != nil || exists {
		t.Fatalf("receipt interruption unexpectedly finalized proof: exists=%v err=%v", exists, err)
	}
	eventStoreRepairSupersessionAfterStage = previous
	out, retryErr, retryCode := runEventStoreCLI(t, "repair", "supersede", oldPlan.PlanPath,
		"--successor", broaderPlan.PlanPath, "--review", reviewPath,
		"--work", workID, "--actor", actor, "--home", homePath, "--json")
	if retryCode != 0 {
		t.Fatalf("receipt retry code=%d stderr=%s stdout=%s", retryCode, retryErr, out)
	}
	var report eventStoreRepairSupersessionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.ActionIntentID != completed.ActionIntentID || report.PolicyDecisionID != completed.PolicyDecisionID || report.ReceiptID != completed.ReceiptID || report.TransitionID != completed.TransitionID || report.SupersessionProofDigest == "" {
		t.Fatalf("receipt retry changed completed transaction identity: completion=%+v report=%+v", completed, report)
	}
}

type eventStoreRepairReceiptedIdentity struct {
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
	ReceiptID        string `json:"receipt_id"`
	TransitionID     string `json:"transition_id"`
}

func setupEventStoreRepairSupersessionCLI(t *testing.T) (string, string, string, eventstore.PostCompletionRepairPlanRecord, string, eventstore.PostCompletionRepairPlanRecord) {
	t.Helper()
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	oldPlan, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	seedLooseCLIRecord(t, homePath, "rec_cli_after_old_plan", "broader")
	broaderPlan := planEventStoreRepairCLI(t, homePath)
	return homePath, workID, actor, oldPlan, reviewPath, broaderPlan
}

func readEventStoreRepairReceiptedIdentity(t *testing.T, path string) eventStoreRepairReceiptedIdentity {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var identity eventStoreRepairReceiptedIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func eventStoreRepairReasonContains(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

func TestEventStoreRepairCLIPlanningIsIdempotentAndReviewIsSeparateAuthoritySeam(t *testing.T) {
	homePath, _, _ := setupEventStoreRepairCLI(t)
	out, errStr, code := runEventStoreCLI(t, "repair", "plan", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%s", code, errStr)
	}
	var planned eventstore.PostCompletionRepairPlanRecord
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(planned.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	out, errStr, code = runEventStoreCLI(t, "repair", "plan", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("repeat plan code=%d stderr=%s stdout=%s", code, errStr, out)
	}
	var replayed eventstore.PostCompletionRepairPlanRecord
	if err := json.Unmarshal([]byte(out), &replayed); err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(replayed.PlanPath)
	if err != nil || replayed.Plan.PlanDigest != planned.Plan.PlanDigest || replayed.Plan.CreatedAt != planned.Plan.CreatedAt || !bytes.Equal(secondBytes, firstBytes) {
		t.Fatalf("repeat plan changed immutable artifact: err=%v first=%+v replay=%+v", err, planned.Plan, replayed.Plan)
	}
	reviewOut, errStr, code := runEventStoreCLI(t, "repair", "review", planned.PlanPath,
		"--reviewer", "operator:test", "--decision", "authorize", "--attest-writer-stopped",
		"--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("review code=%d stderr=%s stdout=%s", code, errStr, reviewOut)
	}
	var review map[string]any
	if err := json.Unmarshal([]byte(reviewOut), &review); err != nil {
		t.Fatalf("decode review: %v\n%s", err, reviewOut)
	}
	if review["review_digest"] == "" || review["reviewer"] != "operator:test" || review["decision"] != "authorize" || review["writer_stopped_attested"] != true {
		t.Fatalf("review = %+v", review)
	}
	if _, applyErr, applyCode := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--work", "work_missing", "--actor", "codex:test", "--home", homePath); applyCode == 0 || !strings.Contains(applyErr, "--review") {
		t.Fatalf("apply without review code=%d stderr=%s", applyCode, applyErr)
	}
}

func TestEventStoreRepairCLIReviewAndAdmissionRefusals(t *testing.T) {
	t.Run("wrong reviewer and decision", func(t *testing.T) {
		homePath, _, _ := setupEventStoreRepairCLI(t)
		planned := planEventStoreRepairCLI(t, homePath)
		for _, tc := range []struct {
			name, reviewer, decision, want string
		}{
			{name: "reviewer", reviewer: "codex:test", decision: "authorize", want: "operator identity"},
			{name: "bare operator", reviewer: "operator:", decision: "authorize", want: "operator identity"},
			{name: "whitespace suffix", reviewer: "operator:   ", decision: "authorize", want: "operator identity"},
			{name: "not exact", reviewer: " operator:test", decision: "authorize", want: "operator identity"},
			{name: "decision", reviewer: "operator:test", decision: "hold", want: "must be authorize"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, stderr, code := runEventStoreCLI(t, "repair", "review", planned.PlanPath,
					"--reviewer", tc.reviewer, "--decision", tc.decision, "--attest-writer-stopped", "--home", homePath)
				if code == 0 || !strings.Contains(stderr, tc.want) {
					t.Fatalf("review code=%d stderr=%s", code, stderr)
				}
			})
		}
	})

	t.Run("tampered review", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		body, err := os.ReadFile(reviewPath)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(`"decision": "authorize"`), []byte(`"decision": "tampered"`), 1)
		if err := os.WriteFile(reviewPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath)
		if code == 0 || !strings.Contains(stderr, "review") {
			t.Fatalf("tampered review code=%d stderr=%s", code, stderr)
		}
		assertRepairCLILoosePresent(t, homePath)
	})

	t.Run("writer drift after review", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		seedLooseCLIRecord(t, homePath, "rec_cli_after_review", "late-writer")
		_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath)
		if code == 0 || !strings.Contains(stderr, "writer fence drifted") {
			t.Fatalf("writer drift code=%d stderr=%s", code, stderr)
		}
		assertRepairCLILoosePresent(t, homePath)
	})

	t.Run("policy denial", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome:\n  local_reversible:\n    outcome: deny\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath)
		if code == 0 || !strings.Contains(stderr, "refused") {
			t.Fatalf("policy denial code=%d stderr=%s", code, stderr)
		}
		assertRepairCLILoosePresent(t, homePath)
	})

	t.Run("wrong actor and non-running work", func(t *testing.T) {
		homePath, workID, actor := setupEventStoreRepairCLI(t)
		planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
		_, wrongActorErr, wrongActorCode := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", workID, "--actor", "codex:not-holder", "--home", homePath)
		if wrongActorCode == 0 || !strings.Contains(wrongActorErr, "lease") {
			t.Fatalf("wrong actor code=%d stderr=%s", wrongActorCode, wrongActorErr)
		}
		store, err := work.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := store.Create(work.CreateInput{Owner: "eventstore", Actor: actor, SuccessPredicate: "must remain unmutated", State: "ready"}, cliTestNow.Add(10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		_, notRunningErr, notRunningCode := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
			"--review", reviewPath, "--work", ready.ID, "--actor", actor, "--home", homePath)
		if notRunningCode == 0 || !strings.Contains(notRunningErr, "running") {
			t.Fatalf("non-running work code=%d stderr=%s", notRunningCode, notRunningErr)
		}
		assertRepairCLILoosePresent(t, homePath)
	})
}

func TestEventStoreRepairCLICompletedBytesFinalizeAgainstSealedAuthorityWhenCurrentPolicyIsUnreadable(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, _ := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	workAt, _ := time.Parse(time.RFC3339Nano, w.CreatedAt)
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, planned.Plan), source, actionAt)
	if err := validateEventStoreRepairCurrentAuthority(action, evaluation, time.Now()); err != nil {
		t.Fatal(err)
	}
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(evaluation.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: evaluation.Decision.ID,
		AuthorityRevision: evaluation.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	receiptInput := eventStoreRepairReceiptedInput(action, evaluation.Decision, planned.Plan, evaluation.Authority.Revision)
	if err := workStore.ReserveReceiptedAction(workID, receiptInput, actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := events.ApplyPostCompletionRepair(planned.PlanPath, authorization, eventstore.PostCompletionRepairApplyOptions{ValidateAuthority: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("sealed finalization code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Replayed || report.Changed || report.PolicyAuthority.Current || report.ReceiptID == "" || report.RepairProofDigest == "" {
		t.Fatalf("sealed finalization report = %+v", report)
	}
}

func TestEventStoreRepairCLIProgressCompleteWithoutResultFinalizesAgainstSealedAuthority(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, _ := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	workAt, _ := time.Parse(time.RFC3339Nano, w.CreatedAt)
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, planned.Plan), source, actionAt)
	if err := validateEventStoreRepairCurrentAuthority(action, evaluation, time.Now()); err != nil {
		t.Fatal(err)
	}
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(evaluation.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: evaluation.Decision.ID,
		AuthorityRevision: evaluation.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	receiptInput := eventStoreRepairReceiptedInput(action, evaluation.Decision, planned.Plan, evaluation.Authority.Revision)
	if err := workStore.ReserveReceiptedAction(workID, receiptInput, actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupt after physical completion before result")
	_, err = events.ApplyPostCompletionRepair(planned.PlanPath, authorization, eventstore.PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			pending, exists, readErr := events.ReadPostCompletionRepairPending(planned.Plan.ID)
			if readErr != nil || !exists {
				return readErr
			}
			for _, entry := range pending.Entries {
				if entry.Phase != "removed" {
					return nil
				}
			}
			return injected
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("physical-completion interruption = %v", err)
	}
	if _, exists, readErr := events.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("interruption unexpectedly wrote result: exists=%v err=%v", exists, readErr)
	}
	pending, exists, err := events.ReadPostCompletionRepairPending(planned.Plan.ID)
	if err != nil || !exists {
		t.Fatalf("read physically complete progress: exists=%v err=%v", exists, err)
	}
	for _, entry := range pending.Entries {
		if entry.Phase != "removed" {
			t.Fatalf("physical completion entry = %+v", entry)
		}
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("sealed physical-completion retry code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.PolicyAuthority.Current || report.ReceiptID == "" || report.RepairProofDigest == "" {
		t.Fatalf("sealed physical-completion report = %+v", report)
	}
}

func TestEventStoreRepairCLIPartialProgressRefusesNewMutationWhenCurrentPolicyIsUnreadable(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, _ := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	workAt, _ := time.Parse(time.RFC3339Nano, w.CreatedAt)
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, planned.Plan), source, actionAt)
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(evaluation.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: evaluation.Decision.ID,
		AuthorityRevision: evaluation.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	receiptInput := eventStoreRepairReceiptedInput(action, evaluation.Decision, planned.Plan, evaluation.Authority.Revision)
	if err := workStore.ReserveReceiptedAction(workID, receiptInput, actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries := make([]eventstore.PostCompletionRepairProgressEntry, 0, len(planned.Plan.Entries))
	for _, entry := range planned.Plan.Entries {
		entries = append(entries, eventstore.PostCompletionRepairProgressEntry{ID: entry.ID, Phase: "planned"})
	}
	pending := eventstore.PostCompletionRepairPending{
		SchemaVersion: "compound.eventstore.post_completion_repair_pending.v0.1",
		PlanID:        planned.Plan.ID, PlanDigest: planned.Plan.PlanDigest, Authorization: authorization,
		StartedAt: actionAt.Format(time.RFC3339Nano), Entries: entries,
	}
	pending.PendingDigest = repairCLITestDigest(t, pending)
	body, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".pending.json")
	if err := os.WriteFile(pendingPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath)
	if code == 0 || !strings.Contains(stderr, "policy") {
		t.Fatalf("partial unreadable policy code=%d stderr=%s", code, stderr)
	}
	assertRepairCLILoosePresent(t, homePath)
}

func TestEventStoreRepairCLIPartialProgressReauthorizesCurrentAllowedPolicy(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, _ := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	workAt, _ := time.Parse(time.RFC3339Nano, w.CreatedAt)
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, planned.Plan), source, actionAt)
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(evaluation.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: evaluation.Decision.ID,
		AuthorityRevision: evaluation.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	if err := workStore.ReserveReceiptedAction(workID, eventStoreRepairReceiptedInput(action, evaluation.Decision, planned.Plan, evaluation.Authority.Revision), actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries := make([]eventstore.PostCompletionRepairProgressEntry, 0, len(planned.Plan.Entries))
	for _, entry := range planned.Plan.Entries {
		entries = append(entries, eventstore.PostCompletionRepairProgressEntry{ID: entry.ID, Phase: "planned"})
	}
	pending := eventstore.PostCompletionRepairPending{
		SchemaVersion: "compound.eventstore.post_completion_repair_pending.v0.1",
		PlanID:        planned.Plan.ID, PlanDigest: planned.Plan.PlanDigest, Authorization: authorization,
		StartedAt: actionAt.Format(time.RFC3339Nano), Entries: entries,
	}
	pending.PendingDigest = repairCLITestDigest(t, pending)
	body, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".pending.json"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reauthorizationStartedAt := time.Now().UTC()
	out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("allowed policy reauthorization code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Changed || len(report.Result.AuthorizationHistory) != 2 || report.Result.AuthorizationHistory[0].AuthorityRevision == report.Result.AuthorizationHistory[1].AuthorityRevision {
		t.Fatalf("reauthorized repair did not preserve both authority revisions: %+v", report.Result.AuthorizationHistory)
	}
	latestAuthorization := report.Result.AuthorizationHistory[len(report.Result.AuthorizationHistory)-1]
	latestAuthorizationAt := mustEventStoreRepairTime(t, latestAuthorization.AuthorizedAt)
	var latestDecision envelope.PolicyDecision
	readTestEventJSON(t, homePath, latestAuthorization.PolicyDecisionID, &latestDecision)
	latestDecisionAt := mustEventStoreRepairTime(t, latestDecision.CreatedAt)
	if latestDecisionAt.Before(reauthorizationStartedAt) || !latestAuthorizationAt.Equal(latestDecisionAt) {
		t.Fatalf("reauthorization is not stamped at current evaluation: started=%s decision=%s authorized=%s", reauthorizationStartedAt, latestDecisionAt, latestAuthorizationAt)
	}
}

func TestEventStoreRepairCLIRetryConvergesAfterWorkReservationReauthorizationBeforeJournal(t *testing.T) {
	fixture := setupEventStoreRepairCLIReauthorizationFixture(t)
	current, nextAuthorization, nextAt := fixture.nextAuthorization(t)
	previousInput := eventStoreRepairReceiptedInput(fixture.action, fixture.initialDecision, fixture.planned.Plan, fixture.initialAuthorization.AuthorityRevision)
	nextInput := eventStoreRepairReceiptedInput(fixture.action, current.Decision, fixture.planned.Plan, nextAuthorization.AuthorityRevision)
	if err := fixture.workStore.ReauthorizeReceiptedActionReservation(fixture.workID, previousInput, fixture.actionAt, nextInput, nextAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	pending, exists, err := fixture.events.ReadPostCompletionRepairPending(fixture.planned.Plan.ID)
	if err != nil || !exists || pending.Authorization.PolicyDecisionID != fixture.initialAuthorization.PolicyDecisionID {
		t.Fatalf("crash shape did not preserve stale journal authority: exists=%v pending=%+v err=%v", exists, pending, err)
	}

	report := retryEventStoreRepairCLIReauthorization(t, fixture)
	assertEventStoreRepairCLIReauthorizationConverged(t, fixture, report, current.Decision.ID)
}

func TestEventStoreRepairCLIReauthorizationReceiptsFollowLatestAuthorization(t *testing.T) {
	fixture := setupEventStoreRepairCLIReauthorizationFixture(t)
	current, nextAuthorization, nextAt := fixture.nextAuthorization(t)
	previousInput := eventStoreRepairReceiptedInput(fixture.action, fixture.initialDecision, fixture.planned.Plan, fixture.initialAuthorization.AuthorityRevision)
	nextInput := eventStoreRepairReceiptedInput(fixture.action, current.Decision, fixture.planned.Plan, nextAuthorization.AuthorityRevision)
	if err := fixture.workStore.ReauthorizeReceiptedActionReservation(fixture.workID, previousInput, fixture.actionAt, nextInput, nextAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := fixture.events.ReauthorizePostCompletionRepair(fixture.planned.PlanPath, fixture.initialAuthorization, nextAuthorization, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	report := retryEventStoreRepairCLIReauthorization(t, fixture)
	latestAuthorizationAt := mustEventStoreRepairTime(t, report.Result.Authorization.AuthorizedAt)
	if latestAuthorizationAt.Before(nextAt) {
		t.Fatalf("repair lost latest authorization time: got=%s want-at-least=%s", latestAuthorizationAt, nextAt)
	}
	var receipt envelope.Receipt
	readTestEventJSON(t, fixture.homePath, report.ReceiptID, &receipt)
	var transition envelope.Transition
	readTestEventJSON(t, fixture.homePath, report.TransitionID, &transition)
	receiptAt := mustEventStoreRepairTime(t, receipt.CreatedAt)
	transitionAt := mustEventStoreRepairTime(t, transition.CreatedAt)
	if receiptAt.Before(latestAuthorizationAt) || transitionAt.Before(latestAuthorizationAt) {
		t.Fatalf("reauthorized transaction predates latest authority: authorization=%s receipt=%s transition=%s", latestAuthorizationAt, receiptAt, transitionAt)
	}
}

func TestEventStoreRepairCLIRetryConvergesAfterJournalReauthorizationBeforeRootHead(t *testing.T) {
	fixture := setupEventStoreRepairCLIReauthorizationFixture(t)
	current, nextAuthorization, nextAt := fixture.nextAuthorization(t)
	previousInput := eventStoreRepairReceiptedInput(fixture.action, fixture.initialDecision, fixture.planned.Plan, fixture.initialAuthorization.AuthorityRevision)
	nextInput := eventStoreRepairReceiptedInput(fixture.action, current.Decision, fixture.planned.Plan, nextAuthorization.AuthorityRevision)
	if err := fixture.workStore.ReauthorizeReceiptedActionReservation(fixture.workID, previousInput, fixture.actionAt, nextInput, nextAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	repairRoot := filepath.Dir(fixture.planned.PlanPath)
	rootPath := filepath.Join(repairRoot, "root.json")
	anchorPath := filepath.Join(filepath.Dir(repairRoot), "post-completion-repair-anchor.json")
	rootBefore, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	anchorBefore, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.events.ReauthorizePostCompletionRepair(fixture.planned.PlanPath, fixture.initialAuthorization, nextAuthorization, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootPath, rootBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorPath, anchorBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	pending, exists, err := fixture.events.ReadPostCompletionRepairPending(fixture.planned.Plan.ID)
	if err != nil || !exists || pending.Authorization.PolicyDecisionID != nextAuthorization.PolicyDecisionID {
		t.Fatalf("crash shape lost journal reauthorization: exists=%v pending=%+v err=%v", exists, pending, err)
	}

	report := retryEventStoreRepairCLIReauthorization(t, fixture)
	assertEventStoreRepairCLIReauthorizationConverged(t, fixture, report, current.Decision.ID)
}

func TestEventStoreRepairCLIRetryRecoversInterruptedIntermediateAuthorityBeforeCurrentPolicy(t *testing.T) {
	fixture := setupEventStoreRepairCLIReauthorizationFixture(t)
	policyB, authorizationB, atB := fixture.nextAuthorization(t)
	inputA := eventStoreRepairReceiptedInput(fixture.action, fixture.initialDecision, fixture.planned.Plan, fixture.initialAuthorization.AuthorityRevision)
	inputB := eventStoreRepairReceiptedInput(fixture.action, policyB.Decision, fixture.planned.Plan, authorizationB.AuthorityRevision)
	if err := fixture.workStore.ReauthorizeReceiptedActionReservation(fixture.workID, inputA, fixture.actionAt, inputB, atB, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	repairRoot := filepath.Dir(fixture.planned.PlanPath)
	rootPath := filepath.Join(repairRoot, "root.json")
	anchorPath := filepath.Join(filepath.Dir(repairRoot), "post-completion-repair-anchor.json")
	rootBefore, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	anchorBefore, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.events.ReauthorizePostCompletionRepair(fixture.planned.PlanPath, fixture.initialAuthorization, authorizationB, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(repairRoot, fixture.planned.Plan.ID+".progress.jsonl")
	progress, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	lineStart := bytes.LastIndex(progress[:len(progress)-1], []byte{'\n'}) + 1
	if lineStart <= 0 || lineStart >= len(progress)-1 {
		t.Fatalf("intermediate authorization transition line missing: bytes=%d start=%d", len(progress), lineStart)
	}
	cut := lineStart + (len(progress)-lineStart)/2
	if err := os.WriteFile(progressPath, progress[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootPath, rootBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorPath, anchorBefore, 0o644); err != nil {
		t.Fatal(err)
	}

	policyCBody := []byte("class_outcome:\n  local_reversible:\n    outcome: allow_with_obligations\n    obligations:\n      - emit a Receipt\n      - record a rollback_plan\n      - preserve authorization history\n")
	if err := os.WriteFile(filepath.Join(fixture.homePath, "config", "policy.yaml"), policyCBody, 0o644); err != nil {
		t.Fatal(err)
	}
	policyC, err := evaluateEventStoreRepairCurrentAuthority(fixture.homePath, fixture.action, fixture.planned.Plan, fixture.actionAt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if policyC.Decision.ID == policyB.Decision.ID && policyC.Authority.Revision == authorizationB.AuthorityRevision {
		t.Fatalf("policy C did not advance beyond B: B=%s/%s C=%s/%s", policyB.Decision.ID, authorizationB.AuthorityRevision, policyC.Decision.ID, policyC.Authority.Revision)
	}

	out, stderr, code := runEventStoreCLI(t, "repair", "apply", fixture.planned.PlanPath,
		"--review", fixture.reviewPath, "--work", fixture.workID, "--actor", fixture.actor, "--home", fixture.homePath, "--json")
	if code != 0 {
		t.Fatalf("A->B torn / policy C retry code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	history := report.Result.AuthorizationHistory
	if len(history) != 3 || history[0].PolicyDecisionID != fixture.initialDecision.ID || history[1].AuthorityRevision != authorizationB.AuthorityRevision || history[2].AuthorityRevision != policyC.Authority.Revision {
		t.Fatalf("CLI did not recover exact A->B before B->C: %+v", history)
	}
	verifyOut, verifyErr, verifyCode := runEventStoreCLI(t, "migrate", "--verify", "--home", fixture.homePath, "--json")
	if verifyCode != 0 {
		t.Fatalf("A->B->C verify code=%d stderr=%s stdout=%s", verifyCode, verifyErr, verifyOut)
	}
}

func TestEventStoreRepairCLIVerifyFailsWhenDurableProofIsDeleted(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	out, stderr, code := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("apply code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	proofPath := filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".proof.json")
	if err := os.Remove(proofPath); err != nil {
		t.Fatal(err)
	}
	verifyOut, verifyErr, verifyCode := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath, "--json")
	if verifyCode == 0 || !strings.Contains(verifyOut+verifyErr, "indexed artifact missing") {
		t.Fatalf("proof deletion verify code=%d stderr=%s stdout=%s", verifyCode, verifyErr, verifyOut)
	}
}

func TestEventStoreRepairCLIRejectsProgressedUnrootedPendingSnapshot(t *testing.T) {
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, _ := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	workAt, _ := time.Parse(time.RFC3339Nano, w.CreatedAt)
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, planned.Plan), source, actionAt)
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(evaluation.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: evaluation.Decision.ID,
		AuthorityRevision: evaluation.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	if err := workStore.ReserveReceiptedAction(workID, eventStoreRepairReceiptedInput(action, evaluation.Decision, planned.Plan, evaluation.Authority.Revision), actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	wrongSegments, err := filepath.Glob(filepath.Join(homePath, "events", "segments", "receipt", "*.jsonl"))
	if err != nil || len(wrongSegments) == 0 {
		t.Fatalf("initial segment missing: %v %v", wrongSegments, err)
	}
	wrongRel, err := filepath.Rel(filepath.Join(homePath, "events"), wrongSegments[0])
	if err != nil {
		t.Fatal(err)
	}
	entry := planned.Plan.Entries[0]
	pending := eventstore.PostCompletionRepairPending{
		SchemaVersion: "compound.eventstore.post_completion_repair_pending.v0.1",
		PlanID:        planned.Plan.ID, PlanDigest: planned.Plan.PlanDigest, Authorization: authorization,
		StartedAt: actionAt.Format(time.RFC3339Nano),
		Entries: []eventstore.PostCompletionRepairProgressEntry{{
			ID: entry.ID, Phase: "append_reserved", Segment: wrongRel, SegmentOffset: 0, CanonicalSHA256: entry.CanonicalSHA256,
		}},
	}
	pending.PendingDigest = repairCLITestDigest(t, pending)
	body, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".pending.json"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	_, applyErr, applyCode := runEventStoreCLI(t, "repair", "apply", planned.PlanPath,
		"--review", reviewPath, "--work", workID, "--actor", actor, "--home", homePath)
	if applyCode == 0 || !strings.Contains(applyErr, "pending snapshot is not immutable initial state") {
		t.Fatalf("progressed unrooted pending refusal code=%d stderr=%s", applyCode, applyErr)
	}
	assertRepairCLILoosePresent(t, homePath)
}

func repairCLITestDigest(t *testing.T, pending eventstore.PostCompletionRepairPending) string {
	t.Helper()
	pending.PendingDigest = ""
	body, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type eventStoreRepairCLIReauthorizationFixture struct {
	homePath             string
	workID               string
	actor                string
	reviewPath           string
	planned              eventstore.PostCompletionRepairPlanRecord
	events               *eventstore.Store
	workStore            *work.Store
	action               envelope.ActionIntent
	initialDecision      envelope.PolicyDecision
	initialAuthorization eventstore.PostCompletionRepairAuthorization
	actionAt             time.Time
}

func setupEventStoreRepairCLIReauthorizationFixture(t *testing.T) eventStoreRepairCLIReauthorizationFixture {
	t.Helper()
	homePath, workID, actor := setupEventStoreRepairCLI(t)
	planned, reviewPath := planAndReviewEventStoreRepairCLI(t, homePath)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	review, err := events.PostCompletionRepairReviewForApply(reviewPath, planned.Plan, false)
	if err != nil {
		t.Fatal(err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	planAt, err := time.Parse(time.RFC3339Nano, planned.Plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	workAt, err := time.Parse(time.RFC3339Nano, w.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	actionAt := planAt
	if workAt.After(actionAt) {
		actionAt = workAt
	}
	action := eventStoreRepairIntent(w, actor, planned.Plan, review, actionAt)
	initial, err := evaluateEventStoreRepairCurrentAuthority(homePath, action, planned.Plan, actionAt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sealedAction, _ := json.Marshal(action)
	sealedDecision, _ := json.Marshal(initial.Decision)
	authorization := eventstore.PostCompletionRepairAuthorization{
		WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: initial.Decision.ID,
		AuthorityRevision: initial.Authority.Revision, PlanDigest: planned.Plan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
		AuthorizedAt: actionAt.Format(time.RFC3339Nano), SealedAction: sealedAction, SealedDecision: sealedDecision,
	}
	initialInput := eventStoreRepairReceiptedInput(action, initial.Decision, planned.Plan, initial.Authority.Revision)
	if err := workStore.ReserveReceiptedAction(workID, initialInput, actionAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupt after rooted progress creation")
	validations := 0
	_, err = events.ApplyPostCompletionRepair(planned.PlanPath, authorization, eventstore.PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			validations++
			if validations > 1 {
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("rooted partial-progress setup = %v", err)
	}
	return eventStoreRepairCLIReauthorizationFixture{
		homePath: homePath, workID: workID, actor: actor, reviewPath: reviewPath, planned: planned,
		events: events, workStore: workStore, action: action, initialDecision: initial.Decision,
		initialAuthorization: authorization, actionAt: actionAt,
	}
}

func (fixture eventStoreRepairCLIReauthorizationFixture) nextAuthorization(t *testing.T) (policy.AuthorityEvaluation, eventstore.PostCompletionRepairAuthorization, time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.homePath, "config", "policy.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Production reauthorization mints the decision and stamps AuthorizedAt at one
	// instant, so the sealed decision id reproduces on retry evaluated at that instant.
	nextAt := time.Now().UTC()
	current, err := evaluateEventStoreRepairCurrentAuthority(fixture.homePath, fixture.action, fixture.planned.Plan, nextAt, nextAt)
	if err != nil {
		t.Fatal(err)
	}
	if current.Decision.ID == fixture.initialDecision.ID && current.Authority.Revision == fixture.initialAuthorization.AuthorityRevision {
		t.Fatalf("policy fixture did not create a new authority revision: initial=%s/%s current=%s/%s", fixture.initialDecision.ID, fixture.initialAuthorization.AuthorityRevision, current.Decision.ID, current.Authority.Revision)
	}
	next := fixture.initialAuthorization
	next.PolicyDecisionID = current.Decision.ID
	next.AuthorityRevision = current.Authority.Revision
	next.AuthorizedAt = nextAt.Format(time.RFC3339Nano)
	next.SealedAction, _ = json.Marshal(fixture.action)
	next.SealedDecision, _ = json.Marshal(current.Decision)
	return current, next, nextAt
}

func retryEventStoreRepairCLIReauthorization(t *testing.T, fixture eventStoreRepairCLIReauthorizationFixture) eventStoreRepairApplyReport {
	t.Helper()
	out, stderr, code := runEventStoreCLI(t, "repair", "apply", fixture.planned.PlanPath,
		"--review", fixture.reviewPath, "--work", fixture.workID, "--actor", fixture.actor, "--home", fixture.homePath, "--json")
	if code != 0 {
		t.Fatalf("reauthorization crash retry code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairApplyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func assertEventStoreRepairCLIReauthorizationConverged(t *testing.T, fixture eventStoreRepairCLIReauthorizationFixture, report eventStoreRepairApplyReport, finalDecisionID string) {
	t.Helper()
	if !report.Changed || report.Replayed || report.PolicyDecisionID != finalDecisionID || len(report.Result.AuthorizationHistory) != 2 || report.Result.AuthorizationHistory[0].PolicyDecisionID != fixture.initialDecision.ID || report.Result.AuthorizationHistory[1].PolicyDecisionID != finalDecisionID {
		t.Fatalf("reauthorization did not converge exactly once: %+v", report)
	}
	verifyOut, verifyErr, verifyCode := runEventStoreCLI(t, "migrate", "--verify", "--home", fixture.homePath, "--json")
	if verifyCode != 0 {
		t.Fatalf("reauthorization crash retry verify code=%d stderr=%s stdout=%s", verifyCode, verifyErr, verifyOut)
	}
}

func assertEventStoreRepairStatusNoMutation(t *testing.T, homePath string, args ...string) eventStoreRepairStatusTestReport {
	t.Helper()
	before := snapshotHomeFileHashes(t, homePath)
	cliArgs := append([]string{"repair", "status", "--home", homePath, "--json"}, args...)
	out, stderr, code := runEventStoreCLI(t, cliArgs...)
	after := snapshotHomeFileHashes(t, homePath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("repair status mutated COMPOUND_HOME:\nbefore=%v\nafter=%v", before, after)
	}
	if code != 0 {
		t.Fatalf("repair status code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	var report eventStoreRepairStatusTestReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode repair status: %v\n%s", err, out)
	}
	return report
}

func snapshotHomeFileHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		files[rel] = "sha256:" + hex.EncodeToString(sum[:])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertEventStoreRepairNextCommandParses(t *testing.T, report eventStoreRepairStatusTestReport, wantVerb string) {
	t.Helper()
	type verbSpec struct {
		positionals int
		flags       map[string]bool
	}
	specs := map[string]verbSpec{
		"plan":      {flags: map[string]bool{"--home": true}},
		"review":    {positionals: 1, flags: map[string]bool{"--home": true, "--reviewer": true, "--decision": true, "--attest-writer-stopped": false}},
		"apply":     {positionals: 1, flags: map[string]bool{"--home": true, "--review": true, "--work": true, "--actor": true}},
		"supersede": {positionals: 1, flags: map[string]bool{"--home": true, "--successor": true, "--review": true, "--work": true, "--actor": true}},
	}
	spec, ok := specs[wantVerb]
	if !ok {
		t.Fatalf("test has no parser spec for %s", wantVerb)
	}
	argv := report.NextCommandArgv
	if len(argv) < 4+spec.positionals || !reflect.DeepEqual(argv[:3], []string{"compound", "event-store", "repair"}) || argv[3] != wantVerb {
		t.Fatalf("next_command_argv = %v, want repair %s prefix", argv, wantVerb)
	}
	parserArgs := argv[4+spec.positionals:]
	if _, _, err := parseEventStoreRepairFlags(parserArgs, spec.flags); err != nil {
		t.Fatalf("next command for %s does not parse with the verb's real flags: argv=%v err=%v", wantVerb, argv, err)
	}
	if report.NextCommand == "" {
		t.Fatalf("next command for %s is empty despite argv=%v", wantVerb, argv)
	}
}

func setupEventStoreRepairHealthyCLI(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_initial_healthy", "initial")
	if out, stderr, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("complete healthy migration code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	return homePath
}

func planEventStoreRepairCLI(t *testing.T, homePath string) eventstore.PostCompletionRepairPlanRecord {
	t.Helper()
	out, stderr, code := runEventStoreCLI(t, "repair", "plan", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%s", code, stderr)
	}
	var planned eventstore.PostCompletionRepairPlanRecord
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatal(err)
	}
	return planned
}

func planAndReviewExistingEventStoreRepairCLI(t *testing.T, homePath string, planned eventstore.PostCompletionRepairPlanRecord) (eventstore.PostCompletionRepairPlanRecord, string) {
	t.Helper()
	_, stderr, code := runEventStoreCLI(t, "repair", "review", planned.PlanPath,
		"--reviewer", "operator:test", "--decision", "authorize", "--attest-writer-stopped", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("review code=%d stderr=%s", code, stderr)
	}
	return planned, filepath.Join(filepath.Dir(planned.PlanPath), planned.Plan.ID+".review.json")
}

func planAndReviewEventStoreRepairCLI(t *testing.T, homePath string) (eventstore.PostCompletionRepairPlanRecord, string) {
	t.Helper()
	planned := planEventStoreRepairCLI(t, homePath)
	return planAndReviewExistingEventStoreRepairCLI(t, homePath, planned)
}

func assertRepairCLILoosePresent(t *testing.T, homePath string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(homePath, "events", "rec_cli_post_cutover.json")); err != nil {
		t.Fatalf("repair refusal mutated loose source: %v", err)
	}
}

func setupEventStoreRepairCLI(t *testing.T) (homePath, workID, actor string) {
	t.Helper()
	priorObservationInterval := eventStoreRepairObservationInterval
	eventStoreRepairObservationInterval = 0
	t.Cleanup(func() { eventStoreRepairObservationInterval = priorObservationInterval })
	homePath = t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_initial", "initial")
	if _, stderr, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("complete initial migration code=%d stderr=%s", code, stderr)
	}
	seedLooseCLIRecord(t, homePath, "rec_cli_post_cutover", "late")

	actor = "codex:eventstore-repair"
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "eventstore", Actor: actor, SuccessPredicate: "post-completion loose records are archived and verify is healthy", State: "ready"}, cliTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: actor}, cliTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return homePath, w.ID, actor
}

func createRunningEventStoreRepairWorkAt(t *testing.T, homePath, actor string, createdAt time.Time) string {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "eventstore", Actor: actor, SuccessPredicate: "future causal predecessor is rejected before mutation", State: "ready"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, createdAt.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: actor}, createdAt.Add(2*time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

// FALSIFIER (refusal floor): rebaseline refuses outright — in BOTH preview and
// --confirm — when the home never completed migration; there is nothing to
// re-baseline yet, and no sidecar is written.
func TestEventStoreRebaselineRefusesWhenMigrationIncomplete(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()

	out, errStr, code := runEventStoreCLI(t, "rebaseline", "--home", homePath)
	if code == 0 {
		t.Fatalf("expected non-zero exit with no completed migration; stdout=%q", out)
	}
	if !strings.Contains(errStr, "migration is not complete") {
		t.Fatalf("expected a migration-incomplete message, got stderr=%q", errStr)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "migration", "stream-baseline.json")); !os.IsNotExist(err) {
		t.Fatalf("no sidecar should exist: stat err=%v", err)
	}

	_, errStr, code = runEventStoreCLI(t, "rebaseline", "--confirm", "--work", "work_incomplete", "--home", homePath)
	if code == 0 {
		t.Fatalf("expected non-zero exit on --confirm with no completed migration")
	}
	if !strings.Contains(errStr, "migration is not complete") {
		t.Fatalf("expected a migration-incomplete message on confirm, got stderr=%q", errStr)
	}
}

// FALSIFIER F6 (preview render): the dry-run default writes nothing and
// projects current -> proposed sizes for each frozen stream.
func TestEventStoreRebaselineDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	seedLooseCLIRecord(t, homePath, "rec_rb_dry", "x")
	streamPath := filepath.Join(homePath, "events", "hook-events.ndjson")
	if err := os.MkdirAll(filepath.Dir(streamPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath, []byte(`{"id":"pre"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("apply exit=%d", code)
	}
	sidecarPath := filepath.Join(homePath, "events", "migration", "stream-baseline.json")
	before, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("expected the auto-written sidecar after apply: %v", err)
	}

	out, errStr, code := runEventStoreCLI(t, "rebaseline", "--home", homePath)
	if code != 0 {
		t.Fatalf("dry-run should succeed, got code=%d stderr=%q", code, errStr)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run output should say so, got %q", out)
	}
	if !strings.Contains(out, "proposed=") {
		t.Fatalf("dry-run should project proposed sizes, got %q", out)
	}
	after, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry-run must not write the sidecar")
	}
}

// FALSIFIER F4 (governance floor): --confirm WITHOUT an allowing PolicyDecision
// refuses and leaves the sidecar byte-identical.
func TestEventStoreRebaselineRefusesWithoutGrant(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	seedLooseCLIRecord(t, homePath, "rec_rb_nogrant", "x")
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("apply exit=%d", code)
	}
	sidecarPath := filepath.Join(homePath, "events", "migration", "stream-baseline.json")
	before, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(homePath, "events", "migration", ".rebaseline.lock")
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	homeBefore := snapshotHomeFiles(t, homePath)

	out, errStr, code := runEventStoreCLI(t, "rebaseline", "--confirm", "--work", "work_nogrant", "--home", homePath)
	if code == 0 {
		t.Fatalf("expected non-zero exit with no allowing PolicyDecision; stdout=%q", out)
	}
	if !strings.Contains(errStr, "refused") {
		t.Fatalf("expected a refusal message, got stderr=%q", errStr)
	}
	after, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("sidecar mutated despite refusal")
	}
	if entries, err := os.ReadDir(filepath.Join(homePath, "artifacts", "sha256")); err == nil && len(entries) != 0 {
		t.Fatalf("refused confirm wrote artifact bodies: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "events", "migration", "rebaseline-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("refused confirm wrote pending journal: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("refused confirm created rebaseline lock: %v", err)
	}
	if homeAfter := snapshotHomeFiles(t, homePath); !reflect.DeepEqual(homeAfter, homeBefore) {
		t.Fatalf("refused confirm changed home:\nbefore=%v\nafter=%v", homeBefore, homeAfter)
	}
}

func snapshotHomeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// --confirm requires --work (a governed act belongs to a work), mirroring `compound act`.
func TestEventStoreRebaselineConfirmRequiresWork(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	seedLooseCLIRecord(t, homePath, "rec_rb_nowork", "x")
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("apply exit=%d", code)
	}
	if _, _, code := runEventStoreCLI(t, "rebaseline", "--confirm", "--home", homePath); code != 2 {
		t.Fatalf("expected exit=2 without --work, got %d", code)
	}
}

// FALSIFIERS F2 (re-baseline path) + F6 (confirm render): a completed home
// whose hook-events.ndjson legitimately grew reports degraded; a GRANTED
// --confirm performs the governed write, records a resolvable receipt +
// transition, and `migrate --verify` reports ok again afterward.
func TestEventStoreRebaselineWritesWithGrantAndClosesTheGateAgain(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	seedLooseCLIRecord(t, homePath, "rec_rb_grant", "x")
	streamPath := filepath.Join(homePath, "events", "hook-events.ndjson")
	if err := os.MkdirAll(filepath.Dir(streamPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath, []byte(`{"id":"pre"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runEventStoreCLI(t, "migrate", "--apply", "--home", homePath); code != 0 {
		t.Fatalf("apply exit=%d", code)
	}

	// Legitimate growth AFTER completion (the gap this whole packet closes).
	f, err := os.OpenFile(streamPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"grew"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if out, _, code := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath); code == 0 {
		t.Fatalf("expected verify to report degraded before rebaseline:\n%s", out)
	}

	sidecarPath := filepath.Join(homePath, "events", "migration", "stream-baseline.json")
	grantSrc := filepath.Join(homePath, "rebaseline-grant.yaml")
	raw := "generated_at: \"2026-07-16T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-rebaseline\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_rebaseline\n" +
		"    capabilities: [\"" + eventStoreRebaselineCapability + "\"]\n" +
		"    target_scope: \"" + sidecarPath + "\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: reversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSrc, []byte(raw), 0o644); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSrc, time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}

	out, errStr, code := runEventStoreCLI(t, "rebaseline", "--confirm", "--work", "work_rebaseline", "--home", homePath, "--json")
	if code != 0 {
		t.Fatalf("granted confirm should succeed, got code=%d stderr=%q\n%s", code, errStr, out)
	}
	var result struct {
		ReceiptID       string               `json:"receipt_id"`
		TransitionID    string               `json:"transition_id"`
		PolicySource    string               `json:"policy_source"`
		PolicyAuthority policy.AuthorityFace `json:"policy_authority"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse rebaseline output json: %v (%q)", err, out)
	}
	if result.ReceiptID == "" || result.TransitionID == "" {
		t.Fatalf("expected receipt_id and transition_id in output: %q", out)
	}
	if result.PolicySource == "" || !result.PolicyAuthority.Current || !result.PolicyAuthority.Allowed {
		t.Fatalf("confirmed JSON omitted current allowing authority: %+v", result)
	}
	if !testEventExists(t, homePath, result.ReceiptID) {
		t.Fatalf("receipt %s must resolve in the canonical event store", result.ReceiptID)
	}
	if !testEventExists(t, homePath, result.TransitionID) {
		t.Fatalf("transition %s must resolve in the canonical event store", result.TransitionID)
	}

	if out2, errStr2, code2 := runEventStoreCLI(t, "migrate", "--verify", "--home", homePath); code2 != 0 {
		t.Fatalf("verify should report ok after rebaseline: code=%d stderr=%s\n%s", code2, errStr2, out2)
	}
}
