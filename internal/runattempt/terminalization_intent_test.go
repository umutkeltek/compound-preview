package runattempt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func terminalizationIntentForTest(at time.Time) TerminalizationIntent {
	return TerminalizationIntent{
		SchemaVersion:              TerminalizationIntentSchemaVersion,
		Owner:                      TerminalizationOwnerPanelApply,
		WorkID:                     "work_terminalization_intent",
		RunAttemptActionIntentID:   "act_run_generation",
		RunAttemptPolicyDecisionID: "pol_run_generation",
		AuthorityActionIntentID:    "act_run_generation",
		AuthorityPolicyDecisionID:  "pol_run_generation",
		ExpectedWorkState:          "verifying",
		ExpectedWorkSHA256:         "sha256:" + strings.Repeat("a", 64),
		ExpectedActionIntentID:     "act_run_generation",
		ExpectedPolicyDecisionID:   "pol_run_generation",
		CloseInputSHA256:           "sha256:" + strings.Repeat("b", 64),
		CloseActionIntentID:        "act_close_generation",
		ClosePolicyDecisionID:      "pol_close_generation",
		Outcome:                    "completed",
		TargetStatus:               StatusFailed,
		ReceiptID:                  "rec_terminalization_intent",
		TransitionID:               "tr_terminalization_intent",
		PreparedAt:                 at.UTC(),
	}
}

func mintRunAttemptForTerminalizationIntent(t *testing.T, store *Store, at time.Time) RunAttempt {
	t.Helper()
	ra, err := store.MintWithActionIntent("work_terminalization_intent", "panel.apply", HarnessPlaceholder, "act_run_generation", at)
	if err != nil {
		t.Fatalf("mint RunAttempt: %v", err)
	}
	ra, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_run_generation"})
	if err != nil {
		t.Fatalf("bind execution policy: %v", err)
	}
	return ra
}

func TestPreparedTerminalizationIsImmutableAndCompletesOnlyExactIntent(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	ra := mintRunAttemptForTerminalizationIntent(t, store, now)
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	prepared, err := store.PrepareTerminalization(ra.ID, intent)
	if err != nil {
		t.Fatalf("prepare terminalization: %v", err)
	}
	if prepared.TerminalizationIntent == nil || !reflect.DeepEqual(*prepared.TerminalizationIntent, intent) {
		t.Fatalf("prepared intent mismatch: got=%+v want=%+v", prepared.TerminalizationIntent, intent)
	}
	if _, err := Open(store.Home()).PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatalf("exact idempotent prepare: %v", err)
	}
	stale := intent
	stale.ClosePolicyDecisionID = "pol_other_close_generation"
	if _, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, stale, now.Add(2*time.Minute)); err == nil {
		t.Fatal("completion accepted a different authority generation")
	}
	stillPending, err := Open(store.Home()).Get(ra.ID)
	if err != nil {
		t.Fatalf("get after refused completion: %v", err)
	}
	if IsTerminal(stillPending.Status) {
		t.Fatalf("mismatched completion terminalized the RunAttempt: %+v", stillPending)
	}
	completed, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("complete exact prepared terminalization: %v", err)
	}
	if completed.Status != StatusFailed || completed.TerminalizationOwner != TerminalizationOwnerPanelApply || completed.TerminalizationReceiptID != intent.ReceiptID || completed.TerminalizationTransitionID != intent.TransitionID {
		t.Fatalf("exact completion did not preserve requested status/proof: %+v", completed)
	}
	idempotent, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("idempotent exact completion: %v", err)
	}
	if !idempotent.CompletedAt.Equal(completed.CompletedAt) {
		t.Fatalf("idempotent completion moved completed_at: %s -> %s", completed.CompletedAt, idempotent.CompletedAt)
	}
}

