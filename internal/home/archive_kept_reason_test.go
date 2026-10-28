package home

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationalArchiveReportsReasonForKeptTerminalReview(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, homePath, "inbox/comparisons/cmp_kept.json", `{"id":"cmp_kept","state":"judged"}`)
	writeHomeFile(t, homePath, "reviews/panel-verdicts/panel_verdict_kept.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_kept","target":{"kind":"comparison","ref":"cmp_kept"},"synthesis":{"decision":"needs_human"}}`)
	writeHomeFile(t, homePath, "reviews/panel-verdicts/panel_verdict_kept.md", "# kept\n")

	report, err := ArchiveOperationalHistory(homePath, OperationalArchiveOptions{Kinds: []string{"review"}, DryRun: true}, archiveTestNow)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(data)
	for _, want := range []string{
		`"original_path":"reviews/panel-verdicts/panel_verdict_kept.json"`,
		`"kept_reason":"operator_pending_without_reviewed_by"`,
	} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("archive report missing %s: %s", want, serialized)
		}
	}
	if report.ArchivedCount != 0 || len(report.Entries) != 0 {
		t.Fatalf("kept review must not count as archived: %+v", report)
	}
}

func TestOperationalArchiveReportsReasonForEveryDeliberatelyKeptReview(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, homePath, "inbox/comparisons/cmp_kept_all.json", `{"id":"cmp_kept_all","state":"judged"}`)
	fixtures := map[string]string{
		"panel_verdict_needs_human": `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_needs_human","target":{"kind":"comparison","ref":"cmp_kept_all"},"synthesis":{"decision":"needs_human"}}`,
		"panel_verdict_needs_trial": `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_needs_trial","target":{"kind":"comparison","ref":"cmp_kept_all"},"synthesis":{"decision":"needs_trial"}}`,
		"panel_verdict_legacy":      `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_legacy","target":{"kind":"comparison","ref":"cmp_kept_all"},"synthesis":{"decision":"needs_human"}}`,
	}
	for id, body := range fixtures {
		writeHomeFile(t, homePath, "reviews/panel-verdicts/"+id+".json", body)
		writeHomeFile(t, homePath, "reviews/panel-verdicts/"+id+".md", "# kept\n")
	}
	writeHomeFile(t, homePath, "reviews/panel-verdicts/panel_verdict_legacy.reviewed.json", `{"schema_version":"legacy.review.v0.1","status":"recorded"}`)

	report, err := ArchiveOperationalHistory(homePath, OperationalArchiveOptions{Kinds: []string{"review"}, DryRun: true}, archiveTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Kept) != len(fixtures) {
		t.Fatalf("kept entries = %+v, want %d", report.Kept, len(fixtures))
	}
	seen := map[string]string{}
	for _, kept := range report.Kept {
		if kept.KeptReason == "" {
			t.Fatalf("kept terminal review missing reason: %+v", kept)
		}
		seen[filepath.Base(kept.OriginalPath)] = kept.KeptReason
	}
	for id := range fixtures {
		name := id + ".json"
		if seen[name] != "operator_pending_without_reviewed_by" {
			t.Fatalf("kept reason for %s = %q", name, seen[name])
		}
	}
	if report.ArchivedCount != 0 || len(report.Entries) != 0 {
		t.Fatalf("deliberately kept reviews leaked into archive entries: %+v", report)
	}
}
