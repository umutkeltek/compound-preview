//go:build darwin

package work

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
)

func TestHomeFixtureRestrictedRunnerExecutesThenReuses(t *testing.T) {
	f := newHomeProofFixture(t)
	temporary := newIsolatedHomeFixtureTestRoot(t)
	cacheRoot, err := filepath.EvalSymlinks(temporary)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(cacheRoot, "fixture-cache")
	_, file, _, _ := runtime.Caller(0)
	helper, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../scripts/testbattery/home_fixture.py"))
	if err != nil {
		t.Fatal(err)
	}
	// Give the committed synthetic fixture stable ancestor inputs while the real
	// sandbox, controls, execution and native reuse lifecycle remain exercised.
	// The unmodified helper's full host observations are tested separately below;
	// ResolveHomeFixtureReuse also rejects any changed ancestor fingerprint.
	needle := "    rows = []\n    for path in sorted(paths):"
	if strings.Count(string(helper), needle) != 1 {
		t.Fatal("synthetic ancestor observation seam changed")
	}
	scoped := "    boundary = Path(" + strconv.Quote(cacheRoot) + ")\n    paths = {path for path in paths if path == boundary or boundary in path.parents}\n" + needle
	helper = []byte(strings.Replace(string(helper), needle, scoped, 1))
	fixtures := map[string][]byte{
		"go.mod":                              []byte("module compound\n\ngo 1.23\n"),
		"SCHEMA.md":                           []byte("synthetic schema fixture\n"),
		"scripts/testbattery/home_fixture.py": helper,
		"internal/home/home_test.go": []byte(`package home
import("errors";"fmt";"os";"os/exec";"syscall";"testing")
func TestFixture(t *testing.T){
 for _,fd:=range []int{3,4,5}{
 marker:=make([]byte,64);n,e:=syscall.Pread(fd,marker,0)
 if e!=nil||n<0||string(marker[:n])!="known-liveness-descriptor"{t.Fatalf("proof barrier descriptor %d lost: %v",fd,e)}
 }
 if os.Getenv("FIXTURE_CHILD")=="1"{return}
 executable,e:=os.Executable();if e!=nil{t.Fatal(e)}
 child:=exec.Command(executable,"-test.run=^TestFixture$");child.Env=append(os.Environ(),"FIXTURE_CHILD=1")
 if output,e:=child.CombinedOutput();e!=nil{t.Fatalf("descendant lost proof barrier: %v %s",e,output)}

 if os.Getenv("COMPOUND_HOME")!=""||os.Getenv("COMPOUND_TEST_SECRET")!=""{t.Fatal("ambient environment leaked")}
 if _,e:=os.ReadFile("../../SCHEMA.md");e!=nil{t.Fatal(e)}
 if e:=os.WriteFile("../../SCHEMA.md",[]byte("bad"),0600);!errors.Is(e,syscall.EPERM)&&!errors.Is(e,syscall.EACCES){t.Fatalf("input write: %v",e)}
 if e:=os.WriteFile(t.TempDir()+"/positive",[]byte("ok"),0600);e!=nil{t.Fatal(e)}
 fmt.Println("FRESH_HOME_EXECUTION")
}
`),
	}
	for name, body := range fixtures {
		path := filepath.Join(f.repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		runProofSourceGit(t, f.repo, "add", name)
	}
	runProofSourceGit(t, f.repo, "commit", "-m", "synthetic restricted home fixture")
	f.source, err = gitrepository.CaptureCleanSnapshot(f.repo, f.source.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	originalCache, originalNow := homeFixtureCacheBase, homeFixtureNow
	homeFixtureCacheBase = func() (string, error) { return cache, nil }
	homeFixtureNow = func() time.Time { return fixedNow.Add(20 * time.Second) }
	defer func() { homeFixtureCacheBase, homeFixtureNow = originalCache, originalNow }()
	ctx := ContextWithHomeFixtureReuse(t.Context(), f.store.home, f.workID, &f.source)
	barrierPath := filepath.Join(t.TempDir(), "liveness")
	if err := os.WriteFile(barrierPath, []byte("known-liveness-descriptor"), 0600); err != nil {
		t.Fatal(err)
	}
	barrier, err := os.Open(barrierPath)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.ExtraFiles = []*os.File{barrier, barrier, barrier}
		return cmd
	}
	t.Setenv("COMPOUND_TEST_SECRET", "must-not-enter-fixture")
	first, evidence, err := RunHomeFixtureGate(ctx, f.repo, homeProofTestGate(f.source), os.Environ(), command)
	if err != nil {
		t.Fatalf("fresh restricted phase: %v\n%s", err, first)
	}
	if evidence == nil || evidence.OriginVerificationRef != "" || !strings.Contains(string(first), "FRESH_HOME_EXECUTION") {
		t.Fatalf("fresh evidence=%+v output=%s", evidence, first)
	}
	completed, err := f.complete(t, f.repo, evidence, true, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx = ContextWithHomeFixtureReuse(t.Context(), f.store.home, f.workID, &f.source, f.repo)
	// This sibling is outside the synthetic fixture's observation boundary.
	unrelated, err := os.MkdirTemp(filepath.Dir(cacheRoot), "ambient-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(unrelated); err != nil {
			t.Error(err)
		}
	})
	second, reused, err := RunHomeFixtureGate(ctx, f.repo, homeProofTestGate(f.source), os.Environ(), command)
	if err != nil {
		t.Fatalf("second phase: %v\n%s", err, second)
	}
	if reused == nil || reused.OriginVerificationRef != completed.VerificationRef || strings.Contains(string(second), "FRESH_HOME_EXECUTION") {
		t.Fatalf("identical fixture was not reused: first=%+v evidence=%+v output=%s", evidence, reused, second)
	}
	if !strings.Contains(string(second), "payload retired") {
		t.Fatalf("target gate did not retire fixture: %s", second)
	}
	if _, err := os.Stat(filepath.Join(evidence.Inputs.StageDirectory, "home.test")); !os.IsNotExist(err) {
		t.Fatalf("target fixture payload remains: %v", err)
	}
	if reused.InputSHA256 != evidence.InputSHA256 {
		t.Fatal("fixture input identity changed across identical preparation")
	}
	// Model the helper's successful full execution with ancestor drift. The
	// adapter must retain this fresh result without certifying it or replaying Go.
	freshExecutions, fallbackExecutions := 0, 0
	driftCommand := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "go" {
			fallbackExecutions++
		}
		cmd := command(ctx, name, args...)
		if name == "/usr/bin/python3" && len(args) > 2 && args[2] == "execute" {
			freshExecutions++
			wrapped := append([]string{"-c", `"$@"; result=$?; if [ "$result" -eq 0 ]; then exit 76; fi; exit "$result"`, "fresh-drift"}, name)
			wrapped = append(wrapped, args...)
			cmd = exec.CommandContext(ctx, "/bin/sh", wrapped...)
			cmd.ExtraFiles = []*os.File{barrier, barrier, barrier}
		}
		return cmd
	}
	third, uncertified, err := RunHomeFixtureGate(ctx, f.repo, homeProofTestGate(f.source), os.Environ(), driftCommand)
	if err != nil || uncertified != nil || freshExecutions != 1 || fallbackExecutions != 0 || !strings.Contains(string(third), "FRESH_HOME_EXECUTION") {
		t.Fatalf("fresh drift replayed or certified: fresh=%d fallback=%d evidence=%+v err=%v output=%s", freshExecutions, fallbackExecutions, uncertified, err, third)
	}

}