func TestSweepPreservesPreparedTerminalizationForExactCompletion(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	ra := mintRunAttemptForTerminalizationIntent(t, store, now)
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	if _, err := store.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatalf("prepare terminalization: %v", err)
	}
	report, err := Open(store.Home()).SweepStale(30*time.Minute, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Swept != 0 || report.Skipped != 1 {
		t.Fatalf("prepared terminalization must be held for reconciliation, not swept: %+v", report)
	}
	swept, err := Open(store.Home()).Get(ra.ID)
	if err != nil {
		t.Fatalf("get swept RunAttempt: %v", err)
	}
	if swept.TerminalizationIntent == nil || !reflect.DeepEqual(*swept.TerminalizationIntent, intent) {
		t.Fatalf("sweep destroyed or changed durable terminalization intent: %+v", swept.TerminalizationIntent)
	}
	if IsTerminal(swept.Status) || swept.SweepReason != "" || swept.SweepClassification != "" {
		t.Fatalf("sweep contradicted prepared terminalization: %+v", swept)
	}
	corrected, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("exact correction after sweep: %v", err)
	}
	if corrected.Status != StatusFailed {
		t.Fatalf("exact completion after a held sweep is wrong: %+v", corrected)
	}
}

func TestPreparedTerminalizationFreezesExactEmptyBindingsAndCacheCopy(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 11, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_terminalization_intent", "codex", HarnessPlaceholder, now)
	if err != nil {
		t.Fatalf("mint unbound RunAttempt: %v", err)
	}
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	intent.RunAttemptActionIntentID = ""
	intent.RunAttemptPolicyDecisionID = ""
	if _, err := store.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatalf("prepare empty bindings: %v", err)
	}
	if err := store.AttachActionIntent(ra.ID, "act_late_binding"); err == nil {
		t.Fatal("prepared terminalization allowed a late action binding")
	}
	if _, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_late_binding"}); err == nil {
		t.Fatal("prepared terminalization allowed a late policy binding")
	}
	first, err := store.Get(ra.ID)
	if err != nil {
		t.Fatalf("get prepared RunAttempt: %v", err)
	}
	first.TerminalizationIntent.CloseActionIntentID = "act_mutated_cache_copy"
	second, err := store.Get(ra.ID)
	if err != nil {
		t.Fatalf("get prepared RunAttempt again: %v", err)
	}
	if second.TerminalizationIntent == nil || !reflect.DeepEqual(*second.TerminalizationIntent, intent) {
		t.Fatalf("caller mutated cached terminalization intent through an alias: %+v", second.TerminalizationIntent)
	}
}

func TestPrepareTerminalizationRejectsInventedRawBindings(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_terminalization_intent", "codex", HarnessPlaceholder, now)
	if err != nil {
		t.Fatalf("mint unbound RunAttempt: %v", err)
	}
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	if _, err := store.PrepareTerminalization(ra.ID, intent); err == nil {
		t.Fatal("prepare accepted action/policy bindings absent from the RunAttempt")
	}
}

func TestPreparedTerminalizationFencesGenericTerminalWriters(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	ra := mintRunAttemptForTerminalizationIntent(t, store, now)
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	if _, err := store.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatalf("prepare terminalization: %v", err)
	}
	writers := []struct {
		name string
		call func() error
	}{
		{"complete", func() error {
			_, err := Open(store.Home()).Complete(ra.ID, StatusSucceeded, now.Add(2*time.Minute))
			return err
		}},
		{"reason", func() error {
			_, err := Open(store.Home()).CompleteWithReason(ra.ID, StatusFailed, "generic failure", now.Add(2*time.Minute))
			return err
		}},
		{"classified", func() error {
			_, err := Open(store.Home()).CompleteWithReasonAndClassification(ra.ID, StatusAbandoned, "stale", SweepClassificationAmbiguous, now.Add(2*time.Minute))
			return err
		}},
		{"owner_proof", func() error {
			_, err := Open(store.Home()).CompleteWithOwnerProof(ra.ID, intent.TargetStatus, intent.Owner, intent.ReceiptID, intent.TransitionID, now.Add(2*time.Minute))
			return err
		}},
		{"work_close", func() error {
			_, err := Open(store.Home()).CompleteForWorkClose(ra.ID, intent.TargetStatus, intent.ReceiptID, intent.TransitionID, now.Add(2*time.Minute))
			return err
		}},
	}
	for _, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			if err := writer.call(); err == nil || !strings.Contains(err.Error(), "prepared terminalization") {
				t.Fatalf("generic terminal writer error = %v, want prepared-terminalization fence", err)
			}
		})
	}
	completed, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("exact completion after fenced writers: %v", err)
	}
	if completed.Status != intent.TargetStatus {
		t.Fatalf("exact completion status = %q, want %q", completed.Status, intent.TargetStatus)
	}
}

