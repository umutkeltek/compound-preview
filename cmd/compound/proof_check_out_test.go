package main

// Falsifiers for `proof-check --out FILE`: the battery owns its output contract — it
// writes the full report to FILE itself, prints a final one-line verdict to stdout, and
// keeps a truthful exit code — retiring the `> out.json 2>&1; echo "EXIT=$?"` shell
// wrapper whose masked exit has lied green three times (2026-07-07, 2026-07-15,
// 2026-07-18; see .claude/skills/compound-land/SKILL.md §1). Packet:
// docs_private/plan/packets/2026-07-19-proofcheck-out-verdict.md (work_ad791a800a0c).

import (
	"bytes"
	"compound/internal/home"
	"compound/internal/work"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const proofCheckOutCLISubEnv = "COMPOUND_PROOFCHECK_OUT_SUB"

type injectedProofCheckCAS struct {
	delegate  proofCheckRootedCASPublication
	state     home.RootedFileCASState
	commitErr error
}

func (publication *injectedProofCheckCAS) State() home.RootedFileCASState {
	return publication.state
}

func (publication *injectedProofCheckCAS) Commit() error {
	if publication.commitErr != nil {
		publication.state = home.RootedFileCASAmbiguous
		return publication.commitErr
	}
	err := publication.delegate.Commit()
	publication.state = publication.delegate.State()
	return err
}

func (publication *injectedProofCheckCAS) Rollback() error {
	err := publication.delegate.Rollback()
	publication.state = publication.delegate.State()
	return err
}

func TestProofCheckGoRunInvocationsUseStableGateIdentity(t *testing.T) {
	t.Parallel()
	repoPath, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	run := func() proofCheckReport {
		t.Helper()
		// Isolated home: `go run` executes the real CLI dispatch, whose
		// governed-command admission requires a compatible home manifest —
		// bare CI/container $HOME failed before the report was produced.
		// The 90s budget covers go run's compile on cold 2-core runners
		// (the previous 15s deadline only held on warm dev machines).
		homePath := t.TempDir()
		if err := home.Init(homePath, "test", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx,
			"go", "run", "./cmd/compound",
			"proof-check", "--dry-run", "--json", "--repo", repoPath,
		)
		cmd.Dir = repoPath
		cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
		body, err := cmd.Output()
		if err != nil {
			t.Fatalf("go run proof-check: %v", err)
		}
		var report proofCheckReport
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatalf("decode go-run report: %v\n%s", err, body)
		}
		return report
	}
	first := run()
	second := run()
	if len(first.Steps) != len(second.Steps) {
		t.Fatalf("go-run gate count changed: %d vs %d", len(first.Steps), len(second.Steps))
	}
	for i := range first.Steps {
		if !reflect.DeepEqual(first.Steps[i].Command, second.Steps[i].Command) {
			t.Fatalf(
				"go-run gate %q identity changed across invocations: %q vs %q",
				first.Steps[i].Name,
				first.Steps[i].Command,
				second.Steps[i].Command,
			)
		}
		if len(first.Steps[i].Command) > 0 &&
			strings.Contains(first.Steps[i].Command[0], "go-build") {
			t.Fatalf("gate %q exposed temporary go-run executable identity %q", first.Steps[i].Name, first.Steps[i].Command[0])
		}
	}
}

func TestProofCheckInvocationIdentityIsLossless(t *testing.T) {
	t.Parallel()
	withSpace, err := work.ProofCheckInvocationIdentity([]string{"--repo", "/synthetic/repo with space", "--out", "/synthetic/report.json"})
	if err != nil {
		t.Fatal(err)
	}
	split, err := work.ProofCheckInvocationIdentity([]string{"--repo", "/synthetic/repo", "with", "space", "--out", "/synthetic/report.json"})
	if err != nil {
		t.Fatal(err)
	}
	if withSpace == split {
		t.Fatalf("lossy command identity collapsed distinct argv: %q", withSpace)
	}
}

