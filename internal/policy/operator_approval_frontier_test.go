package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportOperatorApprovalFrontierWritesAuthority(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "operator-approval-frontier-summary.json")
	raw := []byte(`{
		"schema_version": 1,
		"generated_at": "2026-07-01T12:00:00Z",
		"run_id": "wave1-operator-approval-frontier-fixture",
		"status": "operator_approval_frontier_ready",
		"approval_required_open_count": 2,
		"families": [
			{
				"id": "operator-decision-prompt-cards",
				"current_decision": "defer",
				"recommended_decision": "see decision brief",
				"exact_operator_phrase": "I approve ` + "`see decision brief`" + ` for ` + "`operator-decision-prompt-cards`" + ` only."
			},
			{
				"id": "provider-cost-coverage",
				"current_decision": "approve",
				"recommended_decision": "already consumed"
			}
		]
	}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ImportOperatorApprovalFrontier(homePath, sourcePath, fixedNow)
	if err != nil {
		t.Fatalf("import operator approval frontier: %v", err)
	}
	if report.AuthorityRef != OperatorApprovalFrontierAuthorityRef || report.FamilyCount != 2 || report.DeferredCount != 1 || report.ApprovalRequiredOpenCount != 2 {
		t.Fatalf("bad import report: %+v", report)
	}
	if _, err := os.Stat(OperatorApprovalFrontierPath(homePath)); err != nil {
		t.Fatalf("operator approval frontier authority missing: %v", err)
	}
	loaded, err := LoadOperatorApprovalFrontierFromHome(homePath)
	if err != nil {
		t.Fatalf("load operator approval frontier: %v", err)
	}
	if len(loaded.Families) != 2 || loaded.Families[0].ID != "operator-decision-prompt-cards" {
		t.Fatalf("bad loaded families: %+v", loaded.Families)
	}
	records, err := LoadOperatorApprovalFrontierSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 || records[0].AuthorityStatus != "compound_primary" || records[0].SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("bad source authority records: %+v", records)
	}
	if len(records[0].FamilyIDs) != 2 || records[0].FamilyIDs[0] != "operator-decision-prompt-cards" {
		t.Fatalf("bad family ids: %+v", records[0].FamilyIDs)
	}
}

func TestImportOperatorApprovalFrontierRejectsMissingStatus(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "operator-approval-frontier-summary.json")
	raw := []byte(`{"families":[{"id":"operator-decision-prompt-cards"}]}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ImportOperatorApprovalFrontier(homePath, sourcePath, fixedNow); err == nil {
		t.Fatal("expected invalid frontier to fail")
	}
	if _, err := os.Stat(OperatorApprovalFrontierPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("authority should not be written on invalid input: %v", err)
	}
}
