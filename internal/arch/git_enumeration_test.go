package arch

// Git enumeration determinism — the source inventory must not read ambient Git state.
//
// The doctor's source inventory is `git ls-files` over the repo it was asked to certify.
// The child Git process inherited the operator's environment, so two ambient inputs could
// change WHICH SOURCE FILES the doctor enumerated:
//
//	A. the CONFIG FILES git reads outside the checkout — the system-wide file,
//	   ~/.gitconfig and $XDG_CONFIG_HOME/git/config. A malformed one makes `git ls-files`
//	   fail, which moved the doctor's findings: the doc-leak scan failed closed and the
//	   private-body contract silently fell back to a filesystem walk;
//	B. core.fsmonitor, an EXTERNAL PROGRAM git runs while listing the index. An ambient
//	   (or repo-local) core.fsmonitor made the enumeration execute whatever helper it
//	   names.
//
// The controls below pin the fix: the source findings do not move, no helper runs, the
// tracked inventory stays exactly Git's, and the repo's OWN .git/config is still consulted.
//
// Every hostile input here is a disposable fixture: a temp repo, a temp HOME, a temp XDG
// config home, temp system/global config files and a temp helper script. Nothing reads
// or writes the operator's real Git configuration, and the raw controls run under an
// explicitly owned environment so an inherited GIT_* variable can neither redirect the
// fixture's own git commands nor mask the hostile globals under test.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAmbient is a disposable ambient Git world: a HOME and an XDG config home the test
// owns, disposable SYSTEM and GLOBAL config files, plus the marker an fsmonitor helper
// would write if git ever executed it.
type gitAmbient struct {
	home         string
	xdg          string
	systemConfig string
	globalConfig string
	marker       string
	helper       string
}

