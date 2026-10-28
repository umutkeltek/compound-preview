package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/home"
	"compound/internal/work"
)

func TestAuthorityWorktreeRetireCLIRejectsOversizeRecoveryInventory(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	fixture := newWorktreeRetirementCLIFixture(t, homePath)
	runner := installRecoveryPreflightCLIGit(t)
	refs := recoveryPreflightCLIInventory(t, "oversize")
	var stdout, stderr bytes.Buffer
	code := runAuthorityWorktreeRetire(homePath, recoveryPreflightCLIPlanArgs(fixture, refs), &stdout, &stderr, cliTestNow.Add(10*time.Minute))
	if code == 0 {
		t.Fatalf("native plan admitted correctly hashed recovery file above 64 MiB: %s", stdout.String())
	}
	assertRecoveryPreflightCLINoMutation(t, fixture, runner)
}

func TestAuthorityWorktreeRetireCLIRejectsUncustodiableRecoveryCountsAndTotals(t *testing.T) {
	for _, kind := range []string{"aggregate", "count"} {
		t.Run(kind, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			fixture := newWorktreeRetirementCLIFixture(t, homePath)
			runner := installRecoveryPreflightCLIGit(t)
			refs := recoveryPreflightCLIInventory(t, kind)
			args := recoveryPreflightCLIPlanArgs(fixture, refs)
			var stdout, stderr bytes.Buffer
			if code := runAuthorityWorktreeRetire(homePath, args, &stdout, &stderr, cliTestNow.Add(10*time.Minute)); code == 0 {
				t.Fatalf("native PLAN admitted %s inventory: %s", kind, stdout.String())
			}
			if !strings.Contains(stdout.String(), "recovery_inventory_preflight_failed") {
				t.Fatalf("not capacity refusal: %s %s", stdout.String(), stderr.String())
			}
			assertRecoveryPreflightCLINoMutation(t, fixture, runner)
		})
	}
}

