package arch

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseBuildSHA(t *testing.T) {
	cases := map[string]string{
		"compound 0.1.0 (build a1b2c3d)\n": "a1b2c3d",
		"compound 0.1.0 (build dev)\n":     "dev",
		"compound 0.1.0\n":                 "", // pre-embed binary: no build segment at all
		"garbage output":                   "",
	}
	for in, want := range cases {
		if got := ParseBuildSHA(in); got != want {
			t.Errorf("ParseBuildSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyBuildFreshness(t *testing.T) {
	cases := []struct {
		installed, head string
		want            BuildFreshnessStatus
	}{
		{"a1b2c3d", "a1b2c3d", BuildFresh},
		{"a1b2c3d", "a1b2c3dxx", BuildFresh}, // installed is a prefix of a longer HEAD sha
		{"a1b2c3d", "b2c3d4e", BuildStale},
		{"dev", "a1b2c3d", BuildUnknown},
		{"", "a1b2c3d", BuildUnknown},
		{"a1b2c3d", "", BuildUnknown},
	}
	for _, c := range cases {
		if got := classifyBuildFreshness(c.installed, c.head); got != c.want {
			t.Errorf("classifyBuildFreshness(%q,%q) = %q, want %q", c.installed, c.head, got, c.want)
		}
	}
}

// TestCheckBuildFreshnessAtMissing proves the reporter classifies a never-installed
// binary as missing, INFO (never Fail), with no real installed binary required — a
// nonexistent temp path stands in for the fixture.
func TestCheckBuildFreshnessAtMissing(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "compound-does-not-exist")
	called := false
	findings := checkBuildFreshnessAt(binaryPath,
		func() (string, error) { called = true; return "a1b2c3d", nil },
		func(string) (string, error) { called = true; return "", nil },
	)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one", findings)
	}
	f := findings[0]
	if f.Severity == Fail {
		t.Fatalf("missing binary must never Fail the battery, got %+v", f)
	}
	if f.Severity != Info || !strings.Contains(f.Message, string(BuildMissing)) {
		t.Fatalf("finding = %+v, want Info mentioning %q", f, BuildMissing)
	}
	if called {
		t.Fatal("headFn/versionFn should not run when the binary is missing")
	}
}

// TestCheckBuildFreshnessAtFresh/Stale prove the fresh/stale split using a stub
// binaryPath (a fixture file that merely needs to exist for os.Stat — its content is
// never read) and injected headFn/versionFn, per the falsifier: no real installed
// compound binary required.
func TestCheckBuildFreshnessAtFresh(t *testing.T) {
	binaryPath := fakeBinary(t)
	findings := checkBuildFreshnessAt(binaryPath,
		func() (string, error) { return "a1b2c3d", nil },
		func(string) (string, error) { return "compound 0.1.0 (build a1b2c3d)\n", nil },
	)
	f := requireOneFinding(t, findings)
	if f.Severity == Fail {
		t.Fatalf("fresh must never Fail, got %+v", f)
	}
	if !strings.Contains(f.Message, string(BuildFresh)) {
		t.Fatalf("finding = %+v, want it to report %q", f, BuildFresh)
	}
}

func TestCheckBuildFreshnessAtStale(t *testing.T) {
	binaryPath := fakeBinary(t)
	findings := checkBuildFreshnessAt(binaryPath,
		func() (string, error) { return "deadbee", nil },
		func(string) (string, error) { return "compound 0.1.0 (build a1b2c3d)\n", nil },
	)
	f := requireOneFinding(t, findings)
	if f.Severity == Fail {
		t.Fatalf("stale must never Fail the proof-check battery (landing always outdates the installed binary), got %+v", f)
	}
	if f.Severity != Warn || !strings.Contains(f.Message, string(BuildStale)) {
		t.Fatalf("finding = %+v, want Warn mentioning %q", f, BuildStale)
	}
}

func TestCheckBuildFreshnessAtUnknownWhenSHAUnparseable(t *testing.T) {
	binaryPath := fakeBinary(t)
	findings := checkBuildFreshnessAt(binaryPath,
		func() (string, error) { return "a1b2c3d", nil },
		func(string) (string, error) { return "compound 0.1.0\n", nil }, // pre-embed binary
	)
	f := requireOneFinding(t, findings)
	if f.Severity == Fail {
		t.Fatalf("unparseable sha must never Fail, got %+v", f)
	}
	if !strings.Contains(f.Message, string(BuildUnknown)) {
		t.Fatalf("finding = %+v, want it to report %q", f, BuildUnknown)
	}
}

func TestCheckBuildFreshnessAtVersionCommandErrors(t *testing.T) {
	binaryPath := fakeBinary(t)
	findings := checkBuildFreshnessAt(binaryPath,
		func() (string, error) { return "a1b2c3d", nil },
		func(string) (string, error) { return "", errors.New("boom") },
	)
	f := requireOneFinding(t, findings)
	if f.Severity == Fail {
		t.Fatalf("a version-command error must never Fail, got %+v", f)
	}
	if f.Severity != Warn {
		t.Fatalf("finding = %+v, want Warn", f)
	}
}

func fakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compound")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireOneFinding(t *testing.T, findings []Finding) Finding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one", findings)
	}
	return findings[0]
}

