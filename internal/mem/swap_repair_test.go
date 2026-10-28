package mem

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	routecodec "compound/internal/routeevidence/codec"
)

func swapRow(t *testing.T, id string) []byte {
	t.Helper()
	ev := routecodec.Evidence{
		SchemaVersion:  "compound.execution_route_evidence.v0.1",
		ID:             id,
		CreatedAt:      "2026-08-24T10:00:00Z",
		RequestID:      "req_" + id,
		ClientProfile:  "test-profile",
		Protocol:       "test-protocol",
		SelectedTarget: routecodec.Target{Provider: "test", Model: "test-model", Protocol: "test-proto"},
		Status:         "selected",
	}
	row, err := routecodec.Encode(ev)
	if err != nil {
		t.Fatal(err)
	}
	return append(row, '\n')
}

// swapName builds a leaf the parser accepts: PID.preimage12.digest12.
func swapName(t *testing.T, preimage, postimage []byte) string {
	t.Helper()
	sum := func(b []byte) string { h := sha256Hex(b); return h[:12] }
	return "." + routeEvidenceLedgerFile + ".4242." + sum(preimage) + "." + sum(postimage) + ".swap"
}

func writeSwapFixture(t *testing.T, ledgerDir, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ledgerDir, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRouteEvidenceSwapRepairClassifiesAndQuarantinesResidue covers the three
// classifications and the apply/dry-run contract:
//   - residue_already_applied: swap bytes fully inside current -> quarantined
//   - residue_race_lost: swap from a divergent older base -> quarantined
//   - committable: current is swap minus one canonical row -> reported, untouched
//   - dry-run touches nothing; apply writes quarantine + receipt; re-run is clean.
func TestRouteEvidenceSwapRepairClassifiesAndQuarantinesResidue(t *testing.T) {
	home := t.TempDir()
	ledgerDir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	row1, row2, row3 := swapRow(t, "ev_1"), swapRow(t, "ev_2"), swapRow(t, "ev_3")
	// Current ledger: three rows.
	current := append(append(append([]byte{}, row1...), row2...), row3...)
	if err := os.WriteFile(filepath.Join(ledgerDir, routeEvidenceLedgerFile), current, 0o644); err != nil {
		t.Fatal(err)
	}
	// A: swap whose bytes are a prefix of current (already applied).
	base12 := append(append([]byte{}, row1...), row2...)
	writeSwapFixture(t, ledgerDir, swapName(t, row1, base12), base12)
	// B: swap from a divergent base (row1 then a DIFFERENT row3') — race lost.
	divergent := append(append([]byte{}, row1...), swapRow(t, "ev_2b")...)
	writeSwapFixture(t, ledgerDir, swapName(t, row1, divergent), divergent)
	// C: committable — current minus its last row, plus one canonical row.
	committable := append(append([]byte{}, current...), swapRow(t, "ev_4")...)
	writeSwapFixture(t, ledgerDir, swapName(t, current, committable), committable)

	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)

	// Dry run: reports all three, touches nothing.
	dry, err := RouteEvidenceSwapRepair(home, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Items) != 3 || dry.Quarantined != 0 {
		t.Fatalf("dry run items=%d quarantined=%d, want 3/0", len(dry.Items), dry.Quarantined)
	}
	classes := map[string]string{}
	for _, it := range dry.Items {
		classes[it.Artifact[strings.Index(it.Artifact, ".4242."):]] = it.Classification
		if _, err := os.Stat(filepath.Join(ledgerDir, it.Artifact)); err != nil {
			t.Fatalf("dry run removed or disturbed %s: %v", it.Artifact, err)
		}
	}

	// Apply: two quarantined (residue), committable untouched.
	rep, err := RouteEvidenceSwapRepair(home, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Quarantined != 2 || len(rep.Items) != 3 {
		t.Fatalf("apply quarantined=%d items=%d, want 2/3", rep.Quarantined, len(rep.Items))
	}
	// Receipt log exists with one line per quarantine, sha256 matching.
	logBytes, err := os.ReadFile(filepath.Join(ledgerDir, RouteEvidenceSwapRepairLog))
	if err != nil {
		t.Fatalf("repair receipt log missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("receipt lines=%d, want 2", len(lines))
	}
	seenClasses := map[string]bool{}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("receipt line not JSON: %v", err)
		}
		if rec["action"] != "quarantined" {
			t.Fatalf("receipt action=%v, want quarantined", rec["action"])
		}
		seenClasses[rec["classification"].(string)] = true
		if len(rec["sha256"].(string)) != 64 {
			t.Fatalf("receipt sha256 malformed: %v", rec["sha256"])
		}
	}
	if !seenClasses[SwapClassResidueAlreadyApplied] || !seenClasses[SwapClassResidueRaceLost] {
		t.Fatalf("receipt missing residue classes: %v", seenClasses)
	}
	// The committable swap is untouched and the ledger is byte-identical.
	for _, it := range rep.Items {
		if it.Classification == SwapClassCommittable {
			if it.Action != "reported" {
				t.Fatalf("committable swap action=%s, want reported", it.Action)
			}
			if _, err := os.Stat(filepath.Join(ledgerDir, it.Artifact)); err != nil {
				t.Fatalf("committable swap was disturbed: %v", err)
			}
		}
	}
	after, err := os.ReadFile(filepath.Join(ledgerDir, routeEvidenceLedgerFile))
	if err != nil || !bytes.Equal(after, current) {
		t.Fatalf("ledger mutated by repair: err=%v equal=%v", err, bytes.Equal(after, current))
	}

	// Re-run: only the committable remains; nothing new quarantined.
	rep2, err := RouteEvidenceSwapRepair(home, true, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Quarantined != 0 || len(rep2.Items) != 1 || rep2.Items[0].Classification != SwapClassCommittable {
		t.Fatalf("re-run items=%+v quarantined=%d, want 1 committable / 0", rep2.Items, rep2.Quarantined)
	}
}

// TestRouteEvidenceSwapRepairPreflightsReceiptLogBeforeMutation: when the
// receipt journal cannot be opened, apply must fail BEFORE renaming anything.
// Mutating first and discovering the unwritable log afterward is how an
// artifact ends up quarantined with no durable record of the repair — under
// exactly the disk pressure this subsystem exists to manage.
func TestRouteEvidenceSwapRepairPreflightsReceiptLogBeforeMutation(t *testing.T) {
	home := t.TempDir()
	ledgerDir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	row1, row2 := swapRow(t, "ev_1"), swapRow(t, "ev_2")
	current := append(append([]byte{}, row1...), row2...)
	if err := os.WriteFile(filepath.Join(ledgerDir, routeEvidenceLedgerFile), current, 0o644); err != nil {
		t.Fatal(err)
	}
	// Residue swap (bytes already in current) that apply would quarantine.
	name := swapName(t, row1, current)
	writeSwapFixture(t, ledgerDir, name, current)

	// Make the receipt journal unopenable: a directory where the log goes.
	if err := os.Mkdir(filepath.Join(ledgerDir, RouteEvidenceSwapRepairLog), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)
	_, err := RouteEvidenceSwapRepair(home, true, now)
	if err == nil {
		t.Fatal("apply must fail when the receipt log cannot be opened")
	}
	if !strings.Contains(err.Error(), "repair log preflight") {
		t.Fatalf("error should name the preflight, got: %v", err)
	}
	// The swap must still be in place: no mutation without a durable receipt.
	if _, statErr := os.Stat(filepath.Join(ledgerDir, name)); statErr != nil {
		t.Fatalf("preflight failure must not quarantine %s: %v", name, statErr)
	}
	// And nothing may have landed in quarantine.
	entries, readErr := os.ReadDir(filepath.Join(ledgerDir, RouteEvidenceSwapQuarantineDir))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if readErr == nil && len(entries) > 0 {
		t.Fatalf("quarantine dir must be empty after failed preflight, found %d entries", len(entries))
	}
}

// TestRouteEvidenceSwapRepairCLIRejectsUnknownFlags: a typo must be a usage
// error (exit 2), not a silent dry-run of a mutating command.
func TestRouteEvidenceSwapRepairCLIRejectsUnknownFlags(t *testing.T) {
	home := t.TempDir()
	var out, errBuf bytes.Buffer
	code := RouteEvidenceSwapRepairCLI(home, []string{"--aply"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("unknown flag exit=%d, want 2 (stderr: %s)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "unknown flag") {
		t.Fatalf("stderr should name the unknown flag: %s", errBuf.String())
	}
}

// TestRouteEvidenceSwapRepairCLIAcceptsCallerFlags: the home-layer flags the
// caller resolves (--home <path>, --local, --global) plus the explicit
// --dry-run must all stay accepted.
func TestRouteEvidenceSwapRepairCLIAcceptsCallerFlags(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := RouteEvidenceSwapRepairCLI(home, []string{"--dry-run", "--home", home, "--local", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("accepted flags exit=%d, want 0 (stderr: %s)", code, errBuf.String())
	}
}
