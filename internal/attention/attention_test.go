package attention

import (
	"math"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

func TestWorkbenchSafeWorkWatchesWhileOperatorGatesStillNeedYou(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	safeIDs := []string{"work_ready", "work_admitted", "work_proposed"}
	report := Build(Inputs{Workbench: &work.WorkbenchReport{
		GeneratedAt: now.Format(time.RFC3339),
		Open: []work.WorkbenchItem{
			{WorkID: safeIDs[0], State: "ready", SuccessPredicate: "run a local reversible check without publishing", ActionContext: &work.WorkActionContext{Boundary: "stop before public or external side effects"}},
			{WorkID: safeIDs[1], State: "admitted", SuccessPredicate: "prepare admitted local work"},
			{WorkID: safeIDs[2], State: "proposed", SuccessPredicate: "prepare proposed local work"},
			{WorkID: "work_held", State: "held", OpenReason: "operator decision required"},
			{WorkID: "work_blocked", State: "blocked", OpenReason: "conflict requires resolution"},
			{WorkID: "work_conflict", State: "ready", OpenReason: "merge conflict requires resolution"},
			{WorkID: "work_explicit_gate", State: "ready", ActionContext: &work.WorkActionContext{RequiresOperator: true}},
			{WorkID: "work_review_card_gate", State: "proposed", ReviewCards: []work.WorkbenchReviewCard{{RequiresOperator: true}}},
			{WorkID: "work_public", State: "ready", ActionContext: &work.WorkActionContext{ActionSummary: "publish the public release"}},
			{WorkID: "work_external", State: "admitted", ActionContext: &work.WorkActionContext{ActionSummary: "send an external submission"}},
			{WorkID: "work_destructive", State: "proposed", ActionContext: &work.WorkActionContext{ActionSummary: "delete production records"}},
			{WorkID: "work_financial", State: "ready", ActionContext: &work.WorkActionContext{ActionSummary: "approve financial spend"}},
			{WorkID: "work_security", State: "admitted", ActionContext: &work.WorkActionContext{ActionSummary: "rotate a security credential"}},
		},
		DispatchNext: work.WorkbenchDispatchNext{
			CanRun: true, Action: "claim", WorkID: safeIDs[0],
			Command: []string{"compound", "work", "continue", "--expect-work", safeIDs[0]},
		},
	}}, now)

	watching := groupByKey(t, report, "workbench:can-move")
	if watching.Band != BandWatching || watching.Count != len(safeIDs) || !sameStringSet(watching.ItemIDs, safeIDs) {
		t.Fatalf("safe work = band %q count %d ids %v, want %q %d %v", watching.Band, watching.Count, watching.ItemIDs, BandWatching, len(safeIDs), safeIDs)
	}
	if watching.Action == nil || watching.Action.ID != "workbench.dispatch_next" || watching.Action.RequiresConfirmation {
		t.Fatalf("safe work must expose the runnable can-move action without operator confirmation: %+v", watching.Action)
	}

	needsYou := groupByKey(t, report, "workbench:blocked-on-you")
	wantNeedsYou := []string{"work_held", "work_blocked", "work_conflict", "work_explicit_gate", "work_review_card_gate", "work_public", "work_external", "work_destructive", "work_financial", "work_security"}
	if needsYou.Band != BandNeedsYou || needsYou.Count != len(wantNeedsYou) || !sameStringSet(needsYou.ItemIDs, wantNeedsYou) {
		t.Fatalf("operator-gated work = band %q count %d ids %v, want %q %d %v", needsYou.Band, needsYou.Count, needsYou.ItemIDs, BandNeedsYou, len(wantNeedsYou), wantNeedsYou)
	}
	if needsYou.BlastRadius != BlastSecurity || needsYou.Reversibility != Irreversible {
		t.Fatalf("highest gate risk = %q/%q, want %q/%q", needsYou.BlastRadius, needsYou.Reversibility, BlastSecurity, Irreversible)
	}
	if report.Counts.NeedsYou != len(wantNeedsYou) || report.Counts.Watching != len(safeIDs) || report.Counts.BlockedOnYou != len(wantNeedsYou) {
		t.Fatalf("counts = %+v, want needs_you=%d watching=%d blocked_on_you=%d", report.Counts, len(wantNeedsYou), len(safeIDs), len(wantNeedsYou))
	}
}

func TestExecutorProjectionDoesNotRepeatWorkbenchHeldBurden(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{
		Workbench: &work.WorkbenchReport{GeneratedAt: now.Format(time.RFC3339), Open: []work.WorkbenchItem{{WorkID: "work_held", State: "held"}}},
		Executors: &work.ExecutorsProjection{GeneratedAt: now.Format(time.RFC3339), Executors: []work.ExecutorRow{{
			ID: "codex", Label: "Codex", NeedsMeCount: 1,
			CurrentWork: []work.ExecutorWorkRef{{WorkID: "work_held", State: "held"}},
		}}},
	}, now)

	if len(report.Groups) != 1 || report.Groups[0].Key != "workbench:blocked-on-you" {
		t.Fatalf("held work must appear once from its owning projection, got %+v", report.Groups)
	}
	if report.Counts.NeedsYou != 1 || report.Counts.NeedsYouGroups != 1 || report.Counts.BlockedOnYou != 1 {
		t.Fatalf("duplicate executor burden inflated counts: %+v", report.Counts)
	}
}

