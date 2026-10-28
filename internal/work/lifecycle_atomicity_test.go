package work

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

func TestAdvanceExpectedWorkStatePreventsSecondExecutionReservation(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "executor", SuccessPredicate: "one claimed execution reservation"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "executor", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "executor", ExpectedWorkState: "claimed",
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotLifecycleAuthority(t, s)

	_, err = s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "executor", ExpectedWorkState: "claimed",
	}, fixedNow.Add(3*time.Minute))
	if err == nil || !strings.Contains(err.Error(), `state "running" no longer matches expected state "claimed"`) {
		t.Fatalf("second execution reservation error = %v", err)
	}
	after := snapshotLifecycleAuthority(t, s)
	assertLifecycleAuthorityUnchanged(t, before, after)
	persisted, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.ActionIntentIDs, first.ActionIntentIDs) || persisted.PolicyDecisionID != first.PolicyDecisionID {
		t.Fatalf("losing reservation changed execution authority: first=%+v persisted=%+v", first, persisted)
	}
}

func TestExecutionFinalizationCASRejectsSupersededSameStateGeneration(t *testing.T) {
	newRunning := func(t *testing.T) (*Store, envelope.WorkEnvelope) {
		t.Helper()
		s := newStore(t)
		created, err := s.Create(CreateInput{Owner: "executor", SuccessPredicate: "finalization consumes the exact running generation"}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(created.ID, "executor", fixedNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		running, err := s.Advance(created.ID, AdvanceInput{To: "running", Actor: "executor"}, fixedNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return s, running
	}
	reservation := func(w envelope.WorkEnvelope) (string, string) {
		if len(w.ActionIntentIDs) == 0 {
			t.Fatalf("running work %s has no action intent", w.ID)
		}
		return w.PolicyDecisionID, w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
	}

	t.Run("advance to verifying", func(t *testing.T) {
		s, running := newRunning(t)
		policyID, actionIntentID := reservation(running)
		newer, err := s.Advance(running.ID, AdvanceInput{To: "running", Actor: "executor", Note: "supersede running generation"}, fixedNow.Add(3*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Advance(running.ID, AdvanceInput{
			To:                       "verifying",
			Actor:                    "executor",
			ExpectedWorkState:        "running",
			ExpectedPolicyDecisionID: policyID,
			ExpectedActionIntentID:   actionIntentID,
		}, fixedNow.Add(4*time.Minute))
		if !errors.Is(err, ErrExecutionReservationChanged) {
			t.Fatalf("stale success finalization error = %v, want ErrExecutionReservationChanged", err)
		}
		got, err := s.Get(running.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" || got.PolicyDecisionID != newer.PolicyDecisionID || !reflect.DeepEqual(got.ActionIntentIDs, newer.ActionIntentIDs) {
			t.Fatalf("stale success finalization changed newer generation: got=%+v newer=%+v", got, newer)
		}
	})

	t.Run("close failed", func(t *testing.T) {
		s, running := newRunning(t)
		policyID, actionIntentID := reservation(running)
		newer, err := s.Advance(running.ID, AdvanceInput{To: "blocked", Actor: "executor", Note: "newer block decision"}, fixedNow.Add(3*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Close(running.ID, CloseInput{
			Outcome:                  "failed",
			MoveType:                 "audit_only",
			Actor:                    "executor",
			Summary:                  "stale execution failure",
			Changed:                  true,
			ExpectedWorkState:        "running",
			ExpectedPolicyDecisionID: policyID,
			ExpectedActionIntentID:   actionIntentID,
		}, fixedNow.Add(4*time.Minute))
		if !errors.Is(err, ErrExecutionReservationChanged) {
			t.Fatalf("stale failure finalization error = %v, want ErrExecutionReservationChanged", err)
		}
		got, err := s.Get(running.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "blocked" || got.PolicyDecisionID != newer.PolicyDecisionID || !reflect.DeepEqual(got.ActionIntentIDs, newer.ActionIntentIDs) {
			t.Fatalf("stale failure close changed newer decision: got=%+v newer=%+v", got, newer)
		}
	})
}

func TestAdvanceAutonomyClassMustBeClearAtExecutionCommit(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "executor", SuccessPredicate: "halt wins before mutation-capable execution"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "executor", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{
		DisabledClasses: map[string]string{"build-implement": "incident response"},
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotLifecycleAuthority(t, s)

	_, err = s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "executor", ExpectedWorkState: "claimed", AutonomyClass: "build-implement",
	}, fixedNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "autonomy halted (build-implement)") {
		t.Fatalf("halted execution commit error = %v", err)
	}
	after := snapshotLifecycleAuthority(t, s)
	assertLifecycleAuthorityUnchanged(t, before, after)
	persisted, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "claimed" {
		t.Fatalf("halted execution commit state = %q, want claimed", persisted.State)
	}
}

func TestClaimSerializesWithAdvanceCommit(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "claim cannot interleave with advance commit"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	advancePaused := make(chan struct{})
	releaseAdvance := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAdvance) }) }
	previousHook := advanceBeforeWorkCommit
	advanceBeforeWorkCommit = func() {
		close(advancePaused)
		<-releaseAdvance
	}
	defer func() {
		release()
		advanceBeforeWorkCommit = previousHook
	}()

	type advanceResult struct {
		work envelope.WorkEnvelope
		err  error
	}
	type claimResult struct {
		work envelope.WorkEnvelope
		err  error
	}
	advanceDone := make(chan advanceResult, 1)
	go func() {
		advanced, advanceErr := s.Advance(w.ID, AdvanceInput{To: "held", Actor: "advance-actor"}, fixedNow.Add(time.Minute))
		advanceDone <- advanceResult{work: advanced, err: advanceErr}
	}()
	select {
	case <-advancePaused:
	case <-time.After(5 * time.Second):
		t.Fatal("advance did not reach the pre-commit synchronization seam")
	}

	claimStarted := make(chan struct{})
	claimDone := make(chan claimResult, 1)
	go func() {
		close(claimStarted)
		claimed, claimErr := s.Claim(w.ID, "claim-racer", fixedNow.Add(2*time.Minute))
		claimDone <- claimResult{work: claimed, err: claimErr}
	}()
	<-claimStarted

	// On the broken path Claim ignores the admission lock, writes its lease, and
	// then blocks behind Advance's event lock. On the fixed path it waits before
	// every authoritative write. Give both paths a bounded scheduling window,
	// then release Advance and assert only its transition committed.
	leaseWritten := make(chan struct{}, 1)
	stopProbe := make(chan struct{})
	go func() {
		for {
			if lease, readErr := s.readLease(w.ID); readErr == nil && lease.By == "claim-racer" {
				leaseWritten <- struct{}{}
				return
			}
			select {
			case <-stopProbe:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	select {
	case <-leaseWritten:
	case <-time.After(250 * time.Millisecond):
	}
	close(stopProbe)
	release()

	var advanced advanceResult
	select {
	case advanced = <-advanceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("advance did not finish after release")
	}
	if advanced.err != nil || advanced.work.State != "held" {
		t.Fatalf("advance result = %+v, err=%v; want held", advanced.work, advanced.err)
	}
	var claimed claimResult
	select {
	case claimed = <-claimDone:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not finish after advance released")
	}
	if claimed.err == nil || !strings.Contains(claimed.err.Error(), `cannot claim from state "held"`) {
		t.Fatalf("racing claim = %+v, err=%v; want current held state refusal", claimed.work, claimed.err)
	}
	persisted, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "held" {
		t.Fatalf("persisted work state = %q, want held", persisted.State)
	}
	if lease, err := s.readLease(w.ID); err == nil {
		t.Fatalf("losing racing claim left lease %+v", lease)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read lease after losing claim: %v", err)
	}
}

func TestConcurrentClaimersHaveOneWinnerAndLoserSeesHolder(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "one concurrent claimant wins"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Claim loads policy after reading the WorkEnvelope. The valid padding makes
	// both unlocked callers overlap long enough to falsify the old check-then-write
	// sequence without adding a production-only test hook.
	policyBody := "class_outcome:\n  local_reversible:\n    outcome: allow\n" +
		strings.Repeat("# concurrent claim synchronization padding\n", 125_000)
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte(policyBody), 0o644); err != nil {
		t.Fatal(err)
	}
	previousProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previousProcs)

	type result struct {
		actor string
		work  envelope.WorkEnvelope
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, actor := range []string{"claimant-a", "claimant-b"} {
		actor := actor
		go func() {
			<-start
			claimed, claimErr := s.Claim(w.ID, actor, fixedNow.Add(time.Minute))
			results <- result{actor: actor, work: claimed, err: claimErr}
		}()
	}
	close(start)
	got := []result{<-results, <-results}

	var winner result
	var loser result
	successes := 0
	for _, candidate := range got {
		if candidate.err == nil {
			successes++
			winner = candidate
		} else {
			loser = candidate
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent claim successes = %d, want exactly one: %+v", successes, got)
	}
	if !strings.Contains(loser.err.Error(), `already claimed by "`+winner.actor+`"`) {
		t.Fatalf("loser error = %q, want current claimant %q", loser.err, winner.actor)
	}
	persisted, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "claimed" || persisted.Authority.Owner != winner.actor || lease.By != winner.actor {
		t.Fatalf("winner not authoritative: work=%+v lease=%+v winner=%q", persisted, lease, winner.actor)
	}
}

func TestLifecycleMutationsRejectCorruptEventLogBeforeSideEffects(t *testing.T) {
	type corruption struct {
		name    string
		body    string
		wantErr string
	}
	torn := corruption{name: "torn", body: `{"ts":"torn"`, wantErr: "incomplete trailing record"}
	malformed := corruption{name: "malformed", body: "not-json\n", wantErr: "malformed at line"}

	tests := []struct {
		name       string
		corruption corruption
		prepare    func(*testing.T, *Store) func() error
	}{
		{name: "claim/torn", corruption: torn, prepare: prepareCorruptLogClaim},
		{name: "claim/malformed", corruption: malformed, prepare: prepareCorruptLogClaim},
		{name: "close/torn", corruption: torn, prepare: prepareCorruptLogClose},
		{name: "close/malformed", corruption: malformed, prepare: prepareCorruptLogClose},
		{name: "create/torn", corruption: torn, prepare: prepareCorruptLogCreate},
		{name: "record-action/torn", corruption: torn, prepare: prepareCorruptLogRecordAction},
		{name: "auto-admit-deny/torn", corruption: torn, prepare: prepareCorruptLogAutoAdmitDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			mutate := tt.prepare(t, s)
			appendCorruptWorkEventTail(t, s, tt.corruption.body)
			before := snapshotLifecycleAuthority(t, s)

			err := mutate()
			if err == nil || !strings.Contains(err.Error(), tt.corruption.wantErr) {
				t.Fatalf("mutation error = %v, want %q", err, tt.corruption.wantErr)
			}
			after := snapshotLifecycleAuthority(t, s)
			assertLifecycleAuthorityUnchanged(t, before, after)
		})
	}
}

func prepareCorruptLogClaim(t *testing.T, s *Store) func() error {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "claim fails before corrupt log side effects"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return func() error {
		_, err := s.Claim(w.ID, "claim-actor", fixedNow.Add(time.Minute))
		return err
	}
}

func prepareCorruptLogClose(t *testing.T, s *Store) func() error {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "close fails before corrupt log side effects"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "close-actor", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "close-actor"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return func() error {
		_, err := s.Close(w.ID, CloseInput{
			Outcome:  "failed",
			MoveType: "audit_only",
			Actor:    "close-actor",
			Summary:  "must not persist",
		}, fixedNow.Add(3*time.Minute))
		return err
	}
}

func prepareCorruptLogCreate(t *testing.T, s *Store) func() error {
	t.Helper()
	if _, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "seed lifecycle event"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	return func() error {
		_, err := s.Create(CreateInput{
			WorkID:           "work_corrupt_log_create",
			Owner:            "atomicity-test",
			SuccessPredicate: "create fails before corrupt log side effects",
		}, fixedNow.Add(time.Minute))
		return err
	}
}

func prepareCorruptLogRecordAction(t *testing.T, s *Store) func() error {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "atomicity-test", SuccessPredicate: "record action fails before corrupt log side effects"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "action-actor", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "action-actor"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{
		Header:        newHeader(envelope.SVActionIntent, "act_corrupt_log", fixedNow.Add(3*time.Minute), w.ID, "", "private", "tier1"),
		Actor:         "action-actor",
		Capability:    "capability://skill:atomicity-test",
		Action:        "write_test_artifact",
		Target:        envelope.ActionTarget{Kind: "file", Path: "artifact.invalid"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "write atomicity test artifact",
		RollbackPlan:  "remove atomicity test artifact",
	}
	pd := envelope.PolicyDecision{
		Header:         newHeader(envelope.SVPolicyDecision, "pol_corrupt_log", fixedNow.Add(3*time.Minute), w.ID, "", "private", "tier1"),
		ActionIntentID: ai.ID,
		Outcome:        "allow",
	}
	return func() error {
		_, err := s.RecordAction(w.ID, ai, pd, fixedNow.Add(3*time.Minute))
		return err
	}
}

func prepareCorruptLogAutoAdmitDeny(t *testing.T, s *Store) func() error {
	t.Helper()
	workID := makeProposedDirectorWork(t, s, "job_health", "work_corrupt_log_auto_deny", fixedNow)
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "atomicity test halt"}); err != nil {
		t.Fatal(err)
	}
	return func() error {
		_, err := s.AdmitAuto(workID, AutoAdmitInput{By: "director"}, fixedNow.Add(time.Minute))
		return err
	}
}

func appendCorruptWorkEventTail(t *testing.T, s *Store, tail string) {
	t.Helper()
	file, err := os.OpenFile(s.eventsLog(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(tail); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func snapshotLifecycleAuthority(t *testing.T, s *Store) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, root := range []string{
		filepath.Join(s.home, "work", "work"),
		filepath.Join(s.home, "work", "leases"),
		filepath.Join(s.home, "events"),
	} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(s.home, path)
			if err != nil {
				return err
			}
			if strings.HasPrefix(rel, filepath.Join("events")) &&
				filepath.Base(path) != "work.jsonl" &&
				filepath.Ext(path) != ".json" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = body
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func assertLifecycleAuthorityUnchanged(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if reflect.DeepEqual(before, after) {
		return
	}
	keys := make(map[string]bool, len(before)+len(after))
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	changes := make([]string, 0)
	for _, key := range ordered {
		beforeBody, hadBefore := before[key]
		afterBody, hasAfter := after[key]
		switch {
		case !hadBefore:
			changes = append(changes, "added "+key)
		case !hasAfter:
			changes = append(changes, "removed "+key)
		case !reflect.DeepEqual(beforeBody, afterBody):
			changes = append(changes, "changed "+key)
		}
	}
	t.Fatalf("authoritative lifecycle bytes changed after failed mutation: %s", strings.Join(changes, ", "))
}
