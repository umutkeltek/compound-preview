package work

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
)

func TestRecordReceiptedActionPersistsExactGovernedOutcome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name         string
		outcome      string
		changed      bool
		failureCause *envelope.FailureCause
	}{
		{name: "completed", outcome: "completed", changed: true},
		{name: "failed", outcome: "failed", changed: false, failureCause: &envelope.FailureCause{SurfaceOutcome: "permission apply failed", Mechanism: "concurrent target mutation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := governedActionRunningWork(t, s, "operator:test")
			action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
			result, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
				Action:               action,
				PreEvaluatedDecision: &decision,
				ExpectedWorkState:    "running",
				Outcome:              tc.outcome,
				Changed:              tc.changed,
				Summary:              "permission activation " + tc.outcome,
				MoveType:             "control_plane_repair",
				ProofRefs:            []string{"artifact:permission-render"},
				BeforeRefs:           []string{"sha256:preimage"},
				AfterRefs:            []string{"sha256:postimage"},
				FilesChanged:         []string{"/synthetic/settings.json"},
				FailureCause:         tc.failureCause,
			}, fixedNow.Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision.ID != decision.ID || !reflect.DeepEqual(result.Decision, decision) {
				t.Fatalf("persisted decision differs from evaluated decision:\n got=%+v\nwant=%+v", result.Decision, decision)
			}
			if result.Receipt.Outcome != tc.outcome || result.Receipt.StateDelta.Changed != tc.changed ||
				!reflect.DeepEqual(result.Receipt.FailureCause, tc.failureCause) {
				t.Fatalf("receipt outcome = %+v", result.Receipt)
			}
			if !contains(result.Receipt.ProofRefs, "sha256:preimage") || !contains(result.Receipt.ProofRefs, "sha256:postimage") ||
				!contains(result.Transition.BeforeRefs, "sha256:preimage") || !contains(result.Transition.AfterRefs, "sha256:postimage") {
				t.Fatalf("pre/post proof refs missing: receipt=%v transition=%+v", result.Receipt.ProofRefs, result.Transition)
			}
			if len(result.Receipt.StateDelta.FilesChanged) != 1 || result.Receipt.StateDelta.FilesChanged[0] != "/synthetic/settings.json" {
				t.Fatalf("files_changed = %v", result.Receipt.StateDelta.FilesChanged)
			}
			if result.Action.IDs.WorkID != w.ID || result.Decision.IDs.WorkID != w.ID || result.Receipt.IDs.WorkID != w.ID || result.Transition.IDs.WorkID != w.ID {
				t.Fatalf("governed outcome broke work chain: %+v", result)
			}
		})
	}
}

func TestRecordReceiptedActionDirectCrashReplayReusesCompletionTime(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	action.Capability = "source_control.worktree_retire"
	action.Action = "retire_redundant_git_surface"
	action.Target = envelope.ActionTarget{Kind: "git_worktree", ID: "redundant-worktree", Path: "/synthetic/repo/worktree"}
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "redundant worktree retired", MoveType: "control_plane_repair",
		BeforeRefs: []string{"commit:before"}, AfterRefs: []string{"commit:after"},
	}
	firstCompletion := fixedNow.Add(2 * time.Minute)
	injected := errors.New("direct transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	if _, err := s.RecordReceiptedAction(w.ID, in, firstCompletion); !errors.Is(err, injected) {
		t.Fatalf("first direct RecordReceiptedAction error = %v", err)
	}
	receiptedActionAfterWrite = previous

	replayed, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("replay direct RecordReceiptedAction: %v", err)
	}
	if !replayed.Replayed || replayed.Action.ID != action.ID || replayed.Decision.ID != decision.ID {
		t.Fatalf("direct replay changed transaction identity: %+v", replayed)
	}
	want := firstCompletion.UTC().Format(time.RFC3339Nano)
	if replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
		t.Fatalf("direct replay completion time = receipt %s transition %s, want immutable %s", replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, want)
	}
}

func TestRecordReceiptedActionPendingReplayUsesSealedAuthorityAfterExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	t.Run("exact pending transaction converges", func(t *testing.T) {
		s, w, in, firstAt, expiresAt, _ := interruptedExpiringDirectGovernedAction(t)
		replayed, err := s.RecordReceiptedAction(w.ID, in, expiresAt.Add(time.Minute))
		if err != nil {
			t.Fatalf("replay exact direct transaction after decision expiry: %v", err)
		}
		want := firstAt.UTC().Format(time.RFC3339Nano)
		if !replayed.Replayed || replayed.Decision.ID != in.PreEvaluatedDecision.ID || replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
			t.Fatalf("post-expiry direct replay = replayed %v decision %s receipt %s transition %s, want exact decision %s completion %s", replayed.Replayed, replayed.Decision.ID, replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, in.PreEvaluatedDecision.ID, want)
		}
	})

	t.Run("different decision remains rejected", func(t *testing.T) {
		s, w, in, _, expiresAt, _ := interruptedExpiringDirectGovernedAction(t)
		different := in
		decision := *in.PreEvaluatedDecision
		decision.ID = "pol_permission_different"
		different.PreEvaluatedDecision = &decision
		if result, err := s.RecordReceiptedAction(w.ID, different, expiresAt.Add(time.Minute)); err == nil || result.Replayed || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("different post-expiry transaction replayed: result=%+v err=%v", result, err)
		}
	})

	t.Run("tampered pending digest remains rejected", func(t *testing.T) {
		s, w, in, _, expiresAt, pendingPath := interruptedExpiringDirectGovernedAction(t)
		body, err := os.ReadFile(pendingPath)
		if err != nil {
			t.Fatal(err)
		}
		var marker map[string]any
		if err := json.Unmarshal(body, &marker); err != nil {
			t.Fatal(err)
		}
		marker["transaction_digest"] = strings.Repeat("0", 64)
		body, err = json.Marshal(marker)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pendingPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if result, err := s.RecordReceiptedAction(w.ID, in, expiresAt.Add(time.Minute)); err == nil || result.Replayed || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("tampered post-expiry pending marker replayed: result=%+v err=%v", result, err)
		}
	})
}

