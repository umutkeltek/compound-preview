package home

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveBudgetDefaultsRemainUnchanged(t *testing.T) {
	want := archiveReadLimits{MaxManifestBytes: 64 << 20, MaxSourceBytes: 64 << 30, MaxEntries: 250_000, MaxPathBytes: 4 << 10, MaxPathDepth: 64, MaxEntryBytes: 8 << 30, MaxExpandedBytes: 64 << 30, MaxDecodedBytes: 72 << 30}
	if defaultArchiveReadLimits != want {
		t.Fatalf("resource envelope changed: %+v", defaultArchiveReadLimits)
	}
}

func TestArchiveWriterReaderBudgetBoundaries(t *testing.T) {
	for _, kind := range []string{"backup", "public_fixture"} {
		t.Run(kind, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "home")
			if err := Init(h, "test", archiveTestNow); err != nil {
				t.Fatal(err)
			}
			// Long name requires PAX metadata; JSON escaping makes an estimate
			// from unescaped path lengths insufficient for the manifest cap.
			name := strings.Repeat("x", 110) + " & <evidence>.json"
			if err := os.WriteFile(filepath.Join(h, "ledger", name), []byte("synthetic evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			write := func(target string) (ArchiveReport, error) {
				if kind == "backup" {
					return Backup(h, target, archiveTestNow)
				}
				return ExportPublicFixture(h, target, archiveTestNow)
			}
			baseline := filepath.Join(t.TempDir(), "baseline.tar.gz")
			report, err := write(baseline)
			if err != nil {
				t.Fatal(err)
			}
			baselineBody, err := os.ReadFile(baseline)
			if err != nil {
				t.Fatal(err)
			}
			limits := archiveFixtureExactBudget(t, baselineBody)
			if limits.MaxEntries <= report.Files+1 {
				t.Fatalf("fixture does not distinguish files, directories and manifest: %+v %+v", limits, report)
			}
			prior := defaultArchiveReadLimits
			t.Cleanup(func() { defaultArchiveReadLimits = prior })
			defaultArchiveReadLimits = limits
			exact := filepath.Join(t.TempDir(), "exact.tar.gz")
			if _, err := write(exact); err != nil {
				t.Fatalf("exact writer budget refused: %v", err)
			}
			if _, err := VerifyArchive(exact); err != nil {
				t.Fatalf("exact reader budget refused: %v", err)
			}
			exactBody, err := os.ReadFile(exact)
			if err != nil || !bytes.Equal(exactBody, baselineBody) {
				t.Fatalf("budget changed archive bytes: %v", err)
			}
			if kind == "backup" {
				if _, err := RestoreArchive(exact, filepath.Join(t.TempDir(), "restored"), archiveTestNow); err != nil {
					t.Fatalf("exact restore budget refused: %v", err)
				}
			}
			for _, tc := range []struct {
				name  string
				lower func(*archiveReadLimits)
				match string
			}{
				{"logical_entries", func(l *archiveReadLimits) { l.MaxEntries-- }, "archive entry count exceeds"},
				{"manifest_bytes", func(l *archiveReadLimits) { l.MaxManifestBytes-- }, "archive manifest exceeds"},
				{"entry_bytes", func(l *archiveReadLimits) { l.MaxEntryBytes-- }, "archive entry "},
				{"expanded_bytes", func(l *archiveReadLimits) { l.MaxExpandedBytes-- }, "archive expanded bytes exceed"},
				{"decoded_bytes", func(l *archiveReadLimits) { l.MaxDecodedBytes-- }, "decoded archive exceeds"},
				{"source_bytes", func(l *archiveReadLimits) { l.MaxSourceBytes-- }, "archive source exceeds"},
				{"path_bytes", func(l *archiveReadLimits) { l.MaxPathBytes-- }, "archive path exceeds"},
				{"path_depth", func(l *archiveReadLimits) { l.MaxPathDepth-- }, "archive path depth exceeds"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					defaultArchiveReadLimits = limits
					tc.lower(&defaultArchiveReadLimits)
					t.Cleanup(func() { defaultArchiveReadLimits = limits })
					// The existing baseline must fail the same consumer dimension.
					if _, err := VerifyArchive(baseline); err == nil || !strings.Contains(err.Error(), tc.match) {
						t.Fatalf("reader did not reject the intended budget: %v", err)
					}
					target := filepath.Join(t.TempDir(), "refused.tar.gz")
					if _, err := write(target); err == nil || !strings.Contains(err.Error(), tc.match) {
						t.Fatalf("writer did not reject the same budget: %v", err)
					}
					if files, err := os.ReadDir(filepath.Dir(target)); err != nil || len(files) != 0 {
						t.Fatalf("budget refusal retained output: %v %v", files, err)
					}
					if got, err := os.ReadFile(baseline); err != nil || !bytes.Equal(got, baselineBody) {
						t.Fatalf("refusal changed retained source: %v", err)
					}
				})
			}
		})
	}
}

