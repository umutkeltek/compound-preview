package work

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

// The legacy damage this door repairs CANNOT be produced through the public
// write doors anymore (e5ecfd35 taught sweep-stale to re-seal). The fixtures
// below therefore bypass the write door deliberately — hand-mutating the
// envelope file and appending an UNSEALED terminal event through the
// in-package seam — to reproduce the exact on-disk shape the 2026-08-28
// sweep left behind (envelope rewritten to a terminal state, terminal event
// appended without work_sha256, newest recorded seal still describing the
// pre-sweep bytes).

// damageEnvelopeLikeLegacySweep rewrites the envelope file to the target
// state and appends an UNSEALED claimed->cancelled event, exactly the shape
// sweep-stale produced before e5ecfd35.
func damageEnvelopeLikeLegacySweep(t *testing.T, s *Store, workID string, from, to string, now time.Time) (preDamageSHA string) {
	t.Helper()
	var current envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(workID), &current); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	preDamageSHA, err := workEnvelopeSHA256(current)
	if err != nil {
		t.Fatalf("seal pre-damage envelope: %v", err)
	}
	current.State = to
	if err := os.WriteFile(s.workPath(workID), mustMarshalEnvelope(t, current), 0o644); err != nil {
		t.Fatalf("write damaged envelope: %v", err)
	}
	if err := s.appendEvent(Event{TS: ts(now), WorkID: workID, From: from, To: to, By: "sweep-stale", Note: "legacy unsealed reclaim"}); err != nil {
		t.Fatalf("append unsealed terminal event: %v", err)
	}
	return preDamageSHA
}

func mustMarshalEnvelope(t *testing.T, work envelope.WorkEnvelope) []byte {
	t.Helper()
	body, err := json.Marshal(work)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func resealFixture(t *testing.T, now time.Time) *Store {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// Falsifier 1: the dry run must list every terminal envelope whose recorded
// seal disagrees with its bytes — id, stored hash, computed hash — and the
// default must not mutate anything.
func TestResealDryRunListsSealMismatchWithoutMutating(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := resealFixture(t, now)

	victim, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "reseal victim"}, now)
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if _, err := s.Claim(victim.ID, "test-fixture", now); err != nil {
		t.Fatalf("claim victim: %v", err)
	}
	damageEnvelopeLikeLegacySweep(t, s, victim.ID, "claimed", "cancelled", now)

	healthy, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "reseal healthy"}, now)
	if err != nil {
		t.Fatalf("create healthy: %v", err)
	}
	if _, err := s.Advance(healthy.ID, AdvanceInput{To: "ready", Actor: "test-fixture"}, now); err != nil {
		t.Fatalf("advance healthy: %v", err)
	}
	if _, err := s.Claim(healthy.ID, "test-fixture", now); err != nil {
		t.Fatalf("claim healthy: %v", err)
	}
	if _, err := s.Advance(healthy.ID, AdvanceInput{To: "running", Actor: "test-fixture"}, now); err != nil {
		t.Fatalf("advance healthy to running: %v", err)
	}
	if _, err := s.Close(healthy.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "test-fixture", Changed: true}, now); err != nil {
		t.Fatalf("close healthy: %v", err)
	}

	eventsBefore, err := s.eventsFor("")
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	rep, err := s.ResealPlan("test-fixture", now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Mismatched != 1 || len(rep.Candidates) != 1 {
		t.Fatalf("expected exactly one mismatch candidate, got %+v", rep)
	}
	c := rep.Candidates[0]
	if c.WorkID != victim.ID {
		t.Fatalf("candidate is %s, want victim %s", c.WorkID, victim.ID)
	}
	if c.State != "cancelled" {
		t.Fatalf("candidate state %q, want cancelled", c.State)
	}
	if c.StoredWorkSHA256 == "" || c.ComputedWorkSHA256 == "" {
		t.Fatalf("candidate must list stored and computed hashes, got %+v", c)
	}
	if c.StoredWorkSHA256 == c.ComputedWorkSHA256 {
		t.Fatal("stored and computed hashes are identical; fixture did not reproduce a mismatch")
	}

	eventsAfter, err := s.eventsFor("")
	if err != nil {
		t.Fatalf("re-read events: %v", err)
	}
	if len(eventsBefore) != len(eventsAfter) {
		t.Fatalf("dry run mutated the event ledger: %d -> %d events", len(eventsBefore), len(eventsAfter))
	}
}

