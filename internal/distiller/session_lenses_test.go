package distiller

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSessionLensesCountsSixLensOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi-lens.json")
	writeFile(t, path, `{
  "decisions": [{"summary": "Keep extraction source-only.", "falsifier": "Claims are promoted directly."}],
  "outcomes": [{"summary": "One batch completed."}],
  "patterns": [{"summary": "Repeated recall miss."}, {"summary": "Operator valve respected."}],
  "skills": [{"name": "source bridge", "reason": "Reuse existing output."}],
  "taste": [{"summary": "Plain operator language."}],
  "contradictions": [{"summary": "Extraction exists but recall is not proven."}]
}`)

	got, err := LoadSessionLenses(path)
	if err != nil {
		t.Fatalf("load session lenses: %v", err)
	}
	if got.ID == "" || !strings.HasPrefix(got.ID, "session_lenses:") {
		t.Fatalf("id = %q", got.ID)
	}
	if got.TotalItems != 7 || got.Decisions != 1 || got.Patterns != 2 || got.Contradictions != 1 {
		t.Fatalf("lens counts = %+v", got)
	}
	if got.SourcePath != path || got.SourceDigest == "" || got.Revision != got.SourceDigest {
		t.Fatalf("source identity = %+v", got)
	}
	if !hasPrefix(got.EvidenceRefs, "session_lenses:sha256:") {
		t.Fatalf("evidence refs = %v", got.EvidenceRefs)
	}
}
