package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTastePlanRecordsReviewArtifactsWithoutMutatingTarget(t *testing.T) {
	home, store := setup(t)
	trace := `{
 "trace_ref": "trace_taste_standard",
 "candidates": [{
  "kind": "taste",
  "subject": "product:sample-mobile-app:visuals",
  "statement": "screenshots should feel native, specific, and inspection-ready rather than generic marketing cards",
  "detail": "app_store_visuals",
  "confidence": 0.88,
  "evidence_refs": ["trace:taste", "asset:before-after"]
 }]
}`
	res, err := Run(home, store, []byte(trace), DefaultPolicy(), ReferenceExtractor{}, t0)
	if err != nil {
		t.Fatalf("run trace: %v", err)
	}
	var tasteCandidateID string
	for _, decision := range res.Decisions {
		if decision.Kind == KindTaste {
			tasteCandidateID = decision.CandidateID
			if decision.Verdict != Park {
				t.Fatalf("taste candidate verdict = %q, want park", decision.Verdict)
			}
			break
		}
	}
	if tasteCandidateID == "" {
		t.Fatalf("no taste candidate decision: %+v", res.Decisions)
	}
	targetPath := filepath.Join(t.TempDir(), "taste.md")
	original := []byte("# Taste\n\nExisting standard source.\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildTastePlan(home, tasteCandidateID, targetPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != TastePlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateID != tasteCandidateID ||
		plan.CandidateVerdict != Park ||
		plan.TasteKey != "product-sample-mobile-app-visuals" ||
		plan.StandardDraft.StandardID == "" ||
		!strings.Contains(plan.StandardDraft.PositiveStandard, "inspection-ready") ||
		len(plan.StandardDraft.RequiredEvidence) != 4 ||
		!plan.TargetExists ||
		plan.TargetHash == "" {
		t.Fatalf("bad taste plan: %+v", plan)
	}
	paths, err := RecordTastePlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	var recorded TastePlan
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.ID != plan.ID || recorded.StandardDraft.StandardID != plan.StandardDraft.StandardID {
		t.Fatalf("recorded plan mismatch: %+v", recorded)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not mutate doctrine or product files") ||
		!strings.Contains(string(review), "Required Evidence Before Apply") {
		t.Fatalf("review missing safety/procedure text:\n%s", review)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("taste-plan must not promote claims, stat err=%v", err)
	}
}
