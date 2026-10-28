package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/work"
)

const sourceControlMutationCLIRejectSubEnv = "COMPOUND_SOURCE_CONTROL_MUTATION_CLI_REJECT_SUB"

func TestAuthorityDirtyPlanDerivesMutationBindingFromPersistedActions(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	createSourceControlMutationOwner(t, homePath, "work_owner_cli_dirty", "running")
	repo := initCompoundCommandGitRepo(t)
	writeFileAt(t, filepath.Join(repo, "README.md"), "changed\n")
	writeFileAt(t, filepath.Join(repo, "SECOND.md"), "second\n")

	inputHash := "mutation-input:sha256:" + strings.Repeat("a", 64)
	first := persistedMutationAction("act_cli_dirty_first", "work_live_cli_dirty", filepath.Join(repo, "README.md"), inputHash)
	second := persistedMutationAction("act_cli_dirty_second", "work_live_cli_dirty", filepath.Join(repo, "SECOND.md"), inputHash)
	writeJSONFixture(t, filepath.Join(homePath, "events", first.ID+".json"), first)
	writeJSONFixture(t, filepath.Join(homePath, "events", second.ID+".json"), second)

	out := captureStdout(t, func() {
		cmdAuthority([]string{
			"dirty-plan", repo,
			"--work-id", "work_owner_cli_dirty",
			"--action-intent", second.ID,
			"--action-intent", first.ID,
			"--json",
		})
	})
	var result struct {
		Plan  authority.SourceControlReviewPlan `json:"plan"`
		Paths struct {
			ReviewYAMLPath string `json:"review_yaml_path"`
		} `json:"paths"`
	}
	readJSONBytes(t, []byte(out), &result)
	binding := result.Plan.MutationBinding
	if binding == nil {
		t.Fatalf("dirty-plan omitted persisted mutation binding: %s", out)
	}
	if binding.ActionWorkID != "work_live_cli_dirty" || binding.MutationInputHash != inputHash ||
		binding.Capability != "filesystem.write" || binding.Action != "multi_edit_file" || binding.MutationClass != "local_reversible" {
		t.Fatalf("dirty-plan trusted or lost action semantics: %+v", binding)
	}
	if len(binding.TargetPaths) != 2 || len(binding.ActionIntentIDs) != 2 ||
		binding.ActionIntentIDs[0] != first.ID || binding.ActionIntentIDs[1] != second.ID {
		t.Fatalf("dirty-plan did not bind complete persisted action set: %+v", binding)
	}
	if result.Plan.WorkID != "work_owner_cli_dirty" {
		t.Fatalf("owner work provenance = %q", result.Plan.WorkID)
	}
	reviewOut := captureStdout(t, func() {
		cmdAuthority([]string{
			"dirty-review", result.Paths.ReviewYAMLPath,
			"--decision", "authorize_mutation",
			"--rationale", "operator reviewed exact persisted actions",
			"--reviewed-by", "operator:test",
			"--json",
		})
	})
	var report authority.SourceControlReviewReport
	readJSONBytes(t, []byte(reviewOut), &report)
	if report.Decision != "authorize_mutation" || report.ReviewedBy != "operator:test" ||
		report.AuthorizationDigest == "" || report.SourceDigest == "" {
		t.Fatalf("dirty-review lost reviewer/seal: %+v", report)
	}
}