func TestExecutorProjectionKeepsIndependentDeadExecutorBurden(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{
		Workbench: &work.WorkbenchReport{GeneratedAt: now.Format(time.RFC3339), Open: []work.WorkbenchItem{{WorkID: "work_held", State: "held"}}},
		Executors: &work.ExecutorsProjection{GeneratedAt: now.Format(time.RFC3339), Executors: []work.ExecutorRow{{
			ID: "codex", Label: "Codex", NeedsMeCount: 2,
			CurrentWork: []work.ExecutorWorkRef{
				{WorkID: "work_held", State: "held"},
				{WorkID: "work_dead_executor", State: "running", NeedsMe: "executor process is not alive"},
			},
		}}},
	}, now)

	executor := groupByKey(t, report, "executor:codex")
	if executor.Count != 1 || !sameStringSet(executor.ItemIDs, []string{"work_dead_executor"}) {
		t.Fatalf("independent executor burden = count %d ids %v, want the dead executor only", executor.Count, executor.ItemIDs)
	}
	if report.Counts.NeedsYou != 2 || report.Counts.NeedsYouGroups != 2 {
		t.Fatalf("independent workbench and executor burdens must both remain visible: %+v", report.Counts)
	}
}

func TestContextUnavailableHookEventNeedsOperatorAttention(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{Executors: &work.ExecutorsProjection{
		GeneratedAt: now.Format(time.RFC3339),
		Executors: []work.ExecutorRow{
			{
				ID:    "codex",
				Label: "Codex",
				LastHookEvent: &work.ExecutorHookEventRef{
					CreatedAt:      now.Add(-time.Minute).Format(time.RFC3339),
					Hook:           "UserPromptSubmit",
					Status:         "fail_open",
					FailOpenReason: envelope.FailOpenContextUnavailable,
				},
			},
			{
				ID:    "claude",
				Label: "Claude",
				LastHookEvent: &work.ExecutorHookEventRef{
					CreatedAt:      now.Add(-time.Minute).Format(time.RFC3339),
					Hook:           "SessionEnd",
					Status:         "fail_open",
					FailOpenReason: envelope.FailOpenCapabilityAbsent,
				},
			},
			{
				ID:    "legacy",
				Label: "Legacy",
				LastHookEvent: &work.ExecutorHookEventRef{
					CreatedAt: now.Add(-time.Minute).Format(time.RFC3339),
					Hook:      "UserPromptSubmit",
					Status:    "fail_open",
				},
			},
		},
	}}, now)

	if len(report.Groups) != 1 {
		t.Fatalf("only exact context_unavailable should surface, got %+v", report.Groups)
	}
	group := groupByKey(t, report, "executor-context:codex")
	if group.Band != BandNeedsYou || group.Source != SourceExecutor || group.Kind != KindJobHealth {
		t.Fatalf("context failure group shape = %+v", group)
	}
	const warning = "Context build failed. Harness may be running without Compound context."
	if group.OperatorSummary != warning || group.OperatorUnlock == "" || group.TerminalReason != warning {
		t.Fatalf("context failure copy is not operator-actionable: %+v", group)
	}
	if report.Counts.NeedsYou != 1 || report.Counts.NeedsYouGroups != 1 || report.Counts.BlockedOnYou != 1 {
		t.Fatalf("context failure counts = %+v, want one needs-you group", report.Counts)
	}
}

