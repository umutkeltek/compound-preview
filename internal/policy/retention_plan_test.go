package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	gb = int64(1_000_000_000)
)

// storageTarget is a synthetic volume with both absolute and ratio free-space floors.
func storageTarget(freeBytes int64) ReclaimTarget {
	return ReclaimTarget{
		Consumer:          "fixture-consumer",
		FreeBytes:         freeBytes,
		CapacityBytes:     994 * gb,
		RequiredFreeBytes: 100 * gb,
		RequiredFreeBps:   1500,
	}
}

// With 114 units free the absolute floor passes while the ratio-derived floor still fails.
func TestShortfallTakesTheStricterOfBytesAndRatio(t *testing.T) {
	target := storageTarget(114 * gb)
	short := target.ShortfallBytes()
	if short <= 0 {
		t.Fatalf("bytes-only thinking says satisfied; ratio says otherwise. short=%d", short)
	}
	want := (994 * gb * 1500 / 10_000) - 114*gb
	if short != want {
		t.Fatalf("shortfall = %d, want %d (the RATIO requirement)", short, want)
	}
}

func TestShortfallZeroWhenBothThresholdsPass(t *testing.T) {
	if got := storageTarget(175 * gb).ShortfallBytes(); got != 0 {
		t.Fatalf("the synthetic target clears both bars, got shortfall %d", got)
	}
}

// A target with no capacity cannot evaluate a ratio and must fall back to the byte floor
// rather than silently treating the ratio as satisfied.
func TestShortfallFallsBackToBytesWithoutCapacity(t *testing.T) {
	target := ReclaimTarget{Consumer: "x", FreeBytes: 50 * gb, RequiredFreeBytes: 100 * gb}
	if got := target.ShortfallBytes(); got != 50*gb {
		t.Fatalf("shortfall = %d, want %d", got, 50*gb)
	}
}

func scanWith(items ...RetentionInstance) RetentionScanReport {
	return RetentionScanReport{Declared: items}
}

// Rebuildable first, always. Losing a build cache costs time; everything else costs data.
func TestPlanOrdersRebuildableFirst(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "agent-worktree", Path: "/wt", Bytes: 50 * gb, Rebuildable: false},
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/wt/target", Bytes: 10 * gb, Rebuildable: true},
	)
	plan := PlanReclaim(scan, storageTarget(140*gb))
	if len(plan.Steps) != 1 {
		t.Fatalf("want exactly the rebuildable step, got %+v", plan.Steps)
	}
	if plan.Steps[0].ClassID != "worktree-build-cache" {
		t.Fatalf("first step = %q, want the rebuildable class", plan.Steps[0].ClassID)
	}
}

// The safety property. An operator-gated class must NEVER be planned for execution, however
// much space it would free — a worktree can hold uncommitted work and is not rebuildable.
func TestPlanNeverExecutesOperatorGatedClasses(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "agent-worktree", Path: "/wt", Bytes: 500 * gb, Rebuildable: false},
	)
	plan := PlanReclaim(scan, storageTarget(10*gb))
	for _, step := range plan.Steps {
		if step.ClassID == "agent-worktree" {
			t.Fatalf("operator-gated class was planned for execution: %+v", step)
		}
	}
	if len(plan.OperatorGated) != 1 {
		t.Fatalf("it must still be REPORTED as an operator option, got %+v", plan.OperatorGated)
	}
	if plan.Sufficient {
		t.Fatal("a plan that cannot reach the target must not claim sufficiency")
	}
}

// `sufficient: false` is the case that matters — it means automation cannot fix this and a
// human must decide. Silently reporting success here is how the guard ran 60 cycles green.
func TestPlanReportsInsufficiencyHonestly(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/a", Bytes: 1 * gb, Rebuildable: true},
	)
	plan := PlanReclaim(scan, storageTarget(10*gb))
	if plan.Sufficient {
		t.Fatalf("1 GB cannot cover a ~139 GB shortfall: %+v", plan)
	}
	if plan.Note == "" || !strings.Contains(plan.Note, "CANNOT") {
		t.Fatalf("note must state the failure plainly, got %q", plan.Note)
	}
}

// Reclaim what the consumer needs, not everything available.
func TestPlanStopsOnceTheTargetIsCovered(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/a", Bytes: 40 * gb, Rebuildable: true},
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/b", Bytes: 40 * gb, Rebuildable: true},
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/c", Bytes: 40 * gb, Rebuildable: true},
	)
	// 140 GB free, need 149.1 GB -> ~9.1 GB short. One step covers it.
	plan := PlanReclaim(scan, storageTarget(140*gb))
	if len(plan.Steps) != 1 {
		t.Fatalf("want 1 step to cover a 9 GB shortfall, got %d: %+v", len(plan.Steps), plan.Steps)
	}
	if !plan.Sufficient {
		t.Fatal("40 GB covers a 9 GB shortfall")
	}
}

func TestPlanIsANoOpWhenAlreadySatisfied(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/a", Bytes: 40 * gb, Rebuildable: true},
	)
	plan := PlanReclaim(scan, storageTarget(175*gb))
	if plan.ShortfallBytes != 0 || len(plan.Steps) != 0 {
		t.Fatalf("nothing to do when satisfied, got %+v", plan)
	}
	if !plan.Sufficient {
		t.Fatal("a satisfied target is trivially sufficient")
	}
}

// Every planned step must carry concrete argv, not a template an executor has to interpolate.
// The path is deliberately space-bearing so a joined shell string cannot masquerade as safe
// execution data.
func TestPlannedStepsCarryConcreteCommandArgv(t *testing.T) {
	scan := scanWith(
		RetentionInstance{ClassID: "worktree-build-cache", Path: "/a/cache target", Bytes: 40 * gb, Rebuildable: true},
	)
	plan := PlanReclaim(scan, storageTarget(100*gb))
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if step.Command == "" || strings.Contains(step.Command, "<path>") {
			t.Fatalf("step command is not concrete: %+v", step)
		}
	}
	if !strings.Contains(string(data), `"command_argv":["rm","-rf","--","/a/cache target"]`) {
		t.Fatalf("plan JSON lacks executable argv for exact instance path: %s", data)
	}
}

// An in-use cache must never be handed to an unattended reclaimer. This machine routinely runs
// several build lanes at once; deleting a target/ mid-compile breaks whoever is using it. The
// bytes are still REPORTED so the operator can see them — they are just not automated.
func TestPlanNeverReclaimsRecentlyActiveCaches(t *testing.T) {
	scan := scanWith(
		RetentionInstance{
			ClassID: "worktree-build-cache", Path: "/live/target",
			Bytes: 50 * gb, Rebuildable: true, RecentlyActive: true,
		},
		RetentionInstance{
			ClassID: "worktree-build-cache", Path: "/idle/target",
			Bytes: 20 * gb, Rebuildable: true,
		},
	)
	plan := PlanReclaim(scan, storageTarget(100*gb))
	for _, step := range plan.Steps {
		if step.Path == "/live/target" {
			t.Fatalf("a build in flight was planned for deletion: %+v", step)
		}
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Path != "/idle/target" {
		t.Fatalf("the idle cache should still be planned, got %+v", plan.Steps)
	}
	found := false
	for _, gated := range plan.OperatorGated {
		if gated.Path == "/live/target" {
			found = true
		}
	}
	if !found {
		t.Fatal("an in-use cache must still be reported, not silently dropped")
	}
}
