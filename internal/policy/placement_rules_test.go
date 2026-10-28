package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlacementRulesContext(t *testing.T) {
	homeRoot := t.TempDir()
	t.Setenv("HOME", homeRoot)
	rules := PlacementRules{
		OpenWriteGlobs: []string{"$HOME/open-write/**"},
		Owners: []PlacementOwnerRule{{
			ID:                  "decisions",
			Owner:               "decisions",
			Status:              "active",
			CanonicalWriteGlobs: []string{"$HOME/owners/decisions/**"},
			DenyGlobs:           []string{"$HOME/Desktop/decisions/**"},
			CanonFilenameGlobs:  []string{"DEC-*.md"},
			Confidence:          "block",
		}},
	}

	if ctx := rules.PlacementContext(filepath.Join(homeRoot, "owners", "decisions", "DEC-0001.md")); ctx != nil {
		t.Fatalf("canonical owner home should not trigger placement guard: %+v", ctx)
	}

	if ctx := rules.PlacementContext(filepath.Join(homeRoot, "open-write", "DEC-0001.md")); ctx != nil {
		t.Fatalf("open-write should suppress placement context, got %+v", ctx)
	}

	wrongHome := rules.PlacementContext(filepath.Join(homeRoot, "notes", "DEC-0001.md"))
	if wrongHome == nil || wrongHome.Reason != "canon-filename-outside-owner-home" || wrongHome.OwnerRef != "placement-owner:decisions" {
		t.Fatalf("canon filename outside owner home should trigger owner context, got %+v", wrongHome)
	}
	if got := rules.BlockOwnerIDs(); len(got) != 1 || got[0] != "decisions" {
		t.Fatalf("block owner ids = %v, want [decisions]", got)
	}

	deny := rules.PlacementContext(filepath.Join(homeRoot, "Desktop", "decisions", "draft.md"))
	if deny == nil || deny.Reason != "deny-zone" {
		t.Fatalf("deny glob should trigger deny-zone, got %+v", deny)
	}
}

// TestPlacementRulesDecisionOwnerRedirectsToCompoundDoor is the Wave-2 write-surface falsifier: a
// DEC-shaped write intercepted for the `decisions` placement owner must redirect to compound's OWN
// governed door (inbox/decision-candidates, consumed by `compound decisions candidate-plan`), never
// to the bridge canonical_write_glob an imported OS source still carries
// (~/Projects/operating-system/decisions). Doors-by-attraction: the guard names compound's intake
// regardless of the imported glob, and the bridge glob stays in the registry (keep-all-data), so
// legacy brain/OS homes remain valid canon. RED before the governed-door override (redirect deglobs
// to the OS path); GREEN after.
func TestPlacementRulesDecisionOwnerRedirectsToCompoundDoor(t *testing.T) {
	homeRoot := t.TempDir()
	t.Setenv("HOME", homeRoot)
	rules := PlacementRules{
		Owners: []PlacementOwnerRule{{
			ID:                  "decisions",
			Owner:               "decisions",
			Status:              "active",
			CanonicalWriteGlobs: []string{"~/Projects/operating-system/decisions/**", "~/brain/decisions/**"},
			DenyGlobs:           []string{"~/Desktop/decisions/**"},
			CanonFilenameGlobs:  []string{"DEC-*.md"},
			Confidence:          "block",
		}},
	}

	// canon-filename-outside-owner-home: a synthetic DEC write in an unclaimed location.
	ctx := rules.PlacementContext(filepath.Join(homeRoot, "Documents", "DEC-9999.md"))
	if ctx == nil || ctx.Reason != "canon-filename-outside-owner-home" {
		t.Fatalf("synthetic DEC write should trigger canon-filename context, got %+v", ctx)
	}
	if !strings.Contains(ctx.RedirectTarget, "inbox/decision-candidates") {
		t.Fatalf("redirect must name the compound governed door inbox/decision-candidates, got %q", ctx.RedirectTarget)
	}
	if !strings.Contains(ctx.RedirectTarget, "compound decisions candidate-plan") {
		t.Fatalf("redirect must name the door consumer `compound decisions candidate-plan`, got %q", ctx.RedirectTarget)
	}
	if strings.Contains(ctx.RedirectTarget, "operating-system") {
		t.Fatalf("redirect must NOT name the bridge OS decisions path, got %q", ctx.RedirectTarget)
	}

	// A deny-zone DEC write for the same owner also names the compound door.
	deny := rules.PlacementContext(filepath.Join(homeRoot, "Desktop", "decisions", "DEC-1.md"))
	if deny == nil || deny.Reason != "deny-zone" {
		t.Fatalf("deny-zone DEC write should trigger deny-zone context, got %+v", deny)
	}
	if strings.Contains(deny.RedirectTarget, "operating-system") || !strings.Contains(deny.RedirectTarget, "inbox/decision-candidates") {
		t.Fatalf("deny-zone redirect must name the compound door, got %q", deny.RedirectTarget)
	}
}

