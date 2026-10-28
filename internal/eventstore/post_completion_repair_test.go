package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var repairNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func TestLegacyMigrateApplyAfterCompletionRoutesToGovernedRepairWithoutMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	loosePath := filepath.Join(store.rootPath(), "trace_divergent.json")
	looseBefore, err := os.ReadFile(loosePath)
	if err != nil {
		t.Fatal(err)
	}
	framesBefore := segmentFrameCountForID(t, store, "rec_absent")
	manifestBefore, err := os.ReadFile(store.engine.manifestPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Migrate(MigrateOptions{Mode: ModeApply})
	if !errors.Is(err, ErrPostCompletionRepairRequired) {
		t.Fatalf("post-completion migrate apply error = %v", err)
	}
	looseAfter, err := os.ReadFile(loosePath)
	if err != nil || !bytes.Equal(looseAfter, looseBefore) {
		t.Fatalf("legacy apply changed divergent loose bytes: err=%v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != framesBefore {
		t.Fatalf("legacy apply appended absent record before routing: frames=%d want=%d", got, framesBefore)
	}
	manifestAfter, err := os.ReadFile(store.engine.manifestPath())
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("legacy apply changed frozen manifest: err=%v", err)
	}
}

func TestLegacyMigrateApplyAfterCompletionRefusesArrivalBetweenRetirementAndMutationCensuses(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_initial", "initial")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}
	segmentBefore := readFamilyBytes(t, store.engine.segRoot, "receipt")
	manifestBefore, err := os.ReadFile(store.engine.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	lateBytes, err := json.MarshalIndent(eventRecord("rec_late", schemaForID("rec_late"), repairNow, "late"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	lateBytes = append(lateBytes, '\n')
	latePath := filepath.Join(store.rootPath(), "rec_late.json")

	report, err := store.Migrate(MigrateOptions{
		Mode: ModeApply,
		afterRetirementCensus: func() error {
			return os.WriteFile(latePath, lateBytes, 0o644)
		},
	})
	if !errors.Is(err, ErrPostCompletionRepairRequired) {
		t.Fatalf("post-completion record crossed retired migration boundary: report=%+v err=%v", report, err)
	}
	if report.Discovered != 1 {
		t.Fatalf("retirement refusal discovered=%d want=1", report.Discovered)
	}
	looseAfter, readErr := os.ReadFile(latePath)
	if readErr != nil || !bytes.Equal(looseAfter, lateBytes) {
		t.Fatalf("retirement refusal changed late loose bytes: err=%v", readErr)
	}
	if got := segmentFrameCountForID(t, store, "rec_late"); got != 0 {
		t.Fatalf("retired migration appended late record: frames=%d", got)
	}
	if segmentAfter := readFamilyBytes(t, store.engine.segRoot, "receipt"); !bytes.Equal(segmentAfter, segmentBefore) {
		t.Fatal("retired migration changed segment bytes")
	}
	manifestAfter, readErr := os.ReadFile(store.engine.manifestPath())
	if readErr != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("retired migration changed frozen manifest: err=%v", readErr)
	}
}

func TestPostCompletionRepairMixedCurrentScalePreservesEveryLooseByteAndSegmentAuthority(t *testing.T) {
	// Deliberately NOT gated on testing.Short(): the canonical land gate is
	// `go test -count=1 -timeout=40m ./...`, which never passes -short, so a
	// -short guard here would be decorative — that was the exact trap that
	// made the previous guard on this test useless, since it never actually
	// skipped on the gate that matters. perClass is the real lever instead:
	// it defaults to a small value that still exercises every disposition
	// (identical, divergent, absent) and every assertion below, all of which
	// are expressed relative to perClass rather than a hardcoded count. The
	// full production-scale run (1,101 records: current-home scale) stays
	// available as an explicit, opt-in, non-land-gated variant via
	// COMPOUND_EVENTSTORE_SCALE=367.
	store := openMigrateStore(t)
	perClass := 30
	if raw := os.Getenv("COMPOUND_EVENTSTORE_SCALE"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("COMPOUND_EVENTSTORE_SCALE=%q must be a positive integer", raw)
		}
		perClass = parsed
	}

	type expectedRecord struct {
		id              string
		disposition     string
		loose           []byte
		segmentBefore   []byte
		segmentFrameCnt int
	}
	expected := make([]expectedRecord, 0, perClass*3)

	// Seed the canonical segment side first, then close the original migration.
	for i := 0; i < perClass; i++ {
		for _, prefix := range []string{"rec_identical", "trace_divergent"} {
			id := fmt.Sprintf("%s_%04d", prefix, i)
			seedLoose(t, store, id, "canonical-"+id)
		}
	}
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}

	for i := 0; i < perClass; i++ {
		identicalID := fmt.Sprintf("rec_identical_%04d", i)
		segment, err := store.Read(identicalID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.rootPath(), identicalID+".json"), append([]byte(nil), segment.Bytes...), 0o644); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, expectedRecord{id: identicalID, disposition: RepairArchiveIdentical, loose: append([]byte(nil), segment.Bytes...), segmentBefore: append([]byte(nil), segment.Bytes...), segmentFrameCnt: 1})

		divergentID := fmt.Sprintf("trace_divergent_%04d", i)
		canonical, err := store.Read(divergentID)
		if err != nil {
			t.Fatal(err)
		}
		writeLegacyRecord(t, store, divergentID, eventRecord(divergentID, schemaForID(divergentID), repairNow, "loose-divergent-"+divergentID))
		loose, err := os.ReadFile(filepath.Join(store.rootPath(), divergentID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, expectedRecord{id: divergentID, disposition: RepairArchiveDivergent, loose: loose, segmentBefore: append([]byte(nil), canonical.Bytes...), segmentFrameCnt: 1})

		absentID := fmt.Sprintf("pol_absent_%04d", i)
		writeLegacyRecord(t, store, absentID, eventRecord(absentID, schemaForID(absentID), repairNow, "loose-new-"+absentID))
		absentLoose, err := os.ReadFile(filepath.Join(store.rootPath(), absentID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, expectedRecord{id: absentID, disposition: RepairImportAbsent, loose: absentLoose, segmentFrameCnt: 1})
	}

	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	if created.Plan.Total != perClass*3 || created.Plan.Counts[RepairImportAbsent] != perClass || created.Plan.Counts[RepairArchiveIdentical] != perClass || created.Plan.Counts[RepairArchiveDivergent] != perClass {
		t.Fatalf("plan counts = total=%d counts=%v", created.Plan.Total, created.Plan.Counts)
	}
	// The plan is content-addressed and idempotent: a mechanical rerun over the
	// same source snapshot reuses the exact artifact despite a later clock.
	replayedPlan, err := store.CreatePostCompletionRepairPlan(repairNow.Add(time.Hour))
	if err != nil || replayedPlan.Plan.PlanDigest != created.Plan.PlanDigest || replayedPlan.Plan.CreatedAt != created.Plan.CreatedAt {
		t.Fatalf("idempotent plan = %+v err=%v", replayedPlan.Plan, err)
	}
	authorization := repairTestAuthorization(t, store, created)
	result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Replayed || result.Total != perClass*3 || result.Counts[RepairArchiveDivergent] != perClass {
		t.Fatalf("result = %+v", result)
	}

	for _, want := range expected {
		if _, err := os.Stat(filepath.Join(store.rootPath(), want.id+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hot loose %s remains: %v", want.id, err)
		}
		entry := repairResultEntry(result, want.id)
		archived, err := os.ReadFile(store.postCompletionRepairArchivePath(entry.ArchiveSHA256))
		if err != nil {
			t.Fatalf("read archive %s: %v", want.id, err)
		}
		if !bytes.Equal(archived, want.loose) {
			t.Fatalf("archive %s changed original bytes", want.id)
		}
		resolved, err := store.Read(want.id)
		if err != nil {
			t.Fatalf("resolve %s: %v", want.id, err)
		}
		if want.segmentBefore != nil && !bytes.Equal(resolved.Bytes, want.segmentBefore) {
			t.Fatalf("repair overwrote segment authority for %s", want.id)
		}
		if got := segmentFrameCountForID(t, store, want.id); got != want.segmentFrameCnt {
			t.Fatalf("segment frame count %s = %d, want %d", want.id, got, want.segmentFrameCnt)
		}
	}
	finalizePostCompletionRepairForTest(t, store, created.Plan, authorization, result)

	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Completion.Complete || verified.Completion.RemainingLoose != 0 || verified.Completion.LegacyWriterActive || verified.Completion.RepairedRecords != perClass*3 {
		t.Fatalf("verify after repair = %+v", verified.Completion)
	}
}

func TestPostCompletionRepairCompletedExactSourceReoccurrenceCreatesNewLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_repeat", "same")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}
	canonical, err := store.Read("rec_repeat")
	if err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(store.rootPath(), "rec_repeat.json")
	if err := os.WriteFile(loosePath, canonical.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	originalLoose := append([]byte(nil), canonical.Bytes...)

	first, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	firstPlanBytes, err := os.ReadFile(first.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	activeReplay, err := store.CreatePostCompletionRepairPlan(repairNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	activeReplayBytes, err := os.ReadFile(activeReplay.PlanPath)
	if err != nil || activeReplay.Plan.ID != first.Plan.ID || activeReplay.Plan.PlanDigest != first.Plan.PlanDigest || !bytes.Equal(activeReplayBytes, firstPlanBytes) {
		t.Fatalf("active occurrence planning was not byte-identical: err=%v first=%+v replay=%+v", err, first.Plan, activeReplay.Plan)
	}

	firstAuthorization := repairTestAuthorization(t, store, first)
	firstResult, err := store.ApplyPostCompletionRepair(first.PlanPath, firstAuthorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	firstProof := finalizePostCompletionRepairForTest(t, store, first.Plan, firstAuthorization, firstResult)
	firstPaths := []string{
		first.PlanPath,
		store.postCompletionRepairReviewPath(first.Plan.ID),
		store.postCompletionRepairResultPath(first.Plan.ID),
		store.postCompletionRepairProofPath(first.Plan.ID),
	}
	firstArtifacts := make(map[string][]byte, len(firstPaths))
	for _, path := range firstPaths {
		firstArtifacts[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(loosePath, originalLoose, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Plan.ID == first.Plan.ID || second.Plan.SourceDigest != first.Plan.SourceDigest {
		t.Fatalf("completed exact source did not advance lifecycle identity: first=%+v second=%+v", first.Plan, second.Plan)
	}
	secondPlanBytes, err := os.ReadFile(second.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	secondActiveReplay, err := store.CreatePostCompletionRepairPlan(repairNow.Add(4 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	secondActiveReplayBytes, err := os.ReadFile(secondActiveReplay.PlanPath)
	if err != nil || secondActiveReplay.Plan.ID != second.Plan.ID || secondActiveReplay.Plan.PlanDigest != second.Plan.PlanDigest || !bytes.Equal(secondActiveReplayBytes, secondPlanBytes) {
		t.Fatalf("second active occurrence planning was not byte-identical: err=%v second=%+v replay=%+v", err, second.Plan, secondActiveReplay.Plan)
	}
	for path, before := range firstArtifacts {
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("new occurrence changed first lifecycle artifact %s: %v", filepath.Base(path), readErr)
		}
	}

	secondAuthorization := repairTestAuthorization(t, store, second)
	secondAuthorization.WorkID = "work_eventstore_repair_second"
	secondAuthorization.ActionIntentID = "act_eventstore_repair_second"
	secondAuthorization.PolicyDecisionID = "pol_eventstore_repair_second"
	secondResult, err := store.ApplyPostCompletionRepair(second.PlanPath, secondAuthorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	secondReceiptID, secondTransitionID := seedPostCompletionRepairEnvelopeForTest(t, store, secondAuthorization, "_second", mustRepairTime(t, secondResult.CompletedAt), []string{
		"eventstore-repair-plan:" + secondAuthorization.PlanDigest,
		"eventstore-repair-review:" + secondAuthorization.ReviewDigest,
		"policy-authority:" + secondAuthorization.AuthorityRevision,
	})
	secondProof, err := store.FinalizePostCompletionRepair(second.Plan.ID, secondAuthorization, secondReceiptID, secondTransitionID, repairNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if secondProof.PlanID != second.Plan.ID || secondProof.ProofDigest == firstProof.ProofDigest {
		t.Fatalf("second lifecycle proof identity invalid: first=%+v second=%+v", firstProof, secondProof)
	}

	completedPaths := append(append([]string(nil), firstPaths...), second.PlanPath, store.postCompletionRepairReviewPath(second.Plan.ID), store.postCompletionRepairResultPath(second.Plan.ID), store.postCompletionRepairProofPath(second.Plan.ID), store.postCompletionRepairRootPath(), store.postCompletionRepairAnchorPath())
	completedArtifacts := make(map[string][]byte, len(completedPaths))
	for _, path := range completedPaths {
		completedArtifacts[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, completed := range []struct {
		plan          PostCompletionRepairPlanRecord
		authorization PostCompletionRepairAuthorization
		result        PostCompletionRepairResult
	}{
		{plan: first, authorization: firstAuthorization, result: firstResult},
		{plan: second, authorization: secondAuthorization, result: secondResult},
	} {
		replayed, replayErr := store.ApplyPostCompletionRepairOutcome(completed.plan.PlanPath, completed.authorization, authorizedPostCompletionRepairApplyOptions())
		if replayErr != nil || replayed.Changed || !replayed.Replayed || replayed.Result.ResultDigest != completed.result.ResultDigest {
			t.Fatalf("completed lifecycle replay was not an exact no-op: plan=%s result=%+v err=%v", completed.plan.Plan.ID, replayed, replayErr)
		}
	}
	for path, before := range completedArtifacts {
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("completed lifecycle replay changed %s: %v", filepath.Base(path), readErr)
		}
	}
}

func TestPostCompletionRepairReplayPreservesSealedResultIntegrity(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	first, err := store.ApplyPostCompletionRepairOutcome(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Replayed || !first.Result.Changed || first.Result.Replayed {
		t.Fatalf("initial apply outcome = %+v", first)
	}
	resultPath := store.postCompletionRepairResultPath(planned.Plan.ID)
	persistedBefore, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := store.ApplyPostCompletionRepairOutcome(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if replay.Changed || !replay.Replayed {
		t.Fatalf("replay call outcome = %+v", replay)
	}
	if !replay.Result.Changed || replay.Result.Replayed {
		t.Fatalf("replay mutated sealed result flags = %+v", replay.Result)
	}
	if err := store.validatePostCompletionRepairResult(replay.Result, planned.Plan, authorization); err != nil {
		t.Fatalf("replay result no longer validates: %v", err)
	}
	digest, err := postCompletionRepairResultDigest(replay.Result)
	if err != nil || digest != replay.Result.ResultDigest {
		t.Fatalf("replay result digest mismatch: got=%s sealed=%s err=%v", digest, replay.Result.ResultDigest, err)
	}
	persistedAfter, err := os.ReadFile(resultPath)
	if err != nil || !bytes.Equal(persistedAfter, persistedBefore) {
		t.Fatalf("replay changed persisted result bytes: err=%v", err)
	}
}

func TestPostCompletionRepairRefusesResultAndProofBeforeLatestAuthorization(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	t.Run("result", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := sealedRepairTestAuthorization(t, store, planned)
		authorization.AuthorizedAt = repairNow.Add(10 * time.Minute).Format(time.RFC3339Nano)
		opts := authorizedPostCompletionRepairApplyOptions()
		opts.resultNow = func() time.Time { return repairNow.Add(9 * time.Minute) }

		_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, opts)
		if err == nil || !strings.Contains(err.Error(), "result completed_at precedes latest authorization") {
			t.Fatalf("result persisted before authority: %v", err)
		}
		if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
			t.Fatalf("temporal refusal wrote result: exists=%v err=%v", exists, readErr)
		}
	})

	t.Run("proof", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := sealedRepairTestAuthorization(t, store, planned)
		authorization.AuthorizedAt = repairNow.Add(2 * time.Minute).Format(time.RFC3339Nano)
		opts := authorizedPostCompletionRepairApplyOptions()
		opts.resultNow = func() time.Time { return repairNow.Add(3 * time.Minute) }
		result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, opts)
		if err != nil {
			t.Fatal(err)
		}
		receiptID := "rec_eventstore_repair_temporal"
		transitionID := "tr_eventstore_repair_temporal"
		proofTime := repairNow.Add(3 * time.Minute).Format(time.RFC3339Nano)
		for _, record := range []map[string]any{
			{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": proofTime, "ids": map[string]string{"work_id": authorization.WorkID}},
			{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": proofTime, "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
			{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": proofTime, "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision}},
			{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": proofTime, "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
		} {
			if _, err := store.PutJSON(record["id"].(string), record, "  "); err != nil {
				t.Fatal(err)
			}
		}

		_, err = store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(time.Minute))
		if err == nil || !strings.Contains(err.Error(), "proof recorded_at precedes latest authorization or result") {
			t.Fatalf("proof persisted before authority/result: result=%s err=%v", result.CompletedAt, err)
		}
		if _, exists, readErr := store.ReadPostCompletionRepairProof(planned.Plan.ID); readErr != nil || exists {
			t.Fatalf("temporal refusal wrote proof: exists=%v err=%v", exists, readErr)
		}
	})
}

func TestPostCompletionRepairProofRejectsCausalBackdating(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "before receipt", at: repairNow.Add(5 * time.Minute / 2), want: "precedes receipt"},
		{name: "before transition", at: repairNow.Add(7 * time.Minute / 2), want: "precedes transition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := sealedRepairTestAuthorization(t, store, planned)
			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			receiptID := "rec_eventstore_repair_causal"
			transitionID := "tr_eventstore_repair_causal"
			for _, record := range []map[string]any{
				{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
				{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Add(time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
				{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": repairNow.Add(3 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision}},
				{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": repairNow.Add(4 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
			} {
				id := record["id"].(string)
				if _, err := store.PutJSON(id, record, "  "); err != nil {
					t.Fatalf("seed causal proof %s: %v", id, err)
				}
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, tc.at)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("causally backdated proof was accepted after result %s: %v, want %q", result.CompletedAt, err, tc.want)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairProof(planned.Plan.ID); readErr != nil || exists {
				t.Errorf("causally backdated proof wrote artifact: exists=%v err=%v", exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Errorf("causally backdated proof changed repair root: err=%v", readErr)
			}
		})
	}
}

func TestPostCompletionRepairProofRejectsBackdatedReceiptAndTransitionBeforeMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name            string
		receiptDelta    time.Duration
		transitionDelta time.Duration
		want            string
	}{
		{name: "receipt before result", receiptDelta: -time.Minute, transitionDelta: time.Minute, want: "receipt created_at precedes result completion"},
		{name: "transition before receipt", receiptDelta: time.Minute, transitionDelta: 30 * time.Second, want: "transition created_at precedes receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := sealedRepairTestAuthorization(t, store, planned)
			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			terminalAt, err := time.Parse(time.RFC3339Nano, result.CompletedAt)
			if err != nil {
				t.Fatal(err)
			}
			receiptID := "rec_eventstore_repair_terminal_order"
			transitionID := "tr_eventstore_repair_terminal_order"
			for _, record := range []map[string]any{
				{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
				{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
				{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": terminalAt.Add(tc.receiptDelta).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision}},
				{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": terminalAt.Add(tc.transitionDelta).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
			} {
				if _, err := store.PutJSON(record["id"].(string), record, "  "); err != nil {
					t.Fatal(err)
				}
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, terminalAt.Add(2*time.Minute))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backdated work chain was accepted: err=%v want=%q", err, tc.want)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairProof(planned.Plan.ID); readErr != nil || exists {
				t.Fatalf("causal refusal wrote proof: exists=%v err=%v", exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Fatalf("causal refusal changed repair root: %v", readErr)
			}
		})
	}
}

func TestPostCompletionRepairPhysicalResultExactRecurrenceAdvancesAfterProof(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	t.Run("exact recurrence", func(t *testing.T) {
		store := openMigrateStore(t)
		seedLoose(t, store, "rec_preproof_repeat", "same")
		if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
			t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
		}
		canonical, err := store.Read("rec_preproof_repeat")
		if err != nil {
			t.Fatal(err)
		}
		loosePath := filepath.Join(store.rootPath(), "rec_preproof_repeat.json")
		originalLoose := append([]byte(nil), canonical.Bytes...)
		if err := os.WriteFile(loosePath, originalLoose, 0o644); err != nil {
			t.Fatal(err)
		}
		first, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		firstAuthorization := repairTestAuthorization(t, store, first)
		firstResult, err := store.ApplyPostCompletionRepair(first.PlanPath, firstAuthorization, authorizedPostCompletionRepairApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
		firstResultBytes, err := os.ReadFile(store.postCompletionRepairResultPath(first.Plan.ID))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loosePath, originalLoose, 0o644); err != nil {
			t.Fatal(err)
		}

		replayed, err := store.ApplyPostCompletionRepairOutcome(first.PlanPath, firstAuthorization, authorizedPostCompletionRepairApplyOptions())
		if err != nil || replayed.Changed || !replayed.Replayed || replayed.Result.ResultDigest != firstResult.ResultDigest {
			t.Fatalf("exact recurrence stranded physical result replay: result=%+v err=%v", replayed, err)
		}
		firstReceiptID, firstTransitionID := seedPostCompletionRepairEnvelopeForTest(t, store, firstAuthorization, "_preproof_exact", mustRepairTime(t, firstResult.CompletedAt), []string{
			"eventstore-repair-plan:" + firstAuthorization.PlanDigest,
			"eventstore-repair-review:" + firstAuthorization.ReviewDigest,
			"policy-authority:" + firstAuthorization.AuthorityRevision,
		})
		firstProof, err := store.FinalizePostCompletionRepair(first.Plan.ID, firstAuthorization, firstReceiptID, firstTransitionID, repairNow.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("exact recurrence stranded old proof finalization: %v", err)
		}
		firstResultAfter, err := os.ReadFile(store.postCompletionRepairResultPath(first.Plan.ID))
		if err != nil || !bytes.Equal(firstResultAfter, firstResultBytes) || firstProof.ResultDigest != firstResult.ResultDigest {
			t.Fatalf("old lifecycle changed while finalizing exact recurrence: err=%v proof=%+v", err, firstProof)
		}
		verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
		if err != nil {
			t.Fatal(err)
		}
		if verified.Completion.Complete || verified.Completion.RemainingLoose != 1 {
			t.Fatalf("completed-home verification hid outstanding recurrence: %+v", verified.Completion)
		}

		second, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if second.Plan.ID == first.Plan.ID || second.Plan.SourceDigest != first.Plan.SourceDigest {
			t.Fatalf("exact recurrence did not advance deterministic occurrence: first=%+v second=%+v", first.Plan, second.Plan)
		}
		secondAuthorization := repairTestAuthorization(t, store, second)
		secondAuthorization.WorkID = "work_eventstore_repair_preproof_second"
		secondAuthorization.ActionIntentID = "act_eventstore_repair_preproof_second"
		secondAuthorization.PolicyDecisionID = "pol_eventstore_repair_preproof_second"
		secondResult, err := store.ApplyPostCompletionRepair(second.PlanPath, secondAuthorization, authorizedPostCompletionRepairApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
		secondReceiptID, secondTransitionID := seedPostCompletionRepairEnvelopeForTest(t, store, secondAuthorization, "_preproof_second", mustRepairTime(t, secondResult.CompletedAt), []string{
			"eventstore-repair-plan:" + secondAuthorization.PlanDigest,
			"eventstore-repair-review:" + secondAuthorization.ReviewDigest,
			"policy-authority:" + secondAuthorization.AuthorityRevision,
		})
		secondProof, err := store.FinalizePostCompletionRepair(second.Plan.ID, secondAuthorization, secondReceiptID, secondTransitionID, repairNow.Add(5*time.Minute))
		if err != nil || secondProof.ResultDigest != secondResult.ResultDigest {
			t.Fatalf("second occurrence did not finalize exactly: proof=%+v err=%v", secondProof, err)
		}
		verified, err = store.Migrate(MigrateOptions{Mode: ModeVerify})
		if err != nil || !verified.Completion.Complete || verified.Completion.RemainingLoose != 0 {
			t.Fatalf("verification did not recover after second occurrence: completion=%+v err=%v", verified.Completion, err)
		}
	})

	for _, tc := range []struct {
		name   string
		inject func(t *testing.T, store *Store, loosePath string)
	}{
		{name: "divergent recurrence", inject: func(t *testing.T, store *Store, _ string) {
			writeLegacyRecord(t, store, "rec_preproof_guard", eventRecord("rec_preproof_guard", schemaForID("rec_preproof_guard"), repairNow.Add(time.Minute), "different"))
		}},
		{name: "foreign loose", inject: func(t *testing.T, store *Store, _ string) {
			writeLegacyRecord(t, store, "rec_preproof_foreign", eventRecord("rec_preproof_foreign", schemaForID("rec_preproof_foreign"), repairNow.Add(time.Minute), "foreign"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openMigrateStore(t)
			seedLoose(t, store, "rec_preproof_guard", "same")
			if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
				t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
			}
			canonical, err := store.Read("rec_preproof_guard")
			if err != nil {
				t.Fatal(err)
			}
			loosePath := filepath.Join(store.rootPath(), "rec_preproof_guard.json")
			if err := os.WriteFile(loosePath, canonical.Bytes, 0o644); err != nil {
				t.Fatal(err)
			}
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			tc.inject(t, store, loosePath)

			if _, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions()); err == nil {
				t.Fatal("foreign/divergent loose source was accepted during result replay")
			}
			receiptID, transitionID := seedPostCompletionRepairEnvelopeForTest(t, store, authorization, "_preproof_guard", mustRepairTime(t, result.CompletedAt), []string{
				"eventstore-repair-plan:" + authorization.PlanDigest,
				"eventstore-repair-review:" + authorization.ReviewDigest,
				"policy-authority:" + authorization.AuthorityRevision,
			})
			if _, err := store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(2*time.Minute)); err == nil {
				t.Fatal("foreign/divergent loose source was accepted during proof finalization")
			}
			if _, exists, readErr := store.ReadPostCompletionRepairProof(planned.Plan.ID); readErr != nil || exists {
				t.Fatalf("foreign/divergent refusal wrote proof: exists=%v err=%v", exists, readErr)
			}
		})
	}
}

func TestPostCompletionRepairRejectsExactByteSymlinkBeforeMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	loosePath := filepath.Join(store.rootPath(), "rec_absent.json")
	original, err := os.ReadFile(loosePath)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "exact-source.json")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(loosePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, loosePath); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "loose rec_absent is not a regular file") {
		t.Fatalf("exact-byte symlink was not refused before mutation: %v", err)
	}
	if info, statErr := os.Lstat(loosePath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("refusal renamed or removed symlink: info=%v err=%v", info, statErr)
	}
	if target, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(target, original) {
		t.Fatalf("refusal changed symlink target: err=%v", readErr)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 0 {
		t.Fatalf("refusal appended %d segment frames", got)
	}
	if _, statErr := os.Stat(store.postCompletionRepairArchivePath(recordHash(original))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refusal wrote archive: %v", statErr)
	}
	for _, path := range []string{
		store.postCompletionRepairPendingPath(planned.Plan.ID),
		store.postCompletionRepairProgressPath(planned.Plan.ID),
		store.postCompletionRepairResultPath(planned.Plan.ID),
	} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("refusal wrote lifecycle mutation %s: %v", filepath.Base(path), statErr)
		}
	}
}

func TestPostCompletionRepairRejectsExactByteSymlinkSwappedBeforeAtomicClaim(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_claim_swap", "same")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}
	canonical, err := store.Read("rec_claim_swap")
	if err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(store.rootPath(), "rec_claim_swap.json")
	if err := os.WriteFile(loosePath, canonical.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Plan.Total != 1 || planned.Plan.Entries[0].ID != "rec_claim_swap" {
		t.Fatalf("claim-swap fixture plan = %+v", planned.Plan.Entries)
	}
	entry := planned.Plan.Entries[0]
	authorization := repairTestAuthorization(t, store, planned)
	targetPath := filepath.Join(t.TempDir(), "exact-source.json")
	if err := os.WriteFile(targetPath, canonical.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	segmentBefore, err := store.Read("rec_claim_swap")
	if err != nil {
		t.Fatal(err)
	}
	var archiveBefore []byte
	swapped := false
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		beforeLooseClaim: func(id string) error {
			if id != entry.ID || swapped {
				return nil
			}
			swapped = true
			archiveBefore, err = os.ReadFile(store.postCompletionRepairArchivePath(entry.RawSHA256))
			if err != nil {
				return err
			}
			if err := os.Remove(loosePath); err != nil {
				return err
			}
			return os.Symlink(targetPath, loosePath)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "quarantined loose rec_claim_swap is not a regular file") {
		t.Fatalf("post-validation symlink swap was accepted: %v", err)
	}
	if !swapped {
		t.Fatal("claim-swap seam was not reached")
	}
	if info, statErr := os.Lstat(loosePath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("refusal did not restore exact swapped source custody: info=%v err=%v", info, statErr)
	}
	if _, statErr := os.Lstat(store.postCompletionRepairClaimPath(planned.Plan.ID, entry)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refusal retained non-regular quarantine claim: %v", statErr)
	}
	archiveAfter, readErr := os.ReadFile(store.postCompletionRepairArchivePath(entry.RawSHA256))
	if readErr != nil || !bytes.Equal(archiveAfter, archiveBefore) {
		t.Fatalf("refusal mutated previously admitted archive: err=%v", readErr)
	}
	segmentAfter, readErr := store.Read("rec_claim_swap")
	if readErr != nil || !bytes.Equal(segmentAfter.Bytes, segmentBefore.Bytes) {
		t.Fatalf("refusal mutated canonical segment custody: err=%v", readErr)
	}
	progress := repairProgressForID(t, store, planned.Plan.ID, entry.ID)
	if progress.Phase != repairPhaseRemoveReserved {
		t.Fatalf("refusal advanced claimed progress: %+v", progress)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("refusal published result: exists=%v err=%v", exists, readErr)
	}
}

func TestPostCompletionRepairRejectsMatchingArchiveSymlinkBeforeApply(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	var entry PostCompletionRepairEntry
	for _, candidate := range planned.Plan.Entries {
		if candidate.ID == "rec_absent" {
			entry = candidate
			break
		}
	}
	if entry.ID == "" {
		t.Fatal("rec_absent missing from repair plan")
	}
	loosePath := filepath.Join(store.rootPath(), entry.FileName)
	looseBefore, err := os.ReadFile(loosePath)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "matching-archive.json")
	if err := os.WriteFile(targetPath, looseBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := store.postCompletionRepairArchivePath(entry.RawSHA256)
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, archivePath); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "archive custody for rec_absent is not a regular file") {
		t.Fatalf("matching archive symlink was accepted before apply: %v", err)
	}
	looseAfter, readErr := os.ReadFile(loosePath)
	if readErr != nil || !bytes.Equal(looseAfter, looseBefore) {
		t.Fatalf("archive refusal did not preserve source: err=%v", readErr)
	}
	if got := segmentFrameCountForID(t, store, entry.ID); got != 0 {
		t.Fatalf("archive refusal appended %d canonical frames", got)
	}
	if info, statErr := os.Lstat(archivePath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("archive refusal changed symlink custody: info=%v err=%v", info, statErr)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("archive refusal published result: exists=%v err=%v", exists, readErr)
	}
}

func TestPostCompletionRepairRejectsMatchingArchiveSymlinkDuringReplayAndProof(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, operation := range []string{"replay", "proof"} {
		t.Run(operation, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			var entry PostCompletionRepairEntry
			for _, candidate := range planned.Plan.Entries {
				if candidate.ID == "rec_absent" {
					entry = candidate
					break
				}
			}
			archivePath := store.postCompletionRepairArchivePath(entry.RawSHA256)
			archiveRaw, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(t.TempDir(), "matching-archive.json")
			if err := os.WriteFile(targetPath, archiveRaw, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(archivePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(targetPath, archivePath); err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "replay":
				_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			case "proof":
				receiptID, transitionID := seedPostCompletionRepairEnvelopeForTest(t, store, authorization, "_archive_symlink", mustRepairTime(t, result.CompletedAt), []string{
					"eventstore-repair-plan:" + authorization.PlanDigest,
					"eventstore-repair-review:" + authorization.ReviewDigest,
					"policy-authority:" + authorization.AuthorityRevision,
				})
				_, err = store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(3*time.Minute))
			}
			if err == nil || !strings.Contains(err.Error(), "archive custody for rec_absent is not a regular file") {
				t.Errorf("matching archive symlink was accepted during %s: %v", operation, err)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairProof(planned.Plan.ID); readErr != nil || exists {
				t.Errorf("archive symlink %s wrote proof: exists=%v err=%v", operation, exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Errorf("archive symlink %s changed repair root: err=%v", operation, readErr)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionRejectsSymlinkedPendingControlArtifactBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dangling bool
	}{
		{name: "exact bytes"},
		{name: "dangling", dangling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, oldPlan)
			writeLegacyRecord(t, store, "rec_after_pending_symlink", eventRecord("rec_after_pending_symlink", schemaForID("rec_after_pending_symlink"), repairNow.Add(2*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			pendingPath := store.postCompletionRepairPendingPath(oldPlan.Plan.ID)
			targetPath := filepath.Join(t.TempDir(), filepath.Base(pendingPath))
			var targetBefore []byte
			if tc.dangling {
				targetPath = filepath.Join(t.TempDir(), "missing.pending.json")
			} else {
				targetBefore = mustRepairJSON(newPostCompletionRepairPending(oldPlan.Plan, authorization, repairNow.Add(2*time.Minute)))
				if err := os.WriteFile(targetPath, targetBefore, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(targetPath, pendingPath); err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}
			anchorBefore, err := os.ReadFile(store.postCompletionRepairAnchorPath())
			if err != nil {
				t.Fatal(err)
			}

			authorityChecks := 0
			err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), func() error {
				authorityChecks++
				return nil
			})
			if err == nil || !postCompletionRepairCustodyError(err) {
				t.Fatalf("%s pending symlink was accepted by supersession: %v", tc.name, err)
			}
			if authorityChecks != 0 {
				t.Fatalf("%s pending symlink reopened authority %d time(s)", tc.name, authorityChecks)
			}
			if info, statErr := os.Lstat(pendingPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s pending refusal changed symlink custody: info=%v err=%v", tc.name, info, statErr)
			}
			if !tc.dangling {
				if targetAfter, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(targetAfter, targetBefore) {
					t.Fatalf("%s pending refusal changed symlink target: %v", tc.name, readErr)
				}
			}
			rootAfter, rootErr := os.ReadFile(store.postCompletionRepairRootPath())
			anchorAfter, anchorErr := os.ReadFile(store.postCompletionRepairAnchorPath())
			if rootErr != nil || anchorErr != nil || !bytes.Equal(rootAfter, rootBefore) || !bytes.Equal(anchorAfter, anchorBefore) {
				t.Fatalf("%s pending refusal changed repair root/anchor: root_err=%v anchor_err=%v", tc.name, rootErr, anchorErr)
			}
			if _, statErr := os.Lstat(store.postCompletionRepairSupersessionPath(oldPlan.Plan.ID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s pending refusal wrote supersession: %v", tc.name, statErr)
			}

			verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if verified.Completion.Complete || len(verified.Completion.Reasons) == 0 {
				t.Fatalf("migrate --verify accepted %s pending symlink: %+v", tc.name, verified.Completion)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionRejectsSymlinkedQuarantineDirectoriesBeforeMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name     string
		level    string
		dangling bool
	}{
		{name: "exact quarantine root", level: "root"},
		{name: "dangling quarantine root", level: "root", dangling: true},
		{name: "exact plan directory", level: "plan"},
		{name: "dangling plan directory", level: "plan", dangling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, oldPlan)
			writeLegacyRecord(t, store, "rec_after_quarantine_symlink", eventRecord("rec_after_quarantine_symlink", schemaForID("rec_after_quarantine_symlink"), repairNow.Add(2*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			quarantineRoot := filepath.Join(store.engine.postCompletionRepairRoot(), "quarantine")
			symlinkPath := quarantineRoot
			if tc.level == "plan" {
				if err := os.Mkdir(quarantineRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				symlinkPath = filepath.Join(quarantineRoot, oldPlan.Plan.ID)
			}
			targetPath := filepath.Join(t.TempDir(), "quarantine-target")
			var markerPath string
			var markerBefore []byte
			if !tc.dangling {
				if err := os.Mkdir(targetPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.level == "root" {
					if err := os.Mkdir(filepath.Join(targetPath, oldPlan.Plan.ID), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				markerPath = filepath.Join(targetPath, "custody-marker")
				markerBefore = []byte("unchanged\n")
				if err := os.WriteFile(markerPath, markerBefore, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(targetPath, symlinkPath); err != nil {
				t.Fatal(err)
			}
			authorityRootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}
			anchorBefore, err := os.ReadFile(store.postCompletionRepairAnchorPath())
			if err != nil {
				t.Fatal(err)
			}

			authorityChecks := 0
			err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), func() error {
				authorityChecks++
				return nil
			})
			if err == nil || !postCompletionRepairCustodyError(err) {
				t.Fatalf("%s was accepted by supersession: %v", tc.name, err)
			}
			if authorityChecks != 0 {
				t.Fatalf("%s reopened authority %d time(s)", tc.name, authorityChecks)
			}
			if info, statErr := os.Lstat(symlinkPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s refusal changed symlink custody: info=%v err=%v", tc.name, info, statErr)
			}
			if !tc.dangling {
				if markerAfter, readErr := os.ReadFile(markerPath); readErr != nil || !bytes.Equal(markerAfter, markerBefore) {
					t.Fatalf("%s refusal changed symlink target: %v", tc.name, readErr)
				}
			}
			authorityRootAfter, rootErr := os.ReadFile(store.postCompletionRepairRootPath())
			anchorAfter, anchorErr := os.ReadFile(store.postCompletionRepairAnchorPath())
			if rootErr != nil || anchorErr != nil || !bytes.Equal(authorityRootAfter, authorityRootBefore) || !bytes.Equal(anchorAfter, anchorBefore) {
				t.Fatalf("%s refusal changed repair root/anchor: root_err=%v anchor_err=%v", tc.name, rootErr, anchorErr)
			}
			if _, statErr := os.Lstat(store.postCompletionRepairSupersessionPath(oldPlan.Plan.ID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s refusal wrote supersession: %v", tc.name, statErr)
			}

			verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if verified.Completion.Complete || !postCompletionRepairReasonsContainCustodyError(verified.Completion.Reasons) {
				t.Fatalf("migrate --verify accepted %s without a custody reason: %+v", tc.name, verified.Completion)
			}
			authorityRootAfterVerify, rootErr := os.ReadFile(store.postCompletionRepairRootPath())
			anchorAfterVerify, anchorErr := os.ReadFile(store.postCompletionRepairAnchorPath())
			if rootErr != nil || anchorErr != nil || !bytes.Equal(authorityRootAfterVerify, authorityRootBefore) || !bytes.Equal(anchorAfterVerify, anchorBefore) {
				t.Fatalf("verify changed %s repair root/anchor: root_err=%v anchor_err=%v", tc.name, rootErr, anchorErr)
			}
		})
	}
}

func TestPostCompletionRepairApplyRejectsSymlinkedQuarantineDirectoryBeforeExternalMutation(t *testing.T) {
	for _, level := range []string{"root", "plan"} {
		t.Run(level, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			quarantineRoot := filepath.Join(store.engine.postCompletionRepairRoot(), "quarantine")
			symlinkPath := quarantineRoot
			if level == "plan" {
				if err := os.Mkdir(quarantineRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				symlinkPath = filepath.Join(quarantineRoot, planned.Plan.ID)
			}
			targetPath := filepath.Join(t.TempDir(), "quarantine-target")
			if err := os.Mkdir(targetPath, 0o755); err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(targetPath, "custody-marker")
			markerBefore := []byte("unchanged\n")
			if err := os.WriteFile(markerPath, markerBefore, 0o644); err != nil {
				t.Fatal(err)
			}
			entriesBefore, err := os.ReadDir(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(targetPath, symlinkPath); err != nil {
				t.Fatal(err)
			}

			_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err == nil || !postCompletionRepairCustodyError(err) {
				t.Fatalf("%s quarantine symlink was accepted by apply: %v", level, err)
			}
			entriesAfter, readErr := os.ReadDir(targetPath)
			if readErr != nil || !reflect.DeepEqual(entriesAfter, entriesBefore) {
				t.Fatalf("%s quarantine refusal changed external directory: before=%v after=%v err=%v", level, entriesBefore, entriesAfter, readErr)
			}
			if markerAfter, readErr := os.ReadFile(markerPath); readErr != nil || !bytes.Equal(markerAfter, markerBefore) {
				t.Fatalf("%s quarantine refusal changed marker: %v", level, readErr)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
				t.Fatalf("%s quarantine refusal published result: exists=%v err=%v", level, exists, readErr)
			}
		})
	}
}

func TestPostCompletionRepairClaimDirectoryCreationRejectsSymlinkCustody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		level    string
		dangling bool
	}{
		{name: "exact quarantine root", level: "root"},
		{name: "dangling quarantine root", level: "root", dangling: true},
		{name: "exact plan directory", level: "plan"},
		{name: "dangling plan directory", level: "plan", dangling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			quarantineRoot := filepath.Join(store.engine.postCompletionRepairRoot(), "quarantine")
			claimDir := filepath.Join(quarantineRoot, planned.Plan.ID)
			symlinkPath := quarantineRoot
			if tc.level == "plan" {
				if err := os.Mkdir(quarantineRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				symlinkPath = claimDir
			}
			targetPath := filepath.Join(t.TempDir(), "quarantine-target")
			var entriesBefore []os.DirEntry
			if !tc.dangling {
				if err := os.Mkdir(targetPath, 0o755); err != nil {
					t.Fatal(err)
				}
				markerPath := filepath.Join(targetPath, "custody-marker")
				if err := os.WriteFile(markerPath, []byte("unchanged\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				entriesBefore, err = os.ReadDir(targetPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(targetPath, symlinkPath); err != nil {
				t.Fatal(err)
			}

			err = store.ensurePostCompletionRepairClaimDir(claimDir)
			if err == nil || !postCompletionRepairCustodyError(err) {
				t.Fatalf("%s was accepted by claim-directory creation: %v", tc.name, err)
			}
			if info, statErr := os.Lstat(symlinkPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s refusal changed symlink custody: info=%v err=%v", tc.name, info, statErr)
			}
			if !tc.dangling {
				entriesAfter, readErr := os.ReadDir(targetPath)
				if readErr != nil || !reflect.DeepEqual(entriesAfter, entriesBefore) {
					t.Fatalf("%s refusal changed external directory: before=%v after=%v err=%v", tc.name, entriesBefore, entriesAfter, readErr)
				}
			}
		})
	}
}

func TestPostCompletionRepairSupersessionFinalizationRejectsExactByteSymlink(t *testing.T) {
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, oldPlan)
	writeLegacyRecord(t, store, "rec_after_supersession_symlink", eventRecord("rec_after_supersession_symlink", schemaForID("rec_after_supersession_symlink"), repairNow.Add(2*time.Minute), "broader-source"))
	successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), authorizedPostCompletionRepairMutation); err != nil {
		t.Fatal(err)
	}
	supersession, exists, err := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
	if err != nil || !exists {
		t.Fatalf("read supersession: exists=%v err=%v", exists, err)
	}
	receiptID, transitionID := seedPostCompletionRepairEnvelopeForTest(t, store, authorization, "_supersession_symlink", mustRepairTime(t, supersession.CreatedAt), []string{
		"eventstore-repair-plan:" + authorization.PlanDigest,
		"eventstore-repair-review:" + authorization.ReviewDigest,
		"policy-authority:" + authorization.AuthorityRevision,
		"eventstore-repair-successor-plan:" + supersession.SuccessorPlanDigest,
		"eventstore-repair-supersession:" + supersession.ID,
	})
	supersessionPath := store.postCompletionRepairSupersessionPath(oldPlan.Plan.ID)
	targetPath, targetBefore := replacePostCompletionRepairFileWithExactSymlink(t, supersessionPath)
	rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
	if err != nil {
		t.Fatal(err)
	}
	anchorBefore, err := os.ReadFile(store.postCompletionRepairAnchorPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.FinalizePostCompletionRepairSupersession(oldPlan.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(5*time.Minute))
	if err == nil || !postCompletionRepairCustodyError(err) {
		t.Fatalf("matching-byte supersession symlink was accepted by finalization: %v", err)
	}
	if info, statErr := os.Lstat(supersessionPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("supersession refusal changed symlink custody: info=%v err=%v", info, statErr)
	}
	if targetAfter, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(targetAfter, targetBefore) {
		t.Fatalf("supersession refusal changed symlink target: %v", readErr)
	}
	rootAfter, rootErr := os.ReadFile(store.postCompletionRepairRootPath())
	anchorAfter, anchorErr := os.ReadFile(store.postCompletionRepairAnchorPath())
	if rootErr != nil || anchorErr != nil || !bytes.Equal(rootAfter, rootBefore) || !bytes.Equal(anchorAfter, anchorBefore) {
		t.Fatalf("supersession refusal changed repair root/anchor: root_err=%v anchor_err=%v", rootErr, anchorErr)
	}
	if _, statErr := os.Lstat(store.postCompletionRepairProofPath(oldPlan.Plan.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("supersession refusal wrote proof: %v", statErr)
	}

	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !postCompletionRepairReasonsContainCustodyError(verified.Completion.Reasons) {
		t.Fatalf("migrate --verify accepted supersession symlink without a custody reason: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairRejectsSymlinkedAuthorityAndImmutableArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, kind := range []string{"root", "anchor", "plan", "review", "result", "proof"} {
		t.Run(kind, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			var result PostCompletionRepairResult
			var proof PostCompletionRepairProof
			if kind == "result" || kind == "proof" {
				result, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
				if err != nil {
					t.Fatal(err)
				}
				proof = finalizePostCompletionRepairForTest(t, store, planned.Plan, authorization, result)
			}
			path := map[string]string{
				"root": store.postCompletionRepairRootPath(), "anchor": store.postCompletionRepairAnchorPath(),
				"plan": planned.PlanPath, "review": store.postCompletionRepairReviewPath(planned.Plan.ID),
				"result": store.postCompletionRepairResultPath(planned.Plan.ID), "proof": store.postCompletionRepairProofPath(planned.Plan.ID),
			}[kind]
			targetPath, targetBefore := replacePostCompletionRepairFileWithExactSymlink(t, path)
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}
			anchorBefore, err := os.ReadFile(store.postCompletionRepairAnchorPath())
			if err != nil {
				t.Fatal(err)
			}

			var mutationErr error
			switch kind {
			case "proof":
				_, mutationErr = store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, proof.ReceiptID, proof.TransitionID, mustRepairTime(t, proof.RecordedAt).Add(time.Minute))
			default:
				_, mutationErr = store.ApplyPostCompletionRepairOutcome(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			}
			if mutationErr == nil || !postCompletionRepairCustodyError(mutationErr) {
				t.Fatalf("matching-byte %s symlink was accepted by mutation/finalization: %v", kind, mutationErr)
			}
			if info, statErr := os.Lstat(path); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s refusal changed symlink custody: info=%v err=%v", kind, info, statErr)
			}
			if targetAfter, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(targetAfter, targetBefore) {
				t.Fatalf("%s refusal changed symlink target: %v", kind, readErr)
			}
			rootAfter, rootErr := os.ReadFile(store.postCompletionRepairRootPath())
			anchorAfter, anchorErr := os.ReadFile(store.postCompletionRepairAnchorPath())
			if rootErr != nil || anchorErr != nil || !bytes.Equal(rootAfter, rootBefore) || !bytes.Equal(anchorAfter, anchorBefore) {
				t.Fatalf("%s refusal changed repair root/anchor: root_err=%v anchor_err=%v", kind, rootErr, anchorErr)
			}
			if kind != "result" && kind != "proof" {
				for _, lifecyclePath := range []string{store.postCompletionRepairPendingPath(planned.Plan.ID), store.postCompletionRepairProgressPath(planned.Plan.ID), store.postCompletionRepairResultPath(planned.Plan.ID), store.postCompletionRepairProofPath(planned.Plan.ID)} {
					if _, statErr := os.Lstat(lifecyclePath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("%s refusal wrote lifecycle artifact %s: %v", kind, filepath.Base(lifecyclePath), statErr)
					}
				}
			}

			verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if verified.Completion.Complete || !postCompletionRepairReasonsContainCustodyError(verified.Completion.Reasons) {
				t.Fatalf("migrate --verify accepted %s symlink without a custody reason: %+v", kind, verified.Completion)
			}
		})
	}
}

func TestPostCompletionRepairRejectsSymlinkedProgressJournal(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterArchive:      func(string) error { return errors.New("stop with rooted progress") },
		resultNow:         func() time.Time { return repairNow.Add(2 * time.Minute) },
	})
	if err == nil || !strings.Contains(err.Error(), "stop with rooted progress") {
		t.Fatalf("progress fixture did not stop at injected boundary: %v", err)
	}
	progressPath := store.postCompletionRepairProgressPath(planned.Plan.ID)
	targetPath, targetBefore := replacePostCompletionRepairFileWithExactSymlink(t, progressPath)
	rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !postCompletionRepairCustodyError(err) {
		t.Fatalf("matching-byte progress symlink was accepted: %v", err)
	}
	if info, statErr := os.Lstat(progressPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("progress refusal changed symlink custody: info=%v err=%v", info, statErr)
	}
	if targetAfter, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(targetAfter, targetBefore) {
		t.Fatalf("progress refusal changed symlink target: %v", readErr)
	}
	rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
	if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
		t.Fatalf("progress refusal changed repair root: %v", readErr)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("progress refusal published result: exists=%v err=%v", exists, readErr)
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !postCompletionRepairReasonsContainCustodyError(verified.Completion.Reasons) {
		t.Fatalf("migrate --verify accepted progress symlink without a custody reason: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairVerifyRejectsSymlinkedInProgressQuarantineClaim(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	stopID := planned.Plan.Entries[0].ID
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		beforeLooseRemoval: func(id string) error {
			if id == stopID {
				return errors.New("stop with quarantine claim")
			}
			return nil
		},
		resultNow: func() time.Time { return repairNow.Add(2 * time.Minute) },
	})
	if err == nil || !strings.Contains(err.Error(), "stop with quarantine claim") {
		t.Fatalf("claim fixture did not stop at injected boundary: %v", err)
	}
	entry := planned.Plan.Entries[0]
	claimPath := store.postCompletionRepairClaimPath(planned.Plan.ID, entry)
	targetPath, targetBefore := replacePostCompletionRepairFileWithExactSymlink(t, claimPath)
	rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !postCompletionRepairCustodyError(err) {
		t.Fatalf("matching-byte quarantine claim symlink was accepted: %v", err)
	}
	if info, statErr := os.Lstat(claimPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("claim refusal changed symlink custody: info=%v err=%v", info, statErr)
	}
	if targetAfter, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(targetAfter, targetBefore) {
		t.Fatalf("claim refusal changed symlink target: %v", readErr)
	}
	rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
	if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
		t.Fatalf("claim refusal changed repair root: %v", readErr)
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !postCompletionRepairReasonsContainCustodyError(verified.Completion.Reasons) {
		t.Fatalf("migrate --verify accepted quarantine claim symlink without a custody reason: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairRejectsDanglingRecurrenceBeforeReplayOrProof(t *testing.T) {
	for _, operation := range []string{"replay", "proof"} {
		t.Run(operation, func(t *testing.T) {
			store := openMigrateStore(t)
			seedLoose(t, store, "rec_dangling_recurrence", "same")
			if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
				t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
			}
			canonical, err := store.Read("rec_dangling_recurrence")
			if err != nil {
				t.Fatal(err)
			}
			loosePath := filepath.Join(store.rootPath(), "rec_dangling_recurrence.json")
			if err := os.WriteFile(loosePath, canonical.Bytes, 0o644); err != nil {
				t.Fatal(err)
			}
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing.json"), loosePath); err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "replay":
				if _, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions()); err == nil || !strings.Contains(err.Error(), "reappeared without exact archived bytes") {
					t.Errorf("dangling recurrence was accepted during result replay: %v", err)
				}
			case "proof":
				receiptID, transitionID := seedPostCompletionRepairEnvelopeForTest(t, store, authorization, "_dangling_recurrence", mustRepairTime(t, result.CompletedAt), []string{
					"eventstore-repair-plan:" + authorization.PlanDigest,
					"eventstore-repair-review:" + authorization.ReviewDigest,
					"policy-authority:" + authorization.AuthorityRevision,
				})
				if _, err := store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "reappeared without exact archived bytes") {
					t.Errorf("dangling recurrence was accepted during proof finalization: %v", err)
				}
			}
			if _, exists, err := store.ReadPostCompletionRepairProof(planned.Plan.ID); err != nil || exists {
				t.Errorf("dangling recurrence wrote proof: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestPostCompletionRepairFinalPreflightTreatsDanglingLooseSymlinkAsPresent(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	if _, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions()); err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(store.rootPath(), "rec_absent.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing.json"), loosePath); err != nil {
		t.Fatal(err)
	}
	presence, err := store.engine.buildSegmentPresence()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.preflightPostCompletionRepairFinalWithPresence(planned.Plan, presence); err == nil || !strings.Contains(err.Error(), "final loose rec_absent remains") {
		t.Fatalf("final preflight treated dangling loose symlink as absent: %v", err)
	}
}

func TestPostCompletionRepairDriftFailsBeforeArchiveOrSegmentMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	driftID := "rec_absent"
	driftPath := filepath.Join(store.rootPath(), driftID+".json")
	before, err := os.ReadFile(driftPath)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, created)
	if err := os.WriteFile(driftPath, bytes.Replace(before, []byte("new-loose"), []byte("now-drift"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "writer fence drifted") {
		t.Fatalf("apply drift error = %v", err)
	}
	if got := segmentFrameCountForID(t, store, driftID); got != 0 {
		t.Fatalf("drifted apply appended %d frames", got)
	}
	if _, err := os.Stat(store.postCompletionRepairArchivePath(recordHash(before))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drifted apply wrote archive before refusing: %v", err)
	}
	if _, exists, err := store.ReadPostCompletionRepairPending(created.Plan.ID); err != nil || exists {
		t.Fatalf("drifted apply left pending mutation: exists=%v err=%v", exists, err)
	}
}

func TestPostCompletionRepairInterruptedAppendResumesTrackedAndReplayIsNoOp(t *testing.T) {
	store := postCompletionRepairFixture(t)
	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, created)
	injected := errors.New("injected interruption after segment append")
	_, err = store.ApplyPostCompletionRepair(created.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterSegmentAppend: func(id string) error {
			if id == "rec_absent" {
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("first apply error = %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 1 {
		t.Fatalf("interrupted append frames = %d, want 1", got)
	}
	if _, exists, err := store.ReadPostCompletionRepairPending(created.Plan.ID); err != nil || !exists {
		t.Fatalf("interruption was untracked: exists=%v err=%v", exists, err)
	}

	result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !result.Changed || result.Replayed {
		t.Fatalf("resume result = %+v", result)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 1 {
		t.Fatalf("resume duplicated segment frame: %d", got)
	}
	manifestBefore, err := os.ReadFile(store.postCompletionRepairResultPath(created.Plan.ID))
	if err != nil {
		t.Fatal(err)
	}
	segmentCountBefore := countSegmentFiles(t, store.engine.segRoot)

	replayed, err := store.ApplyPostCompletionRepairOutcome(created.PlanPath, authorization, PostCompletionRepairApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Changed || replayed.Result.ID != result.ID {
		t.Fatalf("replay = %+v, first=%+v", replayed, result)
	}
	manifestAfter, err := os.ReadFile(store.postCompletionRepairResultPath(created.Plan.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestAfter, manifestBefore) || countSegmentFiles(t, store.engine.segRoot) != segmentCountBefore || segmentFrameCountForID(t, store, "rec_absent") != 1 {
		t.Fatal("no-op replay changed result or segment state")
	}
}

func TestPostCompletionRepairFrameVerificationFailureKeepsLooseAndRetryUsesReservedFrame(t *testing.T) {
	store := postCompletionRepairFixture(t)
	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, created)
	injected := errors.New("injected exact-frame verification failure")
	_, err = store.ApplyPostCompletionRepair(created.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		verifyAppendedFrame: func(entry PostCompletionRepairEntry, _ string, _ int64) error {
			if entry.ID == "rec_absent" {
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("verification failure = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); err != nil {
		t.Fatalf("loose source removed before exact frame verification: %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 1 {
		t.Fatalf("reserved frame count after verification failure = %d", got)
	}

	result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 1 {
		t.Fatalf("retry duplicated reserved frame: %d", got)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry did not converge loose removal: %v", err)
	}
	if !result.Changed || result.Replayed {
		t.Fatalf("retry result = %+v", result)
	}
}

func TestPostCompletionRepairDayRolloverUsesCurrentSegmentAndPreservesBindings(t *testing.T) {
	beforeRollover := time.Date(2026, 7, 17, 23, 59, 0, 0, time.UTC)
	afterRollover := beforeRollover.Add(2 * time.Minute)
	current := beforeRollover
	store := postCompletionRepairFixture(t)
	store.engine.clock = func() time.Time { return current }

	planned, err := store.CreatePostCompletionRepairPlan(beforeRollover, 0)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	planBefore, err := os.ReadFile(planned.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewPath := store.postCompletionRepairReviewPath(planned.Plan.ID)
	reviewBefore, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}

	current = afterRollover
	wantSegment := filepath.Join(segmentsDirName, "receipt", ingestionDay(afterRollover)+".jsonl")
	loosePath := filepath.Join(store.rootPath(), "rec_absent.json")
	appendCalls, verificationCalls := 0, 0
	applyOptions := PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		resultNow:         func() time.Time { return repairNow.Add(2 * time.Minute) },
		afterSegmentAppend: func(id string) error {
			if id == "rec_absent" {
				appendCalls++
			}
			return nil
		},
		verifyAppendedFrame: func(entry PostCompletionRepairEntry, segment string, _ int64) error {
			if entry.ID != "rec_absent" {
				return nil
			}
			verificationCalls++
			if segment != wantSegment {
				return fmt.Errorf("reserved segment = %s, want current UTC segment %s", segment, wantSegment)
			}
			if _, err := os.Stat(loosePath); err != nil {
				return fmt.Errorf("loose source retired before exact verification boundary: %w", err)
			}
			return nil
		},
	}
	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, applyOptions)
	if err != nil {
		t.Fatal(err)
	}
	entry := repairResultEntry(result, "rec_absent")
	if appendCalls != 1 || verificationCalls != 1 || entry.Segment != wantSegment || segmentFrameCountForID(t, store, "rec_absent") != 1 {
		t.Fatalf("rollover repair append_calls=%d verification_calls=%d entry=%+v frame_count=%d", appendCalls, verificationCalls, entry, segmentFrameCountForID(t, store, "rec_absent"))
	}
	if _, err := os.Stat(loosePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exactly verified loose source was not retired: %v", err)
	}
	if result.PlanDigest != planned.Plan.PlanDigest || result.Authorization.ReviewID != authorization.ReviewID || result.Authorization.ReviewDigest != authorization.ReviewDigest || result.Authorization.WriterFenceDigest != authorization.WriterFenceDigest {
		t.Fatalf("rollover repair changed immutable plan/review bindings: result=%+v authorization=%+v", result.Authorization, authorization)
	}

	replayed, err := store.ApplyPostCompletionRepairOutcome(planned.PlanPath, authorization, applyOptions)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Changed || appendCalls != 1 || verificationCalls != 1 || segmentFrameCountForID(t, store, "rec_absent") != 1 {
		t.Fatalf("rollover replay=%+v append_calls=%d verification_calls=%d frame_count=%d", replayed, appendCalls, verificationCalls, segmentFrameCountForID(t, store, "rec_absent"))
	}
	proof := finalizePostCompletionRepairForTest(t, store, planned.Plan, authorization, replayed.Result)
	if proof.PlanDigest != planned.Plan.PlanDigest || proof.Authorization.ReviewDigest != authorization.ReviewDigest {
		t.Fatalf("final proof changed immutable plan/review bindings: %+v", proof)
	}
	planAfter, err := os.ReadFile(planned.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewAfter, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(planAfter, planBefore) || !bytes.Equal(reviewAfter, reviewBefore) {
		t.Fatal("rollover apply or finalization rewrote immutable plan/review artifacts")
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || !verified.Completion.Complete || verified.Completion.RemainingLoose != 0 {
		t.Fatalf("verify after rollover repair = %+v err=%v", verified.Completion, err)
	}
}

func TestPostCompletionRepairQuiescenceRefusesWritesDuringObservationAndAfterReview(t *testing.T) {
	t.Run("retired compatibility stream active", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		if err := os.WriteFile(filepath.Join(store.engine.eventsDir, hookEventsStreamName), []byte("late\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.CreatePostCompletionRepairPlan(repairNow, 0)
		if err == nil || !strings.Contains(err.Error(), "compatibility writer is active") {
			t.Fatalf("compatibility writer error = %v", err)
		}
	})

	t.Run("during plan observation", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		_, err := store.createPostCompletionRepairPlan(repairNow, postCompletionRepairPlanOptions{
			betweenWriterSnapshots: func() error {
				writeLegacyRecord(t, store, "rec_during_observation", eventRecord("rec_during_observation", schemaForID("rec_during_observation"), repairNow, "late"))
				return nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "changed source during") {
			t.Fatalf("plan observation error = %v", err)
		}
	})

	t.Run("during review observation", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.reviewPostCompletionRepair(planned.PlanPath, "operator:test", "authorize", true, repairNow.Add(time.Minute), postCompletionRepairReviewOptions{
			betweenWriterSnapshots: func() error {
				writeLegacyRecord(t, store, "rec_during_review", eventRecord("rec_during_review", schemaForID("rec_during_review"), repairNow, "late"))
				return nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "not quiescent") {
			t.Fatalf("review observation error = %v", err)
		}
	})

	t.Run("after review", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := repairTestAuthorization(t, store, planned)
		writeLegacyRecord(t, store, "rec_after_review", eventRecord("rec_after_review", schemaForID("rec_after_review"), repairNow, "late"))
		_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
		if err == nil || !strings.Contains(err.Error(), "writer fence drifted") {
			t.Fatalf("post-review writer error = %v", err)
		}
		if got := segmentFrameCountForID(t, store, "rec_absent"); got != 0 {
			t.Fatalf("post-review drift mutated segment before refusal: %d", got)
		}
	})
}

func TestPostCompletionRepairRechecksWriterFenceBeforeEachPhysicalMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	injected := false
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterArchive: func(string) error {
			if !injected {
				injected = true
				writeLegacyRecord(t, store, "rec_after_boundary", eventRecord("rec_after_boundary", schemaForID("rec_after_boundary"), repairNow, "late"))
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "writer fence drifted") {
		t.Fatalf("writer restart after archive was not fenced before the next mutation: %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 0 {
		t.Fatalf("writer drift reached segment append: %d", got)
	}
	for _, entry := range planned.Plan.Entries {
		if _, err := os.Stat(filepath.Join(store.rootPath(), entry.FileName)); err != nil {
			t.Fatalf("writer drift retired %s before refusal: %v", entry.ID, err)
		}
	}
}

func TestPostCompletionRepairSourceRetirementDoesNotDeleteAtomicReplacement(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	const targetID = "rec_absent"
	var target PostCompletionRepairEntry
	for _, entry := range planned.Plan.Entries {
		if entry.ID == targetID {
			target = entry
			break
		}
	}
	if target.ID == "" {
		t.Fatal("target missing from repair plan")
	}
	replacement, err := json.MarshalIndent(eventRecord(targetID, schemaForID(targetID), repairNow.Add(time.Minute), "replacement-during-retirement"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	replacement = append(replacement, '\n')
	replaced := false
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		beforeLooseRemoval: func(id string) error {
			if id != targetID || replaced {
				return nil
			}
			replaced = true
			return writeBytesAtomic(filepath.Join(store.rootPath(), target.FileName), replacement)
		},
	})
	if err == nil {
		t.Fatal("atomic replacement was deleted and repair incorrectly completed")
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("replacement drift published result: exists=%v err=%v", exists, readErr)
	}
	hot, err := os.ReadFile(filepath.Join(store.rootPath(), target.FileName))
	if err != nil || !bytes.Equal(hot, replacement) {
		t.Fatalf("atomic replacement was not left observable: err=%v", err)
	}
	archived, err := os.ReadFile(store.postCompletionRepairArchivePath(target.RawSHA256))
	if err != nil || recordHash(archived) != target.RawSHA256 {
		t.Fatalf("planned source body was lost: err=%v", err)
	}
	recoveryPath := filepath.Join(store.home, "test-recovery", target.FileName)
	if err := os.MkdirAll(filepath.Dir(recoveryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(store.rootPath(), target.FileName), recoveryPath); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(store.rootPath()); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(filepath.Dir(recoveryPath)); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("retry after preserving replacement: %v", err)
	}
	if !result.Changed || result.Replayed {
		t.Fatalf("retry result = %+v", result)
	}
	recoveredReplacement, err := os.ReadFile(recoveryPath)
	if err != nil || !bytes.Equal(recoveredReplacement, replacement) {
		t.Fatalf("preserved replacement changed during retry: err=%v", err)
	}
}

func TestPostCompletionRepairSourceSealRejectsUnrelatedLooseDriftAfterAtomicClaim(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	const driftID = "rec_unrelated_during_claim"
	injected := false
	claimedID := ""
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		beforeLooseRemoval: func(id string) error {
			if injected {
				return nil
			}
			injected = true
			claimedID = id
			writeLegacyRecord(t, store, driftID, eventRecord(driftID, schemaForID(driftID), repairNow.Add(time.Minute), "unrelated-drift"))
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "outside expected claim delta") {
		t.Fatalf("unrelated loose drift was absorbed after atomic claim: %v", err)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("unrelated loose drift published result: exists=%v err=%v", exists, readErr)
	}
	if _, readErr := os.ReadFile(filepath.Join(store.rootPath(), driftID+".json")); readErr != nil {
		t.Fatalf("unrelated loose source was not preserved: %v", readErr)
	}
	var claimed PostCompletionRepairEntry
	for _, entry := range planned.Plan.Entries {
		if entry.ID == claimedID {
			claimed = entry
			break
		}
	}
	archive, readErr := os.ReadFile(store.postCompletionRepairArchivePath(claimed.RawSHA256))
	if claimed.ID == "" || readErr != nil || recordHash(archive) != claimed.RawSHA256 {
		t.Fatalf("planned source %s was not preserved in its content-addressed archive: %v", claimedID, readErr)
	}
}

func TestPostCompletionRepairResumesAtomicClaimBeforeClaimedJournal(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	injected := errors.New("interrupt after atomic source claim")
	interrupted := false
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		beforeLooseRemoval: func(id string) error {
			if id == "rec_absent" && !interrupted {
				interrupted = true
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("claim interruption = %v", err)
	}
	progress := repairProgressForID(t, store, planned.Plan.ID, "rec_absent")
	if progress.Phase != repairPhaseRemoveReserved {
		t.Fatalf("claim interruption phase = %s, want %s", progress.Phase, repairPhaseRemoveReserved)
	}
	var target PostCompletionRepairEntry
	for _, entry := range planned.Plan.Entries {
		if entry.ID == "rec_absent" {
			target = entry
			break
		}
	}
	claimed, err := os.ReadFile(store.postCompletionRepairClaimPath(planned.Plan.ID, target))
	if err != nil || recordHash(claimed) != target.RawSHA256 {
		t.Fatalf("interrupted claim body missing or changed: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), target.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claimed source pathname unexpectedly remains: %v", err)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("claim interruption published result: exists=%v err=%v", exists, readErr)
	}
	verified, verifyErr := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if verifyErr != nil {
		t.Fatal(verifyErr)
	}
	if reasonsContain(verified.Completion.Reasons, "unowned quarantine entry") {
		t.Fatalf("active crash-recovery claim was treated as unowned: %+v", verified.Completion)
	}

	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("retry claimed source: %v", err)
	}
	if !result.Changed || result.Replayed {
		t.Fatalf("claim retry result = %+v", result)
	}
	if _, err := os.Stat(store.postCompletionRepairClaimPath(planned.Plan.ID, target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed retry left quarantine body: %v", err)
	}
}

func TestPostCompletionRepairAuthorityCallbackFencesEveryPhysicalMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	denied := errors.New("current authority changed")
	checks := 0
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			checks++
			if checks == 2 {
				return denied
			}
			return nil
		},
	})
	if !errors.Is(err, denied) {
		t.Fatalf("authority drift between archive and append was not enforced: checks=%d err=%v", checks, err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 0 {
		t.Fatalf("authority drift reached segment append: %d", got)
	}
	for _, entry := range planned.Plan.Entries {
		if _, err := os.Stat(filepath.Join(store.rootPath(), entry.FileName)); err != nil {
			t.Fatalf("authority drift retired %s before refusal: %v", entry.ID, err)
		}
	}
}

func TestPostCompletionRepairClaimCleanupPreservesAtomicReplacementAfterFinalAuthority(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	var replacementInfo os.FileInfo
	var replacedEntry PostCompletionRepairEntry
	replaced := false

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			if replaced {
				return nil
			}
			progress, loadErr := store.loadPostCompletionRepairProgressCurrent(planned.Plan)
			if errors.Is(loadErr, os.ErrNotExist) {
				return nil
			}
			if loadErr != nil {
				return loadErr
			}
			for index, entryProgress := range progress.Current.Entries {
				if entryProgress.Phase != repairPhaseClaimed {
					continue
				}
				replacedEntry = planned.Plan.Entries[index]
				claimPath := store.postCompletionRepairClaimPath(planned.Plan.ID, replacedEntry)
				body, readErr := os.ReadFile(claimPath)
				if readErr != nil {
					return readErr
				}
				beforeInfo, statErr := os.Stat(claimPath)
				if statErr != nil {
					return statErr
				}
				if writeErr := writeBytesAtomic(claimPath, body); writeErr != nil {
					return writeErr
				}
				replacementInfo, statErr = os.Stat(claimPath)
				if statErr != nil {
					return statErr
				}
				if os.SameFile(beforeInfo, replacementInfo) {
					return fmt.Errorf("atomic replacement did not replace the claimed inode")
				}
				replaced = true
				return nil
			}
			return nil
		},
	})
	if !replaced {
		t.Fatal("final authority callback never observed a claimed repair body")
	}
	if err == nil || !strings.Contains(err.Error(), "file changed after validation") {
		t.Fatalf("atomic replacement after final authority was not refused: %v", err)
	}
	claimPath := store.postCompletionRepairClaimPath(planned.Plan.ID, replacedEntry)
	afterInfo, statErr := os.Stat(claimPath)
	if statErr != nil || !os.SameFile(replacementInfo, afterInfo) {
		t.Fatalf("replacement inode was not preserved: before=%v after=%v err=%v", replacementInfo, afterInfo, statErr)
	}
	body, readErr := os.ReadFile(claimPath)
	if readErr != nil || recordHash(body) != replacedEntry.RawSHA256 {
		t.Fatalf("replacement body was not preserved: err=%v hash=%s", readErr, recordHash(body))
	}
	progress, loadErr := store.loadPostCompletionRepairProgressCurrent(planned.Plan)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	for index, entry := range planned.Plan.Entries {
		if entry.ID == replacedEntry.ID && progress.Current.Entries[index].Phase != repairPhaseClaimed {
			t.Fatalf("replacement refusal advanced phase to %q", progress.Current.Entries[index].Phase)
		}
	}
	if _, exists, resultErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); resultErr != nil || exists {
		t.Fatalf("replacement refusal published result: exists=%v err=%v", exists, resultErr)
	}
}

func TestPostCompletionRepairRequiresCurrentAuthorityBeforeAnyPhysicalMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "current policy authority") {
		t.Fatalf("nil current-authority validator was accepted: %v", err)
	}
	rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
	if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
		t.Fatalf("authority refusal changed repair root: err=%v", readErr)
	}
	if _, statErr := os.Stat(store.postCompletionRepairPendingPath(planned.Plan.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("authority refusal wrote pending state: %v", statErr)
	}
	if _, statErr := os.Stat(store.postCompletionRepairResultPath(planned.Plan.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("authority refusal wrote result: %v", statErr)
	}
	for _, entry := range planned.Plan.Entries {
		if _, statErr := os.Stat(filepath.Join(store.rootPath(), entry.FileName)); statErr != nil {
			t.Fatalf("authority refusal removed loose %s: %v", entry.ID, statErr)
		}
		if _, statErr := os.Stat(store.postCompletionRepairArchivePath(entry.RawSHA256)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("authority refusal wrote archive %s: %v", entry.ID, statErr)
		}
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 0 {
		t.Fatalf("authority refusal appended %d frame(s)", got)
	}
}

func TestPostCompletionRepairSealedFinalizationRequiresResultToBeOnlyMissingRootArtifact(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	injected := errors.New("interrupt after physical completion before result")
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			pending, exists, readErr := store.ReadPostCompletionRepairPending(planned.Plan.ID)
			if readErr != nil || !exists {
				return readErr
			}
			for _, entry := range pending.Entries {
				if entry.Phase != repairPhaseRemoved {
					return nil
				}
			}
			return injected
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("physical-completion interruption = %v", err)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairResult(planned.Plan.ID); readErr != nil || exists {
		t.Fatalf("interruption unexpectedly wrote result: exists=%v err=%v", exists, readErr)
	}
	if err := store.mutatePostCompletionRepairRoot(func(root *PostCompletionRepairRoot) error {
		artifacts := root.Artifacts[:0]
		removed := 0
		for _, artifact := range root.Artifacts {
			if artifact.Kind == "plan" && artifact.ID == planned.Plan.ID {
				removed++
				continue
			}
			artifacts = append(artifacts, artifact)
		}
		if removed != 1 {
			return fmt.Errorf("remove rooted plan artifacts = %d, want 1", removed)
		}
		root.Artifacts = artifacts
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sealed, err := store.PostCompletionRepairPhysicalCompletionForFinalization(planned.PlanPath, authorization)
	if err == nil || sealed {
		t.Fatalf("sealed finalization accepted missing rooted plan: sealed=%v err=%v", sealed, err)
	}
}

func TestPostCompletionRepairResultRemainsPendingUntilWorkProofFinalizes(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	store := postCompletionRepairFixture(t)
	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, created)
	injected := errors.New("injected interruption after durable result")
	_, err = store.ApplyPostCompletionRepair(created.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterResult:       func() error { return injected },
		resultNow:         func() time.Time { return repairNow.Add(2 * time.Minute) },
	})
	if !errors.Is(err, injected) {
		t.Fatalf("first apply error = %v", err)
	}
	if _, exists, err := store.ReadPostCompletionRepairPending(created.Plan.ID); err != nil || !exists {
		t.Fatalf("result interruption lost pending journal: exists=%v err=%v", exists, err)
	}
	if _, exists, err := store.ReadPostCompletionRepairResult(created.Plan.ID); err != nil || !exists {
		t.Fatalf("result interruption did not durably publish result: exists=%v err=%v", exists, err)
	}

	replayed, err := store.ApplyPostCompletionRepairOutcome(created.PlanPath, authorization, PostCompletionRepairApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Changed {
		t.Fatalf("retry = %+v", replayed)
	}
	if _, exists, err := store.ReadPostCompletionRepairPending(created.Plan.ID); err != nil || !exists {
		t.Fatalf("physical retry cleared journal before work proof: exists=%v err=%v", exists, err)
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "finalization pending") {
		t.Fatalf("verify should fail closed before work proof = %+v err=%v", verified.Completion, err)
	}

	finalizePostCompletionRepairForTest(t, store, created.Plan, authorization, replayed.Result)
	if _, exists, err := store.ReadPostCompletionRepairPending(created.Plan.ID); err != nil || exists {
		t.Fatalf("work proof finalization did not clear exact pending journal: exists=%v err=%v", exists, err)
	}
	verified, err = store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || !verified.Completion.Complete {
		t.Fatalf("verify after work proof = %+v err=%v", verified.Completion, err)
	}
}

func TestPostCompletionRepairMissingProgressJournalOnlyRecoversProofBackedCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	t.Run("missing rooted journal before proof refuses", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := repairTestAuthorization(t, store, planned)
		if _, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(store.postCompletionRepairProgressPath(planned.Plan.ID)); err != nil {
			t.Fatal(err)
		}

		if _, exists, err := store.ReadPostCompletionRepairPending(planned.Plan.ID); err == nil || exists {
			t.Fatalf("missing rooted journal without proof was accepted: exists=%v err=%v", exists, err)
		}
	})

	t.Run("journal removed after valid proof resumes cleanup", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := repairTestAuthorization(t, store, planned)
		result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
		proof, receiptID, transitionID := seedPostCompletionRepairProofWithoutCleanup(t, store, planned.Plan, authorization, result)
		if err := os.Remove(store.postCompletionRepairProgressPath(planned.Plan.ID)); err != nil {
			t.Fatal(err)
		}

		pending, exists, err := store.ReadPostCompletionRepairPending(planned.Plan.ID)
		if err != nil || !exists || pending.PlanID != planned.Plan.ID {
			t.Fatalf("proof-backed cleanup state was not readable: exists=%v pending=%+v err=%v", exists, pending, err)
		}
		retried, err := store.FinalizePostCompletionRepair(planned.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(3*time.Minute))
		if err != nil {
			t.Fatalf("proof-backed cleanup retry: %v", err)
		}
		if retried.ProofDigest != proof.ProofDigest {
			t.Fatalf("cleanup retry changed durable proof: got=%s want=%s", retried.ProofDigest, proof.ProofDigest)
		}
		if _, exists, err := store.ReadPostCompletionRepairPending(planned.Plan.ID); err != nil || exists {
			t.Fatalf("cleanup retry left pending state: exists=%v err=%v", exists, err)
		}
		root, _, _, err := store.loadPostCompletionRepairRoot()
		if err != nil {
			t.Fatal(err)
		}
		if postCompletionRepairRootOwns(root, "pending", planned.Plan.ID) || postCompletionRepairRootOwns(root, "progress", planned.Plan.ID) {
			t.Fatalf("cleanup retry left pending/progress root identities: %+v", root.Artifacts)
		}
	})

	t.Run("nonlinear journal refuses without loose removal", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		planned, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := repairTestAuthorization(t, store, planned)
		state, err := store.createPostCompletionRepairProgress(planned.Plan, authorization, repairNow)
		if err != nil {
			t.Fatal(err)
		}
		index := -1
		for i, entry := range planned.Plan.Entries {
			if entry.ID == "rec_absent" {
				index = i
				break
			}
		}
		if index < 0 {
			t.Fatal("rec_absent missing from repair plan")
		}
		entry := planned.Plan.Entries[index]
		segment, offset, err := store.reservePostCompletionRepairFrame(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.advancePostCompletionRepairProgress(&state, planned.Plan, index, repairPhaseAppendReserved, segment, offset, entry.CanonicalSHA256); err != nil {
			t.Fatal(err)
		}
		progressPath := store.postCompletionRepairProgressPath(planned.Plan.ID)
		body, err := os.ReadFile(progressPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(body[:len(body)-1], []byte{'\n'})
		if len(lines) != 2 {
			t.Fatalf("progress journal lines=%d want=2", len(lines))
		}
		var transition PostCompletionRepairProgressTransition
		if err := json.Unmarshal(lines[1], &transition); err != nil {
			t.Fatal(err)
		}
		transition.ToPhase = repairPhaseFrameVerified
		transition.TransitionDigest, err = postCompletionRepairTransitionDigest(transition)
		if err != nil {
			t.Fatal(err)
		}
		lines[1], err = json.Marshal(transition)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(progressPath, append(bytes.Join(lines, []byte{'\n'}), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
		if err == nil || !strings.Contains(err.Error(), "nonlinear") {
			t.Fatalf("nonlinear progress journal was accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); err != nil {
			t.Fatalf("nonlinear journal refusal removed loose source: %v", err)
		}
	})
}

func TestPostCompletionRepairVerifyFailsIfIndexedResultDisappears(t *testing.T) {
	store := postCompletionRepairFixture(t)
	created, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, created)
	result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	finalizePostCompletionRepairForTest(t, store, created.Plan, authorization, result)
	if err := os.Remove(store.postCompletionRepairResultPath(created.Plan.ID)); err != nil {
		t.Fatal(err)
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "indexed artifact missing") {
		t.Fatalf("deleted repair result disappeared from verification: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairRootRejectsProofDeletionAndOrphanArtifacts(t *testing.T) {
	t.Run("proof deletion", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		created, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		authorization := repairTestAuthorization(t, store, created)
		result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
		finalizePostCompletionRepairForTest(t, store, created.Plan, authorization, result)
		if err := os.Remove(store.postCompletionRepairProofPath(created.Plan.ID)); err != nil {
			t.Fatal(err)
		}
		verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
		if err != nil {
			t.Fatal(err)
		}
		if verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "indexed artifact missing") {
			t.Fatalf("deleted proof disappeared from verification: %+v", verified.Completion)
		}
	})

	t.Run("orphan plan", func(t *testing.T) {
		store := postCompletionRepairFixture(t)
		created, err := store.CreatePostCompletionRepairPlan(repairNow)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(created.PlanPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.engine.postCompletionRepairRoot(), "esrp_orphan.plan.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
		if err != nil {
			t.Fatal(err)
		}
		if verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "orphan artifact not indexed") {
			t.Fatalf("orphan plan was not rejected: %+v", verified.Completion)
		}
	})
}

func TestPostCompletionRepairVerifyRejectsUnownedQuarantineInventory(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		rel  string
		dir  bool
	}{
		{name: "orphan file", rel: "orphan.claim"},
		{name: "stale directory", rel: "esrp_stale", dir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			created, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, created)
			result, err := store.ApplyPostCompletionRepair(created.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatal(err)
			}
			finalizePostCompletionRepairForTest(t, store, created.Plan, authorization, result)

			path := filepath.Join(store.engine.postCompletionRepairRoot(), "quarantine", tc.rel)
			if tc.dir {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := writeBytesAtomic(path, []byte("orphan\n")); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Lstat(path)
			if err != nil || info.IsDir() != tc.dir {
				t.Fatalf("quarantine fixture kind mismatch: dir=%v info=%v err=%v", tc.dir, info, err)
			}

			verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "unowned quarantine entry") {
				t.Fatalf("unowned quarantine inventory was accepted: %+v", verified.Completion)
			}
		})
	}
}

func TestPostCompletionRepairConflictOnlyHistoryCannotCountToZeroWhenRepairDirectoryDisappears(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "trace_conflict_only", "canonical")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("initial migration: %+v err=%v", report.Completion, err)
	}
	writeLegacyRecord(t, store, "trace_conflict_only", eventRecord("trace_conflict_only", schemaForID("trace_conflict_only"), repairNow, "divergent"))
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Plan.Counts[RepairArchiveDivergent] != 1 || planned.Plan.Counts[RepairImportAbsent] != 0 {
		t.Fatalf("conflict-only plan = %+v", planned.Plan.Counts)
	}
	authorization := repairTestAuthorization(t, store, planned)
	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	finalizePostCompletionRepairForTest(t, store, planned.Plan, authorization, result)
	if err := os.RemoveAll(store.engine.postCompletionRepairRoot()); err != nil {
		t.Fatal(err)
	}
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || verified.Completion.RepairManifests != 0 || !reasonsContain(verified.Completion.Reasons, "repair authority anchor") {
		t.Fatalf("conflict-only history silently vanished: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairImmutablePlanCannotBeRecomputedAndReblessed(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
	if err != nil {
		t.Fatal(err)
	}
	anchorBefore, err := os.ReadFile(store.postCompletionRepairAnchorPath())
	if err != nil {
		t.Fatal(err)
	}

	tampered := planned.Plan
	tampered.CreatedAt = repairNow.Add(24 * time.Hour).Format(time.RFC3339Nano)
	tampered.PlanDigest, err = postCompletionRepairPlanDigest(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePostCompletionRepairPlan(tampered); err != nil {
		t.Fatalf("tampered plan should remain internally self-consistent: %v", err)
	}
	body, err := json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planned.PlanPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CreatePostCompletionRepairPlan(repairNow.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "immutable repair artifact drift") {
		t.Fatalf("recomputed immutable plan was re-blessed: %v", err)
	}
	rootAfter, _ := os.ReadFile(store.postCompletionRepairRootPath())
	anchorAfter, _ := os.ReadFile(store.postCompletionRepairAnchorPath())
	if !bytes.Equal(rootAfter, rootBefore) || !bytes.Equal(anchorAfter, anchorBefore) {
		t.Fatal("immutable plan drift changed repair root or external anchor")
	}
}

func TestPostCompletionRepairConcurrentApplySerializesRootAndPhysicalMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	start := make(chan struct{})
	results := make(chan PostCompletionRepairApplyOutcome, 2)
	errs := make(chan error, 2)
	var appends atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.ApplyPostCompletionRepairOutcome(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
				ValidateAuthority: authorizedPostCompletionRepairMutation,
				afterSegmentAppend: func(id string) error {
					if id == "rec_absent" {
						appends.Add(1)
					}
					return nil
				},
			})
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply must serialize, not lose one caller: %v", err)
		}
	}
	changed, replayed := 0, 0
	for result := range results {
		if result.Replayed {
			replayed++
		} else if result.Changed {
			changed++
		}
	}
	if changed != 1 || replayed != 1 || appends.Load() != 1 || segmentFrameCountForID(t, store, "rec_absent") != 1 {
		t.Fatalf("concurrent outcomes changed=%d replayed=%d physical_appends=%d", changed, replayed, appends.Load())
	}
	root, _, exists, err := store.loadPostCompletionRepairRoot()
	if err != nil || !exists {
		t.Fatalf("load repair root: exists=%v err=%v", exists, err)
	}
	anchor, anchorExists, err := store.loadPostCompletionRepairAnchor()
	if err != nil || !anchorExists || verifyPostCompletionRepairRootAnchor(root, true, anchor, true) != nil {
		t.Fatalf("root/anchor lost an update: root=%+v anchor=%+v exists=%v err=%v", root, anchor, anchorExists, err)
	}
	seen := map[string]bool{}
	for _, artifact := range root.Artifacts {
		key := artifact.Kind + ":" + artifact.ID
		if seen[key] {
			t.Fatalf("duplicate rooted artifact %s", key)
		}
		seen[key] = true
	}
}

func TestPostCompletionRepairApplyUsesOneLinearSourceCensus(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	store := postCompletionRepairFixture(t)
	for index := range 32 {
		id := fmt.Sprintf("rec_linear_%02d", index)
		writeLegacyRecord(t, store, id, eventRecord(id, schemaForID(id), repairNow, fmt.Sprintf("linear-%02d", index)))
	}
	planned, err := store.CreatePostCompletionRepairPlan(repairNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	type observedApplier interface {
		applyPostCompletionRepair(string, PostCompletionRepairAuthorization, PostCompletionRepairApplyOptions, func(string)) (PostCompletionRepairResult, error)
	}
	applier, ok := any(store).(observedApplier)
	if !ok {
		t.Fatal("post-completion repair linear source-census instrumentation is unavailable")
	}
	inspections := map[string]int{}
	policyChecks := 0
	_, err = applier.applyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: func() error {
			policyChecks++
			return nil
		},
	}, func(kind string) { inspections[kind]++ })
	if err != nil {
		t.Fatal(err)
	}
	if inspections["full_census"] != 1 || inspections["final_loose_census"] != 1 || inspections["segment_presence"] != 1 || inspections["loose_record"] != planned.Plan.Total {
		t.Fatalf("source inspections = %v for %d entries, want one census/presence pass and one loose read per entry", inspections, planned.Plan.Total)
	}
	if policyChecks <= planned.Plan.Total {
		t.Fatalf("test did not exercise repeated mutation policy checks: %d for %d entries", policyChecks, planned.Plan.Total)
	}
}

func TestPostCompletionRepairAppendLockDoesNotBlockNormalWriterDuringArchiveWork(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	writerDone := make(chan error, 1)
	var started sync.Once
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterArchive: func(string) error {
			var writerErr error
			started.Do(func() {
				go func() {
					_, putErr := store.PutJSON("rec_concurrent_normal", eventRecord("rec_concurrent_normal", schemaForID("rec_concurrent_normal"), repairNow, "normal-writer"), "  ")
					writerDone <- putErr
				}()
				select {
				case writerErr = <-writerDone:
				case <-time.After(2 * time.Second):
					writerErr = fmt.Errorf("normal segment writer remained blocked outside segment-sensitive repair work")
				}
			})
			return writerErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("rec_concurrent_normal"); err != nil {
		t.Fatalf("concurrent normal writer did not publish: %v", err)
	}
}

func TestPostCompletionRepairResumesExactReservedTornFrame(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		cut  func([]byte) int
	}{
		{name: "torn header", cut: func([]byte) int { return 12 }},
		{name: "torn body", cut: func(line []byte) int { return len(line) - 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			line := seedReservedTornRepairFrame(t, store, planned.Plan, authorization, nil)
			segment := repairProgressForID(t, store, planned.Plan.ID, "rec_absent").Segment
			segmentPath := filepath.Join(store.engine.eventsDir, segment)
			if err := appendRepairTestBytes(segmentPath, line[:tc.cut(line)]); err != nil {
				t.Fatal(err)
			}

			result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
			if err != nil {
				t.Fatalf("resume exact torn frame: %v", err)
			}
			if !result.Changed || result.Replayed || segmentFrameCountForID(t, store, "rec_absent") != 1 {
				t.Fatalf("torn-frame result=%+v frame_count=%d", result, segmentFrameCountForID(t, store, "rec_absent"))
			}
			if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("verified torn-frame retry did not retire loose source: %v", err)
			}
		})
	}
}

func TestPostCompletionRepairResumesOnlyExactTornPhaseJournalAppend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
	}{
		{name: "exact prefix"},
		{name: "foreign bytes refuse", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, planned)
			state, err := store.createPostCompletionRepairProgress(planned.Plan, authorization, repairNow)
			if err != nil {
				t.Fatal(err)
			}
			index := -1
			for i, entry := range planned.Plan.Entries {
				if entry.ID == "rec_identical" {
					index = i
					break
				}
			}
			if index < 0 {
				t.Fatal("rec_identical missing from plan")
			}
			line := repairTestPhaseTransitionLine(t, state, planned.Plan, index, repairPhaseFrameVerified)
			prefix := append([]byte(nil), line[:len(line)/2]...)
			if tc.foreign {
				prefix[len(prefix)-1] ^= 0x1
			}
			if err := appendRepairTestBytes(store.postCompletionRepairProgressPath(planned.Plan.ID), prefix); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.loadPostCompletionRepairProgress(planned.Plan, authorization)
			if err != nil {
				t.Fatalf("load exact complete prefix before torn suffix: %v", err)
			}
			err = store.advancePostCompletionRepairProgress(&loaded, planned.Plan, index, repairPhaseFrameVerified, "", 0, "")
			if tc.foreign {
				if err == nil || !strings.Contains(err.Error(), "torn") {
					t.Fatalf("foreign journal suffix accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("recover exact phase prefix: %v", err)
			}
			reloaded, err := store.loadPostCompletionRepairProgress(planned.Plan, authorization)
			if err != nil || reloaded.Current.Entries[index].Phase != repairPhaseFrameVerified {
				t.Fatalf("reloaded recovered phase = %+v err=%v", reloaded.Current.Entries[index], err)
			}
		})
	}
}

func TestPostCompletionRepairResumesOnlyExactTornReauthorizationJournalAppend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
	}{
		{name: "exact prefix"},
		{name: "foreign bytes refuse", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			planned, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			initial := sealedRepairTestAuthorization(t, store, planned)
			state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
			if err != nil {
				t.Fatal(err)
			}
			next := nextSealedRepairTestAuthorization(t, initial, "torn", repairNow.Add(time.Minute), 0)
			line := repairTestReauthorizationTransitionLine(t, state, planned.Plan, next)
			prefix := append([]byte(nil), line[:len(line)/2]...)
			if tc.foreign {
				prefix[len(prefix)-1] ^= 0x1
			}
			if err := appendRepairTestBytes(store.postCompletionRepairProgressPath(planned.Plan.ID), prefix); err != nil {
				t.Fatal(err)
			}
			err = store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil })
			if tc.foreign {
				if err == nil || !strings.Contains(err.Error(), "torn") {
					t.Fatalf("foreign reauthorization suffix accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("recover exact reauthorization prefix: %v", err)
			}
			reloaded, err := store.loadPostCompletionRepairProgress(planned.Plan, next)
			if err != nil || len(reloaded.AuthorizationHistory) != 2 || !postCompletionRepairAuthorizationEqual(reloaded.AuthorizationHistory[0], initial) || !postCompletionRepairAuthorizationEqual(reloaded.AuthorizationHistory[1], next) {
				t.Fatalf("recovered authorization history = %+v err=%v", reloaded.AuthorizationHistory, err)
			}
		})
	}
}

func TestPostCompletionRepairTornRecoveryAppendsOnlyMissingSuffix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("append-only file flag proof requires Darwin chflags")
	}
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "append-only-retry", repairNow.Add(time.Minute), 0)
	line := repairTestReauthorizationTransitionLine(t, state, planned.Plan, next)
	prefix := append([]byte(nil), line[:len(line)/3]...)
	progressPath := store.postCompletionRepairProgressPath(planned.Plan.ID)
	if err := appendRepairTestBytes(progressPath, prefix); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("chflags", "uappnd", progressPath).CombinedOutput(); err != nil {
		t.Skipf("append-only file flag unavailable: %v (%s)", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("chflags", "nouappnd", progressPath).Run()
	})
	if err := store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil }); err != nil {
		t.Fatalf("append missing suffix without truncation: %v", err)
	}
	final, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	wantFinal := append(append([]byte(nil), state.JournalBytes...), line...)
	if !bytes.Equal(final, wantFinal) {
		t.Fatalf("recovery retry did not converge to one exact transition: got=%d want=%d", len(final), len(wantFinal))
	}
}

func TestPostCompletionRepairTornRecoverySyncsPartialSuffixBeforeRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	complete := []byte("header\n")
	expected := []byte("{\"kind\":\"reauthorize\",\"sequence\":1}\n")
	torn := append([]byte(nil), expected[:9]...)
	if err := os.WriteFile(path, append(append([]byte(nil), complete...), torn...), 0o644); err != nil {
		t.Fatal(err)
	}
	suffix, err := recoverPostCompletionRepairTornTransition(filepath.Dir(path), path, complete, torn, expected)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupted progress write")
	writer := &repairTestInterruptedSyncWriter{file: file, remaining: 7, err: injected}
	err = writePostCompletionRepairProgressSuffix(writer, suffix)
	closeErr := file.Close()
	if !errors.Is(err, injected) {
		t.Fatalf("partial write error = %v", err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if writer.syncCalls != 1 {
		t.Fatalf("partial progress sync calls = %d want 1", writer.syncCalls)
	}
	longer, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantLonger := append(append([]byte(nil), complete...), expected[:len(torn)+7]...)
	if !bytes.Equal(longer, wantLonger) {
		t.Fatalf("partial progress bytes = %q want %q", longer, wantLonger)
	}
	longerTorn := longer[len(complete):]
	remaining, err := recoverPostCompletionRepairTornTransition(filepath.Dir(path), path, complete, longerTorn, expected)
	if err != nil {
		t.Fatalf("recognize longer torn prefix: %v", err)
	}
	file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := writePostCompletionRepairProgressSuffix(file, remaining)
	closeErr = file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatalf("complete longer torn prefix: %v", err)
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantFinal := append(append([]byte(nil), complete...), expected...)
	if !bytes.Equal(final, wantFinal) {
		t.Fatalf("recovery retry did not converge exactly once: got=%q want=%q", final, wantFinal)
	}
}

type repairTestInterruptedSyncWriter struct {
	file      *os.File
	remaining int
	err       error
	syncCalls int
}

func (w *repairTestInterruptedSyncWriter) Write(body []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	n := w.remaining
	if n > len(body) {
		n = len(body)
	}
	w.remaining -= n
	written, writeErr := w.file.Write(body[:n])
	if writeErr != nil {
		return written, writeErr
	}
	return written, w.err
}

func (w *repairTestInterruptedSyncWriter) Sync() error {
	w.syncCalls++
	return w.file.Sync()
}

func TestPostCompletionRepairRecoversTornPhaseBeforePolicyAdvance(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_only_identical", "same")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}
	record, err := store.Read("rec_only_identical")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath(), "rec_only_identical.json"), record.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	entry := planned.Plan.Entries[0]
	if entry.ID != "rec_only_identical" || entry.Disposition != RepairArchiveIdentical {
		t.Fatalf("phase recovery fixture entry = %+v", entry)
	}
	if err := store.writePostCompletionRepairArchive(entry, record.Bytes); err != nil {
		t.Fatal(err)
	}
	line := repairTestPhaseTransitionLine(t, state, planned.Plan, 0, repairPhaseFrameVerified)
	if err := appendRepairTestBytes(store.postCompletionRepairProgressPath(planned.Plan.ID), line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	before, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || before.Completion.Complete || !reasonsContain(before.Completion.Reasons, "torn final transition awaits exact retry") {
		t.Fatalf("verification did not expose torn phase before retry: completion=%+v err=%v", before.Completion, err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "policy-b", repairNow.Add(time.Minute), 0)
	if err := store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil }); err != nil {
		t.Fatalf("recover phase before policy advance: %v", err)
	}
	reloaded, err := store.loadPostCompletionRepairProgress(planned.Plan, next)
	if err != nil || len(reloaded.TornTail) != 0 || reloaded.Current.Entries[0].Phase != repairPhaseFrameVerified || len(reloaded.AuthorizationHistory) != 2 {
		t.Fatalf("phase/policy history did not converge exactly: state=%+v history=%+v err=%v", reloaded.Current.Entries[0], reloaded.AuthorizationHistory, err)
	}
	after, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || reasonsContain(after.Completion.Reasons, "torn final transition awaits exact retry") {
		t.Fatalf("verification still reports torn phase after retry: completion=%+v err=%v", after.Completion, err)
	}
}

func TestPostCompletionRepairRecoversTornReauthorizationBeforeNewerPolicy(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	policyB := nextSealedRepairTestAuthorization(t, initial, "policy-b", repairNow.Add(time.Minute), 0)
	line := repairTestReauthorizationTransitionLine(t, state, planned.Plan, policyB)
	if err := appendRepairTestBytes(store.postCompletionRepairProgressPath(planned.Plan.ID), line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	before, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || before.Completion.Complete || !reasonsContain(before.Completion.Reasons, "torn final transition awaits exact retry") {
		t.Fatalf("verification did not expose torn reauthorization before retry: completion=%+v err=%v", before.Completion, err)
	}
	policyC := nextSealedRepairTestAuthorization(t, policyB, "newer-policy-c", repairNow.Add(2*time.Minute), 0)
	if err := store.ReauthorizePostCompletionRepair(planned.PlanPath, policyB, policyC, func() error { return nil }); err != nil {
		t.Fatalf("recover prior reauthorization before newer policy: %v", err)
	}
	reloaded, err := store.loadPostCompletionRepairProgress(planned.Plan, policyC)
	if err != nil || len(reloaded.TornTail) != 0 || reloaded.LastSequence != 2 || len(reloaded.AuthorizationHistory) != 3 {
		t.Fatalf("A->B->C authorization history did not converge exactly: sequence=%d history=%+v err=%v", reloaded.LastSequence, reloaded.AuthorizationHistory, err)
	}
	if !postCompletionRepairAuthorizationEqual(reloaded.AuthorizationHistory[0], initial) || !postCompletionRepairAuthorizationEqual(reloaded.AuthorizationHistory[1], policyB) || !postCompletionRepairAuthorizationEqual(reloaded.AuthorizationHistory[2], policyC) {
		t.Fatalf("authorization history reordered or collapsed: %+v", reloaded.AuthorizationHistory)
	}
	after, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || reasonsContain(after.Completion.Reasons, "torn final transition awaits exact retry") {
		t.Fatalf("verification still reports torn reauthorization after retry: completion=%+v err=%v", after.Completion, err)
	}
}

func TestPostCompletionRepairTornJournalPrefixClassifierCoversEveryByteBoundary(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	phaseIndex := -1
	for i, entry := range planned.Plan.Entries {
		if entry.ID == "rec_identical" {
			phaseIndex = i
			break
		}
	}
	if phaseIndex < 0 {
		t.Fatal("rec_identical missing from plan")
	}
	next := nextSealedRepairTestAuthorization(t, initial, "every-byte", repairNow.Add(time.Minute), 0)
	lines := map[string][]byte{
		"phase":           repairTestPhaseTransitionLine(t, state, planned.Plan, phaseIndex, repairPhaseFrameVerified),
		"reauthorization": repairTestReauthorizationTransitionLine(t, state, planned.Plan, next),
	}
	for name, line := range lines {
		t.Run(name, func(t *testing.T) {
			for cut := 1; cut < len(line); cut++ {
				prefix := append([]byte(nil), line[:cut]...)
				if err := validatePostCompletionRepairTornTransitionPrefix(prefix, line); err != nil {
					t.Fatalf("exact prefix cut %d/%d refused: %v", cut, len(line), err)
				}
				foreign := append([]byte(nil), prefix...)
				foreign[len(foreign)-1] ^= 0x1
				if err := validatePostCompletionRepairTornTransitionPrefix(foreign, line); err == nil {
					t.Fatalf("foreign prefix cut %d/%d accepted", cut, len(line))
				}
			}
			if err := validatePostCompletionRepairTornTransitionPrefix(line, line); err == nil {
				t.Fatal("complete transition without a torn boundary was accepted as a recoverable prefix")
			}
			if err := validatePostCompletionRepairTornTransitionPrefix(append(append([]byte(nil), line...), 'x'), line); err == nil {
				t.Fatal("later bytes after expected transition were accepted")
			}
		})
	}
}

func TestPostCompletionRepairTornJournalRefusesCrossCandidatePrefixAmbiguity(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_only_identical", "same")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("complete initial migration: report=%+v err=%v", report.Completion, err)
	}
	record, err := store.Read("rec_only_identical")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath(), "rec_only_identical.json"), record.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	state, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writePostCompletionRepairArchive(planned.Plan.Entries[0], record.Bytes); err != nil {
		t.Fatal(err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "ambiguous", repairNow.Add(time.Minute), 0)
	phaseLine := repairTestPhaseTransitionLine(t, state, planned.Plan, 0, repairPhaseFrameVerified)
	authorizationLine := repairTestReauthorizationTransitionLine(t, state, planned.Plan, next)
	shared := 0
	for shared < len(phaseLine) && shared < len(authorizationLine) && phaseLine[shared] == authorizationLine[shared] {
		shared++
	}
	if shared == 0 || shared >= len(phaseLine) || shared >= len(authorizationLine) {
		t.Fatalf("phase and authorization transitions lack a useful shared torn prefix: phase=%d authorization=%d shared=%d", len(phaseLine), len(authorizationLine), shared)
	}
	prefix := append([]byte(nil), phaseLine[:shared]...)
	if err := appendRepairTestBytes(store.postCompletionRepairProgressPath(planned.Plan.ID), prefix); err != nil {
		t.Fatal(err)
	}

	err = store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("cross-candidate torn prefix selected one transition: %v", err)
	}
	reloaded, loadErr := store.loadPostCompletionRepairProgress(planned.Plan, initial)
	if loadErr != nil || reloaded.LastSequence != 0 || len(reloaded.AuthorizationHistory) != 1 || !bytes.Equal(reloaded.TornTail, prefix) {
		t.Fatalf("ambiguous prefix mutated journal: sequence=%d history=%d torn=%d err=%v", reloaded.LastSequence, len(reloaded.AuthorizationHistory), len(reloaded.TornTail), loadErr)
	}
}

func TestPostCompletionRepairRefusesReservedTornFrameWithLaterBytes(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	laterBody, err := json.Marshal(eventRecord("rec_later", schemaForID("rec_later"), repairNow, "later"))
	if err != nil {
		t.Fatal(err)
	}
	laterFrame, _, err := buildFrame("rec_later", laterBody)
	if err != nil {
		t.Fatal(err)
	}
	laterLine, err := encodeFrame(laterFrame)
	if err != nil {
		t.Fatal(err)
	}
	line := seedReservedTornRepairFrame(t, store, planned.Plan, authorization, nil)
	progress := repairProgressForID(t, store, planned.Plan.ID, "rec_absent")
	if err := appendRepairTestBytes(filepath.Join(store.engine.eventsDir, progress.Segment), append(append([]byte(nil), line[:12]...), laterLine...)); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "cannot safely recover reserved torn frame") {
		t.Fatalf("later bytes after torn reservation were accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); err != nil {
		t.Fatalf("refusal removed loose source: %v", err)
	}
}

func TestPostCompletionRepairProgressUsesAppendOnlyJournalWithoutSnapshotOrRootRewrite(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	stopAfterAppend := errors.New("stop after append")
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterSegmentAppend: func(id string) error {
			if id == "rec_absent" {
				return stopAfterAppend
			}
			return nil
		},
	})
	if !errors.Is(err, stopAfterAppend) {
		t.Fatalf("first interruption: %v", err)
	}
	pendingPath := store.postCompletionRepairPendingPath(planned.Plan.ID)
	progressPath := filepath.Join(store.engine.postCompletionRepairRoot(), planned.Plan.ID+".progress.jsonl")
	pendingBefore, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	progressBefore, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatalf("append-only progress journal missing: %v", err)
	}
	rootBefore, _ := os.ReadFile(store.postCompletionRepairRootPath())
	anchorBefore, _ := os.ReadFile(store.postCompletionRepairAnchorPath())
	stopAfterVerify := errors.New("stop after exact verify")
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		verifyAppendedFrame: func(entry PostCompletionRepairEntry, _ string, _ int64) error {
			if entry.ID == "rec_absent" {
				return stopAfterVerify
			}
			return nil
		},
	})
	if !errors.Is(err, stopAfterVerify) {
		t.Fatalf("second interruption: %v", err)
	}
	pendingAfter, _ := os.ReadFile(pendingPath)
	progressAfter, _ := os.ReadFile(progressPath)
	rootAfter, _ := os.ReadFile(store.postCompletionRepairRootPath())
	anchorAfter, _ := os.ReadFile(store.postCompletionRepairAnchorPath())
	if !bytes.Equal(pendingBefore, pendingAfter) {
		t.Fatalf("progress transition rewrote the %d-byte full pending snapshot", len(pendingBefore))
	}
	if len(progressAfter) <= len(progressBefore) || len(progressAfter)-len(progressBefore) > 2048 {
		t.Fatalf("progress append delta=%d before=%d after=%d", len(progressAfter)-len(progressBefore), len(progressBefore), len(progressAfter))
	}
	if !bytes.Equal(rootBefore, rootAfter) || !bytes.Equal(anchorBefore, anchorAfter) {
		t.Fatal("one progress transition rewrote repair root/anchor instead of extending the owned journal chain")
	}
	root, _, _, err := store.loadPostCompletionRepairRoot()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, artifact := range root.Artifacts {
		kinds[artifact.Kind] = true
	}
	if !kinds["pending"] || !kinds["progress"] {
		t.Fatalf("repair root does not own immutable pending + progress identities: %+v", root.Artifacts)
	}
}

func TestPostCompletionRepairRefusesValidPrefixJournalTruncation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	stopAfterAppend := errors.New("stop after physical append")
	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		afterSegmentAppend: func(id string) error {
			if id == "rec_absent" {
				return stopAfterAppend
			}
			return nil
		},
	})
	if !errors.Is(err, stopAfterAppend) {
		t.Fatalf("first interruption: %v", err)
	}
	progressPath := store.postCompletionRepairProgressPath(planned.Plan.ID)
	body, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := bytes.IndexByte(body, '\n')
	if headerEnd < 0 {
		t.Fatal("progress journal header missing")
	}
	if err := os.WriteFile(progressPath, body[:headerEnd+1], 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "progress history") {
		t.Fatalf("valid-prefix journal truncation was accepted: %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_absent"); got != 1 {
		t.Fatalf("truncated journal replay duplicated the physical frame: %d", got)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_absent.json")); err != nil {
		t.Fatalf("truncated journal refusal removed loose source: %v", err)
	}
}

func TestPostCompletionRepairRefusesAuthorizationOnlySuffixTruncation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	if _, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow); err != nil {
		t.Fatal(err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "second", repairNow.Add(time.Minute), 0)
	if err := store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	progressPath := store.postCompletionRepairProgressPath(planned.Plan.ID)
	body, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	lastRecord := bytes.LastIndex(body[:len(body)-1], []byte{'\n'})
	if lastRecord < 0 {
		t.Fatal("reauthorization transition missing")
	}
	if err := os.WriteFile(progressPath, body[:lastRecord+1], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.loadPostCompletionRepairProgressCurrent(planned.Plan); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("authorization-only valid-prefix truncation was accepted: %v", err)
	}
	for _, entry := range planned.Plan.Entries {
		if _, err := os.Stat(filepath.Join(store.rootPath(), entry.FileName)); err != nil {
			t.Fatalf("authorization truncation refusal removed %s: %v", entry.ID, err)
		}
	}
}

