package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOfflineInstallerVerifiesItsOwnBytesBeforePayload(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	f, err := os.OpenFile(installer, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# tampered after publication\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	output, err := runReleaseInstaller(installer, tarball, installDir, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("tampered installer executed successfully:\n%s", output)
	}
	if !strings.Contains(string(output), "installer checksum mismatch") {
		t.Fatalf("tampered installer refusal did not identify its own checksum:\n%s", output)
	}
	assertPathAbsent(t, filepath.Join(installDir, "compound"))
}

func TestOfflineInstallerRequiresChecksumManifestBeforePayload(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	if err := os.Remove(filepath.Join(dist, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	output, err := runReleaseInstaller(installer, tarball, installDir, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer ran without SHA256SUMS:\n%s", output)
	}
	if !strings.Contains(string(output), "SHA256SUMS is required beside install-release.sh") {
		t.Fatalf("missing-checksum refusal was unclear:\n%s", output)
	}
	assertPathAbsent(t, filepath.Join(installDir, "compound"))
}

func TestInstallerHasNoMutableLatestDownloadFallback(t *testing.T) {
	body, err := os.ReadFile("../../scripts/install-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "releases/latest") {
		t.Fatal("installer retains mutable releases/latest download selection")
	}
	for _, required := range []string{
		`COMPOUND_VERSION is required for an exact tagged download`,
		`releases/download/v${requested_version}/`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer lacks exact tagged download binding %q", required)
		}
	}
}

func TestInstallerUsesExplicitPublicRepositoryIdentity(t *testing.T) {
	body, err := os.ReadFile("../../scripts/install-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases/download`).MatchString(text) {
		t.Fatal("installer hardcodes an operator account into the public release origin")
	}
	for _, required := range []string{
		`COMPOUND_RELEASE_REPOSITORY`,
		`https://github.com/${release_repository}/releases/download/v${requested_version}/`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer lacks explicit public repository identity seam %q", required)
		}
	}
}

func TestInstallerSupportsExactlyPublishedPlatformMatrix(t *testing.T) {
	dist, _ := buildInstallerFixture(t)
	for _, test := range []struct {
		name       string
		unameOS    string
		unameArch  string
		macVersion string
		asset      string
		wantAccept bool
	}{
		{name: "darwin arm64 macOS 15 floor", unameOS: "Darwin", unameArch: "arm64", macVersion: "15.0", asset: "darwin_arm64", wantAccept: true},
		{name: "darwin arm64 newer than floor", unameOS: "Darwin", unameArch: "arm64", macVersion: "16.0", asset: "darwin_arm64", wantAccept: true},
		{name: "darwin arm64 macOS 14 below floor", unameOS: "Darwin", unameArch: "arm64", macVersion: "14.7", asset: "darwin_arm64"},
		{name: "amd64 Linux", unameOS: "Linux", unameArch: "x86_64", asset: "linux_amd64", wantAccept: true},
		{name: "Intel macOS", unameOS: "Darwin", unameArch: "x86_64", macVersion: "15.0", asset: "linux_amd64"},
		{name: "arm64 Linux", unameOS: "Linux", unameArch: "aarch64", asset: "linux_amd64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			writeExecutable(t, filepath.Join(fakeBin, "lockf"), "#!/bin/sh\n[ \"$#\" -eq 4 ] && [ \"$1\" = -s ] && [ \"$2\" = -t ] && [ \"$3\" = 0 ] && [ \"$4\" = 9 ]\n")
			writeExecutable(t, filepath.Join(fakeBin, "flock"), "#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = -n ] && [ \"$2\" = 9 ]\n")
			uname := filepath.Join(fakeBin, "uname")
			body := "#!/bin/sh\ncase \"$1\" in\n  -s) printf '%s\\n' '" + test.unameOS + "' ;;\n  -m) printf '%s\\n' '" + test.unameArch + "' ;;\n  *) exit 64 ;;\nesac\n"
			if err := os.WriteFile(uname, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			if test.unameOS == "Darwin" {
				swVers := "#!/bin/sh\n[ \"$1\" = -productVersion ] || exit 64\nprintf '%s\\n' '" + test.macVersion + "'\n"
				if err := os.WriteFile(filepath.Join(fakeBin, "sw_vers"), []byte(swVers), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			installDir := t.TempDir()
			tarball := filepath.Join(dist, "compound_0.1.0_"+test.asset+".tar.gz")
			output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
				"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
			})
			if test.wantAccept {
				if err != nil {
					t.Fatalf("supported platform was refused: %v\n%s", err, output)
				}
				assertInstalledExecutable(t, filepath.Join(installDir, "compound"))
				return
			}
			if err == nil {
				t.Fatalf("unsupported platform was accepted:\n%s", output)
			}
			if !strings.Contains(string(output), "unsupported platform") && !strings.Contains(string(output), "macOS 15") {
				t.Fatalf("unsupported platform refusal was unclear:\n%s", output)
			}
			assertPathAbsent(t, filepath.Join(installDir, "compound"))
		})
	}
}

func TestOfflineInstallerUsesBusyBoxCompatibleFlockSyntax(t *testing.T) {
	dist, _ := buildInstallerFixture(t)
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("host has no flock implementation for the Linux syntax probe")
	}
	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "uname"), "#!/bin/sh\ncase \"$1\" in\n  -s) echo Linux ;;\n  -m) echo x86_64 ;;\n  *) exit 64 ;;\nesac\n")
	writeExecutable(t, filepath.Join(fakeBin, "flock"), "#!/bin/sh\nset -eu\n[ \"$#\" -eq 2 ] && [ \"$1\" = -n ] && [ \"$2\" = 9 ] || exit 64\nexec \"$REAL_FLOCK\" \"$@\"\n")
	installDir := t.TempDir()
	output, err := runReleaseInstaller(
		filepath.Join(dist, "install-release.sh"),
		filepath.Join(dist, "compound_0.1.0_linux_amd64.tar.gz"),
		installDir,
		t.TempDir(),
		[]string{
			"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
			"REAL_FLOCK=" + realFlock,
		},
	)
	if err != nil {
		t.Fatalf("installer requires util-linux-only flock flags: %v\n%s", err, output)
	}
	assertInstalledExecutable(t, filepath.Join(installDir, "compound"))
}

func TestOfflineInstallerRejectsUnexpectedArchiveMember(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	rewriteReleaseArchive(t, tarball, func(name string, body []byte) []byte { return body }, true)
	rewriteChecksumEntry(t, filepath.Join(dist, "SHA256SUMS"), filepath.Base(tarball), tarball)
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, t.TempDir(), t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer accepted archive with unexpected member:\n%s", output)
	}
	if !strings.Contains(string(output), "archive structure is invalid") {
		t.Fatalf("unexpected-member refusal did not name archive structure:\n%s", output)
	}
}