func TestHomeFixtureHelperFilesystemSafety(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	helper := filepath.Join(filepath.Dir(file), "../../scripts/testbattery/home_fixture_test.py")
	cmd := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", helper)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture safety: %v\n%s", err, output)
	}
}

// Initialize a stable private sibling before sealing a production fixture.
// Later per-run churn changes neither production ancestors nor the OS cache;
// first-time parent creation legitimately invalidates any older home seal.
func newIsolatedHomeFixtureTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".compound-proof-fixture-tests")
	if err := os.Mkdir(base, 0700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	info, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
		t.Fatal("test fixture parent is not a private owned directory")
	}
	temporary, err := os.MkdirTemp(base, "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(temporary); err != nil {
			t.Error(err)
		}
	})
	return temporary
}

func TestHomeFixtureTestCachePreservesProductionAncestors(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Run("initialize", func(t *testing.T) { newIsolatedHomeFixtureTestRoot(t) })
	before, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("next-run", func(t *testing.T) { newIsolatedHomeFixtureTestRoot(t) })
	after, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	previous, current := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		previous.Ino != current.Ino || previous.Ctimespec != current.Ctimespec {
		t.Fatal("test fixture creation or cleanup changed a sealed production ancestor")
	}
}

// Concurrent harness state and OS caches must not invalidate the production
// fixture; every directory it can actually observe remains sealed.
func TestHomeFixtureProductionRootAvoidsHarnessAndSystemCacheChurn(t *testing.T) {
	temporary := newIsolatedHomeFixtureTestRoot(t)
	home := filepath.Join(temporary, "synthetic-home")
	cache := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	previousStat := homeFixtureSharedParentStat
	homeFixtureSharedParentStat = func(path string) (os.FileInfo, error) {
		if path != filepath.Join(string(filepath.Separator), "Users", "Shared") {
			t.Fatalf("unexpected shared parent: %s", path)
		}
		return fixtureParentInfo{mode: os.ModeDir | os.ModeSticky | 0777, uid: 0}, nil
	}
	t.Cleanup(func() { homeFixtureSharedParentStat = previousStat })
	base, err := homeFixtureCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(temporary, "Shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	var stage string
	switch filepath.Dir(base) {
	case filepath.Join(string(filepath.Separator), "Users", "Shared"):
		stage = filepath.Join(shared, filepath.Base(base), "synthetic-stage")
	case home:
		stage = filepath.Join(base, "synthetic-stage")
	default:
		t.Fatalf("unsupported production fixture ancestry: %s", base)
	}
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	testStage := newIsolatedHomeFixtureTestRoot(t)
	_, file, _, _ := runtime.Caller(0)
	helper := filepath.Join(filepath.Dir(file), "../../scripts/testbattery/home_fixture.py")
	script := `import importlib.util, os, sys
from pathlib import Path
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("fixture", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
stage, test_stage, home, root = [Path(value).resolve() for value in sys.argv[2:]]
shared = root / "Shared"
hot = home / "Library" / "Caches"
legacy_home = home / ".legacy-proof-cache" / "stage"
legacy_cache = hot / "legacy-fixture" / "stage"
legacy_home.mkdir(parents=True)
legacy_cache.mkdir(parents=True)
state = home / "harness-state.json"
state.write_bytes(b"old state")
encode = m.encoded
collected = []
def capture(rows):
    collected[:] = rows
    return encode(rows)
m.encoded = capture
def owned_rows(path):
    m.ancestor_digest(path)
    return [row for row in collected
            if Path(row[0]) == root or root in Path(row[0]).parents]
before = owned_rows(stage)
old_home_before = owned_rows(legacy_home)
assert before and old_home_before, "synthetic ancestor observation missing"
replacement = home / "next-state.json"
replacement.write_bytes(b"new state")
os.replace(replacement, state)
assert before == owned_rows(stage), "harness home replacement invalidated shared fixture"
assert old_home_before != owned_rows(legacy_home), "control missed harness home replacement"
before, test_before, old_cache_before = owned_rows(stage), owned_rows(test_stage), owned_rows(legacy_cache)
(hot / "system-cache").write_bytes(b"replacement")
assert before == owned_rows(stage), "OS cache churn invalidated shared fixture"
assert test_before == owned_rows(test_stage), "OS cache churn invalidated test fixture"
assert old_cache_before != owned_rows(legacy_cache), "control missed OS cache churn"
full_before = m.ancestor_digest(stage)
(root.parent / ("external-" + root.name)).mkdir()
assert m.ancestor_digest(stage) != full_before, "production helper ignored host ancestors"
(root.parent / ("external-" + root.name)).rmdir()
before = owned_rows(stage)
(shared / "observable-entry").write_bytes(b"changed")
assert before != owned_rows(stage), "shared parent changes were not sealed"
before = owned_rows(stage)
(stage / "observable-entry").write_bytes(b"changed")
assert before != owned_rows(stage), "fixture stage changes were not sealed"
test_before = owned_rows(test_stage)
(test_stage / "observable-entry").write_bytes(b"changed")
assert test_before != owned_rows(test_stage), "test stage changes were not observed"
`
	cmd := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-c", script, helper, stage, testStage, home, temporary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture placement: %v\n%s", err, output)
	}
}

