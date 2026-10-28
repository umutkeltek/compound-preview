package director

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/product"
	"compound/internal/routeevidence"
)

// TestDirectorEmitsHypothesisDraftForStarvedProduct is the W5 J9-R3G red
// falsifier: the dry-run loop with three starved products must emit EXACTLY
// three product_hypothesis_draft candidates — one per product, never per
// group — with one product_signal:<id> ref per signal in the picked group's
// signal list and a Command that names the deterministic candidate id.
func TestDirectorEmitsHypothesisDraftForStarvedProduct(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	seedStarvedProduct(t, home, "product:gamma-app", "Pro IAP price step", "app_store_connect_iap_units", []string{"sig_ct_1", "sig_ct_2", "sig_ct_3", "sig_ct_4"}, now)
	seedStarvedProduct(t, home, "product:alpha-app", "After public release, the angler-intent subtitle", "gsc_clicks", []string{"sig_hs_1", "sig_hs_2", "sig_hs_3"}, now)
	seedStarvedProduct(t, home, "product:beta-app", "Google Search Console report snapshot observed", "gsc_clicks", []string{"sig_ic_1", "sig_ic_2", "sig_ic_3"}, now)
	// Starved products have < 3 signals — must be filtered out.
	seedStarvedProduct(t, home, "product:quiet", "tiny change", "iap_units", []string{"sig_q_1", "sig_q_2"}, now)

	// productLoopCandidates is the package-internal scanner; it is the
	// surface the W5 J9-R3G red test asserts (precise candidate shape,
	// evidence refs, command). The full Propose() suppresses irrelevant
	// noise and only exposes Proposal fields, which intentionally drop the
	// Command/RequiredInputs the candidate-side test depends on.
	cands, err := productLoopCandidates(home, directorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	var drafts []candidate
	for _, c := range cands {
		if c.Kind == "product_hypothesis_draft" {
			drafts = append(drafts, c)
		}
	}
	if len(drafts) != 3 {
		t.Fatalf("dry-run must emit EXACTLY 3 hypothesis_draft candidates (one per starved product), got %d", len(drafts))
	}
	seen := map[string]bool{}
	signalsPerProduct := map[string]int{
		"product:gamma-app": 4,
		"product:alpha-app": 3,
		"product:beta-app":  3,
	}
	for _, d := range drafts {
		productID, changeEvent := stripDraftMetadata(d)
		if productID == "" {
			t.Fatalf("draft must carry a product_id, got %+v", d)
		}
		seen[productID] = true
		wantID := hypothesisDraftIDFor(productID, changeEvent)
		wantCommand := "compound product hypothesis-plan --candidate " + wantID
		if got := strings.Join(d.Command, " "); got != wantCommand {
			t.Fatalf("proposal command wrong for %s: got %q want %q", productID, got, wantCommand)
		}
		wantSigRefs := signalsPerProduct[productID]
		if wantSigRefs == 0 {
			t.Fatalf("draft carried a product not seeded by the test: %s", productID)
		}
		counted := 0
		var hasLoop, hasActionCard bool
		for _, ref := range d.EvidenceRefs {
			if strings.HasPrefix(ref, "product_signal:") {
				counted++
			}
			if strings.HasPrefix(ref, "product_loop:") {
				hasLoop = true
			}
			if strings.HasPrefix(ref, "product_action_card:") {
				hasActionCard = true
			}
		}
		if counted != wantSigRefs {
			t.Fatalf("product %s: expected %d product_signal:<id> refs, got %d (%v)", productID, wantSigRefs, counted, d.EvidenceRefs)
		}
		if !hasLoop {
			t.Fatalf("product %s: missing product_loop:<id>, got %v", productID, d.EvidenceRefs)
		}
		if !hasActionCard {
			t.Fatalf("product %s: missing product_action_card:<id>, got %v", productID, d.EvidenceRefs)
		}
	}
	for _, want := range []string{"product:gamma-app", "product:alpha-app", "product:beta-app"} {
		if !seen[want] {
			t.Fatalf("missing draft for starved product %s; saw: %v", want, seen)
		}
	}
	if seen["product:quiet"] {
		t.Fatalf("director must NOT emit a draft for a starved product with < 3 signals (product:quiet)")
	}
}

// TestDirectorDryRunIsByteFree is the W5 J9-R3G side-guard: a dry-run never
// writes inbox/product-hypotheses/ even when scanning three starved products.
// The packet insists "keep dry-run byte-free".
func TestDirectorDryRunIsByteFree(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	seedStarvedProduct(t, home, "product:gamma-app", "Pro IAP price step", "iap_units", []string{"sig_a", "sig_b", "sig_c"}, now)
	seedStarvedProduct(t, home, "product:alpha-app", "angler subtitle", "gsc_clicks", []string{"sig_d", "sig_e", "sig_f"}, now)
	report, err := Propose(home, Options{Apply: false, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount == 0 {
		t.Fatalf("expected proposals to test byte-free, got %+v", report)
	}
	entries, err := os.ReadDir(filepath.Join(home, "inbox", "product-hypotheses"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read inbox: %v", err)
	}
	if err == nil && len(entries) > 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dry-run must not write inbox/product-hypotheses/, got %v", names)
	}
}

func expectedChangeForProduct(productID string) string {
	switch productID {
	case "product:gamma-app":
		return "Pro IAP price step"
	case "product:alpha-app":
		return "After public release, the angler-intent subtitle"
	case "product:beta-app":
		return "Google Search Console report snapshot observed"
	}
	return ""
}

// stripDraftMetadata returns (productID, changeEvent) the proposal carries
// inside RequiredInputs. Mirrors the round-trip the apply-side
// mintDraftCandidatesForApply performs.
func stripDraftMetadata(cand candidate) (string, string) {
	var productID, changeEvent string
	for _, line := range cand.RequiredInputs {
		switch {
		case strings.HasPrefix(line, "product_id: "):
			productID = strings.TrimSpace(strings.TrimPrefix(line, "product_id: "))
		case strings.HasPrefix(line, "change_event: "):
			changeEvent = strings.TrimSpace(strings.TrimPrefix(line, "change_event: "))
		}
	}
	return productID, changeEvent
}

// seedStarvedProduct writes JSONL signal rows for one product so the loop
// model sees ONE signals_without_hypothesis group with >= `len(signalIDs)`
// signals. It mirrors what `apply-product-signals` does after a real ASC/GSC
// import — the lighter shape lets us stage products without a full signal
// pipeline.
func seedStarvedProduct(t *testing.T, home, productID, changeEvent, metric string, signalIDs []string, now time.Time) {
	t.Helper()
	logDir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "product-signals.jsonl")
	var lines []string
	for i, sid := range signalIDs {
		at := now.Add(time.Duration(i) * 24 * time.Hour).UTC().Format(time.RFC3339)
		row := map[string]any{
			"schema_version": product.SignalSchemaVersion,
			"id":             sid,
			"product_id":     productID,
			"metric_name":    metric,
			"before":         100,
			"after":          80,
			"observed_at":    at,
			"change_event":   changeEvent,
		}
		body, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(body))
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
