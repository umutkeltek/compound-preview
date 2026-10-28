package arch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// installverify.go answers the one question an installer cannot answer by looking at its own
// exit code: is the artifact now sitting at the install path really this repo's compound binary?
//
// It lives beside freshness.go because arch already owns the installed-binary-vs-source contract
// (freshness.go asks "is the installed binary the code we last committed?"); this asks the
// strictly prior question, "is the installed binary a compound binary at all?".
//
// It exists because of a lived fail-open: the install path was occupied by a four-line shell
// script that printed a fixed `compound <version> (build <sha>)` for `version` and exited 0 for
// everything else. Every probe the installer performed — a version string compare, then three
// subcommands whose success was measured by exit status — is exactly what such a stub answers
// correctly, so the install reported success. The lifecycle hooks then shelled out to that same
// path and every policy gate silently passed, because an exit-0 hook is indistinguishable from an
// allow. Silence and success were the same observation.
//
// The two guards below are chosen so that the SHAPE of that stub cannot pass:
//   - a static half (symlink / regular file / exec bit / size floor / native-executable magic)
//     rejects any script, wrapper, or truncated artifact without shelling out to `file`;
//   - a behavioural half whose correct answer does not exist until the probe is issued — the
//     caller mints a fresh nonce per run, so no fixture, canned string, or recorded transcript
//     can contain the response.

// VersionReportSchema names the structured shape `compound version --json` emits. Verifying a
// parsed field is the point: a plain version STRING is precomputable, and comparing one is what
// let the stub through.
const VersionReportSchema = "compound.version_report.v0.1"

// MinInstalledBinaryBytes is the floor a real compound artifact clears by three orders of
// magnitude (a linked Go binary is tens of MiB). It is deliberately not tuned close to the real
// size: this floor exists to reject scripts and truncated writes, not to police build size.
const MinInstalledBinaryBytes = 1 << 20 // 1 MiB

// installProbeTimeout bounds the behavioural probe. A binary that cannot answer `version --json`
// within this budget fails verification rather than hanging the installer — an install that never
// returns is the same operator experience as one that fails, minus the message.
const installProbeTimeout = 10 * time.Second

// VersionReport is the structured answer to `compound version --json`. It is owned here rather
// than in the CLI so the emitter and the verifier cannot drift into agreeing on nothing.
type VersionReport struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	BuildSHA      string `json:"build_sha"`
	// Challenge is echoed back verbatim so a response cannot be replayed from a different probe.
	Challenge string `json:"challenge,omitempty"`
	// ChallengeDigest binds the caller's nonce to the identity this binary actually links.
	ChallengeDigest string `json:"challenge_digest,omitempty"`
}

// VersionChallengeDigest binds a caller-minted nonce to a binary's linked identity. The nonce is
// fresh per install run, so the digest cannot be embedded in a fixture ahead of time; only code
// that can read its own compiled-in version and build SHA can produce it.
func VersionChallengeDigest(challenge, version, buildSHA string) string {
	sum := sha256.Sum256([]byte(challenge + "\n" + version + "\n" + buildSHA))
	return hex.EncodeToString(sum[:])
}

// InstallVerifyReport is what a passing verification observed, so the installer can print
// evidence rather than the word "ok".
type InstallVerifyReport struct {
	Binary    string `json:"binary"`
	SizeBytes int64  `json:"size_bytes"`
	Format    string `json:"format"`
	Version   string `json:"version"`
	BuildSHA  string `json:"build_sha"`
}

// NormalizeBuildSHA maps the empty string to the same "dev" sentinel the CLI reports when it is
// built without ldflags, so a clone with no git history still gets a real equality check instead
// of a skipped one.
func NormalizeBuildSHA(sha string) string {
	if strings.TrimSpace(sha) == "" {
		return "dev"
	}
	return strings.TrimSpace(sha)
}

