package work

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

const dispatchRecipeFixture = `{"schema_version":"compound.proof_consumers.v1","profile":"xcode_consumers_v1","consumers":[{"id":"ios-app","command":["bash","Tools/compound_proof.sh","--scheme","Health Save"],"dir":".","timeout_seconds":1800,"required_tools":["bash","xcodebuild"],"report_kind":"xcresult","required_tests":["FooTests/testBar()"]}]}`

// dispatchRecipeClaim claims one git-delivery work whose target worktree root
// optionally carries a compound-proof.json body, and returns the dispatched
// pack plus rendered worker prompt.
func dispatchRecipeClaim(t *testing.T, proofBody string) (string, envelope.ContextPack, string) {
	t.Helper()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, base := initLandingRepo(t)
	if proofBody != "" {
		writeLandingFile(t, filepath.Join(repo, CanonicalConsumerContractPath), proofBody)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "the lane builds with the repo-owned recipe",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.DispatchClaim(now.Add(time.Minute), DispatchPlanOptions{Actor: "codex", WorkID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimedWorkID != w.ID {
		t.Fatalf("claimed %q, want %q (status=%s no_action=%s)", claimed.ClaimedWorkID, w.ID, claimed.Status, claimed.NoAction)
	}
	var pack envelope.ContextPack
	if err := store.readJSON(filepath.Join(store.home, "context", "packs", claimed.ContextPackID+".json"), &pack); err != nil {
		t.Fatal(err)
	}
	return w.GitDelivery.TargetWorktreeRoot, pack, claimed.WorkerPromptMD
}

func TestDispatchPromptCarriesWorkspaceProofRecipe(t *testing.T) {
	t.Parallel()
	root, pack, prompt := dispatchRecipeClaim(t, dispatchRecipeFixture)
	command := "bash Tools/compound_proof.sh --scheme 'Health Save'"
	if len(pack.RequiredChecks) != 1 || !strings.Contains(pack.RequiredChecks[0], command) || !strings.Contains(pack.RequiredChecks[0], "consumer:ios-app") {
		t.Fatalf("dispatch pack required_checks must carry the repo recipe %q: %#v", command, pack.RequiredChecks)
	}
	for _, want := range []string{dispatchRecipeHeading, "- workspace: " + root, command, "consumer:ios-app", "timeout 1800s", filepath.Join(root, CanonicalConsumerContractPath)} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("worker prompt missing %q:\n%s", want, prompt)
		}
	}
	sum := sha256.Sum256([]byte(dispatchRecipeFixture))
	wantRev := envelope.SourceRev{Source: "file://" + filepath.Join(root, CanonicalConsumerContractPath), Rev: "sha256:" + hex.EncodeToString(sum[:])}
	found := false
	for _, rev := range pack.SourceRevisions {
		found = found || rev == wantRev
	}
	if !found {
		t.Fatalf("dispatch pack must bind the recipe bytes it rendered: want %+v in %+v", wantRev, pack.SourceRevisions)
	}
}

func TestDispatchPromptWithoutProofRecipeIsUnchanged(t *testing.T) {
	t.Parallel()
	_, pack, prompt := dispatchRecipeClaim(t, "")
	// Golden: the pre-recipe renderer's exact bytes for this fixture.
	d := pack.SourceRevisions[0].Rev
	field := func(key string) string {
		_, rest, _ := strings.Cut(d, key+":")
		value, _, _ := strings.Cut(rest, ";")
		return value
	}
	golden := "# Dispatch Work " + pack.IDs.WorkID + "\n\n\n" +
		"## TASK CONTRACT\n\n" +
		"- intent: the lane builds with the repo-owned recipe\n" +
		"- mode: execute\n\n" +
		"## LOAD THESE SKILLS\n\n" +
		"- none surfaced\n\n" +
		"## SOURCE CONTROL CONTRACT\n\n" +
		"- target_worktree_root: " + field("target_root") + "\n" +
		"- repository_fingerprint: " + field("repository_fingerprint") + "\n" +
		"- target_local_ref: refs/heads/main\n" +
		"- expected_base_sha: " + field("expected_base") + "\n" +
		"- publication_mode: local_only\n" +
		"- Verify the worktree contains this base before editing; if the target moved, rebase and rerun the full proof battery.\n" +
		"- Attach the final verified candidate to this WorkEnvelope as commit:<full-sha>.\n" +
		"- Certify only through: compound work land-check " + pack.IDs.WorkID + " --tip <full-sha>; the command reads repository, target, and publication authority from this WorkEnvelope.\n\n" +
		"## TASK\n\n\n" +
		"## SUCCESS\n\n" +
		"the lane builds with the repo-owned recipe"
	if prompt != golden {
		t.Fatalf("worker prompt without compound-proof.json drifted from the pre-recipe bytes:\n--- got ---\n%q\n--- want ---\n%q", prompt, golden)
	}
	if pack.RequiredChecks == nil || len(pack.RequiredChecks) != 0 {
		t.Fatalf("required_checks must stay present and empty without a recipe: %#v", pack.RequiredChecks)
	}
	if len(pack.Excluded) != 1 || pack.Excluded[0].Ref != dispatchWorkerPromptContractRef {
		t.Fatalf("excluded must hold only the worker-prompt contract note: %+v", pack.Excluded)
	}
	if strings.Contains(prompt, dispatchRecipeHeading) {
		t.Fatalf("worker prompt must not render a recipe section without compound-proof.json:\n%s", prompt)
	}
}

func TestDispatchPromptRecordsInvalidProofRecipeAsExcluded(t *testing.T) {
	t.Parallel()
	root, pack, prompt := dispatchRecipeClaim(t, `{"schema_version":"compound.proof_consumers.v0","profile":"xcode_consumers_v1","consumers":[]}`)
	if len(pack.RequiredChecks) != 0 || strings.Contains(prompt, dispatchRecipeHeading) {
		t.Fatalf("an invalid recipe must never reach the lane: checks=%#v\n%s", pack.RequiredChecks, prompt)
	}
	ref := "file://" + filepath.Join(root, CanonicalConsumerContractPath)
	for _, excluded := range pack.Excluded {
		if excluded.Ref == ref && strings.Contains(excluded.Reason, "unsupported consumer contract") {
			return
		}
	}
	t.Fatalf("invalid recipe must be recorded as excluded with its parse reason: %+v", pack.Excluded)
}
