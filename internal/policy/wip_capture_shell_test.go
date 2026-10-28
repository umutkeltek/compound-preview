package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
)

// foreignDirtyContext is the situation the operator is actually in: a repo with
// unowned uncommitted changes, and a lane that has written none of them.
func foreignDirtyContext(repo string, foreign int) SourceControlContext {
	return SourceControlContext{
		EntityRef:                  "repo:fixture",
		GitRoot:                    repo,
		Status:                     "dirty",
		ChangedPaths:               foreign,
		TargetPath:                 repo,
		TargetStatus:               "dirty",
		SessionOwnershipKnown:      true,
		SessionOwnedChangedPaths:   0,
		SessionForeignChangedPaths: foreign,
	}
}

func monitorShellInDirtyRepo(t *testing.T, cfg Config, cwd, command string, sc SourceControlContext) envelope.PolicyDecision {
	t.Helper()
	monitored := MonitorToolAction(ToolActionRequest{
		WorkID:             "work_wip",
		Actor:              "claude_minimax",
		Harness:            "claude_minimax",
		Tool:               "Bash",
		Cwd:                cwd,
		Arguments:          map[string]any{"command": command},
		ToolSchemaRevision: "builtin:bash",
	}, cfg, envelopeNow())
	// The live hook re-evaluates the monitored intent against the loaded policy
	// WITH the source-control observation attached; that second evaluation is
	// where the dirty gate runs, so the test has to go through it too.
	return EvaluateWithContext(monitored.Intent, cfg, EvaluationContext{SourceControl: &sc}, envelopeNow())
}

// TestForeignDirtyRepoHoldsOnlyWorkCapturingShapes is the falsifier for the
// month-long friction: a lane entering any repo with unowned uncommitted
// changes was held on EVERY mutation, including commands that cannot touch a
// working tree. The split below is the whole fix -- capturing shapes still
// hold, everything else runs.
func TestForeignDirtyRepoHoldsOnlyWorkCapturingShapes(t *testing.T) {
	scope := t.TempDir()
	repo := filepath.Join(scope, "example-app")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := envelopeConfig(scope)
	sc := foreignDirtyContext(repo, 8)

	for _, tc := range []struct {
		name    string
		command string
		hold    bool
	}{
		// The reported case, and its neighbours: none of these can alter a
		// working tree, so eight foreign dirty paths are irrelevant to them.
		{name: "git fetch", command: "git fetch origin main"},
		{name: "git pull", command: "git pull --rebase origin main"},
		{name: "git push", command: "git push origin main"},
		{name: "git status", command: "git status --short"},
		{name: "git log", command: "git log --oneline -5"},
		{name: "git diff", command: "git diff"},
		{name: "go test", command: "go test ./..."},
		{name: "bun install", command: "bun install"},
		// Inline scripts now name cwd as a path mutation. Their opaque body
		// cannot prove that foreign dirty files are untouched.
		{name: "python path mutation", command: `python3 -c "print(1)"`, hold: true},
		{name: "plain commit is index-only", command: "git commit -m msg"},
		{name: "commit with an explicit pathspec", command: "git commit -am msg -- main.go"},
		{name: "add a named file", command: "git add main.go"},
		{name: "restore --staged only unstages", command: "git restore --staged main.go"},
		{name: "reset without --hard", command: "git reset HEAD~1"},
		{name: "checkout a branch", command: "git checkout -b feature"},

		// These record or destroy changes the lane never made.
		{name: "commit -a sweeps", command: "git commit -am msg", hold: true},
		{name: "commit --all sweeps", command: "git commit --all -m msg", hold: true},
		{name: "commit --include sweeps", command: "git commit --include -m msg", hold: true},
		{name: "add -A", command: "git add -A", hold: true},
		{name: "add --all", command: "git add --all", hold: true},
		{name: "add -u", command: "git add -u", hold: true},
		{name: "add dot", command: "git add .", hold: true},
		{name: "add a directory", command: "git add src", hold: true},
		{name: "add a trailing-separator path", command: "git add src/", hold: true},
		{name: "add a glob", command: "git add *.go", hold: true},
		{name: "stash", command: "git stash", hold: true},
		{name: "stash push", command: "git stash push -m wip", hold: true},
		{name: "reset --hard", command: "git reset --hard origin/main", hold: true},
		{name: "clean", command: "git clean -fd", hold: true},
		{name: "checkout -- path", command: "git checkout -- main.go", hold: true},
		{name: "restore worktree", command: "git restore main.go", hold: true},
		{name: "capturing segment in a chain", command: "go build ./... && git add -A", hold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pd := monitorShellInDirtyRepo(t, cfg, repo, tc.command, sc)
			if tc.hold && Allowed(pd) {
				t.Fatalf("outcome=%q; a shape that captures or destroys foreign work must stay held: %v",
					pd.Outcome, pd.Reasons)
			}
			if !tc.hold && !Allowed(pd) {
				t.Fatalf("outcome=%q; this command cannot touch a working tree, so foreign dirty paths must not hold it: %v",
					pd.Outcome, pd.Reasons)
			}
		})
	}
}

