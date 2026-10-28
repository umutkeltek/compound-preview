package projection

import (
	"path/filepath"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

var systemStatusNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

// TestBuildSystemStatusEmptyHomeGracefulAndOK proves the projection degrades per
// section rather than failing: a fresh home with no imported authorities still
// builds, with each not-yet-installed authority reported "missing" (never an
// error) and every section carrying its canonical source ref.
func TestBuildSystemStatusEmptyHomeGracefulAndOK(t *testing.T) {
	h := t.TempDir()
	if err := home.Init(h, "test", systemStatusNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	proj, err := BuildSystemStatus(h, systemStatusNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if proj.SchemaVersion != SystemStatusSchemaVersion || proj.ProjectionRef != SystemStatusProjectionRef {
		t.Fatalf("schema/ref: got %q/%q", proj.SchemaVersion, proj.ProjectionRef)
	}
	// workbench read model opens on any home → ok, zero work.
	if proj.Sections.Workbench.Status != sectionStatusOK || proj.Sections.Workbench.Total != 0 {
		t.Errorf("empty workbench: status=%q total=%d, want ok/0", proj.Sections.Workbench.Status, proj.Sections.Workbench.Total)
	}
	// authorities with no data file yet → missing, not error.
	for name, got := range map[string]string{
		"job_roster":     proj.Sections.JobRoster.Status,
		"route_evidence": proj.Sections.RouteEvidence.Status,
		"capability":     proj.Sections.Capability.Status,
		"product":        proj.Sections.Product.Status,
	} {
		if got != sectionStatusMissing {
			t.Errorf("empty %s: status=%q, want missing", name, got)
		}
	}
	if proj.Verdict != "ok" {
		t.Errorf("empty verdict=%q, want ok (no operator work)", proj.Verdict)
	}
	assertSystemStatusSourceRefs(t, proj)
}

// TestBuildSystemStatusFoldsAuthoritiesFaithfully is the depth falsifier: it
// seeds real data into each of the five named authorities, then asserts the
// folded section EQUALS the authority's own direct computation (no drift) AND
// carries the canonical source ref. A field mis-map (e.g. open<-total) or a
// dropped source ref reds this test.
func TestBuildSystemStatusFoldsAuthoritiesFaithfully(t *testing.T) {
	h := t.TempDir()
	if err := home.Init(h, "test", systemStatusNow); err != nil {
		t.Fatalf("home init: %v", err)
	}

	// workbench: two real work envelopes through the admission path.
	store, err := work.Open(h)
	if err != nil {
		t.Fatalf("work open: %v", err)
	}
	if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "ready work gets claimed"}, systemStatusNow); err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "held work gets reviewed", State: "held"}, systemStatusNow); err != nil {
		t.Fatalf("create held work: %v", err)
	}

	// route evidence: two valid rows through the package's own builder.
	for _, rid := range []string{"req-1", "req-2"} {
		ev, err := routeevidence.Build(routeevidence.BuildInput{
			CreatedAt:     systemStatusNow,
			RequestID:     rid,
			ClientProfile: "test",
			Protocol:      "openai",
			Provider:      "demo",
			Model:         "demo-model",
			Status:        "ok",
			Usage:         routeevidence.Usage{TotalTokens: 10},
		})
		if err != nil {
			t.Fatalf("build route evidence %s: %v", rid, err)
		}
		if err := routeevidence.RecordEvidence(h, ev); err != nil {
			t.Fatalf("record route evidence %s: %v", rid, err)
		}
	}

	// job roster: 3 jobs, one enabled+active with an error → 1 active error.
	writeFile(t, authority.JobRosterAuthorityPath(h), `{"schema_version":"compound.job_roster_authority.v0.1","jobs":[`+
		`{"name":"alpha","enabled":true,"state":"active","last_status":"ok"},`+
		`{"name":"beta","enabled":true,"state":"active","last_status":"error","last_error":"boom"},`+
		`{"name":"gamma","enabled":false,"state":"disabled","last_status":"ok"}]}`)

	// product: one signal for one product.
	writeFile(t, filepath.Join(h, "ledger", "product-signals.jsonl"),
		`{"id":"sig-1","product_id":"demoapp","surface":"app","change_event":"release","metric_name":"installs","evidence_refs":[]}`+"\n")

	// capability: 3 nodes, 2 edges (any non-empty JSONL lines count).
	writeFile(t, filepath.Join(h, "registries", "capability-nodes.jsonl"),
		"{\"id\":\"n1\"}\n{\"id\":\"n2\"}\n{\"id\":\"n3\"}\n")
	writeFile(t, filepath.Join(h, "registries", "capability-edges.jsonl"),
		"{\"id\":\"e1\"}\n{\"id\":\"e2\"}\n")

	proj, err := BuildSystemStatus(h, systemStatusNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// --- fold MIRRORS the authority (no drift), with non-zero seeded data ---

	// route evidence
	rr, err := routeevidence.LoadAuthority(h, 0)
	if err != nil {
		t.Fatalf("route authority: %v", err)
	}
	if rr.RouteEvidenceCount != 2 {
		t.Fatalf("seed sanity: route rows=%d, want 2", rr.RouteEvidenceCount)
	}
	if proj.Sections.RouteEvidence.Status != sectionStatusOK ||
		proj.Sections.RouteEvidence.RouteEvidenceCount != rr.RouteEvidenceCount ||
		proj.Sections.RouteEvidence.TotalTokens != rr.TotalTokens {
		t.Errorf("route fold drift: proj=%+v authority count=%d tokens=%d", proj.Sections.RouteEvidence, rr.RouteEvidenceCount, rr.TotalTokens)
	}

	// job roster
	jr, err := authority.LoadJobRosterAuthority(h)
	if err != nil {
		t.Fatalf("job roster authority: %v", err)
	}
	if jr.JobCount != 3 || jr.EnabledCount != 2 || jr.ActiveErrorCount != 1 {
		t.Fatalf("seed sanity: roster jobs=%d enabled=%d active_err=%d, want 3/2/1", jr.JobCount, jr.EnabledCount, jr.ActiveErrorCount)
	}
	if proj.Sections.JobRoster.Status != sectionStatusOK ||
		proj.Sections.JobRoster.JobCount != jr.JobCount ||
		proj.Sections.JobRoster.EnabledCount != jr.EnabledCount ||
		proj.Sections.JobRoster.ErrorCount != jr.ErrorCount ||
		proj.Sections.JobRoster.ActiveErrorCount != jr.ActiveErrorCount {
		t.Errorf("job roster fold drift: proj=%+v authority=%+v", proj.Sections.JobRoster, jr)
	}

	// workbench
	ws, err := store.Workbench(systemStatusNow, work.WorkbenchOptions{})
	if err != nil {
		t.Fatalf("workbench read model: %v", err)
	}
	if ws.Counts.Total < 2 {
		t.Fatalf("seed sanity: workbench total=%d, want >=2", ws.Counts.Total)
	}
	if proj.Sections.Workbench.Status != sectionStatusOK ||
		proj.Sections.Workbench.Total != ws.Counts.Total ||
		proj.Sections.Workbench.Open != ws.Counts.Open ||
		proj.Sections.Workbench.Terminal != ws.Counts.Terminal ||
		proj.Sections.Workbench.NeedsClaim != ws.Counts.NeedsClaim ||
		proj.Sections.Workbench.NeedsReview != ws.Counts.NeedsReview ||
		proj.Sections.Workbench.InProgress != ws.Counts.InProgress {
		t.Errorf("workbench fold drift: proj=%+v authority counts=%+v", proj.Sections.Workbench, ws.Counts)
	}

	// product (folded from the product-feedback ledger files directly)
	if proj.Sections.Product.Status != sectionStatusOK ||
		proj.Sections.Product.SignalCount != 1 ||
		proj.Sections.Product.ProductCount != 1 ||
		proj.Sections.Product.ReviewCount != 0 {
		t.Errorf("product fold: proj=%+v, want ok/signals=1/products=1/reviews=0", proj.Sections.Product)
	}

	// capability (counted from registry files directly)
	if proj.Sections.Capability.Status != sectionStatusOK ||
		proj.Sections.Capability.NodeCount != 3 ||
		proj.Sections.Capability.EdgeCount != 2 {
		t.Errorf("capability fold: proj=%+v, want ok/3/2", proj.Sections.Capability)
	}

	// --- every section carries its canonical source ref ---
	assertSystemStatusSourceRefs(t, proj)

	// --- verdict flips to attention (active cron error present) ---
	if proj.Verdict != "attention" {
		t.Errorf("verdict=%q, want attention (active job error seeded)", proj.Verdict)
	}
}

func assertSystemStatusSourceRefs(t *testing.T, proj SystemStatusProjection) {
	t.Helper()
	want := map[string]string{
		"workbench":            proj.Sections.Workbench.SourceRef,
		"job_roster":           proj.Sections.JobRoster.SourceRef,
		"route_evidence":       proj.Sections.RouteEvidence.SourceRef,
		"product_authority":    proj.Sections.Product.SourceRef,
		"capability_authority": proj.Sections.Capability.SourceRef,
	}
	expect := map[string]string{
		"workbench":            workbenchSourceRef,
		"job_roster":           authority.JobRosterAuthorityRef,
		"route_evidence":       routeevidence.RouteEvidenceAuthorityRef,
		"product_authority":    productFeedbackSourceRef,
		"capability_authority": capabilityGraphSourceRef,
	}
	for section, got := range want {
		if got != expect[section] {
			t.Errorf("section %s source_ref=%q, want %q", section, got, expect[section])
		}
	}
	// the top-level SourceRefs union carries every folded authority ref.
	for _, ref := range expect {
		found := false
		for _, have := range proj.SourceRefs {
			if have == ref {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SourceRefs missing %q (have %v)", ref, proj.SourceRefs)
		}
	}
}
