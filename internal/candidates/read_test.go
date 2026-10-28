package candidates

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCandidateMissingInboxPreservesNotExistWithoutScaffolding(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	var got struct {
		ID string `json:"id"`
	}

	err := ReadCandidate(home, Memory, "missing", &got)
	if !os.IsNotExist(err) {
		t.Fatalf("missing inbox: want IsNotExist, got %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(home, filepath.Dir(InboxDir(Memory)))); !os.IsNotExist(statErr) {
		t.Fatalf("read scaffolded candidate custody: %v", statErr)
	}
}

// Depth probe for the one candidate-inbox reader: it must enforce the id-match
// contract, reject an empty stored id, surface a missing file as
// os.IsNotExist (multi-inbox callers depend on it), and reject an unknown kind
// before touching disk. Fails if any reader's guard semantics regress.
func TestReadCandidate(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, InboxDir(Memory))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type rec struct {
		ID     string `json:"id"`
		Entity string `json:"entity"`
	}

	write("c1.json", `{"id":"c1","entity":"x"}`)
	var c rec
	if err := ReadCandidate(home, Memory, "c1", &c); err != nil || c.Entity != "x" {
		t.Fatalf("matching id: %+v err=%v", c, err)
	}

	// A stored id is custody identity, not an optional caller default.
	write("c2.json", `{"entity":"y"}`)
	c = rec{}
	if err := ReadCandidate(home, Memory, "c2", &c); err == nil {
		t.Fatalf("empty stored id was actionable: %+v", c)
	}

	// A non-empty mismatched id is the contract violation.
	write("c3.json", `{"id":"other","entity":"z"}`)
	if err := ReadCandidate(home, Memory, "c3", &rec{}); err == nil {
		t.Fatal("mismatched id must error")
	}

	// A missing file is an os.IsNotExist error (multi-inbox callers continue on it).
	if err := ReadCandidate(home, Memory, "nope", &rec{}); !os.IsNotExist(err) {
		t.Fatalf("missing file: want IsNotExist, got %v", err)
	}

	// An unknown kind errors before touching disk.
	if err := ReadCandidate(home, Kind("bogus-kind"), "x", &rec{}); err == nil {
		t.Fatal("unknown kind must error")
	}
}

// Depth probe for the candidate-inbox writer: it shares one path + serialization
// with ReadCandidate (round-trips), writes the canonical two-space JSON + newline,
// and rejects an unknown kind before touching disk.
func TestWriteCandidate(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID     string `json:"id"`
		Entity string `json:"entity"`
	}
	if err := WriteCandidate(home, Memory, "c1", rec{ID: "c1", Entity: "x"}); err != nil {
		t.Fatal(err)
	}
	var got rec
	if err := ReadCandidate(home, Memory, "c1", &got); err != nil || got.Entity != "x" {
		t.Fatalf("write→read round-trip: %+v err=%v", got, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, InboxDir(Memory), "c1.json"))
	if !strings.HasSuffix(string(b), "}\n") || !strings.Contains(string(b), "\n  \"id\":") {
		t.Fatalf("serialization not canonical (two-space + trailing newline): %q", string(b))
	}
	if err := WriteCandidate(home, Kind("bogus-kind"), "x", rec{}); err == nil {
		t.Fatal("unknown kind must error before write")
	}
}