func TestHomeFixtureUnavailableSharedParentExecutesFresh(t *testing.T) {
	f := newHomeProofFixture(t)
	t.Setenv("GOFLAGS", "")
	previous := homeFixtureSharedParentStat
	resolved := 0
	homeFixtureSharedParentStat = func(path string) (os.FileInfo, error) {
		if path != filepath.Join(string(filepath.Separator), "Users", "Shared") {
			t.Fatalf("unexpected shared parent: %s", path)
		}
		resolved++
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { homeFixtureSharedParentStat = previous })
	step := homeProofTestGate(f.source)
	ctx := ContextWithHomeFixtureReuse(t.Context(), f.store.home, f.workID, &f.source)
	for _, tc := range []struct {
		name, script string
		success      bool
	}{
		{"fresh success", "printf fresh-source-result", true},
		{"fresh failure remains failure", "printf fresh-source-result; exit 7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				if name != step.Command[0] || strings.Join(args, "\x00") != strings.Join(step.Command[1:], "\x00") {
					t.Fatal("fallback changed the admitted source command")
				}
				return exec.CommandContext(ctx, "/bin/sh", "-c", tc.script)
			}
			output, evidence, err := RunHomeFixtureGate(ctx, f.repo, step, os.Environ(), command)
			if calls != 1 || evidence != nil || (err == nil) != tc.success {
				t.Fatalf("fresh fallback: calls=%d evidence=%v err=%v", calls, evidence, err)
			}
			if !strings.Contains(string(output), "executed fresh") || !strings.Contains(string(output), "fresh-source-result") {
				t.Fatalf("missing truthful fresh output: %s", output)
			}
			if !tc.success {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 7 {
					t.Fatalf("fresh exit was not preserved: %v", err)
				}
			}
		})
	}
	if resolved != 2 {
		t.Fatalf("shared parent checked %d times, want 2", resolved)
	}
}
