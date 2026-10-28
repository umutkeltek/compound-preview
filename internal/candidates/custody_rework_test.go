package candidates

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type custodyReworkPayload struct {
	ID     string `json:"id"`
	Kind   string `json:"kind,omitempty"`
	Marker string `json:"marker,omitempty"`
}

type custodyReworkOperation struct {
	name string
	run  func(string, string) error
}

func custodyReworkOperations() []custodyReworkOperation {
	return []custodyReworkOperation{
		{name: "ReadRecord", run: func(homePath, id string) error {
			_, err := ReadRecord(homePath, Memory, id)
			return err
		}},
		{name: "ReadPage", run: func(homePath, _ string) error {
			_, err := ReadPage(homePath, Memory, "", 0)
			return err
		}},
		{name: "WriteCandidate", run: func(homePath, id string) error {
			return WriteCandidate(homePath, Memory, id, custodyReworkPayload{ID: id, Marker: "replacement"})
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath, id string) error {
			_, err := CreateCandidateIfAbsent(homePath, Memory, id, custodyReworkPayload{ID: id, Marker: "contender"})
			return err
		}},
	}
}

func seedCustodyReworkCandidate(t *testing.T, homePath, id string) (string, []byte) {
	t.Helper()
	path := filepath.Join(homePath, InboxDir(Memory), id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf("{\"id\":%q,\"marker\":\"original\"}\n", id))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, body
}

func assertCustodyOperationRejected(t *testing.T, op custodyReworkOperation, visibleHome, authoritativePath string, want []byte) {
	t.Helper()
	if err := op.run(visibleHome, "custody-probe"); err == nil {
		t.Fatalf("%s accepted aliased candidate custody", op.name)
	}
	got, err := os.ReadFile(authoritativePath)
	if err != nil {
		t.Fatalf("read authoritative candidate after %s refusal: %v", op.name, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mutated aliased custody: got %q want %q", op.name, got, want)
	}
}

// A swapped home ancestor is the rename-then-symlink version of an aliased
// root: the path string still resolves, but a component beneath the visible
// home was displaced and replaced by a link to foreign custody. Every
// operation must refuse through the descriptor-rooted walk, and the foreign
// candidate must stay byte-identical. (Plain symlinked inbox/home aliases are
// pinned by TestCandidateOperationsRejectAliasedCustodyRoots.)
func TestCandidateOperationsRejectSwappedHomeAncestor(t *testing.T) {
	for _, op := range custodyReworkOperations() {
		t.Run(op.name, func(t *testing.T) {
			container := t.TempDir()
			anchor := filepath.Join(container, "anchor")
			visibleHome := filepath.Join(anchor, "compound-home")
			if err := os.MkdirAll(visibleHome, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(anchor, filepath.Join(container, "anchor-displaced")); err != nil {
				t.Fatal(err)
			}
			replacementAnchor := t.TempDir()
			target, want := seedCustodyReworkCandidate(t, filepath.Join(replacementAnchor, "compound-home"), "custody-probe")
			if err := os.Symlink(replacementAnchor, anchor); err != nil {
				t.Fatal(err)
			}
			assertCustodyOperationRejected(t, op, visibleHome, target, want)
		})
	}
}

// A point read must match the exact filename bytes the directory returned, not
// a filesystem-alias of them (case-folding or unicode-normalization aliases on
// APFS must never make a differently-named file actionable under a requested
// id).
func TestCandidatePointReadRequiresExactFilenameBytes(t *testing.T) {
	tests := []struct {
		name      string
		storedID  string
		requestID string
	}{
		{name: "case alias", storedID: "CUSTODY-ALIAS", requestID: "custody-alias"},
		{name: "unicode normalization alias", storedID: "café", requestID: "café"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, InboxDir(Memory))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			storedPath := filepath.Join(dir, tc.storedID+".json")
			if err := os.WriteFile(storedPath, []byte(fmt.Sprintf("{\"id\":%q}\n", tc.requestID)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRecord(home, Memory, tc.requestID); err == nil {
				t.Fatalf("point read accepted filename alias %q for requested %q", tc.storedID, tc.requestID)
			}
		})
	}
}

// The exact-entry check consults the directory's returned names: if the
// directory only ever returned differently-cased bytes, the read must refuse
// even when the filesystem would resolve the requested spelling.
func TestCandidatePointReadConsultsExactDirectoryEntryBytes(t *testing.T) {
	home := t.TempDir()
	seedCustodyReworkCandidate(t, home, "custody-exact")
	previous := candidateReadDirectoryNames
	candidateReadDirectoryNames = func(*candidateDirectory) ([]string, error) {
		return []string{"CUSTODY-EXACT.json"}, nil
	}
	t.Cleanup(func() { candidateReadDirectoryNames = previous })
	if _, err := ReadRecord(home, Memory, "custody-exact"); err == nil {
		t.Fatal("point read ignored exact directory-entry bytes")
	}
}
