package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual driver in a Git fixture, including its package discovery.
// A copy of ignored evidence must not become source merely because a ceremony
// preserves it beside the source checkout.
func TestSourceGatesIgnoreCopiedEvidence(t *testing.T) {
	for _, mode := range []string{"test", "vet", "build"} {
		t.Run(mode, func(t *testing.T) {
			repo := sourceFixture(t)
			sourceWrite(t, repo, "evidence/snapshot/broken.go", "package archived\nvar _ = MissingArchiveHelper\n")
			sourceWrite(t, repo, "testdata/broken/broken.go", "package fixture\nvar _ = MissingFixtureHelper\n")
			sourceWrite(t, repo, "platform/other.go", "//go:build sourceinventoryfixture\n\npackage platform\n")
			sourceWrite(t, repo, "onlytests/only_test.go", "package onlytests\nimport \"testing\"\nfunc TestOnly(t *testing.T) {}\n")
			sourceWrite(t, repo, "internal/work/value.generated.go", "package work\nfunc generated() int { return 7 }\n")
			sourceWrite(t, repo, "internal/work/work.go", "package work\nvar Value = generated()\n")
			out, err := sourceRun(t, repo, mode)
			if err != nil {
				t.Fatalf("source gate %s treated copied evidence as source: %v\n%s", mode, err, out)
			}
			if mode == "test" && !strings.Contains(out, "compound/onlytests") {
				t.Fatalf("test-only new package was dropped: %s", out)
			}
		})
	}
}

func TestSourceGatesRetainNewSourceFailures(t *testing.T) {
	for _, ownership := range []string{"untracked", "staged", "tracked-ignored"} {
		for _, mode := range []string{"test", "vet", "build"} {
			t.Run(ownership+"/"+mode, func(t *testing.T) {
				repo := sourceFixture(t)
				name := "newsource/broken.go"
				if ownership == "tracked-ignored" {
					name = "evidence/owned/broken.go"
				}
				sourceWrite(t, repo, name, "package newsource\nvar _ = MissingOwnedHelper\n")
				if ownership != "untracked" {
					sourceGit(t, repo, "add", "-f", "--", name)
				}
				out, err := sourceRun(t, repo, mode)
				if err == nil || !strings.Contains(out, "MissingOwnedHelper") {
					t.Fatalf("real source failure hidden or substituted: error=%v\n%s", err, out)
				}
			})
		}
	}
}

func TestSourceGateRetainsRealTestAndVetFailures(t *testing.T) {
	for _, mode := range []string{"test", "vet"} {
		t.Run(mode, func(t *testing.T) {
			repo := sourceFixture(t)
			body := "package newsource\nimport \"testing\"\nfunc TestRed(t *testing.T) { t.Fatal(\"owned-test-falsifier\") }\n"
			marker := "owned-test-falsifier"
			if mode == "vet" {
				body = "package newsource\nimport \"fmt\"\nfunc Bad() { fmt.Printf(\"%d\", \"wrong\") }\n"
				marker = "fmt.Printf format %d"
			}
			sourceWrite(t, repo, "newsource/source_test.go", body)
			out, err := sourceRun(t, repo, mode)
			if err == nil || !strings.Contains(out, marker) {
				t.Fatalf("real %s failure hidden or substituted: error=%v\n%s", mode, err, out)
			}
		})
	}
}

func sourceFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                      "module compound\n\ngo 1.23\n",
		".gitignore":                  "/evidence/\n*.generated.go\n",
		"internal/home/home.go":       "package home\n",
		"internal/release/release.go": "package release\n",
		"internal/work/work.go":       "package work\n",
	} {
		sourceWrite(t, repo, name, body)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate production driver")
	}
	// Include the production phase policy used by the copied driver.
	phasePolicy, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "internal", "proofscope", "test_phases.go"))
	if err != nil {
		t.Fatal(err)
	}
	sourceWrite(t, repo, "internal/proofscope/test_phases.go", string(phasePolicy))
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(filepath.Dir(file), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sourceWrite(t, repo, "scripts/testbattery/"+entry.Name(), string(body))
	}
	sourceGit(t, repo, "init", "-q")
	sourceGit(t, repo, "add", "--", ".gitignore", "go.mod", "internal", "scripts")
	return repo
}

