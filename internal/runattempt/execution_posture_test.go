package runattempt

import (
	"strings"
	"testing"
	"time"
)

func executionPostureAuthorityEvidence() []ExecutionPostureEvidence {
	return []ExecutionPostureEvidence{
		{Kind: "permission_posture", Ref: "harness-posture://claude_code", Revision: "sha256:posture"},
		{Kind: "blocking_hook_installation", Ref: "installed-hook://claude_code/PreToolUse", Revision: "sha256:installation"},
		{Kind: "permission_authority", Ref: "compound://harness-permissions/claude_code", Revision: "sha256:authority"},
		{Kind: "blocking_hook_plan", Ref: "hook://claude_code/PreToolUse", Revision: "sha256:plan"},
	}
}

func TestExecutionPosturePlanBindIsIdempotentAcrossStoreReopen(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_posture", "claude", "claude", now)
	if err != nil {
		t.Fatal(err)
	}

	posture := ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_running"}
	if _, err := store.SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatalf("exact plan bind after reopening store must be idempotent: %v", err)
	}
}

func TestExecutionPostureReturnedValuesDoNotAliasStoreCache(t *testing.T) {
	newBoundStore := func(t *testing.T) (*Store, RunAttempt) {
		t.Helper()
		home := t.TempDir()
		store := Open(home)
		ra, err := store.Mint("work_posture", "claude", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		bound, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
			PermissionMode:    "bypass",
			PolicyDecisionID:  "pol_running",
			AuthorityEvidence: executionPostureAuthorityEvidence(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return store, bound
	}
	assertCanonical := func(t *testing.T, got RunAttempt) {
		t.Helper()
		if got.ExecutionPosture == nil {
			t.Fatal("execution posture is nil")
		}
		if got.ExecutionPosture.PermissionMode != "bypass" {
			t.Fatalf("cached permission mode = %q, want bypass", got.ExecutionPosture.PermissionMode)
		}
		for _, item := range got.ExecutionPosture.AuthorityEvidence {
			if item.Revision == "sha256:caller-mutated" {
				t.Fatal("cached authority evidence was mutated through returned alias")
			}
		}
	}
	mutate := func(t *testing.T, got *RunAttempt) {
		t.Helper()
		if got.ExecutionPosture == nil || len(got.ExecutionPosture.AuthorityEvidence) == 0 {
			t.Fatalf("posture cannot exercise aliasing: %+v", got.ExecutionPosture)
		}
		got.ExecutionPosture.PermissionMode = "plan"
		got.ExecutionPosture.AuthorityEvidence[0].Revision = "sha256:caller-mutated"
	}

	t.Run("set return", func(t *testing.T) {
		store, bound := newBoundStore(t)
		mutate(t, &bound)
		got, err := store.Get(bound.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertCanonical(t, got)
	})

	t.Run("cached get return", func(t *testing.T) {
		store, bound := newBoundStore(t)
		got, err := store.Get(bound.ID)
		if err != nil {
			t.Fatal(err)
		}
		mutate(t, &got)
		got, err = store.Get(bound.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertCanonical(t, got)
	})

	t.Run("disk load return", func(t *testing.T) {
		store, bound := newBoundStore(t)
		reopened := Open(store.Home())
		got, err := reopened.Get(bound.ID)
		if err != nil {
			t.Fatal(err)
		}
		mutate(t, &got)
		got, err = reopened.Get(bound.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertCanonical(t, got)
	})
}

func TestExecutionPostureMutationModesRequireExactlyFourAuthorityKinds(t *testing.T) {
	requiredKinds := []string{
		"permission_authority",
		"permission_posture",
		"blocking_hook_plan",
		"blocking_hook_installation",
	}
	mutationModes := []string{"default", "acceptEdits", "bypass"}
	newAttempt := func(t *testing.T) (*Store, RunAttempt) {
		t.Helper()
		store := Open(t.TempDir())
		ra, err := store.Mint("work_posture", "claude", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		return store, ra
	}

	for _, mode := range mutationModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			for _, missingKind := range requiredKinds {
				missingKind := missingKind
				t.Run("missing_"+missingKind, func(t *testing.T) {
					store, ra := newAttempt(t)
					evidence := executionPostureAuthorityEvidence()
					filtered := evidence[:0]
					for _, item := range evidence {
						if item.Kind != missingKind {
							filtered = append(filtered, item)
						}
					}
					_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
						PermissionMode: mode, PolicyDecisionID: "pol_running", AuthorityEvidence: filtered,
					})
					if err == nil || !strings.Contains(err.Error(), missingKind) {
						t.Fatalf("%s without %s evidence error = %v", mode, missingKind, err)
					}
				})
			}

			t.Run("unexpected_authority_kind", func(t *testing.T) {
				store, ra := newAttempt(t)
				evidence := append(executionPostureAuthorityEvidence(), ExecutionPostureEvidence{
					Kind: "unrelated_authority", Ref: "authority://unrelated", Revision: "sha256:unrelated",
				})
				_, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
					PermissionMode: mode, PolicyDecisionID: "pol_running", AuthorityEvidence: evidence,
				})
				if err == nil || !strings.Contains(err.Error(), "unrelated_authority") {
					t.Fatalf("%s with unexpected authority kind error = %v", mode, err)
				}
			})

			t.Run("exact_authority_kinds", func(t *testing.T) {
				store, ra := newAttempt(t)
				if _, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
					PermissionMode: mode, PolicyDecisionID: "pol_running", AuthorityEvidence: executionPostureAuthorityEvidence(),
				}); err != nil {
					t.Fatalf("%s with exact authority kinds: %v", mode, err)
				}
			})
		})
	}

	t.Run("plan_does_not_require_authority_evidence", func(t *testing.T) {
		store, ra := newAttempt(t)
		if _, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
			PermissionMode: "plan", PolicyDecisionID: "pol_running",
		}); err != nil {
			t.Fatalf("plan posture without authority evidence: %v", err)
		}
	})
}