func TestRecordReservedReceiptedActionCrashReplayUsesPostAuthorizationCompletionTime(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	authorizedAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("reserved transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	if _, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); !errors.Is(err, injected) {
		t.Fatalf("first reserved RecordReceiptedAction error = %v", err)
	}
	receiptedActionAfterWrite = previous

	pending, present, _, err := s.preflightReceiptedActionPending(w.ID)
	if err != nil || !present {
		t.Fatalf("read reserved pending marker: present=%v err=%v", present, err)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, pending.CompletedAt)
	if err != nil {
		t.Fatalf("pending completed_at = %q: %v", pending.CompletedAt, err)
	}
	if !completedAt.After(authorizedAt) {
		t.Fatalf("reserved completion time = %s, want later than authorization %s", completedAt, authorizedAt)
	}

	replayed, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatalf("replay reserved RecordReceiptedAction: %v", err)
	}
	want := completedAt.UTC().Format(time.RFC3339Nano)
	if !replayed.Replayed || replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
		t.Fatalf("reserved replay completion = replayed %v receipt %s transition %s, want immutable %s", replayed.Replayed, replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, want)
	}
}

func TestRecordReservedReceiptedActionFinalizesAfterDecisionExpiryAndReplaysExactly(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	authorizedAt := fixedNow.Add(2 * time.Minute)
	expiresAt := authorizedAt.Add(time.Minute)
	decision.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
		t.Fatalf("reserve before decision expiry: %v", err)
	}

	injected := errors.New("post-expiry reserved transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	if _, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); !errors.Is(err, injected) {
		t.Fatalf("finalize after decision expiry = %v, want physical publication to reach injected crash", err)
	}
	receiptedActionAfterWrite = previous

	pending, present, _, err := s.preflightReceiptedActionPending(w.ID)
	if err != nil || !present {
		t.Fatalf("read post-expiry pending marker: present=%v err=%v", present, err)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, pending.CompletedAt)
	if err != nil {
		t.Fatalf("pending completed_at = %q: %v", pending.CompletedAt, err)
	}
	if completedAt.Before(authorizedAt) || !completedAt.After(expiresAt) {
		t.Fatalf("completion time = %s, want not before authorization %s and after expiry %s", completedAt, authorizedAt, expiresAt)
	}

	replayed, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatalf("replay post-expiry reserved finalization: %v", err)
	}
	want := completedAt.UTC().Format(time.RFC3339Nano)
	if !replayed.Replayed || replayed.Decision.ID != decision.ID || replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
		t.Fatalf("post-expiry replay = replayed %v decision %s receipt %s transition %s, want exact decision %s completion %s", replayed.Replayed, replayed.Decision.ID, replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, decision.ID, want)
	}
}

func TestReceiptedActionPendingWritesV02WithCompletionTime(t *testing.T) {
	s, w, _, _, pendingPath := interruptedDirectGovernedAction(t)
	body, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(body, &marker); err != nil {
		t.Fatal(err)
	}
	if got := marker["schema_version"]; got != "compound.receipted_action_pending.v0.2" {
		t.Fatalf("pending schema_version = %v, want v0.2 for work %s in %s", got, w.ID, s.home)
	}
	if got, ok := marker["completed_at"].(string); !ok || strings.TrimSpace(got) == "" {
		t.Fatalf("pending completed_at = %v, want required timestamp", marker["completed_at"])
	}
}

func TestRecordReceiptedActionReplaysAuthenticV01PendingMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
		want   string
	}{
		{name: "exact HEAD marker", wantOK: true},
		{
			name: "HEAD marker still binds transaction digest",
			mutate: func(marker map[string]any) {
				marker["transaction_digest"] = strings.Repeat("0", 64)
			},
			want: "conflicts",
		},
		{
			name: "v0.2 cannot omit completion time",
			mutate: func(marker map[string]any) {
				marker["schema_version"] = "compound.receipted_action_pending.v0.2"
			},
			want: "completed_at",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w, in, authorizedAt, historicalCompletion, pendingPath := governedV01PendingFixture(t, false, "head")
			body, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			var marker map[string]any
			if err := json.Unmarshal(body, &marker); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(marker)
			}
			body, err = json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pendingPath, body, 0o644); err != nil {
				t.Fatal(err)
			}

			replayed, err := s.RecordReceiptedAction(w.ID, in, authorizedAt)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("replay authentic HEAD v0.1 marker: %v", err)
				}
				want := historicalCompletion.UTC().Format(time.RFC3339)
				if !replayed.Replayed || replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
					t.Fatalf("authentic HEAD replay = replayed %v receipt %s transition %s, want %s", replayed.Replayed, replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, want)
				}
				return
			}
			if err == nil || replayed.Replayed || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid pending marker replayed: result=%+v err=%v, want %q", replayed, err, tc.want)
			}
		})
	}
}

