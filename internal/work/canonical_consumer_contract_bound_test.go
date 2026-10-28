package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/envelope"
)

const proposedConsumerContractBound = 128 << 10

func contractBoundPadded(t *testing.T, size int) []byte {
	t.Helper()
	if size == 0 {
		return nil
	}
	body := pytestContract(t)
	if len(body) > size {
		t.Fatalf("fixture has %d bytes, requested %d", len(body), size)
	}
	return append(body, bytes.Repeat([]byte(" "), size-len(body))...)
}

func contractBound669(t *testing.T) ([]byte, canonicalConsumerContract) {
	t.Helper()
	var contract canonicalConsumerContract
	if err := json.Unmarshal(pytestContract(t), &contract); err != nil {
		t.Fatal(err)
	}
	contract.Consumers[0].AllowedSkips = nil
	contract.Consumers[0].RequiredTests = nil
	for i := 0; i < 669; i++ {
		identity, err := json.Marshal([]string{
			"tests/test_contract_bound.py",
			"tests.test_contract_bound",
			fmt.Sprintf("test_exact_requirement_%04d_%s", i, strings.Repeat("x", 80)),
		})
		if err != nil {
			t.Fatal(err)
		}
		contract.Consumers[0].RequiredTests = append(contract.Consumers[0].RequiredTests, string(identity))
	}
	body, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 64<<10 || len(body) > proposedConsumerContractBound {
		t.Fatalf("large exact-identity fixture has unexpected size %d", len(body))
	}
	return body, contract
}

func contractBoundPytestReport(t *testing.T, required []string) []byte {
	t.Helper()
	var report strings.Builder
	fmt.Fprintf(&report, "<testsuites><testsuite name=\"pytest\" errors=\"0\" failures=\"0\" skipped=\"0\" tests=\"%d\">", len(required))
	for _, raw := range required {
		var identity []string
		if err := json.Unmarshal([]byte(raw), &identity); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&report, "<testcase file=\"%s\" classname=\"%s\" name=\"%s\"/>", identity[0], identity[1], identity[2])
	}
	report.WriteString("</testsuite></testsuites>")
	return []byte(report.String())
}

func contractBoundOwnedReport(t *testing.T, body []byte, contract canonicalConsumerContract, required []string) (map[string]any, ProofResourceReservation) {
	t.Helper()
	step := canonicalConsumerStep(contract.Consumers[0], body)
	step.ConsumerEvidence = &CanonicalConsumerEvidence{
		Contract: append([]byte(nil), body...),
		Report:   contractBoundPytestReport(t, required),
	}
	report, _, reservation := consumerOwnedReportFixture(t, "go_test_json")
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, reservation.Workspace, "", []ProofResourceGateProfile{
		{Name: step.Name, Command: step.Command, Dir: step.Dir, ProofRef: step.ProofRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation.Profile = profile
	report["steps"] = []any{map[string]any{
		"name": step.Name, "command": step.Command, "dir": step.Dir,
		"proof_ref": step.ProofRef, "status": "ok", "consumer_evidence": step.ConsumerEvidence,
	}}
	report["proof_refs"] = []string{step.ProofRef}
	return report, reservation
}

func TestCanonicalConsumerContractBoundBytes(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{
		{0, false},
		{65536, true},
		{65537, true},
		{131072, true},
		{131073, false},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			_, err := parseCanonicalConsumerContract(contractBoundPadded(t, tc.size))
			if (err == nil) != tc.ok {
				t.Fatalf("size %d: accepted=%t, want %t, error=%v", tc.size, err == nil, tc.ok, err)
			}
		})
	}
}

func TestCanonicalConsumerContractBoundPreserves669Identities(t *testing.T) {
	body, expected := contractBound669(t)
	got, err := parseCanonicalConsumerContract(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Consumers[0].RequiredTests) != 669 || !reflect.DeepEqual(got, expected) {
		t.Fatal("large declaration changed or lost exact required identities or consumer fields")
	}
}

func TestCanonicalConsumerContractBoundOwnedEvidence(t *testing.T) {
	body, contract := contractBound669(t)
	required := contract.Consumers[0].RequiredTests
	t.Run("complete report", func(t *testing.T) {
		report, reservation := contractBoundOwnedReport(t, body, contract, required)
		if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("changed sealed declaration", func(t *testing.T) {
		report, reservation := contractBoundOwnedReport(t, body, contract, required)
		step := report["steps"].([]any)[0].(map[string]any)
		evidence := step["consumer_evidence"].(*CanonicalConsumerEvidence)
		evidence.Contract = append(evidence.Contract, ' ')
		if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
			t.Fatal("finalizer accepted altered large sealed declaration")
		}
	})
	t.Run("missing required identity", func(t *testing.T) {
		report, reservation := contractBoundOwnedReport(t, body, contract, required[:len(required)-1])
		if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
			t.Fatal("finalizer accepted report missing an exact required identity")
		}
	})
}

func TestCanonicalConsumerContractBoundDispatch(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{
		{65537, true},
		{131072, true},
		{131073, false},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			workspace, home := t.TempDir(), t.TempDir()
			body := contractBoundPadded(t, tc.size)
			contractPath := filepath.Join(workspace, CanonicalConsumerContractPath)
			if err := os.WriteFile(contractPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			var pack envelope.ContextPack
			attachDispatchRecipe(&pack, workspace, home)
			if !tc.ok {
				if len(pack.RequiredChecks) != 0 || len(pack.SourceRevisions) != 0 || len(pack.Excluded) != 1 {
					t.Fatalf("oversize declaration reached dispatch or lost exclusion: %+v", pack)
				}
				if !strings.Contains(pack.Excluded[0].Reason, fmt.Sprint(proposedConsumerContractBound)) {
					t.Fatalf("dispatch exclusion does not report shared bound: %s", pack.Excluded[0].Reason)
				}
				return
			}
			if len(pack.RequiredChecks) != 1 || len(pack.Excluded) != 0 {
				t.Fatalf("in-bound declaration was not dispatched: %+v", pack)
			}
			sum := sha256.Sum256(body)
			want := envelope.SourceRev{Source: "file://" + contractPath, Rev: "sha256:" + hex.EncodeToString(sum[:])}
			if len(pack.SourceRevisions) != 1 || pack.SourceRevisions[0] != want {
				t.Fatalf("dispatch did not bind exact declaration bytes: got %+v, want %+v", pack.SourceRevisions, want)
			}
		})
	}
}
