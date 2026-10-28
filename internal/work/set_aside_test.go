package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var setAsideNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

type setAsideFixture struct {
	store     *Store
	repo      string
	base      string
	candidate string
	workID    string
}

// newSetAsideFixture builds a repo whose main tip IS the candidate: base commits
// README.md, the feature commit adds feature.txt, and main is fast-forwarded to
// it. feature.txt is therefore an owned (base..candidate) file that starts clean.
func newSetAsideFixture(t *testing.T) setAsideFixture {
	t.Helper()
	repo, base, candidate := sharedLandingLandedRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner: "codex", Actor: "codex", SuccessPredicate: "set-aside restores foreign WIP",
		GitDelivery: localGitDelivery(repo, base),
	}, setAsideNow)
	if err != nil {
		t.Fatal(err)
	}
	return setAsideFixture{store: store, repo: repo, base: base, candidate: candidate, workID: w.ID}
}

func (f setAsideFixture) recordPath() string {
	return filepath.Join(f.store.home, "work", "set-aside", f.workID+".json")
}

func setAsideWriteOwned(t *testing.T, repo, content string) {
	t.Helper()
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), content)
}

func setAsideReadOwned(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setAsideStashList(t *testing.T, repo string) []string {
	t.Helper()
	out := landingGitOutput(t, repo, "stash", "list")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func setAsideAssertNoRecord(t *testing.T, f setAsideFixture) {
	t.Helper()
	if _, err := os.Stat(f.recordPath()); !os.IsNotExist(err) {
		t.Fatalf("a refusal wrote a set-aside record: err=%v", err)
	}
}

func setAsideAssertNoStashNoRecord(t *testing.T, f setAsideFixture) {
	t.Helper()
	if list := setAsideStashList(t, f.repo); len(list) != 0 {
		t.Fatalf("a refusal created a stash entry: %v", list)
	}
	setAsideAssertNoRecord(t, f)
}

func setAsideTamperRecordSHA(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec SetAsideRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) == 0 {
		t.Fatal("record has no files to tamper")
	}
	rec.Files[0].SHA256 = strings.Repeat("0", 64)
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Happy path: begin reverts the owned file to its committed content and records
// the foreign bytes; end restores those exact bytes and cleans up.
func TestSetAsideBeginRevertsAndEndRestores(t *testing.T) {
	f := newSetAsideFixture(t)
	setAsideWriteOwned(t, f.repo, "FOREIGN WIP\n")

	rec, err := f.store.SetAsideBegin(SetAsideBeginOptions{
		WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "neighbor lane WIP",
	}, setAsideNow)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if rec.Token == "" || len(rec.Files) != 1 || rec.Files[0].Path != "feature.txt" || rec.CandidateSHA != f.candidate || rec.SchemaVersion != SetAsideSchemaVersion {
		t.Fatalf("begin record = %+v", rec)
	}
	if got := setAsideReadOwned(t, f.repo); got != "feature\n" {
		t.Fatalf("begin did not revert the working tree: %q", got)
	}
	if _, err := os.Stat(f.recordPath()); err != nil {
		t.Fatalf("begin did not write the record: %v", err)
	}

	res, err := f.store.SetAsideEnd(SetAsideEndOptions{WorkID: f.workID, Actor: "codex"}, setAsideNow)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "feature.txt" || res.Token != rec.Token {
		t.Fatalf("end result = %+v", res)
	}
	if got := setAsideReadOwned(t, f.repo); got != "FOREIGN WIP\n" {
		t.Fatalf("end did not restore the foreign content: %q", got)
	}
	if _, err := os.Stat(f.recordPath()); !os.IsNotExist(err) {
		t.Fatalf("end did not delete the record: %v", err)
	}
	if list := setAsideStashList(t, f.repo); len(list) != 0 {
		t.Fatalf("end did not drop the stash: %v", list)
	}
}

// F1: end applies/drops ONLY the token entry; foreign stash entries created
// before and after the set-aside survive untouched.
func TestSetAsideEndTouchesOnlyTokenEntryAmongForeignStashes(t *testing.T) {
	f := newSetAsideFixture(t)

	writeLandingFile(t, filepath.Join(f.repo, "README.md"), "unrelated before\n")
	landingGit(t, f.repo, "stash", "push", "-m", "unrelated-before", "--", "README.md")

	setAsideWriteOwned(t, f.repo, "FOREIGN NEIGHBOR WIP\n")
	rec, err := f.store.SetAsideBegin(SetAsideBeginOptions{
		WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "neighbor lane WIP",
	}, setAsideNow)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	writeLandingFile(t, filepath.Join(f.repo, "README.md"), "unrelated after\n")
	landingGit(t, f.repo, "stash", "push", "-m", "unrelated-after", "--", "README.md")

	if before := setAsideStashList(t, f.repo); len(before) != 3 {
		t.Fatalf("expected 3 stash entries before end, got %d: %v", len(before), before)
	}

	if _, err := f.store.SetAsideEnd(SetAsideEndOptions{WorkID: f.workID, Actor: "codex"}, setAsideNow); err != nil {
		t.Fatalf("end: %v", err)
	}

	after := setAsideStashList(t, f.repo)
	if len(after) != 2 {
		t.Fatalf("expected 2 foreign stash entries to survive, got %d: %v", len(after), after)
	}
	joined := strings.Join(after, "\n")
	if strings.Contains(joined, rec.Token) {
		t.Fatalf("set-aside stash entry survived end: %v", after)
	}
	if !strings.Contains(joined, "unrelated-before") || !strings.Contains(joined, "unrelated-after") {
		t.Fatalf("foreign stash entries were disturbed: %v", after)
	}
	if got := setAsideReadOwned(t, f.repo); got != "FOREIGN NEIGHBOR WIP\n" {
		t.Fatalf("end restored the wrong content: %q", got)
	}
}

// F2: the owned-file refusal -- working content identical to the candidate blob
// means the dirt IS this envelope's own change; commit it, do not set it aside.
// HEAD is one commit past the candidate so the file can be dirty vs HEAD yet
// byte-identical to the candidate's blob.
func TestSetAsideBeginRefusesFileMatchingCandidateBlob(t *testing.T) {
	repo, base := initLandingRepo(t)
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "CANDIDATE CONTENT\n")
	landingGit(t, repo, "add", "feature.txt")
	landingGit(t, repo, "commit", "-m", "candidate")
	candidate := landingGitOutput(t, repo, "rev-parse", "HEAD")
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "HEAD MOVED CONTENT\n")
	landingGit(t, repo, "add", "feature.txt")
	landingGit(t, repo, "commit", "-m", "head-moves")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "x", GitDelivery: localGitDelivery(repo, base)}, setAsideNow)
	if err != nil {
		t.Fatal(err)
	}
	// Dirty vs HEAD, but byte-identical to candidate:feature.txt.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "CANDIDATE CONTENT\n")

	_, err = store.SetAsideBegin(SetAsideBeginOptions{WorkID: w.ID, CandidateSHA: candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "looks foreign"}, setAsideNow)
	if err == nil || !strings.Contains(err.Error(), "own change") {
		t.Fatalf("expected owned-file refusal, got %v", err)
	}
	if list := setAsideStashList(t, repo); len(list) != 0 {
		t.Fatalf("owned-file refusal created a stash: %v", list)
	}
	if _, statErr := os.Stat(filepath.Join(store.home, "work", "set-aside", w.ID+".json")); !os.IsNotExist(statErr) {
		t.Fatalf("owned-file refusal wrote a record: %v", statErr)
	}
}

