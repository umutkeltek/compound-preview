package work

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/authority/gitrepository"
)

const pytestRequired = `["tests/test_flow.py","tests.test_flow","test_flow"]`
const pytestSkipped = `["tests/test_flow.py","tests.test_flow","test_optional"]`
const pytestExample = `<testsuites><testsuite name="pytest" errors="0" failures="0" skipped="1" tests="2" time="0.01" timestamp="2026-01-01T00:00:00" hostname="fixture.invalid"><testcase classname="tests.test_flow" name="test_flow" file="tests/test_flow.py" line="1" time="0.001"/><testcase classname="tests.test_flow" name="test_optional" file="tests/test_flow.py" line="4" time="0.001"><skipped type="pytest.skip" message="optional">synthetic diagnostic</skipped></testcase></testsuite></testsuites>`

func pytestContract(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(canonicalConsumerContract{
		SchemaVersion: "compound.proof_consumers.v1", Profile: "python_uv_consumers_v1",
		Consumers: []CanonicalConsumer{{ID: "python-flow", Command: []string{"sh", "proof.sh"}, Dir: ".", TimeoutSeconds: 10,
			RequiredTools: []string{"sh", "uv"}, ReportKind: "pytest_junit",
			RequiredTests: []string{pytestRequired}, AllowedSkips: []string{pytestSkipped}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func pytestStep(t *testing.T) ProofCheckStep {
	t.Helper()
	body := pytestContract(t)
	var c canonicalConsumerContract
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	step := canonicalConsumerStep(c.Consumers[0], body)
	step.ConsumerEvidence = &CanonicalConsumerEvidence{Contract: body, Report: []byte(pytestExample)}
	return step
}
func pytestRepo(t *testing.T, extra map[string]string) (string, gitrepository.Snapshot) {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{CanonicalConsumerContractPath: string(pytestContract(t)),
		"pyproject.toml": "[project]\nname = 'synthetic'\nversion = '0.1.0'\n", "uv.lock": "version = 1\n"}
	for name, body := range extra {
		files[name] = body
	}
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	nodeGit(t, repo, "init", "-q")
	nodeGit(t, repo, "add", ".")
	nodeGit(t, repo, "commit", "-qm", "synthetic Python consumer")
	head := nodeGit(t, repo, "rev-parse", "HEAD")
	source, err := gitrepository.CaptureCleanSnapshot(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	return repo, source
}
func TestCanonicalPytestAcceptsExactXunit1Evidence(t *testing.T) {
	if err := ValidateCanonicalConsumerEvidence(pytestStep(t)); err != nil {
		t.Fatalf("Python native evidence rejected: %v", err)
	}
}
func TestCanonicalPytestRootProfile(t *testing.T) {
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatalf("Python profile rejected: %v", err)
	}
	if plan.ID != "python_uv_consumers_v1" || len(plan.Steps) != 3 {
		t.Fatalf("wrong native inventory: %+v", plan)
	}
}
func TestCanonicalPytestReportFalsifiers(t *testing.T) {
	for name, change := range map[string]func(*ProofCheckStep){
		"missing required": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `name="test_flow"`, `name="test_other"`, 1))
		},
		"wrong counts": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `tests="2"`, `tests="3"`, 1))
		},
		"missing counts": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), ` tests="2"`, "", 1))
		},
		"failure": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<skipped `, `<failure `, 1))
		},
		"xfail": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), "pytest.skip", "pytest.xfail", 1))
		},
		"undeclared skip": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), "test_optional", "test_other", 1))
		},
		"required skip": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "test_optional", "test_flow"))
		},
		"absolute file": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "tests/test_flow.py", "/proof/tests/test_flow.py"))
		},
		"escape file": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "tests/test_flow.py", "../tests/test_flow.py"))
		},
		"xunit2": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), ` file="tests/test_flow.py"`, ""))
		},
		"duplicate attr": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<testcase `, `<testcase name="duplicate" `, 1))
		},
		"unknown attr": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<testcase `, `<testcase status="passed" `, 1))
		},
		"namespace": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), "<testsuites>", `<testsuites xmlns="urn:example.invalid">`, 1))
		},
		"trailing root": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = append(s.ConsumerEvidence.Report, []byte("<testsuites/>")...)
		},
		"truncated": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = s.ConsumerEvidence.Report[:len(s.ConsumerEvidence.Report)-15]
		},
		"hidden outcome": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), "</skipped>", "<failure/></skipped>", 1))
		},
		"Xcode tree":       func(s *ProofCheckStep) { s.ConsumerEvidence.Tests = []byte("{}") },
		"changed contract": func(s *ProofCheckStep) { s.ConsumerEvidence.Contract = append(s.ConsumerEvidence.Contract, ' ') },
	} {
		t.Run(name, func(t *testing.T) {
			s := pytestStep(t)
			change(&s)
			if err := ValidateCanonicalConsumerEvidence(s); err == nil {
				t.Fatal("false Python native success")
			}
		})
	}
}
func TestCanonicalPytestRunnerPrivateEnvironment(t *testing.T) {
	proof := `#!/bin/sh
set -eu
for key in AWS_ACCESS_KEY_ID DATABASE_URL PYTHONPATH PYTHONHOME PYTEST_ADDOPTS_INHERITED; do
 eval 'test -z "${'${key}'+x}"'