func TestProofCheckInvocationIdentityBindsResolvedReportPath(t *testing.T) {
	t.Parallel()
	args := []string{"--repo", "/synthetic/repo", "--out", "proof.json"}
	first, err := work.ProofCheckInvocationIdentity(work.ProofCheckArgsWithResolvedOut(args, "/synthetic/one/proof.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := work.ProofCheckInvocationIdentity(work.ProofCheckArgsWithResolvedOut(args, "/synthetic/two/proof.json"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("report-path binding collapsed different resolved targets: %q", first)
	}
}

// F1: a failed-report verdict line names the FIRST failed gate and says FAILED with
// honest passed, skipped, failed, and total counts.
func TestProofCheckVerdictLineFailedNamesFirstFailedGate(t *testing.T) {
	t.Parallel()
	report := proofCheckReport{
		Status: "failed",
		Steps: []proofCheckStep{
			{Name: "go-test", Status: "ok"},
			{Name: "go-vet", Status: "failed"},
			{Name: "go-build", Status: "failed"},
			{Name: "gofmt", Status: "ok"},
		},
	}
	got := proofCheckVerdictLine(report, "/tmp/out.json")
	want := "battery: FAILED passed=2 skipped=0 failed=2 total=4 first_failed=go-vet out=/tmp/out.json"
	if got != want {
		t.Fatalf("verdict = %q, want %q", got, want)
	}
}

// F2: an ok-report verdict line reports the ok status with correct counts and path.
func TestProofCheckVerdictLineOkReportsCorrectCounts(t *testing.T) {
	t.Parallel()
	report := proofCheckReport{
		Status: "ok",
		Steps: []proofCheckStep{
			{Name: "go-test", Status: "ok"},
			{Name: "go-vet", Status: "ok"},
			{Name: "gofmt", Status: "skipped"},
		},
	}
	got := proofCheckVerdictLine(report, "/tmp/out.json")
	want := "battery: ok passed=2 skipped=1 failed=0 total=3 out=/tmp/out.json"
	if got != want {
		t.Fatalf("verdict = %q, want %q", got, want)
	}
}

func TestProofCheckVerdictLineFailedCountsSkippedSeparately(t *testing.T) {
	t.Parallel()
	report := proofCheckReport{
		Status: "failed",
		Steps: []proofCheckStep{
			{Name: "go-test", Status: "ok"},
			{Name: "console-smoke", Status: "skipped"},
			{Name: "go-vet", Status: "failed"},
			{Name: "console-build", Status: "skipped"},
		},
	}
	got := proofCheckVerdictLine(report, "/tmp/out.json")
	want := "battery: FAILED passed=1 skipped=2 failed=1 total=4 first_failed=go-vet out=/tmp/out.json"
	if got != want {
		t.Fatalf("verdict = %q, want %q", got, want)
	}
}

// F3: the --out file contains parseable JSON equal in content to the report (a
// marshal round-trip), matching the same shape --json prints to stdout.
func TestProofCheckWriteReportFileRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		DryRun:        false,
		ProofRefs:     []string{"go-test-count-1-all"},
		Steps: []proofCheckStep{
			{Name: "go-test", Command: []string{"go", "test", "./..."}, ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: 42},
		},
	}
	if err := proofCheckWriteReportFile(path, report); err != nil {
		t.Fatalf("write report file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got proofCheckReport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse written report: %v\n%s", err, raw)
	}
	if !reflect.DeepEqual(got, report) {
		t.Fatalf("round-tripped report = %+v, want %+v", got, report)
	}
	wantJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != string(wantJSON) {
		t.Fatalf("file content != MarshalIndent(report):\nfile: %s\nwant: %s", raw, wantJSON)
	}
}

func TestProofCheckWriteReportFilePreservesExistingMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o600, 0o640, 0o660, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte("prior evidence\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}

			if err := proofCheckWriteReportFile(path, proofCheckReport{
				SchemaVersion: proofCheckSchemaVersion,
				Status:        "ok",
				GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
				Repo:          "/repo",
				ProofRefs:     []string{},
				Steps:         []proofCheckStep{},
			}); err != nil {
				t.Fatalf("write report file: %v", err)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Fatalf("published mode = %04o, want preserved %04o", got, mode)
			}
		})
	}
}

