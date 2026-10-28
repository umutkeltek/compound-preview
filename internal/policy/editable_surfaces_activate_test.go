package policy

import (
	"os"
	"strings"
	"testing"
)

// TestActivateSurfaceAddsActiveEntryIdempotently is the whitelist write path the
// harness-edit review verb rides: ActivateSurface adds a surface as active, is
// byte-idempotent on re-run (no duplicate entry, identical file), and preserves
// every other surface already in the registry.
func TestActivateSurfaceAddsActiveEntryIdempotently(t *testing.T) {
	home := t.TempDir()
	// A pre-existing OTHER surface must survive the activation untouched.
	if err := WriteEditableSurfaces(home, EditableSurfaces{Entries: []EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	reg, changed, err := ActivateSurface(home, "capability_edges")
	if err != nil {
		t.Fatalf("ActivateSurface: %v", err)
	}
	if !changed {
		t.Fatalf("activating an absent surface must report changed=true")
	}
	if allowed, reason := reg.Allowed("capability_edges"); !allowed {
		t.Fatalf("activated surface must be allowed, got denied: %s", reason)
	}
	if allowed, _ := reg.Allowed("skill_descriptions"); !allowed {
		t.Fatalf("pre-existing surface must survive activation")
	}

	path := EditableSurfacesPath(home)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Re-run: idempotent. No new entry, identical bytes, changed=false.
	reg2, changed2, err := ActivateSurface(home, "capability_edges")
	if err != nil {
		t.Fatalf("ActivateSurface re-run: %v", err)
	}
	if changed2 {
		t.Fatalf("re-activating an already-active surface must report changed=false")
	}
	if reg2.ActiveCount() != 2 {
		t.Fatalf("re-run must not duplicate the entry; active count = %d, want 2", reg2.ActiveCount())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("idempotent re-run rewrote different bytes:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestActivateSurfaceReactivatesRetired proves a retired surface flips back to
// active (the toggle is reversible both ways) and reports changed=true.
func TestActivateSurfaceReactivatesRetired(t *testing.T) {
	home := t.TempDir()
	if err := WriteEditableSurfaces(home, EditableSurfaces{Entries: []EditableSurface{
		{Surface: "capability_edges", Status: EditableSurfaceStatusRetired},
	}}); err != nil {
		t.Fatalf("seed retired: %v", err)
	}
	reg, changed, err := ActivateSurface(home, "capability_edges")
	if err != nil {
		t.Fatalf("ActivateSurface: %v", err)
	}
	if !changed {
		t.Fatalf("re-activating a retired surface must report changed=true")
	}
	if allowed, reason := reg.Allowed("capability_edges"); !allowed {
		t.Fatalf("re-activated surface must be allowed, got denied: %s", reason)
	}
}

// TestActivateSurfaceRefusesHardDenyCore is the fail-closed guard: whitelisting a
// compiled-in hard-deny-core surface (policy/permissions/ledger/evaluator) is
// refused BEFORE any write, so a hand-crafted card can never brick the registry
// by pushing a surface that LoadEditableSurfaces would then flag invalid.
func TestActivateSurfaceRefusesHardDenyCore(t *testing.T) {
	home := t.TempDir()
	for _, core := range HardDenyCoreSurfaces() {
		if _, _, err := ActivateSurface(home, core); err == nil {
			t.Fatalf("ActivateSurface must refuse hard-deny-core surface %q", core)
		}
	}
	// Nothing was written: registry still absent.
	if _, err := os.Stat(EditableSurfacesPath(home)); !os.IsNotExist(err) {
		t.Fatalf("a refused hard-deny activation must write nothing; registry file exists")
	}
}

// TestActivateSurfaceRejectsEmpty guards the empty-surface refusal.
func TestActivateSurfaceRejectsEmpty(t *testing.T) {
	home := t.TempDir()
	if _, _, err := ActivateSurface(home, "  "); err == nil || !strings.Contains(err.Error(), "surface") {
		t.Fatalf("empty surface must be refused with a surface error, got %v", err)
	}
}
