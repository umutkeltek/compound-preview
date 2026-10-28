package work

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestRecordReceiptedActionKeepsParentOpenAndIsStateIdempotent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-ruling", "", fixedNow)
	action := receiptedActionIntent(w, "act_receipted_stop", fixedNow.Add(time.Minute))

	result, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "continued the exact Codex turn with a current standing-law answer",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID, "session:sess_receipted", "turn:turn_receipted"},
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("record receipted action: %v", err)
	}
	if result.Replayed {
		t.Fatal("first receipted action reported replay")
	}
	wantReceiptID := idFor("rec_", "receipted-action", w.ID, action.ID, claimID)
	wantTransitionID := idFor("tr_", "receipted-action", w.ID, action.ID, claimID)
	if result.Receipt.ID != wantReceiptID || result.Transition.ID != wantTransitionID {
		t.Fatalf("legacy receipted-action identities changed: receipt=%s want=%s transition=%s want=%s", result.Receipt.ID, wantReceiptID, result.Transition.ID, wantTransitionID)
	}
	if result.Work.State != "running" || result.Work.IDs.TransitionID != "" {
		t.Fatalf("nonterminal action claimed task closure: %+v", result.Work)
	}
	if result.Action.IDs.WorkID != w.ID || result.Decision.IDs.WorkID != w.ID ||
		result.Receipt.IDs.WorkID != w.ID || result.Transition.IDs.WorkID != w.ID {
		t.Fatalf("receipted action broke one-work-id chain: %+v", result)
	}
	if result.Decision.ActionIntentID != result.Action.ID ||
		!contains(result.Transition.ActionRefs, result.Action.ID) ||
		!contains(result.Transition.AfterRefs, result.Receipt.ID) {
		t.Fatalf("receipted action links incomplete: %+v", result)
	}

	_, events, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	for _, id := range []string{result.Action.ID, result.Decision.ID, result.Receipt.ID, result.Transition.ID} {
		if !contains(last.Attached, id) {
			t.Fatalf("same-state event missing %s: %+v", id, last)
		}
	}
	doctor, err := s.ProofChainDoctor(fixedNow.Add(2*time.Minute), ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range doctor.Findings {
		if strings.Contains(finding.Message, result.Receipt.ID) || strings.Contains(finding.Message, result.Transition.ID) {
			t.Fatalf("receipted nonterminal action is orphaned: %+v", doctor)
		}
	}
	workbench, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if workbench.Counts.Open != 1 || workbench.Counts.Terminal != 0 || workbench.Counts.InProgress != 1 || len(workbench.Open) != 1 || len(workbench.Recent) != 0 {
		t.Fatalf("nonterminal action changed parent completion counts: %+v", workbench.Counts)
	}
	open := workbench.Open[0]
	if open.State != "running" || open.ReceiptID != "" || open.TransitionID != "" || open.MoveType != "" || open.Changed {
		t.Fatalf("intermediate action proof leaked into parent completion projection: %+v", open)
	}

	ids := []string{
		result.Action.ID,
		result.Decision.ID,
		result.Receipt.ID,
		result.Transition.ID,
	}
	before := map[string]string{}
	for _, id := range ids {
		body, err := s.eventBytes(id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = string(body)
	}
	replay, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "continued the exact Codex turn with a current standing-law answer",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID, "session:sess_receipted", "turn:turn_receipted"},
	}, fixedNow.Add(10*time.Minute))
	if err != nil || !replay.Replayed || replay.Receipt.ID != result.Receipt.ID || replay.Transition.ID != result.Transition.ID {
		t.Fatalf("idempotent replay failed: err=%v result=%+v", err, replay)
	}
	for _, id := range ids {
		body, err := s.eventBytes(id)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != before[id] {
			t.Fatalf("replay rewrote %s", id)
		}
	}
}