// newGitAmbient confines HOME and XDG_CONFIG_HOME to a disposable tree, confines the
// SYSTEM config to a disposable file and exports them for the rest of the test. It
// never touches the operator's real Git configuration.
func newGitAmbient(t *testing.T) *gitAmbient {
	t.Helper()
	root := t.TempDir()
	a := &gitAmbient{
		home:         filepath.Join(root, "home"),
		xdg:          filepath.Join(root, "xdg"),
		systemConfig: filepath.Join(root, "gitconfig-system"),
		globalConfig: filepath.Join(root, "gitconfig-global"),
		marker:       filepath.Join(root, "fsmonitor-helper-ran"),
		helper:       filepath.Join(root, "fsmonitor-helper.sh"),
	}
	for _, dir := range []string{a.home, filepath.Join(a.xdg, "git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The system config is a disposable EMPTY file: the fixture confines the system
	// config instead of letting the operator's real one be read, and the control that
	// needs a hostile system file writes that file itself.
	writeFixtureFile(t, a.systemConfig, "", 0o644)
	// The hostile helper: a program git would EXECUTE as the fsmonitor hook. It only
	// records that it ran and exits 0, so git's own error handling never masks the
	// observation. It derives the marker from its own directory, so no path quoting is
	// involved.
	writeFixtureFile(t, a.helper,
		"#!/bin/sh\n"+
			"printf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/fsmonitor-helper-ran\"\n"+
			"exit 0\n", 0o755)
	// The operator's shell must not be able to redirect the fixture's own git commands
	// or MASK the hostile globals below.
	ownGitEnvironment(t)
	t.Setenv("HOME", a.home)
	t.Setenv("XDG_CONFIG_HOME", a.xdg)
	t.Setenv("GIT_CONFIG_SYSTEM", a.systemConfig)
	return a
}

// ownGitEnvironment removes every inherited GIT_* variable from the test process's
// environment for the duration of the test and restores the originals afterwards. An
// exported GIT_DIR would redirect the fixture's own git commands; an exported
// GIT_CONFIG_GLOBAL=/dev/null would MASK the disposable globals under test, which
// would make their negative controls vacuous — the control would pass because the
// hostility was never live.
func ownGitEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		value, present := os.LookupEnv(name)
		t.Cleanup(func() {
			if present {
				if err := os.Setenv(name, value); err != nil {
					t.Errorf("restoring %s: %v", name, err)
				}
				return
			}
			if err := os.Unsetenv(name); err != nil {
				t.Errorf("restoring %s: %v", name, err)
			}
		})
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// writeGlobalConfig writes the same body into BOTH ambient global config locations git
// reads (the XDG one and ~/.gitconfig), so every phase of a control is equally ambient.
func (a *gitAmbient) writeGlobalConfig(t *testing.T, body string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(a.xdg, "git", "config"),
		filepath.Join(a.home, ".gitconfig"),
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// helperRan reports whether the hostile fsmonitor helper was executed.
func (a *gitAmbient) helperRan(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(a.marker)
	return err == nil
}

// clearHelperMarker removes the helper's marker so a later observation starts clean.
func (a *gitAmbient) clearHelperMarker(t *testing.T) {
	t.Helper()
	if err := os.Remove(a.marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// rawEnv returns the environment a RAW Git control runs under: an EXPLICITLY OWNED
// environment, not the operator's. Every inherited GIT_* variable is excluded — the
// repository and config injection variables — and the fixture's own disposable system
// config is supplied instead, so the control never reads the operator's real one. The
// disposable HOME and XDG globals are deliberately PRESERVED: they are the ambient
// configuration under test. The extra entries are appended last, so a control can
// supply its own hostile override.
func (a *gitAmbient) rawEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_CONFIG_SYSTEM="+a.systemConfig)
	return append(env, extra...)
}

// rawGitLSFiles runs `git ls-files -z` with NO determinism overrides, under the
// fixture's explicitly owned environment, so it inherits the ambient state a hostile
// shell would hand git. It is the control that proves a hostile ambient state is
// actually hostile — without it, "the doctor still worked" could just mean the
// hostility was never live.
func rawGitLSFiles(t *testing.T, ambient *gitAmbient, repo string, extra ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "ls-files", "-z")
	cmd.Env = ambient.rawEnv(extra...)
	return cmd.Output()
}

// gitStderr returns the stderr of a failed git command, for control messages.
func gitStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return err.Error()
}

// TestGitTrackedFilesIgnoresAmbientSystemAndGlobalConfig proves a malformed ambient
// config cannot change the tracked inventory — whether it is the SYSTEM config, the
// global config, or a config INJECTED through the environment. Before the fix a
// malformed file made `git ls-files` fail, so the doc-leak scan failed closed and the
// private-body contract fell back to a filesystem walk — the doctor's source findings
// moved on malformed AMBIENT state alone.
func TestGitTrackedFilesIgnoresAmbientSystemAndGlobalConfig(t *testing.T) {
	ambient := newGitAmbient(t)
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "tracked.txt", "tracked")
	runGit("add", "--", "tracked.txt")
	// An untracked file must stay invisible: the inventory is Git-owned, not a walk.
	mustWriteArchFixture(t, repo, "untracked.txt", "untracked")
	// The repo's OWN config stays consulted: the fix removes only the AMBIENT sources.
	runGit("config", "probe.localonlymarker", "present")

	const malformed = "[core]\n\tthis is not a valid config line\n"

	// Control 1 — the disposable SYSTEM config file alone is hostile. The globals are
	// untouched at this point, so a failure can only come from the system file.
	writeFixtureFile(t, ambient.systemConfig, malformed, 0o644)
	if out, err := rawGitLSFiles(t, ambient, repo); err == nil {
		t.Fatalf("control: a malformed ambient system config did not break a plain git ls-files (%s) — the hostility is not live, so this control would prove nothing", strings.TrimSpace(string(out)))
	}

	// Control 2 — the disposable global config alone is hostile. The system config is
	// put back into a well-formed state first, so this control isolates the globals.
	writeFixtureFile(t, ambient.systemConfig, "", 0o644)
	ambient.writeGlobalConfig(t, malformed)
	if out, err := rawGitLSFiles(t, ambient, repo); err == nil {
		t.Fatalf("control: a malformed ambient global config did not break a plain git ls-files (%s) — the hostility is not live, so this control would prove nothing", strings.TrimSpace(string(out)))
	}

	// Control 3 — an INJECTED config (the GIT_CONFIG_COUNT family) is hostile too: it
	// names the helper as core.fsmonitor, so a plain listing EXECUTES it. The globals
	// are well-formed again, so this control isolates the injection.
	writeFixtureFile(t, ambient.systemConfig, "", 0o644)
	ambient.writeGlobalConfig(t, "[core]\n\tautocrlf = false\n")
	if _, err := rawGitLSFiles(t, ambient, repo,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsmonitor",
		"GIT_CONFIG_VALUE_0="+ambient.helper,
	); err != nil {
		t.Fatalf("control: plain git ls-files failed under an injected config: %v\n%s", err, gitStderr(err))
	}
	if !ambient.helperRan(t) {
		t.Fatal("control: the injected config did NOT make a plain git ls-files execute the fsmonitor helper — the vector is not live, so this control would prove nothing")
	}
	ambient.clearHelperMarker(t)

	// The hostile overrides are now INHERITED by the enumeration child, exactly as an
	// operator's shell would export them: a hostile system file, a hostile global file
	// and an injected config. Every one of them is disposable.
	writeFixtureFile(t, ambient.systemConfig, malformed, 0o644)
	writeFixtureFile(t, ambient.globalConfig, malformed, 0o644)
	t.Setenv("GIT_CONFIG_SYSTEM", ambient.systemConfig)
	t.Setenv("GIT_CONFIG_GLOBAL", ambient.globalConfig)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", ambient.helper)

	// The child's environment is EXACTLY the prescribed one: the inherited overrides
	// are replaced by the deterministic values and the injected config is gone.
	assertChildGitEnv(t, gitSourceEnumerationEnv())

	tracked, err := gitTrackedFiles(repo)
	if err != nil {
		t.Fatalf("gitTrackedFiles failed under a malformed ambient global config: %v", err)
	}
	if len(tracked) != 1 || tracked[0] != "tracked.txt" {
		t.Fatalf("tracked inventory changed under a malformed ambient global config: %+v", tracked)
	}
	if ambient.helperRan(t) {
		t.Fatal("gitTrackedFiles executed the injected fsmonitor helper (marker exists)")
	}

	// The repo's OWN config is still consulted — the fix removes only the AMBIENT
	// sources — and no ambient config file is.
	cmd := exec.Command("git", "-C", repo, "config", "--list", "--show-origin")
	cmd.Env = gitSourceEnumerationEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the enumeration child could not read the repo's own config: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "probe.localonlymarker=present") {
		t.Fatalf("the enumeration child no longer reads the repo's own config:\n%s", text)
	}
	for _, hostile := range []string{
		ambient.systemConfig,
		ambient.globalConfig,
		filepath.Join(ambient.home, ".gitconfig"),
		filepath.Join(ambient.xdg, "git", "config"),
	} {
		if strings.Contains(text, hostile) {
			t.Fatalf("the enumeration child reads the ambient config %s", hostile)
		}
	}
}

