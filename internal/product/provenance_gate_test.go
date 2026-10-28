package product

// Falsifiers for the product-review provenance gate (packet
// 2026-07-10-product-review-provenance-gate). Each test fails before its slice
// lands: it asserts that a product judgment record refuses to exist — or to be
// treated as an operator adjudication — without a named actor, mirroring the
// panel verdict provenance rule (664e1a7 / 2026-07-02 ruling).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/mem"
)

// validActionReviewWithActor mirrors the action_plan_test fixup but with a named actor.
func validActionReviewWithActor(actor string) ActionReview {
	return ActionReview{
		SchemaVersion: ActionReviewSchemaVersion,
		ID:            "pacr_input_x",
		PlanID:        "plan_x",
		ProductID:     "product:x",
		CardID:        "card_x",
		Decision:      "propose_work",
		Rationale:     "reviewed action card before any work admission",
		Confidence:    0.7,
		EvidenceRefs:  []string{"product_action_card:card_x"},
		ReviewedBy:    actor,
	}
}

// Falsifier (b): ValidateActionReview refuses a record with no reviewed_by.
func TestValidateActionReviewRequiresReviewedBy(t *testing.T) {
	ok := validActionReviewWithActor("operator")
	if err := ValidateActionReview(ok); err != nil {
		t.Fatalf("action review with reviewed_by should validate: %v", err)
	}
	missing := validActionReviewWithActor("")
	if err := ValidateActionReview(missing); err == nil {
		t.Fatal("action review without reviewed_by must be refused (unattributed judgment)")
	}
}

// seedFocusHypothesis imports the market-loop fixture so a reviewable
// hypothesis (hyp_focus_visuals_conversion / product:focus-notes) exists.
func seedFocusHypothesis(t *testing.T) (string, *mem.Store) {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	return home, store
}

func readProductReviewRecordByReviewID(t *testing.T, home, reviewID string) ReviewArtifactRecord {
	t.Helper()
	dir := filepath.Join(home, "reviews", "product-reviews")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read product-reviews dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !bytes.HasSuffix([]byte(e.Name()), []byte(".reviewed.json")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec ReviewArtifactRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.ReviewID == reviewID {
			return rec
		}
	}
	t.Fatalf("no prr_ record found for review %s in %s", reviewID, dir)
	return ReviewArtifactRecord{}
}

// Falsifier (d): a provenanced review's actor lands in all three sinks —
// the prr_ record, the evidence entry, and the Transition producer.
func TestProductReviewProvenanceLandsInAllThreeSinks(t *testing.T) {
	home, store := seedFocusHypothesis(t)
	review, err := LoadReview("../../fixtures/product/review.yaml")
	if err != nil {
		t.Fatal(err)
	}
	review.ReviewedBy = "probe-lane"
	// sourcePath ends in .review.yaml so the prr_ record sink is exercised.
	report, err := ApplyReview(home, store, review, "fixture/focus-visuals.review.yaml", tProduct.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy != "probe-lane" {
		t.Fatalf("review report actor = %q, want probe-lane", report.ReviewedBy)
	}
	// sink 1: prr_ record.
	rec := readProductReviewRecordByReviewID(t, home, report.ReviewID)
	if rec.ReviewedBy != "probe-lane" {
		t.Fatalf("prr_ record reviewed_by = %q, want probe-lane", rec.ReviewedBy)
	}
	// sink 2: evidence entry.
	ev, ok, err := store.GetEvidence(report.EvidenceID)
	if err != nil || !ok {
		t.Fatalf("evidence %s not found (ok=%v err=%v)", report.EvidenceID, ok, err)
	}
	if ev.Actor != "probe-lane" {
		t.Fatalf("evidence actor = %q, want probe-lane", ev.Actor)
	}
	// sink 3: transition producer.
	tr := readTransition(t, home, report.TransitionID)
	if tr.Producer.LaneID != "probe-lane" {
		t.Fatalf("transition producer lane_id = %q, want probe-lane", tr.Producer.LaneID)
	}
}

// plantProductReviewRecord writes a byte-stable prr_ record for a product so
// the doctor read-side can be exercised without rewriting it.
func plantProductReviewRecord(t *testing.T, home, id, productID, actor string) string {
	t.Helper()
	dir := filepath.Join(home, "reviews", "product-reviews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := ReviewArtifactRecord{
		SchemaVersion: "compound.product_review_record.v0.1",
		ID:            id,
		CreatedAt:     "2026-07-02T09:00:00Z",
		Status:        "recorded",
		ProductID:     productID,
		HypothesisID:  "hyp_focus_visuals_conversion",
		ReviewID:      "review_" + id,
		Result:        "supported",
		ReviewedBy:    actor,
	}
	data, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".reviewed.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Falsifier (e): an agent-attributed judgment is an annotation, never an
// operator adjudication — it does not clear an operator-gated pending state.
func TestAgentAttributedProductJudgmentIsAnnotationNotAdjudication(t *testing.T) {
	if !OperatorAdjudicated("operator") {
		t.Fatal("operator actor must count as operator adjudication")
	}
	if OperatorAdjudicated("codex") {
		t.Fatal("agent lane actor must NOT count as operator adjudication (annotation only)")
	}
	if OperatorAdjudicated("") {
		t.Fatal("unattributed actor must NOT count as operator adjudication")
	}
	home, _ := seedFocusHypothesis(t)
	plantProductReviewRecord(t, home, "prr_agent_planted", "product:focus-notes", "codex")
	doc, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doc.AgentAnnotatedJudgments < 1 || !hasString(doc.AgentAnnotatedJudgmentIDs, "prr_agent_planted") {
		t.Fatalf("agent-attributed judgment must surface as operator-pending annotation: %+v", doc)
	}
	if hasString(doc.UnattributedJudgmentIDs, "prr_agent_planted") {
		t.Fatalf("agent record is attributed, must not be counted as unattributed: %+v", doc)
	}
}

// Falsifier (f): a null-actor record is counted as an unattributed judgment and
// is never rewritten on disk (honest history, no laundering).
func TestUnattributedProductJudgmentCountedWithoutRewrite(t *testing.T) {
	home, _ := seedFocusHypothesis(t)
	path := plantProductReviewRecord(t, home, "prr_4b69f447184d", "product:focus-notes", "")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doc.UnattributedJudgments < 1 || !hasString(doc.UnattributedJudgmentIDs, "prr_4b69f447184d") {
		t.Fatalf("null-actor record must be counted as unattributed judgment: %+v", doc)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("doctor must not rewrite existing null-actor records (no history laundering)")
	}
}