// TestCheckBuildFreshnessContractNeverFails proves the real (impure) wiring the doctor
// calls also never returns Fail, for EVERY install state a machine can be in.
//
// The installation is a DISPOSABLE FIXTURE this test owns: GOBIN points at a temp dir
// holding a synthetic compound binary, so installedBinaryPath() resolves to that
// synthetic binary and runInstalledVersion executes it — never the operator's real
// installation. The repo root here is this checkout, so gitHeadSHA resolves for real
// (git may inspect the requested source repo; it never reads live runtime state). The
// synthetic binary is absent, fresh, stale or pre-embed depending on the case; every one
// of those is a reported state, not a failure.
func TestCheckBuildFreshnessContractNeverFails(t *testing.T) {
	head, err := gitHeadSHA(repoRoot)
	if err != nil || strings.TrimSpace(head) == "" {
		t.Fatalf("git HEAD of the requested source repo must resolve for the freshness fixture: %v", err)
	}
	head = strings.TrimSpace(head)
	// A build SHA guaranteed to differ from HEAD (same length, first character flipped),
	// so the synthetic installation is classified stale rather than accidentally fresh.
	stale := "0" + head[1:]
	if head[0] == '0' {
		stale = "1" + head[1:]
	}
	for _, tc := range []struct {
		name       string
		install    bool
		versionOut string
		wantStatus BuildFreshnessStatus
	}{
		{"not installed", false, "", BuildMissing},
		{"fresh", true, "compound 0.1.0 (build " + head + ")", BuildFresh},
		{"stale", true, "compound 0.1.0 (build " + stale + ")", BuildStale},
		{"no embedded build sha", true, "compound 0.1.0", BuildUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gobin := t.TempDir()
			t.Setenv("GOBIN", gobin)
			if tc.install {
				script := "#!/bin/sh\nprintf '%s\\n' '" + tc.versionOut + "'\n"
				if err := os.WriteFile(filepath.Join(gobin, "compound"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			findings := checkBuildFreshnessContract(repoRoot)
			f := requireOneFinding(t, findings)
			if f.Severity == Fail {
				t.Fatalf("build-freshness must never Fail the doctor, got %+v", f)
			}
			if f.Check != "build-freshness" {
				t.Fatalf("finding.Check = %q, want build-freshness", f.Check)
			}
			if !strings.Contains(f.Message, string(tc.wantStatus)) {
				t.Fatalf("finding = %+v, want it to report status=%s", f, tc.wantStatus)
			}
		})
	}
}

func TestClassifySidecarFreshness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []byte
		repo      []byte
		want      SidecarFreshnessStatus
	}{
		{"identical bytes are fresh", []byte("#!/bin/sh\nexit 0\n"), []byte("#!/bin/sh\nexit 0\n"), SidecarFresh},
		{"not installed is missing", nil, []byte("#!/bin/sh\n"), SidecarMissing},
		{"unreadable repo copy is unknown", []byte("#!/bin/sh\n"), nil, SidecarUnknown},
		{"absent on both sides is missing", nil, nil, SidecarMissing},
		// The exact 2026-08-30 drift: one extra flag line in the installed copy.
		{"one added flag line is stale",
			[]byte("promote-driver \\\n  --confirm \\\n  --registry x \\\n"),
			[]byte("promote-driver \\\n  --confirm \\\n"),
			SidecarStale},
		{"whitespace-only difference is stale", []byte("a\tb\n"), []byte("a b\n"), SidecarStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySidecarFreshness(tc.installed, tc.repo); got != tc.want {
				t.Fatalf("classifySidecarFreshness = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckSidecarFreshnessAtWarnsOnlyWhenStale(t *testing.T) {
	repo := []byte("#!/bin/sh\nexit 0\n")
	for _, tc := range []struct {
		name         string
		installed    []byte
		wantSeverity Severity
		wantSubstr   string
	}{
		{"fresh", repo, Info, "matches the repo copy"},
		{"stale", []byte("#!/bin/sh\nexit 1\n"), Warn, "unversioned bytes"},
		{"missing", nil, Info, "not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := checkSidecarFreshnessAt([]string{"x.sh"},
				func(string) ([]byte, error) {
					if tc.installed == nil {
						return nil, os.ErrNotExist
					}
					return tc.installed, nil
				},
				func(string) ([]byte, error) { return repo, nil },
			)
			if len(findings) != 1 {
				t.Fatalf("want exactly 1 finding, got %d", len(findings))
			}
			if findings[0].Severity != tc.wantSeverity {
				t.Fatalf("severity = %v, want %v (%s)", findings[0].Severity, tc.wantSeverity, findings[0].Message)
			}
			if !strings.Contains(findings[0].Message, tc.wantSubstr) {
				t.Fatalf("message %q missing %q", findings[0].Message, tc.wantSubstr)
			}
			if findings[0].Check != "sidecar-freshness" {
				t.Fatalf("check = %q", findings[0].Check)
			}
		})
	}
}

// A CI clone has no installed sidecar, and architecture doctor --strict is a CI-portable
// proof gate: this check must never be able to red it.
func TestCheckSidecarFreshnessNeverFails(t *testing.T) {
	for _, installed := range [][]byte{nil, []byte("drifted")} {
		findings := checkSidecarFreshnessAt([]string{"x.sh"},
			func(string) ([]byte, error) {
				if installed == nil {
					return nil, os.ErrNotExist
				}
				return installed, nil
			},
			func(string) ([]byte, error) { return []byte("repo"), nil },
		)
		for _, f := range findings {
			if f.Severity == Fail {
				t.Fatalf("sidecar-freshness produced a Fail finding: %s", f.Message)
			}
		}
	}
}

// The map is the contract: every sidecar install.sh copies must be covered, and every
// entry must name a file that actually exists in the repo. A script added to install.sh
// without an entry here is invisible to the drift check.
func TestInstalledSidecarScriptsCoverInstallScript(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range installedSidecarScripts {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(script.RepoRel))); err != nil {
			t.Fatalf("installedSidecarScripts[%q] points at missing repo file %s: %v", name, script.RepoRel, err)
		}
	}
	installSH, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Only `cp` out of the repo can drift; a script install.sh EXECUTES from the repo
	// (sync-ceremony-overlays.sh) always runs the committed bytes.
	copied := regexp.MustCompile(`cp "\$repo_root/(scripts/[A-Za-z0-9._-]+)"`).FindAllStringSubmatch(string(installSH), -1)
	if len(copied) == 0 {
		t.Fatal("no `cp \"$repo_root/scripts/...\"` found in install.sh — this test's premise broke, not the map")
	}
	for _, match := range copied {
		covered := false
		for _, known := range installedSidecarScripts {
			if known.RepoRel == match[1] {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("install.sh copies %s but installedSidecarScripts does not cover it — its drift would go unnoticed", match[1])
		}
	}
}