func TestPreparedTerminalizationWinsAgainstCompetingGenericCompletion(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 14, 30, 0, 0, time.UTC)
	ra := mintRunAttemptForTerminalizationIntent(t, store, now)
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	if _, err := store.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatalf("prepare terminalization: %v", err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var genericErr, exactErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, genericErr = Open(store.Home()).Complete(ra.ID, StatusSucceeded, now.Add(2*time.Minute))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, exactErr = Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(2*time.Minute))
	}()
	close(start)
	wg.Wait()
	if genericErr == nil || exactErr != nil {
		t.Fatalf("competing completions: generic_err=%v exact_err=%v", genericErr, exactErr)
	}
	after, err := Open(store.Home()).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != intent.TargetStatus || after.TerminalizationOwner != intent.Owner {
		t.Fatalf("exact prepared result did not win: %+v", after)
	}
}

func TestCompletePreparedTerminalizationRefusesSweptAbandonedRecord(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC)
	ra := mintRunAttemptForTerminalizationIntent(t, store, now)
	intent := terminalizationIntentForTest(now.Add(time.Minute))
	intent.TerminalizationReason = "panel apply failed"
	prepared, err := store.PrepareTerminalization(ra.ID, intent)
	if err != nil {
		t.Fatalf("prepare terminalization: %v", err)
	}
	sweptAt := now.Add(2 * time.Minute)
	prepared.Status = StatusAbandoned
	prepared.CompletedAt = sweptAt
	prepared.SweepReason = "stale: ambiguous"
	prepared.SweepClassification = SweepClassificationAmbiguous
	body, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Home(), "run_attempts", ra.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Home(), "run_attempts", ra.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store.Home()).CompletePreparedTerminalization(ra.ID, intent, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "conflicting terminal status abandoned") {
		t.Fatalf("swept-abandoned prepared completion error = %v, want immutable terminal refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("terminal refusal rewrote the swept-abandoned record")
	}
}

func TestDecodedRunAttemptValidationRejectsInvalidNestedTerminalizationIntent(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	valid := terminalizationIntentForTest(now)
	cases := []struct {
		name         string
		parentSchema string
		mutate       func(*TerminalizationIntent)
	}{
		{name: "unknown nested schema", parentSchema: SchemaVersion, mutate: func(in *TerminalizationIntent) { in.SchemaVersion = "compound.runattempt_terminalization_intent.v9.9" }},
		{name: "invalid owner", parentSchema: SchemaVersion, mutate: func(in *TerminalizationIntent) { in.Owner = "future.owner" }},
		{name: "invalid target status", parentSchema: SchemaVersion, mutate: func(in *TerminalizationIntent) { in.TargetStatus = "running" }},
		{name: "invalid work digest", parentSchema: SchemaVersion, mutate: func(in *TerminalizationIntent) { in.ExpectedWorkSHA256 = "sha256:not-a-digest" }},
		{name: "intent on legacy parent", parentSchema: LegacySchemaVersionV01, mutate: func(*TerminalizationIntent) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "run_attempts"), 0o755); err != nil {
				t.Fatal(err)
			}
			id := MintDeterministic("invalid-nested-"+tc.name, now)
			intent := valid
			tc.mutate(&intent)
			record := RunAttempt{
				SchemaVersion:         tc.parentSchema,
				ID:                    id,
				WorkID:                intent.WorkID,
				ActionIntentID:        intent.RunAttemptActionIntentID,
				Actor:                 "panel.apply",
				Harness:               HarnessPlaceholder,
				Status:                StatusPending,
				CreatedAt:             now,
				ExecutionPosture:      &ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: intent.RunAttemptPolicyDecisionID},
				TerminalizationIntent: &intent,
			}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "run_attempts", id+".json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(home).Get(id); err == nil {
				t.Fatal("Get accepted invalid nested terminalization intent")
			}
			if _, err := Open(home).getFresh(id); err == nil {
				t.Fatal("getFresh accepted invalid nested terminalization intent")
			}
		})
	}
}
