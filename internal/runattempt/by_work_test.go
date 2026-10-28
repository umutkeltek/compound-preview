package runattempt

import (
	"strings"
	"testing"
	"time"
)

var byWorkNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func TestForWorkReturnsExactlyThatWorkFreshlyValidated(t *testing.T) {
	s := Open(t.TempDir())
	ra1, err := s.Mint("work_bywork_alpha", "codex:a", "claude-code", byWorkNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mint("work_bywork_beta", "codex:b", "claude-code", byWorkNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ra1.ID, byWorkNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ForWork("work_bywork_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ra1.ID {
		t.Fatalf("ForWork = %+v, want exactly %s", got, ra1.ID)
	}
	if got[0].LastHeartbeatAt == nil || !got[0].LastHeartbeatAt.Equal(byWorkNow.Add(time.Minute)) {
		t.Fatalf("ForWork served a stale snapshot: %+v", got[0].LastHeartbeatAt)
	}
}

func TestForWorkPreservesReverseOnlyPreparedCloseDiscovery(t *testing.T) {
	s := Open(t.TempDir())
	// Minted but NEVER linked onto any envelope — only the reverse direction
	// (work_id on the record) can find it.
	ra, err := s.Mint("work_bywork_reverse", "codex:a", "claude-code", byWorkNow)
	if err != nil {
		t.Fatal(err)
	}
	intent := TerminalizationIntent{
		SchemaVersion:             TerminalizationIntentSchemaVersion,
		Owner:                     TerminalizationOwnerWorkClose,
		WorkID:                    "work_bywork_reverse",
		AuthorityActionIntentID:   "act_bywork00000000000000000000001",
		AuthorityPolicyDecisionID: "pol_bywork00000000000000000000001",
		ExpectedWorkState:         "claimed",
		ExpectedWorkSHA256:        "sha256:" + strings.Repeat("1", 64),
		CloseInputSHA256:          "sha256:" + strings.Repeat("2", 64),
		CloseActionIntentID:       "act_bywork00000000000000000000002",
		ClosePolicyDecisionID:     "pol_bywork00000000000000000000002",
		Outcome:                   "completed",
		TargetStatus:              StatusSucceeded,
		ReceiptID:                 "rec_bywork000000000000000000000001",
		TransitionID:              "tr_bywork0000000000000000000000001",
		PreparedAt:                byWorkNow.Add(time.Minute),
	}
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatal(err)
	}

	got, err := s.ForWork("work_bywork_reverse")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ra.ID || got[0].TerminalizationIntent == nil {
		t.Fatalf("reverse-only prepared close lost: %+v", got)
	}
}

func TestForWorkFindsLegacyAttemptsWithoutIndex(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	// A legacy writer publishes a main record without any newer metadata.
	legacy := RunAttempt{
		SchemaVersion: SchemaVersion,
		ID:            MintDeterministic("legacy-a", byWorkNow),
		WorkID:        "work_bywork_legacy",
		Actor:         "codex:old",
		Harness:       "claude-code",
		Status:        StatusRunning,
		CreatedAt:     byWorkNow,
	}
	if err := s.write(legacy); err != nil {
		t.Fatal(err)
	}

	got, err := s.ForWork("work_bywork_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != legacy.ID {
		t.Fatalf("legacy fallback = %+v, want %s", got, legacy.ID)
	}
}

func TestForWorkIncludesLegacyAndNewAttempts(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	legacy := RunAttempt{
		SchemaVersion: SchemaVersion,
		ID:            MintDeterministic("legacy-seed", byWorkNow),
		WorkID:        "work_bywork_seed",
		Actor:         "codex:old",
		Harness:       "claude-code",
		Status:        StatusSucceeded,
		CreatedAt:     byWorkNow,
	}
	if err := s.write(legacy); err != nil {
		t.Fatal(err)
	}

	fresh, err := s.Mint("work_bywork_seed", "codex:new", "claude-code", byWorkNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The seed must be lossless: the legacy attempt and the new one are both
	// listed, so ForWork never orphans pre-index records behind the fast path.
	got, err := s.ForWork("work_bywork_seed")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("seeded ForWork = %+v, want both the legacy and the new attempt", got)
	}
	if got[0].ID != legacy.ID || got[1].ID != fresh.ID {
		t.Fatalf("seeded order = [%s %s], want [%s %s]", got[0].ID, got[1].ID, legacy.ID, fresh.ID)
	}
}
