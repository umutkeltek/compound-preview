package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const capabilityLifecycleReviewJSONErrorSubEnv = "COMPOUND_TEST_LIFECYCLE_REVIEW_JSON_ERROR"

func lifecycleReviewCLIFixture(t *testing.T) (string, capgraph.CapabilityLifecycleProposalSnapshot) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	proposal, err := capgraph.BuildCapabilityLifecycleProposal(capgraph.CapabilityLifecycleProposalInput{
		ProposalKind: "revise", Subject: "session-mined skill update",
		Statement:    "teach the existing skill one verified failure pattern",
		EvidenceRefs: []string{"session:fixture", "check:fixture"}, CapabilityRef: "skill:fixture",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordCapabilityLifecycleProposal(homePath, proposal); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capgraph.LoadCapabilityLifecycleProposalSnapshot(homePath, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	return homePath, snapshot
}

func TestCapabilityLifecycleReviewCLIPreviewsThenCreatesOneProposedWork(t *testing.T) {
	homePath, snapshot := lifecycleReviewCLIFixture(t)
	args := []string{
		"lifecycle-review", "--proposal", snapshot.Proposal.ID,
		"--expected-hash", snapshot.Hash,
		"--decision", capgraph.CapabilityLifecycleOpenWork,
		"--rationale", "Create bounded owner work without mutating capability authority.",
		"--actor", "operator-test", "--json",
	}

	previewOut := captureStdout(t, func() { cmdCapability(append(append([]string{}, args...), "--preview")) })
	var preview CapabilityLifecycleReviewResult
	if err := json.Unmarshal([]byte(previewOut), &preview); err != nil {
		t.Fatalf("parse preview: %v\n%s", err, previewOut)
	}
	if preview.Status != "preview" || !preview.Preview || preview.WorkPlan == nil || preview.WorkPlan.PlanPath != "" {
		t.Fatalf("preview = %+v", preview)
	}
	if !strings.Contains(preview.StateDelta.Summary, "would move") || strings.Contains(preview.StateDelta.Summary, "created") {
		t.Fatalf("preview must describe a predicted, not completed, delta: %+v", preview.StateDelta)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(preview.WorkPlan.WorkID); err == nil {
		t.Fatal("preview must not create work")
	}

	commitOut := captureStdout(t, func() { cmdCapability(args) })
	var committed CapabilityLifecycleReviewResult
	if err := json.Unmarshal([]byte(commitOut), &committed); err != nil {
		t.Fatalf("parse commit: %v\n%s", err, commitOut)
	}
	if committed.Status != "recorded" || committed.Disposition == nil || !committed.Disposition.Changed || committed.Admission == nil {
		t.Fatalf("commit = %+v", committed)
	}
	created, err := store.Get(committed.Disposition.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "proposed" || created.Owner != "capability-graph" {
		t.Fatalf("created work = %+v", created)
	}
	_, events, err := store.Show(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !hasAttachedPrefix(events[0].Attached, "create_plan:") || !hasAttachedPrefix(events[0].Attached, "action_context_snapshot:") {
		t.Fatalf("work event attachments = %+v", events)
	}
	createPlansBefore := countFilesWithSuffix(t, filepath.Join(homePath, "create-plans"), ".json")

	replayOut := captureStdout(t, func() { cmdCapability(args) })
	var replay CapabilityLifecycleReviewResult
	if err := json.Unmarshal([]byte(replayOut), &replay); err != nil {
		t.Fatalf("parse replay: %v\n%s", err, replayOut)
	}
	if replay.Status != "already_recorded" || replay.Disposition == nil || replay.Disposition.Changed || replay.Disposition.ID != committed.Disposition.ID || replay.Disposition.WorkID != created.ID {
		t.Fatalf("replay = %+v", replay)
	}
	_, replayEvents, err := store.Show(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayEvents) != 1 || countFilesWithSuffix(t, filepath.Join(homePath, "create-plans"), ".json") != createPlansBefore {
		t.Fatalf("replay created duplicate artifacts: events=%+v", replayEvents)
	}
	if _, err := os.Stat(filepath.Join(homePath, "capability", "nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("lifecycle review mutated CapabilityGraph, stat err=%v", err)
	}
}

func TestCapabilityLifecycleReviewCLIRejectAndDeferMoveOnlyReviewState(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		homePath, snapshot := lifecycleReviewCLIFixture(t)
		result, err := runCapabilityLifecycleReview(homePath, []string{
			"--proposal", snapshot.Proposal.ID, "--expected-hash", snapshot.Hash,
			"--decision", capgraph.CapabilityLifecycleReject,
			"--rationale", "The evidence does not justify owner work.", "--actor", "operator-test",
		}, cliTestNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if result.Disposition == nil || result.Disposition.AfterBand != reviewartifact.BandFiled || result.Disposition.WorkID != "" {
			t.Fatalf("reject = %+v", result)
		}
		if countFilesWithSuffix(t, filepath.Join(homePath, "work", "work"), ".json") != 0 {
			t.Fatal("reject must not create work")
		}
	})

	t.Run("defer", func(t *testing.T) {
		homePath, snapshot := lifecycleReviewCLIFixture(t)
		deferUntil := cliTestNow.Add(2 * time.Hour).Format(time.RFC3339)
		result, err := runCapabilityLifecycleReview(homePath, []string{
			"--proposal", snapshot.Proposal.ID, "--expected-hash", snapshot.Hash,
			"--decision", capgraph.CapabilityLifecycleDefer,
			"--rationale", "Wait for the named evaluation.", "--defer-until", deferUntil,
			"--actor", "operator-test",
		}, cliTestNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if result.Disposition == nil || result.Disposition.AfterBand != reviewartifact.BandWatching || result.Disposition.DeferUntil != deferUntil {
			t.Fatalf("defer = %+v", result)
		}
		if countFilesWithSuffix(t, filepath.Join(homePath, "work", "work"), ".json") != 0 {
			t.Fatal("defer must not create work")
		}
	})
}

func TestCapabilityLifecycleReviewCLIRetryAfterFinalWriteFailureReusesCreatePlan(t *testing.T) {
	homePath, snapshot := lifecycleReviewCLIFixture(t)
	rationale := "Create one bounded work item and recover the exact admission after a crash."
	actor := "operator-test"
	args := []string{
		"--proposal", snapshot.Proposal.ID, "--expected-hash", snapshot.Hash,
		"--decision", capgraph.CapabilityLifecycleOpenWork,
		"--rationale", rationale, "--actor", actor,
	}
	dir, err := reviewartifact.Dir(homePath, capgraph.CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	dispositionID := reviewartifact.ID("cld_", snapshot.Proposal.ID, snapshot.Hash, capgraph.CapabilityLifecycleOpenWork, rationale, actor, "")
	finalPath := filepath.Join(dir, snapshot.Proposal.ID+"."+dispositionID+".decision.json")
	if err := os.Mkdir(finalPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runCapabilityLifecycleReview(homePath, args, cliTestNow.Add(time.Minute)); err == nil {
		t.Fatal("sabotaged final disposition write should fail after work admission")
	}
	if got := countFilesWithSuffix(t, filepath.Join(homePath, "create-plans"), ".json"); got != 1 {
		t.Fatalf("create plans after interrupted commit = %d, want 1", got)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	recovered, err := runCapabilityLifecycleReview(homePath, args, cliTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Disposition == nil || recovered.Admission == nil || recovered.Admission.Status != "already_admitted" {
		t.Fatalf("recovered lifecycle review = %+v", recovered)
	}
	if got := countFilesWithSuffix(t, filepath.Join(homePath, "create-plans"), ".json"); got != 1 {
		t.Fatalf("create plans after exact retry = %d, want original plan only", got)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Show(recovered.Disposition.WorkID)
	if err != nil || len(events) != 1 {
		t.Fatalf("recovered work events = %+v, err=%v", events, err)
	}
}

func TestCapabilityLifecycleReviewCLIStaleHashHasZeroSideEffects(t *testing.T) {
	homePath, snapshot := lifecycleReviewCLIFixture(t)
	_, err := runCapabilityLifecycleReview(homePath, []string{
		"--proposal", snapshot.Proposal.ID, "--expected-hash", strings.Repeat("0", 64),
		"--decision", capgraph.CapabilityLifecycleReject,
		"--rationale", "stale decision", "--actor", "operator-test",
	}, cliTestNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatalf("stale hash should fail, got %v", err)
	}
	if countFilesWithSuffix(t, filepath.Join(homePath, "reviews", capgraph.CapabilityLifecycleProposalReviewArea), ".decision.json") != 0 || countFilesWithSuffix(t, filepath.Join(homePath, "work", "work"), ".json") != 0 {
		t.Fatal("stale decision created side effects")
	}
}

func TestCapabilityLifecycleReviewCLIJSONErrorCarriesStableConflictCode(t *testing.T) {
	if os.Getenv(capabilityLifecycleReviewJSONErrorSubEnv) == "1" {
		cmdCapability([]string{
			"lifecycle-review",
			"--proposal", os.Getenv("COMPOUND_TEST_LIFECYCLE_PROPOSAL_ID"),
			"--expected-hash", strings.Repeat("0", 64),
			"--decision", capgraph.CapabilityLifecycleReject,
			"--rationale", "stale projected decision",
			"--actor", "operator-test",
			"--json",
		})
		return
	}

	homePath, snapshot := lifecycleReviewCLIFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCapabilityLifecycleReviewCLIJSONErrorCarriesStableConflictCode$")
	cmd.Env = append(os.Environ(),
		capabilityLifecycleReviewJSONErrorSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_TEST_LIFECYCLE_PROPOSAL_ID="+snapshot.Proposal.ID,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("stale JSON command should exit 1, err=%v output=%s", err, output)
	}
	start := strings.IndexByte(string(output), '{')
	if start < 0 {
		t.Fatalf("JSON error output missing object: %s", output)
	}
	var result CapabilityLifecycleReviewErrorResult
	if err := json.Unmarshal(output[start:], &result); err != nil {
		t.Fatalf("parse JSON error: %v\n%s", err, output)
	}
	if result.Status != "error" || result.ErrorCode != capgraph.CapabilityLifecycleErrorStaleRevision {
		t.Fatalf("JSON error result = %+v", result)
	}
}

func hasAttachedPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func countFilesWithSuffix(t *testing.T, root, suffix string) int {
	t.Helper()
	count := 0
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry != nil && !entry.IsDir() && strings.HasSuffix(path, suffix) {
			count++
		}
		return nil
	})
	return count
}