func TestOfflineInstallerBindsBothBinaryHashesToReleaseIdentity(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	rewriteReleaseArchive(t, tarball, func(name string, body []byte) []byte {
		if strings.HasSuffix(name, "/compound-breadth-engine") {
			return []byte("#!/bin/sh\nprintf 'substituted breadth engine\\n'\n")
		}
		return body
	}, false)
	rewriteChecksumEntry(t, filepath.Join(dist, "SHA256SUMS"), filepath.Base(tarball), tarball)
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, t.TempDir(), t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer accepted breadth engine outside release identity:\n%s", output)
	}
	if !strings.Contains(string(output), "breadth-engine checksum does not match release identity") {
		t.Fatalf("breadth identity refusal was unclear:\n%s", output)
	}
}

func TestOfflineInstallerRejectsUnstableManifestVersion(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	manifestPath := filepath.Join(dist, "RELEASE")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "version 0.1.0", "version 01.2.3", 1))
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteChecksumEntry(t, filepath.Join(dist, "SHA256SUMS"), "RELEASE", manifestPath)
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, t.TempDir(), t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer accepted unstable manifest version:\n%s", output)
	}
	if !strings.Contains(string(output), "RELEASE manifest is invalid") {
		t.Fatalf("unstable manifest refusal was unclear:\n%s", output)
	}
}

