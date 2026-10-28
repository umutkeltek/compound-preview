package sourceobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/harness"
	"compound/internal/projection"
	"compound/internal/reviewartifact"
	"compound/internal/tooltopology"
)

var tSource = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func TestApplyGeneratedViewObservationWritesCandidateAndNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := validObservation()

	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}

	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Kind != string(candidates.CapabilityObs) || c.Verdict != candidates.NoPromote {
		t.Fatalf("candidate kind/verdict = %q/%q", c.Kind, c.Verdict)
	}
	if c.SourceObservationID != obs.ID || c.PromoteAllowed {
		t.Fatalf("candidate source/promote = %q/%v", c.SourceObservationID, c.PromoteAllowed)
	}
	if !hasString(c.EvidenceRefs, "source:fixtures/generated/skill-manifest.json#sha256:abc123") {
		t.Fatalf("candidate evidence refs = %v", c.EvidenceRefs)
	}

	nopRows := readNoPromoteRows(t, home)
	if len(nopRows) != 1 {
		t.Fatalf("no-promote rows = %d, want 1", len(nopRows))
	}
	if nopRows[0].ID != report.NoPromoteIDs[0] || nopRows[0].CandidateRef != c.ID {
		t.Fatalf("no-promote row = %+v", nopRows[0])
	}
	if nopRows[0].OwnerTarget.Kind != "source_observation" || nopRows[0].OwnerTarget.Ref != obs.ID {
		t.Fatalf("owner target = %+v", nopRows[0].OwnerTarget)
	}

	replay, err := Apply(home, obs, tSource.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replay.NoPromoteIDs[0] != report.NoPromoteIDs[0] {
		t.Fatalf("replay no-promote id = %q, want %q", replay.NoPromoteIDs[0], report.NoPromoteIDs[0])
	}
	if rows := readNoPromoteRows(t, home); len(rows) != 1 {
		t.Fatalf("replay appended duplicate no-promote rows: %d", len(rows))
	}
}

