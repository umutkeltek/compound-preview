package promotedriver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

func recoveredAbandonment(t *testing.T, home, schema, interruptStage string) []Report {
	t.Helper()
	partial := interruptStage == stageOperationRecorded
	if partial {
		seedCandidate(t, home, "cand_first", mem.Candidate{Entity: "first", Predicate: "value", Object: "first"})
	}
	seedCandidate(t, home, "cand_target", mem.Candidate{Entity: "target", Predicate: "value", Object: "before"})
	stateless := []byte(`{"schema_version":"compound.source_candidate.v0.1","id":"src_cand_stateless","kind":"memory","promote_allowed":false,"verdict":"no_promote"}`)
	if err := os.WriteFile(filepath.Join(home, candidates.InboxDir(candidates.Memory), "src_cand_stateless.json"), stateless, 0600); err != nil {
		t.Fatal(err)
	}
	previous := receiptAppender
	changed := false
	receiptAppender = func(homePath string, report Report) error {
		if err := appendReceipt(homePath, report); err != nil {
			return err
		}
		if report.Stage == interruptStage && !changed {
			changed = true
			seedCandidate(t, home, "cand_target", mem.Candidate{Entity: "target", Predicate: "value", Object: "after"})
			return errors.New("simulated interruption after durable journal frame")
		}
		return nil
	}
	t.Cleanup(func() { receiptAppender = previous })
	interrupted, err := Run(home, Options{Apply: true, WorkID: "work_abandon_fixture", SweepScanLimit: 100}, testNow)
	receiptAppender = previous
	if err == nil || !changed || (interrupted.Stage != stagePlanned && interrupted.Stage != stageRecoveryRequired) {
		t.Fatalf("interrupted pass stage=%q err=%v, want durable interrupted pass", interrupted.Stage, err)
	}
	frames := readReceipts(t, home)
	if schema != ReportSchemaVersion {
		for i := range frames {
			frames[i].SchemaVersion = schema
		}
		replaceAbandonmentJournal(t, home, frames)
	}
	recovered, err := Run(home, Options{Apply: true, Recover: true, WorkID: "work_abandon_recover"}, testNow.Add(time.Minute))
	if err != nil || recovered.Stage != stageAbandoned {
		t.Fatalf("recovery stage=%q err=%v, want abandoned", recovered.Stage, err)
	}
	if partial && len(recovered.OperationResults) != 1 {
		t.Fatalf("recovery lost partial operation evidence: %+v", recovered.OperationResults)
	}
	if interruptStage == stageFinalizationRecorded && recovered.Finalization == nil {
		t.Fatal("recovery lost recorded finalization evidence")
	}
	return readReceipts(t, home)
}

// Corrupt-frame tests deliberately bypass the journal writer: the native writer
// cannot produce the malformed history whose rejection these tests must prove.
func replaceAbandonmentJournal(t *testing.T, home string, frames []Report) {
	t.Helper()
	var body bytes.Buffer
	for _, frame := range frames {
		if err := json.NewEncoder(&body).Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, stateDirName, receiptFileName), body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredAbandonmentAllowsNextApply(t *testing.T) {
	for _, schema := range []string{frozenReportSchemaVersion, ReportSchemaVersion} {
		stages := []string{stagePlanned, stageOperationRecorded}
		if schema == ReportSchemaVersion {
			stages = append(stages, stageFinalizationRecorded)
		}
		for _, interruptStage := range stages {
			name := schema + "/" + interruptStage
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				frames := recoveredAbandonment(t, home, schema, interruptStage)
				abandoned := frames[len(frames)-1]
				path := filepath.Join(home, candidates.InboxDir(candidates.Memory), "cand_target.json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(before)
				report, err := Run(home, Options{Apply: true, WorkID: "work_abandon_successor", SweepScanLimit: 100}, testNow.Add(2*time.Minute))
				if err != nil || report.Stage != stageCommitted {
					t.Fatalf("next apply stage=%q err=%v, abandoned recovery must be readable terminal history", report.Stage, err)
				}
				if report.PassID == abandoned.PassID || report.Intent == nil || report.Intent.WorkID != "work_abandon_successor" {
					t.Fatalf("next apply reused abandoned intent: %+v", report.Intent)
				}
				found := false
				for _, op := range report.Intent.Operations {
					if interruptStage == stageOperationRecorded && op.Target.ID == "cand_first" {
						t.Fatal("next apply replayed the already recorded partial operation")
					}
					if op.Target.ID == "src_cand_stateless" {
						t.Fatal("next apply planned a stateless source operation")
					}
					if op.Target.ID == "cand_target" {
						found = true
						if op.Target.BodyDigest != "sha256:"+hex.EncodeToString(digest[:]) {
							t.Fatal("next apply did not bind the current candidate generation")
						}
					}
				}
				if !found {
					t.Fatal("next apply did not plan fresh work for the changed target")
				}
				var current mem.Candidate
				if err := candidates.ReadCandidate(home, candidates.Memory, "cand_target", &current); err != nil {
					t.Fatal(err)
				}
				if current.Object != "after" || current.State != "parked" {
					t.Fatalf("fresh generation was overwritten: %+v", current)
				}
				oldFrames := passReceipts(t, readReceipts(t, home), abandoned.PassID)
				if oldFrames[len(oldFrames)-1].Stage != stageAbandoned {
					t.Fatal("next apply rewrote abandonment as successful replay")
				}
			})
		}
	}
}