func TestExecutionPostureBindingRequiresBypassAuthoritiesAndIsImmutable(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	ra, err := store.Mint("work_posture", "claude", "claude", now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.SetExecutionPosture(ra.ID, ExecutionPosture{
		PermissionMode: "bypass", PolicyDecisionID: "pol_running",
	}); err == nil || !strings.Contains(err.Error(), "permission_authority") {
		t.Fatalf("bypass without authority evidence error = %v", err)
	}
	if got, err := store.Get(ra.ID); err != nil || got.ExecutionPosture != nil {
		t.Fatalf("rejected posture mutated attempt: got=%+v err=%v", got.ExecutionPosture, err)
	}

	evidence := executionPostureAuthorityEvidence()
	posture := ExecutionPosture{PermissionMode: "bypass", PolicyDecisionID: "pol_running", AuthorityEvidence: evidence}
	if _, err := store.SetExecutionPosture(ra.ID, posture); err != nil {
		t.Fatal(err)
	}
	evidence[0].Revision = "sha256:caller-mutated"
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionPosture == nil || len(got.ExecutionPosture.AuthorityEvidence) != 4 {
		t.Fatalf("persisted posture = %+v", got.ExecutionPosture)
	}
	for _, item := range got.ExecutionPosture.AuthorityEvidence {
		if item.Revision == "sha256:caller-mutated" {
			t.Fatal("caller mutation changed persisted execution posture evidence")
		}
	}

	canonical := *got.ExecutionPosture
	if _, err := store.SetExecutionPosture(ra.ID, canonical); err != nil {
		t.Fatalf("exact idempotent bind failed: %v", err)
	}
	canonical.PolicyDecisionID = "pol_different"
	if _, err := store.SetExecutionPosture(ra.ID, canonical); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("mismatched posture rebind error = %v", err)
	}
}

func TestExecutionPostureConcurrentDifferentBindsHaveOneWinner(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		home := t.TempDir()
		seedStore := Open(home)
		ra, err := seedStore.Mint("work_posture", "claude", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		stores := []*Store{Open(home), Open(home)}
		for _, store := range stores {
			if _, err := store.Get(ra.ID); err != nil {
				t.Fatal(err)
			}
		}
		postures := []ExecutionPosture{
			{PermissionMode: "bypass", PolicyDecisionID: "pol_concurrent_a", AuthorityEvidence: executionPostureAuthorityEvidence()},
			{PermissionMode: "acceptEdits", PolicyDecisionID: "pol_concurrent_b", AuthorityEvidence: executionPostureAuthorityEvidence()},
		}
		start := make(chan struct{})
		results := make(chan error, len(postures))
		for i, posture := range postures {
			store := stores[i]
			posture := posture
			go func() {
				<-start
				_, err := store.SetExecutionPosture(ra.ID, posture)
				results <- err
			}()
		}
		close(start)
		errA, errB := <-results, <-results
		successes := 0
		if errA == nil {
			successes++
		}
		if errB == nil {
			successes++
		}
		if successes != 1 {
			t.Fatalf("concurrent different bind successes = %d, want one (iteration=%d errors=[%v %v])", successes, attempt, errA, errB)
		}
		loserErr := errA
		if loserErr == nil {
			loserErr = errB
		}
		if !strings.Contains(loserErr.Error(), "execution posture is already bound") {
			t.Fatalf("concurrent loser error = %v, want immutable-bind refusal (iteration=%d)", loserErr, attempt)
		}
		got, err := Open(home).Get(ra.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExecutionPosture == nil || (got.ExecutionPosture.PolicyDecisionID != "pol_concurrent_a" && got.ExecutionPosture.PolicyDecisionID != "pol_concurrent_b") {
			t.Fatalf("concurrent winner was not persisted: %+v", got.ExecutionPosture)
		}
	}
}