func TestSourceInventoryResolvesOneOwnedPackageSet(t *testing.T) {
	repo := sourceFixture(t)
	sourceWrite(t, repo, "evidence/archive/broken.go", "package archived\nvar _ = MissingArchiveHelper\n")
	sourceWrite(t, repo, "evidence/owned/owned.go", "package owned\n")
	sourceGit(t, repo, "add", "-f", "--", "evidence/owned/owned.go")
	sourceWrite(t, repo, "newsource/new.go", "package newsource\n")
	sourceWrite(t, repo, "onlytests/only_test.go", "package onlytests\n")
	sourceWrite(t, repo, "tagged/tagged.go", "//go:build sourceinventoryfixture\n\npackage tagged\n")
	for _, dir := range []string{"testdata", ".hidden", "_hidden", "vendor"} {
		sourceWrite(t, repo, dir+"/sample/broken.go", "package fixture\nvar _ = MissingFixtureHelper\n")
	}
	t.Chdir(repo)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	got, err := discoverSourcePackages(t.Context(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := sourcePackageInventory{
		All:   []string{"compound/evidence/owned", "compound/internal/home", "compound/internal/proofscope", "compound/internal/release", "compound/internal/work", "compound/newsource", "compound/onlytests", "compound/scripts/testbattery"},
		Build: []string{"compound/evidence/owned", "compound/internal/home", "compound/internal/proofscope", "compound/internal/release", "compound/internal/work", "compound/newsource", "compound/scripts/testbattery"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source inventory = %+v; want %+v", got, want)
	}
	t.Setenv("GOFLAGS", "-tags=sourceinventoryfixture")
	got, err = discoverSourcePackages(t.Context(), io.Discard)
	if err != nil || !strings.Contains(strings.Join(got.All, "\n"), "compound/tagged") ||
		!strings.Contains(strings.Join(got.Build, "\n"), "compound/tagged") {
		t.Fatalf("active build tag lost: inventory=%+v error=%v", got, err)
	}
}

func TestSourceGatesRetainIgnoredFilesInsideOwnedPackages(t *testing.T) {
	for _, mode := range []string{"test", "vet", "build"} {
		t.Run(mode, func(t *testing.T) {
			repo := sourceFixture(t)
			sourceWrite(t, repo, "internal/work/bad.generated.go", "package work\nvar _ = MissingGeneratedHelper\n")
			out, err := sourceRun(t, repo, mode)
			if err == nil || !strings.Contains(out, "MissingGeneratedHelper") {
				t.Fatalf("owned package compiler inputs hidden: %v\n%s", err, out)
			}
		})
	}
}

func TestSourceInventoryRefusesUnsupportedOrMissingSource(t *testing.T) {
	for _, scenario := range []string{"nested-module", "missing-file", "symlink-file", "symlink-directory", "outside-root", "malformed-source"} {
		t.Run(scenario, func(t *testing.T) {
			repo := sourceFixture(t)
			want := ""
			switch scenario {
			case "nested-module":
				sourceWrite(t, repo, "nested/go.mod", "module nested.example/source\n\ngo 1.23\n")
				sourceWrite(t, repo, "nested/source.go", "package nested\n")
				want = "unsupported nested source module"
			case "missing-file":
				if err := os.Remove(filepath.Join(repo, "internal/work/work.go")); err != nil {
					t.Fatal(err)
				}
				want = "source file"
			case "symlink-file":
				if err := os.Symlink("work.go", filepath.Join(repo, "internal/work/link.go")); err != nil {
					t.Fatal(err)
				}
				want = "not a regular file"
			case "symlink-directory":
				if err := os.Rename(filepath.Join(repo, "internal/work"), filepath.Join(repo, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../moved", filepath.Join(repo, "internal/work")); err != nil {
					t.Fatal(err)
				}
				want = "not a real directory"
			case "outside-root":
				repo = filepath.Join(repo, "internal")
				want = "Git repository root"
			case "malformed-source":
				sourceWrite(t, repo, "malformed/bad.go", "this is not Go\n")
				want = "expected 'package'"
			}
			t.Chdir(repo)
			t.Setenv("GOWORK", "off")
			_, err := discoverSourcePackages(t.Context(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("scenario %s: error=%v; want %q", scenario, err, want)
			}
		})
	}
}

func TestSourceInventoryWorksInCloneAndLinkedWorktree(t *testing.T) {
	for _, checkout := range []string{"clone", "worktree"} {
		t.Run(checkout, func(t *testing.T) {
			repo := sourceFixture(t)
			sourceGit(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
				"-c", "commit.gpgsign=false", "commit", "-qm", "source fixture")
			target := filepath.Join(t.TempDir(), "checkout")
			if checkout == "clone" {
				sourceGit(t, repo, "clone", "--quiet", "--no-local", repo, target)
			} else {
				sourceGit(t, repo, "worktree", "add", "--quiet", "--detach", target, "HEAD")
			}
			out, err := sourceRun(t, target, "vet")
			if err != nil {
				t.Fatalf("%s: %v\n%s", checkout, err, out)
			}
		})
	}
}

func TestSourceGateUsesInventoryProjectionsAndStopsOnDiscoveryFailure(t *testing.T) {
	inventory := sourcePackageInventory{
		All:   []string{"compound/internal/home", "compound/internal/release", "compound/internal/work", "compound/onlytests"},
		Build: []string{"compound/internal/home", "compound/internal/release", "compound/internal/work"},
	}
	for _, mode := range []string{"test", "vet", "build"} {
		t.Run(mode, func(t *testing.T) {
			discoveries := 0
			var calls [][]string
			discover := func(context.Context, io.Writer) (sourcePackageInventory, error) {
				discoveries++
				return inventory, nil
			}
			run := func(_ context.Context, args []string, _, _ io.Writer) error {
				calls = append(calls, append([]string(nil), args...))
				return nil
			}
			if err := runSourceGate(t.Context(), mode, discover, run, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			var want [][]string
			switch mode {
			case "test":
				want = [][]string{
					{"test", "-count=1", "-timeout=40m", "compound/onlytests"},
					{"test", "-count=1", "-timeout=40m", "compound/internal/work"},
					{"test", "-count=1", "-timeout=40m", "compound/internal/release"},
					{"test", "-count=1", "-timeout=40m", "compound/internal/home"},
				}
			case "vet":
				want = [][]string{append([]string{"vet"}, inventory.All...)}
			case "build":
				want = [][]string{append([]string{"build"}, inventory.Build...)}
			}
			if discoveries != 1 || !reflect.DeepEqual(calls, want) {
				t.Fatalf("discoveries=%d calls=%v; want %v", discoveries, calls, want)
			}
			for _, failure := range []string{"error", "cancel"} {
				t.Run(failure, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var out bytes.Buffer
					errDiscovery := errors.New("discovery failure")
					discover := func(context.Context, io.Writer) (sourcePackageInventory, error) {
						if failure == "cancel" {
							cancel()
							return inventory, nil
						}
						return sourcePackageInventory{}, errDiscovery
					}
					err := runSourceGate(ctx, mode, discover, func(context.Context, []string, io.Writer, io.Writer) error {
						t.Fatal("launched after discovery failure/cancellation")
						return nil
					}, &out, &out)
					if err == nil || out.Len() != 0 {
						t.Fatalf("error=%v output=%s", err, &out)
					}
				})
			}
		})
	}
}

func sourceWrite(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func sourceGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func sourceRun(t *testing.T, repo, mode string, extra ...string) (string, error) {
	t.Helper()
	// The mode is always explicit: a bare invocation is refused by the driver.
	args := []string{"run", "./scripts/testbattery", mode}
	args = append(args, extra...)
	cmd := exec.Command("go", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSealedSourceGatesRejectSourceCreatedByTests(t *testing.T) {
	repo := sourceFixture(t)
	sourceWrite(t, repo, "creates_source_test.go", "package fixture\nimport (\"os\"; \"testing\")\nfunc TestCreatesSource(t *testing.T) {\n"+
		"if err := os.MkdirAll(\"generated\", 0755); err != nil { t.Fatal(err) }\n"+
		"if err := os.WriteFile(\"generated/source.go\", []byte(\"package generated\\nvar _ = MissingUntestedHelper\\n\"), 0644); err != nil { t.Fatal(err) }\n}\n")
	sourceGit(t, repo, "add", "--", "creates_source_test.go")
	head := sourceCommitFixture(t, repo)
	sourceWrite(t, repo, "evidence/archived/bad.go", "package archived\nvar _ = MissingArchiveHelper\n")
	for _, mode := range []string{"test", "vet", "build"} {
		out, err := sourceRun(t, repo, mode, "--source-head", head)
		if err == nil || !strings.Contains(out, "source snapshot drift") || strings.Contains(out, "undefined: MissingUntestedHelper") {
			t.Fatalf("%s did not refuse changed source inventory: error=%v\n%s", mode, err, out)
		}
		if mode == "test" && !strings.Contains(out, "testbattery: phase 4/4") {
			t.Fatalf("drift control failed before the real test ran: %s", out)
		}
	}
}

func TestSealedSourceGatesAcceptCleanSnapshotAndRejectWrongHead(t *testing.T) {
	repo := sourceFixture(t)
	head := sourceCommitFixture(t, repo)
	sourceWrite(t, repo, "evidence/archived/bad.go", "package archived\nvar _ = MissingArchiveHelper\n")
	for _, mode := range []string{"test", "vet", "build"} {
		out, err := sourceRun(t, repo, mode, "--source-head", head)
		if err != nil {
			t.Fatalf("clean sealed %s: %v\n%s", mode, err, out)
		}
		out, err = sourceRun(t, repo, mode, "--source-head", strings.Repeat("0", 40))
		if err == nil || !strings.Contains(out, "HEAD no longer matches") {
			t.Fatalf("wrong sealed HEAD %s: %v\n%s", mode, err, out)
		}
	}
}

func sourceCommitFixture(t *testing.T, repo string) string {
	t.Helper()
	sourceGit(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "source fixture")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