// F1: stale leases across executors collapse into ONE cross-executor
// lease-hygiene needs_you group carrying the governed sweep action, while
// each row's own job-health group counts ONLY its real burden (dead
// executors, stalled jobs) -- never the stale-lease residue.
func TestLeaseHygieneGroupSweepsStaleLeasesAcrossExecutorsWithoutDoubleCounting(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	staleIDs := []string{"work_stale_1", "work_stale_2", "work_stale_3", "work_stale_4", "work_stale_5"}
	activeIDs := []string{"work_active_1", "work_active_2", "work_active_3"}
	report := Build(Inputs{
		Executors: &work.ExecutorsProjection{
			GeneratedAt: now.Format(time.RFC3339),
			Executors: []work.ExecutorRow{
				{
					ID: "codex", Label: "Codex",
					StaleLeaseCount:   3,
					StaleLeaseWorkIDs: []string{staleIDs[0], staleIDs[1], staleIDs[2]},
					Leases: []work.ExecutorLeaseRef{
						{WorkID: activeIDs[0]},
						{WorkID: activeIDs[1]},
					},
					NeedsMeCount: 1,
					CurrentWork: []work.ExecutorWorkRef{
						{WorkID: "work_dead_executor", State: "running", NeedsMe: "executor died — re-dispatch or release"},
					},
					// The raw dispatch token must never leak onto a needs_you card (F3).
					WaitReason: "proof_ready_close",
				},
				{
					ID: "claude_code", Label: "Claude Code",
					StaleLeaseCount:   2,
					StaleLeaseWorkIDs: []string{staleIDs[3], staleIDs[4]},
					Leases: []work.ExecutorLeaseRef{
						{WorkID: activeIDs[2]},
					},
				},
			},
		},
	}, now)

	lease := groupByKey(t, report, "lease-hygiene")
	if lease.Band != BandNeedsYou || lease.Source != SourceExecutor || lease.Kind != KindHygiene {
		t.Fatalf("lease-hygiene group shape = %+v, want band=%q source=%q kind=%q", lease, BandNeedsYou, SourceExecutor, KindHygiene)
	}
	if lease.Count != 5 || !sameStringSet(lease.ItemIDs, staleIDs) {
		t.Fatalf("lease-hygiene count/ids = %d %v, want 5 %v", lease.Count, lease.ItemIDs, staleIDs)
	}
	for _, activeID := range activeIDs {
		for _, id := range lease.ItemIDs {
			if id == activeID {
				t.Fatalf("active lease id %q leaked into lease-hygiene item_ids: %v", activeID, lease.ItemIDs)
			}
		}
	}
	if lease.Action == nil {
		t.Fatal("lease-hygiene group must carry the governed sweep action")
	}
	wantArgv := []string{"compound", "home", "archive-history", "--kind", "lease"}
	if !sameStringSlice(lease.Action.Argv, wantArgv) {
		t.Fatalf("lease-hygiene action argv = %v, want %v", lease.Action.Argv, wantArgv)
	}
	if lease.Action.Kind != ActionBatch || !lease.Action.RequiresConfirmation {
		t.Fatalf("lease-hygiene action = %+v, want kind=%q requires_confirmation=true", lease.Action, ActionBatch)
	}

	executorGroup := groupByKey(t, report, "executor:codex")
	if executorGroup.Count != 1 || !sameStringSet(executorGroup.ItemIDs, []string{"work_dead_executor"}) {
		t.Fatalf("executor:codex burden = count %d ids %v, want the dead executor only (stale leases must not double-count)", executorGroup.Count, executorGroup.ItemIDs)
	}

	if report.Counts.ActionableNow < 1 {
		t.Fatalf("counts.actionable_now = %d, want the lease-hygiene batch action counted", report.Counts.ActionableNow)
	}

	// F3: no needs_you group anywhere may surface the raw dispatch token.
	for _, g := range report.Groups {
		if g.Band != BandNeedsYou {
			continue
		}
		if g.OperatorUnlock == "proof_ready_close" || g.TerminalReason == "proof_ready_close" {
			t.Fatalf("group %q leaked the raw dispatch token proof_ready_close: unlock=%q terminal=%q", g.Key, g.OperatorUnlock, g.TerminalReason)
		}
	}
}