func TestProofCheckWriteReportFileCreatesPrivateReport(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	if err := proofCheckWriteReportFile(path, proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}); err != nil {
		t.Fatalf("write report file: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new report mode = %04o, want 0600", got)
	}
}

func TestProofCheckOutRefusesLeafSymlinkWithoutTouchingDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination.json")
	link := filepath.Join(dir, "report.json")
	prior := []byte("owned destination evidence\n")
	if err := os.WriteFile(destination, prior, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}

	if err := proofCheckValidateOutPath(link); err == nil {
		t.Fatal("leaf symlink passed --out preflight")
	}
	if err := proofCheckWriteReportFile(link, proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}); err == nil {
		t.Fatal("leaf symlink accepted as report target")
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("report target mode = %s, want symlink preserved", linkInfo.Mode())
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, prior) {
		t.Fatalf("symlink destination bytes changed: got %q want %q", got, prior)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o640 {
		t.Fatalf("symlink destination mode = %04o, want 0640", gotMode)
	}
}

func TestProofCheckOutRefusesExistingNonRegularTargets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	targets := map[string]string{
		"directory": filepath.Join(dir, "directory"),
		"fifo":      filepath.Join(dir, "fifo"),
	}
	if err := os.Mkdir(targets["directory"], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(targets["fifo"], 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range targets {
		t.Run(name, func(t *testing.T) {
			if err := proofCheckValidateOutPath(path); err == nil {
				t.Fatalf("%s passed --out preflight", name)
			}
		})
	}
}

func TestProofCheckOutRefusesTargetRacesBeforePublish(t *testing.T) {
	originalHook := proofCheckBeforeOutPublish
	t.Cleanup(func() { proofCheckBeforeOutPublish = originalHook })

	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}

	t.Run("absent target appears", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "report.json")
		custody, err := proofCheckPrepareOutPath(path)
		if err != nil {
			t.Fatal(err)
		}
		raced := []byte("new unrelated target\n")
		proofCheckBeforeOutPublish = func(gotPath string) {
			if gotPath != path {
				t.Fatalf("publish hook path = %q, want %q", gotPath, path)
			}
			if err := os.WriteFile(path, raced, 0o640); err != nil {
				t.Fatal(err)
			}
		}

		if err := proofCheckWriteReportFileWithCustody(path, report, custody); err == nil {
			t.Fatal("publication overwrote target created after preflight")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, raced) {
			t.Fatalf("raced target bytes changed: got %q want %q", got, raced)
		}
	})

	t.Run("existing target replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(path, []byte("prior evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		custody, err := proofCheckPrepareOutPath(path)
		if err != nil {
			t.Fatal(err)
		}
		raced := []byte("replacement target\n")
		proofCheckBeforeOutPublish = func(string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raced, 0o640); err != nil {
				t.Fatal(err)
			}
		}

		if err := proofCheckWriteReportFileWithCustody(path, report, custody); err == nil {
			t.Fatal("publication overwrote replacement target")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, raced) {
			t.Fatalf("replacement target bytes changed: got %q want %q", got, raced)
		}
	})

	t.Run("existing target becomes symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		destination := filepath.Join(dir, "destination.json")
		if err := os.WriteFile(path, []byte("prior evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		destinationBytes := []byte("symlink destination\n")
		if err := os.WriteFile(destination, destinationBytes, 0o640); err != nil {
			t.Fatal(err)
		}
		custody, err := proofCheckPrepareOutPath(path)
		if err != nil {
			t.Fatal(err)
		}
		proofCheckBeforeOutPublish = func(string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(destination, path); err != nil {
				t.Fatal(err)
			}
		}

		if err := proofCheckWriteReportFileWithCustody(path, report, custody); err == nil {
			t.Fatal("publication replaced raced symlink")
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("raced target mode = %s, want symlink preserved", info.Mode())
		}
		got, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, destinationBytes) {
			t.Fatalf("symlink destination bytes changed: got %q want %q", got, destinationBytes)
		}
	})
}