// Falsifier 2: --yes must repair by appending ONE sealed same-state event —
// never rewriting envelope bytes or history — and a second dry run must show
// zero. This is the round trip the director performs, so after the repair
// the latest recorded seal must equal the envelope hash.
func TestResealApplyAppendsSealedEventAndSecondPassIsClean(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := resealFixture(t, now)

	victim, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "reseal apply victim"}, now)
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if _, err := s.Claim(victim.ID, "test-fixture", now); err != nil {
		t.Fatalf("claim victim: %v", err)
	}
	damageEnvelopeLikeLegacySweep(t, s, victim.ID, "claimed", "cancelled", now)

	beforeBytes, err := os.ReadFile(s.workPath(victim.ID))
	if err != nil {
		t.Fatalf("read envelope bytes: %v", err)
	}
	eventsBefore, err := s.eventsFor(victim.ID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	rep, err := s.ResealApply("test-fixture", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Resealed != 1 || rep.Mismatched != 1 {
		t.Fatalf("expected one resealed of one mismatched, got %+v", rep)
	}

	afterBytes, err := os.ReadFile(s.workPath(victim.ID))
	if err != nil {
		t.Fatalf("re-read envelope bytes: %v", err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("reseal rewrote the envelope file; the repair must be append-only")
	}
	eventsAfter, err := s.eventsFor(victim.ID)
	if err != nil {
		t.Fatalf("re-read events: %v", err)
	}
	if len(eventsAfter) != len(eventsBefore)+1 {
		t.Fatalf("reseal appended %d events, want exactly 1", len(eventsAfter)-len(eventsBefore))
	}
	appended := eventsAfter[len(eventsAfter)-1]
	if appended.From != "cancelled" || appended.To != "cancelled" {
		t.Fatalf("reseal event is %s->%s, want same-state cancelled->cancelled (an annotation, never a second terminal transition)", appended.From, appended.To)
	}

	// The director's round trip: latest recorded seal must now verify.
	var onDisk envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(victim.ID), &onDisk); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	want, err := workEnvelopeSHA256(onDisk)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	latest := ""
	for _, ev := range eventsAfter {
		if ev.WorkSHA256 != "" {
			latest = ev.WorkSHA256
		}
	}
	if latest != want {
		t.Fatalf("custody still broken after reseal:\n  latest sealed = %s\n  envelope now  = %s", latest, want)
	}
	if appended.WorkSHA256 != want || appended.PreviousWorkSHA256 == "" || appended.PreviousWorkSHA256 == want {
		t.Fatalf("reseal event seals are wrong: work=%s previous=%s want=%s", appended.WorkSHA256, appended.PreviousWorkSHA256, want)
	}

	second, err := s.ResealPlan("test-fixture", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if second.Mismatched != 0 || len(second.Candidates) != 0 {
		t.Fatalf("second dry run must show zero mismatches, got %+v", second)
	}
}

// Non-terminal envelopes are never candidates: a live writer owns them and
// the ordinary doors re-seal them.
func TestResealSkipsNonTerminalEnvelopes(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := resealFixture(t, now)

	w, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "reseal live"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "test-fixture", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Mutate the claimed envelope's bytes without a seal: exactly the
	// damage shape, but in a NON-terminal state.
	var current envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(w.ID), &current); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	current.DesiredOutcome = "hand-mutated live work"
	if err := os.WriteFile(s.workPath(w.ID), mustMarshalEnvelope(t, current), 0o644); err != nil {
		t.Fatalf("mutate envelope: %v", err)
	}

	rep, err := s.ResealPlan("test-fixture", now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, c := range rep.Candidates {
		if c.WorkID == w.ID {
			t.Fatalf("non-terminal envelope %s became a reseal candidate: %+v", w.ID, c)
		}
	}
}

// The CLI edge: dry-run default exits 0 without mutating; --yes repairs; a
// positional argument is refused.
func TestResealCLIEdges(t *testing.T) {
	now := time.Time{} // CLI default clock
	s := resealFixture(t, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))

	victim, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "reseal cli victim"}, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(victim.ID, "test-fixture", time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	damageEnvelopeLikeLegacySweep(t, s, victim.ID, "claimed", "cancelled", time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))

	var out strings.Builder
	code := RunWorkResealCLI([]string{"victim-id"}, s, now, &out, &out)
	if code != 2 || !strings.Contains(out.String(), "unexpected argument") {
		t.Fatalf("positional must be refused with usage, code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWorkResealCLI(nil, s, now, &out, &out)
	if code != 0 || !strings.Contains(out.String(), "mismatched=1") {
		t.Fatalf("default dry run must list the mismatch and exit 0, code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWorkResealCLI([]string{"--yes", "--by", "test-fixture"}, s, time.Date(2026, 9, 3, 12, 1, 0, 0, time.UTC), &out, &out)
	if code != 0 || !strings.Contains(out.String(), "resealed=1") {
		t.Fatalf("--yes must repair and exit 0, code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWorkResealCLI([]string{"--json"}, s, now, &out, &out)
	if code != 0 {
		t.Fatalf("json dry run exit=%d out=%q", code, out.String())
	}
	var report ResealReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("json output unparseable: %v", err)
	}
	if report.Mismatched != 0 {
		t.Fatalf("post-repair json dry run must report zero mismatches, got %+v", report)
	}
}
