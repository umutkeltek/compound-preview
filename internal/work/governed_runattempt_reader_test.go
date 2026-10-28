package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func seedGovernedReaderAttempt(t *testing.T, s *Store, seed ...string) runattempt.RunAttempt {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "reader-test", SuccessPredicate: "read owned execution provenance " + strings.Join(seed, "/")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "reader-test", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: "reader-test"}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(s.home)
	ra, err := attempts.MintWithActionIntent(w.ID, "reader-test", runattempt.HarnessPlaceholder,
		w.ActionIntentIDs[len(w.ActionIntentIDs)-1], fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	ra, err = attempts.SetExecutionPosture(ra.ID, runattempt.ExecutionPosture{
		PermissionMode: "plan", PolicyDecisionID: w.PolicyDecisionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ra
}

// Corruption fixtures deliberately bypass immutable setters in a synthetic home.
func writeGovernedReaderFixture(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGovernedAttemptWithoutWrites(t *testing.T, s *Store, id string, wantError bool) runattempt.RunAttempt {
	t.Helper()
	before := snapshotCloseAuthorityFiles(t, s.home)
	got, err := s.LoadGovernedRunAttempt(id)
	if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(before, after) {
		t.Fatal("reader changed durable authority")
	}
	if (err != nil) != wantError {
		t.Fatalf("read error = %v, want error %v", err, wantError)
	}
	if err != nil && !reflect.DeepEqual(got, runattempt.RunAttempt{}) {
		t.Fatalf("error returned nonzero attempt: %+v", got)
	}
	return got
}

func TestLoadGovernedRunAttemptPreservesProvenanceWithoutAdmission(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"running", runattempt.StatusSucceeded, runattempt.StatusFailed, runattempt.StatusAbandoned, runattempt.StatusSuperseded} {
		t.Run(status, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			if status != "running" {
				var err error
				ra, err = runattempt.Open(s.home).Complete(ra.ID, status, ra.CreatedAt.Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
			}
			w, err := s.Get(ra.WorkID)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(w.RunAttemptIDs, ra.ID) || ra.Launch != nil {
				t.Fatal("fixture must have no forward attachment or committed launch")
			}
			got := readGovernedAttemptWithoutWrites(t, s, ra.ID, false)
			if !reflect.DeepEqual(got, ra) {
				t.Fatalf("reader changed provenance: got %+v, want %+v", got, ra)
			}
		})
	}
}

func TestLoadGovernedRunAttemptAdmitsAttachedAndLaunchedProvenance(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"attached", "prepared", "committed", "start_error"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			if kind == "attached" {
				w, err := s.Advance(ra.WorkID, AdvanceInput{
					To: "verifying", Actor: "reader-test", RunAttemptIDs: []string{ra.ID},
				}, ra.CreatedAt.Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(w.RunAttemptIDs, ra.ID) {
					t.Fatal("fixture did not attach attempt")
				}
			} else {
				_, err := runattempt.Open(s.home).CommitLaunch(ra.ID, ra.CreatedAt.Add(time.Second), func() error {
					if kind == "prepared" {
						got := readGovernedAttemptWithoutWrites(t, s, ra.ID, false)
						if got.Launch == nil || got.Launch.PreparedAt == nil || got.Launch.CommittedAt != nil || got.Launch.StartErrorAt != nil {
							t.Fatal("fixture did not expose prepared-only launch")
						}
					}
					if kind == "start_error" {
						return os.ErrPermission
					}
					return nil
				})
				if (err != nil) != (kind == "start_error") {
					t.Fatalf("launch fixture: %v", err)
				}
			}
			got := readGovernedAttemptWithoutWrites(t, s, ra.ID, false)
			if got.ID != ra.ID {
				t.Fatal("reader returned a different attempt")
			}
			if kind == "committed" && (got.Launch == nil || got.Launch.CommittedAt == nil) {
				t.Fatal("fixture did not commit launch")
			}
			if kind == "start_error" && (got.Launch == nil || got.Launch.StartErrorAt == nil) {
				t.Fatal("fixture did not record launch failure")
			}
		})
	}
}

func TestLoadGovernedRunAttemptRejectsInvalidOwnedRecords(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "ratt_bogus", "prun_legacy", "../outside", runattempt.MintDeterministic("missing-reader", fixedNow)} {
		t.Run(id, func(t *testing.T) {
			readGovernedAttemptWithoutWrites(t, newStore(t), id, true)
		})
	}
	for _, kind := range []string{"malformed", "schema", "missing_work", "malformed_work", "launch_sidecar"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			path := filepath.Join(s.home, "run_attempts", ra.ID+".json")
			switch kind {
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "schema":
				ra.SchemaVersion = "compound.runattempt.v9.9"
				writeGovernedReaderFixture(t, path, ra)
			case "missing_work":
				ra.WorkID = "work_missing_reader"
				writeGovernedReaderFixture(t, path, ra)
			case "malformed_work":
				if err := os.WriteFile(s.workPath(ra.WorkID), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "launch_sidecar":
				if _, err := runattempt.Open(s.home).CommitLaunch(ra.ID, ra.CreatedAt.Add(time.Second), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				found := false
				for rel := range snapshotCloseAuthorityFiles(t, s.home) {
					if strings.HasPrefix(filepath.ToSlash(rel), "run_attempts/launch/") && strings.HasSuffix(rel, ".json") {
						if err := os.WriteFile(filepath.Join(s.home, rel), []byte("{"), 0o600); err != nil {
							t.Fatal(err)
						}
						found = true
						break
					}
				}
				if !found {
					t.Fatal("real launch did not publish a sidecar")
				}
			}
			readGovernedAttemptWithoutWrites(t, s, ra.ID, true)
		})
	}
}

