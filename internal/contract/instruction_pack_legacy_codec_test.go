package contract

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestRuntimeSealingClassifiesLegacyContractBeforeDigest(t *testing.T) {
	body, err := os.ReadFile("../envelope/testdata/effective_contract_v01.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy envelope.EffectiveContract
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	in := Input{Harness: "codex", Cwd: t.TempDir(), Task: "review synthetic fixture", RepoKind: "product", RepoKindVia: "flag"}
	runtime := RuntimeMaterialization{EffectiveContract: legacy, ContextPack: Pack(in, DefaultPolicy(), now)}
	runtime.ContextPack.IDs.ContractID = legacy.ID
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.EffectiveContract.ID != legacy.ID {
		t.Fatalf("historical sealed identity changed: %s", sealed.EffectiveContract.ID)
	}
	homePath := t.TempDir()
	if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadContextPackContract(homePath, sealed.ContextPack.ID); err != nil {
		t.Fatalf("historical proof became unreadable: %v", err)
	}
	if _, err := RenderInstructionPackForContext(homePath, "codex", "", sealed.ContextPack.ID, now); err == nil {
		t.Fatal("historical unbound evidence became current profile authority")
	}
	runtime.EffectiveContract.ContextKey = runtimeContextKey(in)
	if _, err := SealRuntimeMaterialization(runtime); err == nil {
		t.Fatal("old schema accepted new digest-covered context field")
	}
}
