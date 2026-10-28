package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
)

const consumerCanonicalSubEnv = "COMPOUND_CONSUMER_CANONICAL_TEST_CHILD"

func TestCanonicalConsumerFullLifecycleOwnsNativeGoEvidence(t *testing.T) {
	if os.Getenv(consumerCanonicalSubEnv) == "1" {
		proofCheckRuntimeArtifactRecorder = func(string, proofCheckReport) (proofCheckReport, error) {
			panic("external consumer must not publish Compound runtime artifact")
		}
		cmdProofCheck([]string{"--repo", os.Getenv("COMPOUND_CONSUMER_TEST_REPO"), "--base", os.Getenv("COMPOUND_CONSUMER_TEST_BASE"), "--work", os.Getenv("COMPOUND_CONSUMER_TEST_WORK"), "--actor", "codex:consumer-fixture", "--out", os.Getenv("COMPOUND_CONSUMER_TEST_OUT")})
		os.Exit(0)
	}
	cache := exec.Command("go", "env", "GOCACHE")
	cache.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	cacheBody, err := cache.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, testName := range []string{"TestBuyerPath", "TestWrongConsumer"} {
		t.Run(testName, func(t *testing.T) {
			contract := consumerContractFixture("go_consumers_v1")
			consumer := contract["consumers"].([]any)[0].(map[string]any)
			consumer["command"] = []string{"sh", "consumer.sh"}
			consumer["required_tools"] = []string{"sh", "go"}
			consumer["required_environment"] = []string{"GOCACHE"}
			consumer["required_tests"] = []string{"example.invalid/product::TestBuyerPath"}
			repo := initCanonicalProofRepo(t, map[string]string{
				"go.mod":              "module example.invalid/product\n\ngo 1.26.4\n",
				"activation_test.go":  "package product\nimport \"testing\"\nfunc " + testName + "(t *testing.T) { t.Log(\"native consumer executed\") }\n",
				"consumer.sh":         "set -eu\ngo test -json -count=1 -timeout=30s . > \"$COMPOUND_PROOF_REPORT\"\n",
				"compound-proof.json": consumerContractJSON(t, contract),
			})
			base := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
			writeCanonicalProofFile(t, repo, "README.md", "candidate\n")
			runCommandGit(t, repo, "add", "README.md")
			runCommandGit(t, repo, "commit", "-m", "candidate")
			source, err := gitrepository.CaptureCleanSnapshot(repo, base)
			if err != nil {
				t.Fatal(err)
			}
			homePath := t.TempDir()
			now := time.Now().UTC()
			if err := home.Init(homePath, "native-consumer-fixture", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(work.CreateInput{
				Owner: "codex:consumer-fixture", Actor: "codex:consumer-fixture", State: "ready",
				SuccessPredicate: "required native consumer has an owned canonical certificate",
				GitDelivery:      &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"},
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, "codex:consumer-fixture", now); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "proof.json")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "redact"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestCanonicalConsumerFullLifecycleOwnsNativeGoEvidence$")
			child.Env = append(os.Environ(),
				consumerCanonicalSubEnv+"=1", "COMPOUND_HOME="+homePath, "GOCACHE="+strings.TrimSpace(string(cacheBody)),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"COMPOUND_CONSUMER_TEST_REPO="+repo, "COMPOUND_CONSUMER_TEST_BASE="+base,
				"COMPOUND_CONSUMER_TEST_WORK="+w.ID, "COMPOUND_CONSUMER_TEST_OUT="+out,
			)
			output, runErr := child.CombinedOutput()
			wantGreen := testName == "TestBuyerPath"
			if (runErr == nil) != wantGreen {
				t.Fatalf("canonical consumer exit=%v wantGreen=%v\n%s", runErr, wantGreen, output)
			}
			body, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("owned report missing: %v\n%s", err, output)
			}
			var report proofCheckReport
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Steps) != 3 || report.Steps[0].ConsumerEvidence == nil || !bytes.Contains(report.Steps[0].ConsumerEvidence.Report, []byte("native consumer executed")) {
				t.Fatalf("real Go native evidence not retained: %+v", report)
			}
			current, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := landlease.Status(homePath, time.Now().UTC())
			if err != nil || lease.Held {
				t.Fatalf("canonical consumer did not release resource: %+v %v", lease, err)
			}
			if !wantGreen {
				if len(current.VerificationIDs) != 0 || report.Status != "failed" || !bytes.Contains(output, []byte("battery: FAILED")) {
					t.Fatalf("wrong native test certified: %+v\n%s", current, output)
				}
				return
			}
			if len(current.VerificationIDs) != 1 || !strings.HasPrefix(current.VerificationIDs[0], "proof-check:pfrs_") || report.Status != "ok" {
				t.Fatalf("canonical native certificate absent: %+v\n%s", current, output)
			}
			if _, err := store.Advance(w.ID, work.AdvanceInput{To: current.State, Actor: "codex:consumer-fixture", ArtifactID: "commit:" + source.HeadSHA}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			landed, err := store.LandCheck(w.ID, work.LandCheckOptions{Actor: "codex:consumer-fixture", CandidateTipSHA: source.HeadSHA, Record: true}, time.Now().UTC())
			if err != nil || landed.Status != "landed" || landed.ProofVerificationRef != current.VerificationIDs[0] {
				t.Fatalf("delivery did not consume native certificate: %+v %v", landed, err)
			}
		})
	}
}