func TestOfflineInstallerPersistsPrivatePreparedTransactionBeforeMutation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
		"COMPOUND_INSTALL_CRASH_AT=after_transaction_ready",
	})
	if err == nil {
		t.Fatalf("prepared-checkpoint crash unexpectedly completed:\n%s", output)
	}
	assertFileBody(t, compoundPath, "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")

	stateRoot := filepath.Join(installDir, ".compound-install-state")
	stateInfo, statErr := os.Lstat(stateRoot)
	if statErr != nil {
		t.Fatalf("prepared transaction has no rooted private state: %v", statErr)
	}
	if !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 || stateInfo.Mode().Perm() != 0o700 {
		t.Fatalf("transaction state mode = %v, want private directory 0700", stateInfo.Mode())
	}
	for _, name := range []string{"owner", filepath.Join("transaction", "journal")} {
		path := filepath.Join(stateRoot, name)
		info, lstatErr := os.Lstat(path)
		if lstatErr != nil {
			t.Fatalf("prepared transaction is missing %s: %v", name, lstatErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want private regular file 0600", name, info.Mode())
		}
	}
	owner, readErr := os.ReadFile(filepath.Join(stateRoot, "owner"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	ownerMatch := regexp.MustCompile(`\Aschema compound\.install\.owner\.v0\.1\npid ([1-9][0-9]*)\nnonce ([0-9a-f]{32})\n\z`).FindStringSubmatch(string(owner))
	if ownerMatch == nil {
		t.Fatalf("owner record is not strict:\n%s", owner)
	}
	journalPath := filepath.Join(stateRoot, "transaction", "journal")
	journal, readErr := os.ReadFile(journalPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	canonicalRoot, evalErr := filepath.EvalSymlinks(installDir)
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	transactionDir := filepath.Dir(journalPath)
	wantJournal := strings.Join([]string{
		`schema compound\.install\.transaction\.v0\.1`,
		`owner_pid ` + regexp.QuoteMeta(ownerMatch[1]),
		`owner_nonce ` + regexp.QuoteMeta(ownerMatch[2]),
		`install_root_sha256 ` + testSHA256String(canonicalRoot),
		`compound_target compound`,
		`breadth_target compound-breadth-engine`,
		`compound_prior_present 1`,
		`compound_prior_type regular`,
		`compound_prior_sha256 ` + testSHA256(t, compoundPath),
		`breadth_prior_present 1`,
		`breadth_prior_type regular`,
		`breadth_prior_sha256 ` + testSHA256(t, breadthPath),
		`compound_new_sha256 ` + testSHA256(t, filepath.Join(transactionDir, "compound.new")),
		`breadth_new_sha256 ` + testSHA256(t, filepath.Join(transactionDir, "compound-breadth-engine.new")),
		`phase prepared`,
	}, `\n`)
	if !regexp.MustCompile(`\A` + wantJournal + `\n\z`).Match(journal) {
		t.Fatalf("prepared journal does not match the exact schema/order/identity:\n%s", journal)
	}
}

func TestOfflineInstallerRejectsMalformedOrForeignPreparedJournalWithoutMutation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, interruptedInstallState)
	}{
		{
			name: "missing journal",
			mutate: func(t *testing.T, state interruptedInstallState) {
				if err := os.Remove(filepath.Join(state.transactionDir, "journal")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra journal field",
			mutate: func(t *testing.T, state interruptedInstallState) {
				path := filepath.Join(state.transactionDir, "journal")
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("unexpected field\n"); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra transaction entry",
			mutate: func(t *testing.T, state interruptedInstallState) {
				if err := os.WriteFile(filepath.Join(state.transactionDir, "unexpected"), []byte("foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink journal",
			mutate: func(t *testing.T, state interruptedInstallState) {
				journal := filepath.Join(state.transactionDir, "journal")
				body, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				foreign := filepath.Join(t.TempDir(), "journal")
				if err := os.WriteFile(foreign, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(journal); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink disposable scratch",
			mutate: func(t *testing.T, state interruptedInstallState) {
				foreign := filepath.Join(t.TempDir(), "compound.publish")
				if err := os.WriteFile(foreign, []byte("foreign\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, filepath.Join(state.transactionDir, "compound.publish")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory disposable scratch",
			mutate: func(t *testing.T, state interruptedInstallState) {
				if err := os.Mkdir(filepath.Join(state.transactionDir, "compound.restore"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hard-linked disposable scratch",
			mutate: func(t *testing.T, state interruptedInstallState) {
				foreign := filepath.Join(t.TempDir(), "scratch")
				if err := os.WriteFile(foreign, []byte("foreign\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(foreign, filepath.Join(state.transactionDir, "compound-breadth-engine.publish")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong-mode disposable scratch",
			mutate: func(t *testing.T, state interruptedInstallState) {
				path := filepath.Join(state.transactionDir, "compound-breadth-engine.restore")
				if err := os.WriteFile(path, []byte("partial\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "foreign install root",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceJournalLine(t, state, "install_root_sha256 ", "install_root_sha256 "+strings.Repeat("0", 64))
			},
		},
		{
			name: "foreign target",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceJournalLine(t, state, "compound_target ", "compound_target ../foreign")
			},
		},
		{
			name: "inconsistent prior type",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceJournalLine(t, state, "compound_prior_type ", "compound_prior_type absent")
			},
		},
		{
			name: "unknown durable phase",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceJournalLine(t, state, "phase ", "phase unknown")
			},
		},
		{
			name: "new digest disagrees with retained bytes",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceJournalLine(t, state, "compound_new_sha256 ", "compound_new_sha256 "+strings.Repeat("0", 64))
			},
		},
		{
			name: "owner nonce disagrees with journal",
			mutate: func(t *testing.T, state interruptedInstallState) {
				replaceRecordLine(t, filepath.Join(state.stateRoot, "owner"), "nonce ", "nonce "+strings.Repeat("0", 32))
			},
		},
		{
			name: "symlink retained new bytes",
			mutate: func(t *testing.T, state interruptedInstallState) {
				path := filepath.Join(state.transactionDir, "compound.new")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				foreign := filepath.Join(t.TempDir(), "compound.new")
				if err := os.WriteFile(foreign, body, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra rooted state entry",
			mutate: func(t *testing.T, state interruptedInstallState) {
				if err := os.WriteFile(filepath.Join(state.stateRoot, "unexpected"), []byte("foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := interruptPreparedInstall(t, dist, tarball)
			test.mutate(t, state)
			before := snapshotInstallTree(t, state.installDir)

			output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, nil)
			if err == nil {
				t.Fatalf("installer accepted unsafe prepared state:\n%s", output)
			}
			if !strings.Contains(string(output), "interrupted install state is unsafe") {
				t.Fatalf("unsafe-state refusal was unclear:\n%s", output)
			}
			assertInstallTreeSnapshot(t, state.installDir, before)
		})
	}
}

func TestOfflineInstallerRejectsForeignTargetTypeWithoutMutation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	state := interruptPreparedInstall(t, dist, tarball)
	foreign := filepath.Join(t.TempDir(), "foreign-compound")
	if err := os.WriteFile(foreign, []byte("foreign target\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state.compoundPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, state.compoundPath); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, state.installDir)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, nil)
	if err == nil {
		t.Fatalf("installer accepted foreign target type:\n%s", output)
	}
	if !strings.Contains(string(output), "interrupted install state is unsafe") {
		t.Fatalf("foreign-target refusal was unclear:\n%s", output)
	}
	assertInstallTreeSnapshot(t, state.installDir, before)
	linkInfo, statErr := os.Lstat(state.compoundPath)
	if statErr != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("foreign target link was mutated: mode=%v err=%v", linkInfo.Mode(), statErr)
	}
	assertFileBody(t, foreign, "foreign target\n")
	assertFileBody(t, state.breadthPath, "old breadth\n")
}

func TestOfflineInstallerCommittedRestartCompletesExactNewPairFromRetainedBytes(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	state := interruptInstallAt(t, dist, tarball, "after_commit")
	compoundNew := filepath.Join(state.transactionDir, "compound.new")
	breadthNew := filepath.Join(state.transactionDir, "compound-breadth-engine.new")
	wantCompound := testSHA256(t, compoundNew)
	wantBreadth := testSHA256(t, breadthNew)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, []string{
		"COMPOUND_INSTALL_CRASH_AT=after_recovery",
	})
	if err == nil {
		t.Fatalf("post-commit recovery checkpoint unexpectedly completed:\n%s", output)
	}
	if !strings.Contains(string(output), "recovered interrupted install state") {
		t.Fatalf("committed restart did not report recovery:\n%s", output)
	}
	if got := testSHA256(t, state.compoundPath); got != wantCompound {
		t.Fatalf("committed restart compound digest = %s, want %s", got, wantCompound)
	}
	if got := testSHA256(t, state.breadthPath); got != wantBreadth {
		t.Fatalf("committed restart breadth digest = %s, want %s", got, wantBreadth)
	}
}

func TestOfflineInstallerCommittedRestartRefusesChangedTarget(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	state := interruptInstallAt(t, dist, tarball, "after_commit")
	previousCompound := filepath.Join(state.transactionDir, "compound.previous")
	if err := copyTestFile(previousCompound, state.compoundPath, 0o700); err != nil {
		t.Fatal(err)
	}
	wantBreadth := testSHA256(t, state.breadthPath)
	before := snapshotInstallTree(t, state.installDir)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, nil)
	if err == nil {
		t.Fatalf("committed restart overwrote a changed target:\n%s", output)
	}
	if !strings.Contains(string(output), "interrupted install state is unsafe") {
		t.Fatalf("committed target mismatch refusal was unclear:\n%s", output)
	}
	assertInstallTreeSnapshot(t, state.installDir, before)
	assertFileBody(t, state.compoundPath, "old compound\n")
	if got := testSHA256(t, state.breadthPath); got != wantBreadth {
		t.Fatalf("committed refusal mutated breadth digest to %s, want %s", got, wantBreadth)
	}
	if _, statErr := os.Lstat(state.transactionDir); statErr != nil {
		t.Fatalf("committed refusal did not preserve transaction state: %v", statErr)
	}
}

func TestOfflineInstallerRejectsJournalNextBesideCommittedAuthority(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	state := interruptInstallAt(t, dist, tarball, "after_commit")
	journalNext := filepath.Join(state.transactionDir, "journal.next")
	if err := os.WriteFile(journalNext, []byte("schema compound.install.transaction.v0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, state.installDir)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, nil)
	if err == nil {
		t.Fatalf("installer accepted journal.next beside committed authority:\n%s", output)
	}
	if !strings.Contains(string(output), "interrupted install state is unsafe") {
		t.Fatalf("committed journal.next refusal was unclear:\n%s", output)
	}
	assertInstallTreeSnapshot(t, state.installDir, before)
}

func TestOfflineInstallerCannotBypassLiveKernelLockWithForgedMarker(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	home := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	fakeBin, entered, release := installBlockingSyncShim(t)
	first := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=prepared",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var firstOutput strings.Builder
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	waited := false
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if first.Process != nil {
			_ = first.Process.Kill()
		}
		if !waited {
			_ = first.Wait()
		}
	})
	waitForTestPath(t, entered)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"_COMPOUND_INSTALL_LOCK_NONCE=" + strings.Repeat("0", 32),
	})
	if err == nil {
		t.Fatalf("forged internal marker bypassed a live kernel lock:\n%s", output)
	}
	if !strings.Contains(string(output), "another install transaction is active") {
		t.Fatalf("forged-marker concurrency refusal was unclear:\n%s", output)
	}
	assertFileBody(t, compoundPath, "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("lock holder failed after forged contender: %v\n%s", err, firstOutput.String())
	}
	waited = true
}

func TestOfflineInstallerPreservesJournalOnSyncFailure(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	for _, test := range []struct {
		phase         string
		wantRecovered string
	}{
		{phase: "prepared", wantRecovered: "old"},
		{phase: "targets", wantRecovered: "old"},
		{phase: "committed", wantRecovered: "new"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			installDir := t.TempDir()
			home := t.TempDir()
			compoundPath := filepath.Join(installDir, "compound")
			breadthPath := filepath.Join(installDir, "compound-breadth-engine")
			if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			fakeBin := installSyncFailureShim(t)
			output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
				"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
				"COMPOUND_TEST_SYNC_FAIL_PHASE=" + test.phase,
			})
			if err == nil {
				t.Fatalf("sync failure at %s unexpectedly succeeded:\n%s", test.phase, output)
			}
			if !strings.Contains(string(output), "durability barrier failed at "+test.phase) ||
				!strings.Contains(string(output), "transaction state preserved") {
				t.Fatalf("sync failure at %s did not preserve ambiguity:\n%s", test.phase, output)
			}
			transactionDir := filepath.Join(installDir, ".compound-install-state", "transaction")
			if _, statErr := os.Lstat(filepath.Join(transactionDir, "journal")); statErr != nil {
				t.Fatalf("sync failure at %s did not preserve its journal: %v", test.phase, statErr)
			}
			wantCompound := testSHA256(t, filepath.Join(transactionDir, "compound.new"))
			wantBreadth := testSHA256(t, filepath.Join(transactionDir, "compound-breadth-engine.new"))

			output, err = runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=after_recovery",
			})
			if err == nil {
				t.Fatalf("recovery checkpoint after %s sync failure unexpectedly completed:\n%s", test.phase, output)
			}
			if test.wantRecovered == "old" {
				assertFileBody(t, compoundPath, "old compound\n")
				assertFileBody(t, breadthPath, "old breadth\n")
				return
			}
			if got := testSHA256(t, compoundPath); got != wantCompound {
				t.Fatalf("committed sync recovery compound digest = %s, want %s", got, wantCompound)
			}
			if got := testSHA256(t, breadthPath); got != wantBreadth {
				t.Fatalf("committed sync recovery breadth digest = %s, want %s", got, wantBreadth)
			}
		})
	}
}

func TestOfflineInstallerPreservesPreparedAuthorityWhenScratchCleanupSyncFails(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	state := interruptInstallAt(t, dist, tarball, "after_compound_publish_copy")
	fakeBin := installSyncFailureShim(t)
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_FAIL_PHASE=unpublished_scratch_discarded",
	})
	if err == nil {
		t.Fatalf("scratch-cleanup sync failure unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "durability barrier failed at unpublished_scratch_discarded") ||
		!strings.Contains(string(output), "transaction state preserved") {
		t.Fatalf("scratch-cleanup sync failure did not preserve prepared authority:\n%s", output)
	}
	journal := filepath.Join(state.transactionDir, "journal")
	if _, statErr := os.Lstat(journal); statErr != nil {
		t.Fatalf("scratch-cleanup sync failure lost authoritative journal: %v", statErr)
	}

	output, err = runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, state.installDir, state.home, []string{
		"COMPOUND_INSTALL_CRASH_AT=after_recovery",
	})
	if err == nil {
		t.Fatalf("recovery checkpoint after scratch-cleanup sync failure unexpectedly completed:\n%s", output)
	}
	if !strings.Contains(string(output), "recovered interrupted install state") {
		t.Fatalf("scratch-cleanup sync failure was not recoverable:\n%s", output)
	}
	assertFileBody(t, state.compoundPath, "old compound\n")
	assertFileBody(t, state.breadthPath, "old breadth\n")
	assertCleanInstallState(t, state.installDir)
}

func TestOfflineInstallerUsesKernelLockForLiveConcurrency(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	home := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBin, entered, release := installBlockingSyncShim(t)
	first := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=prepared",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var firstOutput strings.Builder
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	firstWaited := false
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if first.Process != nil {
			_ = first.Process.Kill()
		}
		if !firstWaited {
			_ = first.Wait()
		}
	})
	waitForTestPath(t, entered)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, nil)
	if err == nil {
		t.Fatalf("concurrent installer entered a live transaction:\n%s", output)
	}
	if !strings.Contains(string(output), "another install transaction is active") {
		t.Fatalf("live concurrency refusal was unclear:\n%s", output)
	}
	assertFileBody(t, compoundPath, "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("lock holder failed after release: %v\n%s", err, firstOutput.String())
	}
	firstWaited = true
}

func TestOfflineInstallerCleanupCannotDeleteReplacedOwnerGeneration(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	home := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(installDir, "compound"):                "old compound\n",
		filepath.Join(installDir, "compound-breadth-engine"): "old breadth\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	fakeBin, entered, release := installBlockingSyncShim(t)
	installer := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=committed",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	installer.Stdout = &output
	installer.Stderr = &output
	waited := false
	if err := installer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if installer.Process != nil {
			_ = installer.Process.Kill()
		}
		if !waited {
			_ = installer.Wait()
		}
	})
	waitForTestPath(t, entered)

	ownerPath := filepath.Join(installDir, ".compound-install-state", "owner")
	replaceRecordLine(t, ownerPath, "pid ", "pid 1")
	replaceRecordLine(t, ownerPath, "nonce ", "nonce "+strings.Repeat("0", 32))
	before := snapshotInstallTree(t, installDir)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installer.Wait()
	waited = true
	if err == nil {
		t.Fatalf("stale installer deleted a replacement owner generation:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "transaction owner changed before cleanup") ||
		!strings.Contains(output.String(), "recovery transaction preserved") {
		t.Fatalf("owner-generation refusal did not preserve recovery evidence:\n%s", output.String())
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerRevalidatesOwnerImmediatelyBeforeRetirement(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(installDir, "compound"):                "old compound\n",
		filepath.Join(installDir, "compound-breadth-engine"): "old breadth\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	fakeBin, entered, release := installBlockingSyncShim(t)
	installer := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=owner_retiring",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	installer.Stdout = &output
	installer.Stderr = &output
	waited := false
	if err := installer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if installer.Process != nil {
			_ = installer.Process.Kill()
		}
		if !waited {
			_ = installer.Wait()
		}
	})
	waitForTestPath(t, entered)

	ownerPath := filepath.Join(installDir, ".compound-install-state", "owner")
	replaceRecordLine(t, ownerPath, "pid ", "pid 1")
	replaceRecordLine(t, ownerPath, "nonce ", "nonce "+strings.Repeat("0", 32))
	before := snapshotInstallTree(t, installDir)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installer.Wait()
	waited = true
	if err == nil {
		t.Fatalf("installer retired an owner generation replaced after validation:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "transaction owner changed before cleanup") {
		t.Fatalf("owner-retirement refusal was unclear:\n%s", output.String())
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerPinsCanonicalRootAcrossGOBINSymlinkRetarget(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	linkParent := t.TempDir()
	installLink := filepath.Join(linkParent, "bin")
	if err := os.Symlink(rootA, installLink); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		root string
		name string
		body string
	}{
		{root: rootA, name: "compound", body: "old compound\n"},
		{root: rootA, name: "compound-breadth-engine", body: "old breadth\n"},
		{root: rootB, name: "compound", body: "foreign compound\n"},
		{root: rootB, name: "compound-breadth-engine", body: "foreign breadth\n"},
	} {
		if err := os.WriteFile(filepath.Join(item.root, item.name), []byte(item.body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	foreignBefore := snapshotInstallTree(t, rootB)
	fakeBin, entered, release := installBlockingSyncShim(t)
	first := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installLink, t.TempDir(), []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=prepared",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	first.Stdout = &output
	first.Stderr = &output
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if first.Process != nil {
			_ = first.Process.Kill()
		}
	})
	waitForTestPath(t, entered)
	if err := os.Remove(installLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rootB, installLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("installer escaped or refused its pinned root: %v\n%s", err, output.String())
	}
	assertInstalledExecutable(t, filepath.Join(rootA, "compound"))
	assertInstalledExecutable(t, filepath.Join(rootA, "compound-breadth-engine"))
	assertInstallTreeSnapshot(t, rootB, foreignBefore)
}

func TestOfflineInstallerPinsPhysicalRootAfterRenameAndPathRecreation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	movedRoot := installDir + ".moved"
	t.Cleanup(func() { _ = os.RemoveAll(movedRoot) })
	home := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(installDir, "compound"):                "old compound\n",
		filepath.Join(installDir, "compound-breadth-engine"): "old breadth\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	fakeBin, entered, release := installBlockingSyncShim(t)
	installer := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=prepared",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	installer.Stdout = &output
	installer.Stderr = &output
	waited := false
	if err := installer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if installer.Process != nil {
			_ = installer.Process.Kill()
		}
		if !waited {
			_ = installer.Wait()
		}
	})
	waitForTestPath(t, entered)

	if err := os.Rename(installDir, movedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(installDir, "compound"):                "old compound\n",
		filepath.Join(installDir, "compound-breadth-engine"): "old breadth\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(
		filepath.Join(movedRoot, ".compound-install-state"),
		filepath.Join(installDir, ".compound-install-state"),
	); err != nil {
		t.Fatal(err)
	}
	replacementBefore := snapshotInstallTree(t, installDir)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installer.Wait()
	waited = true
	if err == nil {
		t.Fatalf("installer followed a recreated install-root pathname:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "physical install root changed") {
		t.Fatalf("physical-root refusal was unclear:\n%s", output.String())
	}
	assertInstallTreeSnapshot(t, installDir, replacementBefore)
	assertFileBody(t, filepath.Join(movedRoot, "compound"), "old compound\n")
	assertFileBody(t, filepath.Join(movedRoot, "compound-breadth-engine"), "old breadth\n")
}

func TestOfflineInstallerRejectsInsecureExistingStateRootWithoutMutation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(installDir, ".compound-install-state")
	if err := os.Mkdir(stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, installDir)
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer repaired and trusted an insecure state root:\n%s", output)
	}
	if !strings.Contains(string(output), "state root is not mode 0700") {
		t.Fatalf("insecure-state refusal was unclear:\n%s", output)
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerDoesNotCreateLockInsideUntrustedExistingStateRoot(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	for path, body := range map[string]string{
		filepath.Join(installDir, "compound"):                "old compound\n",
		filepath.Join(installDir, "compound-breadth-engine"): "old breadth\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stateRoot := filepath.Join(installDir, ".compound-install-state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "foreign"), []byte("foreign state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, installDir)

	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("installer trusted an existing state root without its persistent lock:\n%s", output)
	}
	if !strings.Contains(string(output), "state root has entries but no persistent lock") {
		t.Fatalf("missing-lock refusal was unclear:\n%s", output)
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestInstallerDeclaresKernelLockAndDurabilityBarriersForBothPlatforms(t *testing.T) {
	body, err := os.ReadFile("../../scripts/install-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`exec 9>"$lock_file"`,
		`lockf -s -t 0 9`,
		`flock -n 9`,
		`COMPOUND_INSTALL_SYNC_PHASE="$barrier_phase" sync`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer lacks supported-platform exclusion/durability primitive %q", required)
		}
	}
}

func TestOfflineInstallerRollsBackBothBinariesAfterFirstRenameFailure(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
		"COMPOUND_INSTALL_FAIL_AFTER_FIRST_RENAME=1",
	})
	if err == nil {
		t.Fatalf("injected between-rename failure unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "injected failure after first binary rename") {
		t.Fatalf("injected failure was not observed:\n%s", output)
	}
	assertFileBody(t, compoundPath, "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")
	assertCleanInstallState(t, installDir)
}

func TestOfflineInstallerPreservesRecoveryTransactionWhenRollbackRestoreFails(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
		"COMPOUND_INSTALL_FAIL_AFTER_FIRST_RENAME=1",
		"COMPOUND_INSTALL_FAIL_RESTORE_COMPOUND=1",
	})
	if err == nil {
		t.Fatalf("injected rollback restoration failure unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "rollback restoration failed") || !strings.Contains(string(output), "recovery transaction preserved") {
		t.Fatalf("rollback failure did not report preserved recovery state:\n%s", output)
	}

	stateRoot := filepath.Join(installDir, ".compound-install-state")
	transaction := filepath.Join(stateRoot, "transaction")
	assertFileBody(t, filepath.Join(transaction, "compound.previous"), "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")
	for _, path := range []string{filepath.Join(stateRoot, "lock"), filepath.Join(stateRoot, "owner"), filepath.Join(transaction, "journal")} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("rollback failure did not preserve %s: %v", path, statErr)
		}
	}
}

func TestOfflineInstallerRecoversEveryInterruptedPairPublicationBoundary(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	for _, test := range []struct {
		boundary           string
		hasRecoveryState   bool
		committedOnRestart bool
	}{
		{boundary: "after_lock"},
		{boundary: "after_owner", hasRecoveryState: true},
		{boundary: "after_transaction_ready", hasRecoveryState: true},
		{boundary: "after_prepared", hasRecoveryState: true},
		{boundary: "after_compound_backup", hasRecoveryState: true},
		{boundary: "after_breadth_backup", hasRecoveryState: true},
		{boundary: "after_compound_publish", hasRecoveryState: true},
		{boundary: "after_breadth_publish", hasRecoveryState: true},
		{boundary: "after_targets_synced", hasRecoveryState: true},
		{boundary: "after_commit", hasRecoveryState: true, committedOnRestart: true},
		{boundary: "after_committed", hasRecoveryState: true, committedOnRestart: true},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			installDir := t.TempDir()
			home := t.TempDir()
			compoundPath := filepath.Join(installDir, "compound")
			breadthPath := filepath.Join(installDir, "compound-breadth-engine")
			if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=" + test.boundary,
			})
			if err == nil {
				t.Fatalf("injected crash at %s unexpectedly completed:\n%s", test.boundary, output)
			}

			if !test.hasRecoveryState {
				output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
				if err != nil {
					t.Fatalf("installer did not restart after %s: %v\n%s", test.boundary, err, output)
				}
				assertInstalledExecutable(t, compoundPath)
				assertInstalledExecutable(t, breadthPath)
				assertCleanInstallState(t, installDir)
				return
			}

			output, err = runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=after_recovery",
			})
			if err == nil {
				t.Fatalf("recovery checkpoint after %s unexpectedly completed:\n%s", test.boundary, output)
			}
			if !strings.Contains(string(output), "recovered interrupted install state") {
				t.Fatalf("installer did not report recovery after %s:\n%s", test.boundary, output)
			}
			if test.committedOnRestart {
				assertInstalledExecutable(t, compoundPath)
				assertInstalledExecutable(t, breadthPath)
				for path, old := range map[string]string{compoundPath: "old compound\n", breadthPath: "old breadth\n"} {
					body, readErr := os.ReadFile(path)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if string(body) == old {
						t.Fatalf("committed recovery at %s rolled back %s", test.boundary, filepath.Base(path))
					}
				}
			} else {
				assertFileBody(t, compoundPath, "old compound\n")
				assertFileBody(t, breadthPath, "old breadth\n")
			}

			output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
			if err != nil {
				t.Fatalf("installer did not finish after recovering crash at %s: %v\n%s", test.boundary, err, output)
			}
			assertInstalledExecutable(t, compoundPath)
			assertInstalledExecutable(t, breadthPath)
			assertCleanInstallState(t, installDir)
		})
	}
}

