package work

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// writeCoverageTransition persists one Transition event carrying the given
// branch refs at the given time, so coverage falsifiers control the fold order
// purely through created_at.
func writeCoverageTransition(t *testing.T, s *Store, id, workID string, at time.Time, changed bool, branches []envelope.BranchRef) {
	t.Helper()
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            id,
			CreatedAt:     at.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs:    []string{},
		ActionRefs:    []string{},
		AfterRefs:     []string{},
		Projections:   envelope.Projections{},
		SecondOrder:   envelope.SecondOrder{FutureBehaviorChanged: changed},
		OpensBranches: branches,
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("fixture transition invalid: %v", err)
	}
	if _, err := s.events.PutJSON(id, tr, "  "); err != nil {
		t.Fatal(err)
	}
}

// writeCoverageTransitionWithCause persists a cause-carrying transition (the
// eventstore is append-only, so the Cause must be present at write time).
func writeCoverageTransitionWithCause(t *testing.T, s *Store, id, workID string, at time.Time, cause *envelope.TransitionCause) {
	t.Helper()
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            id,
			CreatedAt:     at.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs:  []string{},
		ActionRefs:  []string{},
		AfterRefs:   []string{},
		Projections: envelope.Projections{},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
		Cause:       cause,
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("fixture transition invalid: %v", err)
	}
	if _, err := s.events.PutJSON(id, tr, "  "); err != nil {
		t.Fatal(err)
	}
}

