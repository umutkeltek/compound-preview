package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportOperatorPromptsWritesAuthority(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "operator-decision-prompts.json")
	raw := []byte(`{
		"version": 1,
		"generated_at": "2026-07-01T12:00:00Z",
		"delivery_mode": "cards_only_no_direct_send",
		"prompts": [
			{
				"id": "prompt-one",
				"severity": "high",
				"question": "Approve the local-only packet draft?",
				"source": "fixture"
			}
		]
	}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ImportOperatorPrompts(homePath, sourcePath, fixedNow)
	if err != nil {
		t.Fatalf("import operator prompts: %v", err)
	}
	if report.AuthorityRef != OperatorPromptsAuthorityRef || report.PromptCount != 1 || report.HighSeverityCount != 1 {
		t.Fatalf("bad import report: %+v", report)
	}
	if _, err := os.Stat(OperatorPromptsPath(homePath)); err != nil {
		t.Fatalf("operator prompts authority missing: %v", err)
	}
	loaded, err := LoadOperatorPromptsFromHome(homePath)
	if err != nil {
		t.Fatalf("load operator prompts: %v", err)
	}
	if len(loaded.Prompts) != 1 || loaded.Prompts[0].ID != "prompt-one" {
		t.Fatalf("bad loaded prompts: %+v", loaded.Prompts)
	}
	records, err := LoadOperatorPromptsSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 || records[0].AuthorityStatus != "compound_primary" || records[0].SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("bad source authority records: %+v", records)
	}
	if len(records[0].PromptIDs) != 1 || records[0].PromptIDs[0] != "prompt-one" {
		t.Fatalf("bad prompt ids: %+v", records[0].PromptIDs)
	}
}

func TestImportOperatorPromptsRejectsPromptWithoutQuestion(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "operator-decision-prompts.json")
	raw := []byte(`{"prompts":[{"id":"bad"}]}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ImportOperatorPrompts(homePath, sourcePath, fixedNow); err == nil {
		t.Fatal("expected invalid prompt to fail")
	}
	if _, err := os.Stat(OperatorPromptsPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("authority should not be written on invalid input: %v", err)
	}
}