// F2: with zero stale leases, no lease-hygiene group appears and no active
// lease id ever surfaces in a needs_you group's item_ids -- the negative
// control pinning the bug (active leases masquerading as needing attention).
func TestNoLeaseHygieneGroupOrActiveLeaseLeakWhenNoStaleLeases(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{
		Executors: &work.ExecutorsProjection{
			GeneratedAt: now.Format(time.RFC3339),
			Executors: []work.ExecutorRow{
				{
					ID: "codex", Label: "Codex",
					NeedsMeCount: 1,
					CurrentWork: []work.ExecutorWorkRef{
						{WorkID: "work_dead", State: "running", NeedsMe: "executor died — re-dispatch or release"},
					},
					Leases: []work.ExecutorLeaseRef{
						{WorkID: "work_active_only_1"},
						{WorkID: "work_active_only_2"},
					},
				},
			},
		},
	}, now)

	for _, g := range report.Groups {
		if g.Key == "lease-hygiene" || g.Area == "lease-hygiene" {
			t.Fatalf("lease-hygiene group must not appear when no leases are stale: %+v", g)
		}
	}
	for _, g := range report.Groups {
		if g.Band != BandNeedsYou {
			continue
		}
		for _, id := range g.ItemIDs {
			if id == "work_active_only_1" || id == "work_active_only_2" {
				t.Fatalf("active lease id %q leaked into needs_you group %q: %v", id, g.Key, g.ItemIDs)
			}
		}
	}
}

func TestExplicitAreaClassificationOutranksKeywordSniffing(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{
		Executors: &work.ExecutorsProjection{
			GeneratedAt: now.Format(time.RFC3339),
			Executors: []work.ExecutorRow{
				{
					ID: "codex", Label: "Codex",
					StaleLeaseCount:   1,
					StaleLeaseWorkIDs: []string{"work_stale_only"},
				},
			},
		},
	}, now)

	// The card text says "archive"/"sweep" — the keyword net alone would call
	// that destructive/irreversible, but the sweep is restorable and the area
	// classification says so explicitly.
	lease := groupByKey(t, report, "lease-hygiene")
	if lease.BlastRadius != BlastLocalReversible {
		t.Fatalf("lease-hygiene blast_radius = %q, want %q (explicit classification must outrank keyword sniffing)", lease.BlastRadius, BlastLocalReversible)
	}
	if lease.Reversibility != Reversible {
		t.Fatalf("lease-hygiene reversibility = %q, want %q (the sweep is restorable via restore-history)", lease.Reversibility, Reversible)
	}
}