func TestProofCheckOutPreservesRecoveryPreimageOnAmbiguousCASFailures(t *testing.T) {
	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}
	originalBegin := proofCheckBeginRootedFileCAS
	t.Cleanup(func() { proofCheckBeginRootedFileCAS = originalBegin })

	for _, tc := range []struct {
		name      string
		wrapBegin func(proofCheckRootedCASPublication) (proofCheckRootedCASPublication, error)
	}{
		{
			name: "begin ambiguous after exchange",
			wrapBegin: func(publication proofCheckRootedCASPublication) (proofCheckRootedCASPublication, error) {
				return &injectedProofCheckCAS{
					delegate: publication,
					state:    home.RootedFileCASAmbiguous,
				}, errors.New("injected ambiguous begin failure")
			},
		},
		{
			name: "commit ambiguous",
			wrapBegin: func(publication proofCheckRootedCASPublication) (proofCheckRootedCASPublication, error) {
				return &injectedProofCheckCAS{
					delegate:  publication,
					state:     publication.State(),
					commitErr: errors.New("injected ambiguous commit failure"),
				}, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.json")
			prior := []byte("prior complete report\n")
			if err := os.WriteFile(path, prior, 0o600); err != nil {
				t.Fatal(err)
			}
			custody, err := proofCheckPrepareOutPath(path)
			if err != nil {
				t.Fatal(err)
			}

			proofCheckBeginRootedFileCAS = func(
				root *os.Root,
				canonicalName,
				recoveryName string,
				expectedData []byte,
				expectedInfo os.FileInfo,
				replacementData []byte,
				replacementInfo os.FileInfo,
			) (proofCheckRootedCASPublication, error) {
				publication, err := originalBegin(
					root,
					canonicalName,
					recoveryName,
					expectedData,
					expectedInfo,
					replacementData,
					replacementInfo,
				)
				if err != nil {
					return publication, err
				}
				return tc.wrapBegin(publication)
			}

			if err := proofCheckWriteReportFileWithCustody(path, report, custody); err == nil {
				t.Fatal("ambiguous rooted CAS failure returned success")
			}
			assertProofCheckRecoveryPreimage(t, dir, filepath.Base(path), prior)
		})
	}
}

func assertProofCheckRecoveryPreimage(t *testing.T, dir, base string, prior []byte) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		body, err := os.ReadFile(match)
		if err == nil && bytes.Equal(body, prior) {
			return
		}
	}
	t.Fatalf("ambiguous publication destroyed recovery preimage %q; candidates=%v", prior, matches)
}

func TestProofCheckOutParentSubstitutionCannotRedirectPublication(t *testing.T) {
	container := t.TempDir()
	parent := filepath.Join(container, "out")
	movedParent := filepath.Join(container, "original-out")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "report.json")
	prior := []byte("original report\n")
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	custody, err := proofCheckPrepareOutPath(path)
	if err != nil {
		t.Fatal(err)
	}

	originalHook := proofCheckBeforeOutPublish
	t.Cleanup(func() { proofCheckBeforeOutPublish = originalHook })
	raced := []byte("raced parent report\n")
	proofCheckBeforeOutPublish = func(string) {
		if err := os.Rename(parent, movedParent); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raced, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}
	if err := proofCheckWriteReportFileWithCustody(path, report, custody); err == nil {
		t.Fatal("publication followed substituted parent")
	}
	originalBody, err := os.ReadFile(filepath.Join(movedParent, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalBody, prior) {
		t.Fatalf("original report changed: got %q want %q", originalBody, prior)
	}
	racedBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(racedBody, raced) {
		t.Fatalf("raced report changed: got %q want %q", racedBody, raced)
	}
	temps, err := filepath.Glob(filepath.Join(movedParent, ".report.json.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("parent substitution leaked rooted temp files: %v", temps)
	}
}

func TestProofCheckOutRejectsSpecialPermissionBitsWithoutMutation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	prior := []byte("special-mode report\n")
	if err := os.WriteFile(path, prior, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not preserve setuid on regular files")
	}
	if err := proofCheckWriteReportFile(path, proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}); err == nil {
		t.Fatal("special permission bits were silently stripped")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, prior) {
		t.Fatalf("special-mode report bytes changed: got %q want %q", body, prior)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode()&os.ModeSetuid == 0 || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("special-mode report mode changed: before=%s after=%s", before.Mode(), after.Mode())
	}
}