func TestPostCompletionRepairFinalizationCleansReauthorizedProgressExactly(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	if _, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow); err != nil {
		t.Fatal(err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "second", repairNow.Add(time.Minute), 0)
	if err := store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, next, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AuthorizationHistory) != 2 || !postCompletionRepairAuthorizationEqual(result.AuthorizationHistory[0], initial) || !postCompletionRepairAuthorizationEqual(result.AuthorizationHistory[1], next) {
		t.Fatalf("reauthorized result history = %+v", result.AuthorizationHistory)
	}
	proof, receiptID, transitionID := seedPostCompletionRepairProofWithoutCleanup(t, store, planned.Plan, next, result)
	if err := os.Remove(store.postCompletionRepairProgressPath(planned.Plan.ID)); err != nil {
		t.Fatal(err)
	}
	if pending, exists, err := store.ReadPostCompletionRepairPending(planned.Plan.ID); err != nil || !exists || !postCompletionRepairAuthorizationEqual(pending.Authorization, initial) {
		t.Fatalf("proof-backed reauthorized cleanup state unreadable: exists=%v pending=%+v err=%v", exists, pending, err)
	}
	retried, err := store.FinalizePostCompletionRepair(planned.Plan.ID, next, receiptID, transitionID, repairNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("reauthorized cleanup retry: %v", err)
	}
	if retried.ProofDigest != proof.ProofDigest {
		t.Fatalf("cleanup retry changed proof: got=%s want=%s", retried.ProofDigest, proof.ProofDigest)
	}
	if _, exists, err := store.ReadPostCompletionRepairPending(planned.Plan.ID); err != nil || exists {
		t.Fatalf("reauthorized cleanup left pending state: exists=%v err=%v", exists, err)
	}
}

