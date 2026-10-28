package main

import (
	"strings"
	"testing"

	"compound/internal/work"
)

func TestWorkAdvanceExecuteHonorsExplicitClaudeBypass(t *testing.T) {
	fixture := newGovernedClaudeExecutionFixture(t, "explicit bypass is preserved by the governed rail")
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{
			Ready:    false,
			Details:  []string{"permission_posture: managed settings differ from authority"},
			Evidence: governedClaudeReadyEvidenceFixture(),
		}, nil
	})

	fixture.run(t, "bypass", "edit the claimed change")

	argv := fixture.argv(t)
	if !strings.Contains(argv, "--dangerously-skip-permissions") {
		t.Fatalf("explicit --permission-mode bypass argv = %q, want --dangerously-skip-permissions", argv)
	}
	if strings.Contains(argv, "--permission-mode plan") {
		t.Fatalf("explicit --permission-mode bypass argv must not silently fall back to plan: %q", argv)
	}
	if strings.Contains(argv, "--permission-mode bypass") {
		t.Fatalf("explicit --permission-mode bypass argv must carry --dangerously-skip-permissions, not --permission-mode bypass: %q", argv)
	}
}
