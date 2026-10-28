package projection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestDoctorFreshFixture(t *testing.T) {
	report, err := DoctorFile(filepath.Join("..", "..", "fixtures", "projections", "freshness.yaml"), time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusFresh {
		t.Fatalf("status = %q, report = %+v", report.Status, report)
	}
	if report.OutputHash == "" || len(report.Sources) != 1 {
		t.Fatalf("hashes not populated: %+v", report)
	}
}

func TestDoctorDashboardReadModelFresh(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "source.json"), `{"ok":true}`+"\n")
	modelPath := writeDashboardReadModel(t, dir, DashboardReadModel{
		Version:     1,
		GeneratedAt: "2026-06-29T10:00:00Z",
		SourcePaths: []string{"source.json"},
		Cards: []DashboardCard{{
			ID:          "runtime-health",
			Title:       "Runtime health",
			Lane:        "control",
			Status:      "ok",
			Owner:       "system",
			SourcePaths: []string{"source.json"},
			NextAction:  "keep observing",
			Visibility:  "operator",
		}},
	})
	report, err := DoctorDashboardReadModel(modelPath, 24, time.Date(2026, 6, 29, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusFresh {
		t.Fatalf("status = %q, report = %+v", report.Status, report)
	}
	if report.ProjectionID != "projection:dashboard-read-model" || report.OutputHash == "" || len(report.Sources) != 1 {
		t.Fatalf("dashboard report missing identity/hash/sources: %+v", report)
	}
	if !hasCheck(report.Checks, "card_sources", "ok") || !hasCheck(report.Checks, "source_hashes", "ok") {
		t.Fatalf("dashboard checks missing expected ok coverage: %+v", report.Checks)
	}
}

func TestRecordContextFiltersByRepoKind(t *testing.T) {
	home := t.TempDir()
	report, err := DoctorFile(filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml"), time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	rec, path, err := RecordContext(home, report, []string{"control-plane"}, time.Date(2026, 6, 29, 3, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rec.SchemaVersion != ContextRecordSchemaVersion || rec.ID == "" || path == "" {
		t.Fatalf("bad context record: %+v path=%q", rec, path)
	}
	if ContextRef(report) != "projection://synthetic-runtime-dashboard" || ContextStatus(report.Status) != "stale" {
		t.Fatalf("bad context ref/status: %s %s", ContextRef(report), ContextStatus(report.Status))
	}
	control, err := LoadContext(home, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if len(control) != 1 || control[0].ID != rec.ID {
		t.Fatalf("control-plane records = %+v, want %s", control, rec.ID)
	}
	product, err := LoadContext(home, "product")
	if err != nil {
		t.Fatal(err)
	}
	if len(product) != 0 {
		t.Fatalf("product records = %+v, want none", product)
	}
}

func TestDoctorDashboardReadModelStaleAndPartial(t *testing.T) {
	dir := t.TempDir()
	modelPath := writeDashboardReadModel(t, dir, DashboardReadModel{
		Version:     1,
		GeneratedAt: "2026-06-27T10:00:00Z",
		SourcePaths: []string{"missing.json"},
		Cards: []DashboardCard{{
			ID:         "runtime-health",
			Title:      "Runtime health",
			Lane:       "control",
			Status:     "risk",
			Owner:      "system",
			NextAction: "review source",
			Visibility: "alert",
		}},
	})
	report, err := DoctorDashboardReadModel(modelPath, 1, time.Date(2026, 6, 29, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusDrifted {
		t.Fatalf("status = %q, want drifted (%+v)", report.Status, report)
	}
	if report.CoverageVerdict != "partial" {
		t.Fatalf("coverage = %q, want partial", report.CoverageVerdict)
	}
	if !hasCheck(report.Checks, "card_sources", "fail") || !hasCheck(report.Checks, "source_hashes", "fail") || !hasCheck(report.Checks, "age", "fail") {
		t.Fatalf("dashboard checks missing failures: %+v", report.Checks)
	}
}

func TestDoctorDriftedSourceHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "view.md"), "view\n")
	writeFile(t, filepath.Join(dir, "source.jsonl"), "changed\n")
	manifest := Manifest{
		ProjectionID:         "projection:test",
		ViewRef:              "view.md",
		SourceRefs:           []string{"source.jsonl"},
		SourceLedgerSeq:      2,
		BuiltAt:              "2026-06-29T00:00:00Z",
		MaxAgeHours:          24,
		Generator:            "test",
		CoverageVerdict:      "complete",
		ExpectedSourceHashes: map[string]string{"source.jsonl": "sha256:not-current"},
	}
	report, err := Doctor(manifest, dir, time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusDrifted {
		t.Fatalf("status = %q, want drifted (%+v)", report.Status, report)
	}
}

func TestDoctorStaleCoverage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "view.md"), "view\n")
	writeFile(t, filepath.Join(dir, "source.jsonl"), "source\n")
	manifest := Manifest{
		ProjectionID:    "projection:test",
		ViewRef:         "view.md",
		SourceRefs:      []string{"source.jsonl"},
		SourceLedgerSeq: 2,
		BuiltAt:         "2026-06-29T00:00:00Z",
		MaxAgeHours:     1,
		Generator:       "test",
		CoverageVerdict: "partial",
	}
	report, err := Doctor(manifest, dir, time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusStale {
		t.Fatalf("status = %q, want stale (%+v)", report.Status, report)
	}
}

