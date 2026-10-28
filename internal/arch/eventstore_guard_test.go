package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventStoreClassificationIsSharedEnforcementLeaf(t *testing.T) {
	if got := Classification["eventstore"]; got != Leaf {
		t.Fatalf("eventstore classification = %q, want %q", got, Leaf)
	}
	if spineCore["eventstore"] {
		t.Fatal("eventstore is physical storage, not part of the envelope spine")
	}
	home, ok := mintHomes["organ:eventstore"]
	if !ok {
		t.Fatal("the existing mint doctor requires a two-class home for every new internal package")
	}
	if home.Class != MintEnforcement || !home.Valid() {
		t.Fatalf("eventstore mint home = %+v, want valid enforcement", home)
	}
}

func TestEventStoreOwnershipGuardRejectsProductionPhysicalAccess(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/caller/direct.go", `package caller

import (
	"os"
	fp "path/filepath"
)

func read(home string) ([]byte, error) {
	return os.ReadFile(fp.Join(home, "events", "rec_example.json"))
}

func scan(home string) ([]os.DirEntry, error) {
	return os.ReadDir(fp.Join(home, "events"))
}

func historicalRef(id string) string {
	return "events/" + id + ".json"
}

func concatenatedPath(home, id string) string {
	return home + "/events/" + id + ".json"
}`)
	mustWriteArchFixture(t, repo, "cmd/probe/streams.go", `package probe

import (
	"os"
	"path/filepath"
)

const workStream = "events/work.jsonl"

func openWork(home string) (*os.File, error) {
	return os.OpenFile(filepath.Join(home, filepath.FromSlash(workStream)), os.O_RDONLY, 0)
}`)
	mustWriteArchFixture(t, repo, "internal/reader/streams.go", `package reader

const (
	hookStream = "events/hook-events.ndjson"
	traceStream = "events/traces.ndjson"
)
`)
	mustWriteArchFixture(t, repo, "internal/reader/advertised.go", `package reader

var authorityPaths = []string{"ledger/claims.jsonl", "events/*.json"}
`)
	mustWriteArchFixture(t, repo, "internal/reader/formatted.go", `package reader

import "fmt"

func eventPath(id string) string {
	return fmt.Sprintf("%s/%s", "events", id+".json")
}
`)
	mustWriteArchFixture(t, repo, "internal/reader/retrieval.go", `package reader

const retrievalStream = "retrieval-traffic.ndjson"

func streamName() string { return retrievalStream }
`)
	mustWriteArchFixture(t, repo, "internal/reader/global.go", `package reader

import "path/filepath"

var eventRoot = filepath.Join(homePath, "events")
`)
	mustWriteArchFixture(t, repo, "internal/reader/global_read.go", `package reader

import (
	"os"
	"path/filepath"
)

var eventBytes, eventReadErr = os.ReadFile(filepath.Join(homePath, "events", "rec_example.json"))
`)
	mustWriteArchFixture(t, repo, "internal/act/raw_persistence_proof.go", `package act

import (
	"os"
	"path/filepath"
)

func persisted(home, id string) (bool, error) {
	_, err := os.Stat(filepath.Join(home, "events", id+".json"))
	return err == nil, err
}
`)
	mustWriteArchFixture(t, repo, "internal/reader/diagnostic.go", `package reader

import (
	"os"

	"compound/internal/eventstore"
)

func reopen(store *eventstore.Store, id string) ([]byte, error) {
	record, err := store.Read(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(record.DiagnosticPath)
}
`)

	findings := eventStoreOwnershipFindings(t, DoctorSource(repo, true))
	wantFiles := []string{
		"internal/caller/direct.go",
		"cmd/probe/streams.go",
		"internal/reader/streams.go",
		"internal/reader/advertised.go",
		"internal/reader/formatted.go",
		"internal/reader/retrieval.go",
		"internal/reader/global.go",
		"internal/reader/global_read.go",
		"internal/act/raw_persistence_proof.go",
		"internal/reader/diagnostic.go",
	}
	for _, want := range wantFiles {
		if !eventStoreFindingContains(findings, Fail, want) {
			t.Errorf("ownership guard did not reject %s: %+v", want, findings)
		}
	}
}

func TestEventStoreOwnershipGuardAllowsOwnerAndNonPhysicalVocabulary(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/eventstore/store.go", `package eventstore

import (
	"os"
	"path/filepath"
)

const workStream = "events/work.jsonl"

func read(home string) ([]byte, error) {
	return os.ReadFile(filepath.Join(home, "events", "rec_example.json"))
}`)
	mustWriteArchFixture(t, repo, "internal/caller/refs.go", `package caller

const (
	logicalRef = "compound://events/rec_example"
	externalSourceDir = "events"
)

type summary struct {
	Events int `+"`json:\"events\"`"+`
}
`)
	mustWriteArchFixture(t, repo, "internal/home/catalog.go", `package home

var habitatVocabulary = []string{"events", "events/logs"}
`)
	mustWriteArchFixture(t, repo, "cmd/probe/probe_test.go", `package probe

import "path/filepath"

var fixture = filepath.Join("fixture", "events", "rec_example.json")
`)

	findings := eventStoreOwnershipFindings(t, DoctorSource(repo, true))
	for _, finding := range findings {
		if finding.Severity == Fail {
			t.Fatalf("owner/non-physical vocabulary must pass: %+v", findings)
		}
	}
	if !eventStoreFindingContains(findings, Info, "sole physical owner") {
		t.Fatalf("clean scan should report its ownership proof: %+v", findings)
	}
}

func TestEventStoreOwnershipGuardRejectsNestedOwnerPackage(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/eventstore/nested/bypass.go", `package nested

import (
	"os"
	"path/filepath"
)

func read(home string) ([]byte, error) {
	return os.ReadFile(filepath.Join(home, "events", "rec_nested.json"))
}
`)

	findings := eventStoreOwnershipFindings(t, DoctorSource(repo, true))
	if !eventStoreFindingContains(findings, Fail, "internal/eventstore/nested/bypass.go") {
		t.Fatalf("nested package bypassed exact internal/eventstore ownership: %+v", findings)
	}
}

func TestEventStoreOwnershipGuardConstantFoldsSplitStringConstants(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/reader/split_constant.go", `package reader

import (
	"os"
	"path/filepath"
)

const eventDir = "ev" + "ents"

func open(home string) (*os.File, error) {
	return os.Open(filepath.Join(home, eventDir, "rec_split.json"))
}
`)
	mustWriteArchFixture(t, repo, "internal/chainedreader/chained_constant.go", `package chainedreader

import (
	"os"
	"path/filepath"
)

const eventPrefix = "ev"
const eventDir = eventPrefix + "ents"

func open(home string) (*os.File, error) {
	return os.Open(filepath.Join(home, eventDir, "rec_chained.json"))
}
`)

	findings := eventStoreOwnershipFindings(t, DoctorSource(repo, true))
	for _, rel := range []string{
		"internal/reader/split_constant.go",
		"internal/chainedreader/chained_constant.go",
	} {
		if !eventStoreFindingContains(findings, Fail, rel) {
			t.Errorf("split string constant in %s bypassed eventstore ownership: %+v", rel, findings)
		}
	}
}

func TestEventStoreOwnershipGuardDetectsUntrackedScratchProductionFile(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	mustWriteArchFixture(t, repo, "cmd/probe/zz_eventstore_guard_probe.go", `package probe

import (
	"os"
	"path/filepath"
)

func scratchRead(home string) ([]byte, error) {
	return os.ReadFile(filepath.Join(home, "events", "scratch.json"))
}`)

	tracked := exec.Command("git", "-C", repo, "ls-files", "--error-unmatch", "cmd/probe/zz_eventstore_guard_probe.go")
	if err := tracked.Run(); err == nil {
		t.Fatal("scratch probe unexpectedly became tracked; test would not prove worktree scanning")
	}

	findings := eventStoreOwnershipFindings(t, DoctorSource(repo, true))
	if !eventStoreFindingContains(findings, Fail, "zz_eventstore_guard_probe.go") {
		t.Fatalf("untracked scratch production access must fail: %+v", findings)
	}
}

func TestEventStorePublicArchitectureContract(t *testing.T) {
	required := map[string][]string{
		"docs/architecture/runtime.md": {
			"`internal/eventstore` is the sole physical-layout owner",
			"`compound://events/<id>`",
			"`events/<id>.json`",
			"`events/work.jsonl`",
			"`events/hook-events.ndjson`",
			"`events/traces.ndjson`",
		},
		"docs/architecture/status.md": {
			"| `eventstore` | leaf |",
			"sole physical-layout owner",
		},
		"docs/architecture/contracts/README.md": {
			"Event-reference contract",
			"bare IDs",
			"`compound://events/<id>`",
			"historical `events/<id>.json`",
			"segment/index paths",
		},
	}
	for rel, phrases := range required {
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, phrase := range phrases {
			if !strings.Contains(string(body), phrase) {
				t.Errorf("%s missing public event-store contract phrase %q", rel, phrase)
			}
		}
	}
	var doctorProof bool
	for _, finding := range DoctorSource(repoRoot, true).Findings {
		if finding.Check == "eventstore-public-contract" && finding.Severity == Info {
			doctorProof = true
		}
	}
	if !doctorProof {
		t.Fatal("strict architecture doctor did not enforce the public event-store contract")
	}
}

func eventStoreOwnershipFindings(t *testing.T, report Report) []Finding {
	t.Helper()
	var findings []Finding
	for _, finding := range report.Findings {
		if finding.Check == "eventstore-ownership" {
			findings = append(findings, finding)
		}
	}
	if len(findings) == 0 {
		t.Fatal("architecture doctor did not run the eventstore-ownership check")
	}
	return findings
}

func eventStoreFindingContains(findings []Finding, severity Severity, fragment string) bool {
	for _, finding := range findings {
		if finding.Severity == severity && strings.Contains(finding.Message, fragment) {
			return true
		}
	}
	return false
}