// Falsifier 1: an opened branch that a later transition validates is COVERED —
// it leaves the open/debt set entirely (coverage, not presence).
func TestBranchCoverageOpenThenCoveredLeavesNoDebt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_cov_open", "work_cov_a", t0, true,
		[]envelope.BranchRef{{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "opens"}})
	writeCoverageTransition(t, s, "tr_cov_validate", "work_cov_b", t0.Add(24*time.Hour), true,
		[]envelope.BranchRef{{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "validates"}})

	report, err := s.BranchCoverage(t0.Add(10*24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Opened != 1 || report.Counts.Covered != 1 || report.Counts.Open != 0 || report.Counts.Stale != 0 {
		t.Fatalf("counts = %+v, want opened=1 covered=1 open=0 stale=0", report.Counts)
	}
	if len(report.Debt) != 0 {
		t.Fatalf("covered branch must not sit in debt: %+v", report.Debt)
	}
}

// Falsifier 2: an open branch older than the window surfaces as STALE debt; a
// younger open branch stays quiet. Nothing auto-closes — debt is advisory.
func TestBranchCoverageStaleBranchSurfacesAsDebt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := t0.Add(8 * 24 * time.Hour)
	writeCoverageTransition(t, s, "tr_stale_old", "work_stale_a", t0, true,
		[]envelope.BranchRef{{Branch: "old-branch", Owner: "claude:orchestrator", Kind: "opens"}})
	writeCoverageTransition(t, s, "tr_stale_fresh", "work_stale_b", t0.Add(2*24*time.Hour), true,
		[]envelope.BranchRef{{Branch: "fresh-branch", Owner: "claude:orchestrator", Kind: "opens"}})

	report, err := s.BranchCoverage(now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Open != 2 || report.Counts.Stale != 1 {
		t.Fatalf("counts = %+v, want open=2 stale=1", report.Counts)
	}
	if len(report.Debt) != 2 {
		t.Fatalf("both open branches belong in debt view (stale first): %+v", report.Debt)
	}
	if !report.Debt[0].Stale || report.Debt[0].Branch != "old-branch" {
		t.Fatalf("stale branch must sort first: %+v", report.Debt[0])
	}
	if report.Debt[0].AgeDays != 8 {
		t.Fatalf("age_days = %d, want 8", report.Debt[0].AgeDays)
	}
	if report.Debt[1].Stale {
		t.Fatalf("fresh branch must not be stale: %+v", report.Debt[1])
	}
	if report.WindowDays != 7 {
		t.Fatalf("window_days = %d, want 7", report.WindowDays)
	}
}

// Falsifier 3: covering a branch that was never opened is a DANGLING cover —
// an integrity finding, not silent coverage.
func TestBranchCoverageDanglingCoverFlagged(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_dangle", "work_dangle", t0, true,
		[]envelope.BranchRef{{Branch: "ghost-branch", Owner: "claude:orchestrator", Kind: "supersedes"}})

	report, err := s.BranchCoverage(t0.Add(24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.DanglingCovers != 1 {
		t.Fatalf("dangling_covers = %d, want 1", report.Counts.DanglingCovers)
	}
	if len(report.Dangling) != 1 || report.Dangling[0].Branch != "ghost-branch" || report.Dangling[0].Kind != "supersedes" {
		t.Fatalf("dangling entry wrong: %+v", report.Dangling)
	}
	if report.Dangling[0].ByTransition != "tr_dangle" {
		t.Fatalf("dangling must name its transition: %+v", report.Dangling[0])
	}
}

// Falsifier 4 (evaluation-rule teeth): a transition claiming
// future_behavior_changed with no branch and no next_consumer is counted as an
// UNVERIFIABLE claim; branches or a named consumer make it verifiable.
func TestBranchCoverageUnverifiableChangedClaimsCounted(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_unv_bare", "work_unv_a", t0, true, nil)
	writeCoverageTransition(t, s, "tr_unv_branched", "work_unv_b", t0.Add(time.Hour), true,
		[]envelope.BranchRef{{Branch: "verifiable-branch", Owner: "claude:orchestrator", Kind: "opens"}})
	writeCoverageTransitionWithCause(t, s, "tr_unv_consumer", "work_unv_c", t0.Add(2*time.Hour),
		&envelope.TransitionCause{DeltaCause: "accept", CausedByRefs: []string{"work_unv_c"}, NextConsumer: "workbench"})
	writeCoverageTransition(t, s, "tr_unv_telemetry", "work_unv_d", t0.Add(3*time.Hour), false, nil)

	report, err := s.BranchCoverage(t0.Add(24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.UnverifiableChanged != 1 {
		t.Fatalf("unverifiable_changed = %d, want 1 (only tr_unv_bare)", report.Counts.UnverifiableChanged)
	}
	if len(report.Unverifiable) != 1 || report.Unverifiable[0] != "tr_unv_bare" {
		t.Fatalf("unverifiable list wrong: %+v", report.Unverifiable)
	}
}

// Falsifier 5: re-`opens` on a live slug is an EXPANSION — it keeps the
// original opened_at (age is honest) and never duplicates the branch.
func TestBranchCoverageExpansionKeepsOriginalAge(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_exp_open", "work_exp_a", t0, true,
		[]envelope.BranchRef{{Branch: "expanding-branch", Owner: "claude:orchestrator", Kind: "opens"}})
	writeCoverageTransition(t, s, "tr_exp_expand", "work_exp_b", t0.Add(24*time.Hour), true,
		[]envelope.BranchRef{{Branch: "expanding-branch", Owner: "workbench", Kind: "opens"}})

	report, err := s.BranchCoverage(t0.Add(3*24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Open != 1 || report.Counts.Expansions != 1 {
		t.Fatalf("counts = %+v, want open=1 expansions=1", report.Counts)
	}
	if len(report.Debt) != 1 || report.Debt[0].AgeDays != 3 || report.Debt[0].Expansions != 1 {
		t.Fatalf("expansion must keep original opened_at: %+v", report.Debt)
	}
}

// Falsifier 6: coverage is deterministic — the same event store folds to the
// byte-identical report twice even under the parallel decode (a read model
// other surfaces can trust).
func TestBranchCoverageIsDeterministic(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_det_b", "work_det_b", t0.Add(time.Hour), true,
		[]envelope.BranchRef{{Branch: "b-branch", Owner: "claude:orchestrator", Kind: "opens"}})
	writeCoverageTransition(t, s, "tr_det_a", "work_det_a", t0, true,
		[]envelope.BranchRef{{Branch: "a-branch", Owner: "workbench", Kind: "opens"}})

	now := t0.Add(48 * time.Hour)
	first, err := s.BranchCoverage(now, 7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.BranchCoverage(now, 7)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(second)
	if string(b1) != string(b2) {
		t.Fatalf("coverage is not deterministic:\n%s\nvs\n%s", b1, b2)
	}
	if len(first.Debt) != 2 || first.Debt[0].Branch != "a-branch" {
		t.Fatalf("older branch must sort first: %+v", first.Debt)
	}
}

// Falsifier 7 (fail-closed composite): a corrupt tr_ frame poisons its
// segment, and the error surfaces at the NEXT segment operation — which one is
// engine timing, not contract. The contract that matters: the corruption
// ALWAYS surfaces somewhere (write, coverage, or workbench), and the workbench
// never serves a branch_debt section folded over a poisoned family as if it
// were true. NOTE: this test asserts the data-loss-reggression contract. If
// the engine silently accepts the garbage write AND the subsequent reads
// silently succeed (no fault detectable), the test fails — that's the
// fail-closed signal, not a test bug. A red here means the engine
// tolerates corrupted frames that downstream readers cannot see.
func TestBranchCoverageCorruptFrameFailsClosed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_intact", "work_intact", t0, true,
		[]envelope.BranchRef{{Branch: "intact-branch", Owner: "claude:orchestrator", Kind: "opens"}})
	_, putErr := s.events.PutJSON("tr_garbage", "this is not a transition record", "  ")

	_, covErr := s.BranchCoverage(t0.Add(24*time.Hour), 7)
	report, wbErr := s.Workbench(t0.Add(24*time.Hour), WorkbenchOptions{})

	surfaced := putErr != nil || covErr != nil || wbErr != nil
	for _, note := range report.Integrity {
		if strings.Contains(note, "branch coverage unavailable") {
			surfaced = true
		}
	}
	if !surfaced {
		t.Fatalf("corrupt tr_ frame never surfaced: putErr=%v covErr=%v wbErr=%v integrity=%v",
			putErr, covErr, wbErr, report.Integrity)
	}
	if covErr != nil && report.BranchDebt != nil {
		t.Fatalf("workbench served branch_debt folded over a poisoned family: %+v", report.BranchDebt)
	}
}

// Falsifier 8 (consumer): the workbench carries the bounded branch_debt
// section computed from the real event stream.
func TestWorkbenchSurfacesBranchDebt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_wb_debt", "work_wb_debt", t0, true,
		[]envelope.BranchRef{{Branch: "workbench-visible", Owner: "claude:orchestrator", Kind: "opens"}})

	report, err := s.Workbench(t0.Add(8*24*time.Hour), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.BranchDebt == nil {
		t.Fatalf("workbench must surface branch_debt")
	}
	if report.BranchDebt.Counts.Stale != 1 || report.BranchDebt.Counts.Open != 1 {
		t.Fatalf("branch_debt counts = %+v, want open=1 stale=1", report.BranchDebt.Counts)
	}
	if len(report.BranchDebt.Debt) != 1 || report.BranchDebt.Debt[0].Branch != "workbench-visible" {
		t.Fatalf("branch_debt entry wrong: %+v", report.BranchDebt.Debt)
	}
}

// Falsifier 9 (cross-lane lifecycle): an opened branch is covered by a
// DIFFERENT transition whose id, work, and tail actor are all distinct from
// the opener. Mirrors the real cross-lane shape: a downstream surface
// supersedes the branch, not a self-cover by the same transition that opened
// it. The covered branch leaves the debt set; the cover acts as the
// records-of-record for "the future moved."
func TestBranchCoverageCoveredByDifferentTransition(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCoverageTransition(t, s, "tr_open_a", "work_a", t0, true,
		[]envelope.BranchRef{{Branch: "cross-lane-branch", Owner: "workbench", Kind: "opens"}})
	writeCoverageTransition(t, s, "tr_cover_b", "work_b", t0.Add(24*time.Hour), true,
		[]envelope.BranchRef{{Branch: "cross-lane-branch", Owner: "workbench", Kind: "supersedes"}})

	report, err := s.BranchCoverage(t0.Add(48*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Opened != 1 || report.Counts.Covered != 1 || report.Counts.Open != 0 {
		t.Fatalf("cross-lane cover must remove the open branch: %+v", report.Counts)
	}
	if len(report.Debt) != 0 {
		t.Fatalf("covered branch must not appear in debt: %+v", report.Debt)
	}
	if report.Counts.DanglingCovers != 0 {
		t.Fatalf("a clean cover must not register as dangling: %+v", report.Counts)
	}
}

// Falsifier 10 (unverifiable cap = most-recent): with more unverifiable
// transitions than the JSON cap, the reported list holds the most-recent
// IDs (the operators' actionable surface), while the count is the full fold.
func TestBranchCoverageUnverifiableListIsMostRecent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Exceed the old 40-entry prefix cap; assert every ID from the newest ten.
	total := 100
	for i := 0; i < total; i++ {
		at := t0.Add(time.Duration(i) * time.Hour)
		writeCoverageTransition(t, s,
			fmt.Sprintf("tr_unv_%02d", i), fmt.Sprintf("work_unv_%02d", i), at, true, nil)
	}

	report, err := s.BranchCoverage(t0.Add(24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.UnverifiableChanged != total {
		t.Fatalf("unverifiable count = %d, want %d", report.Counts.UnverifiableChanged, total)
	}
	if len(report.Unverifiable) != branchDebtListCap {
		t.Fatalf("unverifiable list len = %d, want capped at %d",
			len(report.Unverifiable), branchDebtListCap)
	}
	for i, got := range report.Unverifiable {
		want := fmt.Sprintf("tr_unv_%02d", total-1-i)
		if got != want {
			t.Fatalf("unverifiable[%d] = %s, want %s", i, got, want)
		}
	}
}
