package mem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSelfEntityConfig(t *testing.T, homePath, body string) {
	t.Helper()
	path := SelfEntityConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveSelfEntitySetUnconfiguredHomeDefaultsToOperatorOnly is fail-safe
// case (b): an unconfigured home (no config/identity.yaml at all) recognizes
// ONLY "operator" as self-model — the same behavior the hardcoded token used
// to give the operator's real name, minus the leak.
func TestResolveSelfEntitySetUnconfiguredHomeDefaultsToOperatorOnly(t *testing.T) {
	home := t.TempDir()
	set := ResolveSelfEntitySet(home)
	if !set[DefaultSelfEntity] {
		t.Fatalf("unconfigured home set = %+v, want %q present", set, DefaultSelfEntity)
	}
	if set["examplealias"] {
		t.Fatalf("unconfigured home should not recognize an arbitrary alias: %+v", set)
	}
}

// TestResolveSelfEntitySetConfiguredAliasExtendsDefault is fail-safe case (a):
// COMPOUND_HOME/config/identity.yaml can EXTEND the set without dropping the
// always-present "operator" default.
func TestResolveSelfEntitySetConfiguredAliasExtendsDefault(t *testing.T) {
	home := t.TempDir()
	writeSelfEntityConfig(t, home, "self_entities:\n  - \"example-alias\"\n")
	set := ResolveSelfEntitySet(home)
	if !set[DefaultSelfEntity] {
		t.Fatalf("configured home should still recognize the default operator entity: %+v", set)
	}
	if !set["examplealias"] {
		t.Fatalf("configured alias (normalized) should be present in the resolved set: %+v", set)
	}
}

// TestResolveSelfEntitySetMalformedYAMLFallsBackToDefault is fail-safe case
// (c): a malformed identity.yaml never errors the caller and never silently
// admits garbage — it falls back to exactly the unconfigured default set.
func TestResolveSelfEntitySetMalformedYAMLFallsBackToDefault(t *testing.T) {
	home := t.TempDir()
	writeSelfEntityConfig(t, home, "self_entities: [this is not valid yaml")
	set := ResolveSelfEntitySet(home)
	if len(set) != 1 || !set[DefaultSelfEntity] {
		t.Fatalf("malformed config should fall back to the bare default set, got %+v", set)
	}
}

// TestDigestSelfModelClaimHonorsResolvedSelfEntitySet proves
// digestSelfModelClaim (the mem-side mirror of internal/contract's gate)
// classifies purely off the resolved set across all three fail-safe cases,
// with no hardcoded name and a safe nil fallback for defensive callers.
func TestDigestSelfModelClaimHonorsResolvedSelfEntitySet(t *testing.T) {
	arbitrary := Claim{Entity: "example-alias"}
	operatorClaim := Claim{Entity: "operator"}

	unconfigured := ResolveSelfEntitySet(t.TempDir())
	if digestSelfModelClaim(arbitrary, unconfigured) {
		t.Fatalf("unconfigured set should not classify an arbitrary alias as self-model: %+v", unconfigured)
	}
	if !digestSelfModelClaim(operatorClaim, unconfigured) {
		t.Fatalf("bare operator entity should always classify as self-model: %+v", unconfigured)
	}

	configuredHome := t.TempDir()
	writeSelfEntityConfig(t, configuredHome, "self_entities:\n  - \"example-alias\"\n")
	configured := ResolveSelfEntitySet(configuredHome)
	if !digestSelfModelClaim(arbitrary, configured) {
		t.Fatalf("configured alias should classify as self-model: %+v", configured)
	}

	malformedHome := t.TempDir()
	writeSelfEntityConfig(t, malformedHome, "self_entities: [not valid yaml")
	malformed := ResolveSelfEntitySet(malformedHome)
	if !digestSelfModelClaim(operatorClaim, malformed) {
		t.Fatalf("malformed config should still classify the default operator entity: %+v", malformed)
	}
	if digestSelfModelClaim(arbitrary, malformed) {
		t.Fatalf("malformed config must not admit the arbitrary alias: %+v", malformed)
	}

	// Defensive nil fallback: a caller that skips resolution entirely still
	// gets the bare operator default, never a silently permissive gate.
	if !digestSelfModelClaim(operatorClaim, nil) {
		t.Fatalf("nil selfEntities should still classify the bare operator entity")
	}
	if digestSelfModelClaim(arbitrary, nil) {
		t.Fatalf("nil selfEntities should not classify an arbitrary alias")
	}
}

// TestRenderHarnessDigestExcludesConfiguredSelfEntityAlias is the end-to-end
// falsifier for the mem digest path: an aliased self-entity claim that would
// otherwise be perfectly ambient-eligible (visibility=ambient, claim_type
// status, in the trap topic subtree) never reaches the rendered digest once
// COMPOUND_HOME/config/identity.yaml configures it, and an ordinary claim
// with no self-entity relationship still renders normally.
func TestRenderHarnessDigestExcludesConfiguredSelfEntityAlias(t *testing.T) {
	home := t.TempDir()
	writeSelfEntityConfig(t, home, "self_entities:\n  - \"example-alias\"\n")
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	digestFixtureClaim(t, s, Claim{
		ID: "clm_self_alias", Seq: 1, Entity: "example-alias", Predicate: "note",
		Object:    "an aliased self-entity claim must never appear in the digest once configured",
		ClaimType: "status", Visibility: "ambient", TopicRefs: []string{"trap"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_self_other", Seq: 2, Entity: "pattern:generic-fact", Predicate: "trap",
		Object:    "a non-self-entity claim should render normally alongside it",
		ClaimType: "constraint", Visibility: "ambient", TopicRefs: []string{"trap"}, ValidFrom: "2026-01-01T00:00:00Z",
	})

	_, body, err := s.RenderHarnessDigest(
		HarnessDigestOptions{Harness: "claude-code"},
		HarnessDigestMeta{GeneratedBy: "test", RegenerateCmd: "test"},
		t0,
	)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "an aliased self-entity claim") {
		t.Fatalf("configured self-entity alias leaked into rendered digest:\n%s", text)
	}
	if !strings.Contains(text, "a non-self-entity claim should render normally") {
		t.Fatalf("non-self-entity claim should have rendered:\n%s", text)
	}
}