func TestRecordReceiptedActionReplaysHistoricalTransactionWithoutRewindingWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-historical-replay", "", fixedNow)
	aInput := receiptedActionTestInput(w, claimID, "act_receipted_historical_a", "recorded historical action A", fixedNow.Add(time.Minute))
	bInput := receiptedActionTestInput(w, claimID, "act_receipted_historical_b", "recorded newer action B", fixedNow.Add(2*time.Minute))

	a, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	beforeWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeWork.ActionIntentIDs) < 2 || beforeWork.ActionIntentIDs[len(beforeWork.ActionIntentIDs)-2] != a.Action.ID || beforeWork.ActionIntentIDs[len(beforeWork.ActionIntentIDs)-1] != b.Action.ID {
		t.Fatalf("receipted action order = %v, want suffix [%s %s]", beforeWork.ActionIntentIDs, a.Action.ID, b.Action.ID)
	}
	beforeWorkBytes, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeEventBytes, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	replayInput := aInput
	replayInput.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
	replay, err := s.RecordReceiptedAction(w.ID, replayInput, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("historical replay: %v", err)
	}
	if !replay.Replayed || replay.Action.ID != a.Action.ID || replay.Decision.ID != a.Decision.ID || replay.Receipt.ID != a.Receipt.ID || replay.Transition.ID != a.Transition.ID {
		t.Fatalf("historical replay did not return transaction A: %+v", replay)
	}
	if replay.Work.PolicyDecisionID != b.Decision.ID || !slices.Equal(replay.Work.ActionIntentIDs, beforeWork.ActionIntentIDs) {
		t.Fatalf("historical replay returned rewound work: %+v", replay.Work)
	}
	afterWorkBytes, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterWorkBytes) != string(beforeWorkBytes) {
		t.Fatal("historical replay rewrote the WorkEnvelope")
	}
	afterWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterWork.PolicyDecisionID != b.Decision.ID || !slices.Equal(afterWork.ActionIntentIDs, beforeWork.ActionIntentIDs) {
		t.Fatalf("historical replay rewound persisted work: %+v", afterWork)
	}
	afterEventBytes, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) || string(afterEventBytes) != string(beforeEventBytes) {
		t.Fatalf("historical replay republished an event: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
}

func TestRecordReceiptedActionBlocksNewActionUntilIncompletePrefixRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name            string
		failureBoundary string
	}{
		{name: "after transition before work", failureBoundary: "work"},
		{name: "after work before event", failureBoundary: "event"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := receiptedActionRunningWork(t, s)
			claimID := receiptedActionRuling(t, s, "receipted-action-pending-"+strings.ReplaceAll(tc.name, " ", "-"), "", fixedNow)
			aInput := receiptedActionTestInput(w, claimID, "act_receipted_pending_a_"+tc.failureBoundary, "recorded pending action A", fixedNow.Add(time.Minute))
			bInput := receiptedActionTestInput(w, claimID, "act_receipted_pending_b_"+tc.failureBoundary, "recorded blocked action B", fixedNow.Add(2*time.Minute))
			baselineEvents, err := s.admissionEventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}

			injected := errors.New("injected before " + tc.failureBoundary)
			previousBeforeHook := receiptedActionBeforePublish
			failed := false
			receiptedActionBeforePublish = func(boundary string) error {
				if boundary == tc.failureBoundary && !failed {
					failed = true
					return injected
				}
				return nil
			}
			defer func() { receiptedActionBeforePublish = previousBeforeHook }()
			if result, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute)); !errors.Is(err, injected) || result.Replayed {
				t.Fatalf("action A did not stop at %s: err=%v result=%+v", tc.failureBoundary, err, result)
			}
			receiptedActionBeforePublish = previousBeforeHook
			if _, err := os.Stat(receiptedActionPendingTestPath(s, w.ID)); err != nil {
				t.Fatalf("incomplete action A did not retain its pending marker: %v", err)
			}

			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents, err := os.ReadFile(s.eventsLog())
			if err != nil {
				t.Fatal(err)
			}
			beforePending, err := os.ReadFile(receiptedActionPendingTestPath(s, w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if result, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(2*time.Minute)); err == nil || result.Replayed || !strings.Contains(err.Error(), "pending receipted action "+aInput.Action.ID) {
				t.Fatalf("action B crossed pending A: err=%v result=%+v", err, result)
			}
			afterBlockedWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			afterBlockedEvents, err := os.ReadFile(s.eventsLog())
			if err != nil {
				t.Fatal(err)
			}
			if string(afterBlockedWork) != string(beforeWork) || string(afterBlockedEvents) != string(beforeEvents) {
				t.Fatal("blocked action B changed work or event chronology")
			}
			afterBlockedPending, err := os.ReadFile(receiptedActionPendingTestPath(s, w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterBlockedPending) != string(beforePending) {
				t.Fatal("blocked action B changed pending ownership")
			}
			for _, id := range receiptedActionArtifactIDs(w.ID, bInput.Action.ID, claimID) {
				if exists, err := s.eventExists(id); err != nil || exists {
					t.Fatalf("blocked action B left artifact %s: %v", id, err)
				}
			}

			retryA := aInput
			retryA.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
			recoveredA, err := s.RecordReceiptedAction(w.ID, retryA, fixedNow.Add(3*time.Minute))
			if err != nil || !recoveredA.Replayed {
				t.Fatalf("action A did not recover before B: err=%v result=%+v", err, recoveredA)
			}
			assertReceiptedActionConverged(t, s, w.ID, recoveredA)

			recordedB, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(4*time.Minute))
			if err != nil || recordedB.Replayed {
				t.Fatalf("action B did not proceed after A converged: err=%v result=%+v", err, recordedB)
			}
			afterB, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterB.ActionIntentIDs) < 2 || afterB.ActionIntentIDs[len(afterB.ActionIntentIDs)-2] != recoveredA.Action.ID || afterB.ActionIntentIDs[len(afterB.ActionIntentIDs)-1] != recordedB.Action.ID || afterB.PolicyDecisionID != recordedB.Decision.ID {
				t.Fatalf("recovered action order = %v policy=%s, want suffix [%s %s] and policy %s", afterB.ActionIntentIDs, afterB.PolicyDecisionID, recoveredA.Action.ID, recordedB.Action.ID, recordedB.Decision.ID)
			}
			events, err := s.admissionEventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			aIndex, bIndex := -1, -1
			for i, event := range events {
				if contains(event.Attached, recoveredA.Action.ID) {
					aIndex = i
				}
				if contains(event.Attached, recordedB.Action.ID) {
					bIndex = i
				}
			}
			if aIndex < 0 || bIndex != aIndex+1 {
				t.Fatalf("receipted event order A=%d B=%d: %+v", aIndex, bIndex, events)
			}
			if len(events) != len(baselineEvents)+2 {
				t.Fatalf("receipted event count = %d, want baseline %d + A/B", len(events), len(baselineEvents))
			}
		})
	}
}