// F3: clean-file refusal.
func TestSetAsideBeginRefusesCleanFile(t *testing.T) {
	f := newSetAsideFixture(t)
	_, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("expected clean-file refusal, got %v", err)
	}
	setAsideAssertNoStashNoRecord(t, f)
}

// F3b: untracked-file refusal.
func TestSetAsideBeginRefusesUntrackedFile(t *testing.T) {
	f := newSetAsideFixture(t)
	writeLandingFile(t, filepath.Join(f.repo, "untracked.txt"), "brand new\n")
	_, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"untracked.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatalf("expected untracked-file refusal, got %v", err)
	}
	setAsideAssertNoStashNoRecord(t, f)
}

// Refusal: a tracked, dirty file outside the envelope's owned set would not block
// the ceremony, so setting it aside is pointless.
func TestSetAsideBeginRefusesFileOutsideOwnedSet(t *testing.T) {
	f := newSetAsideFixture(t)
	writeLandingFile(t, filepath.Join(f.repo, "README.md"), "changed unrelated\n")
	_, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"README.md"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err == nil || !strings.Contains(err.Error(), "owned set") {
		t.Fatalf("expected owned-set refusal, got %v", err)
	}
	setAsideAssertNoStashNoRecord(t, f)
}

// Refusal: a file path escaping the target root.
func TestSetAsideBeginRefusesPathEscape(t *testing.T) {
	f := newSetAsideFixture(t)
	_, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"../outside.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected path-escape refusal, got %v", err)
	}
	setAsideAssertNoStashNoRecord(t, f)
}

// Refusal: structural arms -- missing envelope and a contractless envelope.
func TestSetAsideBeginRefusesMissingAndContractlessWork(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAsideBegin(SetAsideBeginOptions{WorkID: "work_missing", CandidateSHA: strings.Repeat("a", 40), Files: []string{"x"}, Actor: "a", Reason: "r"}, setAsideNow); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected missing-envelope refusal, got %v", err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "legacy work", State: "ready"}, setAsideNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAsideBegin(SetAsideBeginOptions{WorkID: w.ID, CandidateSHA: strings.Repeat("a", 40), Files: []string{"x"}, Actor: "a", Reason: "r"}, setAsideNow); err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("expected no-git_delivery refusal, got %v", err)
	}
}