func TestLoadGovernedRunAttemptRejectsMissingOrForeignAuthority(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"claim_origin", "manual_origin", "blank_action", "whitespace_action", "nil_posture", "blank_policy", "whitespace_policy", "missing_action", "missing_policy", "foreign_action", "foreign_policy", "corrupt_action", "corrupt_policy"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			switch kind {
			case "claim_origin":
				ra.Origin = runattempt.OriginClaimReservation
			case "manual_origin":
				ra.Origin = ""
			case "blank_action":
				ra.ActionIntentID = ""
			case "whitespace_action":
				ra.ActionIntentID = " "
			case "nil_posture":
				ra.ExecutionPosture = nil
			case "blank_policy":
				ra.ExecutionPosture.PolicyDecisionID = ""
			case "whitespace_policy":
				ra.ExecutionPosture.PolicyDecisionID = " "
			case "missing_action":
				ra.ActionIntentID = "act_missing_reader"
			case "missing_policy":
				ra.ExecutionPosture.PolicyDecisionID = "pol_missing_reader"
			case "corrupt_action", "corrupt_policy":
				id := ra.ActionIntentID
				field := "action"
				if kind == "corrupt_policy" {
					id = ra.ExecutionPosture.PolicyDecisionID
					field = "outcome"
				}
				body, err := s.eventBytes(id)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				// Preserve the event frame's authentic header, but give a
				// typed authority field a value that cannot decode.
				fields[field] = json.RawMessage("[]")
				if err := s.replaceEvent(id, fields); err != nil {
					t.Fatal(err)
				}
			case "foreign_action", "foreign_policy":
				foreign := seedGovernedReaderAttempt(t, s, "foreign")
				if kind == "foreign_action" {
					ra.ActionIntentID = foreign.ActionIntentID
				} else {
					ra.ExecutionPosture.PolicyDecisionID = foreign.ExecutionPosture.PolicyDecisionID
				}
			}
			writeGovernedReaderFixture(t, filepath.Join(s.home, "run_attempts", ra.ID+".json"), ra)
			readGovernedAttemptWithoutWrites(t, s, ra.ID, true)
		})
	}
}

func TestLoadGovernedRunAttemptRejectsReplacedRecordIdentities(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"attempt_identity", "work_schema", "foreign_work_body"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			requestedID := ra.ID
			w, err := s.Get(ra.WorkID)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "attempt_identity":
				ra.ID = runattempt.MintDeterministic("replaced-attempt", fixedNow)
			case "work_schema":
				w.SchemaVersion = "compound.work.v9.9"
				writeGovernedReaderFixture(t, s.workPath(ra.WorkID), w)
			case "foreign_work_body":
				foreign := seedGovernedReaderAttempt(t, s, "foreign")
				foreignWork, err := s.Get(foreign.WorkID)
				if err != nil {
					t.Fatal(err)
				}
				ra.ActionIntentID = foreign.ActionIntentID
				ra.ExecutionPosture = foreign.ExecutionPosture
				writeGovernedReaderFixture(t, s.workPath(ra.WorkID), foreignWork)
			}
			writeGovernedReaderFixture(t, filepath.Join(s.home, "run_attempts", requestedID+".json"), ra)
			readGovernedAttemptWithoutWrites(t, s, requestedID, true)
		})
	}
}

func TestLoadGovernedRunAttemptPreservesExactPolicyRefusalException(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"running", "exact_refusal", "late_completion", "wrong_reason", "heartbeat", "committed_launch"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ra := seedGovernedReaderAttempt(t, s)
			var decision envelope.PolicyDecision
			if err := s.readEventJSON(ra.ExecutionPosture.PolicyDecisionID, &decision); err != nil {
				t.Fatal(err)
			}
			// Keep the authentic work/action/header fields; publish a distinct
			// synthetic denial without overwriting an immutable policy record.
			decision.ID = "pol_reader_denied"
			decision.Outcome = "deny"
			if _, err := s.events.PutJSON(decision.ID, &decision, "  "); err != nil {
				t.Fatal(err)
			}
			if kind == "committed_launch" {
				if _, err := runattempt.Open(s.home).CommitLaunch(ra.ID, ra.CreatedAt.Add(time.Second), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			ra.ExecutionPosture.PolicyDecisionID = decision.ID
			if kind != "running" {
				ra.Status = runattempt.StatusAbandoned
				ra.CompletedAt = ra.CreatedAt
				ra.SweepReason = proofResourcePolicyRefusedBeforeExecution
			}
			switch kind {
			case "late_completion":
				ra.CompletedAt = ra.CreatedAt.Add(time.Second)
			case "wrong_reason":
				ra.SweepReason = "not the exact refusal"
			case "heartbeat":
				at := ra.CreatedAt
				ra.LastHeartbeatAt = &at
			}
			writeGovernedReaderFixture(t, filepath.Join(s.home, "run_attempts", ra.ID+".json"), ra)
			readGovernedAttemptWithoutWrites(t, s, ra.ID, kind != "exact_refusal")
		})
	}
}