func TestAuthorityWorktreeRetireCLIRechecksReviewedRecoveryInventory(t *testing.T) {
	for _, kind := range []string{"legacy_oversize", "legacy_aggregate", "legacy_count", "changed", "grown", "missing"} {
		t.Run(kind, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			fixture := newWorktreeRetirementCLIFixture(t, homePath)
			runner := installRecoveryPreflightCLIGit(t)
			refs := []string{fixture.recoveryRef}
			legacy := strings.HasPrefix(kind, "legacy_")
			if legacy {
				refs = recoveryPreflightCLIInventory(t, strings.TrimPrefix(kind, "legacy_"))
			}
			var stdout, stderr bytes.Buffer
			var paths authority.WorktreeRetirementPlanPaths
			if legacy {
				// Model a genuine pre-upgrade reviewed plan: the old planner checked
				// hashes and Git eligibility but had no custody-capacity admission.
				store, err := work.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := loadWorktreeRetirementOwnerProof(store, fixture.ownerWorkID)
				if err != nil {
					t.Fatal(err)
				}
				source, err := loadWorktreeRetirementSourceProof(store, homePath, fixture.sourceWorkID, cliTestNow.Add(10*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				plan, err := authority.BuildWorktreeRetirementPlan(context.Background(), authority.WorktreeRetirementPlanRequest{
					RepoRoot: fixture.repo, WorktreePath: fixture.worktree, CandidateRef: fixture.candidateRef,
					AuthorityRef: "refs/heads/main", PublicationRef: "refs/remotes/origin/main", OwnerProof: owner, SourceProof: source, RecoveryRefs: refs,
				}, authority.WorktreeRetirementDependencies{Git: runner, Liveness: cliUnusedWorktreeLiveness{}, RecoveryInventoryPreflight: func(authority.WorktreeRetirementPlan) error { return nil }}, cliTestNow.Add(10*time.Minute))
				if err != nil || !plan.Eligible {
					t.Fatalf("legacy plan=%+v err=%v", plan, err)
				}
				paths, err = authority.RecordWorktreeRetirementPlan(homePath, plan)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if code := runAuthorityWorktreeRetire(homePath, recoveryPreflightCLIPlanArgs(fixture, refs), &stdout, &stderr, cliTestNow.Add(10*time.Minute)); code != 0 {
					t.Fatalf("plan code=%d %s", code, stderr.String())
				}
				var planned struct {
					Paths authority.WorktreeRetirementPlanPaths `json:"paths"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &planned); err != nil {
					t.Fatal(err)
				}
				paths = planned.Paths
			}
			stdout.Reset()
			stderr.Reset()
			if code := runAuthorityWorktreeRetire(homePath, []string{"review", paths.ReviewYAMLPath, "--decision", "authorize_retirement", "--rationale", "exact synthetic recovery fixture", "--reviewed-by", "operator:test", "--json"}, &stdout, &stderr, cliTestNow.Add(11*time.Minute)); code != 0 {
				t.Fatalf("review code=%d %s", code, stderr.String())
			}
			path := strings.SplitN(fixture.recoveryRef, ":", 3)[2]
			switch kind {
			case "changed":
				if err := os.WriteFile(path, []byte("changed recovery body"), 0600); err != nil {
					t.Fatal(err)
				}
			case "grown":
				file, err := os.OpenFile(path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate((64 << 20) + 1)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			stdout.Reset()
			stderr.Reset()
			if code := runAuthorityWorktreeRetire(homePath, []string{"apply", paths.PlanPath, "--actor", "codex:test", "--json"}, &stdout, &stderr, cliTestNow.Add(12*time.Minute)); code == 0 {
				t.Fatalf("native APPLY admitted %s: %s", kind, stdout.String())
			}
			if legacy && !strings.Contains(stderr.String(), "recovery-inventory preflight") {
				t.Fatalf("not capacity refusal: %s %s", stdout.String(), stderr.String())
			}
			if !legacy && !strings.Contains(stderr.String(), "recovery evidence invalid") {
				t.Fatalf("not drift refusal: %s %s", stdout.String(), stderr.String())
			}
			assertRecoveryPreflightCLINoMutation(t, fixture, runner)
		})
	}
}

func recoveryPreflightCLIPlanArgs(f worktreeRetirementCLIFixture, refs []string) []string {
	args := []string{"plan", "--repo", f.repo, "--worktree", f.worktree, "--ref", f.candidateRef, "--authority-ref", "refs/heads/main", "--publication-ref", "refs/remotes/origin/main", "--owner-work", f.ownerWorkID, "--source-work", f.sourceWorkID, "--json"}
	for _, ref := range refs {
		args = append(args, "--recovery-ref", ref)
	}
	return args
}

func recoveryPreflightCLIInventory(t *testing.T, kind string) []string {
	t.Helper()
	count, size := 1, int64((64<<20)+1)
	if kind == "aggregate" {
		count, size = 3, 64<<20
	}
	if kind == "count" {
		count, size = 257, 1
	}
	dir := t.TempDir()
	var refs []string
	for i := 0; i < count; i++ {
		path := filepath.Join(dir, fmt.Sprintf("recovery-%03d.bundle", i))
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "aggregate" && i == 2 {
			size = 1
		}
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			t.Fatal(err)
		}
		file.Close()
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, fmt.Sprintf("sha256-file:%x:%s", hash.Sum(nil), path))
	}
	return refs
}

type recoveryPreflightCLIGit struct{ mutations int }

func (g *recoveryPreflightCLIGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) >= 2 && (args[0] == "worktree" && args[1] == "remove" || args[0] == "update-ref" && args[1] == "-d") {
		g.mutations++
	}
	return (authority.ExecWorktreeRetirementGit{}).Run(ctx, repo, args...)
}

func installRecoveryPreflightCLIGit(t *testing.T) *recoveryPreflightCLIGit {
	t.Helper()
	oldGit, oldLiveness := worktreeRetirementCLIGit, worktreeRetirementCLILiveness
	g := &recoveryPreflightCLIGit{}
	worktreeRetirementCLIGit, worktreeRetirementCLILiveness = g, cliUnusedWorktreeLiveness{}
	t.Cleanup(func() { worktreeRetirementCLIGit, worktreeRetirementCLILiveness = oldGit, oldLiveness })
	return g
}

func assertRecoveryPreflightCLINoMutation(t *testing.T, f worktreeRetirementCLIFixture, g *recoveryPreflightCLIGit) {
	t.Helper()
	if g.mutations != 0 {
		t.Fatalf("Git mutations=%d, want zero", g.mutations)
	}
	if _, err := os.Stat(f.worktree); err != nil {
		t.Fatalf("worktree changed: %v", err)
	}
	if !commandRefExists(f.repo, f.candidateRef) {
		t.Fatal("candidate ref deleted")
	}
}