func TestSourceObservationVerdictPlanRecordsNoPromoteEvidence(t *testing.T) {
	home := t.TempDir()
	obs := validObservation()
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CandidateIDs) != 1 {
		t.Fatalf("report candidate ids = %+v", report.CandidateIDs)
	}
	var candidate CandidateRecord
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, report.CandidateIDs[0], &candidate); err != nil {
		t.Fatal(err)
	}
	verdictPath := writeSourceObservationVerdictFixture(t, home, "panel_verdict_source_fixture", candidate)
	plan, err := BuildSourceObservationVerdictPlan(home, verdictPath, "", tSource.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != SourceObservationVerdictPlanSchemaVersion || plan.Status != "review_required" || plan.OwnerKind != "source_observation" || plan.OwnerRef != obs.ID || plan.OwnerStatus != "sourceobs" {
		t.Fatalf("bad source observation verdict plan: %+v", plan)
	}
	if plan.TargetRef != candidate.ID || plan.PanelDecision != "park" {
		t.Fatalf("plan target/decision = %s/%s", plan.TargetRef, plan.PanelDecision)
	}
	if !hasString(plan.EvidenceRefs, "panel_verdict:"+plan.VerdictID) || !hasString(plan.EvidenceRefs, "capability_observation:"+candidate.ID) {
		t.Fatalf("plan missing verdict/candidate evidence refs: %+v", plan.EvidenceRefs)
	}
	if !hasString(plan.Boundaries, "does not promote memory or source truth") || !hasString(plan.Boundaries, "does not mutate ProductSignal ledger") {
		t.Fatalf("plan should carry hard boundaries: %+v", plan.Boundaries)
	}
	paths, err := RecordSourceObservationVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected review artifact %s: %v", path, err)
		}
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceObservationVerdictReviewArea}, tSource.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateObservationVerdictQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("source observation verdict queue should be actionable: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
	if queue.Items[0].DecisionCard == nil || queue.Items[0].Actions[0].ID != "record_source_observation_verdict_review" {
		t.Fatalf("source observation verdict item should expose record action: %+v", queue.Items[0])
	}
	review := SourceObservationVerdictReview{
		SchemaVersion: SourceObservationVerdictReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		VerdictID:     plan.VerdictID,
		CandidateRef:  plan.TargetRef,
		Decision:      "record_no_promote",
		Rationale:     "reviewed generated source verdict remains evidence, not source truth",
		Confidence:    0.82,
		EvidenceRefs:  []string{"manual:source-observation-verdict-review"},
	}
	reviewReport, err := ApplySourceObservationVerdictReview(home, review, paths.ReviewYAMLPath, tSource.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "recorded" || reviewReport.NoPromoteID == "" || reviewReport.OwnerKind != "source_observation" || reviewReport.OwnerRef != obs.ID {
		t.Fatalf("bad source observation verdict review report: %+v", reviewReport)
	}
	rows := readNoPromoteRows(t, home)
	var found bool
	for _, row := range rows {
		if row.ID == reviewReport.NoPromoteID && row.CandidateRef == plan.TargetRef && row.OwnerTarget.Kind == "source_observation" && row.OwnerTarget.Ref == obs.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("review no-promote row not found in %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation verdict review must not promote claims: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation verdict review must not mutate product signals: %v", err)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceObservationVerdictReviewArea}, tSource.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateObservationVerdictQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "no_action" {
		t.Fatalf("reviewed queue should be terminal: counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
}

func TestSourceObservationVerdictPlanParksUnsupportedNoOwnerVerdict(t *testing.T) {
	home := t.TempDir()
	payload := sourceObservationVerdictCandidatePayload{
		ID:           "ocand_unsupported_fixture",
		Kind:         string(candidates.CapabilityObs),
		Subject:      "observation:workflow-health",
		Statement:    "Synthetic workflow health verdict has no source owner yet.",
		Detail:       "subject_kind=workflow observation_kind=system",
		Confidence:   0.61,
		EvidenceRefs: []string{"fixture:unsupported-observation"},
	}
	verdictPath := writeSourceObservationVerdictPayloadFixture(t, home, "panel_verdict_unsupported_fixture", payload)
	plan, err := BuildSourceObservationVerdictPlan(home, verdictPath, "", tSource)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OwnerStatus != "unsupported_no_owner" || plan.OwnerKind != "observation_verdict" || plan.OwnerRef != payload.ID {
		t.Fatalf("unsupported owner status = %+v", plan)
	}
	paths, err := RecordSourceObservationVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := SourceObservationVerdictReview{
		SchemaVersion: SourceObservationVerdictReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		VerdictID:     plan.VerdictID,
		CandidateRef:  plan.TargetRef,
		Decision:      "park",
		Rationale:     "no source owner exists yet, keep reviewed verdict parked as no-promote evidence",
		Confidence:    0.63,
		EvidenceRefs:  []string{"manual:unsupported-source-verdict"},
	}
	report, err := ApplySourceObservationVerdictReview(home, review, paths.ReviewYAMLPath, tSource.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.NoPromoteID == "" || report.OwnerKind != "observation_verdict" || report.OwnerRef != payload.ID {
		t.Fatalf("unsupported review report = %+v", report)
	}
}

func TestProjectionFreshnessObservationWritesMethodUpgradeNoPromote(t *testing.T) {
	home := t.TempDir()
	freshness := projection.Freshness{
		ProjectionID:    "projection:test-dashboard",
		ViewRef:         "generated-dashboard.md",
		SourceLedgerSeq: 8,
		BuiltAt:         "2026-06-29T00:00:00Z",
		CoverageVerdict: "partial",
		Status:          projection.StatusStale,
		OutputHash:      "sha256:view",
		Staleness:       "241h0m0s",
		Sources:         []projection.SourceObservation{{SourceRef: "source-ledger.jsonl", CurrentHash: "sha256:source", Status: "ok"}},
		Checks:          []projection.Check{{Name: "coverage", Status: "fail", Detail: "partial"}},
	}

	obs := FromProjectionFreshness(freshness, tSource)
	if obs.AdapterID != "compound.projection" || obs.GeneratedView.Freshness != projection.StatusStale {
		t.Fatalf("unexpected projection observation: %+v", obs)
	}
	if len(obs.MapsTo) != 1 || obs.MapsTo[0].CandidateKind != string(candidates.MethodUpgrade) || obs.MapsTo[0].PromoteAllowed {
		t.Fatalf("unexpected map target: %+v", obs.MapsTo)
	}
	if !strings.Contains(obs.GeneratedView.DriftReason, "coverage:partial") {
		t.Fatalf("missing drift reason: %q", obs.GeneratedView.DriftReason)
	}

	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.MethodUpgrade), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Kind != string(candidates.MethodUpgrade) || c.Verdict != candidates.NoPromote || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection source observation should not promote claims, stat err=%v", err)
	}
}

func TestToolTopologyObservationWritesCapabilityNoPromote(t *testing.T) {
	home := t.TempDir()
	topo := tooltopology.Topology{
		SchemaVersion: tooltopology.SchemaVersion,
		ID:            "topo_fixture_resolver",
		AdapterID:     "fixture.tool-topology",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: tooltopology.SourceRef{
			Path:        "fixtures/generated/tool-resolver.json",
			Kind:        "generated_view",
			Owner:       "fixture-spine",
			SourceClass: "os-tool-resolver-json",
			Revision:    "sha256:def456",
		},
		Entries: []tooltopology.Entry{
			{
				ID:           "mcp:context7",
				Kind:         "mcp",
				Name:         "context7",
				Interface:    "mcp",
				Status:       "on_demand",
				Triggers:     []string{"docs"},
				EvidenceRefs: []string{"tool_resolver_json:sha256:def456"},
			},
		},
		EvidenceRefs: []string{"tool_resolver_json:sha256:def456"},
	}
	obs := FromToolTopology(topo, tSource)
	if obs.GeneratedView.ViewKind != "tool_topology" || obs.MapsTo[0].CandidateKind != string(candidates.CapabilityObs) {
		t.Fatalf("observation = %+v", obs)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != topo.ID || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("tool topology source observation should not promote claims, stat err=%v", err)
	}
}

func TestSkillManifestObservationWritesCapabilityNoPromote(t *testing.T) {
	home := t.TempDir()
	manifest := capgraph.SkillManifest{
		SchemaVersion: capgraph.SkillManifestSchemaVersion,
		ID:            "skill_manifest_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: capgraph.SkillSourceRef{
			Path:        "fixtures/skills/skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_fixture",
		},
		Skills: []capgraph.SkillManifestEntry{
			{
				ID:           "skill:pricing",
				Name:         "Pricing",
				Status:       "active",
				Triggers:     []string{"pricing"},
				EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
			},
		},
		EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
	}
	obs := FromSkillManifest(manifest, tSource)
	if obs.GeneratedView.ViewKind != "skill_manifest" || obs.MapsTo[0].CandidateKind != string(candidates.CapabilityObs) {
		t.Fatalf("observation = %+v", obs)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != manifest.ID || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill manifest source observation should not promote claims, stat err=%v", err)
	}
}

func TestConversationMiningObservationWritesMethodUpgradeNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := FromConversationMining(ConversationMiningObservation{
		ID:                "conversation_mining_fixture",
		ReportPath:        "fixtures/conversation-mining/report.json",
		MinedTagsPath:     "fixtures/conversation-mining/tags.jsonl",
		LossinessPath:     "fixtures/conversation-mining/lossiness.json",
		Revision:          "sha256:mining",
		Processed:         2,
		Accepted:          2,
		Rejected:          0,
		MinedRows:         2,
		AcceptedForReview: 1,
		DreamPackFiles:    2,
		DreamExecution:    "gated",
		LossinessRecorded: true,
		LossinessDetail:   "claude_ratio=0.044 codex_ratio=0.012 known_rules=true",
		Note:              "Dream execution remains separate gated.",
		EvidenceRefs:      []string{"conversation_mining_report:sha256:abc"},
	}, tSource)
	if obs.GeneratedView.ViewKind != "conversation_mining" || obs.GeneratedView.Consumer != "compound distiller import-conversation-mining" {
		t.Fatalf("observation generated view = %+v", obs.GeneratedView)
	}
	if len(obs.MapsTo) != 1 || obs.MapsTo[0].CandidateKind != string(candidates.MethodUpgrade) || obs.MapsTo[0].PromoteAllowed {
		t.Fatalf("maps_to = %+v", obs.MapsTo)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.MethodUpgrade), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != "conversation_mining_fixture" || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conversation mining source observation should not promote claims, stat err=%v", err)
	}
}