func TestProofCheckOutRejectsSpecialPermissionBitsAddedBeforePublishWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	prior := []byte("ordinary-mode report\n")
	if err := os.WriteFile(path, prior, 0o700); err != nil {
		t.Fatal(err)
	}
	custody, err := proofCheckPrepareOutPath(path)
	if err != nil {
		t.Fatal(err)
	}

	originalBeforePublish := proofCheckBeforeOutPublish
	t.Cleanup(func() { proofCheckBeforeOutPublish = originalBeforePublish })
	proofCheckBeforeOutPublish = func(string) {
		if err := os.Chmod(path, 0o700|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSetuid == 0 {
			t.Skip("filesystem does not retain setuid on regular files")
		}
	}

	if err := proofCheckWriteReportFileWithCustody(path, proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}, custody); err == nil {
		t.Fatal("publication silently stripped special permission bits added after custody capture")
	}
	afterBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBody, prior) {
		t.Fatalf("special-mode race changed report bytes: got=%q want=%q", afterBody, prior)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterInfo.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("special-mode race stripped setuid bit: mode=%s", afterInfo.Mode())
	}
}

func TestProofCheckWriteReportFilePreservesExistingOnPublicationFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	previous := []byte("previous complete proof report\n")
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	err := proofCheckWriteReportFile(path, proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.UTC().Format(time.RFC3339),
		Repo:          "/repo",
		Steps: []proofCheckStep{
			{Name: "go-test", Command: []string{"go", "test", "./..."}, ProofRef: "go-test-count-1-all", Status: "ok"},
		},
	})
	if err == nil {
		t.Fatal("report publication unexpectedly succeeded in a non-writable parent")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !reflect.DeepEqual(got, previous) {
		t.Fatalf("failed report publication changed prior bytes: got=%q want=%q", got, previous)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, ".report.json.*.tmp"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("failed publication leaked sibling temp files: %v", matches)
	}
}

// F4: an unwritable --out path (parent directory does not exist) is refused fail-closed
// with exit 2 BEFORE any gate runs. Observed via the real re-exec child-arm pattern (see
// director_cli_boundary_test.go / product_review_provenance_test.go): the parent spawns
// the test binary itself with an env toggle that runs cmdProofCheck directly, so the
// parent can assert on the REAL observed os.Exit code without killing the test runner.
// This re-exec target is the light form (no compound binary build needed) because the
// refusal must happen before runProofCheck is ever called — no gate subprocess, hence no
// self-exec-recursion risk (see TestProofCheckOutTruthfulExitOneOnRedBattery for why the
// heavy (built-binary) form is required once real gates execute).
func TestProofCheckOutUnwritablePathFailsClosedBeforeGates(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckOutCLISubEnv) == "unwritable-path" {
		cmdProofCheck([]string{"--dry-run", "--out", os.Getenv("COMPOUND_PROOFCHECK_OUT_PATH")})
		return // if cmdProofCheck returns, exit 0 fails the parent's exit-2 assertion below
	}
	badPath := filepath.Join(t.TempDir(), "nonexistent-parent-dir", "out.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckOutUnwritablePathFailsClosedBeforeGates$")
	cmd.Env = append(os.Environ(),
		proofCheckOutCLISubEnv+"=unwritable-path",
		"COMPOUND_PROOFCHECK_OUT_PATH="+badPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("--out under a nonexistent dir should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("--out under a nonexistent dir should exit 2 (fail-closed setup error), got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "--out path not writable") {
		t.Fatalf("stderr should name the --out failure, got %s", output)
	}
	if _, statErr := os.Stat(badPath); !os.IsNotExist(statErr) {
		t.Fatalf("refused --out path should not exist afterward, stat err=%v", statErr)
	}
}

