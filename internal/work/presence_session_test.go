package work

import (
	"testing"
	"time"
)

// THE security property of this slice. Any local process can announce any
// identity, so a declaration standing alone must buy exactly nothing: it may
// not corroborate itself, and it may not conjure the claimed actor into the
// presence set. If corroboration ever starts reading announcements, this is
// the test that goes red.
func TestAnnouncementAloneNeverCorroboratesItself(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := fixedNow.Add(time.Minute)

	if _, err := s.AnnouncePresenceSession(PresenceSessionInput{
		Harness:   "claude_code",
		SessionID: "session-with-no-evidence",
		Cwd:       t.TempDir(),
		Actor:     "claude-code:impostor",
	}, now); err != nil {
		t.Fatal(err)
	}

	projection, err := s.PresenceProjectionFor(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	if len(projection.Sessions) != 1 {
		t.Fatalf("expected the announcement to be recorded: %+v", projection.Sessions)
	}
	row := projection.Sessions[0]
	if row.Corroborated {
		t.Fatalf("a self-declaration must never corroborate itself: %+v", row)
	}
	if row.Basis != PresenceCorroborationNone {
		t.Fatalf("basis = %q, want %q", row.Basis, PresenceCorroborationNone)
	}
	if projection.Counts.Corroborated != 0 {
		t.Fatalf("corroborated count = %d, want 0", projection.Counts.Corroborated)
	}
	// And it must not have manufactured presence for the claimed actor.
	if _, ok := findPresenceRow(projection, "claude-code:impostor"); ok {
		t.Fatalf("announcing must not place an actor in Present: %+v", projection.Present)
	}
}

// The other half of the conjunctive rule: with independent evidence, the
// declaration becomes usable.
func TestAnnouncementCorroboratedByIndependentLease(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "corroborate me"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "claude-code:real-agent", claimAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnouncePresenceSession(PresenceSessionInput{
		Harness:   "claude_code",
		SessionID: "session-of-the-real-agent",
		Cwd:       t.TempDir(),
		Actor:     "claude-code:real-agent",
	}, claimAt); err != nil {
		t.Fatal(err)
	}

	projection, err := s.PresenceProjectionFor(claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Sessions) != 1 {
		t.Fatalf("sessions = %+v", projection.Sessions)
	}
	row := projection.Sessions[0]
	if !row.Corroborated {
		t.Fatalf("a lease-holding actor should corroborate its session: %+v", row)
	}
	if row.Basis == PresenceCorroborationNone {
		t.Fatalf("basis should name real evidence: %+v", row)
	}
	if projection.Counts.Corroborated != 1 {
		t.Fatalf("corroborated count = %d, want 1", projection.Counts.Corroborated)
	}
}

// Evidence that has gone cold is not corroboration. Without this, an actor
// that held a lease weeks ago would keep validating a session forever.
func TestStaleActorEvidenceDoesNotCorroborate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "cold evidence"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, "codex:went-quiet", claimAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnouncePresenceSession(PresenceSessionInput{
		Harness:   "codex",
		SessionID: "long-running-session",
		Cwd:       t.TempDir(),
		Actor:     "codex:went-quiet",
	}, claimAt); err != nil {
		t.Fatal(err)
	}

	later := claimAt.Add(DefaultPresenceTTL + time.Hour)
	projection, err := s.PresenceProjectionFor(later)
	if err != nil {
		t.Fatal(err)
	}
	row := projection.Sessions[0]
	if row.Corroborated {
		t.Fatalf("evidence past the TTL must not corroborate: %+v", row)
	}
	if row.Basis != PresenceCorroborationStale {
		t.Fatalf("basis = %q, want %q", row.Basis, PresenceCorroborationStale)
	}
}

// Repeat announcements are the normal case (every hook fire could emit one),
// so they must refresh rather than accumulate.
func TestAnnouncementIsIdempotentPerSessionTriple(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cwd := t.TempDir()
	in := PresenceSessionInput{
		Harness:   "claude_code",
		SessionID: "repeat-session",
		Cwd:       cwd,
		Actor:     "claude-code:chatty",
	}
	first, err := s.AnnouncePresenceSession(in, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.AnnouncePresenceSession(in, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnouncePresenceSession(in, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	records, err := s.ListPresenceSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one record for one session triple, got %d: %+v", len(records), records)
	}
	if records[0].Announces != 3 {
		t.Fatalf("announce_count = %d, want 3", records[0].Announces)
	}
	if records[0].FirstSeenAt != first.FirstSeenAt {
		t.Fatalf("first_announced_at must be preserved: %q vs %q", records[0].FirstSeenAt, first.FirstSeenAt)
	}
	if records[0].LastSeenAt == third.FirstSeenAt && records[0].LastSeenAt != third.LastSeenAt {
		t.Fatalf("last_announced_at should advance: %+v", records[0])
	}
}

// One session may not be two actors at once. The key excludes the actor so a
// re-announcement REPLACES the claim; if the actor were keyed in, a session
// could accumulate rival identities and resolution would face two candidates.
func TestReannouncingSessionUnderNewActorReplacesTheClaim(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cwd := t.TempDir()
	base := PresenceSessionInput{Harness: "claude_code", SessionID: "shifty", Cwd: cwd}

	first := base
	first.Actor = "claude-code:identity-a"
	if _, err := s.AnnouncePresenceSession(first, fixedNow); err != nil {
		t.Fatal(err)
	}
	second := base
	second.Actor = "claude-code:identity-b"
	if _, err := s.AnnouncePresenceSession(second, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	records, err := s.ListPresenceSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("a session must hold exactly one claimed identity, got %d: %+v", len(records), records)
	}
	if records[0].Actor != "claude-code:identity-b" {
		t.Fatalf("latest declaration should win, got %q", records[0].Actor)
	}
}

// A declaration missing any coordinate is refused rather than stored partially.
func TestAnnouncementRequiresEveryCoordinate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cwd := t.TempDir()
	full := PresenceSessionInput{Harness: "claude_code", SessionID: "s", Cwd: cwd, Actor: "a"}

	for _, missing := range []string{"harness", "session", "cwd", "actor"} {
		in := full
		switch missing {
		case "harness":
			in.Harness = "  "
		case "session":
			in.SessionID = ""
		case "cwd":
			in.Cwd = ""
		case "actor":
			in.Actor = ""
		}
		if _, err := s.AnnouncePresenceSession(in, fixedNow); err == nil {
			t.Fatalf("announcement without %s must be refused", missing)
		}
	}
	records, err := s.ListPresenceSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("refused announcements must not be stored: %+v", records)
	}
}
