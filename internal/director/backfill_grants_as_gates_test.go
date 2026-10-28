package director

// Slice 3 (grants-as-gates, 2026-07-10): backfill fixture. Tonight's two
// binding ratifications become the first live records:
//   - direction-freeze-2026-07-10 (scope: new-organ/absorption imports)
//   - director-apply-grant-2026-07-10 (scope: director-apply)
//
// This test encodes both records and exercises the FULL path in a temp
// home — it does NOT write the real COMPOUND_HOME. The lander runs the
// live-proof commands post-land:
//
//   compound decisions record --kind grant \
//     --id director-apply-grant-2026-07-10 \
//     --title "Grant director --apply" \
//     --scope director-apply \
//     --reviewed-by <operator>
//
//   compound decisions record --kind freeze \
//     --id direction-freeze-2026-07-10 \
//     --title "Freeze new-organ/absorption imports" \
//     --keyword absorption_debt --keyword harness_edit \
//     --reviewed-by <operator>

import (
	"strings"
	"testing"

	"compound/internal/authority"
	"compound/internal/decisions"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestBackfillGrantsAsGatesTonightsTwoRecords(t *testing.T) {
	home := seededDirectorHome(t)

	memStore, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decisions.Record(memStore, home, decisions.RecordInput{
		Kind:       decisions.KindGrant,
		ID:         "director-apply-grant-2026-07-10",
		Title:      "Grant director --apply",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        directorTestNow,
	}); err != nil {
		t.Fatalf("record director-apply grant: %v", err)
	}
	if _, err := decisions.Record(memStore, home, decisions.RecordInput{
		Kind:       decisions.KindFreeze,
		ID:         "direction-freeze-2026-07-10",
		Title:      "Freeze new-organ/absorption imports",
		ReviewedBy: "operator:test",
		Keywords:   []string{"absorption_debt", "harness_edit"},
		Now:        directorTestNow,
	}); err != nil {
		t.Fatalf("record direction freeze: %v", err)
	}

	// A smoothness/wiring candidate (review debt) and a new-organ/absorption
	// candidate (absorption debt row) in the same run.
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-backfill-01")
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_test_wrapper", "cli:test wrapper", "wrapper",
			"compound://registries/test-owner", "test consumer", "asserted", "gate text"),
	})

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatalf("director --apply must run once the grant is recorded: %v", err)
	}
	if !strings.Contains(report.GrantRef, "director-apply-grant-2026-07-10") {
		t.Fatalf("run report must cite the recorded grant, got %q", report.GrantRef)
	}
	reviewProp := findProposalByKind(report, "review_debt")
	absorptionProp := findProposalByKind(report, "absorption_debt")
	if reviewProp == nil || absorptionProp == nil {
		t.Fatalf("expected both a review_debt and an absorption_debt proposal: %+v", report.Proposals)
	}
	if reviewProp.AutoAdmit != nil || absorptionProp.AutoAdmit != nil {
		t.Fatalf("director-apply grant files proposals but must not create cross-class autonomy")
	}

	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	// Materialize + attempt auto-admit on the absorption_debt (new-organ/
	// absorption import) proposal: the active freeze must park it.
	admittedAbsorption, err := store.AdmitCreatePlanPath(absorptionProp.PlanPath, directorTestNow.Add(1), work.AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.AdmitAuto(admittedAbsorption.WorkID, work.AutoAdmitInput{By: "director"}, directorTestNow.Add(2))
	if err == nil || res.Admitted {
		t.Fatalf("new-organ/absorption proposal must be denied by the active freeze: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "direction-freeze-2026-07-10") {
		t.Fatalf("denial must name the freeze record, got %v", err)
	}
	if got, _ := store.Get(admittedAbsorption.WorkID); got.State != "proposed" {
		t.Fatalf("frozen absorption_debt work must stay proposed, got %q", got.State)
	}

	// Materialize the review_debt proposal. It matches no freeze, but the broad
	// director-apply grant still cannot admit it: only a typed class grant and
	// evidence artifact may do that. The operator can always admit manually.
	admittedReview, err := store.AdmitCreatePlanPath(reviewProp.PlanPath, directorTestNow.Add(3), work.AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	resReview, err := store.AdmitAuto(admittedReview.WorkID, work.AutoAdmitInput{By: "director"}, directorTestNow.Add(4))
	if err == nil || resReview.Admitted {
		t.Fatalf("broad filing grant must not auto-admit review_debt: err=%v res=%+v", err, resReview)
	}
	if got, _ := store.Get(admittedReview.WorkID); got.State != "proposed" {
		t.Fatalf("review_debt work should remain proposed, got %q", got.State)
	}
	if _, err := store.Advance(admittedReview.WorkID, work.AdvanceInput{Actor: "operator-test", To: "admitted"}, directorTestNow.Add(5)); err != nil {
		t.Fatalf("manual operator admission must remain available: %v", err)
	}
}