func TestRecordReservedReceiptedActionReplaysAuthenticV01PendingMarker(t *testing.T) {
	t.Parallel()
	s, w, in, authorizedAt, historicalCompletion, _ := governedV01PendingFixture(t, true, "head")
	replayed, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatalf("replay authentic HEAD reserved v0.1 marker: %v", err)
	}
	want := historicalCompletion.UTC().Format(time.RFC3339)
	if !replayed.Replayed || replayed.Receipt.CreatedAt != want || replayed.Transition.CreatedAt != want {
		t.Fatalf("authentic HEAD reserved replay = replayed %v receipt %s transition %s, want %s", replayed.Replayed, replayed.Receipt.CreatedAt, replayed.Transition.CreatedAt, want)
	}
}

func TestRecordReservedReceiptedActionRejectsV01SealedMaxTimestamp(t *testing.T) {
	t.Parallel()
	s, w, in, authorizedAt, _, pendingPath := governedV01PendingFixture(t, true, "sealed_max")
	pendingBefore, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	_, present, reservationPath, err := s.preflightReceiptedActionReservation(w.ID)
	if err != nil || !present {
		t.Fatalf("legacy reservation fixture missing: present=%v err=%v", present, err)
	}
	reservationBefore, err := os.ReadFile(reservationPath)
	if err != nil {
		t.Fatal(err)
	}

	replayed, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err == nil || !strings.Contains(err.Error(), "conflicts with every sealed historical transaction") {
		t.Fatalf("impossible v0.1 sealed-max marker was accepted: result=%+v err=%v", replayed, err)
	}
	if replayed.Replayed {
		t.Fatalf("impossible v0.1 marker reported replay: %+v", replayed)
	}
	if pendingAfter, readErr := os.ReadFile(pendingPath); readErr != nil || !bytes.Equal(pendingAfter, pendingBefore) {
		t.Fatalf("sealed-max refusal changed pending marker: err=%v", readErr)
	}
	if reservationAfter, readErr := os.ReadFile(reservationPath); readErr != nil || !bytes.Equal(reservationAfter, reservationBefore) {
		t.Fatalf("sealed-max refusal changed reservation: err=%v", readErr)
	}
	if _, actionPresent, actionErr := s.preflightReceiptedAction(in.Action); actionErr != nil || actionPresent {
		t.Fatalf("sealed-max refusal published action: present=%v err=%v", actionPresent, actionErr)
	}
}