func TestReplaceCandidateIfCurrentUsesTargetGenerationNotDirectoryTimestamp(t *testing.T) {
	type rec struct {
		ID     string `json:"id"`
		Marker string `json:"marker"`
	}
	home := t.TempDir()
	if err := WriteCandidate(home, Memory, "c1", rec{ID: "c1", Marker: "before"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(home, Memory, "c2", rec{ID: "c2", Marker: "sibling-before"}); err != nil {
		t.Fatal(err)
	}
	page, err := ReadPage(home, Memory, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var expected Record
	for _, entry := range page.Entries {
		if entry.Record != nil && entry.Record.ID == "c1" {
			expected = *entry.Record
		}
	}
	if expected.ID == "" {
		t.Fatal("page did not return c1 record")
	}

	// A sibling publication changes directory timestamps but not c1's exact
	// file generation; c1's CAS must remain eligible.
	if err := WriteCandidate(home, Memory, "c2", rec{ID: "c2", Marker: "sibling-after"}); err != nil {
		t.Fatal(err)
	}
	replaceResult, err := ReplaceCandidateIfCurrent(home, expected, rec{ID: "c1", Marker: "after"})
	if err != nil || !replaceResult.Replaced {
		t.Fatalf("replace after sibling mutation: result=%+v err=%v", replaceResult, err)
	}

	// The same pre-state cannot be applied twice: c1 now has a different file
	// generation, so retry is an idempotent CAS miss and preserves the winner.
	replaceResult, err = ReplaceCandidateIfCurrent(home, expected, rec{ID: "c1", Marker: "stale-overwrite"})
	if err != nil {
		t.Fatalf("stale replacement: %v", err)
	}
	if replaceResult.Replaced {
		t.Fatal("stale candidate generation was replaced")
	}
	if replaceResult.Current.ID != "c1" {
		t.Fatalf("stale CAS did not return the exact lock winner: %+v", replaceResult.Current)
	}
	var got rec
	if err := ReadCandidate(home, Memory, "c1", &got); err != nil {
		t.Fatal(err)
	}
	if got.Marker != "after" {
		t.Fatalf("c1 marker = %q, want prior CAS winner", got.Marker)
	}
}

func TestMutateCandidateIfDigestsBindsTargetAndCompanionUnderOneLock(t *testing.T) {
	type rec struct {
		ID     string `json:"id"`
		Marker string `json:"marker"`
	}
	home := t.TempDir()
	for _, value := range []rec{{ID: "target", Marker: "before"}, {ID: "keeper", Marker: "before"}} {
		if err := WriteCandidate(home, Memory, value.ID, value); err != nil {
			t.Fatal(err)
		}
	}
	target, err := ReadRecord(home, Memory, "target")
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := ReadRecord(home, Memory, "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(home, Memory, "keeper", rec{ID: "keeper", Marker: "changed"}); err != nil {
		t.Fatal(err)
	}

	called := false
	result, err := MutateCandidateIfDigests(home, Memory, "target", []DigestPrecondition{
		{ID: "target", Digest: target.Digest},
		{ID: "keeper", Digest: keeper.Digest},
	}, func(Record) (any, error) {
		called = true
		return rec{ID: "target", Marker: "after"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.MismatchID != "keeper" || called {
		t.Fatalf("companion drift crossed mutation boundary: result=%+v called=%v", result, called)
	}
	var got rec
	if err := ReadCandidate(home, Memory, "target", &got); err != nil {
		t.Fatal(err)
	}
	if got.Marker != "before" {
		t.Fatalf("target changed after companion drift: %+v", got)
	}
}

func TestPageCASDoesNotRescanDirectoryPerCandidate(t *testing.T) {
	type rec struct {
		ID     string `json:"id"`
		Marker string `json:"marker"`
	}
	home := t.TempDir()
	for _, id := range []string{"c1", "c2", "c3"} {
		if err := WriteCandidate(home, Memory, id, rec{ID: id, Marker: "before"}); err != nil {
			t.Fatal(err)
		}
	}

	originalReadDirectoryNames := candidateReadDirectoryNames
	directoryReads := 0
	candidateReadDirectoryNames = func(directory *candidateDirectory) ([]string, error) {
		directoryReads++
		return originalReadDirectoryNames(directory)
	}
	t.Cleanup(func() { candidateReadDirectoryNames = originalReadDirectoryNames })

	page, err := ReadPage(home, Memory, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if entry.Record == nil {
			t.Fatalf("unexpected corrupt page entry: %+v", entry)
		}
		replaceResult, err := ReplaceCandidateIfCurrent(home, *entry.Record, rec{ID: entry.Record.ID, Marker: "after"})
		if err != nil || !replaceResult.Replaced {
			t.Fatalf("replace %s: result=%+v err=%v", entry.Record.ID, replaceResult, err)
		}
	}
	if directoryReads != 1 {
		t.Fatalf("directory reads = %d, want one page enumeration and O(1) CAS per candidate", directoryReads)
	}
}

func TestCreateCandidateIfAbsentPreservesExistingBytes(t *testing.T) {
	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	payload := rec{ID: "c1", Kind: string(Memory), Marker: "contender"}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "same candidate", body: []byte("{\n  \"id\": \"c1\",\n  \"kind\": \"memory\",\n  \"marker\": \"contender\"\n}\n")},
		{name: "different candidate", body: []byte("{\"id\":\"c1\",\"kind\":\"memory\",\"marker\":\"existing\"}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, InboxDir(Memory), "c1.json")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, tc.body, 0o644); err != nil {
				t.Fatal(err)
			}

			created, err := CreateCandidateIfAbsent(home, Memory, "c1", payload)
			if err != nil {
				t.Fatalf("existing classifiable candidate: %v", err)
			}
			if created {
				t.Fatal("existing candidate reported created")
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("existing candidate changed:\n got: %q\nwant: %q", got, tc.body)
			}
		})
	}

	t.Run("malformed candidate", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, InboxDir(Memory), "c1.json")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		want := []byte("{not-json\n")
		if err := os.WriteFile(target, want, 0o644); err != nil {
			t.Fatal(err)
		}

		created, err := CreateCandidateIfAbsent(home, Memory, "c1", payload)
		if err == nil {
			t.Fatal("malformed existing candidate must return an error")
		}
		if created {
			t.Fatal("malformed existing candidate reported created")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("malformed candidate changed: got %q want %q", got, want)
		}
	})

	t.Run("unreadable candidate", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, InboxDir(Memory), "c1.json")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		want := []byte("{\"id\":\"c1\",\"kind\":\"memory\"}\n")
		if err := os.WriteFile(target, want, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o644) })

		created, err := CreateCandidateIfAbsent(home, Memory, "c1", payload)
		if err == nil {
			t.Fatal("unreadable existing candidate must return an error")
		}
		if created {
			t.Fatal("unreadable existing candidate reported created")
		}
		if err := os.Chmod(target, 0o644); err != nil {
			t.Fatal(err)
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("unreadable candidate changed: got %q want %q", got, want)
		}
	})

	t.Run("non-regular candidate", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, InboxDir(Memory), "c1.json")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}

		created, err := CreateCandidateIfAbsent(home, Memory, "c1", payload)
		if err == nil {
			t.Fatal("non-regular existing candidate must return an error")
		}
		if created {
			t.Fatal("non-regular existing candidate reported created")
		}
		info, statErr := os.Lstat(target)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if !info.IsDir() {
			t.Fatalf("non-regular target changed type: %v", info.Mode())
		}
	})

	t.Run("symlink candidate", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, InboxDir(Memory))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(home, "sentinel.json")
		want := []byte("sentinel bytes\n")
		if err := os.WriteFile(sentinel, want, 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "c1.json")
		if err := os.Symlink(sentinel, target); err != nil {
			t.Fatal(err)
		}

		created, err := CreateCandidateIfAbsent(home, Memory, "c1", payload)
		if err == nil {
			t.Fatal("symlink existing candidate must return an error")
		}
		if created {
			t.Fatal("symlink existing candidate reported created")
		}
		info, statErr := os.Lstat(target)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("candidate symlink was replaced: %v", info.Mode())
		}
		got, readErr := os.ReadFile(sentinel)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("symlink referent changed: got %q want %q", got, want)
		}
	})
}