func TestSessionLensObservationWritesMethodUpgradeNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := FromSessionLenses(SessionLensObservation{
		ID:             "session_lenses_fixture",
		SourcePath:     "fixtures/session-distill/multi-lens.json",
		SourceRevision: "sha256:lenses",
		Decisions:      1,
		Outcomes:       1,
		Patterns:       2,
		Skills:         1,
		Taste:          1,
		Contradictions: 1,
		TotalItems:     7,
		EvidenceRefs:   []string{"session_lenses:sha256:lenses"},
	}, tSource)
	if obs.GeneratedView.ViewKind != "session_lenses" || obs.GeneratedView.Consumer != "compound distiller import-session-lenses" {
		t.Fatalf("observation generated view = %+v", obs.GeneratedView)
	}
	if len(obs.MapsTo) != 1 || obs.MapsTo[0].CandidateKind != string(candidates.MethodUpgrade) || obs.MapsTo[0].PromoteAllowed {
		t.Fatalf("maps_to = %+v", obs.MapsTo)
	}
	if !strings.Contains(obs.GeneratedView.DriftReason, "decisions=1 outcomes=1 patterns=2 skills=1 taste=1 contradictions=1 total_items=7") {
		t.Fatalf("drift reason = %q", obs.GeneratedView.DriftReason)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.MethodUpgrade), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Kind != string(candidates.MethodUpgrade) || c.Verdict != candidates.NoPromote || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("session lens source observation should not promote claims, stat err=%v", err)
	}
}