// TestRiskGradeMirrorsClassAtProjectionTime proves Group.RiskGrade is not a second classification
// path: it is Class(g) frozen onto the wire at normalizeGroup time, for every group from every
// source, using the two example cases from the packet falsifier — an irreversible group grades
// gate, a plain low-blast informational group grades tidy.
func TestRiskGradeMirrorsClassAtProjectionTime(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: now.Format(time.RFC3339),
			Open: []work.WorkbenchItem{
				{WorkID: "work_irreversible_gate", State: "held", ActionContext: &work.WorkActionContext{ActionSummary: "irreversible mutation without verified recovery"}},
			},
		},
		Reviews: &reviewartifact.QueueReport{
			GeneratedAt: now.Format(time.RFC3339),
			Groups: []reviewartifact.QueueGroup{
				{
					Key:             "tidy-informational",
					Area:            "bench",
					Band:            reviewartifact.BandNeedsYou,
					Count:           1,
					OperatorSummary: "Routine bench hygiene note.",
					ItemIDs:         []string{"tidy-1"},
				},
			},
		},
	}, now)

	// Every group's risk_grade equals Class(group) — no group escapes the mirror.
	for _, group := range report.Groups {
		if want := Class(group); group.RiskGrade != want {
			t.Fatalf("group %q risk_grade = %q, want %q (must mirror Class)", group.Key, group.RiskGrade, want)
		}
	}

	blocked := groupByKey(t, report, "workbench:blocked-on-you")
	if blocked.Reversibility != Irreversible {
		t.Fatalf("fixture setup: blocked group reversibility = %q, want %q", blocked.Reversibility, Irreversible)
	}
	if blocked.RiskGrade != RiskGradeGate {
		t.Fatalf("irreversible group risk_grade = %q, want %q", blocked.RiskGrade, RiskGradeGate)
	}

	tidy := groupByKey(t, report, "tidy-informational")
	if tidy.RiskGrade != RiskGradeTidy {
		t.Fatalf("plain low-blast informational group risk_grade = %q, want %q", tidy.RiskGrade, RiskGradeTidy)
	}
}

func sameStringSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func groupByKey(t *testing.T, report Report, key string) Group {
	t.Helper()
	for _, group := range report.Groups {
		if group.Key == key {
			return group
		}
	}
	t.Fatalf("group %q missing from %+v", key, report.Groups)
	return Group{}
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, value := range got {
		seen[value] = true
	}
	for _, value := range want {
		if !seen[value] {
			return false
		}
	}
	return true
}

func TestClassAGateOutranksHighVolumeClassB(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	report := Build(Inputs{Reviews: &reviewartifact.QueueReport{
		GeneratedAt: now.Format(time.RFC3339),
		Groups: []reviewartifact.QueueGroup{
			{
				Key:             "judge",
				Area:            "panel-verdicts",
				Band:            reviewartifact.BandNeedsYou,
				Count:           120,
				OperatorSummary: "Panel verdicts need reversible review.",
				ItemIDs:         []string{"judge"},
				Sample:          &reviewartifact.QueueItem{ID: "judge", Area: "panel-verdicts", CreatedAt: now.Add(-14 * 24 * time.Hour).Format(time.RFC3339)},
			},
			{
				Key:             "gate",
				Area:            "route-remorse",
				Band:            reviewartifact.BandNeedsYou,
				Count:           1,
				OperatorSummary: "An irreversible route demotion needs approval.",
				ItemIDs:         []string{"gate"},
				Sample:          &reviewartifact.QueueItem{ID: "gate", Area: "route-remorse", CreatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339)},
			},
		},
	}}, now)

	if report.Top == nil {
		t.Fatal("top group missing")
	}
	if report.Top.Key != "gate" {
		t.Fatalf("top key = %q, want gate; groups=%+v", report.Top.Key, report.Groups)
	}
	if got := Class(*report.Top); got != "gate" {
		t.Fatalf("top class = %q, want gate", got)
	}
}