func TestCreateCandidateIfAbsentPublishesWholeDurableJSON(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	payload := rec{ID: "durable", Kind: string(Memory), Marker: "whole"}
	want := canonicalCandidateBody(t, payload)
	target := filepath.Join(home, InboxDir(Memory), "durable.json")

	originalLink := linkCandidateFile
	originalSync := syncCandidateDirectoryFn
	t.Cleanup(func() {
		linkCandidateFile = originalLink
		syncCandidateDirectoryFn = originalSync
	})
	events := make([]string, 0, 2)
	linkCandidateFile = func(directory *candidateDirectory, tempName, targetName string) error {
		events = append(events, "link")
		tempPath := directory.path(tempName)
		targetPath := directory.path(targetName)
		if targetPath != target {
			t.Fatalf("link target = %s, want %s", targetPath, target)
		}
		if _, err := os.Lstat(targetPath); !os.IsNotExist(err) {
			t.Fatalf("authoritative target existed before link: %v", err)
		}
		staged, err := os.ReadFile(tempPath)
		if err != nil {
			t.Fatalf("read staged candidate: %v", err)
		}
		if !bytes.Equal(staged, want) {
			t.Fatalf("staged candidate was partial/noncanonical: got %q want %q", staged, want)
		}
		info, err := os.Stat(tempPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("staged candidate mode = %o, want 644", info.Mode().Perm())
		}
		return directory.link(tempName, targetName)
	}
	syncCandidateDirectoryFn = func(directory *candidateDirectory) error {
		events = append(events, "sync")
		path := directory.displayPath
		if path != filepath.Dir(target) {
			t.Fatalf("synced directory = %s, want %s", path, filepath.Dir(target))
		}
		return directory.sync()
	}

	created, err := CreateCandidateIfAbsent(home, Memory, "durable", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("new candidate reported not-created")
	}
	if strings.Join(events, ",") != "link,sync" {
		t.Fatalf("publication events = %v, want [link sync]", events)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("published candidate = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".candidate-publish-") {
			t.Fatalf("candidate temp leaked after publication: %s", entry.Name())
		}
	}
}

