package home

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadRootedRegularFileCanonicalOnlyRejectsEveryRemovalResidueWithoutMutation(t *testing.T) {
	tests := []struct {
		name      string
		shape     func(t *testing.T, path string)
		wantBody  []byte
		wantFound bool
		wantErr   bool
	}{
		{name: "absent"},
		{
			name: "canonical only",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("canonical\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantBody:  []byte("canonical\n"),
			wantFound: true,
		},
		{
			name: "quarantine only",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path+rootedRemovalPendingSuffix, []byte("owned\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "canonical and same inode alias",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("owned\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "canonical and gapped repeated same inode alias",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("owned\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "canonical and contiguous repeated same inode aliases",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("owned\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "quarantine only contiguous repeated same inode aliases",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("owned\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+rootedRemovalPendingSuffix+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "split inodes",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("canonical\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+rootedRemovalPendingSuffix, []byte("foreign\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "symlink residue",
			shape: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("outside\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path+rootedRemovalPendingSuffix); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "nonregular residue",
			shape: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path+rootedRemovalPendingSuffix, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			path := filepath.Join(homePath, "artifact.json")
			if tc.shape != nil {
				tc.shape(t, path)
			}
			before := rootedCanonicalOnlyDirectorySnapshot(t, homePath)

			body, present, err := ReadRootedRegularFileCanonicalOnly(homePath, path, "test artifact")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("canonical-only read = body=%q present=%v, want error", body, present)
				}
			} else {
				if err != nil || present != tc.wantFound || !bytes.Equal(body, tc.wantBody) {
					t.Fatalf("canonical-only read = body=%q present=%v err=%v", body, present, err)
				}
			}
			after := rootedCanonicalOnlyDirectorySnapshot(t, homePath)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("canonical-only read mutated custody:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func rootedCanonicalOnlyDirectorySnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			value += ":" + target
		case info.Mode().IsRegular():
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			value += ":" + string(body)
		}
		snapshot[entry.Name()] = value
	}
	return snapshot
}