func TestConversationCoverageObservationWritesMethodUpgradeNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := FromConversationCoverage(ConversationCoverageObservation{
		ID:                "conversation_coverage_fixture",
		ReportPath:        "fixtures/conversation-coverage/summary.json",
		Revision:          "sha256:coverage",
		SourceCount:       2,
		TotalRows:         15,
		RawFiles:          15,
		MappedClean:       12,
		UnmappedRaw:       3,
		Stable:            15,
		DreamCandidates:   2,
		MissingEmbeddings: 1,
		AvgRetainedRatio:  0.095,
		TopTagCount:       1,
		ReviewDebt:        []string{"claude_unmapped_raw=3", "codex_missing_embeddings=1"},
		EvidenceRefs:      []string{"conversation_coverage_report:sha256:abc"},
	}, tSource)
	if obs.GeneratedView.ViewKind != "conversation_coverage" || obs.GeneratedView.Consumer != "compound distiller import-conversation-coverage" {
		t.Fatalf("observation generated view = %+v", obs.GeneratedView)
	}
	if len(obs.MapsTo) != 1 || obs.MapsTo[0].CandidateKind != string(candidates.MethodUpgrade) || obs.MapsTo[0].PromoteAllowed {
		t.Fatalf("maps_to = %+v", obs.MapsTo)
	}
	if !strings.Contains(obs.GeneratedView.DriftReason, "review_debt=claude_unmapped_raw=3,codex_missing_embeddings=1") {
		t.Fatalf("drift reason = %q", obs.GeneratedView.DriftReason)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.MethodUpgrade), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != "conversation_coverage_fixture" || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conversation coverage source observation should not promote claims, stat err=%v", err)
	}
}

func TestCapabilityEvaluationObservationWritesCapabilityNoPromote(t *testing.T) {
	home := t.TempDir()
	set := capgraph.CapabilityEvaluationSet{
		SchemaVersion: capgraph.CapabilityEvaluationSetSchemaVersion,
		ID:            "capability_evaluations_fixture",
		AdapterID:     "os.capability-evaluations",
		SourceSystem:  "capability-evaluations",
		SourcePath:    "state/capability-evaluations",
		SourceKind:    "state_records_dir",
		Revision:      "sha256:evaluations",
		EvidenceRefs:  []string{"capability_evaluation:known"},
		Evaluations: []capgraph.CapabilityEvaluation{
			{
				SchemaVersion:  1,
				ID:             "known",
				CreatedAt:      "2026-06-24T05:49:48Z",
				SourceReceipt:  "receipt:known",
				Capability:     "web-design-guidelines",
				Verdict:        "adapt",
				LocalStateOnly: true,
			},
		},
	}
	obs := FromCapabilityEvaluations(set, tSource)
	if obs.GeneratedView.ViewKind != "capability_evaluations" || obs.GeneratedView.Consumer != "compound capability import-evaluations" {
		t.Fatalf("unexpected capability evaluation observation: %+v", obs)
	}
	if obs.SourceRef.SourceClass != "capability_evaluation" || len(obs.EvidenceRefs) != 1 {
		t.Fatalf("source ref/evidence = %+v refs=%v", obs.SourceRef, obs.EvidenceRefs)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != set.ID || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
}

func TestHarnessRegistryObservationWritesCapabilityNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := FromHarnessRegistry(harness.DefaultRegistry(), tSource)
	if obs.GeneratedView.ViewKind != "harness_registry" || obs.MapsTo[0].CandidateKind != string(candidates.CapabilityObs) {
		t.Fatalf("observation = %+v", obs)
	}
	if obs.SourceRef.Kind != "registry" || obs.SourceRef.Revision == "" || len(obs.EvidenceRefs) == 0 {
		t.Fatalf("source ref/evidence = %+v refs=%v", obs.SourceRef, obs.EvidenceRefs)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != "harness_registry:default" || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("harness registry source observation should not promote claims, stat err=%v", err)
	}
}

