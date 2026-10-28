package runattempt

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIDFormat(t *testing.T) {
	fixed := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id, err := Mint(fixed)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if !Valid(id) {
			t.Fatalf("minted id %q did not pass Valid()", id)
		}
		if !strings.HasPrefix(id, "ratt_") {
			t.Fatalf("minted id %q missing ratt_ prefix", id)
		}
		if !strings.Contains(id, "20260720T000000Z") {
			t.Fatalf("minted id %q missing expected timestamp", id)
		}
	}
	// Two mints at the same instant still produce distinct ids (suffix is from rand).
	a, _ := Mint(fixed)
	b, _ := Mint(fixed)
	if a == b {
		t.Fatalf("two concurrent mints collided: %q", a)
	}
}

func TestIDRejectsLegacyShapes(t *testing.T) {
	for _, bad := range []string{
		"",
		"prun_20260718T100000Z_aaaaaaaaaaaa",
		"run_packet",
		"act_9f2c1d4e",
		"work_abcdef",
		"ratt_2026-07-20_aabbccdd",        // wrong timestamp layout
		"ratt_20260720T000000Z_aabbccd",   // 7-char suffix
		"ratt_20260720T000000Z_AABBCCDD",  // uppercase
		" ratt_20260720T000000Z_aabbccdd", // leading space
		"ratt_20260720T000000Z_aabbccdd ", // trailing space
	} {
		if Valid(bad) {
			t.Fatalf("Valid(%q) must be false", bad)
		}
	}
}

func TestStoreMintAndGet(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound-panel", "fake", now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !Valid(ra.ID) {
		t.Fatalf("minted id %q not valid", ra.ID)
	}
	if ra.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %q, want %q", ra.SchemaVersion, SchemaVersion)
	}
	if ra.Status != StatusPending {
		t.Fatalf("initial status = %q, want pending", ra.Status)
	}
	if !ra.CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want %v", ra.CreatedAt, now)
	}

	// File landed in the right place.
	if _, err := os.Stat(filepath.Join(home, "run_attempts", ra.ID+".json")); err != nil {
		t.Fatalf("expected on-disk artifact: %v", err)
	}

	// Fresh Store re-reads from disk (no cache carry-over).
	s2 := Open(home)
	got, err := s2.Get(ra.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Field-by-field comparison: RunAttempt contains []string (ProofRefs, added in S2)
	// which makes the struct uncomparable with ==.
	if got.ID != ra.ID || got.WorkID != ra.WorkID || got.Actor != ra.Actor ||
		got.Harness != ra.Harness || got.Status != ra.Status ||
		!got.CreatedAt.Equal(ra.CreatedAt) || !got.CompletedAt.Equal(ra.CompletedAt) ||
		got.SchemaVersion != ra.SchemaVersion {
		t.Fatalf("Get round-trip mismatch:\n got: %+v\nwant: %+v", got, ra)
	}
}

func TestStoreMintRejectsMissingFields(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		workID  string
		actor   string
		harness string
		wantSub string
	}{
		{"no work_id", "", "compound", "fake", "work_id is required"},
		{"no actor", "work_abc", "", "fake", "actor is required"},
		{"no harness", "work_abc", "compound", "", "harness is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Mint(tc.workID, tc.actor, tc.harness, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Mint(%s) err = %v, want substring %q", tc.name, err, tc.wantSub)
			}
		})
	}
}

func TestStoreComplete(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	// Only terminal statuses allowed.
	if _, err := s.Complete(ra.ID, StatusRunning, now.Add(time.Second)); err == nil {
		t.Fatalf("Complete with non-terminal status must fail")
	}
	if _, err := s.Complete(ra.ID, "garbage", now.Add(time.Second)); err == nil {
		t.Fatalf("Complete with bogus status must fail")
	}
	completed, err := s.Complete(ra.ID, StatusSucceeded, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.Status != StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", completed.Status)
	}
	if !completed.CompletedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("completed_at = %v, want %v", completed.CompletedAt, now.Add(time.Minute))
	}
	// Second Complete is rejected (already terminal).
	if _, err := s.Complete(ra.ID, StatusFailed, now.Add(2*time.Minute)); err == nil {
		t.Fatalf("re-Complete of terminal RunAttempt must fail")
	}
	// Disk shows the terminal status.
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded {
		t.Fatalf("on-disk status = %q, want succeeded", got.Status)
	}
}

