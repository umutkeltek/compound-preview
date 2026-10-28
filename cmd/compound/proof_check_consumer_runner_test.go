package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const consumerGoReportFixture = `{"Action":"start","Package":"example.invalid/product/integration"}
{"Action":"run","Package":"example.invalid/product/integration","Test":"TestBuyerPath"}
{"Action":"pass","Package":"example.invalid/product/integration","Test":"TestBuyerPath"}
{"Action":"pass","Package":"example.invalid/product/integration"}
`

const consumerBunReportFixture = `<testsuites tests="1" failures="0" skipped="0"><testsuite name="example.test.ts" tests="1" failures="0" skipped="0"><testsuite name="feature" tests="1" failures="0" skipped="0"><testcase name="does the thing" classname="" file="tests/example.test.ts"/></testsuite></testsuite></testsuites>`

func consumerRunnerFixture(t *testing.T, script string, change func(map[string]any)) (string, proofCheckStep) {
	t.Helper()
	contract := consumerContractFixture("go_consumers_v1")
	c := contract["consumers"].([]any)[0].(map[string]any)
	c["command"] = []string{"sh", "consumer.sh"}
	c["required_tools"] = []string{"sh"}
	if change != nil {
		change(c)
	}
	body := consumerContractJSON(t, contract)
	repo := initCanonicalProofRepo(t, map[string]string{"go.mod": "module example.invalid/product\n", "compound-proof.json": body, "consumer.sh": script})
	sum := sha256.Sum256([]byte(body))
	return repo, proofCheckStep{Name: "consumer:buyer-path", Command: []string{"sh", "consumer.sh"}, Dir: ".", ProofRef: "consumer:buyer-path:sha256:" + hex.EncodeToString(sum[:])}
}

func consumerBunRunnerFixture(t *testing.T, script string, cache string) (string, proofCheckStep) {
	t.Helper()
	contract := consumerContractFixture("bun_consumers_v1")
	c := contract["consumers"].([]any)[0].(map[string]any)
	c["command"] = []string{"sh", "consumer.sh"}
	c["required_tools"] = []string{"sh"}
	c["required_environment"] = []string{"BUN_INSTALL_CACHE_DIR"}
	body := consumerContractJSON(t, contract)
	repo := initCanonicalProofRepo(t, map[string]string{
		"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n",
		"compound-proof.json": body, "consumer.sh": script,
	})
	sum := sha256.Sum256([]byte(body))
	t.Setenv("BUN_INSTALL_CACHE_DIR", cache)
	return repo, proofCheckStep{Name: "consumer:buyer-path", Command: []string{"sh", "consumer.sh"}, Dir: ".", ProofRef: "consumer:buyer-path:sha256:" + hex.EncodeToString(sum[:])}
}

func TestCanonicalBunConsumerRunnerRetainsNativeJUnitAndScrubsCredentials(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("NPM_TOKEN", "synthetic-secret")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/tmp/ambient-npmrc")
	t.Setenv("HOME", t.TempDir())
	script := "test -z \"${NPM_TOKEN+x}\" && test -z \"${NPM_CONFIG_USERCONFIG+x}\" && test \"$HOME\" != \"" + os.Getenv("HOME") + "\" || exit 41\n" + nativeReportScript(consumerBunReportFixture)
	repo, step := consumerBunRunnerFixture(t, script, cache)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "ok" || result.ConsumerEvidence == nil || string(result.ConsumerEvidence.Report) != consumerBunReportFixture {
		t.Fatalf("native Bun JUnit was not retained: %+v", result)
	}
}

func TestCanonicalBunConsumerRunnerRequiresCacheBeforeLaunch(t *testing.T) {
	repo, step := consumerBunRunnerFixture(t, "touch launched\n", "")
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("missing Bun cache admitted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("consumer launched before prerequisite validation: %v", err)
	}
}

func TestCanonicalBunConsumerRunnerRequiresBunBeforeLaunch(t *testing.T) {
	contract := consumerContractFixture("bun_consumers_v1")
	c := contract["consumers"].([]any)[0].(map[string]any)
	c["command"] = []string{"sh", "consumer.sh"}
	c["required_tools"] = []string{"sh", "bun"}
	body := consumerContractJSON(t, contract)
	repo := initCanonicalProofRepo(t, map[string]string{"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n", "compound-proof.json": body, "consumer.sh": "touch launched\n"})
	sum := sha256.Sum256([]byte(body))
	step := proofCheckStep{Name: "consumer:buyer-path", Command: []string{"sh", "consumer.sh"}, Dir: ".", ProofRef: "consumer:buyer-path:sha256:" + hex.EncodeToString(sum[:])}
	original := proofCheckRuntimeExecutable
	t.Cleanup(func() { proofCheckRuntimeExecutable = original })
	proofCheckRuntimeExecutable = func(logical string) (string, error) {
		if logical == "bun" {
			return "", errors.New("synthetic Bun unavailable")
		}
		return original(logical)
	}
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("missing Bun admitted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("consumer launched before Bun resolution: %v", err)
	}
}