func TestPostCompletionRepairRejectsOversizedReauthorizationBeforeJournalMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	initial := sealedRepairTestAuthorization(t, store, planned)
	if _, err := store.createPostCompletionRepairProgress(planned.Plan, initial, repairNow); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.postCompletionRepairProgressPath(planned.Plan.ID))
	if err != nil {
		t.Fatal(err)
	}
	next := nextSealedRepairTestAuthorization(t, initial, "oversized", repairNow.Add(time.Minute), maxPostCompletionRepairJournalLine)
	err = store.ReauthorizePostCompletionRepair(planned.PlanPath, initial, next, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "journal bound") {
		t.Fatalf("oversized reauthorization was accepted: %v", err)
	}
	after, readErr := os.ReadFile(store.postCompletionRepairProgressPath(planned.Plan.ID))
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("oversized reauthorization changed journal: err=%v", readErr)
	}
}

func TestPostCompletionRepairSealedJSONComparisonIsSemanticAndNumberExact(t *testing.T) {
	if !postCompletionRepairSealedJSONEqual([]byte(`{"a":1,"b":{"c":2}}`), []byte(`{"b":{"c":2},"a":1}`)) {
		t.Fatal("reordered JSON fields should compare equal")
	}
	if postCompletionRepairSealedJSONEqual([]byte(`{"value":9007199254740992}`), []byte(`{"value":9007199254740993}`)) {
		t.Fatal("distinct large JSON integers compared equal")
	}
	if postCompletionRepairSealedJSONEqual([]byte(`{"a":1}`), []byte(`{"a":1} {"trailing":true}`)) {
		t.Fatal("trailing JSON value compared equal")
	}
}