// F6: --dry-run --out exits 0, writes the (all-"planned") report to FILE, and prints an
// "ok" verdict line. Light re-exec (dry-run never executes a gate subprocess, so no
// self-exec-recursion risk).
func TestProofCheckOutDryRunExitsZeroWithOkVerdict(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckOutCLISubEnv) == "dry-run" {
		proofCheckTimingHistoryRecorder = func(string, proofCheckReport, time.Duration, time.Time) (string, error) {
			err := os.WriteFile(
				os.Getenv("COMPOUND_PROOFCHECK_TIMING_MARKER"),
				[]byte("timing-recorded\n"),
				0o600,
			)
			return "", err
		}
		cmdProofCheck([]string{"--dry-run", "--repo", os.Getenv("COMPOUND_PROOFCHECK_OUT_REPO"), "--out", os.Getenv("COMPOUND_PROOFCHECK_OUT_PATH")})
		// cmdProofCheck does not call os.Exit on the ok path (a real top-level main()
		// would simply return, exiting 0) — exit explicitly here so the parent observes
		// ONLY proof-check's own stdout, not the go-test child binary's own PASS trailer,
		// which would otherwise become the (misleading) "last line".
		os.Exit(0)
	}
	repo := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "dryrun-report.json")
	timingMarker := filepath.Join(t.TempDir(), "timing-recorded")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckOutDryRunExitsZeroWithOkVerdict$")
	cmd.Env = append(os.Environ(),
		proofCheckOutCLISubEnv+"=dry-run",
		"COMPOUND_PROOFCHECK_OUT_REPO="+repo,
		"COMPOUND_PROOFCHECK_OUT_PATH="+outPath,
		"COMPOUND_PROOFCHECK_TIMING_MARKER="+timingMarker,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run --out should exit 0, got err=%v output=%s", err, output)
	}
	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("--out file should exist on dry-run: %v", readErr)
	}
	var report proofCheckReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("parse --out file: %v\n%s", err, raw)
	}
	if report.Status != "ok" || !report.DryRun {
		t.Fatalf("dry-run --out report = %+v, want status ok dry_run true", report)
	}
	if _, err := os.Stat(timingMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run recorded timing history: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(output), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "battery: ok passed=") || !strings.HasSuffix(last, "out="+outPath) {
		t.Fatalf("last stdout line should be the ok verdict, got %q\nfull output:\n%s", last, output)
	}
}