func TestLeverageVolumeDampeningIsLogarithmic(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-24 * time.Hour).Format(time.RFC3339)
	one := leverage(1, oldest, "", now)
	four := leverage(4, oldest, "", now)
	if four <= one {
		t.Fatalf("count=4 leverage %.2f should exceed count=1 %.2f", four, one)
	}
	if four >= one*4 {
		t.Fatalf("count=4 leverage %.2f should be damped below linear %.2f", four, one*4)
	}
	if diff := math.Abs(four - one*3); diff > 0.0001 {
		t.Fatalf("count=4 leverage = %.4f, want %.4f", four, one*3)
	}
}

func TestLeverageStaleBumpPast48Hours(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	at48h := now.Add(-48 * time.Hour).Format(time.RFC3339)
	past48h := now.Add(-49 * time.Hour).Format(time.RFC3339)
	got := leverage(1, past48h, "", now)
	withoutBumpAtSameAge := (1 + math.Log2(1)) * clamp(49.0/24.0, 1, 14)
	if diff := math.Abs(got - withoutBumpAtSameAge*1.25); diff > 0.0001 {
		t.Fatalf("stale leverage = %.4f, want %.4f", got, withoutBumpAtSameAge*1.25)
	}
	notStale := leverage(1, at48h, "", now)
	if notStale != 2 {
		t.Fatalf("48h exact leverage = %.4f, want 2.0000 without stale bump", notStale)
	}
}

func TestCountsAddUp(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	cleanAction := reviewartifact.Action{ID: "record", Label: "Record", Argv: []string{"compound", "reviews", "record-finding"}}
	report := Build(Inputs{Reviews: &reviewartifact.QueueReport{
		GeneratedAt: now.Format(time.RFC3339),
		Counts: reviewartifact.QueueCounts{
			NeedsYouFallbackGroupCount: 1,
		},
		Groups: []reviewartifact.QueueGroup{
			{
				Key:             "actionable",
				Area:            "panel-verdicts",
				Band:            reviewartifact.BandNeedsYou,
				Count:           2,
				OperatorSummary: "Two verdicts need review.",
				ItemIDs:         []string{"a", "b"},
				SafeBatchAction: &cleanAction,
			},
			{
				Key:             "fallback",
				Area:            "external-repo-intake",
				Band:            reviewartifact.BandNeedsYou,
				Count:           3,
				OperatorSummary: "Three intake items need review.",
				SummarySource:   reviewartifact.SummarySourceFallback,
				ItemIDs:         []string{"c", "d", "e"},
			},
			{
				Key:             "watching",
				Area:            "job-health",
				Band:            reviewartifact.BandWatching,
				Count:           4,
				OperatorSummary: "Four jobs are being watched.",
				ItemIDs:         []string{"f", "g", "h", "i"},
			},
		},
	}}, now)

	if report.Counts.NeedsYou != 5 {
		t.Fatalf("needs_you = %d, want 5", report.Counts.NeedsYou)
	}
	if report.Counts.NeedsYouGroups != 2 {
		t.Fatalf("needs_you_groups = %d, want 2", report.Counts.NeedsYouGroups)
	}
	if report.Counts.ActionableNow != 0 {
		t.Fatalf("actionable_now = %d, want 0 because the legacy review action has no authority bundle", report.Counts.ActionableNow)
	}
	legacy := groupByKey(t, report, "actionable").Action
	if legacy == nil || legacy.Kind != ActionCopyOnly || !legacy.RequiresConfirmation {
		t.Fatalf("legacy review action must remain visible but fail closed: %+v", legacy)
	}
	if report.Counts.Watching != 4 {
		t.Fatalf("watching = %d, want 4", report.Counts.Watching)
	}
	if report.Counts.FallbackGroups != 1 {
		t.Fatalf("fallback_groups = %d, want 1", report.Counts.FallbackGroups)
	}
}

