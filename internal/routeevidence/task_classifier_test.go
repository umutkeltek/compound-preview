package routeevidence

import (
	"strings"
	"testing"
)

func TestClassifyCapabilityTaskUsesOwnedOrderedRules(t *testing.T) {
	tests := []struct {
		task string
		want string
	}{
		{task: "bulk rename these files", want: "mechanical-edit"},
		{task: "write regression tests from the spec", want: "tests-from-spec"},
		{task: "translate the app into Turkish", want: "localization"},
		{task: "repair an expired oauth refresh token", want: "certify"},
		{task: "continue the Compound context packet loop", want: "plan-decompose"},
		{task: "improve the frontend interface", want: "build-implement"},
		{task: "extract metrics from the export", want: "classify-extract"},
		{task: "draft a sales email", want: "draft-prose"},
		{task: "audit this authorization design", want: "judge-critique"},
		{task: "plan the release architecture", want: "plan-decompose"},
		{task: "ship the app to production", want: "certify"},
		{task: "implement the cache fix", want: "build-implement"},
		{task: "sort these records", want: "classify-extract"},
	}

	for _, tt := range tests {
		t.Run(tt.want+"/"+tt.task, func(t *testing.T) {
			got := ClassifyCapabilityTask(tt.task)
			if got.CapabilityClass != tt.want {
				t.Fatalf("ClassifyCapabilityTask(%q) class=%q, want %q", tt.task, got.CapabilityClass, tt.want)
			}
			if got.DeterministicFirst == "" || got.EvaluationMethod == "" || got.RuleID == "" {
				t.Fatalf("classification must explain its deterministic and evaluation contract: %+v", got)
			}
			if got.SchemaVersion != CapabilityTaskClassifierSchemaVersion || got.AuthorityRef != CapabilityTaskClassifierAuthorityRef || !strings.HasPrefix(got.AuthorityRevision, "sha256:") {
				t.Fatalf("classification must bind its versioned authority contract: %+v", got)
			}
		})
	}
}

func TestCapabilityTaskClassifierRevisionChangesWithRules(t *testing.T) {
	baseline := taskClassifierRevision(taskClassificationRules, defaultTaskClassificationRule)
	changed := append([]taskClassificationRule{}, taskClassificationRules...)
	changed[0].evaluationMethod += " plus changed proof"
	if got := taskClassifierRevision(changed, defaultTaskClassificationRule); got == baseline {
		t.Fatal("classifier authority revision did not change with rule contract")
	}
	changedDefault := defaultTaskClassificationRule
	changedDefault.evaluationMethod += " plus changed default proof"
	if got := taskClassifierRevision(taskClassificationRules, changedDefault); got == baseline {
		t.Fatal("classifier authority revision did not change with default rule contract")
	}
}

func TestClassifyCapabilityTaskPreservesPrecedence(t *testing.T) {
	got := ClassifyCapabilityTask("audit the test coverage")
	if got.CapabilityClass != "tests-from-spec" {
		t.Fatalf("first matching rule must win, got %+v", got)
	}
}
