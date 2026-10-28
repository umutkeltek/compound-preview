package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestWorkbenchReportsOpenLoopsAndTerminalProof(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ready work gets claimed"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "held work gets reviewed", State: "held"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "running work gets verified"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(running.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(running.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	done, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "terminal work has proof"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(done.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "verifying", ArtifactID: "artifact_terminal", VerificationID: "ver_terminal"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(done.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "workbench terminal proof",
		Changed:   true,
		ProofRefs: []string{"proof:workbench"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 4 || report.Counts.Open != 3 || report.Counts.Terminal != 1 {
		t.Fatalf("counts = %+v", report.Counts)
	}
	if report.Counts.NeedsClaim != 1 || report.Counts.NeedsReview != 1 || report.Counts.InProgress != 1 {
		t.Fatalf("open counts = %+v", report.Counts)
	}
	if report.Counts.MissingReceipt != 0 || report.Counts.MissingTransition != 0 {
		t.Fatalf("terminal proof counts = %+v", report.Counts)
	}
	assertWorkbenchItem(t, report.Open, ready.ID, "needs_claim", "claim before touching")
	assertWorkbenchItem(t, report.Open, held.ID, "needs_review", "resolve policy/source-control hold, then ready or cancel")
	assertWorkbenchItem(t, report.Open, running.ID, "in_progress", "advance verifying, completed, blocked, held, or failed")
	if len(report.Recent) != 1 {
		t.Fatalf("recent = %+v", report.Recent)
	}
	if report.Recent[0].WorkID != done.ID || report.Recent[0].ReceiptID != closed.Receipt.ID || report.Recent[0].TransitionID != closed.Transition.ID {
		t.Fatalf("terminal item missing receipt/transition proof: %+v", report.Recent[0])
	}
	if report.Recent[0].MoveType != "control_plane_repair" || !report.Recent[0].Changed {
		t.Fatalf("terminal item missing receipt learning fields: %+v", report.Recent[0])
	}
	if !contains(report.Recent[0].EvidenceRefs, "proof:workbench") {
		t.Fatalf("terminal item missing receipt proof refs: %+v", report.Recent[0])
	}
}

// TestWorkbenchReportsCorruptWorkFileInIntegrity is the falsifier for the
// silent-swallow bug in (*Store).list: a work/work/<id>.json that fails to
// parse must be counted and surfaced through report.Integrity, not just
// vanish from Counts.Total with no trace anywhere in the report.
func TestWorkbenchReportsCorruptWorkFileInIntegrity(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	healthy, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "healthy work item stays visible"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(s.home, "work", "work", "wk_corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 1 {
		t.Fatalf("want only the one parseable work item counted, got Counts.Total=%d", report.Counts.Total)
	}
	assertWorkbenchItem(t, report.Open, healthy.ID, "needs_claim", "claim before touching")
	if !packetWarningContains(report.Integrity, "wk_corrupt.json") {
		t.Fatalf("corrupt work item was dropped without being reported in Integrity: %+v", report.Integrity)
	}
}

func TestWorkbenchOfferedPacketsSkipsMalformedHeaders(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := `# Projection packet
Date: 2026-07-03
Status: ready
Owner: codex
Proof-medium: cli
Success: projection packet surfaces as offered work.

## Objective
Build one read-only door.
`
	if err := os.WriteFile(filepath.Join(packetDir, "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := `Date: 2026-07-03
Status: ready

## Objective
No title.
`
	if err := os.WriteFile(filepath.Join(packetDir, "bad.md"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{PacketDir: packetDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.OfferedPackets == nil {
		t.Fatal("offered packet surface missing")
	}
	offered := report.OfferedPackets
	if offered.Counts.Total != 2 || offered.Counts.Listed != 1 || offered.Counts.Skipped != 1 || offered.Counts.ByStatus["ready"] != 1 {
		t.Fatalf("offered counts = %+v", offered.Counts)
	}
	if len(offered.Items) != 1 {
		t.Fatalf("offered items = %+v", offered.Items)
	}
	item := offered.Items[0]
	if item.Title != "Projection packet" || item.Status != "ready" || item.Owner != "codex" || item.CanonicalSurface != "packet:projection-packet" {
		t.Fatalf("offered item = %+v", item)
	}
	for _, gate := range []string{"status:ready", "owner:codex", "proof_medium:cli"} {
		if !contains(item.Gates, gate) {
			t.Fatalf("offered gates missing %q: %+v", gate, item.Gates)
		}
	}
	if !packetWarningContains(offered.Warnings, "bad.md") || !packetWarningContains(offered.Warnings, "packet title heading required") {
		t.Fatalf("warnings = %+v", offered.Warnings)
	}
}

func packetWarningContains(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

func TestWorkbenchSurfacesRouteOutcomeReviews(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "route outcome reaches workbench"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "route outcome fixture work",
		Changed:   true,
		ProofRefs: []string{"proof:route-work"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkbenchRouteOutcomeReview(t, s.home, map[string]any{
		"schema_version":    "compound.route_outcome_review.v0.1",
		"id":                "ror_workbench",
		"created_at":        fixedNow.Add(2 * time.Minute).UTC().Format(time.RFC3339),
		"status":            "recorded",
		"route_evidence_id": "route_workbench",
		"request_id":        "gateway:req_workbench",
		"work_id":           w.ID,
		"receipt_id":        closed.Receipt.ID,
		"receipt_outcome":   "completed",
		"route_status":      "completed",
		"selected_target": map[string]any{
			"provider": "fixture-provider",
			"model":    "fixture-model",
			"protocol": "openai-chat",
		},
		"assessment": map[string]any{
			"class":                "route_supported",
			"should_review_policy": false,
		},
	})

	report, err := s.Workbench(fixedNow.Add(3*time.Minute), WorkbenchOptions{RecentLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recent) != 1 {
		t.Fatalf("recent = %+v", report.Recent)
	}
	item := report.Recent[0]
	if item.WorkID != w.ID || len(item.RouteOutcomes) != 1 {
		t.Fatalf("route outcome missing from workbench item: %+v", item)
	}
	out := item.RouteOutcomes[0]
	if out.ID != "ror_workbench" || out.RouteEvidenceID != "route_workbench" || out.ReceiptID != closed.Receipt.ID ||
		out.AssessmentClass != "route_supported" || out.Provider != "fixture-provider" {
		t.Fatalf("bad route outcome summary: %+v", out)
	}
	if !contains(item.EvidenceRefs, "route_outcome_review:ror_workbench") || !contains(item.EvidenceRefs, "route_workbench") {
		t.Fatalf("route outcome proof refs missing: %+v", item.EvidenceRefs)
	}
	if !contains(item.ObservatoryTags, "route:outcome") {
		t.Fatalf("route outcome tag missing: %+v", item.ObservatoryTags)
	}
}

func TestWorkbenchResolvesDispatchStepRunBlockedByTerminalWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch run stale wait resolves"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.DispatchStepRun(fixedNow, DispatchStepRunOptions{
		DispatchStepOptions: DispatchStepOptions{Actor: "codex"},
		MaxSteps:            5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.WaitReason != "run_evidence" || !contains(run.BlockedOn, "work.run_evidence:"+ready.ID) {
		t.Fatalf("dispatch run did not wait on run evidence: %+v", run)
	}
	if _, err := s.Advance(ready.ID, AdvanceInput{To: "verifying", VerificationID: "ver_stale_dispatch"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(ready.ID, CloseInput{
		Outcome:  "completed",
		MoveType: "outward_delta",
		Summary:  "dispatch run wait resolved by terminal work",
		Changed:  true,
	}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(3*time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.DispatchStepRuns) != 1 {
		t.Fatalf("dispatch step runs = %+v", report.DispatchStepRuns)
	}
	got := report.DispatchStepRuns[0]
	if got.Status != "resolved" || got.WaitReason != "terminal_work" || got.CanContinue {
		t.Fatalf("dispatch step run summary not resolved: %+v", got)
	}
	if !contains(got.BlockedOn, "work.terminal:"+ready.ID) || got.NextAction != "referenced work is terminal; no dispatch action required" {
		t.Fatalf("dispatch step run terminal proof not surfaced: %+v", got)
	}
}

func TestWorkbenchRecentOrdersByCompletionTimeNotWorkID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	olderAt := fixedNow
	newerAt := fixedNow.Add(2 * time.Hour)
	var olderInput, newerInput CreateInput
	found := false
	for i := 0; i < 1000; i++ {
		olderInput = CreateInput{Owner: "compound-core", SuccessPredicate: fmt.Sprintf("older terminal %03d", i)}
		newerInput = CreateInput{Owner: "compound-core", SuccessPredicate: fmt.Sprintf("newer terminal %03d", i)}
		older := BuildWork(olderInput, olderAt)
		newer := BuildWork(newerInput, newerAt)
		if older.ID > newer.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("test setup could not find lexicographic id inversion")
	}
	older, err := s.Create(olderInput, olderAt)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.Create(newerInput, newerAt)
	if err != nil {
		t.Fatal(err)
	}
	closeWorkbenchTestWork(t, s, older.ID, olderAt.Add(10*time.Minute), "older completion")
	closeWorkbenchTestWork(t, s, newer.ID, newerAt.Add(10*time.Minute), "newer completion")
	report, err := s.Workbench(newerAt.Add(time.Hour), WorkbenchOptions{RecentLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recent) != 2 {
		t.Fatalf("recent len = %d, want 2: %+v", len(report.Recent), report.Recent)
	}
	if report.Recent[0].WorkID != newer.ID {
		t.Fatalf("recent[0] = %s, want newer %s; older id %s sorts higher lexicographically", report.Recent[0].WorkID, newer.ID, older.ID)
	}
}

func TestWorkbenchExcludesArchivedWorkByDefaultAndResolvesByID(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ready work stays hot"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "completed work can archive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(done.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "verifying", VerificationID: "ver_archive"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(done.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "archived work remains queryable",
		Changed:   true,
		ProofRefs: []string{"proof:archive"},
	}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := home.ArchiveOperationalHistory(s.home, home.OperationalArchiveOptions{Kinds: []string{"work"}}, fixedNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(2*time.Hour), WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Open != 1 || report.Counts.Terminal != 0 || len(report.Recent) != 0 {
		t.Fatalf("default workbench should exclude archived terminal work: counts=%+v recent=%+v", report.Counts, report.Recent)
	}
	assertWorkbenchItem(t, report.Open, ready.ID, "needs_claim", "claim before touching")

	withArchived, err := s.Workbench(fixedNow.Add(2*time.Hour), WorkbenchOptions{RecentLimit: 10, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if withArchived.Counts.Open != 1 || withArchived.Counts.Terminal != 1 || len(withArchived.Recent) != 1 || withArchived.Recent[0].WorkID != done.ID {
		t.Fatalf("include archived workbench should include terminal history: counts=%+v recent=%+v", withArchived.Counts, withArchived.Recent)
	}
	got, err := s.Get(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != done.ID || got.State != "completed" {
		t.Fatalf("archived work lookup = %+v", got)
	}
}

func TestWorkbenchSurfacesRunObservationEvidence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := runningWork(t, s, "observed run gets verified")
	trace := writeTrace(t, s, "trace_workbench_observed", w.ID)
	result, err := s.ObserveRun(w.ID, ObserveRunInput{
		Actor:   "codex",
		TraceID: trace.ID,
		Note:    "workbench should surface this trace",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Open != 1 || report.Counts.InProgress != 1 || report.Counts.RunObservations != 1 {
		t.Fatalf("workbench counts missing observed run: %+v", report.Counts)
	}
	item := findWorkbenchItem(t, report.Open, w.ID)
	if item.State != "verifying" || item.NextAction != "close completed or failed, or return running" {
		t.Fatalf("observed workbench item has wrong state/action: %+v", item)
	}
	if !contains(item.VerificationIDs, trace.ID) || item.TransitionID != "" {
		t.Fatalf("observed workbench item should expose verification without close proof: %+v", item)
	}
	if len(item.RunObservations) != 1 {
		t.Fatalf("run observations = %+v", item.RunObservations)
	}
	observed := item.RunObservations[0]
	if observed.ID != result.ID || observed.TraceID != trace.ID || observed.PolicyDecisionID == "" || observed.ActionIntentID == "" {
		t.Fatalf("run observation summary = %+v result=%+v", observed, result)
	}
	if observed.Note != "workbench should surface this trace" || observed.ResultPath == "" {
		t.Fatalf("run observation summary missing note/path: %+v", observed)
	}
}

func TestWorkbenchSurfacesDispatchStepRunSummaries(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "workbench sees dispatch run"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.DispatchStepRun(fixedNow, DispatchStepRunOptions{
		DispatchStepOptions: DispatchStepOptions{Actor: "codex"},
		MaxSteps:            5,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.DispatchStepRuns != 1 || len(report.DispatchStepRuns) != 1 {
		t.Fatalf("dispatch step run summaries missing: counts=%+v runs=%+v", report.Counts, report.DispatchStepRuns)
	}
	summary := report.DispatchStepRuns[0]
	if summary.ID != run.ID || summary.Status != "waiting" || summary.StepCount != 2 || summary.WaitReason != "run_evidence" {
		t.Fatalf("dispatch step run summary = %+v run=%+v", summary, run)
	}
	if len(summary.StepActions) != 2 || summary.StepActions[0] != "claim" || summary.StepActions[1] != "advance" {
		t.Fatalf("dispatch step run actions = %+v", summary.StepActions)
	}
	if !contains(summary.StepWorkIDs, ready.ID) || !contains(summary.BlockedOn, "work.run_evidence:"+ready.ID) {
		t.Fatalf("dispatch step run refs missing: %+v", summary)
	}
	if summary.ResultPath != run.ResultPath {
		t.Fatalf("dispatch step run path = %q, want %q", summary.ResultPath, run.ResultPath)
	}
}

func TestWorkbenchSurfacesContinueRunSummaries(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "workbench sees continue run"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.ContinueRun(fixedNow, ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: ready.ID},
		MaxSteps:        5,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.ContinueRuns != 1 || len(report.ContinueRuns) != 1 {
		t.Fatalf("continue run summaries missing: counts=%+v runs=%+v", report.Counts, report.ContinueRuns)
	}
	summary := report.ContinueRuns[0]
	if summary.ID != run.ID || summary.Status != "waiting" || summary.StepCount != 2 || summary.WaitReason != "run_evidence" {
		t.Fatalf("continue run summary = %+v run=%+v", summary, run)
	}
	if len(summary.StepActions) != 2 || summary.StepActions[0] != "claim" || summary.StepActions[1] != "advance" {
		t.Fatalf("continue run actions = %+v", summary.StepActions)
	}
	if !contains(summary.StepWorkIDs, ready.ID) || !contains(summary.BlockedOn, "work.run_evidence:"+ready.ID) {
		t.Fatalf("continue run refs missing: %+v", summary)
	}
	if summary.ResultPath != run.ResultPath {
		t.Fatalf("continue run path = %q, want %q", summary.ResultPath, run.ResultPath)
	}
}

func TestWorkbenchResolvesNoOpenWorkDispatchStepRun(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run, err := s.DispatchStepRun(fixedNow, DispatchStepRunOptions{
		DispatchStepOptions: DispatchStepOptions{Actor: "codex"},
		MaxSteps:            5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.WaitReason != "no_open_work" {
		t.Fatalf("fixture dispatch wait = %+v", run)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.DispatchStepRuns != 1 || len(report.DispatchStepRuns) != 1 {
		t.Fatalf("dispatch step run summaries missing: counts=%+v runs=%+v", report.Counts, report.DispatchStepRuns)
	}
	summary := report.DispatchStepRuns[0]
	if summary.ID != run.ID || summary.Status != "resolved" || summary.WaitReason != "no_open_work" || summary.CanContinue {
		t.Fatalf("no-open-work dispatch summary = %+v run=%+v", summary, run)
	}
	if !contains(summary.BlockedOn, "work.ready_or_source_packet") {
		t.Fatalf("no-open-work dispatch blocked_on changed: %+v", summary)
	}
	if summary.NextAction != "no open work at run time; no dispatch action remains for this run" {
		t.Fatalf("no-open-work dispatch next action = %q", summary.NextAction)
	}
}

func TestWorkbenchDispatchNextSuggestsSafeCommand(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ready work can dispatch"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if !next.CanRun || next.Action != "claim" || next.Reason != "ready_work" || next.WorkID != ready.ID {
		t.Fatalf("ready dispatch suggestion = %+v", next)
	}
	want := []string{"compound", "work", "continue", "--actor", "codex", "--expect-work", ready.ID, "--until-wait"}
	if len(next.Command) != len(want) {
		t.Fatalf("dispatch command = %v", next.Command)
	}
	for i := range want {
		if next.Command[i] != want[i] {
			t.Fatalf("dispatch command = %v", next.Command)
		}
	}
}

func TestWorkbenchDispatchNextSurfacesAdjacentProgressForHeldValves(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "held operator valve remains visible", State: "held"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	readyPacket := `# Adjacent proof packet
Date: 2026-07-04
Status: ready
Owner: codex
Success: proves an adjacent slice without clearing the held valve.
`
	if err := os.WriteFile(filepath.Join(packetDir, "adjacent-ready.md"), []byte(readyPacket), 0o644); err != nil {
		t.Fatal(err)
	}
	heldPacket := `# Held packet
Date: 2026-07-04
Status: held (operator action)
Owner: codex
Success: this should not count as adjacent progress.
`
	if err := os.WriteFile(filepath.Join(packetDir, "held.md"), []byte(heldPacket), 0o644); err != nil {
		t.Fatal(err)
	}
	consolePacket := `# Console packet
Date: 2026-07-04
Status: ready - CONSOLE LANE'S CLAIM
Owner: claude-console-lane
Success: should not be offered as Codex adjacent progress.
`
	if err := os.WriteFile(filepath.Join(packetDir, "console.md"), []byte(consolePacket), 0o644); err != nil {
		t.Fatal(err)
	}
	operatorPacket := `# Operator packet
Date: 2026-07-04
Status: ready (operator registration flip remaining)
Owner: codex + operator
Success: should not be offered as Codex adjacent progress.
`
	if err := os.WriteFile(filepath.Join(packetDir, "operator.md"), []byte(operatorPacket), 0o644); err != nil {
		t.Fatal(err)
	}
	deferredPacket := `# Deferred packet
Date: 2026-07-04
Status: offered (LOW PRIORITY - deferred)
Owner: codex
Success: should not be offered as immediate adjacent progress.
`
	if err := os.WriteFile(filepath.Join(packetDir, "deferred.md"), []byte(deferredPacket), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex", PacketDir: packetDir})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if next.CanRun || next.WaitReason != "needs_review" || !contains(next.BlockedOn, "work.review:"+held.ID) {
		t.Fatalf("held valve dispatch next = %+v", next)
	}
	if next.AdjacentProgress == nil {
		t.Fatalf("held valve should expose adjacent progress: %+v", next)
	}
	adjacent := next.AdjacentProgress
	if !adjacent.Available || adjacent.PacketCandidateCount != 1 || !contains(adjacent.PacketRefs, "adjacent-ready.md") {
		t.Fatalf("adjacent progress packets = %+v", adjacent)
	}
	for _, leaked := range []string{"console.md", "operator.md", "deferred.md", "held.md"} {
		if contains(adjacent.PacketRefs, leaked) {
			t.Fatalf("adjacent progress leaked non-actionable packet %s: %+v", leaked, adjacent)
		}
	}
	if !contains(adjacent.HeldWorkIDs, held.ID) || !strings.Contains(adjacent.NextAction, "held valves visible") {
		t.Fatalf("adjacent progress held valve context = %+v", adjacent)
	}
	if !contains(adjacent.Command, "--packet-dir") || !contains(adjacent.Command, packetDir) {
		t.Fatalf("adjacent progress command = %+v", adjacent.Command)
	}
}

func TestWorkbenchDispatchNextOffersNonPacketActionsWhenPacketsFilteredOut(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	asc, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "app-store-connect ASC private config and scheduler valve remains held",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "chatgpt conversation import and memory promotion valve remains held",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	consolePacket := `# Console-only packet
Date: 2026-07-04
Status: ready - CONSOLE LANE'S CLAIM
Owner: claude-console-lane
Success: should not count as Codex adjacent work.
`
	if err := os.WriteFile(filepath.Join(packetDir, "console.md"), []byte(consolePacket), 0o644); err != nil {
		t.Fatal(err)
	}
	operatorPacket := `# Operator registration packet
Date: 2026-07-04
Status: ready (operator registration flip remaining)
Owner: codex + operator
Success: should not count as Codex adjacent work.
`
	if err := os.WriteFile(filepath.Join(packetDir, "operator.md"), []byte(operatorPacket), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex", PacketDir: packetDir})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if next.CanRun || next.WaitReason != "needs_review" {
		t.Fatalf("held dispatch changed semantics: %+v", next)
	}
	if !contains(next.BlockedOn, "work.review:"+asc.ID) || !contains(next.BlockedOn, "work.review:"+conversation.ID) {
		t.Fatalf("held work missing from direct blockage: %+v", next.BlockedOn)
	}
	adjacent := next.AdjacentProgress
	if adjacent == nil || !adjacent.Available || adjacent.PacketCandidateCount != 0 || len(adjacent.PacketRefs) != 0 {
		t.Fatalf("adjacent progress should be non-packet available only: %+v", adjacent)
	}
	for _, id := range []string{
		"operator-gate-" + asc.ID + "-app-store-connect-private-config",
		"asc-sales-series-bridge-proof",
		"conversation-preflight-private-export",
		"conversation-memory-review-gate",
		"second-opinion-evidence-recovery",
		"codex-pretooluse-native-gate-proof",
		"workbench-projection-refresh",
		"architecture-doctor-strict",
		"proof-check-installed-runtime",
	} {
		action, ok := adjacentActionByID(adjacent.NonPacketActions, id)
		if !ok {
			t.Fatalf("missing adjacent non-packet action %s: %+v", id, adjacent.NonPacketActions)
		}
		joined := strings.Join(action.Command, " ")
		if strings.Contains(joined, "/Users/") || strings.Contains(joined, ".compound/private") {
			t.Fatalf("adjacent action leaked private path in command %s: %+v", id, action.Command)
		}
	}
	for _, id := range []string{"operator-gate-" + asc.ID + "-app-store-connect-private-config", "conversation-preflight-private-export", "conversation-memory-review-gate"} {
		action, _ := adjacentActionByID(adjacent.NonPacketActions, id)
		if !action.RequiresOperator || action.Boundary == "" || action.Proof == "" || len(action.RequiredInputs) == 0 || len(action.ExpectedValidation) == 0 || len(action.StopConditions) == 0 {
			t.Fatalf("private preflight action missing operator boundary/proof/inputs/gates: %+v", action)
		}
	}
	ascAction, _ := adjacentActionByID(adjacent.NonPacketActions, "operator-gate-"+asc.ID+"-app-store-connect-private-config")
	if !contains(ascAction.RequiredInputs, "private ASC product mapping config") {
		t.Fatalf("ASC preflight action should name private config input: %+v", ascAction)
	}
	if !contains(ascAction.StopConditions, "credential smoke fails before scheduler load") || !contains(ascAction.ExpectedValidation, "no product signal apply and no scheduler load") {
		t.Fatalf("ASC preflight action should name stop/validation gates: %+v", ascAction)
	}
	if _, ok := adjacentActionByID(adjacent.NonPacketActions, "asc-preflight-private-config"); ok {
		t.Fatalf("generic ASC preflight should be suppressed by exact review card: %+v", adjacent.NonPacketActions)
	}
	bridgeAction, _ := adjacentActionByID(adjacent.NonPacketActions, "asc-sales-series-bridge-proof")
	if bridgeAction.RequiresOperator || !strings.Contains(strings.Join(bridgeAction.Command, " "), "bun test tools/asc_sales_series_bridge.test.ts") {
		t.Fatalf("ASC bridge proof should be a non-operator synthetic test action: %+v", bridgeAction)
	}
	if !strings.Contains(bridgeAction.Boundary, "temp COMPOUND_HOME") || strings.Contains(bridgeAction.Boundary, "operator supplies") || len(bridgeAction.RequiredInputs) != 0 {
		t.Fatalf("ASC bridge proof should use synthetic/temp boundary, not private config: %+v", bridgeAction)
	}
	if !contains(bridgeAction.ExpectedValidation, "apply_results shows signal_count and transition_count") ||
		!contains(bridgeAction.StopConditions, "bridge report lacks apply_results") {
		t.Fatalf("ASC bridge proof should name apply_results validation gates: %+v", bridgeAction)
	}
	conversationAction, _ := adjacentActionByID(adjacent.NonPacketActions, "conversation-preflight-private-export")
	if !contains(conversationAction.RequiredInputs, "private conversation export file or directory") {
		t.Fatalf("conversation preflight action should name private export input: %+v", conversationAction)
	}
	if !contains(conversationAction.StopConditions, "bulk promotion requested") ||
		!contains(conversationAction.ExpectedValidation, "no live extraction or promotion") ||
		!contains(conversationAction.ExpectedValidation, "import_safety proves private bounded evidence/candidates and zero temp promoted claims") {
		t.Fatalf("conversation preflight action should name stop/validation gates: %+v", conversationAction)
	}
	reviewGate, _ := adjacentActionByID(adjacent.NonPacketActions, "conversation-memory-review-gate")
	reviewGateCommand := strings.Join(reviewGate.Command, " ")
	if !strings.Contains(reviewGateCommand, "compound panel plan") ||
		!strings.Contains(reviewGateCommand, "--target-kind memory_candidate") ||
		!strings.Contains(reviewGateCommand, "--privacy-sensitive") ||
		!strings.Contains(reviewGateCommand, "--max-items 1") {
		t.Fatalf("conversation review gate should plan one privacy-sensitive memory candidate review: %+v", reviewGate)
	}
	if strings.Contains(reviewGateCommand, "compound memory adjudicate") ||
		!strings.Contains(reviewGate.Boundary, "no live import") ||
		!strings.Contains(reviewGate.Boundary, "adjudication") ||
		!strings.Contains(reviewGate.Boundary, "promotion") ||
		!strings.Contains(reviewGate.Proof, "no compound memory adjudicate --verdict promote") ||
		!contains(reviewGate.ExpectedValidation, "compound memory adjudicate --verdict promote is not run") ||
		!contains(reviewGate.StopConditions, "review plan would run adjudication or promotion") {
		t.Fatalf("conversation review gate should stop before adjudication/promotion: %+v", reviewGate)
	}
	oracleAction, _ := adjacentActionByID(adjacent.NonPacketActions, "second-opinion-evidence-recovery")
	if !oracleAction.RequiresOperator || !strings.Contains(strings.Join(oracleAction.Command, " "), "tools/second_opinion_preflight.ts") || !strings.Contains(strings.Join(oracleAction.Command, " "), "--source-observation") || len(oracleAction.RequiredInputs) != 3 || len(oracleAction.ExpectedValidation) == 0 || len(oracleAction.StopConditions) == 0 {
		t.Fatalf("second-opinion action should require operator and run preflight before source observe: %+v", oracleAction)
	}
	for _, input := range []string{"second-opinion session id", "private saved answer path", "private source-observation YAML path"} {
		if !contains(oracleAction.RequiredInputs, input) {
			t.Fatalf("second-opinion action missing required input %q: %+v", input, oracleAction)
		}
	}
	if !strings.Contains(oracleAction.Boundary, "evidence, not authority") || !strings.Contains(oracleAction.Proof, "no-promote") {
		t.Fatalf("second-opinion action should preserve evidence/no-promote boundary: %+v", oracleAction)
	}
	if !contains(oracleAction.StopConditions, "source observation would promote truth") || !contains(oracleAction.ExpectedValidation, "no source observe import during preflight") {
		t.Fatalf("second-opinion action should name stop/validation gates: %+v", oracleAction)
	}
	hookAction, _ := adjacentActionByID(adjacent.NonPacketActions, "codex-pretooluse-native-gate-proof")
	if hookAction.RequiresOperator ||
		!strings.Contains(strings.Join(hookAction.Command, " "), "compound proof-check --json") ||
		!strings.Contains(hookAction.Boundary, "temporary Compound home") ||
		!strings.Contains(hookAction.Boundary, "no hook install") ||
		!contains(hookAction.ExpectedValidation, "mixed valid and misplaced-canon apply_patch receives permissionDecision deny") ||
		!contains(hookAction.StopConditions, "proof requires live hook installation or harness config mutation") {
		t.Fatalf("hook gate proof should exercise native allow/deny without registration mutation: %+v", hookAction)
	}
	if !strings.Contains(adjacent.NextAction, "listed non-packet proof action") {
		t.Fatalf("adjacent next action should point to action cards: %q", adjacent.NextAction)
	}
}

func TestWorkbenchAdjacentProgressConsumesVerifiedProofActions(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	asc, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "app-store-connect ASC private config scheduler valve remains held",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "chatgpt conversation import memory promotion valve remains held",
		State:            "held",
	}, fixedNow); err != nil {
		t.Fatal(err)
	}
	refsDir := filepath.Join(s.home, "artifacts", "refs")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proofs := map[string]string{
		"asc-sales-series-bridge-proof-20260705.md":        "`asc-sales-series-bridge-proof` adjacent Workbench action verified synthetic source-flow proof. Do not rerun proof next move unless bridge contract regresses.",
		"chatgpt-conversation-preflight-proof-20260705.md": "`conversation-preflight-private-export` adjacent Workbench action now verified on bounded private corpus preflight.",
	}
	for name, body := range proofs {
		if err := os.WriteFile(filepath.Join(refsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	adjacent := report.DispatchNext.AdjacentProgress
	if adjacent == nil || !adjacent.Available {
		t.Fatalf("adjacent progress missing: %+v", report.DispatchNext)
	}
	for _, id := range []string{"asc-sales-series-bridge-proof", "conversation-preflight-private-export"} {
		if _, ok := adjacentActionByID(adjacent.NonPacketActions, id); ok {
			t.Fatalf("consumed proof action %s should not stay active: %+v", id, adjacent.NonPacketActions)
		}
		action, ok := adjacentActionByID(adjacent.ConsumedNonPacketActions, id)
		if !ok {
			t.Fatalf("consumed proof action %s missing consumed evidence: %+v", id, adjacent.ConsumedNonPacketActions)
		}
		if !action.Consumed || len(action.Command) != 0 || len(action.ConsumedEvidenceRefs) == 0 {
			t.Fatalf("consumed proof action should be non-runnable evidence: %+v", action)
		}
	}
	for _, id := range []string{"operator-gate-" + asc.ID + "-app-store-connect-private-config", "conversation-memory-review-gate", "architecture-doctor-strict"} {
		if _, ok := adjacentActionByID(adjacent.NonPacketActions, id); !ok {
			t.Fatalf("unconsumed adjacent action %s should remain active: %+v", id, adjacent.NonPacketActions)
		}
	}
	if !strings.Contains(adjacent.NextAction, "consumed adjacent proofs") {
		t.Fatalf("next action should name consumed proof actions: %q", adjacent.NextAction)
	}
}

func TestWorkbenchSurfacesRunArtifactsForHeldWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held conversation memory review has existing run artifacts",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.home, "runs", held.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidateDir := filepath.Join(s.home, "inbox", "memory-candidates")
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(candidateDir, "cand_reviewed_conversation.json"), map[string]any{
		"id":               "cand_reviewed_conversation",
		"state":            "parked",
		"entity":           "knowledge:chatgpt:fixture",
		"predicate":        "proposes_strategy",
		"object":           "Compound should show the actual memory candidate before asking for judgment.",
		"confidence":       0.72,
		"visibility":       "private",
		"source_trace_ref": "knowledge_extraction:kx_fixture",
		"qualifiers": map[string]any{
			"extraction_id": "kx_fixture",
			"extracted_by":  "openai-compatible/test-model",
			"item_ref":      "chatgpt:fixture-conversation",
			"source":        "knowledge_extraction",
		},
	})
	writeJSONFile(t, filepath.Join(candidateDir, "cand_other_conversation.json"), map[string]any{
		"id":    "cand_other_conversation",
		"state": "parked",
	})
	writeJSONFile(t, filepath.Join(runDir, "chatgpt-knowledge-export.json"), map[string]any{
		"schema_version": "compound.knowledge_export.v0.1",
		"source":         "conversation-export:fixture",
		"items": []map[string]any{{
			"ref":     "chatgpt:fixture-conversation",
			"title":   "Fixture Conversation Strategy",
			"content": "[assistant]\nThe useful source passage says Compound should show the actual memory candidate before asking for judgment.\n",
		}},
	})
	writeJSONFile(t, filepath.Join(runDir, "panel-run-single-prun_fixture.json"), map[string]any{
		"schema_version": "compound.panel_run.v0.1",
		"id":             "prun_fixture",
		"status":         "applied",
		"target_kind":    "memory_candidate",
		"targets": []map[string]any{{
			"ref":          "cand_reviewed_conversation",
			"privacy":      "private",
			"state_before": "proposed",
		}},
		"findings": []map[string]any{{
			"target_ref": "cand_reviewed_conversation",
			"lens_findings": []map[string]any{
				{
					"lens_ref": "lens:evidence-strength",
					"status":   "completed",
					"findings": []map[string]any{{
						"claim":    "candidate has evidence",
						"severity": "medium",
					}},
				},
				{
					"lens_ref": "lens:inversion-falsification",
					"status":   "failed",
					"findings": []map[string]any{{
						"claim":    "schema mismatch",
						"severity": "error",
					}},
				},
			},
			"synthesis": map[string]any{
				"decision":   "needs_human",
				"confidence": 0.75,
				"rationale":  "operator should inspect the claim before promotion",
			},
		}},
	})
	writeJSONFile(t, filepath.Join(runDir, "knowledge-extract-summary.json"), map[string]any{
		"total_created": 2,
		"candidate_ids": []string{"cand_reviewed_conversation", "cand_other_conversation"},
	})
	writeJSONFile(t, filepath.Join(runDir, "promoted-chatgpt-claims.json"), map[string]any{
		"count": 0,
		"refs":  []string{},
	})

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.RunArtifacts != 4 {
		t.Fatalf("run artifact count = %d, want 4; report=%+v", report.Counts.RunArtifacts, report)
	}
	if len(report.Open) != 1 || report.Open[0].WorkID != held.ID {
		t.Fatalf("open work = %+v", report.Open)
	}
	item := report.Open[0]
	if len(item.RunArtifacts) != 4 {
		t.Fatalf("run artifacts = %+v", item.RunArtifacts)
	}
	for _, artifact := range item.RunArtifacts {
		if strings.Contains(artifact.Ref, s.home) || strings.Contains(artifact.Ref, "/Users/") || strings.HasPrefix(artifact.Ref, "/") {
			t.Fatalf("run artifact leaked absolute/private path: %+v", artifact)
		}
	}
	panelArtifact, ok := runArtifactByKind(item.RunArtifacts, "panel_run")
	if !ok ||
		panelArtifact.Ref != filepath.ToSlash(filepath.Join("runs", held.ID, "panel-run-single-prun_fixture.json")) ||
		panelArtifact.Schema != "compound.panel_run.v0.1" ||
		panelArtifact.ID != "prun_fixture" ||
		panelArtifact.Status != "applied" ||
		panelArtifact.TargetKind != "memory_candidate" ||
		!contains(panelArtifact.TargetRefs, "cand_reviewed_conversation") {
		t.Fatalf("panel run artifact not projected with target refs/status: %+v", panelArtifact)
	}
	extractArtifact, ok := runArtifactByKind(item.RunArtifacts, "knowledge_extract_summary")
	if !ok || extractArtifact.Count == nil || *extractArtifact.Count != 2 || !contains(extractArtifact.CandidateIDs, "cand_other_conversation") {
		t.Fatalf("knowledge extract summary not projected with candidates/count: %+v", extractArtifact)
	}
	promotionArtifact, ok := runArtifactByKind(item.RunArtifacts, "promotion_summary")
	if !ok || promotionArtifact.Count == nil || *promotionArtifact.Count != 0 {
		t.Fatalf("promotion summary should preserve zero promoted claims: %+v", promotionArtifact)
	}
	if !contains(item.MemoryCandidateIDs, "cand_reviewed_conversation") || !contains(item.MemoryCandidateIDs, "cand_other_conversation") {
		t.Fatalf("run artifact candidate refs should feed workbench memory candidate refs: %+v", item.MemoryCandidateIDs)
	}
	if len(item.ReviewCards) != 1 {
		t.Fatalf("review cards = %+v", item.ReviewCards)
	}
	card := item.ReviewCards[0]
	if card.CandidateID != "cand_reviewed_conversation" ||
		card.CandidateState != "parked" ||
		card.CandidateEntity != "knowledge:chatgpt:fixture" ||
		card.CandidatePredicate != "proposes_strategy" ||
		card.CandidateObject != "Compound should show the actual memory candidate before asking for judgment." ||
		card.CandidateConfidence == nil ||
		*card.CandidateConfidence != 0.72 ||
		card.CandidateVisibility != "private" ||
		card.CandidateSourceRef != "knowledge_extraction:kx_fixture" ||
		card.CandidateItemRef != "chatgpt:fixture-conversation" ||
		card.CandidateExtraction != "kx_fixture" ||
		card.CandidateExtractedBy != "openai-compatible/test-model" ||
		card.CandidateSourceKind != "knowledge_extraction" ||
		card.CandidateSourceTitle != "Fixture Conversation Strategy" ||
		!strings.Contains(card.CandidateSourceExcerpt, "actual memory candidate before asking for judgment") ||
		card.PanelDecision != "needs_human" ||
		card.PanelConfidence == nil ||
		*card.PanelConfidence != 0.75 ||
		card.PanelFindingCount != 2 ||
		card.PanelRiskCount != 2 ||
		card.PanelFailedCount != 1 ||
		card.PanelRunID != "prun_fixture" ||
		card.PanelRunRef != filepath.ToSlash(filepath.Join("runs", held.ID, "panel-run-single-prun_fixture.json")) ||
		card.PromotionSummaryRef != filepath.ToSlash(filepath.Join("runs", held.ID, "promoted-chatgpt-claims.json")) ||
		card.PromotionCount == nil || *card.PromotionCount != 0 ||
		!contains(card.Command, "prun_fixture") ||
		!contains(card.StopConditions, "requested action would run import, extraction, panel run, mass import, or automatic promotion") {
		t.Fatalf("review card should expose exact parked candidate/panel/promotion boundary: %+v", card)
	}
	if !strings.Contains(item.NextAction, "cand_reviewed_conversation") {
		t.Fatalf("held work next action should name exact candidate, got %q", item.NextAction)
	}
	adjacent := report.DispatchNext.AdjacentProgress
	if adjacent == nil {
		t.Fatal("adjacent progress missing")
	}
	action, ok := adjacentActionByID(adjacent.NonPacketActions, card.ID)
	if !ok || action.Kind != "review_card" || !contains(action.Command, "prun_fixture") || !contains(action.ExpectedValidation, "promotion summary remains zero before explicit promotion") {
		t.Fatalf("adjacent progress should expose exact review card action: %+v", adjacent.NonPacketActions)
	}
}

func TestWorkbenchSurfacesASCOperatorGateCardForHeldWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Open) != 1 || report.Open[0].WorkID != held.ID {
		t.Fatalf("open work = %+v", report.Open)
	}
	item := report.Open[0]
	if len(item.ReviewCards) != 1 {
		t.Fatalf("review cards = %+v", item.ReviewCards)
	}
	card := item.ReviewCards[0]
	if card.Kind != "source_private_config_gate" ||
		card.GateID != "asc-private-config" ||
		card.SourceKind != "app_store_connect" ||
		!contains(card.Command, "tools/asc_preflight.ts") ||
		!contains(card.RequiredInputs, "private ASC product mapping config") ||
		!contains(card.ExpectedValidation, "no product signal apply and no scheduler load") ||
		!contains(card.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") ||
		!strings.Contains(card.Boundary, "read-only ASC preflight") {
		t.Fatalf("ASC operator gate card should expose exact read-only private-config valve: %+v", card)
	}
	if !strings.Contains(item.NextAction, "private ASC config") {
		t.Fatalf("held ASC next action should name private config gate: %q", item.NextAction)
	}
	adjacent := report.DispatchNext.AdjacentProgress
	if adjacent == nil {
		t.Fatal("adjacent progress missing")
	}
	action, ok := adjacentActionByID(adjacent.NonPacketActions, card.ID)
	if !ok || action.Kind != "review_card" || !contains(action.Command, "tools/asc_preflight.ts") || !contains(action.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") {
		t.Fatalf("adjacent progress should expose ASC operator gate as review card: %+v", adjacent.NonPacketActions)
	}
}

func TestWorkbenchMarksHeldCardConditionStaleWhenReceiptNamesGate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	closed := closeSyntheticEvidenceWork(t, s, []string{"operator_gate:asc-private-config", "asc_credential_smoke=ok"})

	report, err := s.Workbench(fixedNow.Add(5*time.Minute), WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, held.ID)
	if len(item.ReviewCards) != 1 {
		t.Fatalf("review cards = %+v", item.ReviewCards)
	}
	card := item.ReviewCards[0]
	if card.ConditionStatus != workHoldConditionStale ||
		card.TriggeringEvidenceRef != closed.Receipt.ID ||
		!contains(card.MatchedConditionRefs, "operator_gate:asc-private-config") ||
		!strings.Contains(card.NextAction, "condition stale") ||
		!strings.Contains(card.NextAction, "inspect normalized output") {
		t.Fatalf("stale held card missing evidence/next action: %+v receipt=%s", card, closed.Receipt.ID)
	}
	if item.State != "held" || item.NextAction != card.NextAction {
		t.Fatalf("held item should remain held with corrected next action: %+v", item)
	}
	action, ok := adjacentActionByID(report.DispatchNext.AdjacentProgress.NonPacketActions, card.ID)
	if !ok || action.NextAction != card.NextAction || action.ReviewCard == nil || action.ReviewCard.ConditionStatus != workHoldConditionStale {
		t.Fatalf("adjacent action missing stale review card fields: %+v", action)
	}
}

func TestWorkbenchASCOperatorGateUsesObservedSignalDecision(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkbenchProductSignals(t, s.home, []workbenchProductSignalRow{
		{
			ID:           "sig_asc_units",
			MetricName:   "app_store_connect_units",
			ObservedAt:   fixedNow.Add(time.Minute).Format(time.RFC3339),
			EvidenceRefs: []string{"asc:daily:sample-app:2026-07-04:units"},
		},
	})

	report, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, held.ID)
	if len(item.ReviewCards) != 1 {
		t.Fatalf("review cards = %+v", item.ReviewCards)
	}
	card := item.ReviewCards[0]
	if card.Kind != "source_refresh_gate" ||
		card.GateID != "asc-daily-refresh" ||
		card.ConditionStatus != "observed" ||
		!contains(card.Command, "connections") ||
		!strings.Contains(strings.Join(card.RequiredInputs, "\n"), "operator decision") ||
		!strings.Contains(strings.Join(card.ExpectedValidation, "\n"), "asc_daily_signals observed") ||
		!strings.Contains(card.NextAction, "ASC signals observed") ||
		strings.Contains(card.Title, "private ASC config needed") {
		t.Fatalf("ASC observed signal card should ask refresh/close decision: %+v", card)
	}
	if item.State != "held" || item.NextAction != card.NextAction {
		t.Fatalf("observed ASC signal must not auto-release held work: %+v", item)
	}
	action, ok := adjacentActionByID(report.DispatchNext.AdjacentProgress.NonPacketActions, card.ID)
	if !ok || action.Kind != "review_card" || action.ReviewCard == nil || action.ReviewCard.Kind != "source_refresh_gate" {
		t.Fatalf("adjacent action missing observed source card: %+v", action)
	}
}

func TestHeldConditionStaleEvidenceDoesNotAutoReleaseWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	closed := closeSyntheticEvidenceWork(t, s, []string{"operator_gate:asc-private-config"})
	if _, err := s.Workbench(fixedNow.Add(5*time.Minute), WorkbenchOptions{RecentLimit: 10}); err != nil {
		t.Fatal(err)
	}
	afterWorkbench, err := s.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterWorkbench.State != "held" {
		t.Fatalf("workbench evidence projection auto-released held work: %+v", afterWorkbench)
	}
	plan, err := s.AdvancePlan(held.ID, fixedNow.Add(6*time.Minute), AdvancePlanOptions{To: "ready", Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "not_advanceable" || plan.NoAction == "" || len(plan.AdvanceCommand) != 0 {
		t.Fatalf("non-operator held release plan should stay blocked: %+v", plan)
	}
	if _, err := s.Advance(held.ID, AdvanceInput{To: "ready", Actor: "codex", VerificationID: closed.Receipt.ID}, fixedNow.Add(7*time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("codex evidence-triggered release err = %v, want operator provenance rejection", err)
	}
	afterAdvance, err := s.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterAdvance.State != "held" {
		t.Fatalf("failed release attempt mutated held work: %+v", afterAdvance)
	}
}

func TestWorkbenchOldFormatHeldRecordProjectsCleanly(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	legacy := BuildWork(CreateInput{
		WorkID:           "work_legacyheld",
		Owner:            "compound-core",
		SuccessPredicate: "legacy held app-store-connect ASC private config valve",
		State:            "held",
	}, fixedNow)
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "evidence_refs")
	delete(raw, "non_goals")
	delete(raw, "run_ids")
	delete(raw, "artifact_ids")
	delete(raw, "verification_ids")
	delete(raw, "memory_candidate_ids")
	if err := s.writeJSON(s.workPath(legacy.ID), raw); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, legacy.ID)
	if item.State != "held" || len(item.ReviewCards) != 1 {
		t.Fatalf("legacy held record did not project cleanly: %+v", item)
	}
	card := item.ReviewCards[0]
	if card.ConditionStatus != "" || card.TriggeringEvidenceRef != "" || len(card.MatchedConditionRefs) != 0 {
		t.Fatalf("legacy held record should not get stale marker without evidence: %+v", card)
	}
}

func TestWorkbenchSurfacesTerminalActivePacketProof(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	writeContextPack(t, s, "ctx_packet")
	imported, err := s.ImportPacket(packetPath, PacketImportOptions{
		Owner:         "compound-core",
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, fixedNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := s.Claim(imported.Work.ID, "codex", fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet work: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "running"}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:active-packet-lifecycle"}, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := s.Close(imported.Work.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "active packet workbench proof complete",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:active-packet-lifecycle"},
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet work: %v", err)
	}

	report, err := s.Workbench(fixedNow.Add(5*time.Second), WorkbenchOptions{
		RecentLimit:         10,
		Actor:               "codex",
		ActivePacketSurface: imported.Work.CanonicalSurface,
	})
	if err != nil {
		t.Fatalf("workbench: %v", err)
	}
	if report.Counts.Open != 0 || report.Counts.Terminal != 1 {
		t.Fatalf("workbench counts = %+v", report.Counts)
	}
	if report.ActivePacketProof == nil {
		t.Fatal("workbench missing active packet proof")
	}
	if report.ActivePacketProof.Status != "ok" ||
		report.ActivePacketProof.WorkID != imported.Work.ID ||
		report.ActivePacketProof.ContextPackID != "ctx_packet" ||
		report.ActivePacketProof.ReceiptID != closed.Receipt.ID ||
		report.ActivePacketProof.TransitionID != closed.Transition.ID {
		t.Fatalf("active packet proof = %+v", report.ActivePacketProof)
	}
	next := report.DispatchNext
	if next.CanRun || next.WaitReason != "source_packet_terminal" || next.Reason != "active_packet_terminal" || next.WorkID != imported.Work.ID {
		t.Fatalf("dispatch next = %+v", next)
	}
	if !contains(next.BlockedOn, "work.terminal") || !contains(next.BlockedOn, "work.terminal:"+imported.Work.ID) {
		t.Fatalf("dispatch next blocked_on = %+v", next.BlockedOn)
	}
}

func TestWorkbenchDispatchNextWaitsForOtherActorClaim(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	created, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "claimed by other actor"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(created.ID, "claude", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if next.CanRun || next.WaitReason != "claimed_by_other_actor" || !contains(next.BlockedOn, "work.claimed_by_other_actor:"+claimed.ID) {
		t.Fatalf("other actor dispatch suggestion = %+v", next)
	}
	report, err = s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	next = report.DispatchNext
	if !next.CanRun || next.Action != "advance" || next.WorkID != claimed.ID {
		t.Fatalf("claiming actor dispatch suggestion = %+v", next)
	}
}

func TestWorkbenchDispatchNextPrefersProofReadyClose(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ready waits behind proof close"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	running := runningWork(t, s, "proof ready close first")
	verifying, err := s.Advance(running.ID, AdvanceInput{To: "verifying", Actor: "codex", VerificationID: "ver_dispatch_next"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if !next.CanRun || next.Action != "close" || next.Reason != "proof_ready_close" || next.WorkID != verifying.ID || next.WorkID == ready.ID {
		t.Fatalf("proof-ready dispatch suggestion = %+v", next)
	}
}

func TestWorkbenchDispatchNextSkipsProofReadyCloseHeldByOtherActor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ready work remains available to the requesting actor"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "proof-ready work remains with its lease holder"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(foreign.ID, "hermes:minimax-12h-controller", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(foreign.ID, AdvanceInput{To: "running", Actor: "hermes:minimax-12h-controller"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	verifying, err := s.Advance(foreign.ID, AdvanceInput{
		To:             "verifying",
		Actor:          "hermes:minimax-12h-controller",
		VerificationID: "ver_foreign_proof_ready",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "codex:root"})
	if err != nil {
		t.Fatal(err)
	}
	next := report.DispatchNext
	if !next.CanRun || next.Action != "claim" || next.WorkID != ready.ID || next.WorkID == verifying.ID {
		t.Fatalf("requesting actor dispatch suggestion = %+v", next)
	}

	report, err = s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10, Actor: "hermes:minimax-12h-controller"})
	if err != nil {
		t.Fatal(err)
	}
	next = report.DispatchNext
	if !next.CanRun || next.Action != "close" || next.WorkID != verifying.ID {
		t.Fatalf("lease holder dispatch suggestion = %+v", next)
	}
}

func TestWorkbenchDoesNotCountCancelledWorkAsMissingProof(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "cancelled work is terminal without completion proof"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Summary: "cancel test artifact"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Terminal != 1 || report.Counts.MissingReceipt != 0 || report.Counts.MissingTransition != 0 {
		t.Fatalf("cancelled work should not count as missing completion proof: %+v", report.Counts)
	}
	if len(report.Recent) != 1 || report.Recent[0].WorkID != w.ID || report.Recent[0].State != "cancelled" {
		t.Fatalf("recent cancelled work missing: %+v", report.Recent)
	}
	if report.Recent[0].ReceiptID != closed.Receipt.ID || report.Recent[0].TransitionID != closed.Transition.ID {
		t.Fatalf("cancelled terminal proof hidden: item=%+v close=%+v", report.Recent[0], closed)
	}
}

func TestWorkbenchDoesNotProjectCrossWorkTerminalProofComplete(t *testing.T) {
	t.Parallel()
	for _, artifact := range []string{"transition", "receipt"} {
		t.Run(artifact, func(t *testing.T) {
			s := newStore(t)
			w := runningWork(t, s, "terminal proof remains bound to one work")
			closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Summary: "close for mismatch test"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			switch artifact {
			case "transition":
				crossWork := closed.Transition
				crossWork.IDs.WorkID = "work_elsewhere"
				if err := s.replaceEvent(crossWork.ID, &crossWork); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				crossWork := closed.Receipt
				crossWork.IDs.WorkID = "work_elsewhere"
				if err := s.replaceEvent(crossWork.ID, &crossWork); err != nil {
					t.Fatal(err)
				}
			}

			report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Recent) != 1 || len(report.Integrity) == 0 {
				t.Fatalf("cross-work %s did not surface integrity risk: %+v", artifact, report)
			}
			item := report.Recent[0]
			if contains(item.ObservatoryTags, "proof:complete") || item.ReceiptID != "" {
				t.Fatalf("cross-work %s projected terminal proof complete: %+v", artifact, item)
			}
		})
	}
}

func TestWorkbenchDerivesObservatoryTags(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "ready work gets readable facets",
		EvidenceRefs:     []string{"packet_sha256:abc"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "completed memory work gets proof facets"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(done.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "verifying", VerificationID: "ver_memory"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(done.ID, CloseInput{
		Outcome:        "completed",
		MoveType:       "memory_delta",
		Summary:        "memory recipient proof",
		ProofRefs:      []string{"ver_memory"},
		MemoryPromoted: []string{"claim_memory"},
	}, fixedNow); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	readyItem := findWorkbenchItem(t, report.Open, ready.ID)
	for _, tag := range []string{"state:ready", "attention:needs-claim", "owner:compound-core", "evidence:source"} {
		if !contains(readyItem.ObservatoryTags, tag) {
			t.Fatalf("ready observatory tags missing %q: %+v", tag, readyItem.ObservatoryTags)
		}
	}
	doneItem := findWorkbenchItem(t, report.Recent, done.ID)
	for _, tag := range []string{"state:completed", "attention:terminal", "move:memory-delta", "recipient:memory", "change:changed", "evidence:verification", "proof:complete"} {
		if !contains(doneItem.ObservatoryTags, tag) {
			t.Fatalf("done observatory tags missing %q: %+v", tag, doneItem.ObservatoryTags)
		}
	}
}

func TestWorkbenchUsesEffectiveChangedForLegacyProofBearingReceipt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	done, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "legacy receipt projects changed"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(done.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(done.ID, AdvanceInput{To: "verifying", VerificationID: "ver_legacy"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(done.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "legacy receipt proof",
		ProofRefs: []string{"proof:legacy"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	legacy := closed.Receipt
	legacy.StateDelta.Changed = false
	if err := s.replaceEvent(legacy.ID, &legacy); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Recent, done.ID)
	if !item.Changed || !contains(item.ObservatoryTags, "change:changed") {
		t.Fatalf("legacy proof-bearing receipt projected unchanged: %+v", item)
	}
}

func adjacentActionByID(actions []WorkbenchAdjacentAction, id string) (WorkbenchAdjacentAction, bool) {
	for _, action := range actions {
		if action.ID == id {
			return action, true
		}
	}
	return WorkbenchAdjacentAction{}, false
}

func runArtifactByKind(artifacts []WorkbenchRunArtifact, kind string) (WorkbenchRunArtifact, bool) {
	for _, artifact := range artifacts {
		if artifact.Kind == kind {
			return artifact, true
		}
	}
	return WorkbenchRunArtifact{}, false
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func closeWorkbenchTestWork(t *testing.T, s *Store, workID string, at time.Time, summary string) {
	t.Helper()
	if _, err := s.Claim(workID, "codex", at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(workID, AdvanceInput{To: "running"}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(workID, AdvanceInput{To: "verifying", VerificationID: "ver_" + workID}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   summary,
		Changed:   true,
		ProofRefs: []string{"proof:" + workID},
	}, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkbenchSurfacesAdmissionPreviewsWithoutAdmittingWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_product_action_preview",
		Owner:            "product:demo",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "reviewed product action becomes explicit work later",
		DesiredOutcome:   "operator can admit reviewed action without rerunning product review",
		EvidenceRefs:     []string{"product_action_review:reviewed_demo"},
	}, fixedNow, CreatePlanOptions{
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "reviewed_demo",
		Record:        true,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		ActionContext: &WorkActionContext{
			Kind:          "product_action_review",
			Title:         "Follow reviewed product next action",
			ActionSummary: "Run local product L1 review; hold public/external changes.",
			Command:       []string{"compound", "product", "action-plan", "--product", "product:demo", "--card", "demo-card"},
			Proof:         "future receipt or product-loop projection cites the reviewed action",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlanPath == "" {
		t.Fatalf("recorded create-plan missing plan path: %+v", plan)
	}
	if _, err := s.Get(plan.WorkID); err == nil {
		t.Fatalf("create-plan should not admit work %s", plan.WorkID)
	}
	events, err := s.eventsFor(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("create-plan should not write work lifecycle events: %+v", events)
	}

	report, err := s.Workbench(fixedNow.Add(time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.AdmissionPreviews != 1 || len(report.AdmissionPreviews) != 1 {
		t.Fatalf("admission previews not surfaced: counts=%+v previews=%+v", report.Counts, report.AdmissionPreviews)
	}
	preview := report.AdmissionPreviews[0]
	if preview.ActionSummary != "Run local product L1 review; hold public/external changes." {
		t.Fatalf("admission preview should project action_summary from action context: %+v", preview)
	}
	if preview.ActionContext == nil || preview.ActionContext.ActionSummary != preview.ActionSummary {
		t.Fatalf("admission preview action context should preserve projected action_summary: %+v", preview)
	}
	if preview.PlanID != plan.ID ||
		preview.WorkID != plan.WorkID ||
		preview.WorkOwner != "product:demo" ||
		preview.WorkState != "proposed" ||
		preview.Staleness != "fresh" ||
		preview.AgeSeconds != 60 ||
		preview.Age != "1m" ||
		preview.NextAction == "" ||
		!sameStringSlice(preview.AdmitCommand, []string{"compound", "work", "admit-plan", "--plan", plan.PlanPath}) ||
		preview.PolicyOutcome == "" ||
		!preview.PolicyAllowed ||
		preview.PlanPath == "" ||
		len(preview.Command) == 0 ||
		!contains(preview.EvidenceRefs, "product_action_review:reviewed_demo") {
		t.Fatalf("preview did not preserve admission plan fields: %+v", preview)
	}
	if report.DispatchNext.CanRun {
		t.Fatalf("admission preview must not become dispatchable work: %+v", report.DispatchNext)
	}

	if _, err := s.Create(CreateInput{
		WorkID:           plan.WorkID,
		Owner:            plan.Work.Owner,
		Actor:            plan.Actor,
		State:            plan.Work.State,
		SuccessPredicate: plan.Work.SuccessPredicate,
		DesiredOutcome:   plan.Work.DesiredOutcome,
		EvidenceRefs:     plan.Work.EvidenceRefs,
	}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := s.Workbench(fixedNow.Add(3*time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if after.Counts.AdmissionPreviews != 0 || len(after.AdmissionPreviews) != 0 {
		t.Fatalf("admitted work should retire admission preview: counts=%+v previews=%+v", after.Counts, after.AdmissionPreviews)
	}
}

func TestWorkbenchMarksOldAdmissionPreviewsStale(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_stale_admission_preview",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "old proposal does not look fresh forever",
		EvidenceRefs:     []string{"test://stale-admission-preview"},
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(25*time.Hour), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.AdmissionPreviews != 1 || len(report.AdmissionPreviews) != 1 {
		t.Fatalf("admission preview missing: counts=%+v previews=%+v", report.Counts, report.AdmissionPreviews)
	}
	preview := report.AdmissionPreviews[0]
	planRel := filepath.ToSlash(filepath.Join("create-plans", filepath.Base(plan.PlanPath)))
	if preview.Staleness != "stale" ||
		preview.StaleReason == "" ||
		preview.AgeSeconds != int64((25*time.Hour).Seconds()) ||
		preview.Age != "1d" ||
		!strings.Contains(preview.NextAction, "revalidate") ||
		!strings.Contains(preview.NextAction, "retire") ||
		!sameStringSlice(preview.RetireCommand, []string{"compound", "home", "archive-history", "--kind", "create-plan", "--path", planRel}) ||
		!sameStringSlice(preview.RetireDryRun, []string{"compound", "home", "archive-history", "--kind", "create-plan", "--path", planRel, "--dry-run"}) ||
		!sameStringSlice(preview.AdmitCommand, []string{"compound", "work", "admit-plan", "--plan", plan.PlanPath}) {
		t.Fatalf("stale admission preview not classified with safe next action: %+v", preview)
	}
}

func TestWorkbenchBlocksLegacyAdmissionPreviewUntilReplanned(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_legacy_admission_preview",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "legacy preview stays visible but cannot admit",
	}, fixedNow, CreatePlanOptions{Record: true})
	if err != nil {
		t.Fatal(err)
	}
	legacy := plan
	legacy.SchemaVersion = LegacyCreatePlanSchemaVersion
	if err := s.writeJSON(plan.PlanPath, &legacy); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.AdmissionPreviews) != 1 {
		t.Fatalf("legacy preview should remain visible as debt: %+v", report.AdmissionPreviews)
	}
	preview := report.AdmissionPreviews[0]
	if preview.PlanSchemaVersion != LegacyCreatePlanSchemaVersion ||
		preview.Staleness != "legacy" ||
		!strings.Contains(preview.StaleReason, "not content-bound") ||
		!strings.Contains(preview.NextAction, "fresh content-bound plan") ||
		!strings.Contains(preview.AdmissionBlockedReason, "requires re-plan") ||
		len(preview.AdmitCommand) != 0 ||
		len(preview.RetireCommand) == 0 {
		t.Fatalf("legacy preview must be blocked with re-plan/retire actions: %+v", preview)
	}
	if _, err := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(2*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), CreatePlanSchemaVersion) {
		t.Fatalf("legacy preview admission must be rejected, got %v", err)
	}
}

func writeWorkbenchRouteOutcomeReview(t *testing.T, homePath string, review map[string]any) {
	t.Helper()
	dir := filepath.Join(homePath, "reviews", "route-remorse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id, _ := review["id"].(string)
	if id == "" {
		t.Fatal("route outcome review id required")
	}
	data, err := json.MarshalIndent(review, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWorkbenchProductSignals(t *testing.T, homePath string, rows []workbenchProductSignalRow) {
	t.Helper()
	dir := filepath.Join(homePath, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "product-signals.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

func closeSyntheticEvidenceWork(t *testing.T, s *Store, proofRefs []string) CloseResult {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "synthetic evidence work"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", VerificationID: "ver_synthetic_evidence"}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "synthetic held condition evidence",
		Changed:   true,
		ProofRefs: proofRefs,
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return closed
}

func findWorkbenchItem(t *testing.T, items []WorkbenchItem, workID string) WorkbenchItem {
	t.Helper()
	for _, item := range items {
		if item.WorkID == workID {
			return item
		}
	}
	t.Fatalf("missing workbench item %s in %+v", workID, items)
	return WorkbenchItem{}
}

func assertWorkbenchItem(t *testing.T, items []WorkbenchItem, workID, reason, next string) {
	t.Helper()
	for _, item := range items {
		if item.WorkID == workID {
			if item.OpenReason != reason || item.NextAction != next {
				t.Fatalf("item %s = %+v", workID, item)
			}
			return
		}
	}
	t.Fatalf("missing workbench item %s in %+v", workID, items)
}