// TestGitTrackedFilesChildReadsOnlyTheRepoConfig pins the determinism contract of the
// enumeration child: it consults the repo's OWN .git/config and NOTHING outside the
// checkout. The repo-local key proves the local config is still read (the fix removes
// only the AMBIENT sources); the origin list proves no system or global config file is
// consulted at all, so neither a malformed nor a hostile one can reach the enumeration.
func TestGitTrackedFilesChildReadsOnlyTheRepoConfig(t *testing.T) {
	ambient := newGitAmbient(t)
	repo, runGit := newDocLeakGitRepo(t)
	runGit("config", "probe.localonlymarker", "present")
	ambient.writeGlobalConfig(t, "[core]\n\tthis is not a valid config line\n")

	cmd := exec.Command("git", "-C", repo, "config", "--list", "--show-origin")
	cmd.Env = gitSourceEnumerationEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the enumeration child could not read the repo's own config: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "probe.localonlymarker=present") {
		t.Fatalf("the enumeration child no longer reads the repo's own config:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "file:") {
			continue
		}
		origin := strings.TrimPrefix(strings.Fields(line)[0], "file:")
		if origin != ".git/config" {
			t.Fatalf("the enumeration child reads a config file outside the repo: %s", origin)
		}
	}
	for _, hostile := range []string{
		filepath.Join(ambient.home, ".gitconfig"),
		filepath.Join(ambient.xdg, "git", "config"),
	} {
		if strings.Contains(text, hostile) {
			t.Fatalf("the enumeration child reads the ambient global config %s", hostile)
		}
	}
}