// TestForeignDirtyNarrowingNeedsTheEnvelope: the narrowing is a consequence of
// the operator's envelope, not a new default. Outside it the old rule stands.
func TestForeignDirtyNarrowingNeedsTheEnvelope(t *testing.T) {
	scope := t.TempDir()
	repo := filepath.Join(scope, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	sc := foreignDirtyContext(repo, 8)

	t.Run("L2 keeps the repo-wide hold", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.OperatorAutonomyLevel = "L2"
		if pd := monitorShellInDirtyRepo(t, cfg, repo, "git fetch origin main", sc); Allowed(pd) {
			t.Fatalf("outcome=%q; the narrowing must not apply below L3", pd.Outcome)
		}
	})

	t.Run("no covering grant keeps the repo-wide hold", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.StandingGrants = nil
		if pd := monitorShellInDirtyRepo(t, cfg, repo, "git fetch origin main", sc); Allowed(pd) {
			t.Fatalf("outcome=%q; the narrowing follows the grant that draws the envelope", pd.Outcome)
		}
	})

	t.Run("an operator-gated shape is never narrowed", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		if pd := monitorShellInDirtyRepo(t, cfg, repo, "git push --force origin main", sc); Allowed(pd) {
			t.Fatalf("outcome=%q; a gated shape stays held in a dirty repo too", pd.Outcome)
		}
	})
}

// TestFileWritesStillFollowTheirOwnTarget: for a write, "does this capture
// foreign work" is a question about the FILE it lands on, not about the repo.
// A write onto a dirty file still holds; a write onto a clean one does not.
func TestFileWritesStillFollowTheirOwnTarget(t *testing.T) {
	scope := t.TempDir()
	repo := filepath.Join(scope, "repo")
	cfg := envelopeConfig(scope)

	write := func(targetPath, targetStatus string) envelope.PolicyDecision {
		sc := foreignDirtyContext(repo, 8)
		sc.TargetPath = targetPath
		sc.TargetStatus = targetStatus
		ai := envelope.ActionIntent{
			Header: envelope.Header{ID: "act_write", IDs: envelope.IDs{WorkID: "work_wip"}},
			Actor:  "claude_minimax", Capability: "filesystem.write", Action: "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: targetPath},
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
		}
		return EvaluateWithContext(ai, cfg, EvaluationContext{SourceControl: &sc}, envelopeNow())
	}

	if pd := write(filepath.Join(repo, "dirty.go"), "dirty"); Allowed(pd) {
		t.Fatalf("outcome=%q; a write onto an unowned dirty file must stay held", pd.Outcome)
	}
	if pd := write(filepath.Join(repo, "clean.go"), "clean"); !Allowed(pd) {
		t.Fatalf("outcome=%q; a write onto a clean file is not capturing anyone's work: %v",
			pd.Outcome, pd.Reasons)
	}
	// Unresolvable target status stays held: the boundary must not guess that a
	// file it cannot place is clean.
	if pd := write(filepath.Join(repo, "unknown.go"), ""); Allowed(pd) {
		t.Fatalf("outcome=%q; an unresolvable target must fail closed", pd.Outcome)
	}
}

// TestNarrowedDecisionSaysWhy: an operator reading the decision must be able to
// see that the dirty escalation was deliberately left off, and on what basis.
func TestNarrowedDecisionSaysWhy(t *testing.T) {
	scope := t.TempDir()
	repo := filepath.Join(scope, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	pd := monitorShellInDirtyRepo(t, envelopeConfig(scope), repo, "git fetch origin main", foreignDirtyContext(repo, 8))
	found := false
	for _, reason := range pd.Reasons {
		if strings.Contains(reason, "neither captures nor destroys working-tree changes") &&
			strings.Contains(reason, "8 foreign dirty") {
			found = true
		}
	}
	if !found {
		t.Fatalf("narrowed decision does not record the foreign dirty count or the basis: %v", pd.Reasons)
	}
}