func TestPostCompletionRepairResumesRootWrittenBeforeAnchor(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	previousAnchor, err := os.ReadFile(store.postCompletionRepairAnchorPath())
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, planned)
	if err := os.WriteFile(store.postCompletionRepairAnchorPath(), previousAnchor, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyPostCompletionRepair(planned.PlanPath, authorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("resume root-before-anchor commit: %v", err)
	}
	if !result.Changed || result.Replayed {
		t.Fatalf("root-before-anchor resume result = %+v", result)
	}
	root, _, exists, err := store.loadPostCompletionRepairRoot()
	if err != nil || !exists {
		t.Fatalf("load resumed root: exists=%v err=%v", exists, err)
	}
	anchor, anchorExists, err := store.loadPostCompletionRepairAnchor()
	if err != nil || !anchorExists || verifyPostCompletionRepairRootAnchor(root, true, anchor, true) != nil {
		t.Fatalf("root/anchor did not converge: root=%+v anchor=%+v err=%v", root, anchor, err)
	}
}

func TestPostCompletionRepairVerifyRejectsIndexedPlanReviewWithoutExecution(t *testing.T) {
	store := postCompletionRepairFixture(t)
	planned, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	_ = repairTestAuthorization(t, store, planned)
	for _, entry := range planned.Plan.Entries {
		if err := os.Remove(filepath.Join(store.rootPath(), entry.FileName)); err != nil {
			t.Fatal(err)
		}
	}

	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Completion.Complete || !reasonsContain(verified.Completion.Reasons, "indexed plan/review has no pending or result") {
		t.Fatalf("incomplete rooted repair history disappeared from verification: %+v", verified.Completion)
	}
}

