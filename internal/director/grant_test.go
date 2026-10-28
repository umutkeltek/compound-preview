package director

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/decisions"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestResolveApplyGrantPreservesLegacyRecordedGrant(t *testing.T) {
	home := seededDirectorHome(t)
	legacy := mem.Claim{
		ID:         "claim_legacy_director_apply",
		Seq:        1,
		Entity:     "decision:legacy-director-apply",
		Predicate:  "decided",
		Object:     "Legacy director apply grant",
		ClaimType:  "decision",
		Visibility: "operational",
		Qualifiers: map[string]string{
			"kind":             decisions.KindGrant,
			"source":           "decisions-repo",
			"decision_id":      "legacy-director-apply",
			"lifecycle_status": decisions.StatusAccepted,
			"reviewed_by":      "operator:legacy",
			"scope":            directorApplyGrantScope,
		},
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ledger", "claims.jsonl"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	grant, err := resolveApplyGrant(home, directorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if grant.DecisionID != "legacy-director-apply" || grant.ReviewedBy != "operator:legacy" {
		t.Fatalf("legacy recorded grant did not survive authority-stamp migration: %+v", grant)
	}
}

func TestResolveApplyGrantRejectsPersistedNonOperatorAuthority(t *testing.T) {
	home := seededDirectorHome(t)
	forged := mem.Claim{
		ID:         "claim_persisted_non_operator_director_apply",
		Seq:        1,
		Entity:     "decision:persisted-non-operator-director-apply",
		Predicate:  "decided",
		Object:     "Persisted non-operator director apply grant",
		ClaimType:  "decision",
		Visibility: "operational",
		Qualifiers: map[string]string{
			"kind":             decisions.KindGrant,
			"source":           "decisions-repo",
			"decision_id":      "persisted-non-operator-director-apply",
			"lifecycle_status": decisions.StatusAccepted,
			"reviewed_by":      "codex",
			"scope":            directorApplyGrantScope,
			mem.OperationalDecisionAuthorityQualifier: mem.OperationalDecisionAuthorityValue,
		},
	}
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ledger", "claims.jsonl"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if grant, err := resolveApplyGrant(home, directorTestNow); err == nil || grant.DecisionID != "" {
		t.Fatalf("persisted non-operator authority released director apply: grant=%+v err=%v", grant, err)
	}
}

// TestDirectorApplyRefusesWithoutGrant is falsifier a (red half): director
// --apply with no active director-apply grant record is refused BEFORE
// filing anything, naming the missing grant. Dry-run stays unaffected — only
// --apply requires the grant.
func TestDirectorApplyRefusesWithoutGrant(t *testing.T) {
	home := seededDirectorHome(t)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-grant-01")

	_, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err == nil {
		t.Fatal("director --apply with no grant record must be refused")
	}
	if !strings.Contains(err.Error(), "director-apply") {
		t.Fatalf("refusal must name the missing director-apply grant, got %v", err)
	}

	store, openErr := work.Open(home)
	if openErr != nil {
		t.Fatal(openErr)
	}
	works, listErr := store.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("refused apply must file nothing, found %d work item(s)", len(works))
	}

	// Dry-run never requires a grant.
	dry, dryErr := Propose(home, Options{Apply: false, Now: directorTestNow})
	if dryErr != nil {
		t.Fatalf("dry run must not require a grant: %v", dryErr)
	}
	if dry.ProposalCount != 1 {
		t.Fatalf("dry run should still see the review-debt candidate: %+v", dry)
	}
}

// TestDirectorApplyWithGrantRunsAndCitesIt is falsifier a (green half): once
// an active director-apply grant record exists, --apply runs and cites the
// grant — in the report and in the run WorkEnvelope's evidence refs. This
// broad filing grant is deliberately NOT an autonomy grant; a review_debt
// proposal receives no AutoAdmitInput.
func TestDirectorApplyWithGrantRunsAndCitesIt(t *testing.T) {
	home := seededDirectorHome(t)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-grant-02")

	memStore, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	grantReport, err := decisions.Record(memStore, home, decisions.RecordInput{
		Kind:       decisions.KindGrant,
		ID:         "director-apply-grant-2026-07-10",
		Title:      "Grant director --apply",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        directorTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grantReport.Import.ClaimID == "" {
		t.Fatalf("grant record should promote: %+v", grantReport.Import)
	}

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow.Add(1)})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 1 {
		t.Fatalf("expected one proposal, got %+v", report)
	}
	if report.GrantRef == "" || !strings.Contains(report.GrantRef, "director-apply-grant-2026-07-10") {
		t.Fatalf("run report must cite the grant, got GrantRef=%q", report.GrantRef)
	}
	proposal := report.Proposals[0]
	if proposal.AutoAdmit != nil || proposal.EligibilityRef != "" {
		t.Fatalf("director-apply grant must not masquerade as class autonomy: %+v", proposal)
	}

	// The grant is also cited in the run's own WorkEnvelope evidence, not just
	// the JSON report — a real receipt, not a dead struct.
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	runWork, err := store.Get(report.RunWorkID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range runWork.EvidenceRefs {
		if strings.Contains(ref, "director-apply-grant-2026-07-10") {
			found = true
		}
	}
	if !found {
		t.Fatalf("run work evidence refs must cite the grant, got %v", runWork.EvidenceRefs)
	}
}

// TestDirectorApplyGrantMustNotBeSuperseded proves an undone (superseded)
// grant no longer satisfies apply — the operator must record a fresh grant
// (falsifier c wired through director consumption).
func TestDirectorApplyGrantMustNotBeSuperseded(t *testing.T) {
	home := seededDirectorHome(t)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-grant-03")

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
		t.Fatal(err)
	}
	if _, err := decisions.Record(memStore, home, decisions.RecordInput{
		Kind:       decisions.KindGrant,
		ID:         "director-apply-grant-2026-07-10-revoke",
		Title:      "Revoke director --apply grant",
		ReviewedBy: "operator:test",
		UndoOf:     "director-apply-grant-2026-07-10",
		Now:        directorTestNow.Add(1),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = Propose(home, Options{Apply: true, Now: directorTestNow.Add(2)})
	if err == nil {
		t.Fatal("director --apply must refuse once the only grant is superseded")
	}
}
