package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func snapshotCloseAuthorityFiles(t *testing.T, home string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = body
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot close authority files: %v", err)
	}
	return out
}

// Every RunAttempt in the final union (the ids already on the envelope plus
// CloseInput.RunAttemptIDs) is terminal proof. Invalid evidence must fail
// before any close-owned file changes.
func TestCloseFinalRunAttemptUnionFailsClosedBeforeFirstByte(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed func(t *testing.T, s *Store, workID string) string
	}{
		{
			name: "missing",
			seed: func(t *testing.T, _ *Store, _ string) string {
				t.Helper()
				return runattempt.MintDeterministic("missing-close-proof", fixedNow.Add(41*time.Second))
			},
		},
		{
			name: "malformed_json",
			seed: func(t *testing.T, s *Store, workID string) string {
				t.Helper()
				ra, err := runattempt.Open(s.home).Mint(workID, "codex", runattempt.HarnessPlaceholder, fixedNow.Add(42*time.Second))
				if err != nil {
					t.Fatalf("mint malformed target: %v", err)
				}
				path := filepath.Join(s.home, "run_attempts", ra.ID+".json")
				if err := os.WriteFile(path, []byte("{not-json\n"), 0o644); err != nil {
					t.Fatalf("plant malformed RunAttempt: %v", err)
				}
				return ra.ID
			},
		},
		{
			name: "unknown_schema",
			seed: func(t *testing.T, s *Store, workID string) string {
				t.Helper()
				ra, err := runattempt.Open(s.home).Mint(workID, "codex", runattempt.HarnessPlaceholder, fixedNow.Add(43*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				ra.SchemaVersion = "compound.runattempt.v9.9"
				body, err := json.Marshal(ra)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.home, "run_attempts", ra.ID+".json"), body, 0o644); err != nil {
					t.Fatal(err)
				}
				return ra.ID
			},
		},
		{
			name: "unreadable_body",
			seed: func(t *testing.T, s *Store, workID string) string {
				t.Helper()
				ra, err := runattempt.Open(s.home).Mint(workID, "codex", runattempt.HarnessPlaceholder, fixedNow.Add(44*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(s.home, "run_attempts", ra.ID+".json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				return ra.ID
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-final-union-"+tt.name, fixedNow)
			runAttemptID := tt.seed(t, s, workID)
			before := snapshotCloseAuthorityFiles(t, s.home)
			_, closeErr := s.Close(workID, CloseInput{
				Outcome:       "completed",
				MoveType:      "control_plane_repair",
				Summary:       "invalid RunAttempt evidence must prevent close",
				RunAttemptIDs: []string{runAttemptID},
			}, fixedNow.Add(5*time.Minute))
			if closeErr == nil {
				t.Fatal("close accepted invalid final RunAttempt evidence")
			}
			after := snapshotCloseAuthorityFiles(t, s.home)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused close changed durable authority files: before=%v after=%v", sortedSnapshotKeys(before), sortedSnapshotKeys(after))
			}
		})
	}
}

func sortedSnapshotKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestCloseFinalRunAttemptUnionRejectsForeignAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mint func(*runattempt.Store, string) (runattempt.RunAttempt, error)
	}{
		{
			name: "foreign work",
			mint: func(store *runattempt.Store, _ string) (runattempt.RunAttempt, error) {
				return store.Mint("work_foreign_terminal_final_union", "codex", runattempt.HarnessPlaceholder, fixedNow.Add(20*time.Second))
			},
		},
		{
			name: "foreign action",
			mint: func(store *runattempt.Store, workID string) (runattempt.RunAttempt, error) {
				return store.MintWithActionIntent(workID, "codex", runattempt.HarnessPlaceholder, "act_foreign_generation", fixedNow.Add(21*time.Second))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-final-union-foreign-"+tc.name, fixedNow)
			ra, err := tc.mint(runattempt.Open(s.home), workID)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotCloseAuthorityFiles(t, s.home)
			if _, err := s.Close(workID, CloseInput{
				Outcome: "completed", MoveType: "control_plane_repair",
				Summary:       "foreign authority must not enter close proof",
				RunAttemptIDs: []string{ra.ID},
			}, fixedNow.Add(5*time.Minute)); err == nil {
				t.Fatal("close accepted RunAttempt with foreign authority")
			}
			if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(after, before) {
				t.Fatal("foreign RunAttempt refusal mutated durable authority")
			}
		})
	}
}

