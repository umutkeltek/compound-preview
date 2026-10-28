package panel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/candidates"
)

func writePanelCustodyBody(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf("{\"id\":%q,\"entity\":\"entity:test\",\"predicate\":\"custody\",\"object\":\"safe\",\"state\":\"proposed\",\"visibility\":\"candidate\"}\n", id))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func requirePanelCandidateCorruption(t *testing.T, err error) {
	t.Helper()
	var corruption *candidates.Corruption
	if !errors.As(err, &corruption) {
		t.Fatalf("error = %v, want named candidate corruption", err)
	}
}

func TestLoadTargetInputRejectsCorruptMemoryCandidateCustody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
		ref   string
	}{
		{name: "filename body alias", ref: "alias", setup: func(t *testing.T, dir string) {
			writePanelCustodyBody(t, filepath.Join(dir, "alias.json"), "victim")
		}},
		{name: "symlink", ref: "linked", setup: func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "target.json")
			writePanelCustodyBody(t, target, "linked")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, candidates.InboxDir(candidates.Memory))
			tc.setup(t, dir)
			target, err := LoadTargetInput(home, "memory_candidate", tc.ref)
			if err == nil {
				t.Fatalf("derived target from corrupt custody: %+v", target)
			}
			requirePanelCandidateCorruption(t, err)
		})
	}
}

func TestLoadMemoryCandidateTargetsDerivesNoTargetFromCorruptCustody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "filename body alias", setup: func(t *testing.T, dir string) {
			writePanelCustodyBody(t, filepath.Join(dir, "alias.json"), "victim")
		}},
		{name: "symlink", setup: func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "target.json")
			writePanelCustodyBody(t, target, "linked")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, candidates.InboxDir(candidates.Memory))
			tc.setup(t, dir)
			targets, err := LoadMemoryCandidateTargets(home, nil, 0)
			if err == nil {
				t.Fatalf("derived targets from corrupt custody: %+v", targets)
			}
			if len(targets) != 0 {
				t.Fatalf("partial targets escaped corruption: %+v", targets)
			}
			requirePanelCandidateCorruption(t, err)
		})
	}
}
