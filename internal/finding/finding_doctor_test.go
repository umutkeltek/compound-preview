package finding

import (
	"testing"
	"time"
)

// Doctor makes the direct-write finding home accountable: it flags superseded records (an id revised
// by a later finding) and stale live records, read-only. This proves both axes and that a superseded
// record is never double-counted as stale.
func TestDoctorFlagsSupersededAndStale(t *testing.T) {
	home := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	v1, err := Record(home, Finding{Topic: "runbook", Body: "v1 steps"}, base)
	if err != nil {
		t.Fatalf("record v1: %v", err)
	}
	v2, err := Record(home, Finding{Topic: "runbook", Body: "v2 steps", Supersedes: v1.ID}, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("record v2: %v", err)
	}

	now := base.Add(40 * 24 * time.Hour)

	// No age axis: v1 is superseded, v2 is live, nothing stale.
	rep, err := Doctor(home, now, 0)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if rep.Total != 2 || rep.Live != 1 {
		t.Fatalf("total=%d live=%d, want 2/1", rep.Total, rep.Live)
	}
	if len(rep.Superseded) != 1 || rep.Superseded[0].ID != v1.ID || rep.Superseded[0].SupersededBy != v2.ID {
		t.Fatalf("superseded wrong: %+v", rep.Superseded)
	}
	if len(rep.Stale) != 0 {
		t.Fatalf("staleDays=0 must disable the age axis, got %+v", rep.Stale)
	}

	// 30-day threshold: the LIVE v2 (~40d old) is stale; the superseded v1 is NOT double-flagged.
	repStale, err := Doctor(home, now, 30)
	if err != nil {
		t.Fatalf("doctor stale: %v", err)
	}
	if len(repStale.Stale) != 1 || repStale.Stale[0].ID != v2.ID {
		t.Fatalf("expected only live v2 flagged stale, got %+v", repStale.Stale)
	}
}