// TestGitTrackedFilesDoesNotExecuteAmbientFsmonitorHelper proves the enumeration never
// executes an EXTERNAL PROGRAM named by an ambient core.fsmonitor. `git ls-files` runs
// the fsmonitor hook while listing the index, so before the fix an ambient
// core.fsmonitor made the doctor run whatever helper the operator's global config
// pointed at.
func TestGitTrackedFilesDoesNotExecuteAmbientFsmonitorHelper(t *testing.T) {
	ambient := newGitAmbient(t)
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "tracked.txt", "tracked")
	runGit("add", "--", "tracked.txt")

	ambient.writeGlobalConfig(t, "[core]\n\tfsmonitor = "+ambient.helper+"\n")

	// Control: a plain `git ls-files` really does execute the hook.
	if _, err := rawGitLSFiles(t, ambient, repo); err != nil {
		t.Fatalf("control: plain git ls-files failed: %v", err)
	}
	if !ambient.helperRan(t) {
		t.Fatal("control: the ambient fsmonitor helper was NOT executed by a plain git ls-files — the vector is not live, so this control would prove nothing")
	}
	ambient.clearHelperMarker(t)

	if _, err := gitTrackedFiles(repo); err != nil {
		t.Fatalf("gitTrackedFiles failed: %v", err)
	}
	if ambient.helperRan(t) {
		t.Fatal("gitTrackedFiles executed the ambient fsmonitor helper (marker exists)")
	}

	// The source qualification must not execute it either.
	report := DoctorSource(repo, true)
	if ambient.helperRan(t) {
		t.Fatalf("DoctorSource executed the ambient fsmonitor helper (marker exists): %+v", report.Findings)
	}
}

// TestGitTrackedFilesDoesNotExecuteLocalFsmonitorHelper covers the repo's OWN
// .git/config: git consults it, so a core.fsmonitor set there is a live vector too. The
// control proves the repo-local config is still consulted (the plain listing runs the
// hook it names), and the enumeration proves the hook is never executed.
func TestGitTrackedFilesDoesNotExecuteLocalFsmonitorHelper(t *testing.T) {
	ambient := newGitAmbient(t)
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "tracked.txt", "tracked")
	runGit("add", "--", "tracked.txt")
	runGit("config", "core.fsmonitor", ambient.helper)

	// Control: the repo's own config IS consulted — a plain `git ls-files` runs the
	// hook it names. That is what makes the local case a real vector.
	if _, err := rawGitLSFiles(t, ambient, repo); err != nil {
		t.Fatalf("control: plain git ls-files failed: %v", err)
	}
	if !ambient.helperRan(t) {
		t.Fatal("control: the repo-local fsmonitor helper was NOT executed by a plain git ls-files — the vector is not live, so this control would prove nothing")
	}
	ambient.clearHelperMarker(t)

	tracked, err := gitTrackedFiles(repo)
	if err != nil {
		t.Fatalf("gitTrackedFiles failed: %v", err)
	}
	if ambient.helperRan(t) {
		t.Fatal("gitTrackedFiles executed the repo-local fsmonitor helper (marker exists)")
	}
	if len(tracked) != 1 || tracked[0] != "tracked.txt" {
		t.Fatalf("tracked inventory changed: %+v", tracked)
	}
}