func TestPendingReceiptedActionExcludesCanonicalLifecycleWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name string
		run  func(*Store, envelope.WorkEnvelope, envelope.ActionIntent, envelope.PolicyDecision, time.Time) error
	}{
		{name: "record action", run: func(s *Store, w envelope.WorkEnvelope, ai envelope.ActionIntent, pd envelope.PolicyDecision, at time.Time) error {
			_, err := s.RecordAction(w.ID, ai, pd, at)
			return err
		}},
		{name: "advance", run: func(s *Store, w envelope.WorkEnvelope, _ envelope.ActionIntent, _ envelope.PolicyDecision, at time.Time) error {
			_, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, at)
			return err
		}},
		{name: "close", run: func(s *Store, w envelope.WorkEnvelope, _ envelope.ActionIntent, _ envelope.PolicyDecision, at time.Time) error {
			_, err := s.Close(w.ID, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", Summary: "blocked close"}, at)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := receiptedActionRunningWork(t, s)
			claimID := receiptedActionRuling(t, s, "receipted-action-lifecycle-exclusion-"+strings.ReplaceAll(tc.name, " ", "-"), "", fixedNow)
			aInput := receiptedActionTestInput(w, claimID, "act_receipted_lifecycle_pending_"+strings.ReplaceAll(tc.name, " ", "_"), "recover before any other lifecycle write", fixedNow.Add(time.Minute))

			injected := errors.New("injected before work publication")
			previousBeforeHook := receiptedActionBeforePublish
			failed := false
			receiptedActionBeforePublish = func(boundary string) error {
				if boundary == "work" && !failed {
					failed = true
					return injected
				}
				return nil
			}
			defer func() { receiptedActionBeforePublish = previousBeforeHook }()
			if result, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute)); !errors.Is(err, injected) || result.Replayed {
				t.Fatalf("action A did not stop before work publication: err=%v result=%+v", err, result)
			}
			receiptedActionBeforePublish = previousBeforeHook

			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents, err := os.ReadFile(s.eventsLog())
			if err != nil {
				t.Fatal(err)
			}
			beforePending, err := os.ReadFile(receiptedActionPendingTestPath(s, w.ID))
			if err != nil {
				t.Fatal(err)
			}
			beforeEventNames := receiptedActionEventEntryNames(t, s)
			genericAction := receiptedActionIntent(w, "act_record_action_blocked_by_pending", fixedNow.Add(2*time.Minute))
			genericDecision := envelope.PolicyDecision{
				Header:         newHeader(envelope.SVPolicyDecision, "pol_record_action_blocked_by_pending", fixedNow.Add(2*time.Minute), w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
				ActionIntentID: genericAction.ID,
				Outcome:        "allow",
				Obligations:    []string{},
			}

			err = tc.run(s, w, genericAction, genericDecision, fixedNow.Add(2*time.Minute))
			if err == nil || !strings.Contains(err.Error(), "pending receipted action "+aInput.Action.ID) {
				t.Fatalf("%s crossed pending action A: %v", tc.name, err)
			}
			afterWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			afterEvents, err := os.ReadFile(s.eventsLog())
			if err != nil {
				t.Fatal(err)
			}
			afterPending, err := os.ReadFile(receiptedActionPendingTestPath(s, w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) || string(afterPending) != string(beforePending) {
				t.Fatalf("blocked %s changed work, event chronology, or pending ownership", tc.name)
			}
			if afterEventNames := receiptedActionEventEntryNames(t, s); !slices.Equal(afterEventNames, beforeEventNames) {
				t.Fatalf("blocked %s left event artifacts: before=%v after=%v", tc.name, beforeEventNames, afterEventNames)
			}

			retryA := aInput
			retryA.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
			recoveredA, err := s.RecordReceiptedAction(w.ID, retryA, fixedNow.Add(3*time.Minute))
			if err != nil || !recoveredA.Replayed {
				t.Fatalf("action A did not recover: err=%v result=%+v", err, recoveredA)
			}
			assertReceiptedActionConverged(t, s, w.ID, recoveredA)
			recorded, err := s.RecordAction(w.ID, genericAction, genericDecision, fixedNow.Add(4*time.Minute))
			if err != nil || !contains(recorded.ActionIntentIDs, genericAction.ID) || recorded.PolicyDecisionID != genericDecision.ID {
				t.Fatalf("record action did not resume after action A converged: err=%v work=%+v", err, recorded)
			}
			advanced, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(5*time.Minute))
			if err != nil || advanced.State != "verifying" {
				t.Fatalf("advance did not resume after action A converged: err=%v work=%+v", err, advanced)
			}
			closed, err := s.Close(w.ID, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", Summary: "lifecycle resumed"}, fixedNow.Add(6*time.Minute))
			if err != nil || closed.Work.State != "failed" {
				t.Fatalf("close did not resume after action A converged: err=%v result=%+v", err, closed)
			}
		})
	}
}

func TestRecordReceiptedActionRejectsReadyWorkBeforeWriting(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "codex", SuccessPredicate: "receipted action requires active execution", Risk: "tier1", Privacy: "private"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimID := receiptedActionRuling(t, s, "receipted-action-ready-state", "", fixedNow)
	in := receiptedActionTestInput(w, claimID, "act_receipted_ready_rejected", "do not act before claim", fixedNow.Add(time.Minute))
	in.ExpectedWorkState = "ready"
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
	if err == nil || result.Replayed || !strings.Contains(err.Error(), "require state claimed|running|verifying") {
		t.Fatalf("ready work crossed receipted-action state gate: err=%v result=%+v", err, result)
	}
	afterWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) {
		t.Fatal("ready-state rejection changed work or event chronology")
	}
	if exists, err := s.eventExists(in.Action.ID); err != nil || exists {
		t.Fatalf("ready-state rejection left action artifact: %v", err)
	}
	if _, err := os.Stat(receiptedActionPendingTestPath(s, w.ID)); !os.IsNotExist(err) {
		t.Fatalf("ready-state rejection left pending marker: %v", err)
	}
}

func TestRecordReceiptedActionRejectsMalformedPendingRecord(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-malformed-pending", "", fixedNow)
	in := receiptedActionTestInput(w, claimID, "act_receipted_malformed_pending", "do not cross an invalid pending record", fixedNow.Add(time.Minute))
	pendingPath := receiptedActionPendingTestPath(s, w.ID)
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte("{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
	if err == nil || result.Replayed || !strings.Contains(err.Error(), "pending receipted action") {
		t.Fatalf("malformed pending record did not fail closed: err=%v result=%+v", err, result)
	}
	if exists, err := s.eventExists(in.Action.ID); err != nil || exists {
		t.Fatalf("malformed pending record allowed action artifact: %v", err)
	}
	afterWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) {
		t.Fatal("malformed pending record changed work or event state")
	}
}