func TestHarnessRegistryManifestObservationWritesCapabilityNoPromote(t *testing.T) {
	home := t.TempDir()
	manifest, err := harness.LoadRegistryManifest(filepath.Join("..", "..", "fixtures", "harness", "harness-registry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	obs, err := FromHarnessRegistryManifest(manifest, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if obs.ID == "" || obs.AdapterID != "fixture.harness-registry" || obs.SourceSystem != "fixture-os" {
		t.Fatalf("observation identity = %+v", obs)
	}
	if obs.GeneratedView.ViewKind != "harness_registry" || obs.GeneratedView.Marker != harness.RegistryManifestSchemaVersion {
		t.Fatalf("generated view = %+v", obs.GeneratedView)
	}
	if got := obs.MapsTo[0].EntityHint; got != "harness_registry:fixture-harness-registry" {
		t.Fatalf("entity hint = %q", got)
	}
	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs), report.CandidateIDs[0]+".json")
	data, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var c CandidateRecord
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Verdict != candidates.NoPromote || c.Subject != "harness_registry:fixture-harness-registry" || c.SourceObservationID != obs.ID {
		t.Fatalf("candidate = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("harness manifest source observation should not promote claims, stat err=%v", err)
	}
}

func TestValidateRejectsUnknownCandidateKind(t *testing.T) {
	obs := validObservation()
	obs.MapsTo[0].CandidateKind = "../../bad"
	report := Validate(obs)
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail", report.Status)
	}
}

func TestPromoteAllowedCandidateParksWithoutNoPromote(t *testing.T) {
	home := t.TempDir()
	obs := validObservation()
	obs.MapsTo[0].PromoteAllowed = true

	report, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.NoPromoteIDs) != 0 {
		t.Fatalf("promote-allowed map should not no-promote: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("unexpected no-promote ledger, err=%v", err)
	}
}

func validObservation() SourceObservation {
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            "srcobs_fixture_skill_manifest",
		AdapterID:     "fixture.generated-views",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: SourceRef{
			Path:        "fixtures/generated/skill-manifest.json",
			Kind:        "generated_view",
			Owner:       "fixture-spine",
			SourceClass: "generated",
			Revision:    "sha256:abc123",
		},
		GeneratedView: GeneratedView{
			ViewKind:  "skill_manifest",
			Consumer:  "agent harnesses",
			Generator: "fixture-generate",
			Marker:    "generated",
			Freshness: "fresh",
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     "skill-manifest",
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: []string{"artifact_sha256:abc123"},
	}
}

