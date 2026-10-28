package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// incidentStubScript is the SHAPE of the artifact that once occupied the install path: it answers
// `version` with a fixed identity string and exits 0 for everything else. Both halves are what
// made it invisible — the installer's only probe was a version-string compare, and every
// lifecycle hook measured success by exit status.
const incidentStubScript = `#!/bin/sh
[ "$1" = "version" ] && echo "compound 0.1.0 (build abc1234)" && exit 0
exit 0
`

func writeStubScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// TestVerifyInstalledArtifactRejectsFourLineShellStub is the regression the guard exists for: the
// artifact answers the old probe perfectly, so only a check that looks past the version string
// can refuse it.
func TestVerifyInstalledArtifactRejectsFourLineShellStub(t *testing.T) {
	stub := writeStubScript(t, t.TempDir(), "compound", incidentStubScript)

	// Sanity: the stub really does answer the probe the installer used to perform, so this test
	// cannot pass for the trivial reason that the stub is broken.
	out, err := exec.Command(stub, "version").Output()
	if err != nil || !strings.Contains(string(out), "compound 0.1.0 (build ") {
		t.Fatalf("stub did not reproduce the incident: out=%q err=%v", out, err)
	}
	if err := exec.Command(stub, "home", "doctor").Run(); err != nil {
		t.Fatalf("stub must exit 0 for every other subcommand, like the real incident: %v", err)
	}

	_, verifyErr := VerifyInstalledArtifact(stub, "abc1234")
	if verifyErr == nil {
		t.Fatal("verification accepted a shell-script stub — this is the exact artifact that fail-opened every hook")
	}
	if !strings.Contains(verifyErr.Error(), "byte floor") {
		t.Fatalf("rejection must name the property that failed, got: %v", verifyErr)
	}
}

// TestVerifyInstalledArtifactRejectsShellStubThatClearsTheSizeFloor stops the size floor from
// being the only thing standing between the installer and a script: padding is free.
func TestVerifyInstalledArtifactRejectsShellStubThatClearsTheSizeFloor(t *testing.T) {
	padded := incidentStubScript + "# " + strings.Repeat("x", 2*MinInstalledBinaryBytes) + "\n"
	stub := writeStubScript(t, t.TempDir(), "compound", padded)

	info, err := os.Stat(stub)
	if err != nil {
		t.Fatalf("stat padded stub: %v", err)
	}
	if info.Size() < MinInstalledBinaryBytes {
		t.Fatalf("padded stub is %d bytes, must clear the %d-byte floor to exercise the magic check", info.Size(), int64(MinInstalledBinaryBytes))
	}

	_, verifyErr := VerifyInstalledArtifact(stub, "abc1234")
	if verifyErr == nil {
		t.Fatal("verification accepted a padded shell script as an installed binary")
	}
	if !strings.Contains(verifyErr.Error(), "not a native executable") {
		t.Fatalf("rejection must name the magic-byte failure, got: %v", verifyErr)
	}
}

// TestVerifyInstalledArtifactRejectsSymlinkedArtifact: a symlink passes every content check by
// describing its target, while the install path itself stays under someone else's control.
func TestVerifyInstalledArtifactRejectsSymlinkedArtifact(t *testing.T) {
	dir := t.TempDir()
	target := writeStubScript(t, dir, "target", incidentStubScript)
	link := filepath.Join(dir, "compound")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := VerifyInstalledArtifact(link, "abc1234")
	if err == nil {
		t.Fatal("verification accepted a symlinked install path")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("rejection must name the symlink, got: %v", err)
	}
}

// buildStubBinary compiles a real native executable from src. It gives the behavioural probe
// something to fail on that is NOT a script: a genuine Mach-O/ELF image that clears every static
// gate and still cannot be the compound binary.
func buildStubBinary(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module installstub\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write stub go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write stub main.go: %v", err)
	}
	out := filepath.Join(dir, "compound")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub binary: %v\n%s", err, combined)
	}
	return out
}

