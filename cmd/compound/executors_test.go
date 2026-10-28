package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

func TestExecutorsCLIProjectsWorkbenchAndLeases(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	codexWork, err := store.Create(work.CreateInput{
		Owner:            "codex",
		Actor:            "operator-test",
		SuccessPredicate: "Codex edits code with proof",
		DesiredOutcome:   "Projection row shows Codex as running",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create codex work: %v", err)
	}
	if _, err := store.Claim(codexWork.ID, "codex", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim codex work: %v", err)
	}
	writeExecutorHealthFixtures(t, homePath, codexWork.ID)
	readyWork, err := store.Create(work.CreateInput{
		Owner:            "operator",
		Actor:            "operator-test",
		SuccessPredicate: "Ready work needs executor choice",
		DesiredOutcome:   "Projection row shows unassigned ready work",
	}, cliTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdExecutors([]string{"--json"})
	})
	var projection executorsJSONFixture
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatalf("parse executors projection: %v\n%s", err, out)
	}
	if projection.SchemaVersion != work.ExecutorsProjectionSchemaVersion ||
		projection.AuthorityRef != work.ExecutorsAuthorityRef ||
		projection.Status != "ok" {
		t.Fatalf("unexpected projection header: %+v", projection)
	}
	if !containsString(projection.SourceRefs, "compound.workbench") ||
		!containsString(projection.SourceRefs, "compound.work.leases") ||
		!containsString(projection.SourceRefs, "compound.executor.profile_guide") ||
		!containsString(projection.SourceRefs, "compound.events.hook_events") ||
		!containsString(projection.SourceRefs, "compound.ledger.route_evidence") {
		t.Fatalf("projection missing source refs: %+v", projection.SourceRefs)
	}
	codex := requireExecutorRow(t, projection, "codex")
	if codex.StatusSource != "profile_guide" || codex.RunningCount != 1 || codex.LeaseCount != 1 || len(codex.CurrentWork) != 1 {
		t.Fatalf("codex row should be profile-guided with claimed work and lease: %+v", codex)
	}
	if codex.ReadyForAnyCount != 1 || len(codex.ReadyForAny) != 1 || codex.ReadyForAny[0].WorkID != readyWork.ID {
		t.Fatalf("codex row should expose unclaimed ready work as ready for any executor: %+v", codex)
	}
	if codex.WaitReason != "actor_claimed_work" {
		t.Fatalf("codex wait_reason = %q, want actor_claimed_work", codex.WaitReason)
	}
	if codex.LastHookEvent == nil ||
		codex.LastHookEvent.CreatedAt != cliTestNow.Add(3*time.Minute).UTC().Format(time.RFC3339) ||
		codex.LastHookEvent.Hook != "SessionStart" ||
		codex.LastHookEvent.Status != "ok" {
		t.Fatalf("codex last hook event mismatch: %+v", codex.LastHookEvent)
	}
	if len(codex.Leases) != 1 || codex.Leases[0].AgeSeconds <= 0 {
		t.Fatalf("codex lease should expose positive age: %+v", codex.Leases)
	}
	if codex.OldestLeaseAgeSeconds == nil || *codex.OldestLeaseAgeSeconds != codex.Leases[0].AgeSeconds {
		t.Fatalf("codex oldest lease age mismatch: oldest=%v leases=%+v", codex.OldestLeaseAgeSeconds, codex.Leases)
	}
	if codex.CurrentWork[0].WorkID != codexWork.ID || codex.CurrentWork[0].State != "claimed" || codex.CurrentWork[0].Title == "" {
		t.Fatalf("codex current work mismatch: %+v", codex.CurrentWork)
	}
	switchback := requireExecutorRow(t, projection, "switchback_api")
	if !switchback.RouteEvidencePresent || switchback.RouteEvidenceCount != 1 {
		t.Fatalf("switchback route evidence mismatch: %+v", switchback)
	}
	unassigned := requireExecutorRow(t, projection, "unassigned")
	if unassigned.StatusSource != "workbench" || unassigned.WaitReason != "" || unassigned.ReadyCount != 0 || unassigned.NextAssigned != nil || executorRefsContain(unassigned.CurrentWork, readyWork.ID) {
		t.Fatalf("unassigned row should not retain ready work available to executors: %+v", unassigned)
	}
	for _, row := range projection.Executors {
		if row.ID == "unassigned" {
			continue
		}
		if row.ReadyForAnyCount != 1 || len(row.ReadyForAny) != 1 || row.ReadyForAny[0].WorkID != readyWork.ID {
			t.Fatalf("executor %q should expose the ready-for-any work item: %+v", row.ID, row)
		}
	}
	if projection.Counts.ProfileGuide != 6 || projection.Counts.Running != 1 || projection.Counts.Ready != 1 || projection.Counts.Assigned != 1 || projection.Counts.Leases != 1 {
		t.Fatalf("unexpected projection counts: %+v", projection.Counts)
	}
}

