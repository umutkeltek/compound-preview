package product

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/home"
)

// contentStalenessTestNow is the deterministic clock the F3 falsifier uses
// so timestamp-derived ids don't drift across runs.
var contentStalenessTestNow = time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)

// writeContentStalenessModelCatalog installs a small synthetic provider
// catalog into a fresh test home, with a deprecated model
// ("legacy-model", superseded by "alpha-flash") the F3 draft body will name.
// The fixture mirrors the slice-1/2 query test so the staleness gate
// speaks the SAME source authority the doctor speaks.
func writeContentStalenessModelCatalog(t *testing.T, homePath string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "provider-registry.json")
	body := `{
  "schema": "examplecorp/provider-registry@3",
  "models": [
    {
      "provider_id": "examplecorp",
      "model_id": "alpha-flash",
      "display_name": "Alpha Flash",
      "upstream_model_ids": ["alpha-flash-001"],
      "input_micros_per_mtok": 140000,
      "output_micros_per_mtok": 560000,
      "context_window": 128000,
      "tier": "standard",
      "source_url": "https://catalog.invalid/alpha-flash",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "legacy-model",
      "display_name": "Legacy Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 50000,
      "output_micros_per_mtok": 150000,
      "context_window": 8000,
      "tier": "legacy",
      "source_url": "",
      "workload_kind": "chat"
    }
  ],
  "deprecated": [
    {
      "model": "legacy-model",
      "providers": ["examplecorp"],
      "status": "deprecated",
      "note": "superseded by alpha-flash"
    }
  ]
}
`
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ImportModelCatalog(homePath, source, contentStalenessTestNow); err != nil {
		t.Fatalf("import model catalog: %v", err)
	}
}

// TestContentStalenessGateFlagsSupersededModelName is F3's red side: a
// content draft body that NAMES a model the installed catalog marks
// deprecated must surface a visible warning. The packet calls this out
// as a flag (NOT a hard refusal) so the operator still owns the post.
//
// RED at base: ContentDraftWarning and ContentQueueRow.Warnings do not
// exist; the staleness check is absent; the draft body is happily
// accepted without a flag. Green requires a visible warning on the
// draft AND on the corresponding queue plan row.
func TestContentStalenessGateFlagsSupersededModelName(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentStalenessTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	writeContentStalenessModelCatalog(t, homePath)

	// The body names the deprecated model by model_id; the staleness gate
	// must look at the BODY (not the evidence_refs or voice pack) because
	// the operator's post text is what we want to flag.
	supersededBody := "ship-day post for prod_synth_a — use legacy-model for the cheap draft"
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         supersededBody,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    contentStalenessTestNow.Add(72 * time.Hour),
	}, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	// The draft record itself must carry the flag.
	if len(draft.Warnings) == 0 {
		t.Fatal("expected the draft to carry a staleness warning naming legacy-model")
	}
	if !warningMentionsModel(draft.Warnings, "legacy-model") {
		t.Fatalf("draft warnings must name the superseded model, got %+v", draft.Warnings)
	}
	// Re-read from disk to prove the flag was persisted, not just returned
	// in memory (the falsifier guards against an in-memory-only check that
	// the persisted record fails to carry).
	persisted, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("re-read draft: %v", err)
	}
	if len(persisted.Warnings) == 0 {
		t.Fatal("persisted draft must carry the staleness warning, got none")
	}
	if !warningMentionsModel(persisted.Warnings, "legacy-model") {
		t.Fatalf("persisted warnings must name the superseded model, got %+v", persisted.Warnings)
	}

	// The content-queue plan row must ALSO carry the flag, because the
	// operator reads the plan, not the underlying draft, when adjudicating
	// weekly posts.
	plan, err := BuildContentQueuePlan(homePath, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("build queue plan: %v", err)
	}
	if len(plan.Rows) != 1 {
		t.Fatalf("plan rows = %d, want 1, got %+v", len(plan.Rows), plan.Rows)
	}
	if len(plan.Rows[0].Warnings) == 0 {
		t.Fatal("queue plan row must carry the staleness warning for the superseded-model draft")
	}
	if !warningMentionsModel(plan.Rows[0].Warnings, "legacy-model") {
		t.Fatalf("queue plan row warnings must name the superseded model, got %+v", plan.Rows[0].Warnings)
	}
	// The plan must NOT refuse the draft — staleness is a flag, not a
	// hard refusal. The status remains "review_required" so the operator
	// can still approve/reject/hold.
	if plan.Status != "review_required" {
		t.Fatalf("staleness gate must not refuse the plan; status = %q, want review_required", plan.Status)
	}
}