func TestRecordReceiptedActionRejectsPendingRecordChangedBeforeCleanup(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-pending-cleanup-conflict", "", fixedNow)
	in := receiptedActionTestInput(w, claimID, "act_receipted_pending_cleanup_conflict", "do not clear replaced pending ownership", fixedNow.Add(time.Minute))
	pendingPath := receiptedActionPendingTestPath(s, w.ID)

	previousBeforeHook := receiptedActionBeforePublish
	changed := false
	receiptedActionBeforePublish = func(boundary string) error {
		if boundary != "pending_clear" || changed {
			return nil
		}
		var pending receiptedActionPending
		if err := s.readJSON(pendingPath, &pending); err != nil {
			return err
		}
		pending.TransactionDigest = strings.Repeat("0", 64)
		if err := s.writeJSON(pendingPath, &pending); err != nil {
			return err
		}
		changed = true
		return nil
	}
	defer func() { receiptedActionBeforePublish = previousBeforeHook }()

	result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
	if err == nil || result.Replayed || !strings.Contains(err.Error(), "changed before cleanup") {
		t.Fatalf("replaced pending record was cleared: err=%v result=%+v", err, result)
	}
	if !changed {
		t.Fatal("cleanup conflict hook did not replace the pending record")
	}
	var persisted receiptedActionPending
	if err := s.readJSON(pendingPath, &persisted); err != nil {
		t.Fatalf("replaced pending record was removed: %v", err)
	}
	if persisted.TransactionDigest != strings.Repeat("0", 64) {
		t.Fatalf("pending replacement was overwritten: %+v", persisted)
	}
	events, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, event := range events {
		if contains(event.Attached, in.Action.ID) {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("cleanup conflict lost or duplicated the durable action event: %+v", events)
	}
}

func TestRecordReceiptedActionAdoptsLegacyExactPrefixWithoutMarker(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-legacy-pending", "", fixedNow)
	aInput := receiptedActionTestInput(w, claimID, "act_receipted_legacy_pending_a", "recover the exact legacy prefix", fixedNow.Add(time.Minute))
	bInput := receiptedActionTestInput(w, claimID, "act_receipted_legacy_pending_b", "do not cross adopted legacy ownership", fixedNow.Add(2*time.Minute))

	previousBeforeHook := receiptedActionBeforePublish
	failed := false
	injected := errors.New("injected legacy prefix before work")
	receiptedActionBeforePublish = func(boundary string) error {
		if boundary == "work" && !failed {
			failed = true
			return injected
		}
		return nil
	}
	defer func() { receiptedActionBeforePublish = previousBeforeHook }()
	if _, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute)); !errors.Is(err, injected) {
		t.Fatalf("did not create legacy prefix: %v", err)
	}
	receiptedActionBeforePublish = previousBeforeHook
	if err := os.Remove(receiptedActionPendingTestPath(s, w.ID)); err != nil {
		t.Fatal(err)
	}

	failed = false
	receiptedActionBeforePublish = func(boundary string) error {
		if boundary == "work" && !failed {
			failed = true
			return injected
		}
		return nil
	}
	retryA := aInput
	retryA.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.RecordReceiptedAction(w.ID, retryA, fixedNow.Add(3*time.Minute)); !errors.Is(err, injected) {
		t.Fatalf("legacy exact retry did not adopt before work: %v", err)
	}
	receiptedActionBeforePublish = previousBeforeHook
	pending, present, _, err := s.preflightReceiptedActionPending(w.ID)
	if err != nil || !present {
		t.Fatalf("legacy exact retry did not publish pending ownership: err=%v pending=%+v", err, pending)
	}
	if pending.ActionID != aInput.Action.ID || pending.CreatedAt != aInput.Action.CreatedAt {
		t.Fatalf("legacy adoption did not freeze action A: %+v", pending)
	}
	if result, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(4*time.Minute)); err == nil || result.Replayed {
		t.Fatalf("action B crossed adopted legacy prefix: err=%v result=%+v", err, result)
	}
	recoveredA, err := s.RecordReceiptedAction(w.ID, retryA, fixedNow.Add(5*time.Minute))
	if err != nil || !recoveredA.Replayed {
		t.Fatalf("adopted legacy action A did not converge: err=%v result=%+v", err, recoveredA)
	}
	assertReceiptedActionConverged(t, s, w.ID, recoveredA)
}

func TestRecordReceiptedActionRetainsMarkerWhenCleanupFailsAfterDurableEvent(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-pending-cleanup-retry", "", fixedNow)
	aInput := receiptedActionTestInput(w, claimID, "act_receipted_pending_cleanup_a", "retain ownership until cleanup succeeds", fixedNow.Add(time.Minute))
	bInput := receiptedActionTestInput(w, claimID, "act_receipted_pending_cleanup_b", "wait for action A cleanup", fixedNow.Add(2*time.Minute))

	previousBeforeHook := receiptedActionBeforePublish
	injected := errors.New("injected pending cleanup failure")
	failed := false
	receiptedActionBeforePublish = func(boundary string) error {
		if boundary == "pending_clear" && !failed {
			failed = true
			return injected
		}
		return nil
	}
	defer func() { receiptedActionBeforePublish = previousBeforeHook }()
	if result, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute)); !errors.Is(err, injected) || result.Replayed {
		t.Fatalf("cleanup failure did not surface after action A: err=%v result=%+v", err, result)
	}
	receiptedActionBeforePublish = previousBeforeHook
	if _, err := os.Stat(receiptedActionPendingTestPath(s, w.ID)); err != nil {
		t.Fatalf("cleanup failure removed pending ownership: %v", err)
	}
	eventsBeforeRetry, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(2*time.Minute)); err == nil || result.Replayed {
		t.Fatalf("action B crossed cleanup-pending A: err=%v result=%+v", err, result)
	}
	retryA := aInput
	retryA.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
	recoveredA, err := s.RecordReceiptedAction(w.ID, retryA, fixedNow.Add(3*time.Minute))
	if err != nil || !recoveredA.Replayed {
		t.Fatalf("action A cleanup retry did not converge: err=%v result=%+v", err, recoveredA)
	}
	assertReceiptedActionConverged(t, s, w.ID, recoveredA)
	eventsAfterRetry, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfterRetry) != len(eventsBeforeRetry) {
		t.Fatalf("cleanup retry duplicated event: before=%d after=%d", len(eventsBeforeRetry), len(eventsAfterRetry))
	}
}

func TestRecordReceiptedActionRejectsHistoricalReplayWithCorruptNewerPolicyLink(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-corrupt-newer-link", "", fixedNow)
	aInput := receiptedActionTestInput(w, claimID, "act_receipted_corrupt_link_a", "recorded historical action A", fixedNow.Add(time.Minute))
	bInput := receiptedActionTestInput(w, claimID, "act_receipted_corrupt_link_b", "recorded newer action B", fixedNow.Add(2*time.Minute))

	a, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	corruptDecision := b.Decision
	corruptDecision.ActionIntentID = a.Action.ID
	if err := s.replaceEvent(b.Decision.ID, &corruptDecision); err != nil {
		t.Fatal(err)
	}
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}

	replay, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(3*time.Minute))
	if err == nil || replay.Replayed || !strings.Contains(err.Error(), "conflicts with persisted work links") {
		t.Fatalf("corrupt newer policy link authorized historical replay: err=%v result=%+v", err, replay)
	}
	afterWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) {
		t.Fatal("rejected historical replay changed work or event state")
	}
}

