package main

import (
	"strings"
	"testing"

	"compound/internal/policy"
)

// TestPolicyEditableSurfacesSurfacesState proves `compound policy editable-surfaces` reads
// the live registry: an absent home reports installed:false / nothing_editable:true, and an
// installed valid registry surfaces its entries in both JSON and text forms.
func TestPolicyEditableSurfacesSurfacesState(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	absent := captureStdout(t, func() { cmdPolicy([]string{"editable-surfaces", "--json"}) })
	for _, want := range []string{
		`"authority_ref": "compound://registries/editable-surfaces"`,
		`"installed": false`,
		`"nothing_editable": true`,
		`"source": "unset"`,
		`"valid": true`,
	} {
		if !strings.Contains(absent, want) {
			t.Fatalf("absent editable-surfaces output missing %q:\n%s", want, absent)
		}
	}

	if err := policy.WriteEditableSurfaces(homePath, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "skill_descriptions", Description: "generated skill descriptions", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
		{Surface: "injection_templates", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusRetired},
	}}); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	setJSON := captureStdout(t, func() { cmdPolicy([]string{"editable-surfaces", "--json"}) })
	for _, want := range []string{
		`"installed": true`,
		`"valid": true`,
		`"nothing_editable": false`,
		`"surface": "skill_descriptions"`,
		`"status": "active"`,
		`"entry_count": 2`,
		`"active_count": 1`,
		`"hard_deny_core"`,
	} {
		if !strings.Contains(setJSON, want) {
			t.Fatalf("installed editable-surfaces --json missing %q:\n%s", want, setJSON)
		}
	}

	setText := captureStdout(t, func() { cmdPolicy([]string{"editable-surfaces"}) })
	for _, want := range []string{"installed: true", "nothing_editable: false", "skill_descriptions [active]", "entries: 2 (active: 1)"} {
		if !strings.Contains(setText, want) {
			t.Fatalf("installed editable-surfaces text missing %q:\n%s", want, setText)
		}
	}
}

// TestPolicyEditableSurfacesReportsInvalid proves the CLI loudly reports a registry that
// whitelists a HARD-DENY core surface: valid:false, nothing_editable:true, and the
// validation error naming the core violation is surfaced.
func TestPolicyEditableSurfacesReportsInvalid(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := policy.WriteEditableSurfaces(homePath, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "ledger", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	out := captureStdout(t, func() { cmdPolicy([]string{"editable-surfaces", "--json"}) })
	for _, want := range []string{
		`"valid": false`,
		`"nothing_editable": true`,
		"HARD-DENY",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("invalid editable-surfaces output missing %q:\n%s", want, out)
		}
	}
}