// TestContentStalenessGateIsSilentOnCurrentModelName is F3's green side:
// a content draft body that NAMES a current model must NOT carry a
// staleness warning. A red result here means the gate is misfiring on
// legitimate posts and would flood the operator's review queue with
// false positives.
func TestContentStalenessGateIsSilentOnCurrentModelName(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentStalenessTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	writeContentStalenessModelCatalog(t, homePath)

	// The body names a current model — the staleness gate MUST be silent.
	currentBody := "ship-day post for prod_synth_a — use alpha-flash for the standard tier"
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         currentBody,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    contentStalenessTestNow.Add(72 * time.Hour),
	}, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	if len(draft.Warnings) != 0 {
		t.Fatalf("current-model draft must not carry staleness warnings, got %+v", draft.Warnings)
	}

	plan, err := BuildContentQueuePlan(homePath, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("build queue plan: %v", err)
	}
	if len(plan.Rows) != 1 {
		t.Fatalf("plan rows = %d, want 1, got %+v", len(plan.Rows), plan.Rows)
	}
	if len(plan.Rows[0].Warnings) != 0 {
		t.Fatalf("queue plan row must not carry staleness warnings for current-model drafts, got %+v", plan.Rows[0].Warnings)
	}
}

// TestContentStalenessGateFlagsProviderPrefixMatch is a stronger version
// of F3: the body names a model with its provider prefix
// ("examplecorp/legacy-model"), the gate must still catch it. A red
// result here means the gate's name-matching regex/token logic is too
// narrow and operators who follow the "provider/model" convention slip
// through the flag.
func TestContentStalenessGateFlagsProviderPrefixMatch(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentStalenessTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	writeContentStalenessModelCatalog(t, homePath)

	body := "switching off examplecorp/legacy-model and trying alpha-flash instead"
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         body,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    contentStalenessTestNow.Add(72 * time.Hour),
	}, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	if !warningMentionsModel(draft.Warnings, "legacy-model") {
		t.Fatalf("provider-prefixed body must still flag the superseded model, got %+v", draft.Warnings)
	}
}

// TestContentStalenessGateIsSilentWhenNoCatalogInstalled is the
// no-installed-catalog edge: a draft recorded before any
// import-model-catalog run must NOT crash and must NOT spuriously flag.
// A red result here means the gate hard-requires the catalog, breaking
// the cold-start path.
func TestContentStalenessGateIsSilentWhenNoCatalogInstalled(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", contentStalenessTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	// No install. The body STILL names a model the operator wants to talk
	// about; the gate must be silent because it has no installed truth
	// to consult.
	body := "post about legacy-model and alpha-flash"
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         body,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    contentStalenessTestNow.Add(72 * time.Hour),
	}, contentStalenessTestNow)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	if len(draft.Warnings) != 0 {
		t.Fatalf("no-catalog case must not flag, got %+v", draft.Warnings)
	}
}

// warningMentionsModel is a small helper so each test asserts the warning
// text actually names the model id (not just any warning).
func warningMentionsModel(warnings []string, modelID string) bool {
	for _, w := range warnings {
		if strings.Contains(w, modelID) {
			return true
		}
	}
	return false
}

// guard against unused imports if a future edit removes one of the test
// helpers above.
var _ = context.Background