func TestPostCompletionRepairZeroProgressSupersessionPreservesHistoryAndAllowsBroaderPlan(t *testing.T) {
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	oldAuthorization := repairTestAuthorization(t, store, oldPlan)
	writeLegacyRecord(t, store, "rec_after_old_plan", eventRecord("rec_after_old_plan", schemaForID("rec_after_old_plan"), repairNow.Add(2*time.Minute), "later-source"))
	broaderPlan, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if broaderPlan.Plan.ID == oldPlan.Plan.ID || broaderPlan.Plan.Total != oldPlan.Plan.Total+1 {
		t.Fatalf("broader plan = %+v old=%+v", broaderPlan.Plan, oldPlan.Plan)
	}

	type zeroProgressSuperseder interface {
		SupersedePostCompletionRepair(string, string, PostCompletionRepairAuthorization, time.Time, func() error) error
	}
	superseder, ok := any(store).(zeroProgressSuperseder)
	if !ok {
		t.Fatal("governed zero-progress repair supersession is unavailable")
	}
	authorityChecks := 0
	if err := superseder.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, oldAuthorization, repairNow.Add(4*time.Minute), func() error {
		authorityChecks++
		return nil
	}); err != nil {
		t.Fatalf("supersede zero-progress plan: %v", err)
	}
	if authorityChecks != 1 {
		t.Fatalf("supersession authority checks = %d, want 1", authorityChecks)
	}
	if _, err := os.Stat(oldPlan.PlanPath); err != nil {
		t.Fatalf("supersession removed old plan: %v", err)
	}
	if _, err := os.Stat(store.postCompletionRepairReviewPath(oldPlan.Plan.ID)); err != nil {
		t.Fatalf("supersession removed old review: %v", err)
	}
	root, _, exists, err := store.loadPostCompletionRepairRoot()
	if err != nil || !exists || !postCompletionRepairRootOwns(root, "supersession", oldPlan.Plan.ID) {
		t.Fatalf("supersession not rooted: exists=%v root=%+v err=%v", exists, root, err)
	}
	finalizePostCompletionRepairSupersessionForTest(t, store, oldPlan.Plan, oldAuthorization)

	broaderAuthorization := repairTestAuthorization(t, store, broaderPlan)
	result, err := store.ApplyPostCompletionRepair(broaderPlan.PlanPath, broaderAuthorization, authorizedPostCompletionRepairApplyOptions())
	if err != nil {
		t.Fatalf("apply broader plan: %v", err)
	}
	finalizePostCompletionRepairForTest(t, store, broaderPlan.Plan, broaderAuthorization, result)
	verified, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil || !verified.Completion.Complete {
		t.Fatalf("verify after broader repair = %+v err=%v", verified.Completion, err)
	}
}