func TestAreaMappingCoversKnownAreasAndUnknownDefault(t *testing.T) {
	known := []string{
		"panel-verdicts",
		"route-promotions",
		"synthesis-contradictions",
		"external-repo-intake",
		"job-health",
		"product-action-cards",
		"bench",
		"synthesis-sweeps",
		"capability-evaluation-intake",
		"source-control-cleanup",
		"worktree-retirement",
		"capability-lifecycle-proposals",
		"observation-candidates",
		"source-observation-verdicts",
		"distiller-improvements",
		"distiller-requests",
		"parity",
		"route-remorse",
		"lease-hygiene",
		"activity-unaccounted",
	}
	if len(knownAreaClassifications) != len(known) {
		t.Fatalf("knownAreaClassifications has %d entries, want %d", len(knownAreaClassifications), len(known))
	}
	for _, area := range known {
		t.Run(area, func(t *testing.T) {
			kind, blast, reversibility := ClassifyArea(area)
			if _, ok := knownAreaClassifications[area]; !ok {
				t.Fatalf("area %q missing from mapping", area)
			}
			if kind == "" || blast == "" || reversibility == "" {
				t.Fatalf("classification for %q has empty field: kind=%q blast=%q reversibility=%q", area, kind, blast, reversibility)
			}
		})
	}
	kind, blast, reversibility := ClassifyArea("unknown-area")
	if kind != KindAdjudicate || blast != BlastLocalReversible || reversibility != Reversible {
		t.Fatalf("unknown default = %q/%q/%q, want %q/%q/%q", kind, blast, reversibility, KindAdjudicate, BlastLocalReversible, Reversible)
	}
}

// TestNeedsYouLastResortSummaryCarriesWhyAndAction pins the normalizeGroup
// net: when no producer authored a summary, a needs_you group still states
// why it is in front of the operator and one concrete action — and the prose
// holds the shared eighth-grade ceiling. Renderers print Area/Key beside the
// sentence, so identity lives in those fields, not in denser prose.
func TestNeedsYouLastResortSummaryCarriesWhyAndAction(t *testing.T) {
	now := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	report := Build(Inputs{Reviews: &reviewartifact.QueueReport{
		Groups: []reviewartifact.QueueGroup{
			{Key: "bare", Area: "", Band: reviewartifact.BandNeedsYou, Status: "review_required", Count: 2},
		},
	}}, now)
	if len(report.Groups) == 0 {
		t.Fatal("no attention group built")
	}
	g := report.Groups[0]
	if g.Band != BandNeedsYou {
		t.Fatalf("band = %q", g.Band)
	}
	if g.OperatorSummary != "2 items need your decision." {
		t.Fatalf("summary = %q, want the decision why", g.OperatorSummary)
	}
	if g.OperatorUnlock == "" {
		t.Fatal("needs_you last-resort group carries no action unlock")
	}
	if reviewartifact.GradeLevel(g.OperatorSummary) > reviewartifact.ReadabilityGradeCeiling {
		t.Fatalf("last-resort summary above grade: %q (%.2f)", g.OperatorSummary, reviewartifact.GradeLevel(g.OperatorSummary))
	}
	if strings.Contains(g.OperatorSummary, "needs attention") || strings.Contains(g.OperatorSummary, "needs your read") {
		t.Fatalf("generic fallback survived: %q", g.OperatorSummary)
	}
}

// TestWatchingLastResortSummaryStaysPlain pins the non-needs-you branch of
// the same net.
func TestWatchingLastResortSummaryStaysPlain(t *testing.T) {
	now := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	report := Build(Inputs{Reviews: &reviewartifact.QueueReport{
		Groups: []reviewartifact.QueueGroup{
			{Key: "bare-watch", Area: "", Band: reviewartifact.BandWatching, Status: "held", Count: 1},
		},
	}}, now)
	if len(report.Groups) == 0 {
		t.Fatal("no attention group built")
	}
	if summary := report.Groups[0].OperatorSummary; summary != "Background work moved; nothing is needed from you." {
		t.Fatalf("watching summary = %q", summary)
	}
}