func TestRecordReceiptedActionRejectsHistoricalReplayWithoutOriginalEvent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-historical-event", "", fixedNow)
	aInput := receiptedActionTestInput(w, claimID, "act_receipted_missing_event_a", "recorded historical action A", fixedNow.Add(time.Minute))
	bInput := receiptedActionTestInput(w, claimID, "act_receipted_missing_event_b", "recorded newer action B", fixedNow.Add(2*time.Minute))

	eventsBeforeA, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventsAfterA, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReceiptedAction(w.ID, bInput, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventsAfterB, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfterB) < len(eventsAfterA) || !strings.HasPrefix(string(eventsAfterB), string(eventsAfterA)) {
		t.Fatal("receipted action events were not append-only")
	}
	eventsWithoutA := append(append([]byte{}, eventsBeforeA...), eventsAfterB[len(eventsAfterA):]...)
	if err := os.WriteFile(s.eventsLog(), eventsWithoutA, 0o644); err != nil {
		t.Fatal(err)
	}

	replay, err := s.RecordReceiptedAction(w.ID, aInput, fixedNow.Add(3*time.Minute))
	if err == nil || replay.Replayed || !strings.Contains(err.Error(), "historical transaction is missing its event") {
		t.Fatalf("historical replay repaired an event after newer chronology: err=%v result=%+v", err, replay)
	}
	afterReplay, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterReplay) != string(eventsWithoutA) {
		t.Fatal("rejected historical replay changed the event log")
	}
}

func TestRecordReceiptedActionTerminalProjectionUsesClosingTransitionReceipt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-terminal-projection", "", fixedNow)
	intermediate, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
		Action:                     receiptedActionIntent(w, "act_receipted_terminal_projection", fixedNow.Add(time.Minute)),
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "resume the lane without completing the parent work",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	eventsBeforeClose, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "parent work completed with its own terminal proof",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:parent-terminal-proof"},
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Close stopping after its terminal WorkEnvelope write but before
	// its final lifecycle event. The canonical transition still identifies the
	// closing receipt; the earlier ask-resume event must never substitute for it.
	if err := os.WriteFile(s.eventsLog(), eventsBeforeClose, 0o644); err != nil {
		t.Fatal(err)
	}

	workbench, err := s.Workbench(fixedNow.Add(3*time.Minute), WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(workbench.Recent) != 1 {
		t.Fatalf("terminal parent missing from workbench: %+v", workbench)
	}
	item := workbench.Recent[0]
	if item.ReceiptID != closed.Receipt.ID || item.TransitionID != closed.Transition.ID {
		t.Fatalf("terminal projection paired receipt=%q transition=%q, want closing %q/%q (intermediate receipt %q)", item.ReceiptID, item.TransitionID, closed.Receipt.ID, closed.Transition.ID, intermediate.Receipt.ID)
	}
	if item.ReceiptID == intermediate.Receipt.ID {
		t.Fatalf("intermediate ask-resume receipt projected as parent completion: %+v", item)
	}
}

func TestRecordReceiptedActionRecoversEveryWriteBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	boundaries := []string{"pending", "action", "decision", "receipt", "transition", "work", "event", "pending_clear"}
	phases := []string{"before_publish", "after_publish"}
	for _, phase := range phases {
		for _, boundary := range boundaries {
			t.Run(phase+"/"+boundary, func(t *testing.T) {
				s := newStore(t)
				w := receiptedActionRunningWork(t, s)
				claimID := receiptedActionRuling(t, s, "receipted-action-recovery-"+phase+"-"+boundary, "", fixedNow)
				action := receiptedActionIntent(w, "act_receipted_recovery_"+phase+"_"+boundary, fixedNow.Add(time.Minute))
				in := ReceiptedActionInput{
					Action:                     action,
					OperationalDecisionClaimID: claimID,
					ExpectedWorkState:          "running",
					Summary:                    "continued the exact Codex turn after a recoverable write failure",
					MoveType:                   "control_plane_repair",
					ProofRefs:                  []string{"claim:" + claimID, "session:sess_receipted", "turn:turn_receipted"},
				}

				injected := errors.New("injected receipted-action write failure " + phase + " " + boundary)
				failed := false
				previousBeforeHook := receiptedActionBeforePublish
				previousHook := receiptedActionAfterWrite
				receiptedActionBeforePublish = func(got string) error {
					if phase == "before_publish" && got == boundary && !failed {
						failed = true
						return injected
					}
					return nil
				}
				receiptedActionAfterWrite = func(got string) error {
					if phase == "after_publish" && got == boundary && !failed {
						failed = true
						return injected
					}
					return nil
				}
				defer func() {
					receiptedActionBeforePublish = previousBeforeHook
					receiptedActionAfterWrite = previousHook
				}()

				if first, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute)); !errors.Is(err, injected) || first.Receipt.ID != "" {
					t.Fatalf("boundary %s did not expose the injected failure: err=%v result=%+v", boundary, err, first)
				}
				_, pendingErr := os.Stat(receiptedActionPendingTestPath(s, w.ID))
				wantPending := !(boundary == "pending" && phase == "before_publish") &&
					!(boundary == "pending_clear" && phase == "after_publish")
				if wantPending && pendingErr != nil {
					t.Fatalf("boundary %s/%s lost pending ownership: %v", phase, boundary, pendingErr)
				}
				if !wantPending && !os.IsNotExist(pendingErr) {
					t.Fatalf("boundary %s/%s published pending ownership too early: %v", phase, boundary, pendingErr)
				}
				receiptedActionBeforePublish = func(string) error { return nil }
				receiptedActionAfterWrite = func(string) error { return nil }

				retryIn := in
				retryIn.Action.CreatedAt = fixedNow.Add(2 * time.Minute).UTC().Format(time.RFC3339)
				recovered, err := s.RecordReceiptedAction(w.ID, retryIn, fixedNow.Add(2*time.Minute))
				if err != nil {
					t.Fatalf("boundary %s retry did not recover: %v", boundary, err)
				}
				wantReplayed := phase == "after_publish" || boundary != "pending"
				if recovered.Replayed != wantReplayed {
					t.Fatalf("boundary %s/%s retry replayed=%t, want %t: %+v", phase, boundary, recovered.Replayed, wantReplayed, recovered)
				}
				wantCreatedAt := action.CreatedAt
				if boundary == "pending" && phase == "before_publish" {
					wantCreatedAt = retryIn.Action.CreatedAt
				}
				if recovered.Action.CreatedAt != wantCreatedAt {
					t.Fatalf("boundary %s/%s recovered created_at=%s, want %s", phase, boundary, recovered.Action.CreatedAt, wantCreatedAt)
				}
				assertReceiptedActionConverged(t, s, w.ID, recovered)

				replayIn := in
				replayIn.Action.CreatedAt = fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339)
				replay, err := s.RecordReceiptedAction(w.ID, replayIn, fixedNow.Add(3*time.Minute))
				if err != nil || !replay.Replayed || replay.Receipt.ID != recovered.Receipt.ID || replay.Transition.ID != recovered.Transition.ID {
					t.Fatalf("boundary %s stable replay failed: err=%v result=%+v", boundary, err, replay)
				}
				assertReceiptedActionConverged(t, s, w.ID, replay)
			})
		}
	}
}