func TestAuthorityDirtyPlanRejectsUnusableMutationOwner(t *testing.T) {
	if os.Getenv(sourceControlMutationCLIRejectSubEnv) == "1" {
		cmdAuthority([]string{
			"dirty-plan", os.Getenv("COMPOUND_SOURCE_CONTROL_MUTATION_TEST_REPO"),
			"--work-id", os.Getenv("COMPOUND_SOURCE_CONTROL_MUTATION_TEST_OWNER"),
			"--action-intent", os.Getenv("COMPOUND_SOURCE_CONTROL_MUTATION_TEST_ACTION"),
		})
		return
	}

	repo := initCompoundCommandGitRepo(t)
	writeFileAt(t, filepath.Join(repo, "README.md"), "changed\n")
	cases := []struct {
		name       string
		owner      string
		ownerState string
		want       string
	}{
		{name: "synthetic owner", owner: "work_live_cli_reject", want: "must not be synthetic work_live_*"},
		{name: "missing durable owner", owner: "work_cli_missing", want: "does not exist"},
		{name: "non-current durable owner", owner: "work_cli_ready", ownerState: "ready", want: "must be claimed|running|verifying|held"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			if tc.ownerState != "" {
				createSourceControlMutationOwner(t, homePath, tc.owner, tc.ownerState)
			}
			inputHash := "mutation-input:sha256:" + strings.Repeat("7", 64)
			action := persistedMutationAction("act_cli_owner_reject", "work_live_cli_reject", filepath.Join(repo, "README.md"), inputHash)
			writeJSONFixture(t, filepath.Join(homePath, "events", action.ID+".json"), action)

			cmd := exec.Command(os.Args[0], "-test.run=^TestAuthorityDirtyPlanRejectsUnusableMutationOwner$")
			cmd.Env = append(os.Environ(),
				sourceControlMutationCLIRejectSubEnv+"=1",
				"COMPOUND_HOME="+homePath,
				"COMPOUND_SOURCE_CONTROL_MUTATION_TEST_REPO="+repo,
				"COMPOUND_SOURCE_CONTROL_MUTATION_TEST_OWNER="+tc.owner,
				"COMPOUND_SOURCE_CONTROL_MUTATION_TEST_ACTION="+action.ID,
			)
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("dirty-plan should reject %s, got err=%v output=%s", tc.name, err, output)
			}
			if exitErr.ExitCode() != 1 {
				t.Fatalf("dirty-plan %s exit = %d, want 1; output=%s", tc.name, exitErr.ExitCode(), output)
			}
			if !strings.Contains(string(output), tc.want) {
				t.Fatalf("dirty-plan %s output = %s, want %q", tc.name, output, tc.want)
			}
			assertNoSourceControlReviewArtifacts(t, homePath)
		})
	}
}

func TestAuthorityDirtyPlanCleanupOnlyDoesNotRequireDurableMutationOwner(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo := initCompoundCommandGitRepo(t)
	writeFileAt(t, filepath.Join(repo, "README.md"), "changed\n")

	out := captureStdout(t, func() {
		cmdAuthority([]string{"dirty-plan", repo, "--work-id", "work_live_cleanup_evidence", "--json"})
	})
	var result struct {
		Plan authority.SourceControlReviewPlan `json:"plan"`
	}
	readJSONBytes(t, []byte(out), &result)
	if result.Plan.WorkID != "work_live_cleanup_evidence" || result.Plan.MutationBinding != nil {
		t.Fatalf("cleanup-only dirty-plan changed semantics: %+v", result.Plan)
	}
}

func TestDirtyPlanPersistedActionsMustShareExecutableIdentity(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	inputHash := "mutation-input:sha256:" + strings.Repeat("b", 64)
	first := persistedMutationAction("act_cli_mismatch_first", "work_live_cli_mismatch", "/repo/FIRST.md", inputHash)
	second := persistedMutationAction("act_cli_mismatch_second", "work_live_cli_mismatch", "/repo/SECOND.md", inputHash)
	second.Action = "edit_file"
	writeJSONFixture(t, filepath.Join(homePath, "events", first.ID+".json"), first)
	writeJSONFixture(t, filepath.Join(homePath, "events", second.ID+".json"), second)

	if _, err := loadSourceControlMutationBinding(homePath, []string{first.ID, second.ID}); err == nil || !strings.Contains(err.Error(), "share action") {
		t.Fatalf("mismatched persisted semantics error = %v", err)
	}
}

func persistedMutationAction(id, workID, target, inputHash string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            id,
			CreatedAt:     cliTestNow.Format("2006-01-02T15:04:05Z07:00"),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier2",
			EvidenceRefs:  []string{inputHash},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    "filesystem.write",
		Action:        "multi_edit_file",
		Target:        envelope.ActionTarget{Kind: "file", Path: target},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "repo",
	}
}

func createSourceControlMutationOwner(t *testing.T, homePath, workID, state string) {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		WorkID: workID, Owner: "codex", SuccessPredicate: "own one exact source-control mutation review",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if state == "ready" {
		return
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if state == "claimed" {
		return
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("unsupported source-control owner test state %q", state)
	}
}

func assertNoSourceControlReviewArtifacts(t *testing.T, homePath string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(homePath, "reviews", authority.SourceControlCleanupReviewArea))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected mutation owner wrote source-control review artifacts: %+v", entries)
	}
}

func writeFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSONBytes(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, data)
	}
}
