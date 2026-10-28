package authority

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
)

// This tests the byte-validation seam only. Its fixture receipt IDs are not
// accepted by Work; the consumer journey must authenticate real owned records.
func TestWorktreeRetirementDeliveryValidationConsumesExistingArtifacts(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	homePath, paths, report := authorizedRetirementFixture(t, fixture)
	recorded := 0
	result, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
		HomePath: homePath, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}, successfulRetirementApplyDependencies(ExecWorktreeRetirementGit{}, &recorded), time.Date(2026, 7, 15, 7, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	planBody, reportBody, yamlBody, resultBody := read(paths.PlanPath), read(report.ReviewPath), read(report.SourcePath), read(result.ResultPath)
	got, err := gitrepository.ValidateRetirementDelivery(planBody, reportBody, yamlBody, resultBody)
	if err != nil || got.Result.ResultDigest != result.ResultDigest {
		t.Fatalf("existing producer rejected: %+v %v", got, err)
	}
	for _, kind := range []string{"yaml bytes", "second yaml document", "unverified phase", "deletion flags", "authority changed", "trailing value"} {
		t.Run(kind, func(t *testing.T) {
			yamlInput, resultInput, reportInput := yamlBody, resultBody, reportBody
			changed := result
			switch kind {
			case "yaml bytes":
				yamlInput = append(append([]byte(nil), yamlBody...), '\n')
			case "second yaml document":
				yamlInput = append(append([]byte(nil), yamlBody...), []byte("\n---\nsuccess: true\n")...)
				changedReport := report
				changedReport.SourceDigest = "sha256:" + digestRetirementHex(yamlInput)
				changedReport.AuthorizationDigest = worktreeRetirementAuthorizationDigest(changedReport)
				changed.AuthorizationDigest = changedReport.AuthorizationDigest
				reportInput, err = json.Marshal(changedReport)
				if err != nil {
					t.Fatal(err)
				}
			case "unverified phase":
				changed.Phase = WorktreeRetirementPhasePrepared
			case "deletion flags":
				changed.RefDeleted = false
			case "authority changed":
				changed.AuthorityTip = fixture.candidateTip
			case "trailing value":
				resultInput = append(append([]byte(nil), resultBody...), []byte(" {}")...)
			}
			if kind != "yaml bytes" && kind != "trailing value" {
				changed.ResultDigest = worktreeRetirementResultDigest(changed)
				resultInput, err = json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := gitrepository.ValidateRetirementDelivery(planBody, reportInput, yamlInput, resultInput); err == nil {
				t.Fatal("contradictory retirement evidence accepted")
			}
		})
	}
}
