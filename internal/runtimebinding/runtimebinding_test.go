package runtimebinding

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestDoctorValidSyntheticService(t *testing.T) {
	b := validBundle()
	report := Doctor(b)
	if report.Status != "ok" {
		t.Fatalf("status = %q, checks = %+v", report.Status, report.Checks)
	}
	if report.ServiceID != "service:synthetic-api" || report.Owner != "product:synthetic" {
		t.Fatalf("identity not preserved: %+v", report)
	}
	if len(report.Checks) != 10 {
		t.Fatalf("checks = %d, want 10", len(report.Checks))
	}
}

func TestDoctorFailsMissingHost(t *testing.T) {
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	report := Doctor(b)
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail", report.Status)
	}
	if got := checkStatus(report, "host"); got != "fail" {
		t.Fatalf("host check = %q, want fail", got)
	}
}

func TestDoctorWithLiveProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	filePath := filepath.Join(t.TempDir(), "service.db")
	if err := os.WriteFile(filePath, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_RUNTIME_TOKEN", "present")

	b := validBundle()
	b.RuntimeProbes = []RuntimeProbe{
		{ID: "health", Kind: "http", Target: srv.URL + "/health", ExpectedStatus: http.StatusNoContent},
		{ID: "port", Kind: "tcp", Target: ln.Addr().String()},
		{ID: "data", Kind: "file", Target: filePath},
		{ID: "credential", Kind: "env", Target: "SYNTHETIC_RUNTIME_TOKEN"},
	}
	report := DoctorWithLiveProbes(context.Background(), b, time.Now())
	if report.Status != "ok" {
		t.Fatalf("status = %q, checks = %+v", report.Status, report.Checks)
	}
	for _, name := range []string{"live:health", "live:port", "live:data", "live:credential"} {
		if got := checkStatus(report, name); got != "ok" {
			t.Fatalf("%s = %q, checks=%+v", name, got, report.Checks)
		}
	}
}

func TestDoctorWithLiveProbesRequiresCoverage(t *testing.T) {
	report := DoctorWithLiveProbes(context.Background(), validBundle(), time.Now())
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail; checks=%+v", report.Status, report.Checks)
	}
	if got := checkStatus(report, "live:coverage"); got != "fail" {
		t.Fatalf("live coverage check = %q, want fail; checks=%+v", got, report.Checks)
	}
	if !NeedsWriteback(report) {
		t.Fatal("missing live coverage should enter runtime writeback")
	}
}

func TestDoctorWithLiveProbesFailsUnavailableEvidence(t *testing.T) {
	b := validBundle()
	b.RuntimeProbes = []RuntimeProbe{
		{ID: "credential", Kind: "env", Target: "COMPOUND_TEST_MISSING_RUNTIME_TOKEN"},
		{ID: "data", Kind: "file", Target: filepath.Join(t.TempDir(), "missing.db")},
	}
	report := DoctorWithLiveProbes(context.Background(), b, time.Now())
	if report.Status != "fail" {
		t.Fatalf("status = %q, checks = %+v", report.Status, report.Checks)
	}
	if got := checkStatus(report, "live:credential"); got != "fail" {
		t.Fatalf("credential check = %q, want fail", got)
	}
	if got := checkStatus(report, "live:data"); got != "fail" {
		t.Fatalf("data check = %q, want fail", got)
	}
	if !NeedsWriteback(report) {
		t.Fatalf("live runtime failure should need writeback")
	}
}

