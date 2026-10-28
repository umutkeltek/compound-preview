package work

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"compound/internal/authority/gitrepository"
	"compound/internal/finding"
)

func TestPrivateSourceExecutionRunsPublicScannerRetainsBlockingFindingsAndCompleteInventory(t *testing.T) {
	for _, name := range []string{"exact", "later-file", "high", "profile-drift"} {
		t.Run(name, func(t *testing.T) {
			f := newPrivateSourceFixture(t, "public", true)
			stubPrivateDestination(t, f.review.Destination)
			plan := privateSourceBoundPlan(t, f)
			body, err := canonicalProofGitBlob(f.repo, f.source.HeadSHA, "catalog.txt")
			if err != nil {
				t.Fatal(err)
			}
			inputs := []PublicRedactionInput{{Path: "catalog.txt", Body: body}}
			if name == "later-file" {
				inputs = append(inputs, PublicRedactionInput{Path: "later.txt", Body: []byte("unreviewed later file\n")})
			}
			if name == "profile-drift" {
				plan.Steps = append(plan.Steps, ProofCheckStep{Name: "extra", Command: []string{"true"}, ProofRef: "extra"})
			}
			report := f.review.Files[0].Report
			if name == "high" {
				var parsed map[string]any
				if err := json.Unmarshal(report, &parsed); err != nil {
					t.Fatal(err)
				}
				flagged := parsed["findings"].([]any)[0].(map[string]any)
				flagged["severity"], flagged["tier"], flagged["id"] = "HIGH", "HIGH", "credential.token"
				parsed["counts"] = map[string]int{"HIGH": 1, "MEDIUM": 0, "LOW": 0, "WARN": 0}
				report = publicReviewJSON(t, parsed)
			}
			file := filepath.Join(t.TempDir(), "scan.json")
			if err := os.WriteFile(file, report, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				if name != "redact" || !reflect.DeepEqual(args[:6], []string{"--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", inputs[calls-1].Path}) || args[6] != "--json" {
					t.Fatalf("scanner posture changed: %q %q", name, args)
				}
				return exec.CommandContext(ctx, "sh", "-c", `cat "$1"; exit "$2"`, "scanner", file, strconv.Itoa(f.review.Files[0].ExitCode))
			}
			ctx := ContextWithPublicRedactionReview(ContextWithCanonicalProofPlan(context.Background(), plan), f.store.home, f.workID, proofActorAlpha)
			output, public, private, err := RunSourceRedaction(ctx, f.repo, plan.Steps[0], f.source.BaseSHA, []string{f.source.HeadSHA}, func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error) {
				return inputs, nil
			}, command, func(context.Context, string, string, ...string) ([]byte, error) {
				t.Fatal("private scan dispatched to unreviewed callback")
				return nil, nil
			})
			if name == "profile-drift" {
				if err == nil || calls != 0 {
					t.Fatal("profile drift scanned or passed")
				}
				return
			}
			if private == nil || public != nil || len(private.Scans) != len(inputs) || calls != len(inputs) {
				t.Fatalf("blocking exit discarded remaining evidence: %+v calls=%d %v", private, calls, err)
			}
			if private.Scans[0].ExitCode != 2 || !bytes.Equal(private.Scans[0].Report, report) {
				t.Fatal("raw blocking scanner evidence changed")
			}
			if name != "exact" {
				if err == nil {
					t.Fatal("unreviewed inventory or HIGH accepted")
				}
				step := plan.Steps[0]
				step.PrivateSourceEvidence = private
				step.Status = "failed"
				if err := ValidatePrivateSourceEvidence(step, &f.source, f.workID); err != nil {
					t.Fatal(err)
				}
				step.Status = "ok"
				if ValidatePrivateSourceEvidence(step, &f.source, f.workID) == nil {
					t.Fatal("red evidence used as green certificate")
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "not a public-clean certificate") {
				t.Fatalf("exact private scan failed: %s %v", output, err)
			}
		})
	}
}

func TestPrivateSourceScannerOverflowRetainsBoundedPartialEvidence(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", true)
	stubPrivateDestination(t, f.review.Destination)
	plan := privateSourceBoundPlan(t, f)
	body, err := canonicalProofGitBlob(f.repo, f.source.HeadSHA, "catalog.txt")
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithPublicRedactionReview(ContextWithCanonicalProofPlan(context.Background(), plan), f.store.home, f.workID, proofActorAlpha)
	_, evidence, err := runReviewedPrivateSourceRedaction(ctx, f.repo, plan.Steps[0], f.source.BaseSHA, []string{f.source.HeadSHA}, func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error) {
		return []PublicRedactionInput{{Path: "catalog.txt", Body: body}}, nil
	}, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "dd", "if=/dev/zero", "bs=1024", "count=1025")
	})
	if err == nil || evidence == nil || len(evidence.Scans) != 1 || len(evidence.Scans[0].Report) == 0 || len(evidence.Scans[0].Report) > finding.MaxBodyBytes {
		t.Fatalf("private overflow lost bounded current scan: %v %v", evidence != nil, err)
	}
	step := plan.Steps[0]
	step.Status = "failed"
	step.PrivateSourceEvidence = evidence
	if err := ValidatePrivateSourceEvidence(step, &f.source, f.workID); err != nil {
		t.Fatal(err)
	}
	step.Status = "ok"
	if ValidatePrivateSourceEvidence(step, &f.source, f.workID) == nil {
		t.Fatal("partial scan evidence used as successful private proof")
	}
}

func TestPrivateSourceUnreviewedDispatchPreservesDefaultPublicCallback(t *testing.T) {
	called := false
	output, public, private, err := RunSourceRedaction(context.Background(), "/synthetic/source", ProofCheckStep{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}, "base", []string{"head"}, nil, nil, func(_ context.Context, repo, base string, heads ...string) ([]byte, error) {
		called = repo == "/synthetic/source" && base == "base" && reflect.DeepEqual(heads, []string{"head"})
		return []byte("default public scan"), nil
	})
	if err != nil || !called || public != nil || private != nil || string(output) != "default public scan" {
		t.Fatal("default/public scan behavior changed")
	}
}