func TestWritebackGateForStaleProjection(t *testing.T) {
	f := Freshness{
		ProjectionID:    "projection:test",
		ViewRef:         "view.md",
		SourceLedgerSeq: 2,
		Status:          StatusStale,
		Sources:         []SourceObservation{{SourceRef: "source.jsonl", CurrentHash: "sha256:abc", Status: "ok"}},
		CoverageVerdict: "partial",
	}
	if !NeedsWriteback(f) {
		t.Fatalf("stale projection should need writeback")
	}
	gate := WritebackGate(f, "act_projection", "pol_projection", time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if gate.OwnerTarget.Kind != "generated_view" || gate.OwnerTarget.Ref != "view.md" {
		t.Fatalf("wrong owner target: %+v", gate.OwnerTarget)
	}
	if gate.PolicyDecisionRef != "pol_projection" || gate.ActionIntentRef != "act_projection" {
		t.Fatalf("wrong gate refs: %+v", gate)
	}
	if len(gate.EvidenceRefs) < 4 {
		t.Fatalf("expected projection evidence refs, got %+v", gate.EvidenceRefs)
	}
}

func TestBuildRegenerationPlanRecordsGateAndSourceObservationRefs(t *testing.T) {
	f := Freshness{
		ProjectionID:    "projection:test",
		ViewRef:         "view.md",
		SourceLedgerSeq: 2,
		Status:          StatusStale,
		Staleness:       "25h0m0s",
		CoverageVerdict: "partial",
		OutputHash:      "sha256:view",
		Sources:         []SourceObservation{{SourceRef: "source.jsonl", CurrentHash: "sha256:source", Status: "ok"}},
		Checks:          []Check{{Name: "coverage", Status: "fail", Detail: "partial"}},
	}
	refs := RegenerationRefs{
		WritebackGateID:     "owg_projection",
		WorkID:              "work_projection",
		ReceiptID:           "rec_projection",
		TransitionID:        "tr_projection",
		SourceObservationID: "srcobs_projection",
		CandidateIDs:        []string{"src_cand_projection"},
		NoPromoteIDs:        []string{"nop_projection"},
	}

	plan, err := BuildRegenerationPlan(f, refs, time.Date(2026, 6, 29, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != RegenerationPlanSchemaVersion || plan.Status != "review_required" {
		t.Fatalf("bad plan header: %+v", plan)
	}
	if plan.WritebackGateID != refs.WritebackGateID || plan.SourceObservationID != refs.SourceObservationID {
		t.Fatalf("plan did not keep refs: %+v", plan)
	}
	if len(plan.FailedChecks) != 1 || plan.FailedChecks[0].Name != "coverage" {
		t.Fatalf("failed checks = %+v", plan.FailedChecks)
	}
	if len(plan.ProposedActions) == 0 || len(plan.EvidenceRefs) < 4 {
		t.Fatalf("plan missing actions/evidence: %+v", plan)
	}
}

func TestBuildRegenerationPlanRejectsFreshProjection(t *testing.T) {
	_, err := BuildRegenerationPlan(Freshness{ProjectionID: "projection:fresh", Status: StatusFresh}, RegenerationRefs{}, time.Date(2026, 6, 29, 23, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected fresh projection to reject regeneration plan")
	}
}

func TestRecordRegenerationPlanWritesReviewArtifacts(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildRegenerationPlan(Freshness{
		ProjectionID:    "projection:test",
		ViewRef:         "view.md",
		SourceLedgerSeq: 2,
		Status:          StatusStale,
		OutputHash:      "sha256:view",
		Checks:          []Check{{Name: "age", Status: "fail", Detail: "25h0m0s"}},
	}, RegenerationRefs{WritebackGateID: "owg_projection", SourceObservationID: "srcobs_projection"}, time.Date(2026, 6, 29, 23, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	paths, err := RecordRegenerationPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %s missing: %v", path, err)
		}
	}
	if _, err := os.Stat(paths.ReviewYAMLPath); err != nil {
		t.Fatalf("review YAML %s missing: %v", paths.ReviewYAMLPath, err)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "owner-review artifact") || !strings.Contains(string(review), "does not regenerate or edit") {
		t.Fatalf("review artifact missing non-write warning:\n%s", review)
	}
}

func TestRecordRegenerationReviewAnnotatesQueue(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildRegenerationPlan(Freshness{
		ProjectionID:    "projection:test",
		ViewRef:         "view.md",
		SourceLedgerSeq: 2,
		Status:          StatusStale,
		OutputHash:      "sha256:view",
		Checks:          []Check{{Name: "age", Status: "fail", Detail: "25h0m0s"}},
	}, RegenerationRefs{WritebackGateID: "owg_projection", SourceObservationID: "srcobs_projection"}, time.Date(2026, 6, 29, 23, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordRegenerationPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ProjectionRegenerationReviewArea}, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateProjectionRegenerationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.Total != 1 || queue.Counts.ReviewRequired != 1 || queue.Items[0].Readiness != "needs_review" {
		t.Fatalf("unedited queue = %+v", queue)
	}

	review := RegenerationReview{
		SchemaVersion: RegenerationReviewSchemaVersion,
		ID:            reviewartifact.ID("prrv_", plan.ID, plan.ProjectionID, plan.ViewRef),
		PlanID:        plan.ID,
		ProjectionID:  plan.ProjectionID,
		ViewRef:       plan.ViewRef,
		Decision:      "owner_action_required",
		Rationale:     "owner must regenerate through the owning generator before this view can be trusted",
		Confidence:    0.9,
		NextAction:    "run owner generator then rerun projection doctor",
		EvidenceRefs:  plan.EvidenceRefs,
	}
	report, err := RecordRegenerationReview(home, review, paths.ReviewYAMLPath, time.Date(2026, 6, 30, 0, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "recorded" || report.Decision != "owner_action_required" || report.ReviewPath == "" {
		t.Fatalf("review report = %+v", report)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ProjectionRegenerationReviewArea}, time.Date(2026, 6, 30, 0, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateProjectionRegenerationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.OK != 1 || queue.Counts.ReviewRequired != 0 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" {
		t.Fatalf("recorded queue = %+v", queue)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection regeneration review must not promote claims, stat err=%v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDashboardReadModel(t *testing.T, dir string, model DashboardReadModel) string {
	t.Helper()
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "read-model.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasCheck(checks []Check, name, status string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}