// governedV01PendingFixture reproduces cbf6fd24's exact pending schema and
// digest body. The "head" algorithm is authentic: transactionNow came only
// from ActionIntent.created_at and newHeader formatted it as RFC3339. The
// sealed_max variant is a provenance-negative fixture that no released writer
// emitted and must therefore fail closed during compatibility recovery.
func governedV01PendingFixture(t *testing.T, reserved bool, algorithm string) (*Store, envelope.WorkEnvelope, ReceiptedActionInput, time.Time, time.Time, string) {
	t.Helper()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	decision.CreatedAt = fixedNow.Add(90 * time.Second).UTC().Format(time.RFC3339)
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	authorizedAt := fixedNow.Add(2 * time.Minute)
	if reserved {
		if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	historicalCompletion, err := time.Parse(time.RFC3339, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if algorithm == "sealed_max" {
		historicalCompletion = authorizedAt
	} else if algorithm != "head" {
		t.Fatalf("unknown historical fixture algorithm %q", algorithm)
	}
	proofRefs := mergeUnique(in.ProofRefs, action.EvidenceRefs, in.BeforeRefs, in.AfterRefs, []string{"work:" + w.ID})
	receiptID := idFor("rec_", "receipted-action", w.ID, action.ID, decision.ID, in.Outcome)
	transitionID := idFor("tr_", "receipted-action", w.ID, action.ID, decision.ID, in.Outcome)
	used := normalizeCapabilityRefs([]string{action.Capability})
	receipt := envelope.Receipt{
		Header:     newHeader(envelope.SVReceipt, receiptID, historicalCompletion, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Outcome:    in.Outcome,
		MoveType:   in.MoveType,
		StateDelta: envelope.StateDelta{Changed: in.Changed, Summary: in.Summary, FilesChanged: mergeUnique(in.FilesChanged), Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: used, Missed: []string{}, Candidate: []string{}, UsedDeclared: true},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  proofRefs,
	}
	receipt.EvidenceRefs = proofRefs
	transition := envelope.Transition{
		Header:     newHeader(envelope.SVTransition, transitionID, historicalCompletion, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		BeforeRefs: mergeUnique([]string{w.IDs.ContextPackID}, in.BeforeRefs),
		ActionRefs: []string{action.ID},
		AfterRefs:  mergeUnique([]string{receipt.ID}, in.AfterRefs),
		StateDelta: receipt.StateDelta.Summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: used, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
			Typed: closeProjectionRefs(receipt, w.IDs.ContextPackID),
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: in.Changed},
	}
	transition.EvidenceRefs = proofRefs
	payload := struct {
		Action     envelope.ActionIntent   `json:"action"`
		Decision   envelope.PolicyDecision `json:"decision"`
		Receipt    envelope.Receipt        `json:"receipt"`
		Transition envelope.Transition     `json:"transition"`
	}{Action: action, Decision: decision, Receipt: receipt, Transition: transition}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	marker := struct {
		SchemaVersion     string `json:"schema_version"`
		WorkID            string `json:"work_id"`
		ActionID          string `json:"action_intent_id"`
		PolicyDecisionID  string `json:"policy_decision_id"`
		ReceiptID         string `json:"receipt_id"`
		TransitionID      string `json:"transition_id"`
		CreatedAt         string `json:"created_at"`
		TransactionDigest string `json:"transaction_digest"`
	}{
		SchemaVersion: "compound.receipted_action_pending.v0.1", WorkID: w.ID, ActionID: action.ID,
		PolicyDecisionID: decision.ID, ReceiptID: receipt.ID, TransitionID: transition.ID,
		CreatedAt: action.CreatedAt, TransactionDigest: hex.EncodeToString(digest[:]),
	}
	markerBody, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(s.home, "work", "receipted-actions", w.ID+".json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, markerBody, 0o644); err != nil {
		t.Fatal(err)
	}
	return s, w, in, authorizedAt, historicalCompletion, pendingPath
}

func interruptedDirectGovernedAction(t *testing.T) (*Store, envelope.WorkEnvelope, ReceiptedActionInput, time.Time, string) {
	t.Helper()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	completionAt := fixedNow.Add(2 * time.Minute)
	injected := errors.New("direct transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	if _, err := s.RecordReceiptedAction(w.ID, in, completionAt); !errors.Is(err, injected) {
		t.Fatalf("first direct RecordReceiptedAction error = %v", err)
	}
	receiptedActionAfterWrite = previous
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	return s, w, in, completionAt, filepath.Join(s.home, "work", "receipted-actions", w.ID+".json")
}

func interruptedExpiringDirectGovernedAction(t *testing.T) (*Store, envelope.WorkEnvelope, ReceiptedActionInput, time.Time, time.Time, string) {
	t.Helper()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstAt := fixedNow.Add(2 * time.Minute)
	expiresAt := firstAt.Add(time.Minute)
	decision.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	injected := errors.New("direct expiring transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	if _, err := s.RecordReceiptedAction(w.ID, in, firstAt); !errors.Is(err, injected) {
		t.Fatalf("first direct RecordReceiptedAction before expiry = %v", err)
	}
	receiptedActionAfterWrite = previous
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	return s, w, in, firstAt, expiresAt, filepath.Join(s.home, "work", "receipted-actions", w.ID+".json")
}

func TestReceiptedActionReservationBlocksLifecycleUntilFinalizeOrAbort(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	newInput := func(w envelope.WorkEnvelope, action envelope.ActionIntent, decision envelope.PolicyDecision) ReceiptedActionInput {
		return ReceiptedActionInput{
			Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
			Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
			BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
		}
	}
	for _, tc := range []struct {
		name     string
		complete func(t *testing.T, s *Store, w envelope.WorkEnvelope, in ReceiptedActionInput, at time.Time)
	}{
		{name: "finalize", complete: func(t *testing.T, s *Store, w envelope.WorkEnvelope, in ReceiptedActionInput, at time.Time) {
			if result, err := s.RecordReservedReceiptedAction(w.ID, in, at); err != nil || result.Receipt.ID == "" {
				t.Fatalf("finalize reservation: result=%+v err=%v", result, err)
			}
		}},
		{name: "abort", complete: func(t *testing.T, s *Store, w envelope.WorkEnvelope, in ReceiptedActionInput, _ time.Time) {
			if err := s.AbortReceiptedActionReservation(w.ID, in.Action.ID); err != nil {
				t.Fatalf("abort reservation: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := governedActionRunningWork(t, s, "operator:test")
			action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
			in := newInput(w, action, decision)
			authorizedAt := fixedNow.Add(2 * time.Minute)
			if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(3*time.Minute)); err == nil ||
				!strings.Contains(err.Error(), "pending receipted action "+action.ID) {
				t.Fatalf("advance crossed reservation: %v", err)
			}
			tc.complete(t, s, w, in, authorizedAt)
			if advanced, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(4*time.Minute)); err != nil || advanced.State != "verifying" {
				t.Fatalf("advance after reservation completion: work=%+v err=%v", advanced, err)
			}
		})
	}
}

func TestReceiptedActionReservationAuthorizationFailureDoesNotBlockWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
	}
	injected := errors.New("authorization revoked")
	if err := s.ReserveReceiptedAction(w.ID, in, fixedNow.Add(2*time.Minute), func() error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("ReserveReceiptedAction error = %v", err)
	}
	if advanced, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(3*time.Minute)); err != nil || advanced.State != "verifying" {
		t.Fatalf("authorization failure left reservation: work=%+v err=%v", advanced, err)
	}
}

