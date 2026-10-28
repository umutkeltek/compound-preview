package promotedriver

import (
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// writeFrozenParkPass builds a planned + recovery_required pair on disk
// that points at the existing candID, sealed so loadFrozenJournal treats
// it as a replayable pass. The candidate file is left untouched so the
// "frozen replay runs against an unchanged target" scenarios the red
// tests pin are exercised faithfully. The first call also drives a
// seeder pass (which parks the original candID and writes a committed
// receipt) so the test can reuse the existing seeded fixture.
//
// Returns the pass id under which the splice was written — the recovery
// tests use it to assert that the same pass ends up committed.
func writeFrozenParkPass(t *testing.T, home, candID string, now time.Time) string {
	t.Helper()

	_, err := Run(home, Options{Apply: true, WorkID: "work_r3f_seeder", SweepScanLimit: 100}, now)
	if err != nil {
		t.Fatalf("seeder pass: %v", err)
	}
	receipts := readReceipts(t, home)
	var seedIntent *PassIntent
	for _, r := range receipts {
		if r.Stage == stagePlanned && r.Intent != nil && len(r.Intent.Operations) > 0 {
			seedIntent = r.Intent
			break
		}
	}
	if seedIntent == nil {
		t.Fatal("seeder pass did not journal a planned frame")
	}

	targetCand := "cand_4198be82cb51"
	seedCandidate(t, home, targetCand, mem.Candidate{
		Entity:    "umut",
		Predicate: "preference.repeated-tool-choice",
		Object:    "context7",
		ClaimType: "preference",
	})
	targetRec, err := candidates.ReadRecord(home, candidates.Memory, targetCand)
	if err != nil {
		t.Fatalf("read target candidate: %v", err)
	}

	intent := &PassIntent{
		WorkID:                    "work_r3f_repro",
		CandidateIDs:              []string{targetCand},
		CandidateCorpusGeneration: seedIntent.CandidateCorpusGeneration,
		PendingGeneration:         seedIntent.PendingGeneration,
		ExecutionAt:               rfc3339(now),
		PromotePlan:               seedIntent.PromotePlan,
		DedupePlan:                seedIntent.DedupePlan,
		SweepPlan:                 seedIntent.SweepPlan,
		Operations: []PassIntentOperation{{
			Phase:     "sweep",
			Operation: candidates.Park,
			Target: CandidatePrecondition{
				ID:          targetCand,
				EntryCursor: targetRec.EntryCursor,
				BodyDigest:  targetRec.Digest,
				State:       "proposed",
			},
			DedupeKey: "",
			Ordinal:   0,
		}},
	}
	passID := "pass_r3f_replay_" + now.Format("20060102T150405.000000000Z")
	if err := sealPassIntent(passID, intent); err != nil {
		t.Fatalf("seal intent: %v", err)
	}
	planned := Report{
		SchemaVersion: ReportSchemaVersion,
		PassID:        passID,
		Home:          home,
		DryRun:        false,
		StartedAt:     rfc3339(now),
		FinishedAt:    rfc3339(now),
		Stage:         stagePlanned,
		Intent:        intent,
	}
	if err := appendReceipt(home, planned); err != nil {
		t.Fatalf("journal planned: %v", err)
	}
	recovery := Report{
		SchemaVersion: ReportSchemaVersion,
		PassID:        passID,
		Home:          home,
		DryRun:        false,
		StartedAt:     rfc3339(now),
		FinishedAt:    rfc3339(now.Add(time.Second)),
		Stage:         stageRecoveryRequired,
		Intent:        intent,
		Error:         "synthetic_freeze_for_r3f_reproduction",
	}
	if err := appendReceipt(home, recovery); err != nil {
		t.Fatalf("journal recovery: %v", err)
	}
	return passID
}
