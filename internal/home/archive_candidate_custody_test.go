package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/candidates"
)

func writeArchiveCandidateCustodyBody(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf("{\"id\":%q,\"state\":\"promoted\"}\n", id))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOperationalArchiveDerivesNoActionFromCorruptMemoryCandidateCustody(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		mode := "apply"
		if dryRun {
			mode = "dry-run"
		}
		for _, tc := range []struct {
			name  string
			setup func(*testing.T, string) string
		}{
			{name: "filename body alias", setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "alias.json")
				writeArchiveCandidateCustodyBody(t, path, "victim")
				return path
			}},
			{name: "symlink", setup: func(t *testing.T, dir string) string {
				target := filepath.Join(t.TempDir(), "target.json")
				writeArchiveCandidateCustodyBody(t, target, "linked")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "linked.json")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return path
			}},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				homePath := filepath.Join(t.TempDir(), ".compound")
				if err := Init(homePath, "test", archiveTestNow); err != nil {
					t.Fatal(err)
				}
				path := tc.setup(t, filepath.Join(homePath, candidates.InboxDir(candidates.Memory)))
				report, err := ArchiveOperationalHistory(homePath, OperationalArchiveOptions{Kinds: []string{"memory-candidate"}, DryRun: dryRun}, archiveTestNow)
				if err == nil {
					t.Fatalf("derived archive action from corrupt custody: %+v", report)
				}
				var corruption *candidates.Corruption
				if !errors.As(err, &corruption) {
					t.Fatalf("error = %v, want named candidate corruption", err)
				}
				if report.ArchivedCount != 0 || len(report.Entries) != 0 {
					t.Fatalf("partial archive actions escaped corruption: %+v", report)
				}
				if _, statErr := os.Lstat(path); statErr != nil {
					t.Fatalf("corrupt candidate custody changed during refusal: %v", statErr)
				}
				active, indexErr := LoadOperationalArchiveIndex(homePath)
				if indexErr != nil {
					t.Fatal(indexErr)
				}
				if len(active) != 0 {
					t.Fatalf("archive index recorded corrupt custody: %+v", active)
				}
			})
		}
	}
}