func TestCreateCandidateIfAbsentSyncsPublishedLinkWhenTempCleanupFails(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	payload := rec{ID: "cleanup-failure", Kind: string(Memory), Marker: "winner"}
	want := canonicalCandidateBody(t, payload)
	target := filepath.Join(home, InboxDir(Memory), "cleanup-failure.json")

	originalLink := linkCandidateFile
	originalSync := syncCandidateDirectoryFn
	t.Cleanup(func() {
		linkCandidateFile = originalLink
		syncCandidateDirectoryFn = originalSync
	})
	blockedTemp := ""
	linkCandidateFile = func(directory *candidateDirectory, tempName, targetName string) error {
		if err := directory.link(tempName, targetName); err != nil {
			return err
		}
		tempPath := directory.path(tempName)
		// Simulate an external cleanup race after the authoritative hard link
		// exists: replace the staging name with a non-empty directory so the
		// writer's remove fails deterministically.
		if err := os.Remove(tempPath); err != nil {
			t.Fatalf("remove staged link for cleanup failure injection: %v", err)
		}
		if err := os.Mkdir(tempPath, 0o755); err != nil {
			t.Fatalf("create cleanup blocker: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tempPath, "block"), []byte("block\n"), 0o644); err != nil {
			t.Fatalf("populate cleanup blocker: %v", err)
		}
		blockedTemp = tempPath
		return nil
	}
	syncCalls := 0
	syncCandidateDirectoryFn = func(directory *candidateDirectory) error {
		syncCalls++
		return directory.sync()
	}

	created, err := CreateCandidateIfAbsent(home, Memory, "cleanup-failure", payload)
	if blockedTemp != "" {
		t.Cleanup(func() {
			_ = os.Remove(filepath.Join(blockedTemp, "block"))
			_ = os.Remove(blockedTemp)
		})
	}
	if !created {
		t.Fatal("successful link must report created even when temp cleanup fails")
	}
	if err == nil || !strings.Contains(err.Error(), "remove candidate temp") {
		t.Fatalf("cleanup failure error = %v", err)
	}
	if syncCalls != 1 {
		t.Fatalf("directory sync calls after successful link = %d, want 1 despite cleanup failure", syncCalls)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("published target changed after cleanup failure: got %q want %q", got, want)
	}

	if err := os.Remove(filepath.Join(blockedTemp, "block")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blockedTemp); err != nil {
		t.Fatal(err)
	}
	blockedTemp = ""
	retryPayload := rec{ID: "cleanup-failure", Kind: string(Memory), Marker: "retry-must-not-replace"}
	retryCreated, retryErr := CreateCandidateIfAbsent(home, Memory, "cleanup-failure", retryPayload)
	if retryErr != nil {
		t.Fatalf("retry after linked cleanup failure: %v", retryErr)
	}
	if retryCreated {
		t.Fatal("retry replaced an already-published candidate")
	}
	afterRetry, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(afterRetry, want) {
		t.Fatalf("retry rewrote published bytes: got %q want %q", afterRetry, want)
	}
}

// Guards (C3 hardening): the codec rejects path-shaped ids and payloads that
// contradict the path they would be filed under.
func TestCandidateCodecGuards(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID   string `json:"id,omitempty"`
		Kind string `json:"kind,omitempty"`
	}
	for _, bad := range []string{"", "..", ".", "a/b", "../escape", "x\\y"} {
		if err := WriteCandidate(home, Memory, bad, rec{}); err == nil {
			t.Errorf("WriteCandidate id=%q: expected rejection", bad)
		}
		if err := ReadCandidate(home, Memory, bad, &rec{}); err == nil {
			t.Errorf("ReadCandidate id=%q: expected rejection", bad)
		}
	}
	if err := WriteCandidate(home, Memory, "c1", rec{ID: "other"}); err == nil {
		t.Fatal("payload id mismatch must be rejected")
	}
	if err := WriteCandidate(home, Memory, "c2", rec{Kind: "skill"}); err == nil {
		t.Fatal("payload kind mismatch must be rejected")
	}
	if err := WriteCandidate(home, Memory, "c3", rec{ID: "c3", Kind: "memory"}); err != nil {
		t.Fatalf("matching id+kind rejected: %v", err)
	}
}