func TestPostCompletionRepairSupersessionRejectsCausalBackdating(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "before old plan", at: repairNow.Add(-time.Minute), want: "precedes old plan"},
		{name: "before operator review", at: repairNow.Add(30 * time.Second), want: "precedes operator review"},
		{name: "before authorization", at: repairNow.Add(90 * time.Second), want: "precedes authorization"},
		{name: "before successor plan", at: repairNow.Add(7 * time.Minute / 2), want: "precedes successor plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := sealedRepairTestAuthorization(t, store, oldPlan)
			authorization.AuthorizedAt = repairNow.Add(2 * time.Minute).Format(time.RFC3339Nano)
			writeLegacyRecord(t, store, "rec_after_causal_supersession", eventRecord("rec_after_causal_supersession", schemaForID("rec_after_causal_supersession"), repairNow.Add(3*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(4 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, tc.at, authorizedPostCompletionRepairMutation)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("causally backdated supersession was accepted: %v, want %q", err, tc.want)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID); readErr != nil || exists {
				t.Errorf("causally backdated supersession wrote artifact: exists=%v err=%v", exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Errorf("causally backdated supersession changed repair root: err=%v", readErr)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionProofRejectsCausalBackdating(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "before supersession", at: repairNow.Add(4 * time.Minute), want: "precedes supersession"},
		{name: "before receipt", at: repairNow.Add(11 * time.Minute / 2), want: "precedes receipt"},
		{name: "before transition", at: repairNow.Add(13 * time.Minute / 2), want: "precedes transition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := sealedRepairTestAuthorization(t, store, oldPlan)
			authorization.AuthorizedAt = repairNow.Add(2 * time.Minute).Format(time.RFC3339Nano)
			writeLegacyRecord(t, store, "rec_after_causal_proof", eventRecord("rec_after_causal_proof", schemaForID("rec_after_causal_proof"), repairNow.Add(3*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(4 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(5*time.Minute), authorizedPostCompletionRepairMutation); err != nil {
				t.Fatal(err)
			}
			supersession, exists, err := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
			if err != nil || !exists {
				t.Fatalf("read supersession: exists=%v err=%v", exists, err)
			}
			receiptID := "rec_eventstore_repair_causal_supersession"
			transitionID := "tr_eventstore_repair_causal_supersession"
			for _, record := range []map[string]any{
				{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Add(2 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
				{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Add(3 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
				{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": repairNow.Add(6 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision, "eventstore-repair-successor-plan:" + supersession.SuccessorPlanDigest, "eventstore-repair-supersession:" + supersession.ID}},
				{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": repairNow.Add(7 * time.Minute).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
			} {
				id := record["id"].(string)
				if _, err := store.PutJSON(id, record, "  "); err != nil {
					t.Fatalf("seed causal proof %s: %v", id, err)
				}
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.FinalizePostCompletionRepairSupersession(oldPlan.Plan.ID, authorization, receiptID, transitionID, tc.at)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("causally backdated supersession proof was accepted: %v, want %q", err, tc.want)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID); readErr != nil || exists {
				t.Errorf("causally backdated supersession proof wrote artifact: exists=%v err=%v", exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Errorf("causally backdated supersession proof changed repair root: err=%v", readErr)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionProofRejectsBackdatedReceiptAndTransitionBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		receiptDelta    time.Duration
		transitionDelta time.Duration
		want            string
	}{
		{name: "receipt before supersession", receiptDelta: -time.Minute, transitionDelta: time.Minute, want: "receipt created_at precedes supersession"},
		{name: "transition before receipt", receiptDelta: time.Minute, transitionDelta: 30 * time.Second, want: "transition created_at precedes receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := sealedRepairTestAuthorization(t, store, oldPlan)
			authorization.AuthorizedAt = repairNow.Add(2 * time.Minute).Format(time.RFC3339Nano)
			writeLegacyRecord(t, store, "rec_after_terminal_order", eventRecord("rec_after_terminal_order", schemaForID("rec_after_terminal_order"), repairNow.Add(3*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(4 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(5*time.Minute), authorizedPostCompletionRepairMutation); err != nil {
				t.Fatal(err)
			}
			supersession, exists, err := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
			if err != nil || !exists {
				t.Fatalf("read supersession: exists=%v err=%v", exists, err)
			}
			terminalAt, err := time.Parse(time.RFC3339Nano, supersession.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			receiptID := "rec_eventstore_repair_supersession_terminal_order"
			transitionID := "tr_eventstore_repair_supersession_terminal_order"
			for _, record := range []map[string]any{
				{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
				{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
				{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": terminalAt.Add(tc.receiptDelta).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision, "eventstore-repair-successor-plan:" + supersession.SuccessorPlanDigest, "eventstore-repair-supersession:" + supersession.ID}},
				{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": terminalAt.Add(tc.transitionDelta).Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
			} {
				if _, err := store.PutJSON(record["id"].(string), record, "  "); err != nil {
					t.Fatal(err)
				}
			}
			rootBefore, err := os.ReadFile(store.postCompletionRepairRootPath())
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.FinalizePostCompletionRepairSupersession(oldPlan.Plan.ID, authorization, receiptID, transitionID, terminalAt.Add(2*time.Minute))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backdated supersession work chain was accepted: err=%v want=%q", err, tc.want)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID); readErr != nil || exists {
				t.Fatalf("causal refusal wrote supersession proof: exists=%v err=%v", exists, readErr)
			}
			rootAfter, readErr := os.ReadFile(store.postCompletionRepairRootPath())
			if readErr != nil || !bytes.Equal(rootAfter, rootBefore) {
				t.Fatalf("causal refusal changed repair root: %v", readErr)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionProofRequiresSuccessorAndTerminalReceiptRefs(t *testing.T) {
	for _, omitted := range []string{"successor", "terminal"} {
		t.Run("missing "+omitted, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, oldPlan)
			writeLegacyRecord(t, store, "rec_after_weak_supersession_receipt", eventRecord("rec_after_weak_supersession_receipt", schemaForID("rec_after_weak_supersession_receipt"), repairNow.Add(2*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), authorizedPostCompletionRepairMutation); err != nil {
				t.Fatal(err)
			}
			supersession, exists, err := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID)
			if err != nil || !exists {
				t.Fatalf("read supersession: exists=%v err=%v", exists, err)
			}
			successorRef := "eventstore-repair-successor-plan:" + successor.Plan.PlanDigest
			terminalRef := "eventstore-repair-supersession:" + supersession.ID
			proofRefs := []string{
				"eventstore-repair-plan:" + authorization.PlanDigest,
				"eventstore-repair-review:" + authorization.ReviewDigest,
				"policy-authority:" + authorization.AuthorityRevision,
			}
			missingRef := successorRef
			if omitted == "successor" {
				proofRefs = append(proofRefs, terminalRef)
			} else {
				proofRefs = append(proofRefs, successorRef)
				missingRef = terminalRef
			}
			receiptID, transitionID := seedPostCompletionRepairEnvelopeForTest(t, store, authorization, "_weak_"+omitted, mustRepairTime(t, supersession.CreatedAt), proofRefs)
			_, err = store.FinalizePostCompletionRepairSupersession(oldPlan.Plan.ID, authorization, receiptID, transitionID, repairNow.Add(5*time.Minute))
			if err == nil || !strings.Contains(err.Error(), missingRef) {
				t.Fatalf("supersession proof accepted receipt missing %s: %v", missingRef, err)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairSupersessionProof(oldPlan.Plan.ID); readErr != nil || exists {
				t.Fatalf("weak supersession receipt wrote proof: exists=%v err=%v", exists, readErr)
			}
		})
	}
}

func TestPostCompletionRepairSupersessionRetryRootsExactDurableFileBeforeProof(t *testing.T) {
	store, oldPlan, broaderPlan, authorization := postCompletionRepairSupersessionRootGapFixture(t)

	authorityChecks := 0
	if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, authorization, repairNow.Add(5*time.Minute), func() error {
		authorityChecks++
		return nil
	}); err != nil {
		t.Fatalf("resume exact supersession root: %v", err)
	}
	if authorityChecks != 0 {
		t.Fatalf("durable supersession retry reopened current action authority %d time(s)", authorityChecks)
	}
	root, _, exists, err := store.loadPostCompletionRepairRoot()
	if err != nil || !exists || !postCompletionRepairRootOwns(root, "supersession", oldPlan.Plan.ID) {
		t.Fatalf("retry did not root exact durable supersession: exists=%v root=%+v err=%v", exists, root, err)
	}
	proof := finalizePostCompletionRepairSupersessionForTest(t, store, oldPlan.Plan, authorization)
	if proof.SupersessionDigest == "" || proof.ProofDigest == "" {
		t.Fatalf("root-gap retry did not converge proof: %+v", proof)
	}
}

func TestPostCompletionRepairSupersessionRetryRefusesUnmatchedFileAndForeignRoot(t *testing.T) {
	t.Run("unmatched durable file", func(t *testing.T) {
		store, oldPlan, broaderPlan, authorization := postCompletionRepairSupersessionRootGapFixture(t)
		path := store.postCompletionRepairSupersessionPath(oldPlan.Plan.ID)
		supersession, err := loadStrictJSON[PostCompletionRepairSupersession](path)
		if err != nil {
			t.Fatal(err)
		}
		supersession.Authorization.Actor = "codex:foreign-repair"
		supersession.SupersessionDigest, err = postCompletionRepairSupersessionDigest(supersession)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.MarshalIndent(supersession, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBytesAtomic(path, append(body, '\n')); err != nil {
			t.Fatal(err)
		}
		err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, authorization, repairNow.Add(5*time.Minute), func() error { return nil })
		if err == nil || !strings.Contains(err.Error(), "different content") {
			t.Fatalf("unmatched durable supersession accepted: %v", err)
		}
		root, _, _, rootErr := store.loadPostCompletionRepairRoot()
		if rootErr != nil || postCompletionRepairRootOwns(root, "supersession", oldPlan.Plan.ID) {
			t.Fatalf("unmatched durable supersession entered root: root=%+v err=%v", root, rootErr)
		}
	})

	t.Run("foreign rooted identity", func(t *testing.T) {
		store, oldPlan, broaderPlan, authorization := postCompletionRepairSupersessionRootGapFixture(t)
		foreignPath := filepath.Join(store.engine.postCompletionRepairRoot(), "foreign.supersession.json")
		if err := writeBytesAtomic(foreignPath, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		if err := store.ensurePostCompletionRepairArtifact("supersession", oldPlan.Plan.ID, foreignPath); err != nil {
			t.Fatal(err)
		}
		err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, authorization, repairNow.Add(5*time.Minute), func() error { return nil })
		if err == nil || !strings.Contains(err.Error(), "immutable repair artifact drift") {
			t.Fatalf("foreign rooted supersession accepted: %v", err)
		}
	})
}

func postCompletionRepairSupersessionRootGapFixture(t *testing.T) (*Store, PostCompletionRepairPlanRecord, PostCompletionRepairPlanRecord, PostCompletionRepairAuthorization) {
	t.Helper()
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, oldPlan)
	writeLegacyRecord(t, store, "rec_after_supersession_gap", eventRecord("rec_after_supersession_gap", schemaForID("rec_after_supersession_gap"), repairNow.Add(2*time.Minute), "broader-source"))
	broaderPlan, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupt after durable supersession file before root")
	previous := postCompletionRepairSupersessionAfterFile
	postCompletionRepairSupersessionAfterFile = func() error { return injected }
	err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, authorization, repairNow.Add(4*time.Minute), func() error { return nil })
	postCompletionRepairSupersessionAfterFile = previous
	if !errors.Is(err, injected) {
		t.Fatalf("root-gap fault = %v, want %v", err, injected)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID); readErr != nil || !exists {
		t.Fatalf("durable supersession file missing after fault: exists=%v err=%v", exists, readErr)
	}
	root, _, _, rootErr := store.loadPostCompletionRepairRoot()
	if rootErr != nil || postCompletionRepairRootOwns(root, "supersession", oldPlan.Plan.ID) {
		t.Fatalf("fault unexpectedly rooted supersession: root=%+v err=%v", root, rootErr)
	}
	return store, oldPlan, broaderPlan, authorization
}

func TestPostCompletionRepairSupersessionRefusesAfterProgressBoundary(t *testing.T) {
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	oldAuthorization := repairTestAuthorization(t, store, oldPlan)
	if _, err := store.createPostCompletionRepairProgress(oldPlan.Plan, oldAuthorization, repairNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	writeLegacyRecord(t, store, "rec_after_progress", eventRecord("rec_after_progress", schemaForID("rec_after_progress"), repairNow.Add(2*time.Minute), "later-source"))
	broaderPlan, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	type zeroProgressSuperseder interface {
		SupersedePostCompletionRepair(string, string, PostCompletionRepairAuthorization, time.Time, func() error) error
	}
	superseder, ok := any(store).(zeroProgressSuperseder)
	if !ok {
		t.Fatal("governed zero-progress repair supersession is unavailable")
	}
	err = superseder.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, oldAuthorization, repairNow.Add(4*time.Minute), func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "zero progress") {
		t.Fatalf("progressed plan supersession = %v", err)
	}
}

func TestPostCompletionRepairSupersessionRefusesUnownedQuarantineClaimAtZeroProgress(t *testing.T) {
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	authorization := repairTestAuthorization(t, store, oldPlan)
	entry := oldPlan.Plan.Entries[0]
	body, err := os.ReadFile(filepath.Join(store.rootPath(), entry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBytesAtomic(store.postCompletionRepairClaimPath(oldPlan.Plan.ID, entry), body); err != nil {
		t.Fatal(err)
	}
	writeLegacyRecord(t, store, "rec_after_stale_claim", eventRecord("rec_after_stale_claim", schemaForID("rec_after_stale_claim"), repairNow.Add(2*time.Minute), "broader-source"))
	successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	authorityChecks := 0
	err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), func() error {
		authorityChecks++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "zero progress; unowned quarantine entry") {
		t.Fatalf("zero-progress supersession accepted unowned quarantine claim: %v", err)
	}
	if authorityChecks != 0 {
		t.Fatalf("unowned quarantine claim reopened authority %d time(s)", authorityChecks)
	}
	if _, exists, readErr := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID); readErr != nil || exists {
		t.Fatalf("unowned quarantine claim wrote supersession: exists=%v err=%v", exists, readErr)
	}
}

func TestPostCompletionRepairSupersessionRefusesRootedTerminalLifecycleWhenBodiesAreMissing(t *testing.T) {
	for _, tc := range []struct {
		kind string
		id   string
	}{
		{kind: "result", id: "esrr_rooted_terminal"},
		{kind: "proof", id: "sha256:" + strings.Repeat("d", 64)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			store := postCompletionRepairFixture(t)
			oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
			if err != nil {
				t.Fatal(err)
			}
			authorization := repairTestAuthorization(t, store, oldPlan)
			writeLegacyRecord(t, store, "rec_after_rooted_terminal", eventRecord("rec_after_rooted_terminal", schemaForID("rec_after_rooted_terminal"), repairNow.Add(2*time.Minute), "broader-source"))
			successor, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			path := store.postCompletionRepairResultPath(oldPlan.Plan.ID)
			if tc.kind == "proof" {
				path = store.postCompletionRepairProofPath(oldPlan.Plan.ID)
			}
			if err := writeBytesAtomic(path, []byte("{}\n")); err != nil {
				t.Fatal(err)
			}
			if err := store.ensurePostCompletionRepairArtifact(tc.kind, tc.id, path); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			authorityChecks := 0
			err = store.SupersedePostCompletionRepair(oldPlan.PlanPath, successor.PlanPath, authorization, repairNow.Add(4*time.Minute), func() error {
				authorityChecks++
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "zero progress; "+tc.kind+" boundary already exists") {
				t.Fatalf("rooted %s lifecycle with missing body did not refuse supersession: %v", tc.kind, err)
			}
			if authorityChecks != 0 {
				t.Fatalf("rooted %s lifecycle reopened authority %d time(s)", tc.kind, authorityChecks)
			}
			if _, exists, readErr := store.ReadPostCompletionRepairSupersession(oldPlan.Plan.ID); readErr != nil || exists {
				t.Fatalf("rooted %s lifecycle wrote supersession: exists=%v err=%v", tc.kind, exists, readErr)
			}
		})
	}
}

func TestPostCompletionRepairApplyRefusesRootedSupersededPlanBeforeProgressOrMutation(t *testing.T) {
	store := postCompletionRepairFixture(t)
	oldPlan, err := store.CreatePostCompletionRepairPlan(repairNow)
	if err != nil {
		t.Fatal(err)
	}
	oldAuthorization := repairTestAuthorization(t, store, oldPlan)
	writeLegacyRecord(t, store, "rec_after_supersession", eventRecord("rec_after_supersession", schemaForID("rec_after_supersession"), repairNow.Add(2*time.Minute), "broader-source"))
	broaderPlan, err := store.CreatePostCompletionRepairPlan(repairNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SupersedePostCompletionRepair(oldPlan.PlanPath, broaderPlan.PlanPath, oldAuthorization, repairNow.Add(4*time.Minute), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte, len(oldPlan.Plan.Entries))
	for _, entry := range oldPlan.Plan.Entries {
		before[entry.ID], err = os.ReadFile(filepath.Join(store.rootPath(), entry.FileName))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = store.ApplyPostCompletionRepair(oldPlan.PlanPath, oldAuthorization, authorizedPostCompletionRepairApplyOptions())
	if err == nil || !strings.Contains(err.Error(), "rooted superseded plan") {
		t.Fatalf("Apply did not explicitly reject rooted superseded plan: %v", err)
	}
	for _, path := range []string{store.postCompletionRepairPendingPath(oldPlan.Plan.ID), store.postCompletionRepairProgressPath(oldPlan.Plan.ID), store.postCompletionRepairResultPath(oldPlan.Plan.ID), store.postCompletionRepairProofPath(oldPlan.Plan.ID)} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("superseded Apply created progress artifact %s: %v", filepath.Base(path), statErr)
		}
	}
	for _, entry := range oldPlan.Plan.Entries {
		after, readErr := os.ReadFile(filepath.Join(store.rootPath(), entry.FileName))
		if readErr != nil || !bytes.Equal(after, before[entry.ID]) {
			t.Fatalf("superseded Apply mutated source %s: %v", entry.ID, readErr)
		}
	}
}

func seedReservedTornRepairFrame(t *testing.T, store *Store, plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, suffix []byte) []byte {
	t.Helper()
	state, err := store.createPostCompletionRepairProgress(plan, authorization, repairNow)
	if err != nil {
		t.Fatal(err)
	}
	index := -1
	var entry PostCompletionRepairEntry
	for i, candidate := range plan.Entries {
		if candidate.ID == "rec_absent" {
			index, entry = i, candidate
			break
		}
	}
	if index < 0 {
		t.Fatal("rec_absent missing from repair plan")
	}
	raw, err := store.postCompletionRepairSourceBytes(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writePostCompletionRepairArchive(entry, raw); err != nil {
		t.Fatal(err)
	}
	segment, offset, err := store.reservePostCompletionRepairFrame(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.advancePostCompletionRepairProgress(&state, plan, index, repairPhaseAppendReserved, segment, offset, entry.CanonicalSHA256); err != nil {
		t.Fatal(err)
	}
	frame, _, err := buildFrame(entry.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, suffix...)
}

func repairProgressForID(t *testing.T, store *Store, planID, id string) PostCompletionRepairProgressEntry {
	t.Helper()
	pending, exists, err := store.ReadPostCompletionRepairPending(planID)
	if err != nil || !exists {
		t.Fatalf("read repair pending: exists=%v err=%v", exists, err)
	}
	for _, entry := range pending.Entries {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("progress %s missing", id)
	return PostCompletionRepairProgressEntry{}
}

func appendRepairTestBytes(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func repairTestPhaseTransitionLine(t *testing.T, state postCompletionRepairProgressState, plan PostCompletionRepairPlan, index int, phase string) []byte {
	t.Helper()
	transition := PostCompletionRepairProgressTransition{
		SchemaVersion: postCompletionRepairTransitionSchema, Sequence: state.LastSequence + 1,
		PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: index,
		EntryID: plan.Entries[index].ID, FromPhase: state.Current.Entries[index].Phase, ToPhase: phase,
		PreviousDigest: state.LastDigest,
	}
	transition.TransitionDigest, _ = postCompletionRepairTransitionDigest(transition)
	line, err := json.Marshal(transition)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func repairTestReauthorizationTransitionLine(t *testing.T, state postCompletionRepairProgressState, plan PostCompletionRepairPlan, next PostCompletionRepairAuthorization) []byte {
	t.Helper()
	previousDigest, err := digestRepairValue(state.Current.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	transition := PostCompletionRepairProgressTransition{
		SchemaVersion: postCompletionRepairTransitionSchema, Kind: "reauthorize", Sequence: state.LastSequence + 1,
		PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: -1,
		PreviousDigest: state.LastDigest, PreviousAuthorizationDigest: previousDigest, Authorization: &next,
	}
	transition.TransitionDigest, _ = postCompletionRepairTransitionDigest(transition)
	line, err := json.Marshal(transition)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// postCompletionRepairFixture is called 70 times across this file. Each call
// used to run its own real, fsync-bearing Migrate(ModeApply) in a fresh
// t.TempDir() — the dominant cost of the package outside the dedicated scale
// test. Instead, the canonical post-migration tree (two loose records seeded,
// migrated, then re-diverged into the identical/divergent/absent shapes every
// repair test exercises) is built exactly ONCE per test binary run
// (buildPostCompletionRepairFixtureSource, guarded by sync.Once below), and
// every caller gets a private copy via a plain, non-syncing recursive file
// copy (copyFixtureTree) of already-durable bytes — no fsync is needed to
// duplicate bytes that were already made durable once.
//
// This sharing is safe only because no caller here depends on the fixture
// tree's OWN identity (inode, mtime, hardlinks) or mutates the shared
// canonical source directory: every caller receives its own store.home under
// a fresh t.TempDir() and every subsequent mutation (symlink swaps, atomic
// replacements, custody checks, etc.) lands on that private copy, never on
// postCompletionRepairFixtureRoot. Verified against this file: the only
// mtime-sensitive comparison in the engine (engine.legacyStreamRegrew,
// migrate.go) reads the completion marker's mtime as a lower bound against
// the hook_events/traces compatibility streams — streams this fixture never
// creates. A stream written after the copy is active even if its timestamp
// equals the marker's: equality cannot prove it was frozen. The segments are always
// "today" relative to the fixed migrateNow clock, so they are never sealed
// and never indexed (no persisted .idx sidecar exists in the tree), which
// rules out the other mtime hazard: a stale ModifiedUnixNano baked into a
// copied index file. If a future caller needs real file identity from the
// fixture itself, it must not use this helper.
func postCompletionRepairFixture(t *testing.T) *Store {
	t.Helper()
	src := postCompletionRepairFixtureSource(t)
	dst := t.TempDir()
	if err := copyFixtureTree(src, dst); err != nil {
		t.Fatalf("copy post-completion repair fixture tree: %v", err)
	}
	store, err := openWithConfig(dst, engineConfig{
		clock: func() time.Time { return migrateNow },
		probe: func(string) (string, bool, error) { return "apfs", true, nil },
	})
	if err != nil {
		t.Fatalf("open copied post-completion repair fixture: %v", err)
	}
	return store
}

var (
	postCompletionRepairFixtureOnce sync.Once
	postCompletionRepairFixtureRoot string
	postCompletionRepairFixtureErr  error
)

// postCompletionRepairFixtureSource returns the canonical fixture tree's root,
// building it on the first call and reusing it for the rest of the test
// binary run.
func postCompletionRepairFixtureSource(t *testing.T) string {
	t.Helper()
	postCompletionRepairFixtureOnce.Do(func() {
		postCompletionRepairFixtureRoot, postCompletionRepairFixtureErr = buildPostCompletionRepairFixtureSource()
	})
	if postCompletionRepairFixtureErr != nil {
		t.Fatalf("build canonical post-completion repair fixture: %v", postCompletionRepairFixtureErr)
	}
	return postCompletionRepairFixtureRoot
}

// cleanupPostCompletionRepairFixtureSource removes the canonical fixture
// tree, if one was ever built. Called from TestMain after m.Run() so the one
// directory this file creates outside any t.TempDir() does not leak.
func cleanupPostCompletionRepairFixtureSource() {
	if postCompletionRepairFixtureRoot != "" {
		_ = os.RemoveAll(postCompletionRepairFixtureRoot)
	}
}

// buildPostCompletionRepairFixtureSource performs the one real, fsync-bearing
// migration that seeds the canonical fixture tree, reproducing exactly what
// postCompletionRepairFixture used to do inline on every call. It returns
// errors instead of calling t.Fatal on purpose: it runs inside a sync.Once,
// and t.Fatal calls runtime.Goexit, which would still leave the Once marked
// done (Go stores "done" via a deferred call wrapping the invocation) — so a
// failure here would silently wedge every later caller onto a half-built
// tree instead of failing loudly. Surfacing the error lets every caller's own
// t.Fatal report it.
func buildPostCompletionRepairFixtureSource() (string, error) {
	dir, err := os.MkdirTemp("", "eventstore-repair-fixture-src-*")
	if err != nil {
		return "", err
	}
	store, err := openWithConfig(dir, engineConfig{
		clock: func() time.Time { return migrateNow },
		probe: func(string) (string, bool, error) { return "apfs", true, nil },
	})
	if err != nil {
		return "", err
	}
	if err := writeLegacyRecordFile(store, "rec_identical", eventRecord("rec_identical", schemaForID("rec_identical"), migrateNow, "same")); err != nil {
		return "", err
	}
	if err := writeLegacyRecordFile(store, "trace_divergent", eventRecord("trace_divergent", schemaForID("trace_divergent"), migrateNow, "canonical")); err != nil {
		return "", err
	}
	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		return "", err
	}
	if !report.Completion.Complete {
		return "", fmt.Errorf("fixture initial migration incomplete: %+v", report.Completion)
	}
	rec, err := store.Read("rec_identical")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(store.rootPath(), "rec_identical.json"), rec.Bytes, 0o644); err != nil {
		return "", err
	}
	if err := writeLegacyRecordFile(store, "trace_divergent", eventRecord("trace_divergent", schemaForID("trace_divergent"), repairNow, "loose-divergent")); err != nil {
		return "", err
	}
	if err := writeLegacyRecordFile(store, "rec_absent", eventRecord("rec_absent", schemaForID("rec_absent"), repairNow, "new-loose")); err != nil {
		return "", err
	}
	return dir, nil
}

// writeLegacyRecordFile mirrors writeLegacyRecord (scan_test.go) without a
// *testing.T, for use from buildPostCompletionRepairFixtureSource, which runs
// inside a sync.Once and cannot safely call t.Fatal (see the comment there).
func writeLegacyRecordFile(store *Store, id string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.rootPath(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(store.rootPath(), id+".json"), append(data, '\n'), 0o644)
}

// copyFixtureTree recursively copies src to dst with plain reads and writes —
// deliberately no fsync. The source tree was already made durable once by
// the real migration in buildPostCompletionRepairFixtureSource; duplicating
// its bytes into a fresh, disposable t.TempDir() carries no durability
// obligation of its own; a crash mid-copy just loses that one test's
// TempDir, exactly as it always could.
func copyFixtureTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func repairTestAuthorization(t *testing.T, store *Store, planned PostCompletionRepairPlanRecord) PostCompletionRepairAuthorization {
	t.Helper()
	plan := planned.Plan
	reviewed, err := store.ReviewPostCompletionRepair(planned.PlanPath, "operator:test", "authorize", true, repairNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("review repair: %v", err)
	}
	return PostCompletionRepairAuthorization{
		WorkID: "work_eventstore_repair", Actor: "codex:eventstore-repair",
		ActionIntentID: "act_eventstore_repair", PolicyDecisionID: "pol_eventstore_repair",
		AuthorityRevision: "sha256:" + strings.Repeat("a", 64), PlanDigest: plan.PlanDigest,
		ReviewID: reviewed.Review.ID, ReviewDigest: reviewed.Review.ReviewDigest, WriterFenceDigest: reviewed.Review.WriterFenceDigest,
	}
}

func sealedRepairTestAuthorization(t *testing.T, store *Store, planned PostCompletionRepairPlanRecord) PostCompletionRepairAuthorization {
	t.Helper()
	authorization := repairTestAuthorization(t, store, planned)
	authorization.AuthorizedAt = repairNow.Format(time.RFC3339Nano)
	authorization.SealedAction = mustRepairTestJSON(t, map[string]any{
		"schema_version": "compound.action_intent.v0.1",
		"id":             authorization.ActionIntentID,
		"ids":            map[string]any{"work_id": authorization.WorkID},
		"target":         map[string]any{"kind": "event_store", "id": planned.Plan.ID},
	})
	authorization.SealedDecision = mustRepairTestJSON(t, map[string]any{
		"schema_version":   "compound.policy_decision.v0.1",
		"id":               authorization.PolicyDecisionID,
		"ids":              map[string]any{"work_id": authorization.WorkID},
		"action_intent_id": authorization.ActionIntentID,
	})
	return authorization
}

func nextSealedRepairTestAuthorization(t *testing.T, previous PostCompletionRepairAuthorization, suffix string, at time.Time, padding int) PostCompletionRepairAuthorization {
	t.Helper()
	next := previous
	next.PolicyDecisionID = "pol_eventstore_repair_" + suffix
	next.AuthorityRevision = "sha256:" + strings.Repeat(string('b'+rune(len(suffix)%20)), 64)
	next.AuthorizedAt = at.UTC().Format(time.RFC3339Nano)
	next.SealedDecision = mustRepairTestJSON(t, map[string]any{
		"schema_version":   "compound.policy_decision.v0.1",
		"id":               next.PolicyDecisionID,
		"ids":              map[string]any{"work_id": next.WorkID},
		"action_intent_id": next.ActionIntentID,
		"padding":          strings.Repeat("x", padding),
	})
	return next
}

func mustRepairTestJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustRepairTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func replacePostCompletionRepairFileWithExactSymlink(t *testing.T, path string) (string, []byte) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, path); err != nil {
		t.Fatal(err)
	}
	return targetPath, original
}

func postCompletionRepairCustodyError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "symbolic") || strings.Contains(message, "symlink") || strings.Contains(message, "regular file") || strings.Contains(message, "must be a directory") || strings.Contains(message, "stable beneath")
}

func postCompletionRepairReasonsContainCustodyError(reasons []string) bool {
	return reasonsContain(reasons, "symbolic") || reasonsContain(reasons, "symlink") || reasonsContain(reasons, "regular file") || reasonsContain(reasons, "must be a directory") || reasonsContain(reasons, "stable beneath")
}

func authorizedPostCompletionRepairMutation() error { return nil }

func authorizedPostCompletionRepairApplyOptions() PostCompletionRepairApplyOptions {
	return PostCompletionRepairApplyOptions{
		ValidateAuthority: authorizedPostCompletionRepairMutation,
		resultNow:         func() time.Time { return repairNow.Add(2 * time.Minute) },
	}
}

func repairResultEntry(result PostCompletionRepairResult, id string) PostCompletionRepairResultEntry {
	for _, entry := range result.Entries {
		if entry.ID == id {
			return entry
		}
	}
	return PostCompletionRepairResultEntry{}
}

func finalizePostCompletionRepairForTest(t *testing.T, store *Store, plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, result PostCompletionRepairResult) PostCompletionRepairProof {
	t.Helper()
	resultAt, err := time.Parse(time.RFC3339Nano, result.CompletedAt)
	if err != nil {
		t.Fatal(err)
	}
	receiptAt := resultAt.Add(time.Minute)
	transitionAt := resultAt.Add(2 * time.Minute)
	receiptID := "rec_eventstore_repair"
	transitionID := "tr_eventstore_repair"
	for _, record := range []map[string]any{
		{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
		{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
		{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": receiptAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision}},
		{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": transitionAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
	} {
		id := record["id"].(string)
		if _, err := store.PutJSON(id, record, "  "); err != nil {
			t.Fatalf("seed proof %s: %v", id, err)
		}
	}
	proof, err := store.FinalizePostCompletionRepair(plan.ID, authorization, receiptID, transitionID, transitionAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("finalize repair proof: %v", err)
	}
	if proof.PlanID != plan.ID || proof.ResultDigest != result.ResultDigest || proof.Authorization.AuthorityRevision != authorization.AuthorityRevision || proof.ReceiptID != receiptID || proof.TransitionID != transitionID {
		t.Fatalf("repair proof = %+v", proof)
	}
	return proof
}

func finalizePostCompletionRepairSupersessionForTest(t *testing.T, store *Store, plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization) PostCompletionRepairSupersessionProof {
	t.Helper()
	supersession, exists, err := store.ReadPostCompletionRepairSupersession(plan.ID)
	if err != nil || !exists {
		t.Fatalf("read repair supersession before proof: exists=%v err=%v", exists, err)
	}
	supersessionAt, err := time.Parse(time.RFC3339Nano, supersession.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	receiptAt := supersessionAt.Add(time.Minute)
	transitionAt := supersessionAt.Add(2 * time.Minute)
	receiptID := "rec_eventstore_repair_supersession"
	transitionID := "tr_eventstore_repair_supersession"
	for _, record := range []map[string]any{
		{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
		{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
		{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": receiptAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision, "eventstore-repair-successor-plan:" + supersession.SuccessorPlanDigest, "eventstore-repair-supersession:" + supersession.ID}},
		{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": transitionAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
	} {
		id := record["id"].(string)
		if _, err := store.PutJSON(id, record, "  "); err != nil {
			t.Fatalf("seed supersession proof %s: %v", id, err)
		}
	}
	proof, err := store.FinalizePostCompletionRepairSupersession(plan.ID, authorization, receiptID, transitionID, transitionAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("finalize repair supersession proof: %v", err)
	}
	if proof.PlanID != plan.ID || proof.Authorization.AuthorityRevision != authorization.AuthorityRevision || proof.ReceiptID != receiptID || proof.TransitionID != transitionID {
		t.Fatalf("repair supersession proof = %+v", proof)
	}
	return proof
}

func seedPostCompletionRepairEnvelopeForTest(t *testing.T, store *Store, authorization PostCompletionRepairAuthorization, suffix string, terminalAt time.Time, proofRefs []string) (string, string) {
	t.Helper()
	receiptID := "rec_eventstore_repair" + suffix
	transitionID := "tr_eventstore_repair" + suffix
	for _, record := range []map[string]any{
		{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
		{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
		{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": terminalAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": proofRefs},
		{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": terminalAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
	} {
		id := record["id"].(string)
		if _, err := store.PutJSON(id, record, "  "); err != nil {
			t.Fatalf("seed repair envelope %s: %v", id, err)
		}
	}
	return receiptID, transitionID
}

func seedPostCompletionRepairProofWithoutCleanup(t *testing.T, store *Store, plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, result PostCompletionRepairResult) (PostCompletionRepairProof, string, string) {
	t.Helper()
	resultAt := mustRepairTime(t, result.CompletedAt)
	receiptID := "rec_eventstore_repair"
	transitionID := "tr_eventstore_repair"
	for _, record := range []map[string]any{
		{"schema_version": "compound.action_intent.v0.1", "id": authorization.ActionIntentID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}},
		{"schema_version": "compound.policy_decision.v0.1", "id": authorization.PolicyDecisionID, "created_at": repairNow.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_intent_id": authorization.ActionIntentID},
		{"schema_version": "compound.receipt.v0.1", "id": receiptID, "created_at": resultAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "proof_refs": []string{"eventstore-repair-plan:" + authorization.PlanDigest, "eventstore-repair-review:" + authorization.ReviewDigest, "policy-authority:" + authorization.AuthorityRevision}},
		{"schema_version": "compound.transition.v0.1", "id": transitionID, "created_at": resultAt.Format(time.RFC3339Nano), "ids": map[string]string{"work_id": authorization.WorkID}, "action_refs": []string{authorization.ActionIntentID}, "after_refs": []string{receiptID}},
	} {
		id := record["id"].(string)
		if _, err := store.PutJSON(id, record, "  "); err != nil {
			t.Fatalf("seed proof %s: %v", id, err)
		}
	}
	proof := PostCompletionRepairProof{
		SchemaVersion: postCompletionRepairProofSchema,
		PlanID:        plan.ID, PlanDigest: plan.PlanDigest,
		ResultID: result.ID, ResultDigest: result.ResultDigest,
		Authorization: authorization, ReceiptID: receiptID, TransitionID: transitionID,
		RecordedAt: resultAt.Format(time.RFC3339Nano),
	}
	var err error
	proof.ProofDigest, err = postCompletionRepairProofDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	proofPath := store.postCompletionRepairProofPath(plan.ID)
	if err := store.writePostCompletionRepairExclusiveOrEqualJSON(proofPath, "post-completion repair proof", proof); err != nil {
		t.Fatal(err)
	}
	if err := store.ensurePostCompletionRepairArtifact("proof", proof.ProofDigest, proofPath); err != nil {
		t.Fatal(err)
	}
	return proof, receiptID, transitionID
}

// TestPostCompletionRepairRestorePropagatesSymlinkFailureAndPreservesClaim is the
// falsifier for the restore error path. restorePostCompletionRepairClaim once declared
// a block-scoped err inside the symlink branch, so a failed os.Symlink was never seen
// by the outer check: the restore reported success, synced the directory, and removed
// the quarantined claim — possibly the only surviving copy of the loose bytes. The
// failure is forced by pre-creating the restore destination (EEXIST), which is
// deterministic on every platform and UID, unlike a read-only directory.
func TestPostCompletionRepairRestorePropagatesSymlinkFailureAndPreservesClaim(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("loose bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(dir, "claim.json")
	if err := os.Symlink(target, claimPath); err != nil {
		t.Fatal(err)
	}
	// The drift case this path defends against: the loose pathname was recreated after
	// the atomic claim, so re-creating the symlink must fail with EEXIST.
	sourcePath := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(sourcePath, []byte("recreated"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := restorePostCompletionRepairClaim(claimPath, sourcePath)
	if err == nil {
		t.Fatal("restore succeeded despite the symlink destination already existing")
	}
	if !strings.Contains(err.Error(), "restore refused post-completion repair claim") {
		t.Errorf("error = %v, want the refused-restore wrapper", err)
	}
	if _, lerr := os.Lstat(claimPath); lerr != nil {
		t.Fatalf("quarantined claim destroyed by a failed restore: %v", lerr)
	}
}
