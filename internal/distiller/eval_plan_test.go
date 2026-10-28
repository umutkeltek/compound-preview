package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalPlanRecordsReviewArtifactsWithoutMutatingCorpus(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-session.json")
	res, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0)
	if err != nil {
		t.Fatalf("run trace fixture: %v", err)
	}
	var evalCandidateID string
	for _, decision := range res.Decisions {
		if decision.Kind == KindEvalCase {
			evalCandidateID = decision.CandidateID
			break
		}
	}
	if evalCandidateID == "" {
		t.Fatalf("no eval_case decision: %+v", res.Decisions)
	}
	corpusDir := filepath.Join(t.TempDir(), "eval-corpus")
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(corpusDir, "existing-eval.json")
	original := []byte(`{"existing":true}` + "\n")
	if err := os.WriteFile(corpusFile, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildEvalPlan(home, evalCandidateID, corpusDir, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != EvalPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateID != evalCandidateID ||
		plan.CandidateVerdict != Operationalize ||
		plan.FixtureDraft.SchemaVersion != EvalCaseSchemaVersion ||
		plan.FixtureDraft.ProposedCaseID == "" ||
		plan.FixtureDraft.RequestTraceRef == "" ||
		len(plan.FixtureDraft.RequiredArtifacts) != 3 ||
		!plan.TargetExists ||
		plan.TargetHash == "" {
		t.Fatalf("bad eval plan: %+v", plan)
	}
	paths, err := RecordEvalPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	var recorded EvalPlan
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.ID != plan.ID || recorded.FixtureDraft.ProposedCaseID != plan.FixtureDraft.ProposedCaseID {
		t.Fatalf("recorded plan mismatch: %+v", recorded)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not mutate the eval corpus") ||
		!strings.Contains(string(review), "Required Before Corpus Apply") {
		t.Fatalf("review missing safety/procedure text:\n%s", review)
	}
	got, err := os.ReadFile(corpusFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("corpus mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("eval-plan must not promote claims, stat err=%v", err)
	}
}
