package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkCollaborateCLIRecordsIdempotentObservation(t *testing.T) {
	t.Parallel()
	if os.Getenv("COMPOUND_TEST_WORK_COLLABORATE_HELPER") == "1" {
		separator := -1
		for i, arg := range os.Args {
			if arg == "--" {
				separator = i
				break
			}
		}
		if separator < 0 {
			os.Exit(2)
		}
		cmdWork(os.Args[separator+1:])
		os.Exit(0)
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "CLI records collaboration observation",
	}, time.Date(2026, 7, 24, 12, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:multiplayer"
	if _, err := store.Claim(w.ID, actor, time.Date(2026, 7, 24, 12, 0, 2, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("CLI collaboration body"))
	if err != nil {
		t.Fatal(err)
	}

	args := []string{
		"collaborate", w.ID,
		"--actor", actor,
		"--idempotency-key", "cli-turn-1",
		"--session", "cli-session",
		"--target-actor", "claude:reviewer",
		"--body-ref", bodyRef,
		"--delivery-class", work.CollaborationDeliveryNextBoundary,
		"--delta-kind", "policy",
		"--delta-subject", "policy:review",
		"--delta-before", "allow",
		"--delta-after", "hold",
		"--affects-work", w.ID,
		"--scope-ref", "scope:hookplan",
		"--dependency-ref", "dependency:stream",
		"--policy-ref", "policy:review",
		"--base-revision", "git:base",
		"--resource-ref", "resource:proof-battery",
		"--json",
	}
	first := runWorkCollaborateHelper(t, homePath, args)
	second := runWorkCollaborateHelper(t, homePath, args)
	if !first.Created || second.Created {
		t.Fatalf("CLI created flags first=%v second=%v, want true then false", first.Created, second.Created)
	}
	if first.Event.ID == "" || second.Event.ID != first.Event.ID {
		t.Fatalf("CLI event IDs differ: %q vs %q", first.Event.ID, second.Event.ID)
	}
	if first.Event.SessionID != "cli-session" || first.Event.TargetActor != "claude:reviewer" {
		t.Fatalf("CLI metadata missing: %+v", first.Event)
	}
	if first.Event.DeliveryClass != work.CollaborationDeliveryNextBoundary ||
		first.Event.Delta == nil || first.Event.Delta.Kind != "policy" ||
		first.Event.Delta.SubjectRef != "policy:review" ||
		first.Event.Delta.Before != "allow" || first.Event.Delta.After != "hold" ||
		first.Event.Affects == nil ||
		len(first.Event.Affects.WorkIDs) != 1 || first.Event.Affects.WorkIDs[0] != w.ID ||
		len(first.Event.Affects.ScopeRefs) != 1 ||
		len(first.Event.Affects.DependencyRefs) != 1 ||
		len(first.Event.Affects.PolicyRefs) != 1 ||
		len(first.Event.Affects.BaseRevisions) != 1 ||
		len(first.Event.Affects.ResourceRefs) != 1 {
		t.Fatalf("CLI delivery semantics missing: %+v", first.Event)
	}
}

func runWorkCollaborateHelper(t *testing.T, homePath string, args []string) work.CollaborationResult {
	t.Helper()
	commandArgs := []string{"-test.run=^TestWorkCollaborateCLIRecordsIdempotentObservation$", "--"}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(os.Args[0], commandArgs...)
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+homePath,
		"COMPOUND_TEST_WORK_COLLABORATE_HELPER=1",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("work collaborate CLI: %v\n%s", err, output)
	}
	var result work.CollaborationResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse work collaborate JSON: %v\n%s", err, output)
	}
	return result
}

func TestWorkCollaborateBodyCLIUsesExactActiveAttemptWithoutAcknowledgement(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(work.CreateInput{Owner: "delivery", SuccessPredicate: "CLI body door is exact"}, now)
	if err != nil {
		t.Fatal(err)
	}
	const (
		sender   = "codex:sender"
		receiver = "claude:receiver"
	)
	if _, err := store.Claim(item.ID, sender, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempt, err := runattempt.Open(homePath).Mint(item.ID, receiver, "claude-code", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(homePath)
	if attempt, err = attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if attempt, err = attempts.SetWorkspace(attempt.ID, workspace); err != nil {
		t.Fatal(err)
	}
	const bindingID = "wde_1111111111111111"
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(bindingID, map[string]any{
		"schema_version": "compound.work_delegate_event.v0.1",
		"id":             bindingID,
		"kind":           "work.delegate.spawned",
		"ts":             now.Add(2 * time.Second).UTC().Format(time.RFC3339),
		"work_id":        item.ID,
		"harness":        "claude-code",
		"holder":         receiver,
		"session":        "body-cli-session",
		"run_attempt_id": attempt.ID,
	}, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(item.ID, work.AdvanceInput{
		To: "claimed", Actor: sender, RunAttemptIDs: []string{attempt.ID},
		EvidenceRefs: []string{bindingID}, Note: "body CLI native session bound",
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.MarkRunning(attempt.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("production on-demand body"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: sender, IdempotencyKey: "body-cli", TargetActor: receiver,
		BodyRef: bodyRef, DeliveryClass: work.CollaborationDeliveryNextBoundary,
		Delta: work.CollaborationStateDelta{Kind: "work", SubjectRef: item.ID, After: "changed"},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{attempt.ID},
		},
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	commandArgs := []string{
		"-test.run=^TestWorkCollaborateCLIRecordsIdempotentObservation$", "--",
		"collaborate", "body", item.ID,
		"--event", recorded.Event.ID,
		"--actor", receiver,
		"--run-attempt", attempt.ID,
		"--session", "body-cli-session",
		"--json",
	}
	cmd := exec.Command(os.Args[0], commandArgs...)
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+homePath,
		"COMPOUND_TEST_WORK_COLLABORATE_HELPER=1",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("work collaborate body CLI: %v\n%s", err, output)
	}
	var got struct {
		EventID    string `json:"event_id"`
		BodyRef    string `json:"body_ref"`
		BodyBase64 []byte `json:"body_base64"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.EventID != recorded.Event.ID || got.BodyRef != bodyRef || string(got.BodyBase64) != "production on-demand body" {
		t.Fatalf("work collaborate body output = %+v", got)
	}
}