func TestCanonicalConsumerTimeoutRetainsPartialEvidence(t *testing.T) {
	partial := strings.Join(strings.Split(consumerGoReportFixture, "\n")[:2], "\n") + "\n"
	repo, step := consumerRunnerFixture(t, nativeReportScript(partial)+"exec sleep 30\n", func(c map[string]any) { c["timeout_seconds"] = 1 })
	before := time.Now()
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" || time.Since(before) > 10*time.Second || !strings.Contains(result.OutputTail, "deadline exceeded") {
		t.Fatalf("consumer deadline not enforced: %+v", result)
	}
	if result.ConsumerEvidence == nil || string(result.ConsumerEvidence.Report) != partial {
		t.Fatalf("timeout discarded partial native progress: %+v", result)
	}
}

func TestCanonicalBunConsumerLifecycleOwnsNativeEvidenceAndLandCheck(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "wrong-native-identity"}[valid], func(t *testing.T) {
			contract := consumerContractFixture("bun_consumers_v1")
			consumer := contract["consumers"].([]any)[0].(map[string]any)
			consumer["command"] = []string{"sh", "consumer.sh"}
			consumer["required_tools"] = []string{"sh"}
			report := consumerBunReportFixture
			if !valid {
				report = strings.Replace(report, `name="does the thing"`, `name="wrong native test"`, 1)
			}
			repo := initCanonicalProofRepo(t, map[string]string{
				"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n",
				"consumer.sh":         nativeReportScript(report),
				"compound-proof.json": consumerContractJSON(t, contract),
			})
			baseBody, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(string(baseBody))
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("candidate\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runCommandGit(t, repo, "add", "README.md")
			runCommandGit(t, repo, "commit", "-m", "candidate")
			source, err := gitrepository.CaptureCleanSnapshot(repo, base)
			if err != nil {
				t.Fatal(err)
			}
			homePath := t.TempDir()
			now := time.Now().UTC()
			if err := home.Init(homePath, "native-bun-fixture", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(work.CreateInput{Owner: "codex:consumer-fixture", Actor: "codex:consumer-fixture", State: "ready", SuccessPredicate: "required native Bun consumer has an owned canonical certificate", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, "codex:consumer-fixture", now); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "proof.json")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "redact"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestCanonicalConsumerFullLifecycleOwnsNativeGoEvidence$")
			child.Env = append(os.Environ(), consumerCanonicalSubEnv+"=1", "COMPOUND_HOME="+homePath, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "COMPOUND_CONSUMER_TEST_REPO="+repo, "COMPOUND_CONSUMER_TEST_BASE="+base, "COMPOUND_CONSUMER_TEST_WORK="+w.ID, "COMPOUND_CONSUMER_TEST_OUT="+out)
			output, runErr := child.CombinedOutput()
			if (runErr == nil) != valid {
				t.Fatalf("Bun canonical exit=%v valid=%v\n%s", runErr, valid, output)
			}
			body, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("owned report missing: %v\n%s", err, output)
			}
			var proof proofCheckReport
			if err := json.Unmarshal(body, &proof); err != nil {
				t.Fatal(err)
			}
			if len(proof.Steps) != 3 || proof.Steps[0].ConsumerEvidence == nil || !bytes.Equal(proof.Steps[0].ConsumerEvidence.Report, []byte(report)) {
				t.Fatalf("native Bun bytes not owned: %+v", proof)
			}
			current, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if valid {
				if len(current.VerificationIDs) != 1 {
					t.Fatalf("Bun certificate absent: %+v", current)
				}
				if _, err := store.Advance(w.ID, work.AdvanceInput{To: current.State, Actor: "codex:consumer-fixture", ArtifactID: "commit:" + source.HeadSHA}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				landed, err := store.LandCheck(w.ID, work.LandCheckOptions{Actor: "codex:consumer-fixture", CandidateTipSHA: source.HeadSHA}, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if landed.ProofVerificationRef != current.VerificationIDs[0] {
					t.Fatal("land-check lost Bun proof certificate")
				}
			} else if len(current.VerificationIDs) != 0 {
				t.Fatalf("wrong native identity certified: %+v", current)
			}
		})
	}
}

