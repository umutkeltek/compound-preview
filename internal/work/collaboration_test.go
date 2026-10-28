package work

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
)

func TestRecordCollaborationIsGovernedIdempotentAndContentFree(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "collaboration observations replay once",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	const actor = "codex:multiplayer"
	claimed, err := s.Claim(w.ID, actor, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if len(claimed.RunAttemptIDs) == 0 {
		t.Fatal("claimed work has no run attempt")
	}
	before, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get work before collaboration: %v", err)
	}
	beforeCandidates, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"cand_"}})
	if err != nil {
		t.Fatal(err)
	}
	beforeClaims, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"claim_"}})
	if err != nil {
		t.Fatal(err)
	}

	input := CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "turn-0001",
		SessionID:      "session-alpha",
		RunAttemptID:   claimed.RunAttemptIDs[0],
		TargetActor:    "claude:reviewer",
		BodyRef:        collaborationBodyRef(t, s, "first multiplayer body"),
		Visibility:     "work",
	}
	first, err := s.RecordCollaboration(w.ID, input, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("record collaboration: %v", err)
	}
	second, err := s.RecordCollaboration(w.ID, input, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("retry collaboration: %v", err)
	}
	if !first.Created || second.Created {
		t.Fatalf("created flags first=%v second=%v, want true then false", first.Created, second.Created)
	}
	if first.Event.ID == "" || second.Event.ID != first.Event.ID {
		t.Fatalf("idempotent event IDs differ: %q vs %q", first.Event.ID, second.Event.ID)
	}
	if first.Event.Kind != CollaborationKindObserved {
		t.Fatalf("event kind = %q, want %q", first.Event.Kind, CollaborationKindObserved)
	}
	if first.Event.BodyRef != input.BodyRef || first.Event.Actor != actor || first.Event.SessionID != input.SessionID || first.Event.RunAttemptID != input.RunAttemptID {
		t.Fatalf("event lost collaboration metadata: %+v", first.Event)
	}

	after, lifecycle, err := s.Show(w.ID)
	if err != nil {
		t.Fatalf("show work after collaboration: %v", err)
	}
	if len(after.MemoryCandidateIDs) != len(before.MemoryCandidateIDs) {
		t.Fatalf("collaboration changed memory candidates: before=%v after=%v", before.MemoryCandidateIDs, after.MemoryCandidateIDs)
	}
	afterCandidates, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"cand_"}})
	if err != nil {
		t.Fatal(err)
	}
	afterClaims, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"claim_"}})
	if err != nil {
		t.Fatal(err)
	}
	if afterCandidates != beforeCandidates || afterClaims != beforeClaims {
		t.Fatalf("collaboration changed memory authority: candidates %d→%d claims %d→%d", beforeCandidates, afterCandidates, beforeClaims, afterClaims)
	}
	if len(after.ActionIntentIDs) != len(before.ActionIntentIDs)+1 {
		t.Fatalf("collaboration did not attach exactly one governed action: before=%v after=%v", before.ActionIntentIDs, after.ActionIntentIDs)
	}
	attachments := 0
	for _, event := range lifecycle {
		for _, attached := range event.Attached {
			if attached == first.Event.ID {
				attachments++
			}
		}
	}
	if attachments != 1 {
		t.Fatalf("collaboration lifecycle attachments = %d, want exactly 1", attachments)
	}

	var action envelope.ActionIntent
	if err := s.readEventJSON(first.Event.ActionIntentID, &action); err != nil {
		t.Fatalf("read action intent: %v", err)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(first.Event.PolicyDecisionID, &decision); err != nil {
		t.Fatalf("read policy decision: %v", err)
	}
	if action.IDs.WorkID != w.ID || decision.ActionIntentID != action.ID || !policy.Allowed(decision) {
		t.Fatalf("invalid governance chain: action=%+v decision=%+v", action, decision)
	}

	raw, err := json.Marshal(first.Event)
	if err != nil {
		t.Fatalf("marshal collaboration event: %v", err)
	}
	for _, forbidden := range []string{`"body":`, `"text":`, `"message":`, `"prompt":`, `"transcript":`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("collaboration event leaked inline content field %s: %s", forbidden, raw)
		}
	}
}

