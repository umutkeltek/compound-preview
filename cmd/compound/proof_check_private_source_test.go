package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

func TestPrivateSourceDispatchPreservesTrackedGofmt(t *testing.T) {
	for _, tc := range []struct{ name, body, status string }{
		{"clean", "package fixture\n\nfunc Ready() bool { return true }\n", "ok"},
		{"unformatted", "package fixture\nfunc Ready( )bool{return true}\n", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initCanonicalProofRepo(t, map[string]string{"fixture.go": tc.body})
			result := runProofCheckStepContext(context.Background(), repo, proofCheckStep{Name: "gofmt", Command: []string{"gofmt", "-l", "<tracked *.go>"}, ProofRef: "gofmt-check"}, "HEAD")
			if result.Status != tc.status {
				t.Fatalf("tracked gofmt dispatch: got %s want %s; %s", result.Status, tc.status, result.OutputTail)
			}
		})
	}
}

func TestPrivateSourceGenuineScannerAndNativeCLIFinalizerLifecycle(t *testing.T) {
	scanner, err := exec.LookPath("redact")
	if err != nil {
		t.Skip("genuine private-source scanner coverage UNEXECUTED: scanner unavailable")
	}
	cache := exec.Command("go", "env", "GOCACHE")
	cache.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	cacheBody, err := cache.Output()
	if err != nil {
		t.Fatal(err)
	}
	contract := consumerContractFixture("go_consumers_v1")
	consumer := contract["consumers"].([]any)[0].(map[string]any)
	consumer["command"] = []string{"sh", "consumer.sh"}
	consumer["required_tools"] = []string{"sh", "go"}
	consumer["required_environment"] = []string{"GOCACHE"}
	consumer["required_tests"] = []string{"example.invalid/product::TestBuyerPath"}
	repo := initCanonicalProofRepo(t, map[string]string{
		"go.mod":              "module example.invalid/product\n\ngo 1.26.4\n",
		"activation_test.go":  "package product\nimport \"testing\"\nfunc TestBuyerPath(t *testing.T) { t.Log(\"genuine private-source consumer\") }\n",
		"consumer.sh":         "set -eu\ngo test -json -count=1 -timeout=30s > \"$COMPOUND_PROOF_REPORT\"\n",
		"compound-proof.json": consumerContractJSON(t, contract),
	})
	base := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	body := strings.Join([]string{"provider: +1", "202", "555", "0197\n"}, "")
	writeCanonicalProofFile(t, repo, "catalog.txt", body)
	runCommandGit(t, repo, "add", "catalog.txt")
	runCommandGit(t, repo, "commit", "-m", "synthetic private source candidate")
	runCommandGit(t, repo, "remote", "add", "origin", "https://github.com/example-owner/proof-fixture.git")
	runCommandGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	source, err := gitrepository.CaptureCleanSnapshot(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	// Only destination API observations are fake. The scanner, consumer,
	// native approval, proof subprocess, report and finalizer are genuine.
	bin := t.TempDir()
	gh := "#!/bin/sh\nset -eu\ncase \"$*\" in\n'api --hostname github.com user') printf '%s' '{\"id\":7,\"login\":\"synthetic-reviewer\"}' ;;\n'api --hostname github.com repos/example-owner/proof-fixture') printf '%s' '{\"id\":11,\"full_name\":\"example-owner/proof-fixture\",\"private\":true,\"visibility\":\"private\",\"archived\":false,\"permissions\":{\"push\":true}}' ;;\n*) exit 97 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	actor := "codex:private-source-fixture"
	w, err := store.Create(work.CreateInput{Owner: actor, Actor: actor, State: "ready", SuccessPredicate: "genuine private source certificate", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "required", PublicationRef: "refs/remotes/origin/main"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan, err := work.ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := work.ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scanner, "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", "catalog.txt", "--json")
	cmd.Stdin = strings.NewReader(body)
	raw, scanErr := cmd.CombinedOutput()
	exit := work.ExitCodeFromRunErr(scanErr)
	findings, err := work.ParsePublicRedactionReport(raw, exit)
	if err != nil {
		t.Fatalf("genuine scanner: %v %s", err, raw)
	}
	if exit != 2 || len(findings) != 1 || findings[0].ID != "pii.phone.e164" {
		t.Fatalf("private positive control did not block: %s", raw)
	}
	hash := sha256.Sum256([]byte(body))
	destination := work.PrivateSourceDestination{Host: "github.com", RepositoryID: 11, FullName: "example-owner/proof-fixture", CanonicalURL: "https://github.com/example-owner/proof-fixture.git", PushURL: "https://github.com/example-owner/proof-fixture.git", Remote: "origin", PublicationRef: "refs/remotes/origin/main", TargetRef: "refs/heads/main", ViewerID: 7, ViewerLogin: "synthetic-reviewer", Visibility: "private", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	review := work.PrivateSourceReview{SchemaVersion: work.PrivateSourceReviewSchema, WorkID: w.ID, Source: source, SourceProfileDigest: profile.Digest, Destination: destination, Files: []work.PublicRedactionFile{{PublicRedactionScan: work.PublicRedactionScan{Path: "catalog.txt", SHA256: hex.EncodeToString(hash[:]), Report: raw, ExitCode: exit}, Approvals: []work.PublicRedactionApproval{{Finding: findings[0], Classification: "private_provider_identifier", EvidenceRefs: []string{"provider://synthetic/nonsecret-identifier"}}}}}}
	reviewBody, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	f, err := finding.Record(homePath, finding.Finding{Topic: "synthetic-private-source-review", Body: string(reviewBody), ProducedBy: actor, WorkID: w.ID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_synthetic_private_source_review", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "test", Name: "synthetic-fixture"}, IDs: envelope.IDs{WorkID: w.ID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: []string{"finding:" + f.ID}}, Actor: actor, Capability: work.PrivateSourceReviewCapability, Action: work.PrivateSourceReviewAction, Target: envelope.ActionTarget{Kind: "repository", ID: source.RepositoryFingerprint, Path: repo}, MutationClass: "public", Reversibility: "manual", BlastRadius: "repo", ArgumentsHash: f.CID, RollbackPlan: "retain exact source and review"}
	if _, err := store.EvaluateAndRecordAction(w.ID, ai, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(homePath)
	approval, _, err := approvals.Plan(ai.ID, cfg, 10*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(approval.ID, "operator:synthetic-reviewer", cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.EvaluateAndRecordAction(w.ID, ai, time.Now()); err != nil || !allowed.Recorded {
		t.Fatalf("private review approval: %+v %v", allowed, err)
	}
	out := filepath.Join(t.TempDir(), "proof.json")
	child := exec.Command(compoundTestBinary(t), "proof-check", "--repo", repo, "--base", base, "--work", w.ID, "--actor", actor, "--redact-review", ai.ID, "--out", out)
	child.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "GOCACHE="+strings.TrimSpace(string(cacheBody)))
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "battery: ok") {
		t.Fatalf("genuine private CLI lifecycle: %v\n%s", err, output)
	}
	resultBody, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Steps []work.ProofCheckStep `json:"steps"`
	}
	if err := json.Unmarshal(resultBody, &report); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, step := range report.Steps {
		if step.Name == "redact-public" {
			seen = true
			if step.Status != "ok" || step.ProofRef != "redact-private-source-reviewed" || step.PrivateSourceEvidence == nil || step.RedactionEvidence != nil || len(step.PrivateSourceEvidence.Scans) != 1 || step.PrivateSourceEvidence.Scans[0].ExitCode != 2 {
				t.Fatalf("private source evidence lost: %+v", step)
			}
		}
	}
	if !seen {
		t.Fatal("private scanner gate absent")
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) == 0 || !strings.HasPrefix(current.VerificationIDs[len(current.VerificationIDs)-1], "proof-check:pfrs_") {
		t.Fatal("genuine private certificate not attached to work")
	}
}
