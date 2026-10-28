package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/finding"
	"compound/internal/policy"
)

func TestPrivateSourceExecutionPreservesCleanFilesWithBlockingFindings(t *testing.T) {
	for _, realScanner := range []bool{false, true} {
		name := "recorded-json"
		if realScanner {
			name = "actual-public-scanner"
		}
		t.Run(name, func(t *testing.T) {
			if realScanner {
				if _, err := exec.LookPath("redact"); err != nil {
					t.Skip("actual PUBLIC scanner is not installed")
				}
			}
			for _, omitClean := range []bool{false, true} {
				name := "reviewed-clean-inventory"
				if omitClean {
					name = "unreviewed-clean-file"
				}
				t.Run(name, func(t *testing.T) {
					f, inputs, reports := privateSourceCleanInventoryFixture(t, realScanner, omitClean)
					stubPrivateDestination(t, f.review.Destination)
					plan := privateSourceBoundPlan(t, f)
					ctx := ContextWithPublicRedactionReview(ContextWithCanonicalProofPlan(context.Background(), plan), f.store.home, f.workID, proofActorAlpha)
					calls := []string{}
					command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
						if name != "redact" || len(args) != 7 || !reflect.DeepEqual(args[:5], []string{"--repo-visibility", "public", "--max-bytes", "8388608", "--source-path"}) || args[6] != "--json" {
							t.Fatalf("native PUBLIC scanner posture changed: %q %q", name, args)
						}
						path := args[5]
						calls = append(calls, path)
						if realScanner {
							return exec.CommandContext(ctx, name, args...)
						}
						report := reports[path]
						file := filepath.Join(t.TempDir(), "scanner.json")
						if err := os.WriteFile(file, report.Report, 0600); err != nil {
							t.Fatal(err)
						}
						return exec.CommandContext(ctx, "sh", "-c", `cat "$1"; exit "$2"`, "scanner", file, strconv.Itoa(report.ExitCode))
					}
					_, public, private, err := RunSourceRedaction(ctx, f.repo, plan.Steps[0], f.source.BaseSHA, []string{f.source.HeadSHA}, func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error) {
						// Deliberately reversed: the native private runner owns ordering.
						return []PublicRedactionInput{inputs[2], inputs[1], inputs[0]}, nil
					}, command, func(context.Context, string, string, ...string) ([]byte, error) {
						t.Fatal("reviewed private source fell back to unreviewed dispatch")
						return nil, nil
					})
					expected := []string{".env.example", "catalog.txt", "zz-clean.txt"}
					if public != nil || private == nil || len(private.Scans) != 3 || !reflect.DeepEqual(calls, expected) {
						t.Fatalf("complete clean/blocking/clean inventory lost: calls=%q evidence=%+v err=%v", calls, private, err)
					}
					for i, path := range expected {
						got, want := private.Scans[i], reports[path]
						if got.Path != path || got.SHA256 != want.SHA256 || got.ExitCode != want.ExitCode || !bytes.Equal(got.Report, want.Report) {
							t.Fatalf("actual scan identity/exit/raw bytes changed for %s: got exit=%d want=%d", path, got.ExitCode, want.ExitCode)
						}
					}
					if private.Scans[0].ExitCode != 0 || private.Scans[1].ExitCode != 2 || private.Scans[2].ExitCode != 0 {
						t.Fatal("clean success or blocking exit was normalized incorrectly")
					}
					if omitClean {
						if err == nil {
							t.Fatal("unreviewed clean file accepted")
						}
						step := plan.Steps[0]
						step.PrivateSourceEvidence, step.Status = private, "ok"
						if ValidatePrivateSourceEvidence(step, &f.source, f.workID) == nil {
							t.Fatal("incomplete review became certifying evidence")
						}
						return
					}
					if err != nil {
						t.Fatalf("reviewed complete clean/blocking/clean inventory failed: %v", err)
					}
					step := plan.Steps[0]
					step.PrivateSourceEvidence, step.Status = private, "ok"
					if err := ValidatePrivateSourceEvidence(step, &f.source, f.workID); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func privateSourceCleanInventoryFixture(t *testing.T, realScanner, omitClean bool) (privateSourceFixture, []PublicRedactionInput, map[string]PublicRedactionScan) {
	t.Helper()
	f := newPrivateSourceFixture(t, "public", false)
	for _, path := range []string{".env.example", "zz-clean.txt"} {
		if err := os.WriteFile(filepath.Join(f.repo, path), []byte("# clean source example\n"), 0600); err != nil {
			t.Fatal(err)
		}
		runProofSourceGit(t, f.repo, "add", path)
	}
	runProofSourceGit(t, f.repo, "commit", "-m", "synthetic complete source inventory")
	source, err := gitrepository.CaptureCleanSnapshot(f.repo, f.source.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	f.source, f.review.Source = source, source
	unbound := CanonicalProofPlan{ID: "synthetic", Repo: f.repo, Source: source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	f.review.SourceProfileDigest, err = privateSourceProfileDigest(unbound)
	if err != nil {
		t.Fatal(err)
	}
	blocking := f.review.Files[0].PublicRedactionScan
	f.review.Files = nil
	inputs := []PublicRedactionInput{}
	reports := map[string]PublicRedactionScan{}
	for _, path := range []string{".env.example", "catalog.txt", "zz-clean.txt"} {
		body, err := canonicalProofGitBlob(f.repo, source.HeadSHA, path)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, PublicRedactionInput{Path: path, Body: body})
		raw := publicReviewJSON(t, map[string]any{"findings": []PublicRedactionFinding{}, "counts": map[string]int{"HIGH": 0, "MEDIUM": 0, "LOW": 0, "WARN": 0}, "repoVisibility": "public", "oversize": false})
		exit := 0
		if path == "catalog.txt" {
			raw, exit = blocking.Report, blocking.ExitCode
		}
		if realScanner {
			cmd := exec.Command("redact", "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", path, "--json")
			cmd.Dir, cmd.Stdin = f.repo, bytes.NewReader(body)
			var runErr error
			raw, runErr = cmd.CombinedOutput()
			exit = 0
			if runErr != nil {
				exit = ExitCodeFromRunErr(runErr)
			}
		}
		findings, err := ParsePublicRedactionReport(raw, exit)
		if err != nil {
			t.Fatal(err)
		}
		if (path == "catalog.txt" && (exit != 2 || len(findings) != 1)) || (path != "catalog.txt" && (exit != 0 || len(findings) != 0)) {
			t.Fatal("fixture must contain two actual clean reports and one synthetic MEDIUM")
		}
		scan := PublicRedactionScan{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Report: raw, ExitCode: exit}
		reports[path] = scan
		file := PublicRedactionFile{PublicRedactionScan: scan, Approvals: []PublicRedactionApproval{}}
		for _, flagged := range findings {
			if flagged.ID != "pii.phone.e164" || flagged.Tier != "MEDIUM" {
				t.Fatal("unexpected synthetic fixture finding")
			}
			file.Approvals = append(file.Approvals, PublicRedactionApproval{Finding: flagged, Classification: "private_synthetic_fixture", EvidenceRefs: []string{"fixture://reserved-synthetic-provider"}})
		}
		if !omitClean || path != "zz-clean.txt" {
			f.review.Files = append(f.review.Files, file)
		}
	}
	at := time.Now()
	recorded, err := finding.Record(f.store.home, finding.Finding{Topic: "synthetic-private-complete-inventory", Body: string(publicReviewJSON(t, f.review)), ProducedBy: proofActorAlpha, WorkID: f.workID}, at)
	if err != nil {
		t.Fatal(err)
	}
	f.finding = recorded
	f.action.ID = "act_private_source_complete_clean_inventory"
	f.action.CreatedAt = at.UTC().Format(time.RFC3339Nano)
	f.action.ArgumentsHash, f.action.EvidenceRefs = recorded.CID, []string{"finding:" + recorded.ID}
	result, err := f.store.EvaluateAndRecordAction(f.workID, f.action, at)
	if err != nil || result.Recorded || result.Decision.Outcome != "hold" {
		t.Fatalf("exact complete review must begin held: %+v %v", result, err)
	}
	cfg, _, err := policy.LoadConfig(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(f.store.home)
	approval, _, err := approvals.Plan(f.action.ID, cfg, 5*time.Minute, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(approval.ID, "operator:synthetic-reviewer", cfg, at); err != nil {
		t.Fatal(err)
	}
	result, err = f.store.EvaluateAndRecordAction(f.workID, f.action, at)
	if err != nil || !result.Recorded {
		t.Fatalf("exact complete review admission failed: %+v %v", result, err)
	}
	return f, inputs, reports
}
