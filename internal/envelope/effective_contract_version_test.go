package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEffectiveContractFrozenV01RemainsByteAndHashStable(t *testing.T) {
	body, err := os.ReadFile("testdata/effective_contract_v01.json")
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.TrimSuffix(string(body), "\n"))
	var legacy EffectiveContract
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.SchemaVersion != "compound.effective_contract.v0.1" {
		t.Fatal("frozen previous schema changed")
	}
	if kind, err := ValidateBytes(body); err != nil || kind != "effective_contract" {
		t.Fatalf("historical envelope unreadable: %q, %v", kind, err)
	}
	roundTrip, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTrip) != string(body) {
		t.Fatalf("historical bytes changed:\n%s", roundTrip)
	}
	legacy.ID = ""
	unsealed, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(unsealed)
	if hex.EncodeToString(sum[:]) != "6f810a5b0bfd71f09b6bd7dbcccd4882318cca9705bf98ace7a61b16827a787c" {
		t.Fatalf("historical digest changed: %x", sum)
	}
}

func TestEffectiveContractContextKeyHasVersionedCodec(t *testing.T) {
	if SVEffectiveContract != "compound.effective_contract.v0.2" {
		t.Errorf("current schema=%q, want frozen v0.2 literal", SVEffectiveContract)
	}
	for _, tc := range []struct {
		name, version, key string
		valid              bool
	}{
		{"old-unbound", "compound.effective_contract.v0.1", "", true},
		{"old-cannot-carry-new-field", "compound.effective_contract.v0.1", strings.Repeat("a", 64), false},
		{"current-scoped", "compound.effective_contract.v0.2", strings.Repeat("a", 64), true},
		{"current-unbound-evidence", "compound.effective_contract.v0.2", "", true},
		{"current-malformed", "compound.effective_contract.v0.2", "not-a-digest", false},
		{"current-uppercase", "compound.effective_contract.v0.2", strings.Repeat("A", 64), false},
		{"unknown", "compound.effective_contract.v99", strings.Repeat("a", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec := EffectiveContract{Header: hdr(tc.version, "contract_codec"), Harness: "codex", ContextKey: tc.key}
			body, err := json.Marshal(ec)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ValidateBytes(body)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
