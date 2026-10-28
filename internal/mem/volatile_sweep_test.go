package mem

import (
	"strings"
	"testing"
	"time"
)

// sweepNow is the injected clock for every volatile-sweep test (never wall-clock;
// fixtures are all relative to it, so nothing here ages into a time bomb).
var sweepNow = t0.Add(48 * time.Hour)

// promoteVolatileClaim drives the REAL doors (Capture → Adjudicate) to land a
// current volatile claim with the given clock.
func promoteVolatileClaim(t *testing.T, s *Store, entity, object, expiresAt string) Claim {
	t.Helper()
	c := volatileBase()
	c.Entity = entity
	c.Object = object
	c.ExpiresAt = expiresAt
	got, err := s.Capture([]Candidate{c}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d candidates", len(got))
	}
	res, err := s.Adjudicate(got[0].ID, "promote", "work_test_volsweep", t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil {
		t.Fatal("promotion returned no claim")
	}
	return *res.Claim
}

// plantTemperature writes a real temperature projection file — the sweep reads
// it as data, exactly like the rank path does.
func plantTemperature(t *testing.T, s *Store, entries ...TemperatureEntry) {
	t.Helper()
	if _, err := WriteTemperature(s.home, TemperatureProjection{
		SchemaVersion: TemperatureSchemaVersion,
		ProjectionRef: TemperatureProjectionRef,
		ComputedAt:    rfc3339(sweepNow),
		Entries:       entries,
	}); err != nil {
		t.Fatal(err)
	}
}

func currentByEntity(t *testing.T, s *Store, entity string) Claim {
	t.Helper()
	claims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := s.supersededAsOf(s.maxSeq(claims))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if c.Entity == entity && !superseded[c.ID] {
			return c
		}
	}
	t.Fatalf("no current claim for entity %s", entity)
	return Claim{}
}

// Falsifier (c): a volatile claim consumed within the window (net>0, fresh
// LastInjected) must have its clock extended by the sweep — a planted
// reinforced-volatile fixture that still expires is the bug.
func TestVolatileSweepReinforcedExtendsClock(t *testing.T) {
	s := newStore(t)
	lastInjected := sweepNow.Add(-2 * 24 * time.Hour)
	claim := promoteVolatileClaim(t, s, "proj:reinforced", "hot fact", rfc3339(sweepNow.Add(5*24*time.Hour)))
	plantTemperature(t, s, TemperatureEntry{
		ClaimID: claim.ID, Injected: 3, Consumed: 2, Ignored: 0,
		LastInjected: rfc3339(lastInjected),
		Temperature:  0.2,
	})

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileExtendedCount != 1 {
		t.Fatalf("extended = %d, want 1; actions=%+v", report.VolatileExtendedCount, report.VolatileActions)
	}
	cur := currentByEntity(t, s, "proj:reinforced")
	if cur.ID == claim.ID {
		t.Fatalf("extension must append a new version, still current: %s", cur.ID)
	}
	if cur.Visibility != VisibilityVolatile {
		t.Fatalf("extended claim visibility = %q, want volatile", cur.Visibility)
	}
	wantClock := rfc3339(lastInjected.Add(30 * 24 * time.Hour))
	if cur.ExpiresAt != wantClock {
		t.Fatalf("extended clock = %q, want LastInjected+30d = %q", cur.ExpiresAt, wantClock)
	}
	if cur.Supersedes != claim.ID {
		t.Fatalf("extension must supersede the prior version, got supersedes=%q", cur.Supersedes)
	}
}

// Falsifier (g) — the deep-reasoner amendment: an injected-but-always-ignored
// volatile claim (net<=0) must STILL expire despite a fresh LastInjected.
// Injection recency is not consumption; Consumed>=1 alone would make this
// claim immortal.
func TestVolatileSweepIgnoredStillExpires(t *testing.T) {
	s := newStore(t)
	claim := promoteVolatileClaim(t, s, "proj:ignored", "stale fact", rfc3339(sweepNow.Add(-24*time.Hour)))
	plantTemperature(t, s, TemperatureEntry{
		ClaimID: claim.ID, Injected: 5, Consumed: 1, Ignored: 3,
		LastInjected: rfc3339(sweepNow.Add(-24 * time.Hour)), // fresh — but net <= 0
		Temperature:  -0.1,
	})

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileCooledCount != 1 {
		t.Fatalf("cooled = %d, want 1 (ignored-forever must not be immortal); actions=%+v", report.VolatileCooledCount, report.VolatileActions)
	}
	cur := currentByEntity(t, s, "proj:ignored")
	if cur.Visibility != "retired" || cur.CooledReason != CooledReasonVolatileExpired {
		t.Fatalf("expected cooled current version, got visibility=%q cooled_reason=%q", cur.Visibility, cur.CooledReason)
	}
}