func archiveFixtureExactBudget(t *testing.T, body []byte) archiveReadLimits {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	limits := archiveReadLimits{MaxSourceBytes: int64(len(body)), MaxDecodedBytes: int64(len(decoded))}
	tr := tar.NewReader(bytes.NewReader(decoded))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		limits.MaxEntries++
		limits.MaxEntryBytes = max(limits.MaxEntryBytes, hdr.Size)
		limits.MaxExpandedBytes += hdr.Size
		limits.MaxPathBytes = max(limits.MaxPathBytes, len(hdr.Name))
		limits.MaxPathDepth = max(limits.MaxPathDepth, len(strings.Split(strings.TrimSuffix(hdr.Name, "/"), "/")))
		if hdr.Name == ArchiveManifestName {
			limits.MaxManifestBytes = hdr.Size
		}
	}
	if err := validateArchiveReadLimits(limits); err != nil {
		t.Fatal(fmt.Errorf("fixture budget: %w", err))
	}
	return limits
}

func TestBackupRefusesReaderBudgetBeforePublication(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := Init(h, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "ledger", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "ledger", "nested", "evidence.json"), []byte("synthetic evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := filepath.Join(t.TempDir(), "baseline.tar.gz")
	if _, err := Backup(h, baseline, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(baseline); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		limit func(*archiveReadLimits)
		match string
	}{
		{"logical_entries", func(l *archiveReadLimits) { l.MaxEntries = 1 }, "entry count exceeds 1"},
		{"path_bytes", func(l *archiveReadLimits) { l.MaxPathBytes = 1 }, "path exceeds 1 bytes"},
		{"path_depth", func(l *archiveReadLimits) { l.MaxPathDepth = 1 }, "path depth exceeds 1"},
		{"entry_bytes", func(l *archiveReadLimits) { l.MaxEntryBytes = 1 }, "exceeds 1 bytes"},
		{"expanded_bytes", func(l *archiveReadLimits) { l.MaxEntryBytes = 1; l.MaxExpandedBytes = 1 }, "exceeds 1 bytes"},
		{"decoded_bytes", func(l *archiveReadLimits) { l.MaxEntryBytes = 1; l.MaxExpandedBytes = 1; l.MaxDecodedBytes = 1 }, "exceeds 1 bytes"},
		{"source_bytes", func(l *archiveReadLimits) { l.MaxSourceBytes = 1 }, "exceeds 1 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := defaultArchiveReadLimits
			t.Cleanup(func() { defaultArchiveReadLimits = prior })
			tc.limit(&defaultArchiveReadLimits)
			target := filepath.Join(t.TempDir(), "refused.tar.gz")
			_, err := Backup(h, target, archiveTestNow)
			if err == nil {
				_, verifyErr := VerifyArchive(target)
				t.Fatalf("backup published outside the reader budget: verify error = %v", verifyErr)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("backup error = %v, want %q", err, tc.match)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("refused archive was published: %v", err)
			}
			files, err := os.ReadDir(filepath.Dir(target))
			if err != nil || len(files) != 0 {
				t.Fatalf("refused archive left temporary output: %v %v", files, err)
			}
		})
	}
}

func TestRestoreFinalValidationRetainsAdmittedBudget(t *testing.T) {
	archive := backupArchiveFixture(t)
	limits := defaultArchiveReadLimits
	oldHook := restoreBeforePublish
	oldLimits := defaultArchiveReadLimits
	t.Cleanup(func() { restoreBeforePublish = oldHook; defaultArchiveReadLimits = oldLimits })
	restoreBeforePublish = func(string, string) error {
		defaultArchiveReadLimits.MaxEntries = 1
		return nil
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := restoreArchiveWithLimits(archive, target, archiveTestNow, limits); err != nil {
		t.Fatalf("final validation discarded the operation's admitted budget: %v", err)
	}
}