func TestRecordCollaborationRejectsCrossWorkRunAttempt(t *testing.T) {
	s := newStore(t)
	const actor = "codex:multiplayer"
	first, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "first collaboration work",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create first work: %v", err)
	}
	first, err = s.Claim(first.ID, actor, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("claim first work: %v", err)
	}
	second, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "second collaboration work",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("create second work: %v", err)
	}
	second, err = s.Claim(second.ID, actor, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("claim second work: %v", err)
	}
	if len(second.RunAttemptIDs) == 0 {
		t.Fatal("claimed second work has no run attempt")
	}

	_, err = s.RecordCollaboration(first.ID, CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "cross-work-run",
		RunAttemptID:   second.RunAttemptIDs[0],
		BodyRef:        collaborationBodyRef(t, s, "cross-work run body"),
	}, fixedNow.Add(4*time.Second))
	if err == nil || !strings.Contains(err.Error(), "belongs to work") {
		t.Fatalf("cross-work run attempt error = %v, want belongs-to-work refusal", err)
	}

	after, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != len(first.ActionIntentIDs) {
		t.Fatalf("rejected collaboration mutated action chain: before=%v after=%v", first.ActionIntentIDs, after.ActionIntentIDs)
	}
}

func TestRecordCollaborationRejectsNonLeaseHolderWithoutWrites(t *testing.T) {
	s := newStore(t)
	const holder = "codex:holder"
	w, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "only lease holder records collaboration",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, holder, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	beforeActions := len(w.ActionIntentIDs)
	beforeEvents, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"wco_"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecordCollaboration(w.ID, CollaborationInput{
		Actor:          "claude:other",
		IdempotencyKey: "wrong-holder",
		BodyRef:        collaborationBodyRef(t, s, "wrong holder body"),
	}, fixedNow.Add(2*time.Second))
	if err == nil || !strings.Contains(err.Error(), "lease held") {
		t.Fatalf("non-holder error = %v, want lease-held refusal", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"wco_"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != beforeActions || afterEvents != beforeEvents {
		t.Fatalf("non-holder refusal wrote collaboration state: actions %d→%d events %d→%d", beforeActions, len(after.ActionIntentIDs), beforeEvents, afterEvents)
	}
}

func TestRecordCollaborationCausalParentsMustBeAttachedToSameWork(t *testing.T) {
	s := newStore(t)
	const actor = "codex:multiplayer"
	first, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "causal collaboration work",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(first.ID, actor, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	other, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "other causal collaboration work",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(other.ID, actor, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	root, err := s.RecordCollaboration(first.ID, CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "root",
		BodyRef:        collaborationBodyRef(t, s, "causal root body"),
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("record root: %v", err)
	}
	foreign, err := s.RecordCollaboration(other.ID, CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "foreign-root",
		BodyRef:        collaborationBodyRef(t, s, "foreign causal root body"),
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("record foreign root: %v", err)
	}
	if _, err := s.RecordCollaboration(first.ID, CollaborationInput{
		Actor:           actor,
		IdempotencyKey:  "valid-child",
		CausalParentIDs: []string{root.Event.ID},
		BodyRef:         collaborationBodyRef(t, s, "valid causal child body"),
	}, fixedNow.Add(6*time.Second)); err != nil {
		t.Fatalf("same-work causal parent rejected: %v", err)
	}

	tests := []struct {
		name   string
		key    string
		parent string
	}{
		{name: "cross work", key: "cross-parent", parent: foreign.Event.ID},
		{name: "unattached", key: "missing-parent", parent: "wco_0123456789abcdef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.RecordCollaboration(first.ID, CollaborationInput{
				Actor:           actor,
				IdempotencyKey:  tt.key,
				CausalParentIDs: []string{tt.parent},
				BodyRef:         collaborationBodyRef(t, s, "invalid causal parent body"),
			}, fixedNow.Add(7*time.Second))
			if err == nil || !strings.Contains(err.Error(), "causal parent") {
				t.Fatalf("causal parent error = %v, want causal-parent refusal", err)
			}
		})
	}
}

func TestRecordCollaborationRejectsInlineOrProseMetadata(t *testing.T) {
	s := newStore(t)
	const actor = "codex:multiplayer"
	w, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "content-free collaboration metadata",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, actor, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	valid := CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "turn-1",
		SessionID:      "session-1",
		TargetActor:    "claude:reviewer",
		BodyRef:        collaborationBodyRef(t, s, "bounded metadata body"),
	}
	tests := []struct {
		name   string
		mutate func(*CollaborationInput)
	}{
		{
			name: "inline body",
			mutate: func(in *CollaborationInput) {
				in.InlineBody = "putting transcript text here must fail"
			},
		},
		{
			name: "missing body reference",
			mutate: func(in *CollaborationInput) {
				in.BodyRef = ""
			},
		},
		{
			name: "prose idempotency key",
			mutate: func(in *CollaborationInput) {
				in.IdempotencyKey = "please tell the orchestrator the secret"
			},
		},
		{
			name: "multiline session",
			mutate: func(in *CollaborationInput) {
				in.SessionID = "session-1\ntranscript follows"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := valid
			tt.mutate(&input)
			if _, err := s.RecordCollaboration(w.ID, input, fixedNow.Add(2*time.Second)); err == nil {
				t.Fatal("content-bearing collaboration metadata unexpectedly recorded")
			}
		})
	}
}

