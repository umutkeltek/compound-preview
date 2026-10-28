package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/work"
)

type proofRedactScannerCapture struct {
	Argv []string
	Body []byte
}

func TestProofCheckRedactSnapshotTransportPreservesSourceIdentityAndBody(t *testing.T) {
	// PATH is scoped to this non-parallel test; production scanner resolution stays unchanged.
	repo := initCompoundCommandGitRepo(t)
	baseSHA := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	logicalPath := "site/public/contact notes.html"
	workingPath := filepath.Join(repo, filepath.FromSlash(logicalPath))
	body := append([]byte("sealed\x00\xff\r\n"), bytes.Repeat([]byte("complete committed body\n"), 128*1024)...)
	if err := os.MkdirAll(filepath.Dir(workingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workingPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", logicalPath)
	runCommandGit(t, repo, "commit", "-m", "sealed scanner source")
	source, err := gitrepository.CaptureCleanSnapshot(repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	scanner := filepath.Join(binDir, "redact")
	shim := "#!/bin/sh\nexport COMPOUND_TEST_REDACT_ARGV0=\"$0\"\nexec \"$COMPOUND_TEST_REDACT_EXEC\" -test.run=^TestProofCheckRedactScannerFixture$ -- \"$@\"\n"
	if err := os.WriteFile(scanner, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOUND_TEST_REDACT_EXEC", os.Args[0])
	for _, tc := range []struct {
		name    string
		deleted bool
		exit    int
	}{
		{name: "working_file_changed"},
		{name: "working_file_deleted", deleted: true},
		{name: "scanner_failure", exit: 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.deleted {
				if err := os.Remove(workingPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(workingPath, []byte("mutable working body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			capturePath := filepath.Join(t.TempDir(), "scanner.json")
			t.Setenv("COMPOUND_TEST_REDACT_CAPTURE", capturePath)
			t.Setenv("COMPOUND_TEST_REDACT_EXIT", strconv.Itoa(tc.exit))
			_, err := runProofCheckRedactContext(t.Context(), repo, source.BaseSHA, source.HeadSHA)
			if tc.exit == 0 {
				if err != nil {
					t.Fatalf("scanner failed: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exit {
					t.Fatalf("scanner error = %v, want exit %d", err, tc.exit)
				}
			}
			encoded, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatal(err)
			}
			var capture proofRedactScannerCapture
			if err := json.Unmarshal(encoded, &capture); err != nil {
				t.Fatal(err)
			}
			wantArgv := []string{scanner, "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", logicalPath}
			if !reflect.DeepEqual(capture.Argv, wantArgv) {
				t.Fatalf("scanner argv = %q, want %q", capture.Argv, wantArgv)
			}
			if !bytes.Equal(capture.Body, body) {
				t.Fatalf("scanner stdin changed: got %d bytes, want %d committed bytes", len(capture.Body), len(body))
			}
		})
	}
}

func TestProofCheckRedactScannerFixture(t *testing.T) {
	capturePath := os.Getenv("COMPOUND_TEST_REDACT_CAPTURE")
	if capturePath == "" {
		return
	}
	delimiter := slices.Index(os.Args, "--")
	if delimiter < 0 {
		t.Fatal("scanner fixture has no argv delimiter")
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	capture := proofRedactScannerCapture{
		Argv: append([]string{os.Getenv("COMPOUND_TEST_REDACT_ARGV0")}, os.Args[delimiter+1:]...),
		Body: body,
	}
	encoded, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capturePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	exitCode, err := strconv.Atoi(os.Getenv("COMPOUND_TEST_REDACT_EXIT"))
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(exitCode)
}

func TestProofCheckAdmissionProfileUsesOneImmutableSourceRange(t *testing.T) {
	t.Parallel()
	repo := initCompoundCommandGitRepo(t)
	baseSHA := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(repo+"/candidate.go", []byte("package candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "candidate.go")
	runCommandGit(t, repo, "commit", "-m", "candidate")
	source, err := gitrepository.CaptureCleanSnapshot(repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := work.ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if profile.BaseRef != source.BaseSHA || profile.SourceSnapshot == nil ||
		!reflect.DeepEqual(*profile.SourceSnapshot, source) {
		t.Fatalf("admission profile did not seal source snapshot: %+v", profile)
	}

	steps, err := proofCheckSteps(source.BaseSHA, source.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	var diffCommand []string
	for _, step := range steps {
		if step.Name == "diff-check" {
			diffCommand = step.Command
			break
		}
	}
	wantRange := source.BaseSHA + "..." + source.HeadSHA
	if !slices.Contains(diffCommand, wantRange) {
		t.Fatalf("diff-check command = %v, want immutable range %s", diffCommand, wantRange)
	}
	files, err := proofCheckChangedFilesAtSnapshot(repo, source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{"candidate.go"}) {
		t.Fatalf("redact changed files = %v, want exact committed source range", files)
	}
	if err := os.WriteFile(repo+"/candidate.go", []byte("package mutated_after_snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs, err := proofCheckRedactInputsAtSnapshot(repo, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].Path != "candidate.go" ||
		string(inputs[0].Body) != "package candidate\n" {
		t.Fatalf("redact inputs = %+v, want exact committed candidate blob", inputs)
	}
	if err := os.Remove(repo + "/candidate.go"); err != nil {
		t.Fatal(err)
	}
	inputs, err = proofCheckRedactInputsAtSnapshot(repo, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || string(inputs[0].Body) != "package candidate\n" {
		t.Fatalf("deleted working copy hid committed redact input: %+v", inputs)
	}
}

func proofSourceGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestProofCheckSnapshotReportCarriesSealedIdentity(t *testing.T) {
	t.Parallel()
	repo := initCompoundCommandGitRepo(t)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	report, err := runProofCheckSnapshotContext(
		t.Context(),
		repo,
		true,
		source,
		false,
		false,
		time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceSnapshot == nil || !reflect.DeepEqual(*report.SourceSnapshot, source) {
		t.Fatalf("proof report source snapshot = %+v, want %+v", report.SourceSnapshot, source)
	}
	if report.BaseRef != source.BaseSHA {
		t.Fatalf("proof report base_ref = %q, want immutable %q", report.BaseRef, source.BaseSHA)
	}
}

func TestProofCheckAdmissionRejectsSourceMutationAfterGates(t *testing.T) {
	t.Parallel()
	repo := initCompoundCommandGitRepo(t)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	homePath := t.TempDir()
	now := time.Date(2026, 7, 28, 5, 15, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-source-revalidate-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "source unchanged through completion", "codex:source-revalidate", now)
	gates := []work.ProofResourceGateProfile{{
		Name:     "synthetic-gate",
		Command:  []string{"go", "test", "./internal/synthetic"},
		ProofRef: "synthetic-proof",
	}}
	profile, err := work.NewProofResourceProfileWithSource(
		work.ProofResourceProfileCanonicalFull,
		repo,
		source,
		gates,
	)
	if err != nil {
		t.Fatal(err)
	}
	report := proofCheckReport{
		SchemaVersion:  proofCheckSchemaVersion,
		ProfileKind:    work.ProofResourceProfileCanonicalFull,
		Status:         "ok",
		GeneratedAt:    now.Add(2 * time.Second).Format(time.RFC3339),
		Repo:           repo,
		BaseRef:        source.BaseSHA,
		SourceSnapshot: &source,
		ProofRefs:      []string{"synthetic-proof"},
		Steps: []proofCheckStep{{
			Name:     "synthetic-gate",
			Command:  []string{"go", "test", "./internal/synthetic"},
			ProofRef: "synthetic-proof",
			Status:   "ok",
		}},
	}
	_, completed, err := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    item.ID,
		Actor:     "codex:source-revalidate",
		Workspace: repo,
		Command:   "compound proof-check --base HEAD --out /synthetic/proof.json",
		Profile:   profile,
		TTL:       time.Minute,
		Now:       func() time.Time { return now.Add(time.Second) },
	}, func(context.Context) (proofCheckReport, error) {
		if err := os.WriteFile(repo+"/late-uncommitted.txt", []byte("late\n"), 0o644); err != nil {
			return proofCheckReport{}, err
		}
		return report, nil
	})
	if err == nil || !strings.Contains(err.Error(), "source snapshot changed") {
		t.Fatalf("source mutation completion error = %v", err)
	}
	if completed.VerificationRef != "" {
		t.Fatalf("source mutation emitted reusable verification: %+v", completed)
	}
}