// F5: double-begin refusal -- a second begin with a record present refuses and
// creates no second stash entry.
func TestSetAsideBeginRefusesDoubleBegin(t *testing.T) {
	f := newSetAsideFixture(t)
	setAsideWriteOwned(t, f.repo, "FOREIGN\n")
	rec, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "x"}, setAsideNow); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected double-begin refusal, got %v", err)
	}
	count := 0
	for _, line := range setAsideStashList(t, f.repo) {
		if strings.Contains(line, rec.Token) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one set-aside stash entry after double-begin, got %d", count)
	}
}

// F6: a begin refusal is atomic -- a valid earlier file must not be partially
// stashed when a later file in the same call is refused.
func TestSetAsideBeginRefusalIsAtomicNoPartialStash(t *testing.T) {
	f := newSetAsideFixture(t)
	setAsideWriteOwned(t, f.repo, "FOREIGN VALID\n") // feature.txt: valid, dirty, owned
	before := setAsideReadOwned(t, f.repo)

	// README.md is clean and unowned -> it refuses AFTER feature.txt validated OK.
	_, err := f.store.SetAsideBegin(SetAsideBeginOptions{
		WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt", "README.md"}, Actor: "codex", Reason: "x",
	}, setAsideNow)
	if err == nil {
		t.Fatal("expected a refusal on the second (clean) file")
	}
	if got := setAsideReadOwned(t, f.repo); got != before {
		t.Fatalf("the valid file was mutated during a refusal: before=%q after=%q", before, got)
	}
	setAsideAssertNoStashNoRecord(t, f)
}

// F4: hash-mismatch loud-fail -- if the restored bytes do not reproduce the
// recorded SHA-256, end fails loudly and preserves BOTH the stash entry and the
// record. The record's stored hash is tampered to force the divergence.
func TestSetAsideEndLoudFailsOnHashMismatchPreservingStashAndRecord(t *testing.T) {
	f := newSetAsideFixture(t)
	setAsideWriteOwned(t, f.repo, "FOREIGN\n")
	rec, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	setAsideTamperRecordSHA(t, f.recordPath())

	if _, err := f.store.SetAsideEnd(SetAsideEndOptions{WorkID: f.workID, Actor: "codex"}, setAsideNow); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("expected diverged loud-fail, got %v", err)
	}
	found := false
	for _, line := range setAsideStashList(t, f.repo) {
		if strings.Contains(line, rec.Token) {
			found = true
		}
	}
	if !found {
		t.Fatal("loud-fail must preserve the stash entry")
	}
	if _, err := os.Stat(f.recordPath()); err != nil {
		t.Fatalf("loud-fail must preserve the record: %v", err)
	}
}

// F4 (precheck arm): if a recorded path is re-dirtied after begin, end refuses
// before applying so the fresh dirt is never overwritten; stash and record stay.
func TestSetAsideEndRefusesWhenRecordedPathReDirtied(t *testing.T) {
	f := newSetAsideFixture(t)
	setAsideWriteOwned(t, f.repo, "FOREIGN\n")
	rec, err := f.store.SetAsideBegin(SetAsideBeginOptions{WorkID: f.workID, CandidateSHA: f.candidate, Files: []string{"feature.txt"}, Actor: "codex", Reason: "x"}, setAsideNow)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	setAsideWriteOwned(t, f.repo, "FRESH DIRT\n") // re-dirty the reverted path

	if _, err := f.store.SetAsideEnd(SetAsideEndOptions{WorkID: f.workID, Actor: "codex"}, setAsideNow); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("expected dirty-precheck refusal, got %v", err)
	}
	if got := setAsideReadOwned(t, f.repo); got != "FRESH DIRT\n" {
		t.Fatalf("end applied despite the dirty precheck: %q", got)
	}
	found := false
	for _, line := range setAsideStashList(t, f.repo) {
		if strings.Contains(line, rec.Token) {
			found = true
		}
	}
	if !found {
		t.Fatal("dirty-precheck refusal must preserve the stash entry")
	}
	if _, err := os.Stat(f.recordPath()); err != nil {
		t.Fatalf("dirty-precheck refusal must preserve the record: %v", err)
	}
}

// F8: the land-check dirty_target next action names the sanctioned set-aside door
// while keeping its preserve-unrelated-work framing.
func TestLandingDirtyTargetNextActionNamesSetAsideDoor(t *testing.T) {
	msg := landingOwnedDirtyNextAction("refs/heads/main", []string{"internal/foo.go"})
	if !strings.Contains(msg, "compound work set-aside begin") {
		t.Fatalf("dirty_target next action must name the set-aside door: %q", msg)
	}
	if !strings.Contains(msg, "without discarding unrelated work") {
		t.Fatalf("dirty_target next action lost its preserve-WIP framing: %q", msg)
	}
}
