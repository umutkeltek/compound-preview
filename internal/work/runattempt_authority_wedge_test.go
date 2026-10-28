// Wedge-prevention tests for run-attempt authority: an attempt may be bound
// to exactly ONE (act_, pol_) pair per work. The wedge (lived 2026-08-23,
// envelope work_1d968c3f0522) happened when two lifecycle events attached the
// same ratt_ id with different pairs: the close-time gate failed permanently
// with `runattempt ... authority generation is ambiguous (pairs=2)`.
//
// Falsifiers:
//   1. PREVENTION: a second advance that re-attaches a ratt_ id with a
//      DIFFERENT (act_, pol_) pair is REFUSED at advance time with a truthful
//      error naming the existing event, the existing pair, the new pair, and
//      remediation. Pre-fix histories still surface at close (defense in
//      depth) with an error that NAMES both pairs + both events.
//   2. SAME-PAIR IDEMPOTENT RE-ATTACH: a re-attach with the SAME (act_, pol_)
//      pair is allowed; close succeeds under that pair.

package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// writeWedgeLifecycleEvent synthesizes one lifecycle event carrying an
// action + policy + ratt_ triple. Tests use this to construct wedged histories
// deterministically (the production path does not mint two events with the
// same ratt_ + different pairs; that was a bug, not a feature).
func writeWedgeLifecycleEvent(t *testing.T, s *Store, workID, from, to, note string, actionID, policyID, rattID string, at time.Time) {
	t.Helper()
	ev := Event{
		TS:       ts(at),
		WorkID:   workID,
		From:     from,
		To:       to,
		Note:     note,
		Attached: []string{actionID, policyID, rattID},
	}
	if err := s.appendEventUnlocked(ev); err != nil {
		t.Fatalf("append wedge event: %v", err)
	}
}

// writeActionIntentJSON persists a synthetic act_ envelope so
// resolveRunAttemptAuthorityFromEvents can read it back. The fixture must
// pass Validate() — mutation_class=read skips the target.id/path requirement.
func writeActionIntentJSON(t *testing.T, s *Store, id, workID string, at time.Time) {
	t.Helper()
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            id,
			CreatedAt:     at.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    "wedge.test",
		Action:        "wedge-test:fake",
		MutationClass: "read",
		Reversibility: "easy",
	}
	if err := ai.Validate(); err != nil {
		t.Fatalf("fixture action_intent %s invalid: %v", id, err)
	}
	if _, err := s.events.PutJSON(id, &ai, "  "); err != nil {
		// PutJSON refuses overwrites with different content. Wedge fixtures
		// are deterministic: the test author picks one specific (id, ts)
		// tuple and the store is per-test (t.TempDir), so a duplicate id is
		// always a test bug.
		t.Fatalf("put action intent %s: %v", id, err)
	}
}

// writePolicyDecisionJSON persists a synthetic pol_ envelope with the given
// action_intent_id binding. The close-time authority gate calls
// decision.Validate() + policy.Allowed(decision) on it, so Outcome must be
// "allow" (or "allow_with_obligations") and ActionIntentID must match the
// act_ id it pairs with.
func writePolicyDecisionJSON(t *testing.T, s *Store, id, workID, actionID string, at time.Time) {
	t.Helper()
	pd := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            id,
			CreatedAt:     at.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: actionID,
		Outcome:        "allow",
		Reasons:        []string{"wedge fixture decision"},
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("fixture policy_decision %s invalid: %v", id, err)
	}
	if _, err := s.events.PutJSON(id, &pd, "  "); err != nil {
		t.Fatalf("put policy decision %s: %v", id, err)
	}
}

// mintExecutionRunAttempt mints a harness_execution-origin RunAttempt without
// binding an ActionIntent (so resolveRunAttemptAuthorityFromEvents falls
// through to the event-iteration path that scans every attaching event).
func mintExecutionRunAttempt(t *testing.T, s *Store, workID string, at time.Time) runattempt.RunAttempt {
	t.Helper()
	raStore := runattempt.Open(s.home)
	ra, err := raStore.Mint(workID, "codex", runattempt.HarnessPlaceholder, at)
	if err != nil {
		t.Fatalf("mint RunAttempt: %v", err)
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatalf("SetOrigin: %v", err)
	}
	return ra
}

