package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"compound/internal/work"
)

func TestWorkAdvanceExecuteForwardsArtifactProofRefs(t *testing.T) {
	cases := []struct {
		name string
		refs []string
	}{
		{name: "forwards ordered artifact proof refs", refs: []string{"evidence:first", "evidence:second"}},
		{name: "nil artifacts control", refs: nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaimedExecutionFixture(t, "executor", "artifact proof forwarding")

			for _, ref := range tc.refs {
				if _, err := fixture.store.Advance(fixture.workID, work.AdvanceInput{
					To:         "claimed",
					Actor:      "executor",
					ArtifactID: ref,
				}, cliTestNow); err != nil {
					t.Fatalf("advance %q: %v", ref, err)
				}
			}

			envelope, err := fixture.store.Get(fixture.workID)
			if err != nil {
				t.Fatalf("get envelope: %v", err)
			}
			if !slices.Equal(envelope.ArtifactIDs, tc.refs) {
				t.Fatalf("envelope ArtifactIDs: want %v, got %v", tc.refs, envelope.ArtifactIDs)
			}

			cmd := exec.Command(
				compoundTestBinary(t),
				"work", "advance", fixture.workID,
				"--to", "running",
				"--by", "executor",
				"--execute",
				"--harness", "os-harness-entrypoints",
				"--workspace", t.TempDir(),
				"--", "/bin/echo", "proof-forwarding",
			)
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+fixture.homePath)
			repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatalf("abs repo root: %v", err)
			}
			cmd.Dir = repoRoot

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute: %v\n%s", err, out)
			}

			final, err := fixture.store.Get(fixture.workID)
			if err != nil {
				t.Fatalf("get final: %v", err)
			}
			if final.State != "verifying" {
				t.Fatalf("final state: want verifying, got %s", final.State)
			}

			attempt := fixture.executionAttempt(t)
			if !slices.Equal(attempt.ProofRefs, tc.refs) {
				t.Fatalf("attempt ProofRefs: want %v, got %v", tc.refs, attempt.ProofRefs)
			}
		})
	}
}