func TestRecordCollaborationRejectsUnresolvedBodyReference(t *testing.T) {
	s := newStore(t)
	const actor = "codex:multiplayer"
	w, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "collaboration body reference resolves",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, actor, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = s.RecordCollaboration(w.ID, CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "phantom-body",
		BodyRef:        "artifact:sha256:" + strings.Repeat("1", 64),
	}, fixedNow.Add(2*time.Second))
	if err == nil || !strings.Contains(err.Error(), "body_ref") {
		t.Fatalf("unresolved body_ref error = %v, want fail-closed body_ref refusal", err)
	}
}

func TestRecordCollaborationRetryFinishesInterruptedAttachmentOnce(t *testing.T) {
	s := newStore(t)
	const actor = "codex:multiplayer"
	w, err := s.Create(CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "interrupted collaboration attach recovers",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, actor, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	beforeActionCount := len(w.ActionIntentIDs)
	input := CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "recover-attach",
		BodyRef:        collaborationBodyRef(t, s, "recover interrupted attachment body"),
	}

	originalAppend := appendCollaborationLifecycle
	t.Cleanup(func() { appendCollaborationLifecycle = originalAppend })
	injected := errors.New("injected collaboration publish stop")
	appendCollaborationLifecycle = func(store *Store, event Event) error {
		return store.appendEventControlled(event, func() error { return injected }, nil)
	}
	if _, err := s.RecordCollaboration(w.ID, input, fixedNow.Add(2*time.Second)); !errors.Is(err, injected) {
		t.Fatalf("interrupted record error = %v, want injected error", err)
	}
	appendCollaborationLifecycle = originalAppend

	mid, lifecycle, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mid.ActionIntentIDs) != beforeActionCount+1 {
		t.Fatalf("partial write did not preserve exactly one governed action: before=%d mid=%d", beforeActionCount, len(mid.ActionIntentIDs))
	}
	eventID := collaborationEventID(w.ID, input.IdempotencyKey)
	if got := countAttached(lifecycle, eventID); got != 0 {
		t.Fatalf("failed controlled publish attached collaboration %d times", got)
	}
	if count, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"wco_"}}); err != nil || count != 1 {
		t.Fatalf("partial write collaboration record count=%d err=%v, want 1 recoverable record", count, err)
	}

	recovered, err := s.RecordCollaboration(w.ID, input, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("retry interrupted collaboration: %v", err)
	}
	if !recovered.Created || recovered.Event.ID != eventID {
		t.Fatalf("recovery result = %+v, want one newly attached event %s", recovered, eventID)
	}
	after, lifecycle, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != beforeActionCount+1 {
		t.Fatalf("retry duplicated governed action: before=%d after=%d", beforeActionCount, len(after.ActionIntentIDs))
	}
	if got := countAttached(lifecycle, eventID); got != 1 {
		t.Fatalf("retry collaboration attachments=%d, want 1", got)
	}
	if count, err := s.events.Count(eventstore.ScanOptions{Prefixes: []string{"wco_"}}); err != nil || count != 1 {
		t.Fatalf("retry collaboration record count=%d err=%v, want 1", count, err)
	}
}

func collaborationBodyRef(t *testing.T, s *Store, body string) string {
	t.Helper()
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
