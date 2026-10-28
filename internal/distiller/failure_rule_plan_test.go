package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailureRulePlanRecordsReviewArtifactsWithoutMutatingTarget(t *testing.T) {
	home, store := setup(t)
	trace := `{
 "trace_ref": "trace_failure_rule",
 "candidates": [{
  "kind": "failure_pattern",
  "subject": "dirty-source-control-hold",
  "statement": "agent repeatedly tries mutating a dirty repo without cleanup review",
  "detail": "repeated_policy_hold",
  "confidence": 0.91,
  "evidence_refs": ["trace:dirty-hold", "policy:hold"]
 }]
}`
	res, err := Run(home, store, []byte(trace), DefaultPolicy(), ReferenceExtractor{}, t0)
	if err != nil {
		t.Fatalf("run trace: %v", err)
	}
	var failureCandidateID string
	for _, decision := range res.Decisions {
		if decision.Kind == KindFailure {
			failureCandidateID = decision.CandidateID
			if decision.Verdict != Park {
				t.Fatalf("failure candidate verdict = %q, want park", decision.Verdict)
			}
			break
		}
	}
	if failureCandidateID == "" {
		t.Fatalf("no failure candidate decision: %+v", res.Decisions)
	}
	targetPath := filepath.Join(t.TempDir(), "rules.md")
	original := []byte("# Rules\n\nExisting rule source.\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildFailureRulePlan(home, failureCandidateID, targetPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != FailureRulePlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateID != failureCandidateID ||
		plan.CandidateVerdict != Park ||
		plan.FailureKey != "dirty-source-control-hold" ||
		plan.RuleDraft.RuleID == "" ||
		!strings.Contains(plan.RuleDraft.Trigger, "agent repeatedly tries") ||
		len(plan.RuleDraft.RequiredEvidence) != 4 ||
		!plan.TargetExists ||
		plan.TargetHash == "" {
		t.Fatalf("bad failure rule plan: %+v", plan)
	}
	paths, err := RecordFailureRulePlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	var recorded FailureRulePlan
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.ID != plan.ID || recorded.RuleDraft.RuleID != plan.RuleDraft.RuleID {
		t.Fatalf("recorded plan mismatch: %+v", recorded)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not mutate doctrine or policy") ||
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
		t.Fatalf("rule-plan must not promote claims, stat err=%v", err)
	}
}