// VerifyInstalledArtifact proves binaryPath is a native compound executable built from the source
// that produced wantBuildSHA. Every returned error names the specific property that failed, so an
// operator reading one line knows whether they are looking at a script, a symlink, a truncated
// write, a stale binary, or a stub.
func VerifyInstalledArtifact(binaryPath, wantBuildSHA string) (InstallVerifyReport, error) {
	report := InstallVerifyReport{Binary: binaryPath}

	// Lstat, not Stat: Stat follows the link and would happily describe the target, which is how
	// a symlinked artifact passes every size and magic check while the install path itself is
	// controlled by whoever created the link.
	info, err := os.Lstat(binaryPath)
	if err != nil {
		return report, fmt.Errorf("artifact %s is not readable: %w", binaryPath, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return report, fmt.Errorf("artifact %s is a symlink, not an installed executable", binaryPath)
	case !info.Mode().IsRegular():
		return report, fmt.Errorf("artifact %s is not a regular file (mode %s)", binaryPath, info.Mode())
	case info.Mode()&0o111 == 0:
		return report, fmt.Errorf("artifact %s is not executable (mode %s)", binaryPath, info.Mode())
	}
	report.SizeBytes = info.Size()
	if info.Size() < MinInstalledBinaryBytes {
		return report, fmt.Errorf("artifact %s is %d bytes, below the %d-byte floor for a linked binary — this is the size of a script or a truncated write",
			binaryPath, info.Size(), int64(MinInstalledBinaryBytes))
	}

	header, err := readFileHeader(binaryPath, 4)
	if err != nil {
		return report, fmt.Errorf("artifact %s header unreadable: %w", binaryPath, err)
	}
	format, native := nativeExecutableFormat(header)
	if !native {
		return report, fmt.Errorf("artifact %s is not a native executable: leading bytes %s match no Mach-O or ELF image (a shell script starts with `#!`)",
			binaryPath, hex.EncodeToString(header))
	}
	report.Format = format

	probe, err := probeVersionReport(binaryPath)
	if err != nil {
		return report, err
	}
	report.Version = probe.Report.Version
	report.BuildSHA = probe.Report.BuildSHA
	if err := checkVersionReport(binaryPath, probe, wantBuildSHA); err != nil {
		return report, err
	}
	return report, nil
}

// checkVersionReport is the pure half of the behavioural probe: given a parsed response and a
// nonce the caller minted, decide whether only a real compound binary could have produced it.
func checkVersionReport(binaryPath string, probe probedVersion, wantBuildSHA string) error {
	got := probe.Report
	if got.SchemaVersion != VersionReportSchema {
		return fmt.Errorf("artifact %s answered `version --json` with schema_version %q, want %q",
			binaryPath, got.SchemaVersion, VersionReportSchema)
	}
	if strings.TrimSpace(got.Version) == "" {
		return fmt.Errorf("artifact %s answered `version --json` with an empty version", binaryPath)
	}
	want := NormalizeBuildSHA(wantBuildSHA)
	if NormalizeBuildSHA(got.BuildSHA) != want {
		return fmt.Errorf("artifact %s reports build %q, but the source just compiled is %q — the install path holds a different build",
			binaryPath, NormalizeBuildSHA(got.BuildSHA), want)
	}
	if got.Challenge != probe.Challenge {
		return fmt.Errorf("artifact %s echoed challenge %q, want the one this run minted (%q)",
			binaryPath, got.Challenge, probe.Challenge)
	}
	wantDigest := VersionChallengeDigest(probe.Challenge, got.Version, want)
	if got.ChallengeDigest != wantDigest {
		return fmt.Errorf("artifact %s failed the install challenge: digest %q, want %q — it can print a version but cannot compute an answer it has never seen",
			binaryPath, got.ChallengeDigest, wantDigest)
	}
	return nil
}

// probedVersion pairs a response with the nonce it was asked for, so the two can never be
// compared against different runs.
type probedVersion struct {
	Challenge string
	Report    VersionReport
}

// probeVersionReport runs the artifact under test. COMPOUND_HOME is redirected to a throwaway
// directory: verification must not be able to write operator state, and a probe that mutates the
// thing it inspects is not a probe.
func probeVersionReport(binaryPath string) (probedVersion, error) {
	challenge, err := mintChallenge()
	if err != nil {
		return probedVersion{}, fmt.Errorf("mint install challenge: %w", err)
	}
	sandbox, err := os.MkdirTemp("", "compound-install-verify-")
	if err != nil {
		return probedVersion{}, fmt.Errorf("install probe sandbox: %w", err)
	}
	defer os.RemoveAll(sandbox)

	ctx, cancel := context.WithTimeout(context.Background(), installProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "version", "--json", "--challenge", challenge)
	cmd.Dir = sandbox
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+sandbox)
	out, err := cmd.Output()
	if err != nil {
		return probedVersion{}, fmt.Errorf("artifact %s could not answer `version --json`: %w", binaryPath, err)
	}

	var report VersionReport
	if err := json.Unmarshal(out, &report); err != nil {
		return probedVersion{}, fmt.Errorf("artifact %s answered `version --json` with output that is not a version report: %w", binaryPath, err)
	}
	return probedVersion{Challenge: challenge, Report: report}, nil
}

func mintChallenge() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func readFileHeader(path string, n int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, n)
	read, err := file.Read(header)
	if err != nil && read == 0 {
		return nil, err
	}
	return header[:read], nil
}

// nativeExecutableFormat classifies leading bytes as a native executable image. Reading magic
// bytes directly, rather than shelling out to `file`, keeps the check dependency-free and means
// the installer cannot be fooled by a `file` shim on PATH.
//
// Mach-O magic is listed in both byte orders because the same image is written little-endian on
// the hosts that run it; the universal ("fat") archive magic overlaps with other formats, which
// is harmless here because the behavioural probe still has to pass afterwards.
func nativeExecutableFormat(header []byte) (string, bool) {
	if len(header) < 4 {
		return "", false
	}
	if string(header[:4]) == "\x7fELF" {
		return "elf", true
	}
	switch binary.BigEndian.Uint32(header[:4]) {
	case 0xFEEDFACE, 0xFEEDFACF, 0xCEFAEDFE, 0xCFFAEDFE:
		return "mach-o", true
	case 0xCAFEBABE, 0xBEBAFECA:
		return "mach-o-universal", true
	}
	return "", false
}