func TestRecordReceiptedActionRefusesConflictingPartialWork(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-conflicting-work", "", fixedNow)
	action := receiptedActionIntent(w, "act_receipted_conflicting_work", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "continue only from a consistent partial transaction",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}

	injected := errors.New("injected after work link")
	previousHook := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(boundary string) error {
		if boundary == "work" {
			return injected
		}
		return nil
	}
	defer func() { receiptedActionAfterWrite = previousHook }()
	if _, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute)); !errors.Is(err, injected) {
		t.Fatalf("did not stop after work boundary: %v", err)
	}
	receiptedActionAfterWrite = func(string) error { return nil }

	conflicting, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	conflicting.IDs.TransitionID = idFor("tr_", "receipted-action", w.ID, action.ID, claimID)
	if err := s.writeJSON(s.workPath(w.ID), &conflicting); err != nil {
		t.Fatal(err)
	}
	if result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "conflicts with persisted work") {
		t.Fatalf("conflicting partial work was recovered: err=%v result=%+v", err, result)
	}
	events, err := s.admissionEventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if contains(event.Attached, action.ID) {
			t.Fatalf("conflicting partial transaction appended an event: %+v", event)
		}
	}
}

func TestRecordReceiptedActionRefusesInvalidWorkEnvelope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-invalid-work", "", fixedNow)
	action := receiptedActionIntent(w, "act_receipted_invalid_work", fixedNow.Add(time.Minute))
	invalid := w
	invalid.Owner = ""
	if err := s.writeJSON(s.workPath(w.ID), &invalid); err != nil {
		t.Fatal(err)
	}

	result, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "do not complete an invalid partial work envelope",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}, fixedNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "work envelope invalid") {
		t.Fatalf("invalid work envelope was completed: err=%v result=%+v", err, result)
	}
	if exists, statErr := s.eventExists(action.ID); statErr != nil || exists {
		t.Fatalf("invalid work envelope wrote action artifact: %v", statErr)
	}
}

func TestRecordReceiptedActionDiscardsTornStagedEventWrite(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-torn-event", "", fixedNow)
	action := receiptedActionIntent(w, "act_receipted_torn_event", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "recover after a torn event append",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}

	injected := errors.New("injected torn event append")
	previousWriter := writeWorkEvent
	failed := false
	writeWorkEvent = func(file *os.File, data []byte) (int, error) {
		if !failed {
			failed = true
			n, _ := file.Write(data[:len(data)/2])
			return n, injected
		}
		return file.Write(data)
	}
	defer func() { writeWorkEvent = previousWriter }()
	if _, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute)); !errors.Is(err, injected) {
		t.Fatalf("torn event append did not return injected error: %v", err)
	}
	if _, err := s.admissionEventsFor(w.ID); err != nil {
		t.Fatalf("torn event append left a malformed canonical tail: %v", err)
	}
	writeWorkEvent = previousWriter
	recovered, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(2*time.Minute))
	if err != nil || !recovered.Replayed {
		t.Fatalf("retry after torn event append did not recover: err=%v result=%+v", err, recovered)
	}
	assertReceiptedActionConverged(t, s, w.ID, recovered)
}

func TestRecordReceiptedActionCrashDuringEventWriteLeavesCanonicalLogIntact(t *testing.T) {
	t.Parallel()
	const crashEnv = "COMPOUND_TEST_RECEIPTED_ACTION_EVENT_CRASH"
	if os.Getenv(crashEnv) == "1" {
		homePath := os.Getenv("COMPOUND_TEST_HOME")
		workID := os.Getenv("COMPOUND_TEST_WORK_ID")
		claimID := os.Getenv("COMPOUND_TEST_CLAIM_ID")
		actionID := os.Getenv("COMPOUND_TEST_ACTION_ID")
		s, err := Open(homePath)
		if err != nil {
			_, _ = os.Stderr.WriteString("open crash-test store: " + err.Error())
			os.Exit(81)
		}
		w, err := s.Get(workID)
		if err != nil {
			_, _ = os.Stderr.WriteString("read crash-test work: " + err.Error())
			os.Exit(82)
		}
		in := receiptedActionCrashInput(w, claimID, actionID)
		writeWorkEvent = func(file *os.File, data []byte) (int, error) {
			n, err := file.Write(data[:len(data)/2])
			if err != nil {
				_, _ = os.Stderr.WriteString("write crash-test event prefix: " + err.Error())
				os.Exit(83)
			}
			if err := file.Sync(); err != nil {
				_, _ = os.Stderr.WriteString("sync crash-test event prefix: " + err.Error())
				os.Exit(84)
			}
			os.Exit(86) // bypass rollback/defer paths like a killed writer
			return n, nil
		}
		if _, err := s.RecordReceiptedAction(workID, in, fixedNow.Add(time.Minute)); err != nil {
			_, _ = os.Stderr.WriteString("record crash-test action: " + err.Error())
			os.Exit(85)
		}
		os.Exit(87)
	}

	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-event-crash", "", fixedNow)
	actionID := "act_receipted_event_crash"
	in := receiptedActionCrashInput(w, claimID, actionID)
	before, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRecordReceiptedActionCrashDuringEventWriteLeavesCanonicalLogIntact$")
	cmd.Env = append(os.Environ(),
		crashEnv+"=1",
		"COMPOUND_TEST_HOME="+s.home,
		"COMPOUND_TEST_WORK_ID="+w.ID,
		"COMPOUND_TEST_CLAIM_ID="+claimID,
		"COMPOUND_TEST_ACTION_ID="+actionID,
	)
	output, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("crash helper exited without reaching the event write: err=%v output=%s", runErr, output)
	}
	afterCrash, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterCrash) != string(before) {
		t.Fatalf("crashed event writer changed canonical log: before=%d bytes after=%d bytes", len(before), len(afterCrash))
	}

	recovered, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(2*time.Minute))
	if err != nil || !recovered.Replayed {
		t.Fatalf("retry after event-writer crash did not recover: err=%v result=%+v", err, recovered)
	}
	assertReceiptedActionConverged(t, s, w.ID, recovered)
}