// Falsifier 1 (tiebreak, GREEN after fix): a wedged-history work whose
// close is attempted must be recovered by the deterministic tiebreak
// (the running reservation's PolicyDecisionID is the authoritative pair
// for the lifecycle that executed the attempt). Close must SUCCEED under
// the tiebreak pair, not fail with the loud ambiguous error. The loud
// ambiguous error is reserved for histories where the tiebreak cannot
// resolve (no running event, no matching pair) — see
// TestCloseWedgeLoudErrorWhenNoRunningPolicy.
func TestCloseWedgeNamesBothPairsAndRemediation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-test", SuccessPredicate: "wedge resolves with the running-state policy"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}

	ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow)

	// First (legitimate) attach: real running advance with ratt_. The
	// advance mints its own (act_first, pol_first) and stamps them on the
	// lifecycle event; we capture them here. pol_first is the running
	// reservation's PolicyDecisionID and is therefore the tiebreak pair.
	advanced, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "codex", RunAttemptIDs: []string{ra.ID},
	}, fixedNow)
	if err != nil {
		t.Fatalf("first advance: %v", err)
	}
	firstAction := advanced.ActionIntentIDs[len(advanced.ActionIntentIDs)-1]
	firstPolicy := advanced.PolicyDecisionID

	// Seed a SECOND lifecycle event that re-attaches the SAME ratt_ but
	// with a DIFFERENT (act_, pol_) pair. This is the wedge shape — under
	// the deterministic tiebreak the close resolves to the running pair.
	writeActionIntentJSON(t, s, "act_wedge_second", w.ID, fixedNow.Add(time.Minute))
	writePolicyDecisionJSON(t, s, "pol_wedge_second", w.ID, "act_wedge_second", fixedNow.Add(time.Minute))
	writeWedgeLifecycleEvent(t, s, w.ID, "running", "verifying", "manual advance re-attach",
		"act_wedge_second", "pol_wedge_second", ra.ID, fixedNow.Add(2*time.Minute))

	// Close MUST succeed under the tiebreak pair (firstAction, firstPolicy).
	_, closeErr := s.Close(w.ID, CloseInput{
		Outcome:       "completed",
		MoveType:      "outward_delta",
		Actor:         "codex",
		Summary:       "wedge should resolve via running-policy tiebreak",
		Changed:       true,
		ProofRefs:     []string{"proof:wedge"},
		RunAttemptIDs: []string{ra.ID},
	}, fixedNow.Add(10*time.Minute))
	if closeErr != nil {
		t.Fatalf("close must resolve via tiebreak; got %v", closeErr)
	}
	// The release gate is the SELECTED PAIR, not successful closure: a
	// tiebreak that closes under the WRONG pair while appearing successful
	// is worse than the loud ambiguity it replaced. The close-time gate
	// records exactly what it selected in the attempt's terminalization
	// intent, so assert THAT. (This block previously lived inside
	// `if closeErr != nil` behind a nested fatal and never executed when
	// close succeeded — the test proved nothing.)
	loadedRA, err := runattempt.Open(s.home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedRA.TerminalizationIntent == nil {
		t.Fatal("closed attempt must carry a terminalization intent naming the selected authority")
	}
	if got := loadedRA.TerminalizationIntent.AuthorityActionIntentID; got != firstAction {
		t.Fatalf("tiebreak selected action %s; want running-state pair action %s", got, firstAction)
	}
	if got := loadedRA.TerminalizationIntent.AuthorityPolicyDecisionID; got != firstPolicy {
		t.Fatalf("tiebreak selected policy %s; want running-state pair policy %s", got, firstPolicy)
	}
}

