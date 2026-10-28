// S2 RunAttempt tests -- the eight references the reviewer's full shape
// called for (ContextPackRef, Workspace, Command, Heartbeat, TranscriptRef,
// DiffRef, TestRef, ProofRefs) plus the hash-neutrality guard that pins
// pre-S2 records marshal byte-identical after the field additions.
// Kept in a separate file from S1 tests so the S1 file stays unchanged
// after the S2 fields land (S1 was deliberately minimal).

package runattempt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRunAttemptHashNeutralityForPreS2Records pins the contract that adding
// the S2 fields does not invalidate pre-existing ratt_* records on disk. A
// record written by S1 (no S2 fields) re-decoded and re-marshaled must
// produce byte-identical bytes. Pinned alongside the work-side
// TestEmptyRunAttemptIDsIsHashNeutral so future field additions face the
// same discipline.
func TestRunAttemptHashNeutralityForPreS2Records(t *testing.T) {
	// attributionKeys are the S3 fields added in the runattempt-attribution
	// slice. None may emit when unset -- that is the hash-neutrality contract
	// for every field addition (pinned alongside TestEmptyRunAttemptIDsIsHashNeutral).
	// The sweep below fails RED if any new field loses its omitempty tag, which
	// is the teeth proof the byte-identical compare alone cannot give.
	attributionKeys := []string{
		`"lane_id"`,
		`"requested_model_id"`,
		`"requested_provider_route"`,
		`"execution_profile_plan_id"`,
		`"requested_client_harness_id"`,
		`"requested_transport_id"`,
		`"requested_native_effort"`,
		`"model_id"`,
		`"provider_route"`,
		`"tokens_in"`,
		`"tokens_out"`,
		`"execution_posture"`,
		`"launch"`,
	}

	assertNeutral := func(t *testing.T, label string, fixture []byte) {
		t.Helper()
		var ra RunAttempt
		if err := json.Unmarshal(fixture, &ra); err != nil {
			t.Fatal(err)
		}
		out, err := json.MarshalIndent(ra, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(fixture) {
			t.Fatalf("%s re-marshal drifted:\n got: %s\nwant: %s", label, out, fixture)
		}
		for _, key := range attributionKeys {
			if strings.Contains(string(out), key) {
				t.Fatalf("%s emitted unset attribution field %s (omitempty broken):\n%s", label, key, out)
			}
		}
	}

	// Pre-S2 record: a S1-style JSON without any S2 field. last_heartbeat_at
	// is *time.Time (omitempty) so nil is omitted entirely -- the byte order
	// matches the struct field order in runattempt.go.
	preS2JSON := []byte(`{
  "schema_version": "compound.runattempt.v0.1",
  "id": "ratt_20260720T000000Z_deadbeef",
  "work_id": "work_abc",
  "action_intent_id": "act_11112222",
  "actor": "codex",
  "harness": "fake",
  "status": "succeeded",
  "created_at": "2026-07-20T00:00:00Z",
  "completed_at": "2026-07-20T00:01:00Z"
}`)
	// Pre-S3 record: S2 references present, NO S3 attribution fields. Extends
	// the neutrality guard to the attribution additions -- this MUST re-marshal
	// byte-identical AND must not emit any attribution key when those fields
	// are unset. The key-order matches the struct field order in runattempt.go.
	preS3JSON := []byte(`{
  "schema_version": "compound.runattempt.v0.1",
  "id": "ratt_20260720T000000Z_deadbeef",
  "work_id": "work_abc",
  "action_intent_id": "act_11112222",
  "actor": "codex",
  "harness": "fake",
  "status": "succeeded",
  "created_at": "2026-07-20T00:00:00Z",
  "completed_at": "2026-07-20T00:01:00Z",
  "context_pack_ref": "ctx_pack_42",
  "workspace": "/repos/compound",
  "command": "compound panel apply"
}`)
	assertNeutral(t, "pre-S2", preS2JSON)
	assertNeutral(t, "pre-S3", preS3JSON)
}

// Mint -> setter -> Get without loss. Empty defaults on a fresh Mint; set
// the field; re-Get; assert.
func TestRunAttemptRoundTripsNewFields(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		do   func() error
		want func(RunAttempt) bool
	}{
		{"context_pack", func() error { _, e := s.SetContextPack(ra.ID, "ctx_pack_42"); return e },
			func(got RunAttempt) bool { return got.ContextPackRef == "ctx_pack_42" }},
		{"workspace", func() error { _, e := s.SetWorkspace(ra.ID, "/repos/compound"); return e },
			func(got RunAttempt) bool { return got.Workspace == "/repos/compound" }},
		{"command", func() error { _, e := s.SetCommand(ra.ID, "compound panel apply"); return e },
			func(got RunAttempt) bool { return got.Command == "compound panel apply" }},
		{"heartbeat", func() error { _, e := s.Heartbeat(ra.ID, now.Add(30*time.Second)); return e },
			func(got RunAttempt) bool {
				return got.LastHeartbeatAt != nil && got.LastHeartbeatAt.Equal(now.Add(30*time.Second))
			}},
		{"transcript", func() error { _, e := s.SetTranscript(ra.ID, "panel-runs/prun_abc/run_record.json"); return e },
			func(got RunAttempt) bool { return got.TranscriptRef == "panel-runs/prun_abc/run_record.json" }},
		{"diff", func() error { _, e := s.SetDiff(ra.ID, "diff/run_abc.patch"); return e },
			func(got RunAttempt) bool { return got.DiffRef == "diff/run_abc.patch" }},
		{"test", func() error { _, e := s.SetTest(ra.ID, "tests/run_abc.junit.xml"); return e },
			func(got RunAttempt) bool { return got.TestRef == "tests/run_abc.junit.xml" }},
		{"proof_refs", func() error { _, e := s.SetProofRefs(ra.ID, []string{"receipt_abc", "transition_abc"}); return e },
			func(got RunAttempt) bool {
				return len(got.ProofRefs) == 2 && got.ProofRefs[0] == "receipt_abc" && got.ProofRefs[1] == "transition_abc"
			}},
		{"attribution", func() error { _, e := s.SetAttribution(ra.ID, "lane-a:worker", "model-x", "provider-y"); return e },
			func(got RunAttempt) bool {
				return got.LaneID == "lane-a:worker" && got.ModelID == "model-x" && got.ProviderRoute == "provider-y"
			}},
		{"dispatch_intent", func() error {
			_, e := s.SetDispatchIntent(ra.ID, "lane-a:worker", "tuple-x", "provider-requested")
			return e
		},
			func(got RunAttempt) bool {
				return got.LaneID == "lane-a:worker" && got.RequestedModelID == "tuple-x" && got.RequestedProviderRoute == "provider-requested"
			}},
		{"token_usage", func() error { _, e := s.SetTokenUsage(ra.ID, 100, 250); return e },
			func(got RunAttempt) bool { return got.TokensIn == 100 && got.TokensOut == 250 }},
		{"execution_posture", func() error {
			_, e := s.SetExecutionPosture(ra.ID, ExecutionPosture{
				PermissionMode: "plan", PolicyDecisionID: "pol_running",
			})
			return e
		}, func(got RunAttempt) bool {
			return got.ExecutionPosture != nil && got.ExecutionPosture.PermissionMode == "plan" && got.ExecutionPosture.PolicyDecisionID == "pol_running"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(); err != nil {
				t.Fatalf("%s setter: %v", tc.name, err)
			}
			got, err := Open(home).Get(ra.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want(got) {
				t.Fatalf("%s did not round-trip, got: %+v", tc.name, got)
			}
		})
	}
}

// TestRunAttemptSettersRefuseOnTerminal pins the audit invariant: a
// terminalized RunAttempt (succeeded|failed) cannot be retroactively
// rewritten. Every S2 setter must reject a terminal RunAttempt with an
// error so a failed close cannot later sprout fresh evidence.
func TestRunAttemptSettersRefuseOnTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ra.ID, StatusSucceeded, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Heartbeat on a terminal RunAttempt is also refused (it would imply the
	// attempt is still running, contradicting the terminal status).
	setters := []struct {
		name string
		fn   func() error
	}{
		{"context_pack", func() error { _, e := s.SetContextPack(ra.ID, "ctx_x"); return e }},
		{"workspace", func() error { _, e := s.SetWorkspace(ra.ID, "/x"); return e }},
		{"command", func() error { _, e := s.SetCommand(ra.ID, "x"); return e }},
		{"heartbeat", func() error { _, e := s.Heartbeat(ra.ID, now.Add(2*time.Minute)); return e }},
		{"transcript", func() error { _, e := s.SetTranscript(ra.ID, "x"); return e }},
		{"diff", func() error { _, e := s.SetDiff(ra.ID, "x"); return e }},
		{"test", func() error { _, e := s.SetTest(ra.ID, "x"); return e }},
		{"proof_refs", func() error { _, e := s.SetProofRefs(ra.ID, []string{"x"}); return e }},
		{"attribution", func() error { _, e := s.SetAttribution(ra.ID, "lane-x", "model-y", "provider-z"); return e }},
		{"dispatch_intent", func() error { _, e := s.SetDispatchIntent(ra.ID, "lane-x", "tuple-y", "provider-z"); return e }},
		{"token_usage", func() error { _, e := s.SetTokenUsage(ra.ID, 100, 250); return e }},
		{"execution_posture", func() error {
			_, e := s.SetExecutionPosture(ra.ID, ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_running"})
			return e
		}},
	}
	for _, tc := range setters {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatalf("setter on terminal RunAttempt must fail")
			}
		})
	}
}