// TestVerifyInstalledArtifactRejectsNativeBinaryThatPrecomputesTheVersionReport is the test that
// keeps the behavioural probe honest. This stub is a real native binary of real size that emits a
// well-formed report with the CORRECT schema version and the CORRECT build SHA — everything a
// fixture author can know ahead of time. It fails only because the challenge answer does not
// exist until the caller mints the nonce.
func TestVerifyInstalledArtifactRejectsNativeBinaryThatPrecomputesTheVersionReport(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a stub binary: skipped in -short mode")
	}
	src := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(%q)
		return
	}
	os.Exit(0)
}
`, `{"schema_version":"`+VersionReportSchema+`","version":"0.1.0","build_sha":"abc1234","challenge":"precomputed","challenge_digest":"precomputed"}`)
	stub := buildStubBinary(t, src)

	report, err := VerifyInstalledArtifact(stub, "abc1234")
	if err == nil {
		t.Fatal("verification accepted a native binary that only replays a canned version report")
	}
	if report.Format == "" {
		t.Fatalf("stub was rejected before the behavioural probe (static gates fired), got: %v", err)
	}
	if !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("rejection must name the challenge, got: %v", err)
	}
}

// TestVerifyInstalledArtifactRejectsBuildSHAMismatch: an artifact from an older build answers
// every challenge correctly and is still the wrong binary to install.
func TestVerifyInstalledArtifactRejectsBuildSHAMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	binary := buildCompoundBinary(t, "aaaa111")

	_, err := VerifyInstalledArtifact(binary, "bbbb222")
	if err == nil {
		t.Fatal("verification accepted an artifact built from a different revision")
	}
	if !strings.Contains(err.Error(), "different build") {
		t.Fatalf("rejection must name the build mismatch, got: %v", err)
	}
}

// buildCompoundBinary builds the real CLI with the same ldflags install.sh uses.
func buildCompoundBinary(t *testing.T, buildSHA string) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	out := filepath.Join(t.TempDir(), "compound")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.buildSHA="+buildSHA, "-o", out, "./cmd/compound")
	cmd.Dir = repoRoot
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build compound: %v\n%s", err, combined)
	}
	return out
}

// TestVerifyInstalledArtifactAcceptsGenuineBuild is the other half of the guard: a check that
// rejects everything is not a check, it is an outage.
func TestVerifyInstalledArtifactAcceptsGenuineBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	binary := buildCompoundBinary(t, "abc1234")

	report, err := VerifyInstalledArtifact(binary, "abc1234")
	if err != nil {
		t.Fatalf("verification rejected a genuine build: %v", err)
	}
	if report.Format != "mach-o" && report.Format != "elf" && report.Format != "mach-o-universal" {
		t.Fatalf("genuine build reported format %q", report.Format)
	}
	if report.BuildSHA != "abc1234" || strings.TrimSpace(report.Version) == "" {
		t.Fatalf("genuine build report = %+v", report)
	}
	if report.SizeBytes < MinInstalledBinaryBytes {
		t.Fatalf("genuine build is %d bytes, below the floor", report.SizeBytes)
	}
}

// TestVersionChallengeDigestBindsNonceAndIdentity: if the digest ignored any of its three inputs,
// a response recorded from one run (or one build) would verify another.
func TestVersionChallengeDigestBindsNonceAndIdentity(t *testing.T) {
	base := VersionChallengeDigest("nonce-a", "0.1.0", "abc1234")
	for _, other := range []string{
		VersionChallengeDigest("nonce-b", "0.1.0", "abc1234"),
		VersionChallengeDigest("nonce-a", "0.2.0", "abc1234"),
		VersionChallengeDigest("nonce-a", "0.1.0", "def5678"),
	} {
		if other == base {
			t.Fatal("challenge digest ignores one of nonce/version/build-sha, so a response replays across runs")
		}
	}
	if VersionChallengeDigest("nonce-a", "0.1.0", "abc1234") != base {
		t.Fatal("challenge digest is not deterministic")
	}
}

// TestNormalizeBuildSHATreatsEmptyAsDev keeps a clone with no git history under a real equality
// check rather than a skipped one.
func TestNormalizeBuildSHATreatsEmptyAsDev(t *testing.T) {
	if NormalizeBuildSHA("  ") != "dev" || NormalizeBuildSHA("") != "dev" {
		t.Fatal("empty build SHA must normalize to the same dev sentinel the CLI reports")
	}
	if NormalizeBuildSHA(" abc1234 ") != "abc1234" {
		t.Fatal("build SHA must be compared trimmed")
	}
}