func TestWritebackGateForFailedRuntime(t *testing.T) {
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	report := Doctor(b)
	if !NeedsWriteback(report) {
		t.Fatalf("failed runtime doctor should need writeback")
	}
	gate := WritebackGate(report, "act_runtime", "pol_runtime", time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if gate.OwnerTarget.Kind != "runtime_binding" || gate.OwnerTarget.Ref != "service:synthetic-api" {
		t.Fatalf("wrong owner target: %+v", gate.OwnerTarget)
	}
	if gate.PolicyDecisionRef != "pol_runtime" || gate.ActionIntentRef != "act_runtime" {
		t.Fatalf("wrong gate refs: %+v", gate)
	}
	if len(gate.EvidenceRefs) < len(report.Checks)+3 {
		t.Fatalf("expected runtime evidence refs, got %+v", gate.EvidenceRefs)
	}
}

func TestBuildRuntimeRepairPlan(t *testing.T) {
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	report := Doctor(b)
	plan, err := BuildRepairPlan(report, RepairRefs{
		WritebackGateID: "owg_runtime",
		WorkID:          "work_runtime",
		ReceiptID:       "rec_runtime",
		TransitionID:    "tr_runtime",
	}, time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != RepairPlanSchemaVersion || plan.Status != "review_required" || plan.FailedCount != 1 {
		t.Fatalf("bad plan: %+v", plan)
	}
	if plan.WritebackGateID != "owg_runtime" || plan.OwnerTarget != "runtime_binding:service:synthetic-api" {
		t.Fatalf("bad refs: %+v", plan)
	}
	if len(plan.ReviewActions) != 1 || plan.ReviewActions[0].Check != "host" {
		t.Fatalf("review actions = %+v", plan.ReviewActions)
	}
	review := RenderRepairReview(plan)
	if !strings.Contains(review, "does not mutate runtime owner files") || !strings.Contains(review, "declare the host") {
		t.Fatalf("review did not preserve gate warning/action:\n%s", review)
	}
}

func TestRecordRuntimeRepairPlan(t *testing.T) {
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	report := Doctor(b)
	plan, err := BuildRepairPlan(report, RepairRefs{WritebackGateID: "owg_runtime"}, time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordRepairPlan(t.TempDir(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.PlanPath); err != nil {
		t.Fatalf("plan missing: %v", err)
	}
	if _, err := os.Stat(paths.ReviewPath); err != nil {
		t.Fatalf("review missing: %v", err)
	}
	if _, err := os.Stat(paths.ReviewYAMLPath); err != nil {
		t.Fatalf("review yaml missing: %v", err)
	}
}

func TestRecordRuntimeRepairReviewAnnotatesQueue(t *testing.T) {
	homePath := t.TempDir()
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	report := Doctor(b)
	plan, err := BuildRepairPlan(report, RepairRefs{WritebackGateID: "owg_runtime"}, time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordRepairPlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(body), "TODO_decision_owner_action_required_owner_action_done_defer_no_action", "owner_action_required")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: \"\"", "rationale: \"owner reviewed runtime repair path\"")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: -1", "confidence: 0.91")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO: reviewed runtime repair next action", "rerun runtime doctor after owner contract edit")
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadRepairReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := RecordRepairReview(homePath, review, paths.ReviewYAMLPath, time.Date(2026, 6, 29, 1, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "recorded" || record.PlanID != plan.ID || record.ServiceID != plan.ServiceID {
		t.Fatalf("bad record: %+v", record)
	}
	if _, err := os.Stat(record.ReviewPath); err != nil {
		t.Fatalf("reviewed sidecar missing: %v", err)
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: RuntimeRepairReviewArea}, time.Date(2026, 6, 29, 1, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateRepairQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.Total != 1 || queue.Counts.OK != 1 || queue.Counts.ReviewRequired != 0 {
		t.Fatalf("runtime repair review should close queue debt: %+v", queue.Counts)
	}
	if queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" {
		t.Fatalf("runtime repair queue item not recorded: %+v", queue.Items[0])
	}
	if !strings.Contains(queue.Items[0].NextAction, "rerun runtime doctor") {
		t.Fatalf("next action did not come from reviewed sidecar: %+v", queue.Items[0])
	}
}

// Nothing pins a runtime repair review id apart from its plan id, so a review
// YAML that reuses the plan id made the producer join the two into a doubled
// `<id>.<id>.reviewed.json` stem — the shape that left source-control
// dispositions invisible to every sidecar reader, keeping the core item
// non-terminal forever and its review area undrainable.
func TestRecordRepairReviewDeDupesEqualPlanAndReviewIDStem(t *testing.T) {
	homePath := t.TempDir()
	plan, paths := recordRepairPlanForReview(t, homePath)
	review := loadValidRepairReview(t, paths.ReviewYAMLPath)
	review.ID = plan.ID

	record, err := RecordRepairReview(homePath, review, paths.ReviewYAMLPath, time.Date(2026, 6, 29, 1, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(paths.ReviewYAMLPath)
	wantPath := filepath.Join(dir, plan.ID+".reviewed.json")
	if filepath.Clean(record.ReviewPath) != filepath.Clean(wantPath) {
		t.Fatalf("recorded review path = %q, want the sidecar stem %q", record.ReviewPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("recorded disposition is not on the sidecar stem: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, plan.ID+"."+plan.ID+".reviewed.json")); err == nil {
		t.Fatalf("recorded disposition still written at the doubled stem")
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: RuntimeRepairReviewArea}, time.Date(2026, 6, 29, 1, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateRepairQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.Total != 1 || queue.Counts.OK != 1 {
		t.Fatalf("de-duped disposition did not close its queue debt: %+v", queue.Counts)
	}
}

// A review id that genuinely differs from its plan id keeps the joined form, so
// distinct dispositions of one plan never collide on a single stem.
func TestRecordRepairReviewKeepsJoinedStemForDistinctReviewID(t *testing.T) {
	homePath := t.TempDir()
	plan, paths := recordRepairPlanForReview(t, homePath)
	review := loadValidRepairReview(t, paths.ReviewYAMLPath)
	if review.ID == plan.ID {
		t.Fatalf("draft review id must differ from its plan id, got %q twice", plan.ID)
	}

	record, err := RecordRepairReview(homePath, review, paths.ReviewYAMLPath, time.Date(2026, 6, 29, 1, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(filepath.Dir(paths.ReviewYAMLPath), plan.ID+"."+review.ID+".reviewed.json")
	if filepath.Clean(record.ReviewPath) != filepath.Clean(wantPath) {
		t.Fatalf("recorded review path = %q, want the joined stem %q", record.ReviewPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("recorded disposition is not on the joined stem: %v", err)
	}
}

func recordRepairPlanForReview(t *testing.T, homePath string) (RepairPlan, RepairArtifactPaths) {
	t.Helper()
	b := validBundle()
	b.RuntimeBinding.HostBinding = ""
	plan, err := BuildRepairPlan(Doctor(b), RepairRefs{WritebackGateID: "owg_runtime"}, time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordRepairPlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, paths
}

func loadValidRepairReview(t *testing.T, reviewYAMLPath string) RepairReview {
	t.Helper()
	review, err := LoadRepairReview(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	review.Decision = "owner_action_required"
	review.Rationale = "owner reviewed runtime repair path"
	review.Confidence = 0.91
	review.NextAction = "rerun runtime doctor after owner contract edit"
	return review
}

func TestLoadFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.yaml")
	body := `runtime_binding:
  service_id: service:synthetic-api
  owner_entity: product:synthetic
  host_binding: host:local-dev
  deployment_binding: process:synthetic-api
  data_bindings: [sqlite:synthetic.db]
  health_check_refs: [https://runtime-binding.example.invalid/health]
  risk_tier: tier1
  evidence_refs: [fixture:runtime-binding]
  status: active
service_contract:
  service_id: service:synthetic-api
  interface_refs: [http:get:/health]
  dependencies: [sqlite]
  owner_entity: product:synthetic
  risk_tier: tier1
  evidence_refs: [fixture:service-contract]
  status: active
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if report := Doctor(b); report.Status != "ok" {
		t.Fatalf("loaded fixture failed doctor: %+v", report)
	}
}

func validBundle() Bundle {
	return Bundle{
		RuntimeBinding: RuntimeBinding{
			ServiceID:         "service:synthetic-api",
			OwnerEntity:       "product:synthetic",
			HostBinding:       "host:local-dev",
			DeploymentBinding: "process:synthetic-api",
			DataBindings:      []string{"sqlite:synthetic.db"},
			HealthCheckRefs:   []string{"https://runtime-binding.example.invalid/health"},
			RiskTier:          "tier1",
			EvidenceRefs:      []string{"fixture:runtime-binding"},
			Status:            StatusActive,
		},
		ServiceContract: ServiceContract{
			ServiceID:     "service:synthetic-api",
			InterfaceRefs: []string{"http:get:/health"},
			Dependencies:  []string{"sqlite"},
			OwnerEntity:   "product:synthetic",
			RiskTier:      "tier1",
			EvidenceRefs:  []string{"fixture:service-contract"},
			Status:        StatusActive,
		},
	}
}

func checkStatus(r DoctorReport, name string) string {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}