// TestPlacementRulesGovernedRoot proves the open-world fix: a novel filename in an unclaimed
// location under a governed root is flagged. This FAILS on pre-fix code (no GovernedRoots ->
// PlacementContext returns nil -> the ad-hoc write sails through, exactly the itch).
func TestPlacementRulesGovernedRoot(t *testing.T) {
	homeRoot := t.TempDir()
	rules := PlacementRules{
		GovernedRoots: []GovernedRoot{{
			ID:         "compound-home",
			RootGlobs:  []string{filepath.Join(homeRoot, "**")},
			AllowGlobs: []string{filepath.Join(homeRoot, "inbox", "**"), filepath.Join(homeRoot, "reviews", "**")},
			Confidence: "block",
			Redirect:   "compound placement target --kind operational-finding",
		}},
	}

	// The itch: an invented path under COMPOUND_HOME that owns no typed habitat.
	ctx := rules.PlacementContext(filepath.Join(homeRoot, "private", "stray-note.md"))
	if ctx == nil {
		t.Fatal("unowned write under governed root must be flagged (open-world hole)")
	}
	if ctx.Reason != "unowned-under-governed-root" {
		t.Fatalf("reason = %q, want unowned-under-governed-root", ctx.Reason)
	}
	if ctx.RedirectTarget == "" {
		t.Fatal("governed-root context must carry a redirect target")
	}
	if ctx.Confidence != "block" {
		t.Fatalf("confidence = %q, want block", ctx.Confidence)
	}

	// A write into a typed habitat dir is allowed (not scaffolding).
	if ctx := rules.PlacementContext(filepath.Join(homeRoot, "inbox", "memory-candidates", "m.json")); ctx != nil {
		t.Fatalf("typed habitat write must not be flagged: %+v", ctx)
	}
}

// TestPlacementContextFromHomeSynthesizesGovernedRoot proves "works from nothing": with NO
// registry installed, a fresh COMPOUND_HOME still protects itself, and its typed habitat dirs
// are allowed.
func TestPlacementContextFromHomeSynthesizesGovernedRoot(t *testing.T) {
	homePath := t.TempDir()

	ctx, refs, err := PlacementContextFromHome(homePath, filepath.Join(homePath, "private", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil || ctx.Reason != "unowned-under-governed-root" {
		t.Fatalf("fresh home must protect itself with no registry, got %+v", ctx)
	}
	if len(refs) == 0 {
		t.Fatal("expected placement authority ref")
	}

	// A typed habitat dir under the same fresh home must be allowed.
	ok, _, err := PlacementContextFromHome(homePath, filepath.Join(homePath, "reviews", "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ok != nil {
		t.Fatalf("typed habitat under fresh home must be allowed: %+v", ok)
	}

	// A cataloged bridge is preserved by doctor/backup but is not a canonical
	// placement target. New writes remain redirected until its consumer cutover.
	bridge, _, err := PlacementContextFromHome(homePath, filepath.Join(homePath, "close-plans", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bridge == nil || bridge.Reason != "unowned-under-governed-root" {
		t.Fatalf("legacy bridge must remain non-canonical for new writes: %+v", bridge)
	}
}

func TestImportPlacementRulesWritesAuthorityAndSourceRecord(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "placement-rules.json")
	body := []byte(`{
  "open_write_globs": ["$HOME/scratch/**"],
  "owners": [{
    "id": "docs",
    "owner": "docs",
    "status": "active",
    "canonical_write_globs": ["$HOME/owners/docs/**"],
    "canon_filename_globs": ["DOC-*.md"],
    "confidence": "warn"
  }]
}`)
	if err := os.WriteFile(sourcePath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ImportPlacementRules(homePath, sourcePath, fixedNow)
	if err != nil {
		t.Fatalf("import placement rules: %v", err)
	}
	if report.AuthorityRef != PlacementRulesAuthorityRef || report.OwnerCount != 1 || report.OpenWriteCount != 1 {
		t.Fatalf("unexpected import report: %+v", report)
	}
	if got := readPolicyTestFile(t, PlacementRulesPath(homePath)); string(got) != string(body) {
		t.Fatalf("authority body changed:\n%s", got)
	}
	records, err := LoadPlacementSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("source authority records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.AuthorityStatus != "compound_primary" || rec.SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("bad source authority role/status: %+v", rec)
	}
	if !strings.Contains(rec.Retires, sourcePath) || !strings.Contains(rec.SourceRevision, "sha256:") {
		t.Fatalf("source authority missing retire/revision fields: %+v", rec)
	}
	if len(rec.OwnerIDs) != 1 || rec.OwnerIDs[0] != "docs" {
		t.Fatalf("source authority owner ids = %+v", rec.OwnerIDs)
	}
}

func TestLoadPlacementRulesRejectsInvalidConfidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placement-rules.json")
	raw, err := json.Marshal(PlacementRules{Owners: []PlacementOwnerRule{{
		ID:         "bad",
		Confidence: "panic",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadPlacementRules(path)
	if err == nil || !strings.Contains(err.Error(), "invalid confidence") {
		t.Fatalf("expected invalid confidence error, got %v", err)
	}
	if _, err := ImportPlacementRules(t.TempDir(), path, fixedNow); err == nil || !strings.Contains(err.Error(), "invalid confidence") {
		t.Fatalf("expected import invalid confidence error, got %v", err)
	}
}

func readPolicyTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