func TestCanonicalConsumerBackgroundWriterCannotReviseCertifiedReport(t *testing.T) {
	script := nativeReportScript(consumerGoReportFixture) + "(sleep 1; printf 'invalid late report\\n' > \"$COMPOUND_PROOF_REPORT\") >/dev/null 2>&1 &\n"
	repo, step := consumerRunnerFixture(t, script, func(c map[string]any) { c["timeout_seconds"] = 4 })
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("passing bytes certified while a descendant could still revise the native report: %+v", result)
	}
}

func TestCanonicalConsumerXcodeAdapterConsumesBothNativeViews(t *testing.T) {
	contract := consumerContractFixture("xcode_consumers_v1")
	c := contract["consumers"].([]any)[0].(map[string]any)
	c["command"] = []string{"sh", "consumer.sh"}
	c["required_tools"] = []string{"sh", "xcrun"}
	repo := initCanonicalProofRepo(t, map[string]string{
		"compound-proof.json": consumerContractJSON(t, contract),
		"consumer.sh":         "set -eu\nmkdir \"$COMPOUND_PROOF_REPORT\"\n",
	})
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := work.ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
set -eu
if [ "$1" = --find ]; then
  case "$2" in
    xcodebuild|xcresulttool) printf '%s/%s\n' "${0%/*}" "$2"; exit 0 ;;
    *) exit 42 ;;
  esac
fi
test "$1 $2 $3" = 'xcresulttool get test-results'
if [ "$5" = --help ]; then
  printf '%s help\n' "$4"
  exit 0
fi
test -d "$6"
case "$4" in
  summary) printf '%s\n' '{"result":"Passed","totalTestCount":1,"passedTests":1,"failedTests":0,"skippedTests":0,"expectedFailures":0,"testFailures":[]}' ;;
  tests) printf '%s\n' '{"testNodes":[{"nodeType":"Test Case","nodeIdentifier":"ActivationTests/testFirstAppearance()","result":"Passed"}]}' ;;
  *) exit 42 ;;
esac
`
	for name, body := range map[string]string{
		"xcrun":        script,
		"xcodebuild":   "#!/bin/sh\nset -eu\ntest \"$1\" = -version\nprintf 'Xcode 99.0\\nBuild version Synthetic\\n'\n",
		"xcresulttool": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	original := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(logical string) (string, error) {
		if logical == "xcrun" || logical == "xcodebuild" {
			return filepath.Join(bin, logical), nil
		}
		return original(logical)
	}
	t.Cleanup(func() { proofCheckRuntimeExecutable = original })
	result := runProofCheckStepContext(t.Context(), repo, plan.Steps[0], "HEAD", "HEAD")
	if result.Status != "ok" || result.ConsumerEvidence == nil || len(result.ConsumerEvidence.Tests) == 0 {
		t.Fatalf("native Xcode adapter: %+v", result)
	}
}