func TestReceiptedActionReservationSchemaVersionsRecoveryEvidence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	newReservation := func(t *testing.T) (*Store, envelope.WorkEnvelope, ReceiptedActionInput, time.Time, string) {
		t.Helper()
		s := newStore(t)
		w := governedActionRunningWork(t, s, "operator:test")
		action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
		in := ReceiptedActionInput{
			Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
			Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
			ProofRefs: []string{"policy-authority:current"},
		}
		authorizedAt := fixedNow.Add(2 * time.Minute)
		if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		return s, w, in, authorizedAt, filepath.Join(s.home, "work", "receipted-action-reservations", w.ID+".json")
	}

	t.Run("new writes use v0.2 with recoverable input", func(t *testing.T) {
		_, _, _, _, path := newReservation(t)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var reservation map[string]json.RawMessage
		if err := json.Unmarshal(body, &reservation); err != nil {
			t.Fatal(err)
		}
		var schemaVersion, reauthorizationDigest string
		if err := json.Unmarshal(reservation["schema_version"], &schemaVersion); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(reservation["reauthorization_digest"], &reauthorizationDigest); err != nil {
			t.Fatal(err)
		}
		if schemaVersion != "compound.receipted_action_reservation.v0.2" || len(reauthorizationDigest) != sha256.Size*2 || len(reservation["input"]) == 0 {
			t.Fatalf("new reservation lacks v0.2 recovery evidence: schema=%q digest=%q input=%s", schemaVersion, reauthorizationDigest, reservation["input"])
		}
	})

	t.Run("authentic v0.1 remains convergent", func(t *testing.T) {
		s, w, in, authorizedAt, path := newReservation(t)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var reservation map[string]json.RawMessage
		if err := json.Unmarshal(body, &reservation); err != nil {
			t.Fatal(err)
		}
		reservation["schema_version"] = json.RawMessage(`"compound.receipted_action_reservation.v0.1"`)
		delete(reservation, "reauthorization_digest")
		delete(reservation, "input")
		body, err = json.Marshal(reservation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if result, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err != nil || result.Decision.ID != in.PreEvaluatedDecision.ID {
			t.Fatalf("authentic v0.1 reservation did not converge: result=%+v err=%v", result, err)
		}
	})

	t.Run("v0.1 carrying v0.2-only fields fails closed", func(t *testing.T) {
		s, w, in, _, path := newReservation(t)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var reservation map[string]json.RawMessage
		if err := json.Unmarshal(body, &reservation); err != nil {
			t.Fatal(err)
		}
		reservation["schema_version"] = json.RawMessage(`"compound.receipted_action_reservation.v0.1"`)
		body, err = json.Marshal(reservation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.ReadReceiptedActionReservationInput(w.ID, in.Action.ID); err == nil || !strings.Contains(err.Error(), "v0.2-only") {
			t.Fatalf("v0.1 reservation smuggled v0.2 fields: %v", err)
		}
	})
}

func TestReceiptedActionReservationReauthorizationAtomicallyReplacesSameAction(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, firstDecision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstInput := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &firstDecision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		ProofRefs: []string{"policy-authority:first"},
	}
	firstAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, firstInput, firstAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	secondDecision := firstDecision
	secondDecision.ID = "pol_permission_reauthorized"
	secondDecision.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
	secondInput := firstInput
	secondInput.PreEvaluatedDecision = &secondDecision
	secondInput.ProofRefs = []string{"policy-authority:second"}
	secondAt := fixedNow.Add(4 * time.Minute)
	authorizations := 0
	authorize := func() error {
		authorizations++
		return nil
	}
	if err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, secondAt, authorize); err != nil {
		t.Fatal(err)
	}
	if err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, secondAt, authorize); err != nil {
		t.Fatalf("reauthorization replay: %v", err)
	}
	if authorizations != 2 {
		t.Fatalf("current authority callback count = %d, want 2", authorizations)
	}
	if _, err := s.RecordReservedReceiptedAction(w.ID, firstInput, firstAt); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("superseded reservation finalized: %v", err)
	}
	if result, err := s.RecordReservedReceiptedAction(w.ID, secondInput, secondAt); err != nil || result.Decision.ID != secondDecision.ID {
		t.Fatalf("finalize reauthorized reservation: result=%+v err=%v", result, err)
	}
}

func TestReceiptedActionReservationReauthorizationFailurePreservesPriorReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, firstDecision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstInput := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &firstDecision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
	}
	firstAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, firstInput, firstAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	secondDecision := firstDecision
	secondDecision.ID = "pol_permission_reauthorized"
	secondInput := firstInput
	secondInput.PreEvaluatedDecision = &secondDecision
	injected := errors.New("current authority revoked")
	if err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, fixedNow.Add(3*time.Minute), func() error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("reauthorization error = %v", err)
	}
	if result, err := s.RecordReservedReceiptedAction(w.ID, firstInput, firstAt); err != nil || result.Decision.ID != firstDecision.ID {
		t.Fatalf("prior reservation was not preserved: result=%+v err=%v", result, err)
	}
}

func TestReceiptedActionReservationReauthorizationRejectsChangedActionSemantics(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, firstDecision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstInput := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &firstDecision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		ProofRefs: []string{"policy-authority:first"},
	}
	firstAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, firstInput, firstAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	secondDecision := firstDecision
	secondDecision.ID = "pol_permission_reauthorized"
	secondInput := firstInput
	secondInput.Action.Target.Path = "/synthetic/other-settings.json"
	secondInput.PreEvaluatedDecision = &secondDecision
	secondInput.ProofRefs = []string{"policy-authority:second"}

	err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, fixedNow.Add(3*time.Minute), func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "preserve") {
		t.Fatalf("same-id changed action semantics were accepted: %v", err)
	}
	if result, err := s.RecordReservedReceiptedAction(w.ID, firstInput, firstAt); err != nil || result.Decision.ID != firstDecision.ID {
		t.Fatalf("changed-action refusal did not preserve prior reservation: result=%+v err=%v", result, err)
	}
}

func TestReceiptedActionReservationReauthorizationRejectsBackwardTime(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, firstDecision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstInput := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &firstDecision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		ProofRefs: []string{"policy-authority:first"},
	}
	firstAt := fixedNow.Add(4 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, firstInput, firstAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	secondDecision := firstDecision
	secondDecision.ID = "pol_permission_reauthorized"
	secondInput := firstInput
	secondInput.PreEvaluatedDecision = &secondDecision
	secondInput.ProofRefs = []string{"policy-authority:second"}

	err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, fixedNow.Add(3*time.Minute), func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "monotonic") {
		t.Fatalf("backward reauthorization time was accepted: %v", err)
	}
}