func TestOfflineInstallerRecoversPartialCommittedJournalStage(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	installDir := t.TempDir()
	home := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runReleaseInstaller(installer, tarball, installDir, home, []string{
		"COMPOUND_INSTALL_CRASH_AT=during_committed_journal_write",
	})
	if err == nil {
		t.Fatalf("partial committed-journal write unexpectedly completed:\n%s", output)
	}
	transaction := filepath.Join(installDir, ".compound-install-state", "transaction")
	journalNext := filepath.Join(transaction, "journal.next")
	assertFileBody(t, journalNext, "schema compound.install.transaction.v0.1\n")
	journal, readErr := os.ReadFile(filepath.Join(transaction, "journal"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.HasSuffix(string(journal), "phase prepared\n") {
		t.Fatalf("partial committed stage replaced authoritative prepared journal:\n%s", journal)
	}

	output, err = runReleaseInstaller(installer, tarball, installDir, home, []string{
		"COMPOUND_INSTALL_CRASH_AT=after_recovery",
	})
	if err == nil {
		t.Fatalf("recovery checkpoint after partial committed journal unexpectedly completed:\n%s", output)
	}
	if !strings.Contains(string(output), "recovered interrupted install state") {
		t.Fatalf("partial committed journal was not recovered deterministically:\n%s", output)
	}
	assertFileBody(t, compoundPath, "old compound\n")
	assertFileBody(t, breadthPath, "old breadth\n")
	assertCleanInstallState(t, installDir)

	output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
	if err != nil {
		t.Fatalf("clean restart after partial committed journal failed: %v\n%s", err, output)
	}
	assertInstalledExecutable(t, compoundPath)
	assertInstalledExecutable(t, breadthPath)
	assertCleanInstallState(t, installDir)
}

func TestOfflineInstallerRecoversPreVerificationPublicationScratch(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	for _, test := range []struct {
		name        string
		crashPoint  string
		scratchName string
	}{
		{name: "compound", crashPoint: "after_compound_publish_copy", scratchName: "compound.publish"},
		{name: "breadth", crashPoint: "after_breadth_publish_copy", scratchName: "compound-breadth-engine.publish"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installDir := t.TempDir()
			home := t.TempDir()
			compoundPath := filepath.Join(installDir, "compound")
			breadthPath := filepath.Join(installDir, "compound-breadth-engine")
			if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=" + test.crashPoint,
			})
			if err == nil {
				t.Fatalf("pre-verification publish scratch crash at %s unexpectedly completed:\n%s", test.crashPoint, output)
			}
			truncatePreVerificationScratch(t, filepath.Join(installDir, ".compound-install-state", "transaction", test.scratchName))

			output, err = runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=after_recovery",
			})
			if err == nil {
				t.Fatalf("recovery checkpoint after %s unexpectedly completed:\n%s", test.crashPoint, output)
			}
			if !strings.Contains(string(output), "recovered interrupted install state") {
				t.Fatalf("publish scratch at %s was not recovered deterministically:\n%s", test.crashPoint, output)
			}
			assertFileBody(t, compoundPath, "old compound\n")
			assertFileBody(t, breadthPath, "old breadth\n")
			assertCleanInstallState(t, installDir)

			output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
			if err != nil {
				t.Fatalf("clean restart after %s failed: %v\n%s", test.crashPoint, err, output)
			}
			assertInstalledExecutable(t, compoundPath)
			assertInstalledExecutable(t, breadthPath)
			assertCleanInstallState(t, installDir)
		})
	}
}