// Falsifier 1b (defense-in-depth): when the tiebreak cannot apply (no
// running event exists), the close-time gate must still fail loudly with
// an error that NAMES both pairs + both events + remediation. This
// protects histories where the wedge shape has no recoverable signal.
func TestCloseWedgeLoudErrorWhenNoRunningPolicy(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-loud", SuccessPredicate: "wedge without running event stays loud"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}

	ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow)

	// Real running advance (with NO ratt_ attached) to put the work in
	// running state. The tiebreak looks for an event with To=running AND
	// the ratt_ in Attached — since this advance has no ratt_, the
	// tiebreak finds no running-with-ratt_ event and the loud error
	// must surface when close tries to resolve authority for the ratt_.
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow); err != nil {
		t.Fatalf("prime running advance: %v", err)
	}
	// No real running advance with ratt_:
	writePolicyDecisionJSON(t, s, "pol_wedge_loud_a", w.ID, "act_wedge_loud_a", fixedNow.Add(time.Minute))
	writeWedgeLifecycleEvent(t, s, w.ID, "claimed", "claimed", "stale attach a", "act_wedge_loud_a", "pol_wedge_loud_a", ra.ID, fixedNow.Add(time.Minute))
	writeActionIntentJSON(t, s, "act_wedge_loud_b", w.ID, fixedNow.Add(2*time.Minute))
	writePolicyDecisionJSON(t, s, "pol_wedge_loud_b", w.ID, "act_wedge_loud_b", fixedNow.Add(2*time.Minute))
	writeWedgeLifecycleEvent(t, s, w.ID, "claimed", "claimed", "stale attach b", "act_wedge_loud_b", "pol_wedge_loud_b", ra.ID, fixedNow.Add(2*time.Minute))

	_, closeErr := s.Close(w.ID, CloseInput{
		Outcome:       "completed",
		MoveType:      "outward_delta",
		Actor:         "codex",
		Summary:       "wedge without tiebreak stays loud",
		Changed:       true,
		ProofRefs:     []string{"proof:wedge-loud"},
		RunAttemptIDs: []string{ra.ID},
	}, fixedNow.Add(10*time.Minute))
	if closeErr == nil {
		t.Fatal("close must fail loudly when no running event exists")
	}
	msg := closeErr.Error()
	for _, want := range []string{"act_wedge_loud_a", "pol_wedge_loud_a", "act_wedge_loud_b", "pol_wedge_loud_b"} {
		if !strings.Contains(msg, want) {
			t.Errorf("close error missing %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "remedia") && !strings.Contains(msg, "Reconcile") {
		t.Errorf("close error missing remediation hint: %s", msg)
	}
}

// Falsifier 2 (prevention, RED before fix): the SECOND advance carrying an
// already-attached ratt_ must NOT write a second attaching event — the
// attach is idempotent-dropped while the advance itself proceeds (same run,
// no new information; a genuinely new execution mints a fresh ratt_). The
// close-time authority gate must see exactly ONE (act_, pol_) pair for the
// ratt_ afterward. This replaces the original refusal semantics: refusal
// broke the legitimate idempotent observe-run re-link (lived red: battery
// go-test, TestObserveRunAfterRunningTerminalizesNormally).
func TestAdvanceRefusesReattachWithDifferentPair(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-prevention", SuccessPredicate: "advance drops duplicate ratt attach idempotently"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}

	// First attach: real running advance with ratt_. Establishes the single
	// attaching event + (act_, pol_) pair binding for this ratt_.
	ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow)
	first, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "codex", RunAttemptIDs: []string{ra.ID},
	}, fixedNow)
	if err != nil {
		t.Fatalf("first advance: %v", err)
	}
	firstAction := first.ActionIntentIDs[len(first.ActionIntentIDs)-1]
	firstPolicy := first.PolicyDecisionID

	// Second advance: same ratt_, state moves running -> verifying. The
	// advance must succeed AND its event must NOT re-attach the ratt_.
	second, err := s.Advance(w.ID, AdvanceInput{
		To: "verifying", Actor: "codex", RunAttemptIDs: []string{ra.ID},
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("second advance must succeed with the duplicate attach dropped: %v", err)
	}
	if second.State != "verifying" {
		t.Fatalf("state = %q, want verifying", second.State)
	}

	// Exactly one lifecycle event attaches the ratt_ (the first advance's).
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.eventsFor(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	attachEvents := 0
	for _, event := range events {
		if attachedHas(event.Attached, ra.ID) {
			attachEvents++
		}
	}
	if attachEvents != 1 {
		t.Fatalf("events attaching %s = %d, want exactly 1 (no wedge written)", ra.ID, attachEvents)
	}

	// The authority gate resolves the single original pair — no wedge.
	loadedRA, err := runattempt.Open(s.home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := s.resolveRunAttemptAuthorityFromEvents(got, loadedRA, events)
	if err != nil {
		t.Fatalf("authority must resolve to the single original pair: %v", err)
	}
	if authority.ActionIntentID != firstAction || authority.PolicyDecisionID != firstPolicy {
		t.Fatalf("authority = (%s,%s); want (%s,%s)",
			authority.ActionIntentID, authority.PolicyDecisionID, firstAction, firstPolicy)
	}
}

// Falsifier 3 (idempotent re-attach, GREEN): a re-attach that uses the
// SAME (act_, pol_) pair is allowed (per packet: legitimate same-id
// re-attach patterns stay allowed). This is the EXCEPTION the prevention
// preserves — the resolveRunAttemptAuthorityFromEvents gate collapses
// same-pair reattaches to a single pair because the pairs map dedupes on
// (act_, pol_) equality.
func TestAdvanceAllowsReattachWithSamePair(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-prevention", SuccessPredicate: "advance allows same-pair re-attach"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow)
	first, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "codex", RunAttemptIDs: []string{ra.ID},
	}, fixedNow)
	if err != nil {
		t.Fatalf("first advance: %v", err)
	}
	firstAction := first.ActionIntentIDs[len(first.ActionIntentIDs)-1]
	firstPolicy := first.PolicyDecisionID

	// Seed a SECOND lifecycle event attaching the SAME ratt_ with the
	// SAME (act_first, pol_first) pair. The act_ and pol_ envelopes were
	// already written by the first advance (and would collide on
	// overwrite) so we only need to write the event itself.
	writeWedgeLifecycleEvent(t, s, w.ID, "running", "verifying", "same-pair re-link",
		firstAction, firstPolicy, ra.ID, fixedNow.Add(time.Minute))

	// The close-time gate must see exactly ONE pair, not two, even though
	// two events attached it. This proves the same-pair re-link is
	// treated as idempotent by the authority gate.
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedRA, err := runattempt.Open(s.home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.eventsFor(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := s.resolveRunAttemptAuthorityFromEvents(got, loadedRA, events)
	if err != nil {
		t.Fatalf("same-pair re-attach must resolve to a single authority: %v", err)
	}
	if authority.ActionIntentID != firstAction || authority.PolicyDecisionID != firstPolicy {
		t.Fatalf("authority = (%s,%s); want (%s,%s)",
			authority.ActionIntentID, authority.PolicyDecisionID, firstAction, firstPolicy)
	}
}

// --- findRunningAuthorityForAttempt: unit falsifiers on plain event slices ---

func TestFindRunningAuthorityForAttemptBindsToExactAttempt(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		events     []Event
		ratt       string
		wantAction string
		wantPolicy string
		wantOK     bool
	}{
		{
			name:   "no events",
			ratt:   "ratt_a",
			events: nil,
		},
		{
			name: "running event attaching a different attempt is ignored",
			ratt: "ratt_a",
			events: []Event{{
				TS: ts(base), To: "running",
				Attached: []string{"act_other", "pol_other", "ratt_b"},
			}},
		},
		{
			name: "running event attaching the attempt resolves its pair",
			ratt: "ratt_a",
			events: []Event{{
				TS: ts(base), To: "running",
				Attached: []string{"act_1", "pol_1", "ratt_a"},
			}},
			wantAction: "act_1", wantPolicy: "pol_1", wantOK: true,
		},
		{
			name: "latest of two running generations wins",
			ratt: "ratt_a",
			events: []Event{
				{TS: ts(base), To: "running", Attached: []string{"act_1", "pol_1", "ratt_a"}},
				{TS: ts(base.Add(time.Hour)), To: "verifying", Attached: []string{"act_1", "pol_1", "ratt_a"}},
				{TS: ts(base.Add(2 * time.Hour)), To: "running", Attached: []string{"act_2", "pol_2", "ratt_a"}},
			},
			wantAction: "act_2", wantPolicy: "pol_2", wantOK: true,
		},
		{
			name: "governing running event with two policies is ambiguity, not preference",
			ratt: "ratt_a",
			events: []Event{{
				TS: ts(base), To: "running",
				Attached: []string{"act_1", "pol_1", "pol_2", "ratt_a"},
			}},
		},
		{
			name: "governing running event with two actions is ambiguity, not preference",
			ratt: "ratt_a",
			events: []Event{{
				TS: ts(base), To: "running",
				Attached: []string{"act_1", "act_2", "pol_1", "ratt_a"},
			}},
		},
		{
			name: "non-ratt id never resolves",
			ratt: "not-an-attempt",
			events: []Event{{
				TS: ts(base), To: "running",
				Attached: []string{"act_1", "pol_1", "not-an-attempt"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, policy, ok := findRunningAuthorityForAttempt(tc.events, tc.ratt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if action != tc.wantAction || policy != tc.wantPolicy {
				t.Fatalf("pair = (%s,%s); want (%s,%s)", action, policy, tc.wantAction, tc.wantPolicy)
			}
		})
	}
}

// Falsifier 2 (lived shape: claim → run → retry → run again): the SAME ratt_
// re-entered `running` under a second (act_, pol_) generation. The first
// running event anywhere in the work history carries the OLD policy; closing
// under it is confidently wrong. The tiebreak must resolve the LATEST running
// generation for the exact attempt.
func TestWedgeTiebreakUsesLatestRunningGenerationForSameAttempt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-gen", SuccessPredicate: "retry closes under its own generation"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow)

	advanced, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "codex", RunAttemptIDs: []string{ra.ID},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	firstAction := advanced.ActionIntentIDs[len(advanced.ActionIntentIDs)-1]
	firstPolicy := advanced.PolicyDecisionID

	// Retry generation: the attempt re-enters running under a NEW pair.
	writeActionIntentJSON(t, s, "act_retry_gen", w.ID, fixedNow.Add(3*time.Minute))
	writePolicyDecisionJSON(t, s, "pol_retry_gen", w.ID, "act_retry_gen", fixedNow.Add(3*time.Minute))
	writeWedgeLifecycleEvent(t, s, w.ID, "verifying", "running", "retry re-entry under new policy",
		"act_retry_gen", "pol_retry_gen", ra.ID, fixedNow.Add(4*time.Minute))

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Resolution validates the action against the work's action chain; the
	// retry generation's action must be part of the chain for a valid close.
	// Extend the in-memory envelope the same way a real retry advance would.
	loaded, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.ActionIntentIDs = append(loaded.ActionIntentIDs, "act_retry_gen")

	authority, err := s.resolveRunAttemptAuthorityFromEvents(loaded, ra, events)
	if err != nil {
		t.Fatalf("tiebreak must resolve the retry generation: %v", err)
	}
	if authority.ActionIntentID != "act_retry_gen" || authority.PolicyDecisionID != "pol_retry_gen" {
		t.Fatalf("authority = (%s,%s); want retry generation (act_retry_gen,pol_retry_gen); first generation was (%s,%s)",
			authority.ActionIntentID, authority.PolicyDecisionID, firstAction, firstPolicy)
	}
}

// Falsifier 3: a DIFFERENT attempt's earlier running event must not decide
// this attempt's authority. The pre-fix lookup scanned for the first
// running event anywhere in the work history; with two attempts that is the
// other attempt's policy, and the close would bind the wrong pair.
func TestWedgeTiebreakIgnoresOtherAttemptsRunningEvents(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "wedge-foreign", SuccessPredicate: "foreign running events do not govern"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	raOther := mintExecutionRunAttempt(t, s, w.ID, fixedNow)
	raMine := mintExecutionRunAttempt(t, s, w.ID, fixedNow.Add(time.Second))

	// An EARLIER running generation for a different attempt of this work.
	writeActionIntentJSON(t, s, "act_foreign", w.ID, fixedNow)
	writePolicyDecisionJSON(t, s, "pol_foreign", w.ID, "act_foreign", fixedNow)
	writeWedgeLifecycleEvent(t, s, w.ID, "claimed", "running", "foreign attempt ran first",
		"act_foreign", "pol_foreign", raOther.ID, fixedNow.Add(time.Minute))

	// My attempt's real running advance mints its own pair.
	if _, err := s.Advance(w.ID, AdvanceInput{
		To: "running", Actor: "codex", RunAttemptIDs: []string{raMine.ID},
	}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Wedge: the foreign pair re-attached to MY attempt on a non-running
	// event, so both pairs are candidates for raMine.
	writeWedgeLifecycleEvent(t, s, w.ID, "running", "verifying", "foreign pair re-attach",
		"act_foreign", "pol_foreign", raMine.ID, fixedNow.Add(3*time.Minute))

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.ActionIntentIDs = append(loaded.ActionIntentIDs, "act_foreign")

	authority, err := s.resolveRunAttemptAuthorityFromEvents(loaded, raMine, events)
	if err != nil {
		t.Fatalf("tiebreak must resolve via my attempt's running event: %v", err)
	}
	if authority.PolicyDecisionID == "pol_foreign" {
		t.Fatalf("tiebreak selected foreign attempt's policy %s; the pre-fix first-running-event-anywhere lookup is exactly this bug", authority.PolicyDecisionID)
	}
	if !strings.HasPrefix(authority.ActionIntentID, "act_") || authority.PolicyDecisionID == "" {
		t.Fatalf("authority = (%s,%s); want the pair minted by my attempt's running advance", authority.ActionIntentID, authority.PolicyDecisionID)
	}
}