done
test "$UV_OFFLINE" = 1
test "$UV_FROZEN" = 1
test "$UV_NO_CONFIG" = 1
test "$UV_PYTHON_DOWNLOADS" = never
test "$UV_PROJECT_ENVIRONMENT" = "$TMPDIR/venv"
test "$UV_CACHE_DIR" = "$TMPDIR/uv-cache"
test "$PYTEST_ADDOPTS" = "--runxfail -o junit_family=xunit1"
case "$HOME" in "$TMPDIR"/*) ;; *) exit 3;; esac
printf '%s' '` + pytestExample + `' > "$COMPOUND_PROOF_REPORT"
`
	repo, source := pytestRepo(t, map[string]string{"proof.sh": proof})
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "DATABASE_URL", "PYTHONPATH", "PYTHONHOME", "UV_PROJECT_ENVIRONMENT", "UV_CACHE_DIR", "PYTEST_ADDOPTS"} {
		t.Setenv(key, "synthetic-do-not-inherit")
	}
	// This test exercises runner isolation, not uv or pytest behavior.
	resolver := func(name string) (string, error) {
		if name == "uv" {
			return exec.LookPath("sh")
		}
		return exec.LookPath(name)
	}
	output, evidence, err := RunCanonicalConsumer(context.Background(), repo, plan.Steps[0], []string{source.HeadSHA}, exec.CommandContext, resolver)
	if err != nil {
		t.Fatalf("private Python runner failed: %v: %s", err, output)
	}
	step := plan.Steps[0]
	step.ConsumerEvidence = evidence
	if err := ValidateCanonicalConsumerEvidence(step); err != nil {
		t.Fatal(err)
	}
}

func pytestGoldenStep(t *testing.T) ProofCheckStep {
	t.Helper()
	body := pytestContract(t)
	var contract canonicalConsumerContract
	if err := json.Unmarshal(body, &contract); err != nil {
		t.Fatal(err)
	}
	contract.Consumers[0].RequiredTests = []string{
		`["test_native.py","test_native","test_values[alpha]"]`,
		`["test_native.py","test_native","test_values[beta]"]`,
		`["test_native.py","test_native.TestGroup","test_nested"]`,
	}
	contract.Consumers[0].AllowedSkips = []string{`["test_native.py","test_native","test_optional"]`}
	body, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	report, err := os.ReadFile("testdata/pytest9/native.junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	step := canonicalConsumerStep(contract.Consumers[0], body)
	step.ConsumerEvidence = &CanonicalConsumerEvidence{Contract: body, Report: report}
	return step
}
func TestCanonicalPytestGenuineNativeFixture(t *testing.T) {
	if err := ValidateCanonicalConsumerEvidence(pytestGoldenStep(t)); err != nil {
		t.Fatalf("genuine pytest report rejected: %v", err)
	}
}
func TestCanonicalPytestOwnedFinalizerRechecksNativeEvidence(t *testing.T) {
	step := pytestGoldenStep(t)
	report, _, reservation := consumerOwnedReportFixture(t, "go_test_json")
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, reservation.Workspace, "", []ProofResourceGateProfile{
		{Name: step.Name, Command: step.Command, Dir: step.Dir, ProofRef: step.ProofRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation.Profile = profile
	report["steps"] = []any{map[string]any{"name": step.Name, "command": step.Command, "dir": step.Dir, "proof_ref": step.ProofRef, "status": "ok", "consumer_evidence": step.ConsumerEvidence}}
	report["proof_refs"] = []string{step.ProofRef}
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatal(err)
	}
	step.ConsumerEvidence.Report = []byte(strings.Replace(string(step.ConsumerEvidence.Report), `tests="4"`, `tests="5"`, 1))
	if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
		t.Fatal("finalizer accepted altered pytest counts")
	}
}
func TestCanonicalPytestDeclarationFalsifiers(t *testing.T) {
	for _, marker := range []string{"pyproject.toml", "uv.lock"} {
		t.Run("missing "+marker, func(t *testing.T) {
			repo, _ := pytestRepo(t, nil)
			nodeGit(t, repo, "rm", marker)
			nodeGit(t, repo, "commit", "-qm", "missing marker")
			head := nodeGit(t, repo, "rev-parse", "HEAD")
			source, err := gitrepository.CaptureCleanSnapshot(repo, head)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveCanonicalProofPlan(repo, source, nil); err == nil {
				t.Fatal("missing Python marker accepted")
			}
		})
	}
	for name, change := range map[string]func(*canonicalConsumerContract){
		"wrong report": func(c *canonicalConsumerContract) { c.Consumers[0].ReportKind = "go_test_json" },
		"missing uv":   func(c *canonicalConsumerContract) { c.Consumers[0].RequiredTools = []string{"sh"} },
		"ambient DSN":  func(c *canonicalConsumerContract) { c.Consumers[0].RequiredEnvironment = []string{"DATABASE_URL"} },
		"ambient environment": func(c *canonicalConsumerContract) {
			c.Consumers[0].RequiredEnvironment = []string{"UV_PROJECT_ENVIRONMENT"}
		},
		"whitespace identity": func(c *canonicalConsumerContract) {
			c.Consumers[0].RequiredTests = []string{`[ "tests/test_flow.py","tests.test_flow","test_flow"]`}
		},
		"absolute identity": func(c *canonicalConsumerContract) {
			c.Consumers[0].RequiredTests = []string{`["/tests/test_flow.py","tests.test_flow","test_flow"]`}
		},
		"empty classname": func(c *canonicalConsumerContract) {
			c.Consumers[0].RequiredTests = []string{`["tests/test_flow.py","","test_flow"]`}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var c canonicalConsumerContract
			if err := json.Unmarshal(pytestContract(t), &c); err != nil {
				t.Fatal(err)
			}
			change(&c)
			body, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseCanonicalConsumerContract(body); err == nil {
				t.Fatal("invalid Python contract accepted")
			}
		})
	}
}
