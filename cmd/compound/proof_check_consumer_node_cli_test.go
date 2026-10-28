package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// This launches the real public binary. Only the unrelated redact executable is
// stubbed, as in the existing native-consumer lifecycle fixtures. Native reports
// come directly from the installed Node test runner, never from fixture XML.
func TestCanonicalNodePublicCLILifecycleOwnsGenuineNativeEvidence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node native CLI coverage UNEXECUTED: installed Node unavailable")
	}
	versionCommand := exec.Command(node, "--version")
	versionCommand.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + isolatedTestUserHome(t)}
	version, err := versionCommand.Output()
	if err != nil {
		t.Fatalf("read installed Node version: %v", err)
	}
	if !strings.HasPrefix(string(version), "v26.") {
		t.Skipf("Node native CLI coverage UNEXECUTED: tested report shape is Node26; installed %s", strings.TrimSpace(string(version)))
	}
	t.Logf("genuine public CLI native coverage using %s", strings.TrimSpace(string(version)))
	binary := compoundTestBinary(t)
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "native-pass-certifies", false: "native-assertion-failure-noncertifying"}[valid], func(t *testing.T) {
			const declarationPath = "artifacts/app/compound-proof.json"
			contract := consumerContractFixture("node_npm_consumers_v1")
			consumer := contract["consumers"].([]any)[0].(map[string]any)
			consumer["command"] = []string{"node", "proof.mjs"}
			consumer["dir"] = "artifacts/app"
			consumer["required_tools"] = []string{"node"}
			consumer["report_kind"] = "node_test_junit"
			consumer["required_tests"] = []string{`["artifacts/app/checks.test.mjs","product checks","keeps the contract",["product checks"]]`}
			consumer["timeout_seconds"] = 10
			declaration := consumerContractJSON(t, contract)
			want := "4"
			if !valid {
				want = "5"
			}
			repo := initCanonicalProofRepo(t, map[string]string{
				declarationPath:                   declaration,
				"artifacts/app/package.json":      `{"name":"synthetic-native-consumer","private":true}`,
				"artifacts/app/package-lock.json": `{"name":"synthetic-native-consumer","lockfileVersion":3,"packages":{"":{"name":"synthetic-native-consumer"}}}`,
				"artifacts/app/checks.test.mjs":   `import {describe,test} from 'node:test'; import assert from 'node:assert/strict'; describe('product checks',()=>{test('keeps the contract',()=>assert.equal(2+2,` + want + `));});`,
				"artifacts/app/proof.mjs":         `import {spawnSync} from 'node:child_process'; const r=spawnSync(process.execPath,['--test','--test-reporter=junit','--test-reporter-destination='+process.env.COMPOUND_PROOF_REPORT,'checks.test.mjs'],{env:process.env,stdio:'inherit'}); if(r.error)throw r.error;process.exit(r.status??1);`,
			})
			base := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
			writeCanonicalProofFile(t, repo, "README.md", "synthetic candidate\n")
			runCommandGit(t, repo, "add", "README.md")
			runCommandGit(t, repo, "commit", "-m", "candidate")
			source, err := gitrepository.CaptureCleanSnapshot(repo, base)
			if err != nil {
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
			homePath := t.TempDir()
			now := time.Now().UTC()
			if err := home.Init(homePath, "native-node-cli-fixture", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			const actor = "codex:consumer-fixture"
			w, err := store.Create(work.CreateInput{
				Owner: actor, Actor: actor, State: "ready", SuccessPredicate: "genuine nested Node consumer has an owned canonical certificate",
				GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"},
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, actor, now); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "proof.json")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "redact"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(binary, "proof-check", "--repo", repo, "--base", base, "--work", w.ID, "--actor", actor, "--out", out)
			child.Env = []string{
				"COMPOUND_HOME=" + homePath, "HOME=" + isolatedTestUserHome(t), "LANG=C", "LC_ALL=C",
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			}
			output, runErr := child.CombinedOutput()
			if (runErr == nil) != valid {
				t.Fatalf("public Node proof exit=%v want success=%v\n%s", runErr, valid, output)
			}
			body, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("public report missing: %v\n%s", err, output)
			}
			var report proofCheckReport
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.SourceSnapshot, &source) || len(report.Steps) != 3 || report.Steps[1].Name != "diff-check" || report.Steps[2].Name != "redact-public" {
				t.Fatalf("public report lost source or complete inventory: %+v", report)
			}
			evidence := report.Steps[0].ConsumerEvidence
			if evidence == nil || evidence.ContractPath != declarationPath || !bytes.Equal(evidence.Contract, []byte(declaration)) || evidence.SourceRoot == "" || evidence.SourceRoot == repo || !bytes.Contains(evidence.Report, []byte(filepath.Join(evidence.SourceRoot, "artifacts/app/checks.test.mjs"))) {
				t.Fatalf("native source/declaration/custody binding absent: %+v", evidence)
			}
			if !bytes.Contains(evidence.Report, []byte("<testsuites>")) || !bytes.Contains(evidence.Report, []byte(`name="keeps the contract"`)) || (!valid && !bytes.Contains(evidence.Report, []byte("<failure"))) {
				t.Fatalf("genuine native outcome missing: %s", evidence.Report)
			}
			if valid {
				if err := work.ValidateCanonicalConsumerEvidence(work.ProofCheckStep(report.Steps[0])); err != nil {
					t.Fatal(err)
				}
			}
			current, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := landlease.Status(homePath, time.Now().UTC())
			if err != nil || lease.Held {
				t.Fatalf("public proof retained resource: %+v %v", lease, err)
			}
			events, err := eventstore.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			results, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"pfrs_"}, RegularOnly: true})
			if err != nil || len(results) != 1 {
				t.Fatalf("expected one proof result: %d %v", len(results), err)
			}
			resultRecord, err := results[0].Read()
			if err != nil {
				t.Fatal(err)
			}
			var result work.ProofResourceResult
			if err := json.Unmarshal(resultRecord.Bytes, &result); err != nil {
				t.Fatal(err)
			}
			var reservation work.ProofResourceReservation
			var completion work.ProofResourceCompletion
			for id, target := range map[string]any{result.ReservationID: &reservation, result.CompletionID: &completion} {
				record, err := events.Read(id)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(record.Bytes, target); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := runattempt.Open(homePath).Get(result.RunAttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if result.WorkID != w.ID || !result.OwnedReport || !reflect.DeepEqual(result.SourceSnapshot, &source) || !reflect.DeepEqual(reservation.Profile, profile) || reservation.RunAttemptID != attempt.ID || completion.RunAttemptID != attempt.ID || completion.ResultID != result.ID || completion.ReservationID != reservation.ID || attempt.WorkID != w.ID || attempt.Harness != "proof-check" || !containsString(current.RunAttemptIDs, attempt.ID) {
				t.Fatalf("source/reservation/attempt/completion chain diverged: result=%+v reservation=%+v completion=%+v attempt=%+v", result, reservation, completion, attempt)
			}
			artifacts, err := home.NewArtifactStore(homePath)
			if err != nil {
				t.Fatal(err)
			}
			owned, err := artifacts.Get(result.ReportArtifactRef)
			if err != nil {
				t.Fatal(err)
			}
			var ownedReport proofCheckReport
			if err := json.Unmarshal(owned, &ownedReport); err != nil {
				t.Fatal(err)
			}
			if len(ownedReport.Steps) != 3 || !reflect.DeepEqual(ownedReport.Steps[0].ConsumerEvidence, evidence) || !reflect.DeepEqual(ownedReport.SourceSnapshot, &source) {
				t.Fatal("content-addressed report changed exact native evidence")
			}
			if valid {
				if report.Status != "ok" || result.Status != work.ProofResourceVerified || attempt.Status != "succeeded" || len(current.VerificationIDs) != 1 || current.VerificationIDs[0] != result.VerificationRef || result.VerificationRef != "proof-check:"+result.ID || completion.VerificationRef != result.VerificationRef || !bytes.Contains(output, []byte("battery: ok")) {
					t.Fatalf("native success did not certify exact attempt: %+v %+v\n%s", result, attempt, output)
				}
			} else if report.Status != "failed" || result.Status != work.ProofResourceFailed || attempt.Status != "failed" || len(current.VerificationIDs) != 0 || result.VerificationRef != "" || completion.VerificationRef != "" || !bytes.Contains(output, []byte("battery: FAILED")) {
				t.Fatalf("real native assertion failure certified or lost truth: %+v %+v\n%s", result, attempt, output)
			}
			if _, err := store.Advance(w.ID, work.AdvanceInput{To: current.State, Actor: actor, ArtifactID: "commit:" + source.HeadSHA}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			landed, err := store.LandCheck(w.ID, work.LandCheckOptions{Actor: actor, CandidateTipSHA: source.HeadSHA, Record: valid}, time.Now().UTC())
			if valid {
				if err != nil || landed.Status != "landed" || landed.ProofVerificationRef != result.VerificationRef {
					t.Fatalf("delivery did not consume public native certificate: %+v %v", landed, err)
				}
			} else if err == nil && (landed.Status == "landed" || landed.ProofVerificationRef != "") {
				t.Fatalf("delivery accepted failed native proof: %+v", landed)
			}
		})
	}
}