func nativeReportScript(report string) string {
	return "set -eu\nif [ -n \"${COMPOUND_PROOF_REPORT:-}\" ]; then\nprintf '%s' '" + strings.ReplaceAll(report, "'", "'\\''") + "' > \"$COMPOUND_PROOF_REPORT\"\nfi\n"
}

func TestCanonicalConsumerRunnerRetainsNativeEvidence(t *testing.T) {
	repo, step := consumerRunnerFixture(t, nativeReportScript(consumerGoReportFixture), nil)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "ok" {
		t.Fatalf("native consumer failed: %+v", result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded["consumer_evidence"]) == 0 {
		t.Fatal("successful native evidence was discarded before canonical report custody")
	}
}

func TestCanonicalConsumerRunnerRejectsVacuousReports(t *testing.T) {
	cases := map[string]string{
		"missing":          "exit 0\n",
		"empty":            nativeReportScript(""),
		"malformed":        nativeReportScript("not JSON"),
		"missing required": nativeReportScript(strings.ReplaceAll(consumerGoReportFixture, "TestBuyerPath", "TestSomethingElse")),
		"required skipped": nativeReportScript(strings.Replace(consumerGoReportFixture, `"Action":"pass","Package":"example.invalid/product/integration","Test"`, `"Action":"skip","Package":"example.invalid/product/integration","Test"`, 1)),
		"duplicate":        nativeReportScript(consumerGoReportFixture + consumerGoReportFixture),
		"incomplete":       nativeReportScript(strings.Join(strings.Split(consumerGoReportFixture, "\n")[:3], "\n") + "\n"),
		"reordered":        nativeReportScript(strings.Join([]string{strings.Split(consumerGoReportFixture, "\n")[0], strings.Split(consumerGoReportFixture, "\n")[2], strings.Split(consumerGoReportFixture, "\n")[1], strings.Split(consumerGoReportFixture, "\n")[3]}, "\n") + "\n"),
		"duplicate key":    nativeReportScript(strings.Replace(consumerGoReportFixture, `"Action":"run"`, `"Action":"skip","Action":"run"`, 1)),
		"symlink":          "if [ -n \"${COMPOUND_PROOF_REPORT:-}\" ]; then ln -s consumer.sh \"$COMPOUND_PROOF_REPORT\"; fi\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			repo, step := consumerRunnerFixture(t, script, nil)
			result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
			if result.Status != "failed" {
				t.Fatalf("%s native report accepted: %+v", name, result)
			}
		})
	}
}

func TestCanonicalConsumerRunnerScrubsAmbientEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "synthetic-secret")
	t.Setenv("GOFLAGS", "-skip=all")
	t.Setenv("COMPOUND_PROOF_REPORT", filepath.Join(t.TempDir(), "caller-report"))
	script := "test -z \"${DATABASE_URL+x}\" && test -z \"${GOFLAGS+x}\" && test \"$GOPROXY\" = off && test \"$GOENV\" = off || exit 41\n" + nativeReportScript(consumerGoReportFixture)
	repo, step := consumerRunnerFixture(t, script, nil)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "ok" {
		t.Fatalf("ambient environment reached required consumer: %+v", result)
	}
	if _, err := os.Stat(os.Getenv("COMPOUND_PROOF_REPORT")); !os.IsNotExist(err) {
		t.Fatalf("caller report path was used: %v", err)
	}
}

func TestCanonicalConsumerRunnerRejectsMissingPrerequisiteBeforeLaunch(t *testing.T) {
	t.Setenv("PG_BIN_DIR", "")
	repo, step := consumerRunnerFixture(t, "touch launched\n", func(c map[string]any) { c["required_environment"] = []string{"PG_BIN_DIR"} })
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("missing prerequisite admitted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("consumer launched before prerequisite validation: %v", err)
	}
}

func TestCanonicalConsumerRunnerRejectsContractDriftBeforeLaunch(t *testing.T) {
	repo, step := consumerRunnerFixture(t, "touch launched\n", nil)
	step.ProofRef = "consumer:buyer-path:sha256:" + strings.Repeat("0", 64)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("unbound contract admitted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("consumer launched before contract validation: %v", err)
	}
}

func TestCanonicalConsumerCachedGoReportCannotCertify(t *testing.T) {
	cached := `{"Action":"output","Package":"example.invalid/product/integration","Output":"ok \texample.invalid/product/integration\t(cached)\n"}`
	report := strings.Replace(consumerGoReportFixture, "\n", "\n"+cached+"\n", 1)
	repo, step := consumerRunnerFixture(t, nativeReportScript(report), nil)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatalf("cached Go replay certified as native execution: %+v", result)
	}
}

func TestCanonicalConsumerCaseVariantKeysCannotOverrideNativeFailure(t *testing.T) {
	report := strings.Replace(consumerGoReportFixture, `"Action":"run"`, `"Action":"skip","action":"run"`, 1)
	repo, step := consumerRunnerFixture(t, nativeReportScript(report), nil)
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" {
		t.Fatal("case-variant JSON key overrode native failure")
	}
}
