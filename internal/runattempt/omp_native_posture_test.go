package runattempt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ompNativeVerifierHarness = "omp-native-verifier"

const ompPromptOnlyHarness = "omp-prompt-only"

// ompNativeVerifierMarker is the single authority evidence item the
// prompt_only posture admits. The values match the production consumer
// (internal/actuator/omp_native_launch.go).
var ompNativeVerifierMarker = []ExecutionPostureEvidence{
	{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"},
}

// ompPromptOnlyMarker is the second registered authority evidence item
// the prompt_only posture admits. Mirrors ompNativeVerifierMarker for
// the omp-prompt-only carve-out.
var ompPromptOnlyMarker = []ExecutionPostureEvidence{
	{Kind: "omp_prompt_only", Ref: "harness:omp-prompt-only", Revision: "v0.1"},
}

// mintOMPNativeExecutionAttempt mints a harness_execution-flavored RunAttempt
// bound to a nonblank ActionIntentID — the only state shape prompt_only
// admission accepts. Caller-supplied workID keeps the test independent of
// any production fixture.
func mintOMPNativeExecutionAttempt(t *testing.T, store *Store, now time.Time) RunAttempt {
	t.Helper()
	ra, err := store.MintWithActionIntent(
		"work_omp_native_posture", "codex",
		ompNativeVerifierHarness, "act_native_verify", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	return ra
}

// mintOMPPromptOnlyExecutionAttempt is the parallel mint helper for the
// omp-prompt-only harness. It mirrors mintOMPNativeExecutionAttempt
// against a fresh work id so the new pair stays independent of the
// existing fixtures.
func mintOMPPromptOnlyExecutionAttempt(t *testing.T, store *Store, now time.Time) RunAttempt {
	t.Helper()
	ra, err := store.MintWithActionIntent(
		"work_omp_prompt_only_posture", "codex",
		ompPromptOnlyHarness, "act_prompt_only_verify", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	return ra
}

// TestOMPNativePromptOnlyPostureAdmitsAndPersists is the canonical happy path.
// A prompt_only posture with the exact marker, admitted on a fresh
// harness_execution attempt with a bound ActionIntentID, must survive a store
// reopen byte-for-byte. Reopens exercise the disk path (not just cache).
func TestOMPNativePromptOnlyPostureAdmitsAndPersists(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)

	posture := ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	}
	bound, err := store.SetExecutionPosture(ra.ID, posture)
	if err != nil {
		t.Fatalf("admit prompt_only posture: %v", err)
	}
	if bound.ExecutionPosture == nil || bound.ExecutionPosture.PermissionMode != "prompt_only" {
		t.Fatalf("admitted posture = %+v", bound.ExecutionPosture)
	}

	reopened := Open(home)
	reloaded, err := reopened.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExecutionPosture == nil {
		t.Fatal("posture missing after store reopen")
	}
	if reloaded.ExecutionPosture.PermissionMode != "prompt_only" {
		t.Fatalf("reloaded permission_mode = %q, want prompt_only", reloaded.ExecutionPosture.PermissionMode)
	}
	if reloaded.ExecutionPosture.PolicyDecisionID != "pol_native_running" {
		t.Fatalf("reloaded policy_decision_id = %q", reloaded.ExecutionPosture.PolicyDecisionID)
	}
	if len(reloaded.ExecutionPosture.AuthorityEvidence) != 1 {
		t.Fatalf("reloaded authority evidence len = %d, want 1", len(reloaded.ExecutionPosture.AuthorityEvidence))
	}
	marker := reloaded.ExecutionPosture.AuthorityEvidence[0]
	if marker.Kind != "omp_native_verifier" ||
		marker.Ref != "harness:omp-native-verifier" ||
		marker.Revision != "v0.1" {
		t.Fatalf("reloaded marker = %+v", marker)
	}
}

// TestOMPNativePromptOnlyPostureExactBindIsIdempotent pins the
// existing-bind-immutability contract for prompt_only: an exact replay after
// a store reopen must succeed without rewriting the on-disk record.
func TestOMPNativePromptOnlyPostureExactBindIsIdempotent(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)

	posture := ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	}
	if _, err := store.SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatalf("exact prompt_only bind after store reopen must be idempotent: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsWrongHarness pins the
// fresh-record-inside-mutateMutable read for harness identity. A non-OMP
// harness never gets prompt_only, regardless of every other shape being
// correct.
func TestOMPNativePromptOnlyPostureRejectsWrongHarness(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra, err := store.MintWithActionIntent(
		"work_omp_native_posture", "codex",
		"claude_code", "act_native_verify", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	_, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), ompNativeVerifierHarness) {
		t.Fatalf("expected harness refusal naming %q, got: %v", ompNativeVerifierHarness, err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsWrongOrigin pins the fresh-record read
// for origin. A claim_reservation attempt cannot borrow the OMP-native
// marker even when the harness and intent id are correct.
func TestOMPNativePromptOnlyPostureRejectsWrongOrigin(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra, err := store.MintWithActionIntent(
		"work_omp_native_posture", "codex",
		ompNativeVerifierHarness, "act_native_verify", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(ra.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}
	_, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("expected origin refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsBlankActionIntent pins the bound
// ActionIntentID precondition. The plain Mint path leaves the binding
// empty; prompt_only must refuse that shape even when everything else is
// correct.
func TestOMPNativePromptOnlyPostureRejectsBlankActionIntent(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra, err := store.Mint(
		"work_omp_native_posture", "codex",
		ompNativeVerifierHarness, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	_, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "action_intent_id") {
		t.Fatalf("expected action_intent_id refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsMissingMarker pins the
// exactly-one-evidence contract. Zero evidence items are not the marker
// shape; the pre-validation stage must refuse before reading the record.
func TestOMPNativePromptOnlyPostureRejectsMissingMarker(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "omp_native_verifier") {
		t.Fatalf("expected missing-marker refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsWrongMarker pins the
// exactly-the-marker contract. A wrong revision must not pass as the
// OMP-native authority marker, regardless of all other fields being
// correct.
func TestOMPNativePromptOnlyPostureRejectsWrongMarker(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:   "prompt_only",
		PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: []ExecutionPostureEvidence{
			{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.2"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "omp_native_verifier") {
		t.Fatalf("expected marker-shape refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsExtraMarker pins the
// exactly-one-evidence contract. Two markers would let an OMP-native
// posture smuggle in a non-OMP-native authority kind, so it is refused.
func TestOMPNativePromptOnlyPostureRejectsExtraMarker(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:   "prompt_only",
		PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: []ExecutionPostureEvidence{
			{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"},
			{Kind: "permission_authority", Ref: "compound://harness-permissions/claude_code", Revision: "sha256:authority"},
		},
	})
	if err == nil {
		t.Fatal("two evidence items for prompt_only must be refused")
	}
}

// TestOMPNativePromptOnlyPostureRejectsDuplicateMarker pins that two
// identical OMP-native markers do not become one valid pair; the
// duplicate-kind rejection is the same gate every other mode honors.
func TestOMPNativePromptOnlyPostureRejectsDuplicateMarker(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:   "prompt_only",
		PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: []ExecutionPostureEvidence{
			{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"},
			{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate-kind refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRejectsBadPolicyID pins the pol_ prefix
// requirement that every other mode honors. prompt_only must not relax it.
func TestOMPNativePromptOnlyPostureRejectsBadPolicyID(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "not_a_policy_id",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "pol_") {
		t.Fatalf("expected policy_id prefix refusal, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureBindingIsImmutable pins that a second
// prompt_only bind with any different field is refused — the exact-bind
// idempotence rule is byte-identity, not "same mode with different policy".
func TestOMPNativePromptOnlyPostureBindingIsImmutable(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)

	posture := ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	}
	if _, err := store.SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatal(err)
	}

	different := posture
	different.PolicyDecisionID = "pol_native_other"
	_, err := Open(home).SetExecutionPosture(ra.ID, different)
	if err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("expected already-bound refusal for mismatched prompt_only rebind, got: %v", err)
	}
}

// TestOMPNativePromptOnlyPostureRefusesTerminal pins that launch-evidence
// freeze remains authoritative: a terminal RunAttempt cannot adopt a
// prompt_only posture, no matter how correct every other field is.
func TestOMPNativePromptOnlyPostureRefusesTerminal(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	if _, err := store.Complete(ra.ID, StatusAbandoned, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil {
		t.Fatal("prompt_only posture on terminal RunAttempt must fail")
	}
}

// TestOMPNativePromptOnlyPostureRefusesPreparedTerminalization pins that
// the prepared-terminalization freeze (launch-evidence freeze) remains
// authoritative for prompt_only too — once a close intent has been
// installed, any second posture mutation (even the exact same bind) is
// refused. Binding prompt_only first is required because PrepareTerminalization
// validates against the existing ExecutionPosture.
func TestOMPNativePromptOnlyPostureRefusesPreparedTerminalization(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)

	if _, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	}); err != nil {
		t.Fatal(err)
	}

	zero := "sha256:" + strings.Repeat("0", 64)
	if _, err := store.PrepareTerminalization(ra.ID, TerminalizationIntent{
		SchemaVersion:              TerminalizationIntentSchemaVersion,
		Owner:                      TerminalizationOwnerWorkClose,
		WorkID:                     ra.WorkID,
		RunAttemptActionIntentID:   ra.ActionIntentID,
		RunAttemptPolicyDecisionID: "pol_native_running",
		AuthorityActionIntentID:    "act_native_verify",
		AuthorityPolicyDecisionID:  "pol_native_running",
		ExpectedWorkState:          "verifying",
		ExpectedWorkSHA256:         zero,
		CloseInputSHA256:           zero,
		CloseActionIntentID:        "act_native_close",
		ClosePolicyDecisionID:      "pol_native_close",
		Outcome:                    "completed",
		TargetStatus:               StatusSucceeded,
		ReceiptID:                  "rec_close_1",
		TransitionID:               "tr_close_1",
		PreparedAt:                 now,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "terminalization") {
		t.Fatalf("expected prepared-terminalization refusal, got: %v", err)
	}
}

func TestOMPNativePromptOnlyPostureRejectsWhitespaceActionIntent(t *testing.T) {
	store := Open(t.TempDir())
	ra := mintOMPNativeExecutionAttempt(t, store, time.Now())
	loaded, err := store.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	ra = loaded
	// A syntactically valid persisted record can still carry a blank binding.
	// Warm the original Store cache before replacing the fixture to exercise
	// the fresh record read inside the mutation lock.
	ra.ActionIntentID = " \t "
	body, err := json.Marshal(ra)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Home(), "run_attempts", ra.ID+".json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode: "prompt_only", PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "action_intent_id") {
		t.Fatalf("whitespace-only action binding accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, after) {
		t.Fatalf("refusal changed the persisted record: %v", err)
	}
}

func TestOMPNativePromptOnlyPostureRefusesPostLaunchBind(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Now()
	ra := mintOMPNativeExecutionAttempt(t, store, now)
	launched, err := store.CommitLaunch(ra.ID, now.Add(time.Second), func() error { return nil })
	if err != nil || launched.Launch == nil || launched.Launch.CommittedAt == nil {
		t.Fatalf("committed launch fixture: %+v, %v", launched.Launch, err)
	}
	_, err = store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode: "prompt_only", PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("post-launch posture binding accepted: %v", err)
	}
	got, err := Open(store.Home()).Get(ra.ID)
	if err != nil || got.ExecutionPosture != nil || got.Launch == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("refusal changed launch or bound a posture: %+v, %v", got, err)
	}
}

func TestOMPNativePromptOnlyPostureRechecksOriginFromDisk(t *testing.T) {
	store := Open(t.TempDir())
	ra := mintOMPNativeExecutionAttempt(t, store, time.Now())
	if _, err := store.Get(ra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store.Home()).SetOrigin(ra.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}
	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode: "prompt_only", PolicyDecisionID: "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("stale cached origin authorized posture: %v", err)
	}
}

// TestOMPPromptOnlyPostureAdmitsAndPersists is the canonical happy path
// for the second registered pair. A prompt_only posture with the exact
// omp_prompt_only marker, admitted on a fresh harness_execution attempt
// bound to a nonblank ActionIntentID, must survive a store reopen
// byte-for-byte.
func TestOMPPromptOnlyPostureAdmitsAndPersists(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPPromptOnlyExecutionAttempt(t, store, now)

	posture := ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_prompt_only_running",
		AuthorityEvidence: ompPromptOnlyMarker,
	}
	bound, err := store.SetExecutionPosture(ra.ID, posture)
	if err != nil {
		t.Fatalf("admit prompt_only posture: %v", err)
	}
	if bound.ExecutionPosture == nil || bound.ExecutionPosture.PermissionMode != "prompt_only" {
		t.Fatalf("admitted posture = %+v", bound.ExecutionPosture)
	}

	reopened := Open(home)
	reloaded, err := reopened.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExecutionPosture == nil {
		t.Fatal("posture missing after store reopen")
	}
	if reloaded.ExecutionPosture.PermissionMode != "prompt_only" {
		t.Fatalf("reloaded permission_mode = %q, want prompt_only", reloaded.ExecutionPosture.PermissionMode)
	}
	if reloaded.ExecutionPosture.PolicyDecisionID != "pol_prompt_only_running" {
		t.Fatalf("reloaded policy_decision_id = %q", reloaded.ExecutionPosture.PolicyDecisionID)
	}
	if len(reloaded.ExecutionPosture.AuthorityEvidence) != 1 {
		t.Fatalf("reloaded authority evidence len = %d, want 1", len(reloaded.ExecutionPosture.AuthorityEvidence))
	}
	marker := reloaded.ExecutionPosture.AuthorityEvidence[0]
	if marker.Kind != "omp_prompt_only" ||
		marker.Ref != "harness:omp-prompt-only" ||
		marker.Revision != "v0.1" {
		t.Fatalf("reloaded marker = %+v", marker)
	}
}

// TestOMPPromptOnlyPostureExactBindIsIdempotent pins the
// existing-bind-immutability contract for the second pair: an exact
// replay after a store reopen must succeed without rewriting the on-disk
// record.
func TestOMPPromptOnlyPostureExactBindIsIdempotent(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPPromptOnlyExecutionAttempt(t, store, now)

	posture := ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_prompt_only_running",
		AuthorityEvidence: ompPromptOnlyMarker,
	}
	if _, err := store.SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatalf("exact prompt_only bind after store reopen must be idempotent: %v", err)
	}
}

// TestOMPPromptOnlyPostureRejectsCrossPairFromNative pins the cross-pair
// gate: a RunAttempt minted on the omp-native-verifier harness cannot
// bind the omp_prompt_only marker. Both pairs are individually valid,
// but they must not be mixed.
func TestOMPPromptOnlyPostureRejectsCrossPairFromNative(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPNativeExecutionAttempt(t, store, now)

	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_prompt_only_running",
		AuthorityEvidence: ompPromptOnlyMarker,
	})
	if err == nil {
		t.Fatal("cross-pair bind (omp-native-verifier + omp_prompt_only marker) must be refused")
	}
	if !strings.Contains(err.Error(), "cross-pair") {
		t.Fatalf("expected cross-pair refusal, got: %v", err)
	}
}

// TestOMPPromptOnlyPostureRejectsCrossPairFromPromptOnly pins the inverse
// cross-pair gate: a RunAttempt minted on the omp-prompt-only harness
// cannot bind the omp_native_verifier marker.
func TestOMPPromptOnlyPostureRejectsCrossPairFromPromptOnly(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ra := mintOMPPromptOnlyExecutionAttempt(t, store, now)

	_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  "pol_native_running",
		AuthorityEvidence: ompNativeVerifierMarker,
	})
	if err == nil {
		t.Fatal("cross-pair bind (omp-prompt-only + omp_native_verifier marker) must be refused")
	}
	if !strings.Contains(err.Error(), "cross-pair") {
		t.Fatalf("expected cross-pair refusal, got: %v", err)
	}
}
