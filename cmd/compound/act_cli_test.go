package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
)

// These tests drive the `compound act` verb — the operator door onto the generic governed
// outbound-write seam. They prove the CLI's own responsibilities: the dry-run default writes
// nothing, --confirm without an allowing PolicyDecision REFUSES and leaves the target
// byte-identical (the governance floor), and a granted --confirm performs the governed write and
// records a resolvable receipt to the canonical event store. Every proof mutates only a mktemp
// config; nothing touches a real home and no provider call is made.

func actScratch(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, "switchback.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}
	return path
}

// FALSIFIER (governance floor): --confirm WITHOUT an allowing PolicyDecision refuses and leaves
// the target byte-identical. A mutation without a matching allowing decision must not happen.
func TestActRepointRefusesWithoutGrant(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := actScratch(t, home, "egress:\n  auto/judge: providerA\n")
	before, _ := os.ReadFile(path)

	var out, errOut bytes.Buffer
	code := runActRepoint([]string{
		"--target-path", path, "--target-id", "egress.auto/judge",
		"--value", "providerB", "--work", "work_nogrant", "--confirm", "--home", home,
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("expected non-zero exit when no grant allows the write; stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "refused") {
		t.Fatalf("expected a refusal message, got stderr=%q", errOut.String())
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("config mutated despite refusal:\nbefore=%q\nafter=%q", before, after)
	}
}

// The governance floor above proves the write did not happen; this proves the operator is
// told how to make it happen legitimately. A refusal that names only the outcome sends the
// reader looking for the grant surface by hand, and the nearest thing to hand is editing the
// target file directly — the exact ungoverned move the gate exists to prevent.
//
// RED WITHOUT the conversion: the site printed `refused: act: blocked by policy (hold): …`,
// which carries no gate name, no tried-trace, and no command, so assertTeachingRefusal fails
// on the first assertion.
func TestActRepointRefusalTeachesTheGrantPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := actScratch(t, home, "egress:\n  auto/judge: providerA\n")

	var out, errOut bytes.Buffer
	code := runActRepoint([]string{
		"--target-path", path, "--target-id", "egress.auto/judge",
		"--value", "providerB", "--work", "work_teachgrant", "--confirm", "--home", home,
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("expected non-zero exit when no grant allows the write; stdout=%q", out.String())
	}

	assertTeachingRefusal(t, errOut.String(), "policy_decision",
		// The capability is what a grant must be scoped to, so naming it is the
		// difference between an actionable refusal and a description of one.
		actDefaultCapability,
		"compound policy import-standing-grants",
		// The re-run command must carry the caller's own work id, not a placeholder,
		// when the caller supplied one.
		"--work work_teachgrant",
	)
	// The dry-run escape hatch belongs in the ladder: it is the safe way to see the
	// decision without asking for the write.
	if !strings.Contains(errOut.String(), "without --confirm") {
		t.Errorf("refusal should name the dry-run path as the zero-risk next step, got:\n%s", errOut.String())
	}
}

// The dry-run default (no --confirm) writes nothing and reports the policy outcome.
func TestActRepointDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := actScratch(t, home, "egress:\n  auto/judge: providerA\n")
	before, _ := os.ReadFile(path)

	var out, errOut bytes.Buffer
	code := runActRepoint([]string{
		"--target-path", path, "--target-id", "egress.auto/judge", "--value", "providerB", "--home", home,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("dry-run should succeed, got code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("dry-run output should say so, got %q", out.String())
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run must not write to the target")
	}
}

// --confirm requires --work (a governed act belongs to a work).
func TestActRepointConfirmRequiresWork(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := actScratch(t, home, "egress:\n  auto/judge: providerA\n")
	var out, errOut bytes.Buffer
	code := runActRepoint([]string{
		"--target-path", path, "--target-id", "egress.auto/judge", "--value", "providerB", "--confirm", "--home", home,
	}, &out, &errOut)
	if code != 2 {
		t.Fatalf("expected usage exit 2 without --work, got %d", code)
	}
}

// FALSIFIER (allow path is load-bearing): with an imported standing grant scoped to the capability,
// class, reversibility, and target, --confirm performs the governed write, flips the value, and
// records a resolvable receipt to the canonical event store.
func TestActRepointWritesWithGrantAndRecordsReceipt(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := actScratch(t, home, "egress:\n  auto/judge: providerA\n")

	grantSrc := filepath.Join(home, "grants.yaml")
	raw := "generated_at: \"2026-07-01T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-act-repoint\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_act_repoint\n" +
		"    capabilities: [\"" + actDefaultCapability + "\"]\n" +
		"    target_scope: \"" + path + "\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: reversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSrc, []byte(raw), 0o644); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(home, grantSrc, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}

	var out, errOut bytes.Buffer
	code := runActRepoint([]string{
		"--target-path", path, "--target-id", "egress.auto/judge",
		"--value", "providerB", "--work", "work_grant", "--confirm", "--home", home, "--json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("granted write should succeed, got code=%d stderr=%q", code, errOut.String())
	}

	// The value flipped on disk.
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "providerB") {
		t.Fatalf("granted write did not apply: %q", string(after))
	}
	// The receipt id in the output resolves to a real envelope file in the canonical event store.
	var result struct {
		ReceiptID string `json:"receipt_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parse act output json: %v (%q)", err, out.String())
	}
	recID := result.ReceiptID
	if recID == "" {
		t.Fatalf("expected a receipt_id in output: %q", out.String())
	}
	if !testEventExists(t, home, recID) {
		t.Fatalf("receipt %s must resolve in the canonical event store", recID)
	}
}
