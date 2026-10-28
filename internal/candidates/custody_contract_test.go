package candidates

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type custodyContractRecord struct {
	ID     string `json:"id,omitempty"`
	Marker string `json:"marker,omitempty"`
}

func writeCustodyContractFile(t *testing.T, homePath, name, body string) string {
	t.Helper()
	dir := filepath.Join(homePath, InboxDir(Memory))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCandidateCustodyRejectsNonCanonicalPointReads(t *testing.T) {
	homePath := t.TempDir()
	writeCustodyContractFile(t, homePath, "blank.json", `{"id":"","marker":"blank"}`)
	writeCustodyContractFile(t, homePath, "alias.json", `{"id":"victim","marker":"alias"}`)
	writeCustodyContractFile(t, homePath, "inside-target", `{"id":"inside-link","marker":"inside"}`)
	if err := os.Symlink("inside-target", filepath.Join(homePath, InboxDir(Memory), "inside-link.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"id":"outside-link","marker":"outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(homePath, InboxDir(Memory), "outside-link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(homePath, InboxDir(Memory), "directory.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"blank", "alias", "inside-link", "outside-link", "directory"} {
		t.Run(id, func(t *testing.T) {
			var got custodyContractRecord
			if err := ReadCandidate(homePath, Memory, id, &got); err == nil {
				t.Fatalf("ReadCandidate(%q) accepted noncanonical custody: %+v", id, got)
			}
		})
	}

	writeCustodyContractFile(t, homePath, "hardlink.json", `{"id":"hardlink","marker":"canonical"}`)
	if err := os.Link(
		filepath.Join(homePath, InboxDir(Memory), "hardlink.json"),
		filepath.Join(homePath, InboxDir(Memory), "hardlink-backup"),
	); err != nil {
		t.Fatal(err)
	}
	var got custodyContractRecord
	if err := ReadCandidate(homePath, Memory, "hardlink", &got); err != nil || got.ID != "hardlink" {
		t.Fatalf("canonical hard-linked regular file rejected: %+v err=%v", got, err)
	}
}

func TestCandidateCustodyRejectsNonCanonicalWriterIdentity(t *testing.T) {
	for _, writer := range []struct {
		name string
		run  func(string, string, custodyContractRecord) error
	}{
		{name: "WriteCandidate", run: func(homePath, id string, payload custodyContractRecord) error {
			return WriteCandidate(homePath, Memory, id, payload)
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath, id string, payload custodyContractRecord) error {
			_, err := CreateCandidateIfAbsent(homePath, Memory, id, payload)
			return err
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := writer.run(homePath, "blank-payload", custodyContractRecord{}); err == nil {
				t.Fatal("blank payload id must be rejected")
			}
			if err := writer.run(homePath, " spaced ", custodyContractRecord{ID: " spaced "}); err == nil {
				t.Fatal("whitespace-padded candidate identity must be rejected")
			}
		})
	}

	homePath := t.TempDir()
	for _, id := range []string{"../escape", filepath.Join(homePath, "absolute")} {
		if err := ReadCandidate(homePath, Memory, id, &custodyContractRecord{}); err == nil {
			t.Fatalf("path-shaped read id %q must be rejected", id)
		}
	}
}

func TestCandidateOperationsRejectAliasedCustodyRoots(t *testing.T) {
	operations := []struct {
		name string
		run  func(string) error
	}{
		{name: "ReadCandidate", run: func(homePath string) error {
			return ReadCandidate(homePath, Memory, "custody-probe", &custodyContractRecord{})
		}},
		{name: "WriteCandidate", run: func(homePath string) error {
			return WriteCandidate(homePath, Memory, "custody-probe", custodyContractRecord{ID: "custody-probe", Marker: "replacement"})
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath string) error {
			_, err := CreateCandidateIfAbsent(homePath, Memory, "custody-probe", custodyContractRecord{ID: "custody-probe", Marker: "contender"})
			return err
		}},
	}

	rootAliases := []struct {
		name  string
		setup func(*testing.T, string, string)
	}{
		{name: "symlinked inbox", setup: func(t *testing.T, visibleHome, authoritativeHome string) {
			link := filepath.Join(visibleHome, InboxDir(Memory))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(authoritativeHome, InboxDir(Memory)), link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlinked home", setup: func(t *testing.T, visibleHome, authoritativeHome string) {
			if err := os.Remove(visibleHome); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(authoritativeHome, visibleHome); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, rootAlias := range rootAliases {
		for _, operation := range operations {
			t.Run(rootAlias.name+"/"+operation.name, func(t *testing.T) {
				visibleHome := filepath.Join(t.TempDir(), "visible-home")
				if err := os.Mkdir(visibleHome, 0o755); err != nil {
					t.Fatal(err)
				}
				canonicalHome := t.TempDir()
				target := writeCustodyContractFile(t, canonicalHome, "custody-probe.json", `{"id":"custody-probe","marker":"original"}`)
				want, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				rootAlias.setup(t, visibleHome, canonicalHome)

				if err := operation.run(visibleHome); err == nil {
					t.Fatalf("%s accepted %s", operation.name, rootAlias.name)
				}
				got, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s mutated aliased custody: got %q want %q", operation.name, got, want)
				}
			})
		}
	}
}

func TestCandidateCanonicalFilenameMatchesJSONIdentity(t *testing.T) {
	homePath := t.TempDir()
	for _, tc := range []struct {
		filename string
		storedID string
	}{
		{filename: "blank.json", storedID: ""},
		{filename: "mismatch.json", storedID: "other"},
		{filename: " spaced .json", storedID: " spaced "},
	} {
		writeCustodyContractFile(t, homePath, tc.filename, fmt.Sprintf(`{"id":%q}`, tc.storedID))
		id := tc.filename[:len(tc.filename)-len(".json")]
		if err := ReadCandidate(homePath, Memory, id, &custodyContractRecord{}); err == nil {
			t.Fatalf("candidate filename=%q stored_id=%q was actionable", tc.filename, tc.storedID)
		}
	}
}