func TestStoreGetRejectsBadID(t *testing.T) {
	s := Open(t.TempDir())
	for _, bad := range []string{"", "prun_xxx", "free_string", "ratt_20260720T000000Z_short"} {
		if _, err := s.Get(bad); err == nil {
			t.Fatalf("Get(%q) must fail", bad)
		}
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := Open(t.TempDir())
	_, err := s.Get("ratt_20260720T000000Z_deadbeef")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// TestStoreAttachActionIntentHappy binds a freshly minted RunAttempt to an act_ id and proves
// the binding is persisted (a fresh Store reads it back) and retry-idempotent.
func TestStoreAttachActionIntentHappy(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound-panel", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	actID := "act_9f2c1d4e"
	if err := s.AttachActionIntent(ra.ID, actID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActionIntentID != actID {
		t.Fatalf("persisted action_intent_id = %q, want %q", got.ActionIntentID, actID)
	}
	// Idempotent retry: attaching the SAME intent again is a no-op success.
	if err := s.AttachActionIntent(ra.ID, actID); err != nil {
		t.Fatalf("idempotent re-attach must succeed, got: %v", err)
	}
}

// TestStoreAttachActionIntentRejects covers the three refusal paths: a non-act_ intent, a
// mismatched rebind (ErrAlreadyBound), and binding after terminalization.
func TestStoreAttachActionIntentRejects(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound-panel", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	// Non-act_ intent id.
	if err := s.AttachActionIntent(ra.ID, "prun_deadbeef"); err == nil {
		t.Fatalf("attach with non-act_ intent must fail")
	}
	// Bind once, then attempt a mismatched rebind.
	first := "act_11112222"
	if err := s.AttachActionIntent(ra.ID, first); err != nil {
		t.Fatal(err)
	}
	err = s.AttachActionIntent(ra.ID, "act_33334444")
	if !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("mismatched rebind must return ErrAlreadyBound, got: %v", err)
	}
	// Binding after terminalization is a bug.
	ra2, err := s.Mint("work_def", "compound-panel", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ra2.ID, StatusFailed, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachActionIntent(ra2.ID, "act_55556666"); err == nil {
		t.Fatalf("attach after terminal must fail")
	}
	// Malformed ratt_ id.
	if err := s.AttachActionIntent("not-a-ratt-id", "act_77778888"); err == nil {
		t.Fatalf("attach with malformed id must fail")
	}
}

// --- C1: abandoned terminal status ---

func TestCompleteAcceptsAbandoned(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.Complete(ra.ID, StatusAbandoned, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Complete(abandoned): %v", err)
	}
	if completed.Status != StatusAbandoned {
		t.Fatalf("status = %q, want abandoned", completed.Status)
	}
	if !completed.CompletedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("completed_at = %v, want %v", completed.CompletedAt, now.Add(time.Minute))
	}
	// Re-complete is rejected.
	if _, err := s.Complete(ra.ID, StatusFailed, now.Add(2*time.Minute)); err == nil {
		t.Fatalf("re-Complete of abandoned RunAttempt must fail")
	}
	// Disk shows the terminal status.
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAbandoned {
		t.Fatalf("on-disk status = %q, want abandoned", got.Status)
	}
}

func TestSettersRefuseOnAbandoned(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ra.ID, StatusAbandoned, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	setters := []struct {
		name string
		fn   func() error
	}{
		{"context_pack", func() error { _, e := s.SetContextPack(ra.ID, "x"); return e }},
		{"workspace", func() error { _, e := s.SetWorkspace(ra.ID, "/x"); return e }},
		{"command", func() error { _, e := s.SetCommand(ra.ID, "x"); return e }},
		{"heartbeat", func() error { _, e := s.Heartbeat(ra.ID, now.Add(2*time.Minute)); return e }},
		{"transcript", func() error { _, e := s.SetTranscript(ra.ID, "x"); return e }},
		{"diff", func() error { _, e := s.SetDiff(ra.ID, "x"); return e }},
		{"test", func() error { _, e := s.SetTest(ra.ID, "x"); return e }},
		{"proof_refs", func() error { _, e := s.SetProofRefs(ra.ID, []string{"x"}); return e }},
		{"attach_action_intent", func() error { return s.AttachActionIntent(ra.ID, "act_12345678") }},
		{"origin", func() error { _, e := s.SetOrigin(ra.ID, OriginClaimReservation); return e }},
		{"mark_running", func() error { _, e := s.MarkRunning(ra.ID, now.Add(2*time.Minute)); return e }},
		{"execution_posture", func() error {
			_, e := s.SetExecutionPosture(ra.ID, ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_running"})
			return e
		}},
	}
	for _, tc := range setters {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatalf("setter on abandoned RunAttempt must fail")
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{StatusPending, false},
		{StatusRunning, false},
		{StatusSucceeded, true},
		{StatusFailed, true},
		{StatusAbandoned, true},
		{"garbage", false},
		{"", false},
	} {
		if got := IsTerminal(tc.status); got != tc.want {
			t.Errorf("IsTerminal(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// --- C2: deterministic mint ---

func TestMintDeterministicSameSeedSameID(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	a := MintDeterministic("codex-job-abc123", now)
	b := MintDeterministic("codex-job-abc123", now)
	if a != b {
		t.Fatalf("same seed produced different ids: %q vs %q", a, b)
	}
	if !Valid(a) {
		t.Fatalf("deterministic id %q did not pass Valid()", a)
	}
}

func TestMintDeterministicDifferentSeedDifferentID(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	a := MintDeterministic("codex-job-abc123", now)
	b := MintDeterministic("codex-job-def456", now)
	if a == b {
		t.Fatalf("different seeds produced same id: %q", a)
	}
}

func TestMintWithIDIdempotent(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	id := MintDeterministic("job-xyz", now)
	first, err := s.MintWithID(id, "work_abc", "codex", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != id {
		t.Fatalf("first mint id = %q, want %q", first.ID, id)
	}
	// Second call with same id returns the existing record.
	second, err := s.MintWithID(id, "work_abc", "codex", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.WorkID != first.WorkID || second.Status != first.Status {
		t.Fatalf("idempotent mint returned different record: %+v vs %+v", second, first)
	}
	// Only one file on disk.
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("run_attempts has %d files, want 1", count)
	}
}

func TestMintWithIDRejectsInvalidID(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := s.MintWithID("bad-id", "work_abc", "codex", "codex", now); err == nil {
		t.Fatal("MintWithID with invalid id must fail")
	}
}

// F4: MintWithID must fail closed on identity mismatch -- codex job ids are
// globally meaningful, so a second work reusing the same deterministic id is
// a split-truth bug, not a legitimate idempotent retrieve.

func TestMintWithIDRejectsCrossWorkReuse(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	id := MintDeterministic("job-cross-work", now)
	if _, err := s.MintWithID(id, "work_abc", "codex", "codex", now); err != nil {
		t.Fatal(err)
	}
	_, err := s.MintWithID(id, "work_def", "codex", "codex", now.Add(time.Minute))
	if err == nil {
		t.Fatal("cross-work reuse of an observed job's RunAttempt must be rejected")
	}
	if !strings.Contains(err.Error(), "work_abc") || !strings.Contains(err.Error(), "work_def") {
		t.Fatalf("error must name both work ids, got: %v", err)
	}
	// The first work's record is intact -- not overwritten by the failed second mint.
	got, gerr := Open(home).Get(id)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.WorkID != "work_abc" {
		t.Fatalf("existing record mutated: work_id = %q, want work_abc", got.WorkID)
	}
	// Only one ratt_ file landed on disk for this id.
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("run_attempts has %d files, want 1", count)
	}
}

func TestMintWithIDSameWorkActorHarnessStaysIdempotent(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	id := MintDeterministic("job-same-work", now)
	first, err := s.MintWithID(id, "work_abc", "codex", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.MintWithID(id, "work_abc", "codex", "codex", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("same work/actor/harness re-observe must stay idempotent: %v", err)
	}
	if second.ID != first.ID || second.WorkID != first.WorkID {
		t.Fatalf("idempotent mint returned different record: %+v vs %+v", second, first)
	}
}

// --- C4: retry lineage (PredecessorID) ---

func TestSetPredecessorChains(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// First attempt: no predecessor.
	first, err := s.Mint("work_abc", "actor", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	if first.PredecessorID != "" {
		t.Fatalf("first attempt predecessor = %q, want empty", first.PredecessorID)
	}

	// Second attempt chains to first.
	second, err := s.Mint("work_abc", "actor", "fake", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := s.SetPredecessor(second.ID, first.ID)
	if err != nil {
		t.Fatalf("SetPredecessor: %v", err)
	}
	if linked.PredecessorID != first.ID {
		t.Fatalf("predecessor = %q, want %q", linked.PredecessorID, first.ID)
	}

	// Persisted on disk.
	got, err := Open(home).Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PredecessorID != first.ID {
		t.Fatalf("on-disk predecessor = %q, want %q", got.PredecessorID, first.ID)
	}

	// Idempotent: same predecessor again is a no-op.
	again, err := s.SetPredecessor(second.ID, first.ID)
	if err != nil {
		t.Fatalf("idempotent SetPredecessor: %v", err)
	}
	if again.PredecessorID != first.ID {
		t.Fatalf("idempotent predecessor = %q, want %q", again.PredecessorID, first.ID)
	}

	// Third attempt chains to second (three-link chain).
	third, err := s.Mint("work_abc", "actor", "fake", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	linked3, err := s.SetPredecessor(third.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked3.PredecessorID != second.ID {
		t.Fatalf("third predecessor = %q, want %q", linked3.PredecessorID, second.ID)
	}
}

func TestSetPredecessorRejectsSelfAndForeignWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
		padded  bool
		wantErr string
	}{
		{name: "self", wantErr: "must differ"},
		{name: "padded self", padded: true, wantErr: "must differ"},
		{name: "foreign work", foreign: true, wantErr: "same work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := Open(home)
			now := time.Now().UTC()
			attempt, err := store.Mint("work_retry_target", "actor", "fake", now)
			if err != nil {
				t.Fatal(err)
			}
			predecessor := attempt
			if tc.foreign {
				predecessor, err = store.Mint("work_unrelated", "actor", "fake", now)
				if err != nil {
					t.Fatal(err)
				}
			}
			readAttempt := func(id string) []byte {
				t.Helper()
				body, err := os.ReadFile(filepath.Join(home, "run_attempts", id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				return body
			}
			before := map[string][]byte{attempt.ID: readAttempt(attempt.ID), predecessor.ID: readAttempt(predecessor.ID)}
			predecessorID := predecessor.ID
			if tc.padded {
				predecessorID = " " + predecessorID + " "
			}
			if _, err := store.SetPredecessor(attempt.ID, predecessorID); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SetPredecessor error = %v, want %q", err, tc.wantErr)
			}
			for id, body := range before {
				if !reflect.DeepEqual(readAttempt(id), body) {
					t.Errorf("refused lineage changed attempt %s", id)
				}
			}
		})
	}
}

func TestSetPredecessorRefuses(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	first, _ := s.Mint("work_abc", "actor", "fake", now)
	second, _ := s.Mint("work_abc", "actor", "fake", now.Add(time.Minute))
	third, _ := s.Mint("work_abc", "actor", "fake", now.Add(2*time.Minute))

	// Link second → first.
	if _, err := s.SetPredecessor(second.ID, first.ID); err != nil {
		t.Fatal(err)
	}

	// Changing to a different predecessor is refused (append-only).
	if _, err := s.SetPredecessor(second.ID, third.ID); err == nil {
		t.Fatal("changing predecessor must be refused (append-only chain)")
	}

	// Non-ratt_ predecessor is refused.
	if _, err := s.SetPredecessor(third.ID, "prun_deadbeef"); err == nil {
		t.Fatal("non-ratt_ predecessor must be refused")
	}

	// Phantom predecessor (valid format but not on disk) is refused.
	if _, err := s.SetPredecessor(third.ID, "ratt_20260720T000000Z_deadbeef"); err == nil {
		t.Fatal("phantom predecessor must be refused")
	}

	// Empty predecessor is refused.
	if _, err := s.SetPredecessor(third.ID, ""); err == nil {
		t.Fatal("empty predecessor must be refused")
	}

	// Terminal attempt cannot be linked.
	s.Complete(third.ID, StatusFailed, now.Add(3*time.Minute))
	if _, err := s.SetPredecessor(third.ID, first.ID); err == nil {
		t.Fatal("SetPredecessor on terminal attempt must fail")
	}
}

func TestPredecessorIDOmitempty(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	ra, _ := s.Mint("work_abc", "actor", "fake", now)
	// No predecessor set → omitempty → not in JSON.
	data, err := os.ReadFile(filepath.Join(home, "run_attempts", ra.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "predecessor_id") {
		t.Fatal("predecessor_id must be absent from JSON when empty (omitempty)")
	}
}

// --- F2: Origin provenance ---

func TestSetOriginStampsAndPersists(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Origin != "" {
		t.Fatalf("fresh mint origin = %q, want empty", ra.Origin)
	}
	updated, err := s.SetOrigin(ra.ID, OriginClaimReservation)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Origin != OriginClaimReservation {
		t.Fatalf("origin = %q, want %q", updated.Origin, OriginClaimReservation)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != OriginClaimReservation {
		t.Fatalf("on-disk origin = %q, want %q", got.Origin, OriginClaimReservation)
	}
}

func TestSetOriginRefusesOnTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ra.ID, StatusSucceeded, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrigin(ra.ID, OriginHarnessExecution); err == nil {
		t.Fatal("SetOrigin on terminal RunAttempt must fail")
	}
}

// --- F5: MarkRunning ---

func TestMarkRunningTransitionsAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.MarkRunning(ra.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusRunning {
		t.Fatalf("status = %q, want running", updated.Status)
	}
	// Idempotent: marking running again is a no-op success, not an error.
	again, err := s.MarkRunning(ra.ID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("re-MarkRunning must be a no-op, not an error: %v", err)
	}
	if again.Status != StatusRunning {
		t.Fatalf("status after re-mark = %q, want running", again.Status)
	}
}

func TestMarkRunningRefusesOnTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", "fake", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ra.ID, StatusSucceeded, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunning(ra.ID, now.Add(2*time.Minute)); err == nil {
		t.Fatal("MarkRunning on terminal RunAttempt must fail")
	}
}

// nilProbeMatchesSweepStale is the store-level compatibility test:
// SweepStaleWithProbe with probe == nil produces the same on-disk
// effect for the same inputs as SweepStale -- same counters, same
// terminal-status distribution. The CLI surface always passes the
// work-owned probe; this assertion guarantees the pre-probe API path
// remains a literal fall-through that anyone wired before
// `compound run-attempt sweep` switched defaults can rely on. Both
// passes start from byte-identical cloned homes so both the report and
// every resulting RunAttempt JSON body can be compared exactly.
func TestNilProbeMatchesSweepStaleByteForByte(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	now := t0.Add(60 * time.Minute)
	home1 := t.TempDir()
	home2 := t.TempDir()
	s := Open(home1)
	a, _ := s.Mint("work_stale", "actor", "fake", t0)
	f, _ := s.Mint("work_fresh", "actor", "fake", t0.Add(50*time.Minute))
	r, _ := s.Mint("work_running", "actor", "fake", t0)
	if _, err := s.MarkRunning(r.ID, t0); err != nil {
		t.Fatal(err)
	}
	ids := []string{a.ID, f.ID, r.ID}
	cloneRunAttemptDir(t, home1, home2)
	repA, err := Open(home1).SweepStale(30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	repB, err := Open(home2).SweepStaleWithProbe(30*time.Minute, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repA, repB) {
		t.Fatalf("reports diverged: sweep=%+v probe=%+v", repA, repB)
	}
	// Each home's two "stale" attempts (pending+running) must end up
	// abandoned on disk with the same terminal_status. The fresh
	// attempt must stay pending on both homes.
	for _, home := range []string{home1, home2} {
		s := Open(home)
		// fresh (idx 1) -> still pending
		raFresh, err := s.Get(ids[1])
		if err != nil {
			t.Fatal(err)
		}
		if raFresh.Status != StatusPending {
			t.Fatalf("fresh must stay pending, got %s", raFresh.Status)
		}
		// stale (idx 0) and running-stale (idx 2) -> abandoned
		for _, j := range []int{0, 2} {
			ra, err := s.Get(ids[j])
			if err != nil {
				t.Fatal(err)
			}
			if ra.Status != StatusAbandoned {
				t.Fatalf("id %s status %s, want abandoned", ids[j], ra.Status)
			}
		}
	}
	if got, want := snapshotRunAttemptDir(t, home1), snapshotRunAttemptDir(t, home2); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil-probe on-disk bytes diverged\nsweep=%v\nprobe=%v", got, want)
	}
}

func cloneRunAttemptDir(t *testing.T, fromHome, toHome string) {
	t.Helper()
	from := filepath.Join(fromHome, "run_attempts")
	to := filepath.Join(toHome, "run_attempts")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotRunAttemptDir(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(home, "run_attempts", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(body)
	}
	return out
}

func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// liveProbeHeartbeatsAndProtects covers the probe=lived case end-to-end:
// the candidate's binding is probed live (live=true), heartbeat refresh,
// no terminalization, no kill observable.
func TestLiveProbeHeartbeatsAndProtects(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_live", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(2 * time.Hour)
	probeCalls := 0
	rep, err := s.SweepStaleWithProbe(30*time.Minute, now, func(r RunAttempt) (bool, error) {
		probeCalls++
		if r.ID != ra.ID {
			t.Fatalf("probe received wrong ratt: %s", r.ID)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != 1 {
		t.Fatalf("probe should have been called exactly once, got %d", probeCalls)
	}
	if rep.Swept != 0 || rep.Protected != 1 {
		t.Fatalf("expected 1 protected, 0 swept: %+v", rep)
	}
	if rep.Skipped != 1 || rep.SkippedIDs[0] != ra.ID {
		t.Fatalf("expected the live candidate in SkippedIDs: %+v", rep)
	}
	after, _ := s.Get(ra.ID)
	if after.LastHeartbeatAt == nil || !after.LastHeartbeatAt.Equal(now) {
		t.Fatalf("live probe did not heartbeat exact ratt: %+v", after)
	}
	if after.Status != StatusRunning && after.Status != StatusPending {
		t.Fatalf("live probe terminalized the attempt: %s", after.Status)
	}
}

func TestProbeRunsOnlyForStaleCandidates(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	stale, _ := s.Mint("work_stale", "actor", "fake", t0)
	fresh, _ := s.Mint("work_fresh", "actor", "fake", t0.Add(50*time.Minute))
	called := []string{}
	if _, err := s.SweepStaleWithProbe(30*time.Minute, t0.Add(time.Hour), func(ra RunAttempt) (bool, error) {
		called = append(called, ra.ID)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(called, []string{stale.ID}) {
		t.Fatalf("probe calls = %v, want only stale %s (fresh %s)", called, stale.ID, fresh.ID)
	}
}

func TestLiveProbeReportsHeartbeatFailure(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_live", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(home, "run_attempts")
	if err := os.Chmod(runDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runDir, 0o700) })
	report, err := s.SweepStaleWithProbe(30*time.Minute, t0.Add(time.Hour), func(RunAttempt) (bool, error) {
		return true, nil
	})
	// Per-attempt best-effort (audit E15): a heartbeat-write failure on one
	// live-flagged candidate is reported via SkippedIDs and does NOT abort
	// the sweep pass. The previous contract -- returning the wrapped error
	// and stopping the loop -- stranded every later candidate.
	if report.Protected != 0 || len(report.SkippedIDs) != 1 || report.SkippedIDs[0] != ra.ID {
		t.Fatalf("heartbeat failure report = %+v (must carry the failed id in SkippedIDs)", report)
	}
	if report.Scanned != 1 {
		t.Fatalf("heartbeat failure Scanned=%d, want 1", report.Scanned)
	}
	if err != nil {
		t.Fatalf("sweep should not return a top-level error for a per-attempt heartbeat failure: %v", err)
	}
}

// deadProbeAbandonsViaSweep covers probe=live=false: the candidate is
// abandoned as the generic sweep would, but the report shows it as
// swept (the generic abandon path), not protected.
func TestDeadProbeAbandonsViaSweep(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	ra, _ := s.Mint("work_dead", "actor", "fake", t0)
	now := t0.Add(60 * time.Minute)
	rep, err := s.SweepStaleWithProbe(30*time.Minute, now, func(r RunAttempt) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 1 || rep.SweptIDs[0] != ra.ID {
		t.Fatalf("dead probe must abandon: %+v", rep)
	}
	if rep.Protected != 0 {
		t.Fatalf("dead probe must not protect: %+v", rep)
	}
	after, _ := s.Get(ra.ID)
	if after.Status != StatusAbandoned {
		t.Fatalf("expected abandoned, got %s", after.Status)
	}
}

// probeErrorSkipsRatherThanAbandon: a transient probe fault leaves the
// candidate non-terminal so the next pass retries.
func TestProbeErrorSkipsRatherThanAbandon(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	ra, _ := s.Mint("work_probe_err", "actor", "fake", t0)
	now := t0.Add(60 * time.Minute)
	rep, err := s.SweepStaleWithProbe(30*time.Minute, now, func(r RunAttempt) (bool, error) {
		return false, errors.New("transport: broken pipe")
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 0 || rep.Protected != 0 {
		t.Fatalf("probe error must not sweep/protect: %+v", rep)
	}
	if len(rep.SkippedIDs) != 1 || rep.SkippedIDs[0] != ra.ID {
		t.Fatalf("probe error must keep candidate as skipped: %+v", rep)
	}
	after, _ := s.Get(ra.ID)
	if after.Status != StatusPending {
		t.Fatalf("probe error must not terminalize: %s", after.Status)
	}
}

func TestCompleteForWorkCloseRecordsOwnerProofAndIsIdempotent(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_owner_proof", "codex", HarnessPlaceholder, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	completed, err := store.CompleteForWorkClose(ra.ID, StatusSucceeded, "rec_ownerproof", "tr_ownerproof", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("CompleteForWorkClose: %v", err)
	}
	if completed.TerminalizationOwner != TerminalizationOwnerWorkClose || completed.TerminalizationReceiptID != "rec_ownerproof" || completed.TerminalizationTransitionID != "tr_ownerproof" {
		t.Fatalf("terminalization proof not recorded: %+v", completed)
	}
	again, err := store.CompleteForWorkClose(ra.ID, StatusSucceeded, "rec_ownerproof", "tr_ownerproof", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("idempotent CompleteForWorkClose: %v", err)
	}
	if !again.CompletedAt.Equal(completed.CompletedAt) {
		t.Fatalf("idempotent CompleteForWorkClose moved completed_at: %v -> %v", completed.CompletedAt, again.CompletedAt)
	}
	if _, err := store.CompleteForWorkClose(ra.ID, StatusSucceeded, "rec_other", "tr_ownerproof", now.Add(3*time.Minute)); err == nil {
		t.Fatal("CompleteForWorkClose with mismatched close proof on terminal attempt must fail")
	}
}

func TestCompleteWithOwnerProofRecordsCallerOwnedTerminalization(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_panel_owned", "panel.apply", HarnessPlaceholder, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	completed, err := store.CompleteWithOwnerProof(ra.ID, StatusFailed, TerminalizationOwnerPanelApply, "rec_panel", "tr_panel", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("CompleteWithOwnerProof: %v", err)
	}
	if completed.TerminalizationOwner != TerminalizationOwnerPanelApply || completed.TerminalizationReceiptID != "rec_panel" || completed.TerminalizationTransitionID != "tr_panel" {
		t.Fatalf("caller terminalization proof not recorded: %+v", completed)
	}
}