func TestMalformedAbandonmentRefusesBeforePlanning(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"schema", func(r *Report) { r.SchemaVersion = frozenReportSchemaVersion }},
		{"home", func(r *Report) { r.Home = filepath.Join(r.Home, "other") }},
		{"dry_run", func(r *Report) { r.DryRun = true }},
		{"finished_at", func(r *Report) { r.FinishedAt = testNow.Add(-time.Second).Format(time.RFC3339) }},
		{"missing_intent", func(r *Report) { r.Intent = nil }},
		{"changed_intent", func(r *Report) { r.Intent.WorkID = "work_other"; sealPassIntent(r.PassID, r.Intent) }},
		{"missing_reason", func(r *Report) { r.Error = "" }},
		{"locked", func(r *Report) { r.Locked = true }},
		{"summary", func(r *Report) { r.Promote.Promoted++ }},
		{"missing_partial_result", func(r *Report) { r.OperationResults = nil }},
		{"changed_partial_result", func(r *Report) { r.OperationResults[0].Key = "op_other" }},
		{"finalization", func(r *Report) { r.Finalization = &PassFinalizationResult{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			frames := recoveredAbandonment(t, home, ReportSchemaVersion, stageOperationRecorded)
			tc.mutate(&frames[len(frames)-1])
			replaceAbandonmentJournal(t, home, frames)
			journalPath := filepath.Join(home, stateDirName, receiptFileName)
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			states := candidateStatesDirect(t, home)
			report, err := Run(home, Options{Apply: true, WorkID: "work_abandon_invalid"}, testNow.Add(2*time.Minute))
			if err == nil {
				t.Fatalf("malformed abandonment accepted: stage=%q", report.Stage)
			}
			after, readErr := os.ReadFile(journalPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("malformed history was changed: %v", readErr)
			}
			for id, state := range states {
				if got := candidateStatesDirect(t, home)[id]; got != state {
					t.Fatalf("malformed history changed candidate %s: %s -> %s", id, state, got)
				}
			}
		})
	}
}

func TestAbandonmentPreservesRecordedFinalization(t *testing.T) {
	for _, mutation := range []string{"missing", "changed"} {
		t.Run(mutation, func(t *testing.T) {
			home := t.TempDir()
			frames := recoveredAbandonment(t, home, ReportSchemaVersion, stageFinalizationRecorded)
			abandoned := &frames[len(frames)-1]
			if mutation == "missing" {
				abandoned.Finalization = nil
			} else {
				abandoned.Finalization.PromoteCursorAfter = "changed"
			}
			replaceAbandonmentJournal(t, home, frames)
			journalPath := filepath.Join(home, stateDirName, receiptFileName)
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			states := candidateStatesDirect(t, home)
			report, err := Run(home, Options{Apply: true, WorkID: "work_abandon_finalization_invalid"}, testNow.Add(2*time.Minute))
			if err == nil {
				t.Fatalf("changed recorded finalization accepted: stage=%q", report.Stage)
			}
			after, readErr := os.ReadFile(journalPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("malformed finalization history changed: %v", readErr)
			}
			for id, state := range states {
				if got := candidateStatesDirect(t, home)[id]; got != state {
					t.Fatalf("malformed history changed candidate %s: %s -> %s", id, state, got)
				}
			}
		})
	}
}

func TestAbandonmentRequiresPlanAndClosesHistory(t *testing.T) {
	for _, shape := range []string{"missing_plan", "duplicate_abandonment", "operation_after_abandonment"} {
		t.Run(shape, func(t *testing.T) {
			home := t.TempDir()
			frames := recoveredAbandonment(t, home, ReportSchemaVersion, stageOperationRecorded)
			switch shape {
			case "missing_plan":
				frames = frames[len(frames)-1:]
			case "duplicate_abandonment":
				frames = append(frames, frames[len(frames)-1])
			case "operation_after_abandonment":
				if frames[1].Stage != stageOperationRecorded {
					t.Fatalf("fixture operation frame has stage %q", frames[1].Stage)
				}
				frames = append(frames, frames[1])
			}
			replaceAbandonmentJournal(t, home, frames)
			if report, err := Run(home, Options{Apply: true, WorkID: "work_abandon_history"}, testNow.Add(2*time.Minute)); err == nil {
				t.Fatalf("invalid abandonment history accepted: stage=%q", report.Stage)
			}
		})
	}
}
