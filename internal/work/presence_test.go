package work

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// The whole reason presence exists: an agent Compound never spawned, holding a
// lease under its own name, must be visible. If recipient addressability ever
// regresses to spawn provenance (a delegate binding, a run-attempt origin),
// this is the test that goes red -- no delegate binding is created anywhere in
// this fixture.
func TestPresenceIncludesLeaseHolderWithNoDelegateBinding(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "unspawned agent is still here"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "claude-code:never-delegated-20260731", claimAt); err != nil {
		t.Fatal(err)
	}

	projection, err := s.PresenceProjectionFor(claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	row, ok := findPresenceRow(projection, "claude-code:never-delegated-20260731")
	if !ok {
		t.Fatalf("lease holder absent from presence: %+v", projection.Present)
	}
	if !row.Fresh {
		t.Fatalf("lease holder should be fresh one minute after claiming: %+v", row)
	}
	if row.Leases != 1 {
		t.Fatalf("lease count = %d, want 1 (%+v)", row.Leases, row)
	}
	if !containsPresenceString(row.WorkIDs, w.ID) {
		t.Fatalf("work id %s missing from row: %+v", w.ID, row)
	}
}

// A lease does not decay on its own, so an abandoned one would advertise a
// long-dead agent as present forever. The live home carries leases weeks old;
// without a TTL this projection would confidently name absent actors.
func TestPresenceMarksActorStaleBeyondTTL(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "stale holder"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "codex:long-gone", claimAt); err != nil {
		t.Fatal(err)
	}

	// Far beyond the TTL.
	later := claimAt.Add(DefaultPresenceTTL + time.Hour)

	projection, err := s.PresenceProjectionFor(later)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findPresenceRow(projection, "codex:long-gone"); ok {
		t.Fatalf("stale actor should be excluded by default: %+v", projection.Present)
	}
	if projection.Counts.Stale != 1 {
		t.Fatalf("stale count = %d, want 1 (%+v)", projection.Counts.Stale, projection.Counts)
	}

	withStale, err := s.PresenceProjectionFor(later, PresenceOptions{IncludeStale: true})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := findPresenceRow(withStale, "codex:long-gone")
	if !ok {
		t.Fatalf("IncludeStale should surface the actor: %+v", withStale.Present)
	}
	if row.Fresh {
		t.Fatalf("actor past TTL must not be marked fresh: %+v", row)
	}
	if row.AgeSeconds < int64(DefaultPresenceTTL/time.Second) {
		t.Fatalf("age %ds should exceed the TTL: %+v", row.AgeSeconds, row)
	}
}

// One agent seen twice is one agent. If lease and event sightings ever produce
// separate rows, the fleet looks twice its real size and last-seen becomes
// whichever source happened to be read last.
func TestPresenceMergesLeaseAndEventSightingsIntoOneActor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "merge sightings"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "codex:one-agent", claimAt); err != nil {
		t.Fatal(err)
	}

	projection, err := s.PresenceProjectionFor(claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for _, row := range projection.Present {
		if row.Actor == "codex:one-agent" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("actor appeared %d times, want exactly 1: %+v", seen, projection.Present)
	}
	row, _ := findPresenceRow(projection, "codex:one-agent")
	if row.Leases == 0 || row.Events == 0 {
		t.Fatalf("expected both a lease and an event sighting: %+v", row)
	}
	if projection.Counts.BothSources != 1 {
		t.Fatalf("both_sources = %d, want 1 (%+v)", projection.Counts.BothSources, projection.Counts)
	}
}

// The work log grows without bound, so presence reads only its tail. That
// bound must not become a blind spot: a lease holder whose events fall outside
// the window is still present, because the lease is independent evidence.
func TestPresenceTailBoundDoesNotHideLeaseHolders(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "bounded read"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "grok:outside-the-window", claimAt); err != nil {
		t.Fatal(err)
	}

	// A window so small that no whole event line can survive it.
	projection, err := s.PresenceProjectionFor(claimAt.Add(time.Minute), PresenceOptions{TailBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := findPresenceRow(projection, "grok:outside-the-window")
	if !ok {
		t.Fatalf("lease holder must survive a truncated tail read: %+v", projection.Present)
	}
	if row.Events != 0 {
		t.Fatalf("no event should have been readable in a 1-byte window: %+v", row)
	}
	if row.Leases != 1 {
		t.Fatalf("lease evidence lost: %+v", row)
	}
	if !projection.TailTruncated {
		t.Fatal("projection should report the tail was truncated")
	}
}

// Presence is a read-only projection over a home that a dozen live agents are
// mutating. If it ever writes -- a cache, an index, a candidate -- it becomes
// a contention source on exactly the surface it is meant to observe safely.
func TestPresenceWritesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "read only"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "claude-code:observer", claimAt); err != nil {
		t.Fatal(err)
	}

	before := hashHomeTree(t, s.home)
	if _, err := s.PresenceProjectionFor(claimAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PresenceProjectionFor(claimAt.Add(2*time.Minute), PresenceOptions{IncludeStale: true}); err != nil {
		t.Fatal(err)
	}
	after := hashHomeTree(t, s.home)

	if before != after {
		t.Fatalf("presence mutated the home:\n before=%s\n after =%s", before, after)
	}
}

// OpenReadOnly is the opener the presence CLI must use: unlike Open it creates
// no work/* dirs and never calls events.Ensure. A bare home (nothing but the
// directory itself) must yield an empty, well-formed projection AND remain
// byte-identical on disk — inspecting presence leaves no footprints.
func TestOpenReadOnlyCreatesNoDirsAndProjectsEmpty(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()

	s, err := OpenReadOnly(homePath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	projection, err := s.PresenceProjectionFor(fixedNow)
	if err != nil {
		t.Fatalf("projection on a bare home must be empty, not an error: %v", err)
	}
	if len(projection.Present) != 0 || projection.Counts.Actors != 0 {
		t.Fatalf("bare home must project nobody present: %+v", projection)
	}

	entries, err := os.ReadDir(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("OpenReadOnly + projection must create nothing, found %d entries:", len(entries))
	}
}

// OpenReadOnly against a populated home must see exactly what Open sees — the
// read-only opener may not observe a different ledger.
func TestOpenReadOnlySeesExistingLeases(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "ro opener sees leases"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "claude-code:ro-opener", claimAt); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(s.home)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	projection, err := ro.PresenceProjectionFor(claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findPresenceRow(projection, "claude-code:ro-opener"); !ok {
		t.Fatalf("read-only opener must see the lease holder: %+v", projection.Present)
	}
}

func findPresenceRow(projection PresenceProjection, actor string) (PresenceRow, bool) {
	for _, row := range projection.Present {
		if row.Actor == actor {
			return row, true
		}
	}
	return PresenceRow{}, false
}

func containsPresenceString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// hashHomeTree digests every regular file path plus its bytes, so both content
// edits and new files are caught.
func hashHomeTree(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		path string
		sum  string
	}
	entries := []entry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{path: rel, sum: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	digest := sha256.New()
	for _, item := range entries {
		digest.Write([]byte(item.path))
		digest.Write([]byte(item.sum))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
