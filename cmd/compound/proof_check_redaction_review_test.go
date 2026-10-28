package main

import (
	"bytes"
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
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/work"
)

// Public CLI + genuine installed scanner + native Go report + native approval.
// No scanner rule, email allowlist, mocked PolicyDecision or translated test
// success is used. Missing external scanner is explicit unexecuted coverage.
func TestReviewedPublicRedactionRealCLILifecycle(t *testing.T) {
	scanner, err := exec.LookPath("redact")
	if err != nil {
		t.Skip("genuine redaction CLI coverage UNEXECUTED: scanner unavailable")
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
		"activation_test.go":  "package product\nimport \"testing\"\nfunc TestBuyerPath(t *testing.T) { t.Log(\"genuine reviewed public consumer\") }\n",
		"consumer.sh":         "set -eu\ngo test -json -count=1 -timeout=30s . > \"$COMPOUND_PROOF_REPORT\"\n",
		"compound-proof.json": consumerContractJSON(t, contract),
	})
	base := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	// Generate a reserved synthetic number; no real provider or person is used.
	providerBody := strings.Join([]string{"public provider: +1", "202", "555", "0197\n"}, "")
	// Generate a reserved synthetic contact for the scanner positive control.
	supportBody := strings.Join([]string{"helpdesk", "catalog.invalid\n"}, "@")
	writeCanonicalProofFile(t, repo, "catalog.txt", providerBody)
	writeCanonicalProofFile(t, repo, "support.txt", supportBody)
	writeCanonicalProofFile(t, repo, "z-clean.txt", "one-time unlock price verified\n")
	runCommandGit(t, repo, "add", "catalog.txt", "support.txt", "z-clean.txt")
	runCommandGit(t, repo, "commit", "-m", "synthetic public candidate")
	source, err := gitrepository.CaptureCleanSnapshot(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "reviewed-public-cli-fixture", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:public-review-fixture"
	w, err := store.Create(work.CreateInput{Owner: actor, Actor: actor, State: "ready", SuccessPredicate: "genuine public scanner findings require native exact approval", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, time.Now()); err != nil {
		t.Fatal(err)
	}
	review := work.PublicRedactionReview{SchemaVersion: work.PublicRedactionReviewSchema, WorkID: w.ID, Source: source}
	for _, input := range []struct{ path, body string }{{"catalog.txt", providerBody}, {"support.txt", supportBody}, {"z-clean.txt", "one-time unlock price verified\n"}} {
		cmd := exec.Command(scanner, "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", input.path, "--json")
		cmd.Stdin = strings.NewReader(input.body)
		raw, scanErr := cmd.CombinedOutput()
		exit := work.ExitCodeFromRunErr(scanErr)
		if scanErr == nil {
			exit = 0
		}
		fs, err := work.ParsePublicRedactionReport(raw, exit)
		if err != nil {
			t.Fatalf("genuine scanner report: %v\n%s", err, raw)
		}
		hash := sha256.Sum256([]byte(input.body))
		file := work.PublicRedactionFile{PublicRedactionScan: work.PublicRedactionScan{Path: input.path, SHA256: hex.EncodeToString(hash[:]), Report: raw, ExitCode: exit}, Approvals: []work.PublicRedactionApproval{}}
		for _, f := range fs {
			if f.Severity == "MEDIUM" {
				classification := "public_provider_identifier"
				if f.ID == "pii.email" {
					classification = "public_support_contact"
				}
				file.Approvals = append(file.Approvals, work.PublicRedactionApproval{Finding: f, Classification: classification, EvidenceRefs: []string{"provider://synthetic/public-catalog"}})
			}
		}
		review.Files = append(review.Files, file)
	}
	if review.Files[0].ExitCode != 2 || len(review.Files[0].Approvals) != 1 {
		t.Fatal("real scanner positive control did not block the synthetic provider number")
	}
	if review.Files[1].ExitCode != 2 || len(review.Files[1].Approvals) != 1 || review.Files[1].Approvals[0].Finding.ID != "pii.email" {
		t.Fatal("real scanner positive control did not block the synthetic support address")
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	f, err := finding.Record(homePath, finding.Finding{Topic: "synthetic-public-catalog-review", Body: string(body), ProducedBy: actor, WorkID: w.ID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_native_public_review", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "manual", Name: "synthetic-fixture"}, IDs: envelope.IDs{WorkID: w.ID}, Authority: envelope.Authority{SourceClass: "runtime"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{"finding:" + f.ID}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: work.PublicRedactionReviewCapability, Action: work.PublicRedactionReviewAction, Target: envelope.ActionTarget{Kind: "repository", ID: source.RepositoryFingerprint, Path: repo}, MutationClass: "public", Reversibility: "manual", BlastRadius: "repo", ArgumentsHash: f.CID, Preview: "review exact synthetic provider finding", RollbackPlan: "retain source and require a new review on drift"}
	held, err := store.EvaluateAndRecordAction(w.ID, ai, time.Now())
	if err != nil || held.Recorded || held.Decision.Outcome != "hold" {
		t.Fatalf("native public review must hold first: %+v %v", held, err)
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(homePath)
	p, _, err := approvals.Plan(ai.ID, cfg, 10*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(p.ID, "operator:synthetic-reviewer", cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	allowed, err := store.EvaluateAndRecordAction(w.ID, ai, time.Now())
	if err != nil || !allowed.Recorded {
		t.Fatalf("native reviewed authority failed: %+v %v", allowed, err)
	}
	out := filepath.Join(t.TempDir(), "proof.json")
	child := exec.Command(compoundTestBinary(t), "proof-check", "--repo", repo, "--base", base, "--work", w.ID, "--actor", actor, "--redact-review", ai.ID, "--out", out)
	child.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "GOCACHE="+strings.TrimSpace(string(cacheBody)))
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("native reviewed public CLI did not certify: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report proofCheckReport
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.Steps) != 3 || report.Steps[0].ConsumerEvidence == nil || !bytes.Contains(report.Steps[0].ConsumerEvidence.Report, []byte("genuine reviewed public consumer")) {
		t.Fatalf("real native consumer missing: %+v", report)
	}
	redaction := report.Steps[2]
	if redaction.ProofRef != "redact-public-reviewed" || redaction.RedactionEvidence == nil || len(redaction.RedactionEvidence.Scans) != 3 || redaction.RedactionEvidence.Binding.CID != f.CID || !bytes.Equal(redaction.RedactionEvidence.Scans[0].Report, review.Files[0].Report) || !bytes.Equal(redaction.RedactionEvidence.Scans[1].Report, review.Files[1].Report) {
		t.Fatalf("exact real scanner bytes and native authority were not retained: %+v", redaction)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 1 {
		t.Fatal("native review did not produce an owned canonical verification")
	}
	lease, err := landlease.Status(homePath, time.Now())
	if err != nil || lease.Held {
		t.Fatalf("reviewed proof failed to release resource: %+v %v", lease, err)
	}
}

func TestReviewedPublicRedactionCLIRefusesMissingAndNoncanonicalModes(t *testing.T) {
	for _, args := range [][]string{
		{"--redact-review"},
		{"--redact-review", "--ci"},
		{"--redact-review", "act_unapproved", "--dev"},
		{"--redact-review", "act_unapproved", "--ci"},
		{"--redact-review", "act_unapproved", "--ci-console"},
		{"--redact-review", "act_unapproved", "--dry-run"},
		{"--redact-review", "act_unapproved", "--timings"},
		{"--redact-review", "act_unapproved", "--recover-legacy-retained"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			child := exec.Command(compoundTestBinary(t), append([]string{"proof-check"}, args...)...)
			output, err := child.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "proof-check:") || strings.Contains(string(output), "battery:") {
				t.Fatalf("invalid review invocation reached execution: %v\n%s", err, output)
			}
		})
	}
}