func readNoPromoteRows(t *testing.T, home string) []struct {
	ID           string `json:"id"`
	CandidateRef string `json:"candidate_ref"`
	OwnerTarget  struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"owner_target"`
} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var rows []struct {
		ID           string `json:"id"`
		CandidateRef string `json:"candidate_ref"`
		OwnerTarget  struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref"`
		} `json:"owner_target"`
	}
	for _, line := range lines {
		var row struct {
			ID           string `json:"id"`
			CandidateRef string `json:"candidate_ref"`
			OwnerTarget  struct {
				Kind string `json:"kind"`
				Ref  string `json:"ref"`
			} `json:"owner_target"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func writeSourceObservationVerdictFixture(t *testing.T, home, verdictID string, candidate CandidateRecord) string {
	t.Helper()
	payload := sourceObservationVerdictCandidatePayload{
		ID:                  candidate.ID,
		Kind:                candidate.Kind,
		Subject:             candidate.Subject,
		Statement:           candidate.Statement,
		Detail:              candidate.Detail,
		Confidence:          candidate.Confidence,
		Verdict:             candidate.Verdict,
		EvidenceRefs:        candidate.EvidenceRefs,
		SourceObservationID: candidate.SourceObservationID,
		SourceRef:           candidate.SourceRef,
		PromoteAllowed:      candidate.PromoteAllowed,
	}
	return writeSourceObservationVerdictPayloadFixture(t, home, verdictID, payload)
}

func writeSourceObservationVerdictPayloadFixture(t *testing.T, home, verdictID string, payload sourceObservationVerdictCandidatePayload) string {
	t.Helper()
	dir := filepath.Join(home, "reviews", "panel-verdicts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             verdictID,
		"created_at":     tSource.Format(time.RFC3339),
		"run_id":         "run_source_observation_fixture",
		"target": map[string]any{
			"kind":       string(candidates.CapabilityObs),
			"ref":        payload.ID,
			"visibility": "internal",
			"summary":    payload.Statement,
			"payload":    json.RawMessage(raw),
		},
		"lens_findings": []any{},
		"synthesis": map[string]any{
			"schema_version":     "compound.panel_synthesis.v0.1",
			"decision":           "park",
			"confidence":         0.72,
			"rationale":          "Fixture panel verdict says source observation should remain evidence until reviewed.",
			"dissent":            []any{},
			"guardrails_applied": []string{"evidence_not_truth"},
		},
		"evidence_refs": []string{"route_evidence:source-observation-verdict-fixture"},
	}
	data, err := json.MarshalIndent(body, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, verdictID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSourceObservationVerdictRenderRecordGolden pins the render + record + load
// scaffolding byte-for-byte across the extraction. The two "recorded file == render
// func output" equalities exercise the shared plan-record seam (verdictflow), so a
// mutation there reddens this golden without touching sourceobs's own render code.
func TestSourceObservationVerdictRenderRecordGolden(t *testing.T) {
	home := t.TempDir()
	payload := sourceObservationVerdictCandidatePayload{
		ID:                  "ocand_golden_fixture",
		Kind:                string(candidates.CapabilityObs),
		Subject:             "observation:source:generated-view",
		Statement:           "Synthetic generated-view drift needs source-owner review.",
		Detail:              "source observation evidence, not truth",
		Confidence:          0.66,
		EvidenceRefs:        []string{"fixture:source:golden"},
		SourceObservationID: "srcobs_golden_fixture",
	}
	verdictPath := writeSourceObservationVerdictPayloadFixture(t, home, "panel_verdict_source_golden", payload)
	plan, err := BuildSourceObservationVerdictPlan(home, verdictPath, "", tSource)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OwnerKind != "source_observation" || plan.OwnerRef != "srcobs_golden_fixture" || plan.OwnerStatus != "sourceobs" {
		t.Fatalf("owner derivation drift: %+v", plan)
	}

	reviewYAML := RenderSourceObservationVerdictReviewYAML(plan)
	for _, want := range []string{
		"schema_version: compound.source_observation_verdict_review.v0.1\n",
		"decision: TODO_decision_record_no_promote_park_reject\n",
		"candidate_ref: ocand_golden_fixture\n",
	} {
		if !strings.Contains(reviewYAML, want) {
			t.Fatalf("review YAML missing %q:\n%s", want, reviewYAML)
		}
	}
	markdown := renderSourceObservationVerdictPlanMarkdown(plan)
	if !strings.HasPrefix(markdown, "# Source Observation Verdict Plan: panel_verdict_source_golden\n") {
		t.Fatalf("markdown header drift:\n%s", markdown)
	}

	paths, err := RecordSourceObservationVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSourceVerdictGoldenFile(t, paths.ReviewYAMLPath); got != reviewYAML {
		t.Fatalf("recorded review.yaml != render:\n got=%q\nwant=%q", got, reviewYAML)
	}
	if got := readSourceVerdictGoldenFile(t, paths.ReviewPath); got != markdown {
		t.Fatalf("recorded .md != render:\n got=%q\nwant=%q", got, markdown)
	}

	filled := strings.NewReplacer(
		"decision: TODO_decision_record_no_promote_park_reject", "decision: record_no_promote",
		"rationale: TODO_rationale", "rationale: keep reviewed source verdict as no-promote evidence",
		"confidence: TODO_confidence_0_to_1", "confidence: 0.7",
	).Replace(reviewYAML)
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadSourceObservationVerdictReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if review.Decision != "record_no_promote" || review.PlanID != plan.ID || review.CandidateRef != plan.TargetRef || review.Confidence != 0.7 {
		t.Fatalf("load round-trip drift: %+v", review)
	}
}

func readSourceVerdictGoldenFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