// TestDoctorSourceFindingsUnchangedUnderHostileAmbientGitState is the end-to-end
// control: the source findings of this repo must be IDENTICAL under a well-formed
// ambient config, a malformed one and an ambient fsmonitor helper — and no helper may
// run. This is the property the source qualification depends on: a hostile ambient
// Git state must not change what the doctor says about the source.
func TestDoctorSourceFindingsUnchangedUnderHostileAmbientGitState(t *testing.T) {
	ambient := newGitAmbient(t)

	// Phase 1 — a well-formed ambient config: the baseline source findings.
	ambient.writeGlobalConfig(t, "[core]\n\tautocrlf = false\n")
	baseline := DoctorSource(repoRoot, true)
	if len(baseline.Findings) == 0 {
		t.Fatal("the baseline source report is empty — the source inventory was not exercised")
	}

	// Phase 2 — a malformed ambient global config.
	ambient.writeGlobalConfig(t, "[core]\n\tthis is not a valid config line\n")
	assertSameSourceFindings(t, "a malformed ambient global config", baseline, DoctorSource(repoRoot, true))

	// Phase 3 — an ambient fsmonitor helper.
	ambient.writeGlobalConfig(t, "[core]\n\tfsmonitor = "+ambient.helper+"\n")
	assertSameSourceFindings(t, "an ambient fsmonitor helper", baseline, DoctorSource(repoRoot, true))
	if ambient.helperRan(t) {
		t.Fatal("DoctorSource executed the ambient fsmonitor helper (marker exists)")
	}
}

// TestGitTrackedFilesNonGitRootStillErrors preserves the existing failure semantics: a
// root that is not a Git checkout must still return an error, so the callers keep their
// fail-closed / fallback behaviour.
func TestGitTrackedFilesNonGitRootStillErrors(t *testing.T) {
	if _, err := gitTrackedFiles(t.TempDir()); err == nil {
		t.Fatal("gitTrackedFiles reported success for a root that is not a Git checkout")
	}
}

// assertChildGitEnv asserts the enumeration child's environment carries EXACTLY the
// prescribed Git overrides: the three deterministic values and nothing else. Any
// inherited override that survived — GIT_CONFIG_GLOBAL, the injected GIT_CONFIG_COUNT
// family, GIT_DIR — would show up as an extra entry and fail here.
func assertChildGitEnv(t *testing.T, env []string) {
	t.Helper()
	got := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		got[name] = value
	}
	want := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("the enumeration child's %s is %q, want %q (full Git environment: %+v)", name, got[name], value, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the enumeration child's Git environment carries %d entries, want exactly %d: %+v", len(got), len(want), got)
	}
}

// assertSameSourceFindings asserts two source reports carry byte-identical findings.
func assertSameSourceFindings(t *testing.T, phase string, want, got Report) {
	t.Helper()
	if len(want.Findings) != len(got.Findings) {
		t.Fatalf("source finding count moved under %s: %d -> %d\nwant=%+v\ngot =%+v",
			phase, len(want.Findings), len(got.Findings), want.Findings, got.Findings)
	}
	for i := range want.Findings {
		if want.Findings[i] != got.Findings[i] {
			t.Fatalf("source finding %d moved under %s:\nwant=%+v\ngot =%+v",
				i, phase, want.Findings[i], got.Findings[i])
		}
	}
	if want.OK != got.OK {
		t.Fatalf("source OK moved under %s: %v -> %v", phase, want.OK, got.OK)
	}
}