func TestOfflineInstallerRecoversPreVerificationRestoreScratch(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	for _, test := range []struct {
		name          string
		initialCrash  string
		recoveryCrash string
		scratchName   string
	}{
		{
			name:          "compound",
			initialCrash:  "after_compound_publish",
			recoveryCrash: "after_compound_restore_copy",
			scratchName:   "compound.restore",
		},
		{
			name:          "breadth",
			initialCrash:  "after_breadth_publish",
			recoveryCrash: "after_breadth_restore_copy",
			scratchName:   "compound-breadth-engine.restore",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			installDir := t.TempDir()
			home := t.TempDir()
			compoundPath := filepath.Join(installDir, "compound")
			breadthPath := filepath.Join(installDir, "compound-breadth-engine")
			if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=" + test.initialCrash,
			})
			if err == nil {
				t.Fatalf("initial publication crash at %s unexpectedly completed:\n%s", test.initialCrash, output)
			}
			output, err = runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=" + test.recoveryCrash,
			})
			if err == nil {
				t.Fatalf("pre-verification restore scratch crash at %s unexpectedly completed:\n%s", test.recoveryCrash, output)
			}
			truncatePreVerificationScratch(t, filepath.Join(installDir, ".compound-install-state", "transaction", test.scratchName))

			output, err = runReleaseInstaller(installer, tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=after_recovery",
			})
			if err == nil {
				t.Fatalf("recovery checkpoint after %s unexpectedly completed:\n%s", test.recoveryCrash, output)
			}
			if !strings.Contains(string(output), "recovered interrupted install state") {
				t.Fatalf("restore scratch at %s was not recovered deterministically:\n%s", test.recoveryCrash, output)
			}
			assertFileBody(t, compoundPath, "old compound\n")
			assertFileBody(t, breadthPath, "old breadth\n")
			assertCleanInstallState(t, installDir)

			output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
			if err != nil {
				t.Fatalf("clean restart after %s failed: %v\n%s", test.recoveryCrash, err, output)
			}
			assertInstalledExecutable(t, compoundPath)
			assertInstalledExecutable(t, breadthPath)
			assertCleanInstallState(t, installDir)
		})
	}
}

