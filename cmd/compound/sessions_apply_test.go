package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

func TestSessionsMineJobRunRebuildsMalformedCheckpoint(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	ev := sessionMiningTestEvent("hook_rebuild", "sess_rebuild", "work_rebuild", "SessionEnd", "lane:rebuild")
	ev.CreatedAt = now.Add(-time.Hour).Format(time.RFC3339)
	writeSessionHookEvents(t, homePath, []sessionHookEvent{ev})
	path := sessionMiningCheckpointPath(homePath, sessionMiningJobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := buildSessionMiningJobReport(homePath, "24h", "codex", sessionMiningJobID, now)
	if err != nil {
		t.Fatalf("malformed cache should rebuild, got: %v", err)
	}
	if report.HookEventCount != 1 || report.HookEventScanMode != "full" {
		t.Fatalf("rebuild report = %+v", report)
	}
	if !stringSliceContains(report.NextCommand, "--job-run") {
		t.Fatalf("job next command lost job mode: %+v", report.NextCommand)
	}
}

func TestSessionsMineJobRunResumesAndPreservesCrossRunPatterns(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	first := sessionMiningTestEvent("hook_job_first", "sess_job_first", "work_job_first", "UserPromptSubmit", "operator_correction:prefer reversible edits")
	first.CreatedAt = now.Add(-time.Hour).Format(time.RFC3339)
	writeSessionHookEvents(t, homePath, []sessionHookEvent{first})
	// Close the migration gate so ScanFrom can issue a stable segment cursor.
	migrationDir := filepath.Join(homePath, "events", "migration")
	if err := os.MkdirAll(migrationDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrationDir, "completion.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	initial, err := buildSessionMiningJobReport(homePath, "24h", "codex", sessionMiningJobID, now)
	if err != nil {
		t.Fatal(err)
	}
	if initial.HookEventScanMode != "full" || initial.HookEventsScanned != 1 || initial.HookEventCount != 1 {
		t.Fatalf("initial job report = mode:%s scanned:%d events:%d", initial.HookEventScanMode, initial.HookEventsScanned, initial.HookEventCount)
	}
	if len(initial.PreferencesFound) != 0 {
		t.Fatalf("one occurrence unexpectedly formed a repeated preference: %+v", initial.PreferencesFound)
	}

	second := sessionMiningTestEvent("hook_job_second", "sess_job_second", "work_job_second", "SessionEnd", "operator_correction:prefer reversible edits")
	second.CreatedAt = now.Add(30 * time.Minute).Format(time.RFC3339)
	writeSessionHookEvents(t, homePath, []sessionHookEvent{second})
	resumed, err := buildSessionMiningJobReport(homePath, "24h", "codex", sessionMiningJobID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.HookEventScanMode != "incremental" || resumed.HookEventsScanned != 1 || resumed.HookEventCount != 2 {
		t.Fatalf("resumed job report = mode:%s scanned:%d events:%d, want incremental/1/2", resumed.HookEventScanMode, resumed.HookEventsScanned, resumed.HookEventCount)
	}
	if len(resumed.PreferencesFound) != 1 {
		t.Fatalf("rolling aggregate lost cross-run repeated preference: %+v", resumed.PreferencesFound)
	}
	checkpoint, ok, err := loadSessionMiningCheckpoint(homePath, sessionMiningJobID, "24h", "codex")
	if err != nil || !ok || len(checkpoint.Events) != 2 || checkpoint.Cursor == "" {
		t.Fatalf("checkpoint = ok:%t err:%v value:%+v", ok, err, checkpoint)
	}
	receipt, ok, err := loadLatestSessionMiningReceipt(homePath, sessionMiningJobID)
	if err != nil || !ok {
		t.Fatalf("latest run receipt = ok:%t err:%v", ok, err)
	}
	if receipt.Status != "succeeded" || receipt.ScanMode != "incremental" || receipt.EventsScanned != 1 || receipt.EventsRetained != 2 || receipt.SessionsSeen != 2 {
		t.Fatalf("latest receipt = %+v", receipt)
	}
	files, err := sessionMiningReceiptFiles(homePath, sessionMiningJobID)
	if err != nil || len(files) != 2 {
		t.Fatalf("immutable run receipts = %v err=%v, want 2", files, err)
	}
}

func TestSessionsMineFiltersHarnessAndCountsDistinctSessions(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}

	codexPrompt := sessionMiningTestEvent("hook_codex_prompt", "sess_codex", "work_codex", "UserPromptSubmit", "operator_correction:prefer reversible edits")
	codexEnd := sessionMiningTestEvent("hook_codex_end", "sess_codex", "work_codex", "SessionEnd", "lane:local")
	claude := sessionMiningTestEvent("hook_claude", "sess_claude", "work_claude", "SessionEnd", "lane:local")
	claude.Harness = "claude_code"
	old := sessionMiningTestEvent("hook_old", "sess_old", "work_old", "SessionEnd", "lane:old")
	for _, ev := range []*sessionHookEvent{&codexPrompt, &codexEnd, &claude} {
		ev.CreatedAt = now.Add(-time.Hour).Format(time.RFC3339)
	}
	old.CreatedAt = now.Add(-48 * time.Hour).Format(time.RFC3339)
	writeSessionHookEvents(t, homePath, []sessionHookEvent{codexPrompt, codexEnd, claude, old})

	report, err := buildSessionMiningReport(homePath, "24h", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Harness != "codex" || report.SessionCount != 1 || report.HookEventCount != 2 {
		t.Fatalf("filtered report = %+v, want harness=codex sessions=1 hook_events=2", report)
	}
	if report.HookEventsScanned != 4 || report.HookEventScanMode != "full" {
		t.Fatalf("scan observability = scanned:%d mode:%q, want 4/full", report.HookEventsScanned, report.HookEventScanMode)
	}
	if len(report.HarnessCounts) != 1 || report.HarnessCounts["codex"] != 2 {
		t.Fatalf("harness counts leaked another harness: %+v", report.HarnessCounts)
	}
	if report.HookCounts["UserPromptSubmit"] != 1 || report.HookCounts["SessionEnd"] != 1 {
		t.Fatalf("hook counts = %+v, want one codex event of each kind", report.HookCounts)
	}
}

func TestSessionsMineWithoutHarnessCountsDistinctSessionsAcrossHarnesses(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	one := sessionMiningTestEvent("hook_one", "sess_shared", "work_one", "UserPromptSubmit", "lane:one")
	two := sessionMiningTestEvent("hook_two", "sess_shared", "work_one", "SessionEnd", "lane:two")
	three := sessionMiningTestEvent("hook_three", "sess_other", "work_two", "SessionEnd", "lane:three")
	three.Harness = "claude_code"
	for _, ev := range []*sessionHookEvent{&one, &two, &three} {
		ev.CreatedAt = now.Add(-time.Hour).Format(time.RFC3339)
	}
	writeSessionHookEvents(t, homePath, []sessionHookEvent{one, two, three})

	report, err := buildSessionMiningReport(homePath, "24h", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionCount != 2 || report.HookEventCount != 3 {
		t.Fatalf("report counts = sessions:%d hook_events:%d, want 2/3", report.SessionCount, report.HookEventCount)
	}
}

func TestSessionsMineApplyRoutesSyntheticFindingsProposeOnly(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	writeSessionMiningFixture(t, homePath)

	out := captureStdout(t, func() {
		cmdSessions([]string{"mine", "--since", "48h", "--apply", "--json"})
	})
	var report sessionMiningReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse sessions mine apply report: %v\n%s", err, out)
	}
	if report.Apply == nil || report.Apply.Status != "review_required" || report.DryRun {
		t.Fatalf("apply report = %+v", report.Apply)
	}
	if len(report.PreferencesFound) == 0 || len(report.RulesToRetire) == 0 || len(report.CandidateSkillUpdates) == 0 {
		t.Fatalf("expected preference, retire, and skill update findings: prefs=%d retire=%d skill=%d",
			len(report.PreferencesFound), len(report.RulesToRetire), len(report.CandidateSkillUpdates))
	}
	assertSessionFindingsHaveEvidenceRefs(t, report.PreferencesFound)
	assertSessionFindingsHaveEvidenceRefs(t, report.RulesToRetire)
	assertSessionFindingsHaveEvidenceRefs(t, report.CandidateSkillUpdates)
	if len(report.Apply.PreferenceCandidateIDs) == 0 || len(report.Apply.RetireProposalIDs) == 0 || len(report.Apply.SkillUpdateProposalIDs) == 0 {
		t.Fatalf("apply routes missing: %+v", report.Apply)
	}
	assertSessionApplyRoutesAllFindings(t, report)

	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	memoryCandidates, err := memStore.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(memoryCandidates) == 0 {
		t.Fatal("expected preference memory candidate")
	}
	if memoryCandidates[0].ClaimType != "preference" || memoryCandidates[0].Visibility != "contextual" || memoryCandidates[0].PromotionBasis != "" {
		t.Fatalf("preference candidate must be contextual and non-promotable by default: %+v", memoryCandidates[0])
	}
	if memoryCandidates[0].ProvenanceLane != "deterministic" {
		t.Fatalf("preference candidate provenance_lane = %q, want deterministic", memoryCandidates[0].ProvenanceLane)
	}

	retirePath := filepath.Join(homePath, "reviews", "distiller-improvements", report.Apply.RetireProposalIDs[0]+".json")
	var retirePlan struct {
		Status       string   `json:"status"`
		CandidateID  string   `json:"candidate_id"`
		EvidenceRefs []string `json:"evidence_refs"`
		Warnings     []string `json:"warnings"`
	}
	readJSONFile(t, retirePath, &retirePlan)
	if retirePlan.Status != "review_required" || retirePlan.CandidateID == "" || len(retirePlan.EvidenceRefs) == 0 {
		t.Fatalf("retire plan must be propose-only with evidence refs: %+v", retirePlan)
	}
	if !stringSliceContains(retirePlan.Warnings, "does not mutate distiller policy") {
		t.Fatalf("retire plan missing no-mutation warning: %+v", retirePlan.Warnings)
	}

	skillPath := filepath.Join(homePath, "reviews", capgraph.CapabilityLifecycleProposalReviewArea, report.Apply.SkillUpdateProposalIDs[0]+".json")
	var skillProposal struct {
		Status       string   `json:"status"`
		ProposalKind string   `json:"proposal_kind"`
		EvidenceRefs []string `json:"evidence_refs"`
		Warnings     []string `json:"warnings"`
	}
	readJSONFile(t, skillPath, &skillProposal)
	if skillProposal.Status != "review_required" || skillProposal.ProposalKind != "revise" || len(skillProposal.EvidenceRefs) == 0 {
		t.Fatalf("skill proposal must be a revise review artifact with evidence refs: %+v", skillProposal)
	}
	if !stringSliceContains(skillProposal.Warnings, "does not mutate CapabilityGraph") {
		t.Fatalf("skill proposal missing no-mutation warning: %+v", skillProposal.Warnings)
	}
}

func TestSessionsMineApplyReusesSemanticReviewArtifactsAcrossOverlappingWindows(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	first := sessionMiningReport{
		PreferencesFound: []sessionMiningFinding{{
			Kind:         "repeated_tool_choice",
			SemanticKey:  "repeated_tool_choice:terminal",
			Summary:      `Operator repeatedly chose tool "terminal" across 20 session evidence refs.`,
			EvidenceRefs: []string{"hook:first", "session:first"},
			Confidence:   0.8,
		}},
		RulesToRetire: []sessionMiningFinding{{
			Kind:         "review_friction",
			SemanticKey:  "review_friction:held-work",
			Summary:      "Review-held work exists; retire review-only friction.",
			EvidenceRefs: []string{"work:first"},
			Confidence:   0.3,
		}},
		CandidateSkillUpdates: []sessionMiningFinding{{
			Kind:         "session_pattern_review",
			SemanticKey:  "session_pattern_review:codex",
			Summary:      "Review 20 recent codex hook events for repeated tool gaps or reusable skill updates.",
			EvidenceRefs: []string{"hook:first", "session:first"},
			Confidence:   0.35,
		}},
	}
	firstApply, err := applySessionMiningReport(homePath, &first, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	second := first
	second.PreferencesFound = []sessionMiningFinding{{
		Kind:         "repeated_tool_choice",
		SemanticKey:  "repeated_tool_choice:terminal",
		Summary:      `Operator repeatedly chose tool "terminal" across 21 session evidence refs.`,
		EvidenceRefs: []string{"hook:first", "session:first", "hook:second", "session:second"},
		Confidence:   0.82,
	}}
	second.RulesToRetire = []sessionMiningFinding{{
		Kind:         "review_friction",
		SemanticKey:  "review_friction:held-work",
		Summary:      "Review-held work still exists; retire review-only friction.",
		EvidenceRefs: []string{"work:first", "work:second", "check:first", "check:second"},
		Confidence:   0.3,
	}}
	second.CandidateSkillUpdates = []sessionMiningFinding{{
		Kind:         "session_pattern_review",
		SemanticKey:  "session_pattern_review:codex",
		Summary:      "Review 21 recent codex hook events for repeated tool gaps or reusable skill updates.",
		EvidenceRefs: []string{"hook:first", "session:first", "hook:second", "session:second"},
		Confidence:   0.35,
	}}
	secondApply, err := applySessionMiningReport(homePath, &second, cliTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if got, want := secondApply.PreferenceCandidateIDs, firstApply.PreferenceCandidateIDs; !slices.Equal(got, want) {
		t.Fatalf("overlapping preference window minted duplicate ids: first=%v second=%v", want, got)
	}
	if got, want := secondApply.RetireProposalIDs, firstApply.RetireProposalIDs; !slices.Equal(got, want) {
		t.Fatalf("overlapping rule-retirement window minted duplicate ids: first=%v second=%v", want, got)
	}
	if got, want := secondApply.SkillUpdateProposalIDs, firstApply.SkillUpdateProposalIDs; !slices.Equal(got, want) {
		t.Fatalf("overlapping skill window minted duplicate ids: first=%v second=%v", want, got)
	}
	for _, artifact := range secondApply.RoutedArtifacts {
		if len(artifact.EvidenceRefs) != 4 {
			t.Fatalf("reused route lost current evidence refs: %+v", artifact)
		}
	}
}

func TestSessionsMineApplyDoesNotMutateWithoutOperatorReview(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	writeSessionMiningFixture(t, homePath)

	_ = captureStdout(t, func() {
		cmdSessions([]string{"mine", "--since", "48h", "--apply", "--json"})
	})

	for _, path := range []string{
		filepath.Join(homePath, "ledger", "claims.jsonl"),
		filepath.Join(homePath, "ledger", "adjudications.jsonl"),
		filepath.Join(homePath, "registries", "capability-graph.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("sessions mine --apply must not mutate %s without review, stat err=%v", path, err)
		}
	}
	assertNoReviewDecisionArtifacts(t, homePath)
	assertCandidateInboxStates(t, filepath.Join(homePath, candidates.InboxDir(candidates.Memory)), "proposed")
	assertCandidateInboxStates(t, filepath.Join(homePath, candidates.InboxDir(candidates.PolicyPatch)), candidates.NoPromote)
}

func TestSessionsMineApplyRejectsFindingsWithoutEvidenceRefs(t *testing.T) {
	tests := []struct {
		name   string
		report sessionMiningReport
	}{
		{
			name: "preference",
			report: sessionMiningReport{
				PreferencesFound: []sessionMiningFinding{{
					Kind:       "operator_correction",
					Summary:    "prefer reversible edits",
					Confidence: 0.8,
				}},
			},
		},
		{
			name: "rule_retire",
			report: sessionMiningReport{
				RulesToRetire: []sessionMiningFinding{{
					Kind:       "review_friction",
					Summary:    "retire stale review-only rule",
					Confidence: 0.3,
				}},
			},
		},
		{
			name: "skill_update",
			report: sessionMiningReport{
				CandidateSkillUpdates: []sessionMiningFinding{{
					Kind:       "session_pattern_review",
					Summary:    "revise skill for repeated gap",
					Confidence: 0.4,
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatalf("init home: %v", err)
			}
			_, err := applySessionMiningReport(homePath, &tt.report, cliTestNow)
			if err == nil || !strings.Contains(err.Error(), "missing session evidence refs") {
				t.Fatalf("apply without evidence refs err = %v, want missing evidence failure", err)
			}
			assertNoSessionApplyArtifacts(t, homePath)
		})
	}
}

func TestSessionsMineDailyDryRunDigestJobRosterFixture(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	source := "../../fixtures/authority/job-roster-sessions-mine-digest.json"
	now := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	report, err := authority.ImportJobRoster(homePath, source, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.JobCount != 1 || report.EnabledCount != 0 {
		t.Fatalf("job roster report = %+v", report)
	}
	roster, err := authority.LoadJobRosterAuthority(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Jobs) != 1 {
		t.Fatalf("jobs = %+v", roster.Jobs)
	}
	job := roster.Jobs[0]
	if job.JobID != "sessions-mine-daily-dry-run" || job.State != "pending_operator_scheduling" || job.Enabled {
		t.Fatalf("sessions mine digest job should be registered but unloaded: %+v", job)
	}
	if !strings.Contains(job.Script, "sessions mine --since 24h --dry-run --job-run sessions-mine-daily-dry-run --json") {
		t.Fatalf("unexpected job script: %+v", job)
	}
}

func writeSessionMiningFixture(t *testing.T, homePath string) {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID:           "work_review_rule",
		Owner:            "codex",
		State:            "held",
		SuccessPredicate: "review stale decision-card-only rule",
		DesiredOutcome:   "operator review decides whether the rule changes future execution",
		EvidenceRefs:     []string{"session:sess_rule", "hook:hook_rule_review"},
	}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	events := []sessionHookEvent{
		sessionMiningTestEvent("hook_pref_correction_1", "sess_pref_1", "work_pref_1", "UserPromptSubmit", "operator_correction:prefer reversible edits before wider rewrites"),
		sessionMiningTestEvent("hook_pref_correction_2", "sess_pref_2", "work_pref_2", "SessionEnd", "operator_correction:prefer reversible edits before wider rewrites"),
		sessionMiningTestEvent("hook_override_1", "sess_override_1", "work_override_1", "PreToolUse", "manual_override:skip auto-install suggestion"),
		sessionMiningTestEvent("hook_override_2", "sess_override_2", "work_override_2", "PreToolUse", "manual_override:skip auto-install suggestion"),
		sessionMiningTestEvent("hook_tool_1", "sess_tool_1", "work_tool_1", "PostToolUse", "tool:rg"),
		sessionMiningTestEvent("hook_tool_2", "sess_tool_2", "work_tool_2", "PostToolUse", "tool:rg"),
		sessionMiningTestEvent("hook_lane_1", "sess_lane_1", "work_lane_1", "SessionEnd", "lane:local-dry-run"),
		sessionMiningTestEvent("hook_lane_2", "sess_lane_2", "work_lane_2", "SessionEnd", "lane:local-dry-run"),
	}
	writeSessionHookEvents(t, homePath, events)
}

func sessionMiningTestEvent(id, sessionID, workID, hookName, sideEffect string) sessionHookEvent {
	var ev sessionHookEvent
	ev.ID = id
	ev.CreatedAt = time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	ev.Harness = "codex"
	ev.HookName = hookName
	ev.HookCapability = "session_mining_fixture"
	ev.Status = "ok"
	ev.SideEffects = []string{sideEffect}
	ev.EvidenceRefs = []string{"hook:" + id, "session:" + sessionID}
	ev.IDs.SessionID = sessionID
	ev.IDs.WorkID = workID
	return ev
}

func writeSessionHookEvents(t *testing.T, homePath string, events []sessionHookEvent) {
	t.Helper()
	// Seed authoritative hook_ records: session mining now reads them through the
	// eventstore Scan seam, not the frozen hook-events.ndjson stream.
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if _, err := store.PutJSON(ev.ID, ev, "  "); err != nil {
			t.Fatalf("seed hook event %s: %v", ev.ID, err)
		}
	}
}

func assertSessionFindingsHaveEvidenceRefs(t *testing.T, findings []sessionMiningFinding) {
	t.Helper()
	for _, finding := range findings {
		if len(finding.EvidenceRefs) == 0 {
			t.Fatalf("finding missing evidence refs: %+v", finding)
		}
	}
}

func assertSessionApplyRoutesAllFindings(t *testing.T, report sessionMiningReport) {
	t.Helper()
	if report.Apply == nil {
		t.Fatal("apply report missing")
	}
	wantPreference := len(report.PreferencesFound)
	wantRetire := len(report.RulesToRetire)
	wantSkill := len(report.CandidateSkillUpdates)
	if len(report.Apply.PreferenceCandidateIDs) != wantPreference {
		t.Fatalf("preference routes = %d, want %d", len(report.Apply.PreferenceCandidateIDs), wantPreference)
	}
	if len(report.Apply.RetireProposalIDs) != wantRetire {
		t.Fatalf("retire proposal routes = %d, want %d", len(report.Apply.RetireProposalIDs), wantRetire)
	}
	if len(report.Apply.SkillUpdateProposalIDs) != wantSkill {
		t.Fatalf("skill update routes = %d, want %d", len(report.Apply.SkillUpdateProposalIDs), wantSkill)
	}
	routeCounts := map[string]int{}
	routedIDs := map[string]bool{}
	for _, artifact := range report.Apply.RoutedArtifacts {
		if artifact.ID == "" || len(artifact.EvidenceRefs) == 0 {
			t.Fatalf("routed artifact must carry id and evidence refs: %+v", artifact)
		}
		routeCounts[artifact.Route]++
		routedIDs[artifact.ID] = true
	}
	if routeCounts["memory_candidate"] != wantPreference ||
		routeCounts["distiller_improvement_plan"] != wantRetire ||
		routeCounts["capability_lifecycle_proposal"] != wantSkill {
		t.Fatalf("routed artifact counts = %+v, want prefs=%d retire=%d skill=%d", routeCounts, wantPreference, wantRetire, wantSkill)
	}
	for _, id := range append(append([]string{}, report.Apply.PreferenceCandidateIDs...), append(report.Apply.RetireProposalIDs, report.Apply.SkillUpdateProposalIDs...)...) {
		if !routedIDs[id] {
			t.Fatalf("route id %q missing from routed_artifacts: %+v", id, report.Apply.RoutedArtifacts)
		}
	}
}

func assertNoReviewDecisionArtifacts(t *testing.T, homePath string) {
	t.Helper()
	root := filepath.Join(homePath, "reviews")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := filepath.Base(path)
		if strings.HasSuffix(name, ".reviewed.json") || strings.HasSuffix(name, ".decision.json") || strings.HasSuffix(name, ".apply.json") {
			t.Fatalf("unexpected reviewed/mutating artifact without operator review: %s", path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func assertNoSessionApplyArtifacts(t *testing.T, homePath string) {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(homePath, candidates.InboxDir(candidates.Memory)),
		filepath.Join(homePath, candidates.InboxDir(candidates.PolicyPatch)),
		filepath.Join(homePath, "reviews", "distiller-improvements"),
		filepath.Join(homePath, "reviews", capgraph.CapabilityLifecycleProposalReviewArea),
	} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			t.Fatalf("unexpected apply artifact without evidence refs: %s", filepath.Join(dir, entry.Name()))
		}
	}
}

func assertCandidateInboxStates(t *testing.T, dir, wantState string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected candidates in %s", dir)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var payload struct {
			State   string `json:"state"`
			Verdict string `json:"verdict"`
		}
		readJSONFile(t, filepath.Join(dir, entry.Name()), &payload)
		got := firstNonEmpty(payload.State, payload.Verdict)
		if got != wantState {
			t.Fatalf("candidate %s state/verdict = %q, want %q", entry.Name(), got, wantState)
		}
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data := readTestPath(t, path)
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func TestSessionsMineJobStatusReconcilesDeclarationReceiptAndLiveness(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	status, err := buildSessionMiningJobStatus(homePath, sessionMiningJobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "not_declared" || status.Declared {
		t.Fatalf("undeclared job status = %+v", status)
	}

	source := "../../fixtures/authority/job-roster-sessions-mine-digest.json"
	if _, err := authority.ImportJobRoster(homePath, source, now); err != nil {
		t.Fatal(err)
	}
	status, err = buildSessionMiningJobStatus(homePath, sessionMiningJobID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Declared || status.Status == "not_declared" {
		t.Fatalf("declared job status = %+v", status)
	}
	if status.SchedulerObserved {
		t.Fatal("declaration without probe must not report scheduler observed")
	}

	if _, err := authority.RefreshJobRosterFromObservations(homePath, "test-probe", []authority.JobRosterObservation{
		{JobID: sessionMiningJobID, Alive: true, Source: "test"},
	}, now); err != nil {
		t.Fatal(err)
	}
	status, err = buildSessionMiningJobStatus(homePath, sessionMiningJobID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.SchedulerObserved || status.SchedulerState != "alive" {
		t.Fatalf("probed-alive job status = %+v", status)
	}
}

func TestSessionsMineJobStatusSurfaceShowsDryRunUsage(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "projections", "sessions-mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "projections", "sessions-mine", sessionMiningJobID+".json"), []byte(`{
  "schema_version": "`+sessionMiningCheckpointSchemaVersion+`",
  "job_id": "`+sessionMiningJobID+`",
  "since": "24h",
  "cursor": "ZmFrZS1ieXRlLWN1cnNvcg",
  "updated_at": "`+now.Format(time.RFC3339)+`",
  "events": [{"id":"h1","created_at":"`+now.Add(-time.Hour).Format(time.RFC3339)+`","harness":"codex","hook_name":"UserPromptSubmit","status":"ok","side_effects":[],"evidence_refs":[],"ids":{"session_id":"s1","work_id":"w1"}}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdSessions([]string{"job", "status"})
	})
	if !strings.Contains(out, "incremental_ready=true") {
		t.Fatalf("status text missing incremental readiness: %s", out)
	}
	if !strings.Contains(out, "checkpoint: present=true events=1") {
		t.Fatalf("status text missing checkpoint detail: %s", out)
	}
}

func TestSessionsJobSchedulerPlanGeneratesLaunchAgentPlist(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.TargetPath, "LaunchAgents") || !strings.Contains(plan.TargetContent, "<?xml") {
		t.Fatalf("expected LaunchAgent plist plan, got target=%s content:\n%s", plan.TargetPath, plan.TargetContent)
	}
	if plan.OS != "darwin" {
		t.Skipf("scheduler plan OS = %s, plist semantics expect darwin", plan.OS)
	}
	if !strings.Contains(plan.Command, sessionMiningJobID) {
		t.Fatalf("plan command must reference job id, got: %s", plan.Command)
	}
	if plan.TargetHash == "" {
		t.Fatal("plan target hash required")
	}
	targetDir, err := launchAgentDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.TargetPath, targetDir) {
		t.Fatalf("target path %s not under %s", plan.TargetPath, targetDir)
	}
}

func TestSessionsJobSchedulerApplyRejectsUnapprovedPlan(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "projections", "sessions-mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "projections", "sessions-mine", sessionMiningJobID+".json"), []byte(`{
  "schema_version": "`+sessionMiningCheckpointSchemaVersion+`",
  "job_id": "`+sessionMiningJobID+`",
  "since": "24h",
  "cursor": "ZmFrZS1ieXRlLWN1cnNvcg",
  "updated_at": "`+now.Format(time.RFC3339)+`",
  "events": [{"id":"h1","created_at":"`+now.Add(-time.Hour).Format(time.RFC3339)+`","harness":"codex","hook_name":"UserPromptSubmit","status":"ok","side_effects":[],"evidence_refs":[],"ids":{"session_id":"s1","work_id":"w1"}}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := applySessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err == nil || !strings.Contains(err.Error(), "no approved scheduler plan") {
		t.Fatalf("apply without approved plan err = %v, want 'no approved scheduler plan'", err)
	}
}

func TestSessionsJobSchedulerFullLifecycle(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err != nil {
		t.Fatal(err)
	}
	plan.Status = "review_required"
	markdown := renderSessionSchedulerPlanMarkdown(plan)
	paths, err := reviewartifact.Write(homePath, sessionSchedulerReviewArea, plan.ID, plan, "  ", markdown)
	if err != nil {
		t.Fatal(err)
	}
	yamlPath, err := reviewartifact.WriteRaw(homePath, sessionSchedulerReviewArea, plan.ID+".review.yaml", []byte(renderSessionSchedulerReviewYAML(plan.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "projections", "sessions-mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "projections", "sessions-mine", sessionMiningJobID+".json"), []byte(`{
  "schema_version": "`+sessionMiningCheckpointSchemaVersion+`",
  "job_id": "`+sessionMiningJobID+`",
  "since": "24h",
  "cursor": "ZmFrZS1ieXRlLWN1cnNvcg",
  "updated_at": "`+now.Format(time.RFC3339)+`",
  "events": []
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	recorded, err := recordSessionSchedulerReview(homePath, yamlPath, sessionSchedulerReview{
		Decision:  "approved",
		Rationale: "plan matches expected LaunchAgent shape",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if recorded["status"] != "approved" {
		t.Fatalf("review status = %v", recorded)
	}

	result, err := applySessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "applied" {
		t.Fatalf("apply result = %v", result)
	}
	if _, err := os.Stat(result["target_path"].(string)); os.IsNotExist(err) {
		t.Fatalf("plist not written to target: %s", result["target_path"])
	}
	if _, ok := result["apply_path"]; !ok {
		t.Fatal("missing apply_path in result")
	}
	applyData, err := os.ReadFile(result["apply_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var applyRecord map[string]any
	if err := json.Unmarshal(applyData, &applyRecord); err != nil {
		t.Fatal(err)
	}
	if applyRecord["plan_id"] != plan.ID || applyRecord["target_hash"] != plan.TargetHash {
		t.Fatalf("apply record = %+v, want plan_id=%s target_hash=%s", applyRecord, plan.ID, plan.TargetHash)
	}
	_ = paths
}

func TestSessionsJobSchedulerRejectsParkedPlan(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err != nil {
		t.Fatal(err)
	}
	plan.Status = "review_required"
	_, err = reviewartifact.Write(homePath, sessionSchedulerReviewArea, plan.ID, plan, "  ", renderSessionSchedulerPlanMarkdown(plan))
	if err != nil {
		t.Fatal(err)
	}
	yamlPath, err := reviewartifact.WriteRaw(homePath, sessionSchedulerReviewArea, plan.ID+".review.yaml", []byte(renderSessionSchedulerReviewYAML(plan.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "projections", "sessions-mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "projections", "sessions-mine", sessionMiningJobID+".json"), []byte(`{"schema_version":"`+sessionMiningCheckpointSchemaVersion+`","job_id":"`+sessionMiningJobID+`","since":"24h","cursor":"x","updated_at":"`+now.Format(time.RFC3339)+`","events":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recordSessionSchedulerReview(homePath, yamlPath, sessionSchedulerReview{
		Decision:  "parked",
		Rationale: "need to verify scheduler environment first",
	}, now); err != nil {
		t.Fatal(err)
	}
	_, err = applySessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err == nil || !strings.Contains(err.Error(), "no approved scheduler plan") {
		t.Fatalf("apply with only a parked review err = %v, want 'no approved scheduler plan'", err)
	}
}
