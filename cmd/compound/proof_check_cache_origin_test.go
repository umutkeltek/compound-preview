package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/authority/gitrepository"
	"compound/internal/work"
)

func TestProofCommandsPreserveCallerCacheAcrossSealedWorkspaces(t *testing.T) {
	for _, mode := range []string{"isolated", "shared"} {
		t.Run(mode, func(t *testing.T) {
			// The caller cache is a per-worktree Go cache: isolate its root so
			// the fixture never creates directories under the real user cache.
			_, cacheBase := proofCheckGoCacheFixture(t)
			t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
			if mode == "shared" {
				t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "1")
			}
			inheritedCache := t.TempDir()
			t.Setenv("GOCACHE", inheritedCache)
			t.Setenv("GOTOOLCHAIN", "auto")
			binDir := t.TempDir()
			probe := filepath.Join(binDir, "bun")
			if err := os.WriteFile(probe, []byte("#!/bin/sh\npwd\nprintf '%s\\n' \"$GOCACHE\" \"$GOTOOLCHAIN\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			probeBody, err := os.ReadFile(probe)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "go"), probeBody, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var previousOriginCache string
			for range 2 {
				repo := initCompoundCommandGitRepo(t)
				if err := os.MkdirAll(filepath.Join(repo, "tools"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "tools", "cache.test.ts"), []byte("// synthetic tool input\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runCommandGit(t, repo, "add", "tools/cache.test.ts")
				runCommandGit(t, repo, "commit", "-m", "add synthetic tool input")
				source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				wantCache := inheritedCache
				if mode == "isolated" {
					cache, ok := work.ProofGoCache(repo)
					if !ok {
						t.Fatal("no cache chosen for original caller")
					}
					wantCache = cache
					if wantCache == previousOriginCache || wantCache == inheritedCache {
						t.Fatalf("caller cache %q was not isolated from sibling and inherited caches", wantCache)
					}
					if !strings.HasPrefix(wantCache, cacheBase+string(filepath.Separator)) {
						t.Fatalf("caller cache %q escaped the isolated fixture cache root %q", wantCache, cacheBase)
					}
				}
				previousOriginCache = wantCache
				ctx := context.WithValue(context.Background(), proofCheckCacheOriginKey{}, repo)
				var previousSealedRepo string
				for range 2 {
					sealedRepo, cleanup, err := prepareProofCheckSealedWorkspace(ctx, repo, source)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := cleanup(); err != nil {
							t.Error(err)
						}
					})
					if sealedRepo == repo || sealedRepo == previousSealedRepo {
						t.Fatalf("expected a fresh sealed checkout, got %q", sealedRepo)
					}
					previousSealedRepo = sealedRepo
					wantDir, err := filepath.EvalSymlinks(sealedRepo)
					if err != nil {
						t.Fatal(err)
					}
					step := runProofCheckStepContext(ctx, sealedRepo, proofCheckStep{
						Name: "cache-origin", Command: []string{"go"}, ProofRef: "cache-origin",
					}, "HEAD")
					if step.Status != "ok" {
						t.Fatalf("ordinary proof child failed: %+v", step)
					}
					bunOutput, err := runProofCheckBunTools(ctx, sealedRepo)
					if err != nil {
						t.Fatalf("Bun proof child failed: %v: %s", err, bunOutput)
					}
					for child, output := range map[string]string{"ordinary": step.OutputTail, "bun": string(bunOutput)} {
						want := strings.Join([]string{wantDir, wantCache, "local"}, "\n")
						if got := strings.TrimSpace(output); got != want {
							t.Fatalf("%s child environment/cwd = %q, want %q", child, got, want)
						}
					}
					if err := cleanup(); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(sealedRepo); !os.IsNotExist(err) {
						t.Fatalf("sealed checkout survived cleanup: %v", err)
					}
				}
			}
			if os.Getenv("GOCACHE") != inheritedCache || os.Getenv("GOTOOLCHAIN") != "auto" {
				t.Fatal("child environment selection mutated the parent environment")
			}
		})
	}
}
