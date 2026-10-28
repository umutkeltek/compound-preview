package work

import (
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

// Falsifier: sweep-stale must RE-SEAL every envelope it mutates.
//
// WHY THIS TEST EXISTS. sweep-stale rewrites a stale work envelope's state and appends a
// lifecycle event. It used to append that event WITHOUT a work_sha256. The autonomy
// director verifies a terminal work by re-hashing the envelope on disk and comparing to
// the most recent recorded work_sha256, so an unsealed mutation leaves the newest seal
// describing PRE-sweep bytes. The director's custody check then fail-closes — and it
// vetoes the whole cohort rather than excluding the one sample, so a single swept
// envelope silently disabled `director propose` entirely.
//
// That is not hypothetical: a sweep pass reclaimed 71 envelopes (40 cancelled, 31 failed)
// and the autonomy heartbeat failed on every subsequent hourly run with
// "recent terminal work <id> snapshot hash does not match terminal event", while its other
// three stages kept returning rc=0 — one governed subsystem disabling another with no
// error surfaced at the writer.
//
// Without the seal this test reds on the empty work_sha256.
func TestSweepStaleSealsReclaimedEnvelopeForCustodyVerification(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)

	rep, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(rep.Candidates) != 1 || !rep.Candidates[0].Reclaimed {
		t.Fatalf("expected exactly one reclaimed candidate, got %+v", rep.Candidates)
	}

	// Hash the post-sweep envelope exactly the way the director does.
	var swept envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(w.ID), &swept); err != nil {
		t.Fatalf("read swept envelope: %v", err)
	}
	wantSHA, err := workEnvelopeSHA256(swept)
	if err != nil {
		t.Fatalf("hash swept envelope: %v", err)
	}

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sweepEvent *Event
	for i := range events {
		if events[i].By == "sweep-stale" {
			sweepEvent = &events[i]
		}
	}
	if sweepEvent == nil {
		t.Fatal("no sweep-stale lifecycle event was appended")
	}

	if sweepEvent.WorkSHA256 == "" {
		t.Fatal("sweep-stale event carries no work_sha256: the envelope it mutated is left " +
			"UNSEALED, so the autonomy director's custody check fail-closes and vetoes " +
			"`director propose` for every cohort containing this work")
	}
	if sweepEvent.WorkSHA256 != wantSHA {
		t.Fatalf("seal does not describe the envelope on disk:\n  event = %s\n  actual= %s",
			sweepEvent.WorkSHA256, wantSHA)
	}

	// The pre-image seal must be present and must differ, or the transition was not
	// actually captured and a reader cannot tell what changed.
	if sweepEvent.PreviousWorkSHA256 == "" {
		t.Fatal("sweep-stale event carries no previous_work_sha256: the pre-sweep state is unrecoverable")
	}
	if sweepEvent.PreviousWorkSHA256 == sweepEvent.WorkSHA256 {
		t.Fatal("previous and current seals are identical; a state change must change the hash")
	}
}

// Falsifier: the seal must survive the round trip the director actually performs —
// read envelope from disk, re-marshal, hash, compare against the LATEST recorded hash.
//
// This is the end-to-end shape of the production check. It reds if sweep-stale ever again
// mutates an envelope without recording a matching hash, even if the event carries some
// other non-empty value.
func TestSweepStaleReclaimedEnvelopeVerifiesAgainstLatestRecordedSeal(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	w := staleEnvelope(t, s, "verifying", 30*24*time.Hour, now)

	if _, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), now); err != nil {
		t.Fatalf("apply: %v", err)
	}

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	// The director takes the MOST RECENT non-empty work_sha256 as the custody seal.
	latest := ""
	for _, ev := range events {
		if ev.WorkSHA256 != "" {
			latest = ev.WorkSHA256
		}
	}
	if latest == "" {
		t.Fatal("no lifecycle event records a custody seal for this work at all")
	}

	var onDisk envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(w.ID), &onDisk); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	actual, err := workEnvelopeSHA256(onDisk)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	if latest != actual {
		t.Fatalf("custody broken after sweep — this is the exact failure the director reports as "+
			"%q:\n  latest sealed = %s\n  envelope now  = %s", "snapshot hash does not match terminal event", latest, actual)
	}
}