func TestOfflineInstallerRecoversEveryExpectedTransactionNamespaceBoundary(t *testing.T) {
	for _, crashPoint := range []string{
		"after_prepared_staged",
		"after_committed_staged",
		"after_transaction_retired",
		"cleanup_after_compound_new",
		"cleanup_after_breadth_new",
		"cleanup_after_compound_previous",
		"cleanup_after_breadth_previous",
		"cleanup_after_journal",
		"cleanup_after_transaction_dir",
	} {
		t.Run(crashPoint, func(t *testing.T) {
			dist, tarball := buildInstallerFixture(t)
			installDir := t.TempDir()
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(installDir, "compound"), []byte("old compound\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(installDir, "compound-breadth-engine"), []byte("old breadth\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
				"COMPOUND_INSTALL_CRASH_AT=" + crashPoint,
			})
			if err == nil {
				t.Fatalf("installer did not crash at %s:\n%s", crashPoint, output)
			}
			output, err = runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, nil)
			if err != nil {
				t.Fatalf("restart after %s failed: %v\n%s", crashPoint, err, output)
			}
			assertInstalledExecutable(t, filepath.Join(installDir, "compound"))
			assertInstalledExecutable(t, filepath.Join(installDir, "compound-breadth-engine"))
			assertCleanInstallState(t, installDir)
		})
	}
}