// TestRunAttemptSetProofRefsClearsOnEmpty proves SetProofRefs("") clears
// existing refs rather than no-op-ing. Useful when a harness attaches the
// wrong refs by mistake and wants to retract (before terminal only).
func TestRunAttemptSetProofRefsClearsOnEmpty(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProofRefs(ra.ID, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProofRefs(ra.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ProofRefs) != 0 {
		t.Fatalf("SetProofRefs(nil) must clear refs, got %v", got.ProofRefs)
	}
}

// TestRunAttemptHeartbeatAdvancesOnlyForward pins heartbeat semantics:
// each Heartbeat must advance LastHeartbeatAt; a stale heartbeat (now <=
// current) is a no-op. The audit invariant is that heartbeats reflect the
// latest activity, not the earliest call.
func TestRunAttemptHeartbeatAdvancesOnlyForward(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_abc", "compound", HarnessPlaceholder, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ra.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Stale heartbeat: must NOT move LastHeartbeatAt backwards.
	if _, err := s.Heartbeat(ra.ID, t0.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("stale heartbeat regressed timestamp: got %v, want %v", got.LastHeartbeatAt, t0.Add(time.Minute))
	}
	// Forward heartbeat: must advance.
	if _, err := s.Heartbeat(ra.ID, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ra.ID)
	if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("forward heartbeat did not advance: got %v, want %v", got.LastHeartbeatAt, t0.Add(2*time.Minute))
	}
}