func TestCloseReadsLegacyV01RunAttemptAndMigratesPreparedRecord(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-legacy-v01-runattempt", fixedNow)
	legacy, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	legacy.SchemaVersion = runattempt.LegacySchemaVersionV01
	body, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "run_attempts", legacy.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(workID, CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary:   "v0.1 RunAttempt remains readable",
		ProofRefs: []string{"check:legacy-v01-runattempt-close"},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatalf("close accepted legacy v0.1 read-window record: %v", err)
	}
	after, err := runattempt.Open(s.home).Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SchemaVersion != runattempt.SchemaVersion || after.TerminalizationIntent == nil {
		t.Fatalf("close did not migrate prepared v0.1 RunAttempt: %+v", after)
	}
}

func preparedIntentForCloseTest(t *testing.T, s *Store, workID, runAttemptID string, in CloseInput, at time.Time, owner, target, reason string) runattempt.TerminalizationIntent {
	t.Helper()
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	workSHA, err := workEnvelopeSHA256(w)
	if err != nil {
		t.Fatal(err)
	}
	inputSHA, err := canonicalEnvelopeSHA256(in)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClosePlan(workID, at, ClosePlanOptions{Actor: in.Actor, Outcome: in.Outcome, MoveType: in.MoveType, ProofRefs: in.ProofRefs})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(runAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := s.resolveRunAttemptAuthority(w, ra)
	if err != nil {
		t.Fatal(err)
	}
	intent := runattempt.TerminalizationIntent{
		SchemaVersion:             runattempt.TerminalizationIntentSchemaVersion,
		Owner:                     owner,
		WorkID:                    workID,
		RunAttemptActionIntentID:  ra.ActionIntentID,
		AuthorityActionIntentID:   authority.ActionIntentID,
		AuthorityPolicyDecisionID: authority.PolicyDecisionID,
		ExpectedWorkState:         w.State,
		ExpectedWorkSHA256:        workSHA,
		ExpectedActionIntentID:    latestActionIntentID(w),
		ExpectedPolicyDecisionID:  w.PolicyDecisionID,
		CloseInputSHA256:          inputSHA,
		CloseActionIntentID:       plan.ActionIntent.ID,
		ClosePolicyDecisionID:     plan.Decision.ID,
		Outcome:                   in.Outcome,
		TargetStatus:              target,
		TerminalizationReason:     reason,
		ReceiptID:                 idFor("rec_", workID, ts(at)),
		TransitionID:              idFor("tr_", workID, ts(at)),
		PreparedAt:                at.UTC(),
	}
	if ra.ExecutionPosture != nil {
		intent.RunAttemptPolicyDecisionID = ra.ExecutionPosture.PolicyDecisionID
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("test prepared intent invalid: %v", err)
	}
	return intent
}

func TestCloseRejectsConflictingPreparedIntentBeforeLeaseStealOrAnyMutation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-conflict-before-steal", fixedNow)
	closeAt := fixedNow.Add(5 * time.Minute)
	in := CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary: "conflicting prepared intent must fail before lease steal",
		Actor:   "other-agent", Steal: true, StealReason: "test conflict ordering",
	}
	conflict := preparedIntentForCloseTest(t, s, workID, runAttemptIDs[0], in, closeAt,
		runattempt.TerminalizationOwnerPanelApply, runattempt.StatusFailed, "caller failure")
	if _, err := runattempt.Open(s.home).PrepareTerminalization(runAttemptIDs[0], conflict); err != nil {
		t.Fatalf("seed conflicting intent: %v", err)
	}
	before := snapshotCloseAuthorityFiles(t, s.home)
	if _, err := s.Close(workID, in, closeAt); err == nil {
		t.Fatal("close accepted conflicting existing prepared intent")
	}
	after := snapshotCloseAuthorityFiles(t, s.home)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("conflict was detected after a mutation: before=%v after=%v", sortedSnapshotKeys(before), sortedSnapshotKeys(after))
	}
}

func TestCloseRetriesExactPreparedIntentIdempotently(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-exact-prepared-retry", fixedNow)
	closeAt := fixedNow.Add(5 * time.Minute)
	in := CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary:   "exact prepared close retry",
		ProofRefs: []string{"check:exact-prepared-close-retry"},
	}
	exact := preparedIntentForCloseTest(t, s, workID, runAttemptIDs[0], in, closeAt,
		runattempt.TerminalizationOwnerWorkClose, runattempt.StatusSucceeded, "")
	if _, err := runattempt.Open(s.home).PrepareTerminalization(runAttemptIDs[0], exact); err != nil {
		t.Fatalf("seed exact intent: %v", err)
	}
	if _, err := s.Close(workID, in, closeAt.Add(time.Hour)); err != nil {
		t.Fatalf("exact prepared close retry failed: %v", err)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if after.TerminalizationIntent == nil || !reflect.DeepEqual(*after.TerminalizationIntent, exact) || after.Status != runattempt.StatusSucceeded {
		t.Fatalf("exact retry changed reservation or failed to complete it: %+v", after)
	}
}
