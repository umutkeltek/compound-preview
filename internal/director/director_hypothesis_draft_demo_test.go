package director

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/product"
	"compound/internal/routeevidence"
)

// TestDirectorHypothesisDraftDemo is the human-readable proof surface the
// packet's REPORT names: it seeds one starved product and runs the director in
// dry-run mode so the operator can see the new proposal text without granting
// the apply path. Output is intended for inclusion in REPORT.md, not for
// CI gating.
func TestDirectorHypothesisDraftDemo(t *testing.T) {
	if os.Getenv("DIRECTOR_DEMO") != "1" {
		t.Skip("set DIRECTOR_DEMO=1 to capture the director dry-run output for the report")
	}
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	seedStarvedProduct(t, home, "product:alpha-app",
		"After public release, the angler-intent subtitle",
		"gsc_clicks",
		[]string{"sig_hs_1", "sig_hs_2", "sig_hs_3"}, now)

	report, err := Propose(home, Options{Apply: false, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, p := range report.Proposals {
		if p.Kind != "product_hypothesis_draft" {
			continue
		}
		matches++
		summary := map[string]any{
			"kind":         p.Kind,
			"title":        p.Title,
			"trigger_ref":  p.TriggerRef,
			"work_id":      p.WorkID,
			"evidence_ref": p.EvidenceRefs,
		}
		body, _ := json.MarshalIndent(summary, "", "  ")
		t.Logf("director proposal:\n%s", string(body))
	}
	entries, _ := os.ReadDir(filepath.Join(home, "inbox", "product-hypotheses"))
	if entries != nil && len(entries) > 0 {
		t.Fatalf("dry-run leaked %d files under inbox/product-hypotheses/", len(entries))
	}
	if matches != 1 {
		t.Fatalf("demo expected 1 draft, got %d", matches)
	}
	_ = product.LoopModelSchemaVersion
	_ = time.Time{}
}
