package stream

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestReadAvailableStrictRejectsPartialFinalFrame(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(homePath, "events", "work.jsonl")
	valid := eventLine(t, "work_strict_finite", "", "running")
	if err := os.WriteFile(logPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}

	complete, cursor, err := ReadAvailableStrict(homePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 1 || cursor != int64(len(valid)) {
		t.Fatalf("strict complete replay = frames=%d cursor=%d, want one/%d", len(complete), cursor, len(valid))
	}

	partial := `{"ts":"2026-07-24T12:01:00Z","work_id":"work_strict_finite","from":"running","to":"running","attached":["wco_clearance`
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(partial); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	frames, failedCursor, err := ReadAvailableStrict(homePath, 0)
	if err == nil {
		t.Fatalf("strict finite replay accepted partial tail: frames=%+v cursor=%d", frames, failedCursor)
	}
	if len(frames) != 0 || failedCursor != 0 {
		t.Fatalf("strict finite replay returned partial result: frames=%+v cursor=%d", frames, failedCursor)
	}
	defaultFrames, defaultCursor, err := ReadAvailable(homePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultFrames) != 1 || defaultCursor != int64(len(valid)) {
		t.Fatalf("ordinary finite buffering changed: frames=%+v cursor=%d", defaultFrames, defaultCursor)
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("strict finite read mutated the work log")
	}
}

func TestReadAvailableProjectsGovernedCollaborationOnceByCursor(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(work.CreateInput{
		Owner:            "delivery",
		SuccessPredicate: "finite stream replay sees governed collaboration",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:delivery"
	claimed, err := store.Claim(item.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runAttemptID := claimed.RunAttemptIDs[len(claimed.RunAttemptIDs)-1]
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_stream_interrupt",
			CreatedAt: now.UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:       envelope.IDs{WorkID: item.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "work.execute", Action: "continue_run",
		Target:        envelope.ActionTarget{Kind: "work", ID: item.ID},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_stream_interrupt",
			CreatedAt: now.Add(time.Second).UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "policy", Name: "compound"},
			IDs:       envelope.IDs{WorkID: item.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{runAttemptID},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID, Outcome: "hold",
		Reasons: []string{"active action invalidated"}, Obligations: []string{},
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, " "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, " "); err != nil {
		t.Fatal(err)
	}
	if err := runattempt.Open(homePath).AttachActionIntent(runAttemptID, action.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("full collaboration body"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "stream-delivery-1",
		BodyRef:        bodyRef,
		DeliveryClass:  work.CollaborationDeliveryInterrupt,
		Delta: work.CollaborationStateDelta{
			Kind:       "run_attempt",
			SubjectRef: runAttemptID,
			Before:     "active",
			After:      "invalidated",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{runAttemptID},
			PolicyRefs: []string{decision.ID},
		},
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	frames, cursor, err := ReadAvailable(homePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	var collaborations []Frame
	for _, frame := range frames {
		if frame.Kind == KindCollaborationEvent {
			collaborations = append(collaborations, frame)
		}
	}
	if len(collaborations) != 1 ||
		collaborations[0].Collaboration == nil ||
		collaborations[0].Collaboration.ID != recorded.Event.ID ||
		cursor <= 0 {
		t.Fatalf("finite replay = frames=%+v cursor=%d", collaborations, cursor)
	}

	repeated, sameCursor, err := ReadAvailable(homePath, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated) != 0 || sameCursor != cursor {
		t.Fatalf("cursor replay duplicated frames=%+v cursor=%d want=%d", repeated, sameCursor, cursor)
	}
}
