package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func consumerOwnedReportFixture(t *testing.T, kind string) (map[string]any, map[string]any, ProofResourceReservation) {
	t.Helper()
	profile, required := "go_consumers_v1", "example.invalid/product::TestActivation"
	var allowed []string
	if kind == "xcresult" {
		profile, required = "xcode_consumers_v1", "ActivationTests/testFirstAppearance()"
		allowed = []string{"HardwareTests/testCamera()"}
	}
	contract, err := json.Marshal(map[string]any{"schema_version": "compound.proof_consumers.v1", "profile": profile, "consumers": []any{map[string]any{
		"id": "activation", "command": []string{"make", "test-activation"}, "dir": ".", "timeout_seconds": 240, "required_tools": []string{"make"}, "report_kind": kind, "required_tests": []string{required}, "allowed_skips": allowed,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contract)
	ref := "consumer:activation:sha256:" + hex.EncodeToString(sum[:])
	gate := ProofResourceGateProfile{Name: "consumer:activation", Command: []string{"make", "test-activation"}, Dir: ".", ProofRef: ref}
	p, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, t.TempDir(), "", []ProofResourceGateProfile{gate})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	r := ProofResourceReservation{Profile: p, Workspace: p.Repo, AcquiredAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	evidence := map[string]any{"contract": contract}
	if kind == "xcresult" {
		evidence["report"] = []byte(`{"result":"Passed","totalTestCount":2,"passedTests":1,"failedTests":0,"skippedTests":1,"expectedFailures":0,"testFailures":[]}`)
		evidence["tests"] = []byte(`{"testNodes":[{"nodeType":"Test Plan","children":[{"nodeType":"Test Case","nodeIdentifier":"ActivationTests/testFirstAppearance()","result":"Passed"},{"nodeType":"Test Case","nodeIdentifier":"HardwareTests/testCamera()","result":"Skipped"}]}]}`)
	} else {
		evidence["report"] = []byte("{\"Action\":\"start\",\"Package\":\"example.invalid/product\"}\n{\"Action\":\"run\",\"Package\":\"example.invalid/product\",\"Test\":\"TestActivation\"}\n{\"Action\":\"pass\",\"Package\":\"example.invalid/product\",\"Test\":\"TestActivation\"}\n{\"Action\":\"pass\",\"Package\":\"example.invalid/product\"}\n")
	}
	step := map[string]any{"name": gate.Name, "command": gate.Command, "dir": gate.Dir, "proof_ref": ref, "status": "ok", "consumer_evidence": evidence}
	report := map[string]any{"schema_version": "compound.proof_check_report.v0.4", "profile_kind": p.Kind, "repo": p.Repo, "status": "ok", "generated_at": now.Format(time.RFC3339), "steps": []any{step}, "proof_refs": []string{ref}}
	return report, evidence, r
}

func validateConsumerOwnedFixture(t *testing.T, report map[string]any, r ProofResourceReservation) error {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ownedProofReport
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return validateOwnedProofReportAgainstReservation(decoded, r)
}

func TestCanonicalConsumerOwnedReportRequiresNativeEvidence(t *testing.T) {
	for _, kind := range []string{"go_test_json", "xcresult"} {
		t.Run(kind, func(t *testing.T) {
			report, _, reservation := consumerOwnedReportFixture(t, kind)
			if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
				t.Fatalf("valid native report: %v", err)
			}
			delete(report["steps"].([]any)[0].(map[string]any), "consumer_evidence")
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("missing native evidence certified by owned finalizer")
			}
		})
	}
}

func TestCanonicalConsumerOwnedReportRejectsChangedContract(t *testing.T) {
	report, evidence, reservation := consumerOwnedReportFixture(t, "go_test_json")
	evidence["contract"] = append(evidence["contract"].([]byte), ' ')
	if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
		t.Fatal("contract bytes detached from admitted digest")
	}
}

func TestCanonicalConsumerOwnedXcodeAllowsSkipDiagnostic(t *testing.T) {
	report, evidence, reservation := consumerOwnedReportFixture(t, "xcresult")
	evidence["tests"] = []byte(strings.Replace(string(evidence["tests"].([]byte)), `"result":"Skipped"`, `"result":"Skipped","children":[{"name":"Test skipped - Physical-device only.","nodeType":"Failure Message"}]`, 1))
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("native optional hardware skip diagnostic rejected: %v", err)
	}
}

func TestCanonicalConsumerOwnedXcodeReportRejectsFalseGreen(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing tree": func(e map[string]any) { delete(e, "tests") },
		"summary only": func(e map[string]any) { e["tests"] = []byte(`{"testNodes":[]}`) },
		"wrong counts": func(e map[string]any) {
			e["report"] = []byte(strings.Replace(string(e["report"].([]byte)), `"passedTests":1`, `"passedTests":2`, 1))
		},
		"missing required": func(e map[string]any) {
			e["tests"] = []byte(strings.ReplaceAll(string(e["tests"].([]byte)), "testFirstAppearance", "testUnrelated"))
		},
		"required skip": func(e map[string]any) {
			e["tests"] = []byte(strings.ReplaceAll(string(e["tests"].([]byte)), `"result":"Passed"`, `"result":"Skipped"`))
		},
		"unexpected skip": func(e map[string]any) {
			e["tests"] = []byte(strings.ReplaceAll(string(e["tests"].([]byte)), "HardwareTests/testCamera", "OtherTests/testSkipped"))
		},
		"duplicate required": func(e map[string]any) {
			e["tests"] = []byte(strings.ReplaceAll(string(e["tests"].([]byte)), "HardwareTests/testCamera()", "ActivationTests/testFirstAppearance()"))
		},
		"expected failure": func(e map[string]any) {
			e["report"] = []byte(strings.Replace(string(e["report"].([]byte)), `"expectedFailures":0`, `"expectedFailures":1`, 1))
		},
		"failed summary": func(e map[string]any) {
			e["report"] = []byte(strings.Replace(string(e["report"].([]byte)), "Passed", "Failed", 1))
		},
		"malformed": func(e map[string]any) { e["tests"] = []byte("not JSON") },
		"duplicate keys": func(e map[string]any) {
			e["report"] = []byte(strings.Replace(string(e["report"].([]byte)), `"failedTests":0`, `"failedTests":1,"failedTests":0`, 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report, evidence, reservation := consumerOwnedReportFixture(t, "xcresult")
			mutate(evidence)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatalf("%s accepted by finalizer", name)
			}
		})
	}
}