// Falsifier (d), strengthened per review: expiry cools (supersedes to retired
// with the named reason), never deletes — the warm version stays in ledger
// history; the undo verb restores volatile with a FRESH clock; and the restored
// claim SURVIVES a subsequent sweep un-re-cooled (the stale-clock oscillation
// is the bug).
func TestVolatileSweepExpiryCoolsUndoRestores(t *testing.T) {
	s := newStore(t)
	claim := promoteVolatileClaim(t, s, "proj:expired", "cold fact", rfc3339(sweepNow.Add(-24*time.Hour)))
	// no temperature entry at all: zero reinforcement

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileCooledCount != 1 || len(report.VolatileActions) != 1 {
		t.Fatalf("cooled = %d actions=%+v, want exactly 1", report.VolatileCooledCount, report.VolatileActions)
	}
	action := report.VolatileActions[0]
	if action.Action != "cooled" || action.Reason != CooledReasonVolatileExpired {
		t.Fatalf("action = %+v, want cooled/%s", action, CooledReasonVolatileExpired)
	}
	if !strings.Contains(action.UndoCommand, "volatile-restore") || !strings.Contains(action.UndoCommand, action.NewClaimID) {
		t.Fatalf("cooled action must carry a one-line undo naming the cooled version, got %q", action.UndoCommand)
	}

	// Decay, never delete: BOTH versions are in the raw ledger history.
	raw, err := s.loadClaimsRaw()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range raw {
		ids[c.ID] = true
	}
	if !ids[claim.ID] || !ids[action.NewClaimID] {
		t.Fatalf("expired volatile vanished from ledger history: have=%v want both %s and %s", ids, claim.ID, action.NewClaimID)
	}

	// Undo: restore appends a fresh-clocked volatile version.
	restoreNow := sweepNow.Add(6 * time.Hour)
	restored, err := s.VolatileRestore(action.NewClaimID, restoreNow)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Visibility != VisibilityVolatile || restored.CooledReason != "" {
		t.Fatalf("restored claim = visibility %q cooled_reason %q, want volatile/empty", restored.Visibility, restored.CooledReason)
	}
	wantClock := rfc3339(restoreNow.Add(30 * 24 * time.Hour))
	if restored.ExpiresAt != wantClock {
		t.Fatalf("restored clock = %q, want FRESH now+30d = %q (restoring the stale clock re-cools next sweep)", restored.ExpiresAt, wantClock)
	}

	// Strengthened (d): the restored claim survives the next sweep un-re-cooled.
	report2, err := s.SynthesisSweep([]string{"volatile"}, restoreNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report2.VolatileCooledCount != 0 {
		t.Fatalf("restored claim re-cooled by next sweep (oscillation): %+v", report2.VolatileActions)
	}
	cur := currentByEntity(t, s, "proj:expired")
	if cur.ID != restored.ID {
		t.Fatalf("current version = %s, want restored %s", cur.ID, restored.ID)
	}
}

// Falsifier (e): the sweep is idempotent — a second run over unchanged claims +
// temperature produces zero new expiries and zero new immunity extensions.
func TestVolatileSweepIdempotent(t *testing.T) {
	s := newStore(t)
	lastInjected := sweepNow.Add(-2 * 24 * time.Hour)
	warm := promoteVolatileClaim(t, s, "proj:warm", "warm fact", rfc3339(sweepNow.Add(5*24*time.Hour)))
	promoteVolatileClaim(t, s, "proj:cold", "cold fact", rfc3339(sweepNow.Add(-24*time.Hour)))
	plantTemperature(t, s, TemperatureEntry{
		ClaimID: warm.ID, Injected: 3, Consumed: 2, Ignored: 0,
		LastInjected: rfc3339(lastInjected),
	})

	first, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if first.VolatileExtendedCount != 1 || first.VolatileCooledCount != 1 {
		t.Fatalf("first sweep extended=%d cooled=%d, want 1/1: %+v", first.VolatileExtendedCount, first.VolatileCooledCount, first.VolatileActions)
	}
	second, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if second.VolatileExtendedCount != 0 || second.VolatileCooledCount != 0 {
		t.Fatalf("second sweep must be a no-op, got extended=%d cooled=%d: %+v", second.VolatileExtendedCount, second.VolatileCooledCount, second.VolatileActions)
	}
}

// Falsifier (f), sweep half: a doctrine-class volatile claim (write guard
// bypassed — planted straight into the ledger) is CARDED by the sweep, never
// silently cooled: doctrine never auto-cools.
func TestVolatileSweepCardsDoctrineClassNeverCools(t *testing.T) {
	s := newStore(t)
	var planted Claim
	err := s.withLock(func() error {
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		planted = Claim{
			ID: "claim_planted_doctrine", Seq: seq,
			Entity: "proj:doctrine", Predicate: "has_rule", Object: "never do X",
			ValidFrom: rfc3339(t0), Confidence: 0.9, Derivation: "deterministic",
			ClaimType: "decision", EvidenceRefs: []string{"ev_planted"},
			Visibility: VisibilityVolatile, ExpiresAt: rfc3339(sweepNow.Add(-24 * time.Hour)),
			CreatedAt: rfc3339(t0),
		}
		return appendJSONL(s.claimsLog(), &planted)
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileCooledCount != 0 {
		t.Fatalf("doctrine-class volatile was silently cooled: %+v", report.VolatileActions)
	}
	if report.VolatileCardedCount != 1 {
		t.Fatalf("carded = %d, want 1 (surfaced, not silent); actions=%+v", report.VolatileCardedCount, report.VolatileActions)
	}
	cur := currentByEntity(t, s, "proj:doctrine")
	if cur.ID != planted.ID || cur.Visibility != VisibilityVolatile {
		t.Fatalf("doctrine claim must be untouched, got %+v", cur)
	}
}

// Gated-tier valve (deep-reasoner Q2iii): an expired volatile claim in the
// deliberately-unresolved gated tier (product_fact / unclassified) is carded,
// not silently cooled — only low_risk is authorized for silent state change.
func TestVolatileSweepCardsGatedTierNeverSilentlyCools(t *testing.T) {
	s := newStore(t)
	c := volatileBase()
	c.Entity = "proj:gated"
	c.CanonClass = "product_fact"
	c.ExpiresAt = rfc3339(sweepNow.Add(-24 * time.Hour))
	got, err := s.Capture([]Candidate{c}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(got[0].ID, "promote", "work_test_volsweep", t0); err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileCooledCount != 0 || report.VolatileCardedCount != 1 {
		t.Fatalf("gated tier must card not cool: cooled=%d carded=%d %+v", report.VolatileCooledCount, report.VolatileCardedCount, report.VolatileActions)
	}
	cur := currentByEntity(t, s, "proj:gated")
	if cur.Visibility != VisibilityVolatile {
		t.Fatalf("gated volatile must be untouched, got visibility %q", cur.Visibility)
	}
}

// The restore verb's own guards: only a current, volatile-cooled version can be
// restored; double-undo refuses.
func TestVolatileRestoreGuards(t *testing.T) {
	s := newStore(t)
	claim := promoteVolatileClaim(t, s, "proj:guards", "fact", rfc3339(sweepNow.Add(-24*time.Hour)))

	// Not cooled yet → refuse.
	if _, err := s.VolatileRestore(claim.ID, sweepNow); err == nil {
		t.Fatal("restore of a non-cooled claim must refuse")
	}

	report, err := s.SynthesisSweep([]string{"volatile"}, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	cooledID := report.VolatileActions[0].NewClaimID

	if _, err := s.VolatileRestore(cooledID, sweepNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Double-undo: the cooled version is no longer current → refuse.
	if _, err := s.VolatileRestore(cooledID, sweepNow.Add(2*time.Hour)); err == nil {
		t.Fatal("double restore must refuse (cooled version no longer current)")
	}
}

// The volatile stage is part of the default sweep — running `synthesis sweep`
// with no --stage flag exercises the clock loop.
func TestSynthesisSweepDefaultStagesIncludeVolatile(t *testing.T) {
	stages, stageSet, err := normalizeSynthesisStages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stageSet["volatile"] {
		t.Fatalf("default sweep stages %v must include volatile", stages)
	}
}