func TestOfflineInstallerReparsesCommittedJournalBeforeCleanup(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	fakeBin, entered, release := installBlockingSyncShim(t)
	installer := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=committed",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	installer.Stdout = &output
	installer.Stderr = &output
	waited := false
	if err := installer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if installer.Process != nil {
			_ = installer.Process.Kill()
		}
		if !waited {
			_ = installer.Wait()
		}
	})
	waitForTestPath(t, entered)

	journal := filepath.Join(installDir, ".compound-install-state", "transaction", "journal")
	replaceRecordLine(t, journal, "owner_nonce ", "owner_nonce "+strings.Repeat("0", 32))
	before := snapshotInstallTree(t, installDir)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installer.Wait()
	waited = true
	if err == nil {
		t.Fatalf("installer cleaned a replaced committed journal:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "journal changed before cleanup") {
		t.Fatalf("committed-journal refusal was unclear:\n%s", output.String())
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerReparsesPreparedJournalAfterRecoveryMutation(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, "compound"), []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "compound-breadth-engine"), []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"COMPOUND_INSTALL_CRASH_AT=after_compound_publish",
	}); err == nil {
		t.Fatalf("installer did not create prepared recovery state:\n%s", output)
	}

	fakeBin, entered, release := installBlockingSyncShim(t)
	recovery := releaseInstallerCommand(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"COMPOUND_TEST_SYNC_BLOCK_PHASE=recovery_targets",
		"COMPOUND_TEST_SYNC_ENTERED=" + entered,
		"COMPOUND_TEST_SYNC_RELEASE=" + release,
	})
	var output strings.Builder
	recovery.Stdout = &output
	recovery.Stderr = &output
	waited := false
	if err := recovery.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		if recovery.Process != nil {
			_ = recovery.Process.Kill()
		}
		if !waited {
			_ = recovery.Wait()
		}
	})
	waitForTestPath(t, entered)

	journal := filepath.Join(installDir, ".compound-install-state", "transaction", "journal")
	replaceRecordLine(t, journal, "owner_nonce ", "owner_nonce "+strings.Repeat("0", 32))
	before := snapshotInstallTree(t, installDir)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := recovery.Wait()
	waited = true
	if err == nil {
		t.Fatalf("recovery cleaned a replaced prepared journal:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "recovery transaction preserved") {
		t.Fatalf("recovery journal refusal was unclear:\n%s", output.String())
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerRefusesRecoveryWhenPublishedTargetIdentityChanged(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installer := filepath.Join(dist, "install-release.sh")
	installDir := t.TempDir()
	home := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runReleaseInstaller(installer, tarball, installDir, home, []string{
		"COMPOUND_INSTALL_CRASH_AT=after_compound_publish",
	})
	if err == nil {
		t.Fatalf("injected crash unexpectedly completed:\n%s", output)
	}
	if err := os.WriteFile(compoundPath, []byte("operator replacement\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, installDir)

	output, err = runReleaseInstaller(installer, tarball, installDir, home, nil)
	if err == nil {
		t.Fatalf("installer overwrote a target changed after interruption:\n%s", output)
	}
	if !strings.Contains(string(output), "interrupted install state is unsafe") ||
		!strings.Contains(string(output), "recovery transaction preserved") {
		t.Fatalf("unsafe recovery refusal did not identify preserved evidence:\n%s", output)
	}
	assertInstallTreeSnapshot(t, installDir, before)
}

func TestOfflineInstallerAcceptsExactVerifiedRelease(t *testing.T) {
	dist, tarball := buildInstallerFixture(t)
	installDir := t.TempDir()
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("exact verified release failed to install: %v\n%s", err, output)
	}
	for _, name := range []string{"compound", "compound-breadth-engine"} {
		info, err := os.Lstat(filepath.Join(installDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			t.Fatalf("installed %s mode = %v, want executable regular file", name, info.Mode())
		}
	}
}

func buildInstallerFixture(t *testing.T) (string, string) {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	if err := cloneInstallerFixtureDistribution(t, dist); err != nil {
		t.Fatal(err)
	}
	return dist, filepath.Join(dist, installerFixtureTarball())
}

func installerFixtureTarball() string {
	osName := "linux"
	archName := "amd64"
	switch output, err := exec.Command("uname", "-s").Output(); {
	case err == nil && strings.TrimSpace(string(output)) == "Darwin":
		osName = "darwin"
	}
	switch output, err := exec.Command("uname", "-m").Output(); {
	case err == nil && (strings.TrimSpace(string(output)) == "arm64" || strings.TrimSpace(string(output)) == "aarch64"):
		archName = "arm64"
	}
	return "compound_0.1.0_" + osName + "_" + archName + ".tar.gz"
}

func runReleaseInstaller(installer, tarball, installDir, home string, extra []string) ([]byte, error) {
	cmd := releaseInstallerCommand(installer, tarball, installDir, home, extra)
	return cmd.CombinedOutput()
}

func releaseInstallerCommand(installer, tarball, installDir, home string, extra []string) *exec.Cmd {
	cmd := exec.Command("sh", installer)
	cmd.Env = releaseInstallerEnv(os.Environ(), append([]string{
		"COMPOUND_RELEASE_TARBALL=" + tarball,
		"COMPOUND_VERSION=0.1.0",
		"COMPOUND_HOME=" + home,
		"GOBIN=" + installDir,
	}, extra...)...)
	return cmd
}

func releaseInstallerEnv(base []string, values ...string) []string {
	replace := map[string]bool{
		"COMPOUND_RELEASE_TARBALL": true,
		"COMPOUND_VERSION":         true,
		"COMPOUND_HOME":            true,
		"GOBIN":                    true,
		"COMPOUND_INSTALL_FAIL_AFTER_FIRST_RENAME": true,
		"COMPOUND_INSTALL_FAIL_RESTORE_COMPOUND":   true,
		"COMPOUND_INSTALL_CRASH_AT":                true,
		"COMPOUND_RELEASE_REPOSITORY":              true,
	}
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		replace[key] = true
	}
	result := make([]string, 0, len(base)+len(values))
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if !replace[key] {
			result = append(result, value)
		}
	}
	return append(result, values...)
}

func assertInstalledExecutable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("installed path %s mode = %v, want executable regular file", path, info.Mode())
	}
}