func TestRecordReceiptedActionControlledEventSerializesGenericAppend(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	claimID := receiptedActionRuling(t, s, "receipted-action-event-lock", "", fixedNow)
	in := receiptedActionCrashInput(w, claimID, "act_receipted_event_lock")

	controlledReady := make(chan struct{})
	releaseControlled := make(chan struct{})
	previousBeforeHook := receiptedActionBeforePublish
	receiptedActionBeforePublish = func(boundary string) error {
		if boundary == "event" {
			close(controlledReady)
			<-releaseControlled
		}
		return nil
	}
	defer func() { receiptedActionBeforePublish = previousBeforeHook }()

	type recordResult struct {
		result ReceiptedActionResult
		err    error
	}
	controlledDone := make(chan recordResult, 1)
	go func() {
		result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
		controlledDone <- recordResult{result: result, err: err}
	}()
	<-controlledReady

	generic := Event{TS: ts(fixedNow.Add(2 * time.Minute)), WorkID: "work_concurrent_event", From: "running", To: "verifying", Note: "generic append racing controlled publication"}
	genericDone := make(chan error, 1)
	go func() { genericDone <- s.appendEvent(generic) }()
	select {
	case err := <-genericDone:
		t.Fatalf("generic append crossed controlled publication lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseControlled)
	controlled := <-controlledDone
	if controlled.err != nil {
		t.Fatalf("controlled publication failed: %v", controlled.err)
	}
	if err := <-genericDone; err != nil {
		t.Fatalf("generic append failed after controlled publication: %v", err)
	}
	assertReceiptedActionConverged(t, s, w.ID, controlled.result)

	body, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	events, err := decodeAdmissionEvents(body, "")
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, event := range events {
		if event.WorkID == generic.WorkID && event.Note == generic.Note {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("generic append survived controlled rename %d times, want exactly once: %+v", matching, events)
	}
}

func receiptedActionCrashInput(w envelope.WorkEnvelope, claimID, actionID string) ReceiptedActionInput {
	return ReceiptedActionInput{
		Action:                     receiptedActionIntent(w, actionID, fixedNow.Add(time.Minute)),
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "recover after the event writer exits mid-publication",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}
}

func TestRecordReceiptedActionDiscardsTornStagedJSONWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, boundary := range []string{"pending", "action", "decision", "receipt", "transition", "work"} {
		t.Run(boundary, func(t *testing.T) {
			s := newStore(t)
			w := receiptedActionRunningWork(t, s)
			claimID := receiptedActionRuling(t, s, "receipted-action-torn-json-"+boundary, "", fixedNow)
			action := receiptedActionIntent(w, "act_receipted_torn_json_"+boundary, fixedNow.Add(time.Minute))
			in := ReceiptedActionInput{
				Action:                     action,
				OperationalDecisionClaimID: claimID,
				ExpectedWorkState:          "running",
				Summary:                    "recover after a torn staged JSON write",
				MoveType:                   "control_plane_repair",
				ProofRefs:                  []string{"claim:" + claimID},
			}

			injected := errors.New("injected torn staged JSON write at " + boundary)
			previousWriter := writeReceiptedActionTemp
			failed := false
			writeReceiptedActionTemp = func(got string, file *os.File, data []byte) error {
				if got == boundary && !failed {
					failed = true
					_, _ = file.Write(data[:len(data)/2])
					return injected
				}
				return previousWriter(got, file, data)
			}
			defer func() { writeReceiptedActionTemp = previousWriter }()
			if _, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute)); !errors.Is(err, injected) {
				t.Fatalf("torn %s stage did not return injected error: %v", boundary, err)
			}
			if _, err := s.Get(w.ID); err != nil {
				t.Fatalf("torn %s stage corrupted canonical work: %v", boundary, err)
			}
			writeReceiptedActionTemp = previousWriter
			recovered, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(2*time.Minute))
			if err != nil {
				t.Fatalf("retry after torn %s stage did not recover: %v", boundary, err)
			}
			assertReceiptedActionConverged(t, s, w.ID, recovered)
		})
	}
}