func TestReceiptedActionReservationReauthorizationConvergesFromUnknownIntermediateReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, firstDecision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	firstInput := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &firstDecision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		ProofRefs: []string{"eventstore-plan:stable", "policy-authority:first"},
	}
	firstAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, firstInput, firstAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	secondDecision := firstDecision
	secondDecision.ID = "pol_permission_intermediate"
	secondInput := firstInput
	secondInput.PreEvaluatedDecision = &secondDecision
	secondInput.ProofRefs = []string{"eventstore-plan:stable", "policy-authority:intermediate"}
	secondAt := fixedNow.Add(3 * time.Minute)
	if err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, secondInput, secondAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	thirdDecision := firstDecision
	thirdDecision.ID = "pol_permission_current"
	thirdInput := firstInput
	thirdInput.PreEvaluatedDecision = &thirdDecision
	thirdInput.ProofRefs = []string{"eventstore-plan:stable", "policy-authority:current"}
	thirdAt := fixedNow.Add(4 * time.Minute)
	if err := s.ReauthorizeReceiptedActionReservation(w.ID, firstInput, firstAt, thirdInput, thirdAt, func() error { return nil }); err != nil {
		t.Fatalf("stale predecessor could not converge the exact unpublished action: %v", err)
	}
	if result, err := s.RecordReservedReceiptedAction(w.ID, thirdInput, thirdAt); err != nil || result.Decision.ID != thirdDecision.ID {
		t.Fatalf("finalize converged reservation: result=%+v err=%v", result, err)
	}
}

func TestRecordReservedReceiptedActionReplaysCompletedOutcomeAfterLifecycleAdvances(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	authorizedAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	first, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	replay, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatalf("replay finalized reservation after lifecycle advance: %v", err)
	}
	if !replay.Replayed || replay.Receipt.ID != first.Receipt.ID || replay.Transition.ID != first.Transition.ID {
		t.Fatalf("replay = %+v, want exact completed transaction %+v", replay, first)
	}
}

func TestCompletedReceiptedActionPersistsExactOutputDigests(t *testing.T) {
	t.Parallel()
	s, w, in, authorizedAt, result := completedGovernedAction(t)
	path := filepath.Join(s.home, "work", "receipted-action-completions", w.ID, in.Action.ID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatal(err)
	}
	if got := completion["schema_version"]; got != "compound.receipted_action_completion.v0.1" {
		t.Fatalf("completion schema_version = %v", got)
	}
	for key, wantID := range map[string]string{
		"receipt_sha256":    result.Receipt.ID,
		"transition_sha256": result.Transition.ID,
		"event_sha256":      result.Action.ID,
	} {
		got, ok := completion[key].(string)
		if !ok || len(got) != 64 {
			t.Fatalf("completion %s = %v, want SHA-256 binding for %s", key, completion[key], wantID)
		}
	}
	if replay, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err != nil || !replay.Replayed {
		t.Fatalf("exact completion did not replay: result=%+v err=%v", replay, err)
	}
}

func TestCompletedReceiptedActionRejectsLegacyAndMissingExactOutputDigests(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	baselineStore, w, in, authorizedAt, _ := completedGovernedAction(t)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "legacy completion schema",
			mutate: func(completion map[string]any) {
				completion["schema_version"] = receiptedActionReservationSchemaVersion
				delete(completion, "receipt_sha256")
				delete(completion, "transition_sha256")
				delete(completion, "event_sha256")
			},
			want: "schema_version",
		},
		{name: "missing receipt digest", mutate: func(completion map[string]any) { delete(completion, "receipt_sha256") }, want: "receipt_sha256"},
		{name: "missing transition digest", mutate: func(completion map[string]any) { delete(completion, "transition_sha256") }, want: "transition_sha256"},
		{name: "missing event digest", mutate: func(completion map[string]any) { delete(completion, "event_sha256") }, want: "event_sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), "home")
			if err := os.CopyFS(homePath, os.DirFS(baselineStore.home)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.home, "work", "receipted-action-completions", w.ID, in.Action.ID+".json")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var completion map[string]any
			if err := json.Unmarshal(body, &completion); err != nil {
				t.Fatal(err)
			}
			completion["schema_version"] = "compound.receipted_action_completion.v0.1"
			tc.mutate(completion)
			body, err = json.Marshal(completion)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			if replay, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err == nil || replay.Replayed || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("completion tamper replayed: result=%+v err=%v, want %q", replay, err, tc.want)
			}
		})
	}
	if replay, err := baselineStore.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err != nil || !replay.Replayed {
		t.Fatalf("completion tamper cases changed shared baseline: result=%+v err=%v", replay, err)
	}
}

