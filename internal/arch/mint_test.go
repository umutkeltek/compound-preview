package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mintFails returns the Fail-severity mint-ratchet messages in a finding slice.
func mintFails(findings []Finding) []string {
	var msgs []string
	for _, f := range findings {
		if f.Check == "mint-ratchet" && f.Severity == Fail {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
}

func anyContains(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// FALSIFIER (c): the mint ratchet is GREEN against the real repo — every detected organ, registry
// surface, and status note carries a two-class home or a dated grandfather grant. If someone mints a
// new one without a home, THIS fails in CI (that is the ratchet's teeth). It must also emit zero
// STALE warns, which locks the grandfather sets to exactly the current tree — no drift wallpaper.
func TestMintRatchetGreenAgainstRealRepo(t *testing.T) {
	findings := checkMintTwoClassHome(repoRoot)
	for _, f := range findings {
		if f.Severity == Fail {
			t.Errorf("mint ratchet FAIL [%s] %s", f.Check, f.Message)
		}
		if f.Severity == Warn {
			t.Errorf("mint ratchet STALE/warn (grandfather drifted from tree) [%s] %s", f.Check, f.Message)
		}
	}
	var info bool
	for _, f := range findings {
		if f.Severity == Info && strings.Contains(f.Message, "two-class home") {
			info = true
		}
	}
	if !info {
		t.Error("expected the mint-ratchet green Info line")
	}
}

// FALSIFIER (c) belt-and-suspenders: the whole Doctor stays green with the mint ratchet wired in.
func TestDoctorStaysGreenWithMintRatchet(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	for _, f := range r.Findings {
		if f.Check == "mint-ratchet" && f.Severity == Fail {
			t.Errorf("doctor mint-ratchet FAIL: %s", f.Message)
		}
	}
}

// FALSIFIER (a)+(b), PURE (no fs): a synthetic unclassified organ FAILS the ratchet naming it;
// giving it a two-class home (either class) turns it green. Real organs stay grandfathered-green.
func TestMintRatchetPureOrganFalsifier(t *testing.T) {
	const synthetic = "scratchorgan"
	if grandfatheredOrgans[synthetic] {
		t.Fatalf("test precondition: %q must NOT be grandfathered", synthetic)
	}

	// (a) unclassified, no home -> FAIL naming it.
	red := mintFindings("organ", []string{synthetic}, map[string]MintHome{}, grandfatheredOrgans)
	fails := mintFails(red)
	if len(fails) != 1 || !anyContains(fails, synthetic) {
		t.Fatalf("expected exactly one mint Fail naming %q, got %+v", synthetic, red)
	}
	if !anyContains(fails, "two-class law") {
		t.Errorf("mint failure must cite the two-class law, got %q", fails[0])
	}

	// (b1) classified as an enforcement point -> GREEN.
	enf := mintFindings("organ", []string{synthetic}, map[string]MintHome{
		"organ:" + synthetic: {Class: MintEnforcement},
	}, grandfatheredOrgans)
	if got := mintFails(enf); len(got) != 0 {
		t.Errorf("enforcement-classified organ must pass, got %+v", got)
	}

	// (b2) classified as evidence with a plain sentence + named consumer -> GREEN.
	ev := mintFindings("organ", []string{synthetic}, map[string]MintHome{
		"organ:" + synthetic: {Class: MintEvidence, Purpose: "read-only scratch projection over reviews", Consumer: "orient"},
	}, grandfatheredOrgans)
	if got := mintFails(ev); len(got) != 0 {
		t.Errorf("well-formed evidence organ must pass, got %+v", got)
	}

	// A real grandfathered organ stays green with an empty home registry.
	green := mintFindings("organ", []string{"arch", "mem"}, map[string]MintHome{}, grandfatheredOrgans)
	if got := mintFails(green); len(got) != 0 {
		t.Errorf("grandfathered organs must pass, got %+v", got)
	}
}

// Evidence with a missing consumer OR missing purpose does NOT satisfy the mint gate — evidence that
// names no consumer decays, so it must not pass as a home.
func TestMintRatchetEvidenceNeedsPurposeAndConsumer(t *testing.T) {
	cases := []struct {
		name string
		home MintHome
	}{
		{"no consumer", MintHome{Class: MintEvidence, Purpose: "some purpose"}},
		{"no purpose", MintHome{Class: MintEvidence, Consumer: "orient"}},
		{"neither", MintHome{Class: MintEvidence}},
		{"unknown class", MintHome{Class: MintClass("meter")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.home.Valid() {
				t.Fatalf("home %+v must be invalid", tc.home)
			}
			got := mintFindings("registry", []string{"scratchreg"}, map[string]MintHome{
				"registry:scratchreg": tc.home,
			}, map[string]bool{})
			if len(mintFails(got)) != 1 {
				t.Errorf("incomplete evidence home must FAIL, got %+v", got)
			}
		})
	}
	// A complete enforcement home is always valid.
	if !(MintHome{Class: MintEnforcement}).Valid() {
		t.Error("enforcement home must be valid without prose")
	}
}

// FALSIFIER (a) END-TO-END: planting a real internal/scratchorgan DIRECTORY makes the doctor check
// FAIL naming it (proves the deterministic dir-listing detector is wired to the ratchet), and
// registering a two-class home turns it green (red -> green at the check level, not just the pure core).
func TestMintRatchetDetectsPlantedOrganDir(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "internal", "scratchorgan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs", "architecture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "architecture", "status.md"), []byte("# status\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The detector really sees the new dir.
	organs, err := detectOrgans(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(organs) != 1 || organs[0] != "scratchorgan" {
		t.Fatalf("detectOrgans should find the planted dir, got %v", organs)
	}

	// RED: doctor check fails naming it.
	if got := mintFails(checkMintTwoClassHome(repo)); !anyContains(got, "scratchorgan") {
		t.Fatalf("planted organ must FAIL the mint ratchet naming it, got %+v", got)
	}

	// GREEN: registering a two-class home clears the failure (restore the global after).
	mintHomes["organ:scratchorgan"] = MintHome{Class: MintEnforcement}
	defer delete(mintHomes, "organ:scratchorgan")
	if got := mintFails(checkMintTwoClassHome(repo)); anyContains(got, "scratchorgan") {
		t.Fatalf("registered organ must pass the mint ratchet, still failing: %+v", got)
	}
}

// The status-note and registry detectors are real: a planted marker / URI is detected and, absent a
// home, FAILS. This proves the regex detectors are wired, not just the organ dir-listing.
func TestMintRatchetDetectsPlantedTermAndStatusNote(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs", "architecture"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A new status note marker + a real doc line that is NOT a marker (must not false-positive).
	status := "# status\n\nScratchgate status note: a fresh minted note that has no home yet.\n\n" +
		"This ordinary Sentence mentions nothing and must not match.\n"
	if err := os.WriteFile(filepath.Join(repo, "docs", "architecture", "status.md"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Go source file minting a new registry surface.
	src := "package p\n\nconst reg = \"compound://registries/scratch-registry\"\n"
	if err := os.WriteFile(filepath.Join(repo, "surface.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	notes, err := detectStatusNotes(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "Scratchgate status note" {
		t.Fatalf("detectStatusNotes should find exactly the planted marker, got %v", notes)
	}

	regs, err := detectRegistrySurfaces(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || regs[0] != "scratch-registry" {
		t.Fatalf("detectRegistrySurfaces should find exactly the planted surface, got %v", regs)
	}

	got := mintFails(checkMintTwoClassHome(repo))
	if !anyContains(got, "Scratchgate status note") {
		t.Errorf("planted status note must FAIL the ratchet, got %+v", got)
	}
	if !anyContains(got, "scratch-registry") {
		t.Errorf("planted registry surface must FAIL the ratchet, got %+v", got)
	}
}

// A _test.go registry surface is a fixture, not a minted system surface: it must NOT be detected.
func TestMintRatchetIgnoresTestFileRegistries(t *testing.T) {
	repo := t.TempDir()
	src := "package p\n\nconst reg = \"compound://registries/fixture-only\"\n"
	if err := os.WriteFile(filepath.Join(repo, "surface_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	regs, err := detectRegistrySurfaces(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("registry surfaces in _test.go must be ignored, got %v", regs)
	}
}
