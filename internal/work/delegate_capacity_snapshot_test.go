package work

import "testing"

// The delegate readiness snapshot is evaluated twice: once at admission, once
// immediately before RunAttempt mint. A difference between the two is reported
// as `authority_changed` and refuses the spawn. So anything folded into the
// snapshot revision is, by construction, a claim that it is AUTHORITY —
// something whose change should invalidate an admission.
//
// A lane's remaining-token meter is not that. It is a live measurement that
// moves on its own while the orchestrator burns tokens, so folding it in makes
// every spawn on an actively-used account refuse, no matter how healthy the
// lane is. The capacity VERDICT (state, refused, confirmation_required) is the
// thing admission gated on, and it is what must be stable.

// The base decision deliberately uses the `expiring` shape with a POPULATED
// Message. An all-empty-Message fixture is what let the first version of this
// fix ship a hole: capacityDecisionMessage formats the live `LastProven` and
// `DataRef` into that prose, so a test whose Message is always "" cannot see
// the meter re-entering the hash through the string. `expiring` is also the
// state that matters — it is not refused and does not require confirmation, so
// the run proceeds to mint, which is precisely when a spurious
// `authority_changed` costs a real spawn.
func baseCapacityDecision() LaneCapacityGuardResult {
	return LaneCapacityGuardResult{
		Capacity: DispatchLaneCapacity{
			Remaining:  0.82,
			ResetsAt:   "2026-08-01T05:00:00Z",
			LastProven: "2026-08-01T01:40:00Z",
			State:      "expiring",
			DataRef:    "codexbar-live:provider=minimax,window=300m,updated_at=2026-08-01T01:40:00Z",
		},
		Message: "lane minimax capacity admitted by --confirm: state=expiring " +
			"data_row_timestamp=2026-08-01T01:40:00Z " +
			"data_ref=codexbar-live:provider=minimax,window=300m,updated_at=2026-08-01T01:40:00Z " +
			"alternative_ladder=[none recorded]",
	}
}

func snapshotRevisionFor(t *testing.T, decision LaneCapacityGuardResult) string {
	t.Helper()
	readiness, err := completeDelegateReadinessSnapshot(DelegateHarnessReadinessResult{Ready: true}, "claude-minimax", decision)
	if err != nil {
		t.Fatalf("completeDelegateReadinessSnapshot: %v", err)
	}
	if readiness.SnapshotRevision == "" {
		t.Fatal("snapshot revision is empty")
	}
	return readiness.SnapshotRevision
}

// A meter that merely ticked between the two evaluations must NOT read as an
// authority change. This is the regression: before the fix, every field below
// was hashed, so a few tokens of drift refused the spawn.
func TestDelegateReadinessSnapshotIgnoresLiveCapacityMeasurements(t *testing.T) {
	t.Parallel()
	admission := baseCapacityDecision()

	for _, tc := range []struct {
		name   string
		mutate func(*LaneCapacityGuardResult)
	}{
		{"remaining ticked down", func(d *LaneCapacityGuardResult) { d.Capacity.Remaining = 0.7999 }},
		{"remaining exhausted to zero but state unchanged", func(d *LaneCapacityGuardResult) { d.Capacity.Remaining = 0 }},
		{"last proven re-stamped", func(d *LaneCapacityGuardResult) { d.Capacity.LastProven = "2026-08-01T01:41:12Z" }},
		{"window reset time refreshed", func(d *LaneCapacityGuardResult) { d.Capacity.ResetsAt = "2026-08-01T06:00:00Z" }},
		{"data ref repointed", func(d *LaneCapacityGuardResult) { d.Capacity.DataRef = "codexbar://minimax?v=2" }},
		// The live probe restamps per wall-clock second, and those stamps are
		// formatted into the guard's prose. Same verdict, different sentence.
		{"message re-stamped by a one-second-later probe", func(d *LaneCapacityGuardResult) {
			d.Message = "lane minimax capacity admitted by --confirm: state=expiring " +
				"data_row_timestamp=2026-08-01T01:40:01Z " +
				"data_ref=codexbar-live:provider=minimax,window=300m,updated_at=2026-08-01T01:40:01Z " +
				"alternative_ladder=[none recorded]"
		}},
		{"message alternative ladder reworded", func(d *LaneCapacityGuardResult) {
			d.Message = "lane minimax capacity admitted by --confirm: state=expiring " +
				"data_row_timestamp=2026-08-01T01:40:00Z " +
				"data_ref=codexbar-live:provider=minimax,window=300m,updated_at=2026-08-01T01:40:00Z " +
				"alternative_ladder=[claude-zai]"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recheck := baseCapacityDecision()
			tc.mutate(&recheck)
			if got, want := snapshotRevisionFor(t, recheck), snapshotRevisionFor(t, admission); got != want {
				t.Fatalf("live measurement change altered the snapshot revision\n  admission = %s\n  recheck   = %s\nthis refuses every spawn as authority_changed", want, got)
			}
		})
	}
}

// The guard must keep its teeth: a real verdict change still invalidates the
// admission. If this test ever passes trivially, the snapshot has stopped
// carrying capacity at all and the fix has gone too far.
func TestDelegateReadinessSnapshotTracksCapacityVerdict(t *testing.T) {
	t.Parallel()
	admission := baseCapacityDecision()

	for _, tc := range []struct {
		name   string
		mutate func(*LaneCapacityGuardResult)
	}{
		{"lane went exhausted", func(d *LaneCapacityGuardResult) { d.Capacity.State = "exhausted" }},
		{"lane recovered to alive", func(d *LaneCapacityGuardResult) { d.Capacity.State = "alive" }},
		{"guard now refuses", func(d *LaneCapacityGuardResult) { d.Refused = true }},
		{"confirmation now required", func(d *LaneCapacityGuardResult) { d.ConfirmationRequired = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recheck := baseCapacityDecision()
			tc.mutate(&recheck)
			if got, want := snapshotRevisionFor(t, recheck), snapshotRevisionFor(t, admission); got == want {
				t.Fatalf("capacity verdict change did not alter the snapshot revision (%s); the admission guard has lost its teeth", got)
			}
		})
	}
}

// The lane is part of the identity: the same verdict on a different lane is a
// different snapshot.
func TestDelegateReadinessSnapshotSeparatesLanes(t *testing.T) {
	t.Parallel()
	decision := baseCapacityDecision()
	a, err := completeDelegateReadinessSnapshot(DelegateHarnessReadinessResult{Ready: true}, "claude-minimax", decision)
	if err != nil {
		t.Fatalf("minimax: %v", err)
	}
	b, err := completeDelegateReadinessSnapshot(DelegateHarnessReadinessResult{Ready: true}, "claude-qwen", decision)
	if err != nil {
		t.Fatalf("qwen: %v", err)
	}
	if a.SnapshotRevision == b.SnapshotRevision {
		t.Fatalf("two lanes shared one snapshot revision (%s)", a.SnapshotRevision)
	}
}