func rewriteReleaseArchive(t *testing.T, path string, mutate func(string, []byte) []byte, addExtra bool) {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(source)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	tmp := path + ".tmp"
	target, err := os.Create(tmp)
	if err != nil {
		gz.Close()
		source.Close()
		t.Fatal(err)
	}
	newGzip := gzip.NewWriter(target)
	tw := tar.NewWriter(newGzip)
	var bundle string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if bundle == "" {
			bundle = strings.TrimSuffix(hdr.Name, "/")
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		body = mutate(hdr.Name, body)
		clone := *hdr
		clone.Size = int64(len(body))
		if err := tw.WriteHeader(&clone); err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if addExtra {
		body := []byte("unexpected\n")
		if err := tw.WriteHeader(&tar.Header{Name: bundle + "/unexpected", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []io.Closer{tw, newGzip, target, gz, source} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func rewriteChecksumEntry(t *testing.T, sumsPath, name, contentPath string) {
	t.Helper()
	digest := testSHA256(t, contentPath)
	body, err := os.ReadFile(sumsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	found := false
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			lines[i] = digest + "  " + name
			found = true
		}
	}
	if !found {
		t.Fatalf("checksum entry %s not found", name)
	}
	if err := os.WriteFile(sumsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func testSHA256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type interruptedInstallState struct {
	installDir     string
	home           string
	compoundPath   string
	breadthPath    string
	stateRoot      string
	transactionDir string
}

func interruptPreparedInstall(t *testing.T, dist, tarball string) interruptedInstallState {
	t.Helper()
	return interruptInstallAt(t, dist, tarball, "after_transaction_ready")
}

func interruptInstallAt(t *testing.T, dist, tarball, boundary string) interruptedInstallState {
	t.Helper()
	installDir := t.TempDir()
	home := t.TempDir()
	compoundPath := filepath.Join(installDir, "compound")
	breadthPath := filepath.Join(installDir, "compound-breadth-engine")
	if err := os.WriteFile(compoundPath, []byte("old compound\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(breadthPath, []byte("old breadth\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := runReleaseInstaller(filepath.Join(dist, "install-release.sh"), tarball, installDir, home, []string{
		"COMPOUND_INSTALL_CRASH_AT=" + boundary,
	})
	if err == nil {
		t.Fatalf("injected crash at %s unexpectedly completed:\n%s", boundary, output)
	}
	stateRoot := filepath.Join(installDir, ".compound-install-state")
	return interruptedInstallState{
		installDir:     installDir,
		home:           home,
		compoundPath:   compoundPath,
		breadthPath:    breadthPath,
		stateRoot:      stateRoot,
		transactionDir: filepath.Join(stateRoot, "transaction"),
	}
}

func replaceJournalLine(t *testing.T, state interruptedInstallState, prefix, replacement string) {
	t.Helper()
	replaceRecordLine(t, filepath.Join(state.transactionDir, "journal"), prefix, replacement)
}

func replaceRecordLine(t *testing.T, path, prefix, replacement string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	replaced := 0
	for index, line := range lines {
		if strings.HasPrefix(line, prefix) {
			lines[index] = replacement
			replaced++
		}
	}
	if replaced != 1 {
		t.Fatalf("journal prefix %q matched %d lines:\n%s", prefix, replaced, body)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyTestFile(source, target string, mode os.FileMode) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, body, mode)
}

func installSyncFailureShim(t *testing.T) string {
	t.Helper()
	fakeBin := t.TempDir()
	realSync, err := exec.LookPath("sync")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSync, filepath.Join(fakeBin, "real-sync")); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"if [ \"${COMPOUND_INSTALL_SYNC_PHASE:-}\" = \"${COMPOUND_TEST_SYNC_FAIL_PHASE:-}\" ]; then exit 74; fi\n" +
		"exec \"$(dirname \"$0\")/real-sync\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "sync"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeBin
}

func installBlockingSyncShim(t *testing.T) (fakeBin, entered, release string) {
	t.Helper()
	fakeBin = t.TempDir()
	realSync, err := exec.LookPath("sync")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSync, filepath.Join(fakeBin, "real-sync")); err != nil {
		t.Fatal(err)
	}
	entered = filepath.Join(t.TempDir(), "entered")
	release = filepath.Join(t.TempDir(), "release")
	body := "#!/bin/sh\n" +
		"if [ \"${COMPOUND_INSTALL_SYNC_PHASE:-}\" = \"${COMPOUND_TEST_SYNC_BLOCK_PHASE:-}\" ]; then\n" +
		"  : > \"$COMPOUND_TEST_SYNC_ENTERED\"\n" +
		"  while [ ! -f \"$COMPOUND_TEST_SYNC_RELEASE\" ]; do sleep 0.02; done\n" +
		"fi\n" +
		"exec \"$(dirname \"$0\")/real-sync\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "sync"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeBin, entered, release
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func snapshotInstallTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += "|link=" + target
		case info.Mode().IsRegular():
			value += "|sha256=" + testSHA256(t, path)
		}
		snapshot[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertInstallTreeSnapshot(t *testing.T, root string, want map[string]string) {
	t.Helper()
	got := snapshotInstallTree(t, root)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("install tree mutated after fail-closed refusal:\nwant: %#v\n got: %#v", want, got)
	}
}

func assertCleanInstallState(t *testing.T, installDir string) {
	t.Helper()
	stateRoot := filepath.Join(installDir, ".compound-install-state")
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "lock" || !entries[0].Type().IsRegular() {
		t.Fatalf("clean install state entries = %v, want persistent lock only", entries)
	}
}

func assertPreVerificationScratch(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("pre-verification scratch %s mode = %v, want regular non-symlink", path, info.Mode())
	}
	if info.Mode().Perm() == 0o755 {
		t.Fatalf("pre-verification scratch %s was already finalized mode 0755", path)
	}
}

func truncatePreVerificationScratch(t *testing.T, path string) {
	t.Helper()
	assertPreVerificationScratch(t, path)
	if err := os.Truncate(path, 1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1 {
		t.Fatalf("pre-verification scratch %s size = %d, want injected one-byte short write", path, info.Size())
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path %s exists after refused install: %v", path, err)
	}
}

func assertFileBody(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s body = %q, want %q", path, body, want)
	}
}