// F5: on a genuinely red battery, the observed process exit is 1 AND FILE is written AND
// the verdict line is present (and correct) as the last stdout line.
//
// Mechanism note (deviation from the packet's first-choice suggestion): this builds the
// REAL compound binary (same pattern as TestProofCheckNativeGateDeniesUnownedWrite) and
// runs it as a subprocess, rather than re-executing the go-test binary in-process the way
// F4/F6 do above. That is load-bearing, not a style choice: two of --ci's seven portable
// gates (architecture-doctor, memory-doctor) invoke `exe <verb>` where exe is
// os.Executable() resolved INSIDE the running proof-check process. If that process is the
// compiled `go test` binary (the light re-exec form), those gates would re-exec the TEST
// BINARY itself with args like ["architecture","doctor","--strict"] — the Go testing flag
// parser treats "architecture"/"doctor" as trailing positional args and "--strict" as an
// unparsed flag, so the child re-enters as a bare `go test` run with no -test.run filter,
// i.e. it fans out into the ENTIRE cmd/compound test suite recursively. That is exactly
// the unbounded-cost trap the packet's escape hatch anticipates. The real compound binary
// does not have this problem: inside it, os.Executable() resolves to itself, so
// `architecture doctor --strict` and `memory doctor` run as the real, fast, bounded CLI
// subcommands they are.
//
// The battery's own go-test gate is pointed at a tiny, throwaway, deliberately-red
// fixture module (not the compound module itself) so the falsifier proves the --out/exit
// wiring in seconds, not the ~10-minute real compound test suite.
func TestProofCheckOutTruthfulExitOneOnRedBattery(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	if testing.Short() {
		t.Skip("builds the compound binary and runs a real go-test gate subprocess")
	}
	exe := compoundTestBinary(t)

	fixture := t.TempDir()
	// Copy the production driver, not a surrogate command. Its inventory needs
	// the compound module and both isolated packages; these are inventory-only
	// stubs, not proof of home/release behavior. Missing runner/setup failures
	// must never stand in for the deliberate Go assertion below.
	driver, err := os.ReadFile(filepath.Join(inventoryGuardRepoRoot(t), "scripts", "testbattery", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	inventoryDriver, err := os.ReadFile(filepath.Join(inventoryGuardRepoRoot(t), "scripts", "testbattery", "source_inventory.go"))
	if err != nil {
		t.Fatal(err)
	}
	alignmentDriver, err := os.ReadFile(filepath.Join(inventoryGuardRepoRoot(t), "scripts", "testbattery", "credential_alignment.go"))
	if err != nil {
		t.Fatal(err)
	}
	phasePolicy, err := os.ReadFile(filepath.Join(inventoryGuardRepoRoot(t), "internal", "proofscope", "test_phases.go"))
	if err != nil {
		t.Fatal(err)
	}
	redTest := "package fixture\n\nimport \"testing\"\n\n" +
		"func TestAlwaysFails(t *testing.T) {\n\tt.Fatal(\"deliberate red for proof-check --out falsifier\")\n}\n"
	for name, body := range map[string][]byte{
		"go.mod":                                      []byte("module compound\n\ngo 1.23\n"),
		"red_test.go":                                 []byte(redTest),
		"scripts/testbattery/main.go":                 driver,
		"internal/proofscope/test_phases.go":          phasePolicy,
		"scripts/testbattery/source_inventory.go":     inventoryDriver,
		"scripts/testbattery/credential_alignment.go": alignmentDriver,
		"internal/home/home.go":                       []byte("package home\n"),
		"internal/release/release.go":                 []byte("package release\n"),
	} {
		path := filepath.Join(fixture, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCommandGit(t, fixture, "init")
	runCommandGit(t, fixture, "add", "-A")

	outPath := filepath.Join(t.TempDir(), "report.json")
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, home.HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatalf("init compatible proof-check home: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "proof-check", "--ci", "--out", outPath, "--repo", fixture)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("red battery with --out should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("red battery with --out should exit 1 (gate FAILED), got %d output=%s", exitErr.ExitCode(), output)
	}

	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("--out file should exist on a red battery: %v", readErr)
	}
	var report proofCheckReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("parse --out file: %v\n%s", err, raw)
	}
	if report.Status != "failed" {
		t.Fatalf("written report status = %q, want failed: %+v", report.Status, report)
	}
	firstFailed := ""
	for _, step := range report.Steps {
		if step.Status == "failed" {
			firstFailed = step.Name
			break
		}
	}
	if firstFailed != "go-test" {
		t.Fatalf("expected go-test to be the first failed gate, got %q; steps=%+v", firstFailed, report.Steps)
	}
	for _, step := range report.Steps {
		if step.Name == "go-test" && (!strings.Contains(step.OutputTail, "--- FAIL: TestAlwaysFails") ||
			!strings.Contains(step.OutputTail, "deliberate red for proof-check --out falsifier")) {
			t.Fatalf("go-test did not execute the deliberate failing assertion: %s", step.OutputTail)
		}
	}

	lines := strings.Split(strings.TrimRight(string(output), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "battery: FAILED passed=") || !strings.Contains(last, "first_failed=go-test") || !strings.HasSuffix(last, "out="+outPath) {
		t.Fatalf("last stdout line should be the FAILED verdict naming go-test, got %q\nfull output:\n%s", last, output)
	}
}