func TestCompletedReceiptedActionRejectsSchemaValidExactOutputTamper(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	baselineStore, w, in, authorizedAt, result := completedGovernedAction(t)
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Store, ReceiptedActionResult)
	}{
		{name: "receipt header", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.Authority.Owner = "tampered" })
		}},
		{name: "receipt producer", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.Producer.Name = "tampered" })
		}},
		{name: "receipt skills missed", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.Skills.Missed = []string{"skill:tampered"} })
		}},
		{name: "receipt cost", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { tokens := 1; receipt.Cost.Tokens = &tokens })
		}},
		{name: "receipt memory", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.Memory.Candidates = []string{"candidate:tampered"} })
		}},
		{name: "receipt next", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.Next.SuggestedWork = []string{"work_tampered"} })
		}},
		{name: "receipt artifacts", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.StateDelta.Artifacts = []string{"artifact:tampered"} })
		}},
		{name: "receipt verification", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedReceipt(t, s, result, func(receipt *envelope.Receipt) { receipt.StateDelta.Verification = []string{"check:tampered"} })
		}},
		{name: "transition header", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.Authority.Owner = "tampered" })
		}},
		{name: "transition producer", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.Producer.Name = "tampered" })
		}},
		{name: "transition risk projection", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.Projections.Risk = []string{"risk:tampered"} })
		}},
		{name: "transition should harden", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.SecondOrder.ShouldHarden = true })
		}},
		{name: "transition should create rule", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.SecondOrder.ShouldCreateRule = true })
		}},
		{name: "transition should decay", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) { transition.SecondOrder.ShouldDecay = true })
		}},
		{name: "transition revisit at", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedTransition(t, s, result, func(transition *envelope.Transition) {
				transition.SecondOrder.RevisitAt = fixedNow.Add(24 * time.Hour).Format(time.RFC3339)
			})
		}},
		{name: "lifecycle equal state", mutate: func(t *testing.T, s *Store, result ReceiptedActionResult) {
			tamperGovernedLifecycleEvent(t, s, result.Action.ID, func(event *Event) { event.From, event.To = "verifying", "verifying" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each case gets an independent byte copy of one fully persisted
			// governed completion. The real tamper/replay paths still run for
			// every case without repeating unrelated setup transactions.
			homePath := filepath.Join(t.TempDir(), "home")
			if err := os.CopyFS(homePath, os.DirFS(baselineStore.home)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, s, result)
			if replay, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err == nil || replay.Replayed || !strings.Contains(err.Error(), "digest") {
				t.Fatalf("schema-valid tamper replayed: result=%+v err=%v", replay, err)
			}
		})
	}
	if replay, err := baselineStore.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err != nil || !replay.Replayed {
		t.Fatalf("tamper cases changed shared baseline: result=%+v err=%v", replay, err)
	}
}

func completedGovernedAction(t *testing.T) (*Store, envelope.WorkEnvelope, ReceiptedActionInput, time.Time, ReceiptedActionResult) {
	t.Helper()
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	authorizedAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return s, w, in, authorizedAt, result
}

func tamperGovernedReceipt(t *testing.T, s *Store, result ReceiptedActionResult, mutate func(*envelope.Receipt)) {
	t.Helper()
	var receipt envelope.Receipt
	if err := s.readJSON(s.envPath(result.Receipt.ID), &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(&receipt)
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt tamper is not schema-valid: %v", err)
	}
	if err := s.replaceEvent(receipt.ID, &receipt); err != nil {
		t.Fatal(err)
	}
}

func tamperGovernedTransition(t *testing.T, s *Store, result ReceiptedActionResult, mutate func(*envelope.Transition)) {
	t.Helper()
	var transition envelope.Transition
	if err := s.readJSON(s.envPath(result.Transition.ID), &transition); err != nil {
		t.Fatal(err)
	}
	mutate(&transition)
	if err := transition.Validate(); err != nil {
		t.Fatalf("transition tamper is not schema-valid: %v", err)
	}
	if err := s.replaceEvent(transition.ID, &transition); err != nil {
		t.Fatal(err)
	}
}

func tamperGovernedLifecycleEvent(t *testing.T, s *Store, actionID string, mutate func(*Event)) {
	t.Helper()
	body, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	matched := 0
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if !contains(event.Attached, actionID) {
			continue
		}
		mutate(&event)
		if event.From != event.To {
			t.Fatalf("lifecycle tamper is not an equal-state event: %+v", event)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(encoded)
		matched++
	}
	if matched != 1 {
		t.Fatalf("matched %d lifecycle events for %s", matched, actionID)
	}
	if err := os.WriteFile(s.eventsLog(), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAbortReceiptedActionReservationRefusesAfterReceiptPublicationBegins(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
		BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
	}
	authorizedAt := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, authorizedAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("receipt transaction interrupted after action publication")
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "action" {
			return injected
		}
		return nil
	}
	if _, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); !errors.Is(err, injected) {
		t.Fatalf("RecordReservedReceiptedAction error = %v", err)
	}
	receiptedActionAfterWrite = previous
	t.Cleanup(func() { receiptedActionAfterWrite = previous })
	if err := s.AbortReceiptedActionReservation(w.ID, action.ID); err == nil || !strings.Contains(err.Error(), "publication began") {
		t.Fatalf("AbortReceiptedActionReservation error = %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, fixedNow.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "pending receipted action "+action.ID) {
		t.Fatalf("partial receipt transaction no longer blocked lifecycle: %v", err)
	}
	if result, err := s.RecordReservedReceiptedAction(w.ID, in, authorizedAt); err != nil || result.Receipt.ID == "" {
		t.Fatalf("recover partial reserved transaction: result=%+v err=%v", result, err)
	}
}