func assertReceiptedActionConverged(t *testing.T, s *Store, workID string, result ReceiptedActionResult) {
	t.Helper()
	for _, item := range []struct {
		id string
	}{
		{result.Action.ID},
		{result.Decision.ID},
		{result.Receipt.ID},
		{result.Transition.ID},
	} {
		if item.id == "" {
			t.Fatalf("recovered transaction has an empty artifact id: %+v", result)
		}
		if exists, err := s.eventExists(item.id); err != nil || !exists {
			t.Fatalf("artifact %s missing: %v", item.id, err)
		}
	}
	for _, item := range []struct {
		id     string
		target envelope.Envelope
	}{
		{result.Action.ID, &envelope.ActionIntent{}},
		{result.Decision.ID, &envelope.PolicyDecision{}},
		{result.Receipt.ID, &envelope.Receipt{}},
		{result.Transition.ID, &envelope.Transition{}},
	} {
		if err := s.readJSON(s.envPath(item.id), item.target); err != nil {
			t.Fatalf("read recovered artifact %s: %v", item.id, err)
		}
		if err := item.target.Validate(); err != nil {
			t.Fatalf("recovered artifact %s invalid: %v", item.id, err)
		}
	}
	if result.Action.IDs.WorkID != workID || result.Decision.IDs.WorkID != workID || result.Receipt.IDs.WorkID != workID || result.Transition.IDs.WorkID != workID {
		t.Fatalf("recovered artifacts do not share work %s: %+v", workID, result)
	}
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "running" || w.IDs.TransitionID != "" || !contains(w.ActionIntentIDs, result.Action.ID) || w.PolicyDecisionID != result.Decision.ID {
		t.Fatalf("recovered parent link is inconsistent: %+v", w)
	}
	events, err := s.admissionEventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, event := range events {
		if contains(event.Attached, result.Action.ID) && contains(event.Attached, result.Decision.ID) &&
			contains(event.Attached, result.Receipt.ID) && contains(event.Attached, result.Transition.ID) {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("recovered transaction has %d matching events, want exactly one: %+v", matching, events)
	}
	if _, err := os.Stat(receiptedActionPendingTestPath(s, workID)); !os.IsNotExist(err) {
		t.Fatalf("converged transaction left pending marker: %v", err)
	}
}

func receiptedActionArtifactIDs(workID, actionID, claimID string) []string {
	return []string{
		actionID,
		idFor("pol_", "receipted-action", workID, actionID, claimID),
		idFor("rec_", "receipted-action", workID, actionID, claimID),
		idFor("tr_", "receipted-action", workID, actionID, claimID),
	}
}

func receiptedActionPendingTestPath(s *Store, workID string) string {
	return filepath.Join(s.home, "work", "receipted-actions", workID+".json")
}

func receiptedActionEventEntryNames(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(s.eventsLog()))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRecordReceiptedActionSerializesDecisionRevocation(t *testing.T) {
	s := newStore(t)
	w := receiptedActionRunningWork(t, s)
	const decisionID = "receipted-action-race"
	claimID := receiptedActionRuling(t, s, decisionID, "", fixedNow)
	action := receiptedActionIntent(w, "act_receipted_race", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    "continue under the current ruling",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	previousHook := receiptedActionBeforeCommit
	receiptedActionBeforeCommit = func() {
		close(entered)
		<-release
	}
	defer func() { receiptedActionBeforeCommit = previousHook }()

	type actionOutcome struct {
		result ReceiptedActionResult
		err    error
	}
	actionDone := make(chan actionOutcome, 1)
	go func() {
		result, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
		actionDone <- actionOutcome{result: result, err: err}
	}()
	<-entered

	revokeDone := make(chan error, 1)
	go func() {
		_, err := receiptedActionRecordRuling(s, "receipted-action-race-revoked", decisionID, fixedNow.Add(2*time.Minute))
		revokeDone <- err
	}()
	select {
	case err := <-revokeDone:
		t.Fatalf("revocation crossed the receipted-action admission lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	first := <-actionDone
	if first.err != nil || first.result.Receipt.ID == "" {
		t.Fatalf("action holding the lock should linearize first: err=%v result=%+v", first.err, first.result)
	}
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if replay, err := s.RecordReceiptedAction(w.ID, in, fixedNow.Add(3*time.Minute)); err == nil || replay.Replayed || !strings.Contains(err.Error(), "not a current authoritative operational decision") {
		t.Fatalf("revoked ruling authorized replay: err=%v result=%+v", err, replay)
	}
}

func receiptedActionRunningWork(t *testing.T, s *Store) envelope.WorkEnvelope {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "codex", SuccessPredicate: "parent task remains open", Risk: "tier1", Privacy: "private"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func receiptedActionIntent(w envelope.WorkEnvelope, id string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            id,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound", LaneID: "codex"},
			IDs:           envelope.IDs{WorkID: w.ID, SessionID: "sess_receipted", ContextPackID: w.IDs.ContextPackID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"session:sess_receipted", "turn:turn_receipted"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    "harness.stop_feedback",
		Action:        "resume_lane",
		Target:        envelope.ActionTarget{Kind: "harness_turn", ID: "sess_receipted:turn_receipted"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		RollbackPlan:  "retain the Stop event and review any contradiction",
	}
}

func receiptedActionTestInput(w envelope.WorkEnvelope, claimID, actionID, summary string, now time.Time) ReceiptedActionInput {
	return ReceiptedActionInput{
		Action:                     receiptedActionIntent(w, actionID, now),
		OperationalDecisionClaimID: claimID,
		ExpectedWorkState:          "running",
		Summary:                    summary,
		MoveType:                   "control_plane_repair",
		ProofRefs:                  []string{"claim:" + claimID},
	}
}

func receiptedActionRuling(t *testing.T, s *Store, id, undoOf string, now time.Time) string {
	t.Helper()
	claimID, err := receiptedActionRecordRuling(s, id, undoOf, now)
	if err != nil {
		t.Fatal(err)
	}
	return claimID
}

func receiptedActionRecordRuling(s *Store, id, undoOf string, now time.Time) (string, error) {
	store, err := mem.Open(s.home)
	if err != nil {
		return "", err
	}
	var claimID string
	err = home.WithAutonomyAdmissionLock(s.home, func() error {
		qualifiers := map[string]string{
			"kind":             "ruling",
			"source":           "decisions-repo",
			"decision_id":      id,
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"scope":            "reversible work inside an existing envelope",
		}
		if strings.TrimSpace(undoOf) != "" {
			qualifiers["supersedes"] = undoOf
		}
		captured, err := store.Capture([]mem.Candidate{{
			Entity:         "decision:" + id,
			Predicate:      "decided",
			Object:         "Reversible actions inside a scoped grant proceed without operator approval.",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     qualifiers,
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "test:" + id,
			SnippetHash:    id,
			Visibility:     "operational",
		}}, now)
		if err != nil {
			return err
		}
		result, err := store.AdjudicateOperationalDecision(captured[0].ID, "promote", "test-seed", now)
		if err != nil {
			return err
		}
		if result.Claim != nil {
			claimID = result.Claim.ID
		}
		return nil
	})
	return claimID, err
}
