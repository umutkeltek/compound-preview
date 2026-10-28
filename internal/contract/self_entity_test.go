package contract

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/mem"
)

// TestSelfModelClaimHonorsResolvedSelfEntitySet is the contract-side half of
// the fail-safe self-entity seam (see compound/internal/mem.ResolveSelfEntitySet
// + self_entity_config.go): selfModelClaim classifies purely off the
// resolved set, never a hardcoded name, across all three fail-safe cases.
func TestSelfModelClaimHonorsResolvedSelfEntitySet(t *testing.T) {
	arbitrary := mem.Claim{Entity: "example-alias", Visibility: "ambient", ClaimType: "status"}
	operatorClaim := mem.Claim{Entity: "operator"}

	// (b) no config: only "operator" is self-model, an arbitrary alias is not.
	unconfigured := mem.ResolveSelfEntitySet(t.TempDir())
	if selfModelClaim(arbitrary, unconfigured) {
		t.Fatalf("unconfigured home should not classify an arbitrary alias as self-model: set=%+v", unconfigured)
	}
	if !selfModelClaim(operatorClaim, unconfigured) {
		t.Fatalf("bare operator entity should always classify as self-model even unconfigured: set=%+v", unconfigured)
	}

	// (a) alias configured: the aliased entity now classifies as self-model.
	configuredHome := t.TempDir()
	writeContractSelfEntityConfig(t, configuredHome, "self_entities:\n  - \"example-alias\"\n")
	configured := mem.ResolveSelfEntitySet(configuredHome)
	if !selfModelClaim(arbitrary, configured) {
		t.Fatalf("configured alias should classify as self-model once present in the resolved set: set=%+v", configured)
	}
	if !configured[mem.DefaultSelfEntity] {
		t.Fatalf("configured set must still carry the default operator entity: %+v", configured)
	}

	// (c) malformed yaml: falls back to the default set, no error propagated,
	// and the fallback never silently admits the arbitrary alias.
	malformedHome := t.TempDir()
	writeContractSelfEntityConfig(t, malformedHome, "self_entities: [not valid yaml")
	malformed := mem.ResolveSelfEntitySet(malformedHome)
	if !selfModelClaim(operatorClaim, malformed) {
		t.Fatalf("malformed config should fall back to default set, still classifying operator: set=%+v", malformed)
	}
	if selfModelClaim(arbitrary, malformed) {
		t.Fatalf("malformed config should not somehow admit the arbitrary alias: set=%+v", malformed)
	}
}

// TestSelfModelClaimNilSelfEntitiesFallsBackToBareOperator proves the
// defensive nil path (a caller that skips resolution) never goes silently
// permissive: it still recognizes "operator" and nothing else.
func TestSelfModelClaimNilSelfEntitiesFallsBackToBareOperator(t *testing.T) {
	if !selfModelClaim(mem.Claim{Entity: "operator"}, nil) {
		t.Fatalf("nil selfEntities should still classify the bare operator entity")
	}
	if selfModelClaim(mem.Claim{Entity: "example-alias"}, nil) {
		t.Fatalf("nil selfEntities should not classify an arbitrary alias")
	}
}

// TestCaptureRuntimeMemoryDependenciesResolvesConfiguredSelfEntity proves the
// production wiring seam: captureRuntimeMemoryDependencies (the entry point
// every orient/pack path calls once per runtime snapshot) resolves
// identity.yaml under homePath into dependencies.SelfEntities, which is what
// loadMemoryRefsFromView / loadPositionRefsFromView / probeMemoryPolicyAllows
// thread down to selfModelClaim.
func TestCaptureRuntimeMemoryDependenciesResolvesConfiguredSelfEntity(t *testing.T) {
	home := t.TempDir()
	writeContractSelfEntityConfig(t, home, "self_entities:\n  - \"example-alias\"\n")
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	deps := captureRuntimeMemoryDependencies(home, Input{}, view)
	if !deps.SelfEntities["operator"] {
		t.Fatalf("resolved self entities should always include operator: %+v", deps.SelfEntities)
	}
	if !deps.SelfEntities["examplealias"] {
		t.Fatalf("resolved self entities should include the configured alias: %+v", deps.SelfEntities)
	}
}

func writeContractSelfEntityConfig(t *testing.T, homePath, body string) {
	t.Helper()
	path := mem.SelfEntityConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
