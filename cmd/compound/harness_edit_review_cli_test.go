package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/policy"
)

// TestAuthorityHarnessEditReviewWhitelistDispatch proves the new verb is wired
// into cmdAuthority end to end: a real dispatch whitelists the card's target
// surface (the editable-surfaces registry gains the active surface) and prints
// the recorded-decision report as JSON.
func TestAuthorityHarnessEditReviewWhitelistDispatch(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	cardDir := filepath.Join(homePath, "reviews", "harness-edit-proposals")
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cardPath := filepath.Join(cardDir, "dirprop_clitest.json")
	cardBody := `{"schema_version":"compound.harness_edit_proposal_card.v0.1","id":"dirprop_clitest",` +
		`"status":"review_required","target_surface":"capability_edges"}` + "\n"
	if err := os.WriteFile(cardPath, []byte(cardBody), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdAuthority([]string{"harness-edit-review", cardPath, "--decision", "whitelist", "--rationale", "agents may trim dead offers here", "--actor", "operator:cli-test", "--json"})
	})
	for _, want := range []string{
		`"decision": "whitelist"`,
		`"target_surface": "capability_edges"`,
		`"surface_whitelisted": true`,
		`"status": "recorded"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("harness-edit-review output missing %q:\n%s", want, out)
		}
	}

	// The registry was actually written and the surface is active.
	reg, _, err := policy.LoadEditableSurfaces(homePath)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if allowed, reason := reg.Allowed("capability_edges"); !allowed {
		t.Fatalf("dispatch must whitelist the surface, got denied: %s", reason)
	}
}