// TestRunAttemptProgressRejectsRegressionWithoutMutation pins the dashboard
// contract for out-of-order workers: completed buckets never move backward,
// remaining never moves upward, and a rejected update cannot partially move
// either the progress snapshot or its heartbeat.
func TestRunAttemptProgressRejectsRegressionWithoutMutation(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	ra, err := s.Mint("work_progress", "worker", "claude-minimax", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunning(ra.ID, t0); err != nil {
		t.Fatal(err)
	}
	want := RunProgress{Done: 4, Empty: 1, Failed: 1, Remaining: 4}
	acceptedAt := t0.Add(time.Minute)
	if _, err := s.HeartbeatProgress(ra.ID, acceptedAt, want); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		next RunProgress
	}{
		{name: "done-decreases", next: RunProgress{Done: 3, Empty: 1, Failed: 1, Remaining: 4}},
		{name: "empty-decreases", next: RunProgress{Done: 4, Empty: 0, Failed: 1, Remaining: 4}},
		{name: "failed-decreases", next: RunProgress{Done: 4, Empty: 1, Failed: 0, Remaining: 4}},
		{name: "remaining-increases", next: RunProgress{Done: 4, Empty: 1, Failed: 1, Remaining: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.HeartbeatProgress(ra.ID, acceptedAt.Add(time.Minute), tc.next); err == nil {
				t.Fatal("regressing progress was accepted")
			}
			got, err := Open(home).Get(ra.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Progress == nil || *got.Progress != want {
				t.Fatalf("rejected update mutated progress: got=%+v want=%+v", got.Progress, want)
			}
			if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(acceptedAt) {
				t.Fatalf("rejected update mutated heartbeat: got=%v want=%v", got.LastHeartbeatAt, acceptedAt)
			}
		})
	}
}