func TestRecordReceiptedActionRefusesInvalidPreEvaluatedSecurityDecisionWithoutWriting(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T, s *Store) envelope.WorkEnvelope
		mutate func(*envelope.ActionIntent, *envelope.PolicyDecision)
		want   string
	}{
		{
			name: "ready work",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope {
				w, err := s.Create(CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "permission outcome", State: "ready"}, fixedNow)
				if err != nil {
					t.Fatal(err)
				}
				return w
			},
			want: "running",
		},
		{
			name: "wrong lease holder",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope {
				return governedActionRunningWork(t, s, "operator:lease-holder")
			},
			want: "lease holder",
		},
		{
			name:  "non operator actor",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope { return governedActionRunningWork(t, s, "codex") },
			mutate: func(action *envelope.ActionIntent, _ *envelope.PolicyDecision) {
				action.Actor = "codex"
			},
			want: "operator provenance",
		},
		{
			name: "mismatched decision",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope {
				return governedActionRunningWork(t, s, "operator:test")
			},
			mutate: func(_ *envelope.ActionIntent, decision *envelope.PolicyDecision) {
				decision.ActionIntentID = "act_other"
			},
			want: "bind",
		},
		{
			name: "held decision",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope {
				return governedActionRunningWork(t, s, "operator:test")
			},
			mutate: func(_ *envelope.ActionIntent, decision *envelope.PolicyDecision) {
				decision.Outcome = "hold"
			},
			want: "allowed",
		},
		{
			name: "expired decision",
			setup: func(t *testing.T, s *Store) envelope.WorkEnvelope {
				return governedActionRunningWork(t, s, "operator:test")
			},
			mutate: func(_ *envelope.ActionIntent, decision *envelope.PolicyDecision) {
				decision.ExpiresAt = fixedNow.Add(time.Minute).Format(time.RFC3339)
			},
			want: "expired",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := tc.setup(t, s)
			action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
			if tc.mutate != nil {
				tc.mutate(&action, &decision)
			}
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
				Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
				Outcome: "completed", Changed: true, Summary: "permission activation completed", MoveType: "control_plane_repair",
				BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
			}, fixedNow.Add(2*time.Minute))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("RecordReceiptedAction error = %v, want containing %q", err, tc.want)
			}
			after, readErr := os.ReadFile(s.workPath(w.ID))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatal("refused pre-evaluated action changed work")
			}
			for _, id := range []string{action.ID, decision.ID} {
				if exists, statErr := s.eventExists(id); statErr != nil || exists {
					t.Fatalf("refused pre-evaluated action persisted %s: %v", id, statErr)
				}
			}
		})
	}
}

// TestRecordReceiptedActionRefusesFabricatedDangerousMutationClassDecision is
// the E5 falsifier: validatePreEvaluatedReceiptedActionStatic historically
// re-validated operator provenance and standing-grant authority ONLY when
// MutationClass=="security". A caller could fabricate an "allow" decision for
// any other dangerous mutation class (destructive/financial/public) — no
// operator actor, no standing grant, no policy.Evaluate — and it passed. This
// proves the gate now closes for every dangerous class, not just security.
func TestRecordReceiptedActionRefusesFabricatedDangerousMutationClassDecision(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, mutationClass := range []string{"destructive", "financial", "public"} {
		t.Run(mutationClass, func(t *testing.T) {
			s := newStore(t)
			// The lease holder is an ordinary executor identity, not an
			// operator/human actor: policy.HasOperatorProvenance("codex-lane")
			// is false, and the decision carries no standing_grant_id.
			w := governedActionRunningWork(t, s, "codex-lane")
			action, decision := fabricatedDangerousActionApproval(w, "codex-lane", mutationClass, fixedNow.Add(time.Minute))
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
				Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
				Outcome: "completed", Changed: true, Summary: "fabricated dangerous mutation must not pass", MoveType: "control_plane_repair",
				BeforeRefs: []string{"sha256:preimage"}, AfterRefs: []string{"sha256:postimage"},
			}, fixedNow.Add(2*time.Minute))
			if err == nil {
				t.Fatalf("%s: fabricated allow decision with no operator provenance or standing grant was accepted", mutationClass)
			}
			lower := strings.ToLower(err.Error())
			if !strings.Contains(lower, "provenance") && !strings.Contains(lower, "standing-grant") {
				t.Fatalf("%s: want rejection citing provenance or standing-grant authority, got: %v", mutationClass, err)
			}
			after, readErr := os.ReadFile(s.workPath(w.ID))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatal("refused fabricated dangerous-class action changed work")
			}
			for _, id := range []string{action.ID, decision.ID} {
				if exists, statErr := s.eventExists(id); statErr != nil || exists {
					t.Fatalf("refused fabricated dangerous-class action persisted %s: %v", id, statErr)
				}
			}
		})
	}
}

func fabricatedDangerousActionApproval(w envelope.WorkEnvelope, actor, mutationClass string, now time.Time) (envelope.ActionIntent, envelope.PolicyDecision) {
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_fabricated_" + mutationClass, CreatedAt: now.UTC().Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{"sha256:fabricated-render"}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "harness.permissions.apply", Action: "reconcile_harness_permissions",
		Target:        envelope.ActionTarget{Kind: "harness_settings", ID: "claude_code", Path: "/synthetic/settings.json"},
		MutationClass: mutationClass, Reversibility: "manual", BlastRadius: "public", RollbackPlan: "restore exact pre-image",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_fabricated_" + mutationClass, CreatedAt: now.UTC().Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{"fabricated:no-real-authority"}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID, Outcome: "allow", Reasons: []string{"fabricated by caller, no real policy.Evaluate"},
	}
	return action, decision
}

func governedActionRunningWork(t *testing.T, s *Store, leaseHolder string) envelope.WorkEnvelope {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "permission outcome", State: "ready"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, leaseHolder, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: leaseHolder}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func governedActionApproval(w envelope.WorkEnvelope, actor string, now time.Time) (envelope.ActionIntent, envelope.PolicyDecision) {
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_permission_governed", CreatedAt: now.UTC().Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{"sha256:approved-render"}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "harness.permissions.apply", Action: "reconcile_harness_permissions",
		Target:        envelope.ActionTarget{Kind: "harness_settings", ID: "claude_code", Path: "/synthetic/settings.json"},
		MutationClass: "security", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore exact pre-image",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_permission_governed", CreatedAt: now.UTC().Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{"grant:permission"}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID, Outcome: "allow_with_obligations", Reasons: []string{"covered by grant"},
		Obligations: []string{"emit receipt"}, StandingGrantID: "grant_permission",
	}
	return action, decision
}