func TestExecutorsCLIKeepsLeasedWorkExclusiveToLeaseHolder(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	leasedWork, err := store.Create(work.CreateInput{
		Owner:            "operator",
		Actor:            "operator-test",
		SuccessPredicate: "Leased work stays with its executor",
		DesiredOutcome:   "Projection does not advertise leased work as available",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create leased work: %v", err)
	}
	if _, err := store.Claim(leasedWork.ID, "codex", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim leased work: %v", err)
	}

	projection := runExecutorsJSONFixture(t)
	codex := requireExecutorRow(t, projection, "codex")
	if codex.LeaseCount != 1 || !executorRefsContain(codex.CurrentWork, leasedWork.ID) {
		t.Fatalf("lease holder should retain leased work: %+v", codex)
	}
	currentWorkOccurrences := 0
	for _, row := range projection.Executors {
		if executorRefsContain(row.CurrentWork, leasedWork.ID) {
			currentWorkOccurrences++
			if row.ID != "codex" {
				t.Fatalf("leased work appeared on non-holder row %q: %+v", row.ID, row)
			}
		}
		if row.ReadyForAnyCount != 0 || executorRefsContain(row.ReadyForAny, leasedWork.ID) {
			t.Fatalf("leased work must not be advertised as ready for any executor on %q: %+v", row.ID, row)
		}
	}
	if currentWorkOccurrences != 1 {
		t.Fatalf("leased work appeared %d times in current_work, want exactly once", currentWorkOccurrences)
	}
}

func TestExecutorsCLIProjectionDoesNotMutateHome(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		Owner:            "operator",
		Actor:            "operator-test",
		SuccessPredicate: "Ready projection remains read-only",
		DesiredOutcome:   "Executor availability does not mutate work",
	}, cliTestNow); err != nil {
		t.Fatalf("create ready work: %v", err)
	}

	before := hashHomeDir(t, homePath)
	_ = runExecutorsJSONFixture(t)
	after := hashHomeDir(t, homePath)
	if before != after {
		t.Fatalf("executors projection mutated temp home: before=%s after=%s", before, after)
	}
}

func writeExecutorHealthFixtures(t *testing.T, homePath, workID string) {
	t.Helper()
	eventsDir := filepath.Join(homePath, "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	// Seed the authoritative hook_ record (the executors projection now reads via
	// the eventstore Scan seam, not the frozen hook-events.ndjson stream).
	hookEvent := map[string]any{
		"id":         "hook_executor_health_fixture",
		"created_at": cliTestNow.Add(3 * time.Minute).UTC().Format(time.RFC3339),
		"harness":    "codex",
		"hook_name":  "SessionStart",
		"status":     "ok",
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	if _, err := store.PutJSON("hook_executor_health_fixture", hookEvent, "  "); err != nil {
		t.Fatalf("seed hook event record: %v", err)
	}

	ledgerDir := filepath.Join(homePath, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatalf("mkdir ledger: %v", err)
	}
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     cliTestNow.Add(4 * time.Minute),
		RequestID:     "req_executor_health_fixture",
		WorkID:        workID,
		ClientProfile: "switchback",
		Protocol:      "openai-chat-completions",
		Provider:      "switchback",
		Model:         "auto/judge",
		Status:        "completed",
	})
	if err != nil {
		t.Fatalf("build route evidence: %v", err)
	}
	routeLine, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal route evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ledgerDir, "execution-route-evidence.jsonl"), append(routeLine, '\n'), 0o644); err != nil {
		t.Fatalf("write route evidence: %v", err)
	}
}

type executorsJSONFixture struct {
	SchemaVersion string                   `json:"schema_version"`
	AuthorityRef  string                   `json:"authority_ref"`
	Status        string                   `json:"status"`
	Counts        work.ExecutorCounts      `json:"counts"`
	Executors     []executorJSONFixtureRow `json:"executors"`
	SourceRefs    []string                 `json:"source_refs"`
}

type executorJSONFixtureRow struct {
	work.ExecutorRow
	ReadyForAnyCount int                    `json:"ready_for_any_count"`
	ReadyForAny      []work.ExecutorWorkRef `json:"ready_for_any"`
}

func runExecutorsJSONFixture(t *testing.T) executorsJSONFixture {
	t.Helper()
	out := captureStdout(t, func() {
		cmdExecutors([]string{"--json"})
	})
	var projection executorsJSONFixture
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatalf("parse executors projection: %v\n%s", err, out)
	}
	return projection
}

func executorRefsContain(refs []work.ExecutorWorkRef, workID string) bool {
	for _, ref := range refs {
		if ref.WorkID == workID {
			return true
		}
	}
	return false
}

func requireExecutorRow(t *testing.T, projection executorsJSONFixture, id string) executorJSONFixtureRow {
	t.Helper()
	for _, row := range projection.Executors {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing executor row %q in %+v", id, projection.Executors)
	return executorJSONFixtureRow{}
}
