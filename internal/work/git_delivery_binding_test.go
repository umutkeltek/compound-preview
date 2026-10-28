package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestPreFingerprintGitDeliveryPreservesCanonicalWorkHash(t *testing.T) {
	const legacyJSON = `{"schema_version":"compound.work.v0.1","id":"work_pre_fingerprint","created_at":"2026-07-13T22:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"codex","git_delivery":{"target_worktree_root":"/repo/product","target_local_ref":"refs/heads/main","expected_base_sha":"0123456789abcdef0123456789abcdef01234567","publication_mode":"local_only"},"state":"running","success_predicate":"legacy delivery remains readable without changing its hash","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	var legacy envelope.WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256([]byte(legacyJSON))
	want := "sha256:" + hex.EncodeToString(wantSum[:])
	got, err := workEnvelopeSHA256(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("pre-fingerprint canonical WorkEnvelope hash changed: got %s want %s", got, want)
	}
}

func TestPreFingerprintGitDeliveryBackfillsOnlyThroughExactOperatorSameStateAdvance(t *testing.T) {
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	store, legacy, repo, base := preFingerprintGitDeliveryFixture(t, now, "running", "operator:test")
	before := *legacy.GitDelivery
	_, beforeEvents, err := store.Show(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	requested := localGitDelivery(repo, base)
	callerFingerprint := strings.Repeat("f", 64)
	requested.RepositoryFingerprint = callerFingerprint

	plan, err := store.AdvancePlan(legacy.ID, now.Add(3*time.Minute), AdvancePlanOptions{
		Actor:       "operator:test",
		To:          legacy.State,
		GitDelivery: requested,
	})
	if err != nil {
		t.Fatalf("plan exact repository identity backfill: %v", err)
	}
	if plan.GitDelivery == nil || plan.GitDelivery.RepositoryFingerprint == "" || plan.GitDelivery.RepositoryFingerprint == callerFingerprint {
		t.Fatalf("backfill plan did not derive repository identity: %+v", plan.GitDelivery)
	}
	if !sameGitDeliveryIgnoringFingerprint(before, *plan.GitDelivery) {
		t.Fatalf("backfill changed delivery authority beyond fingerprint: before=%+v after=%+v", before, *plan.GitDelivery)
	}
	if !strings.Contains(plan.ActionIntent.Preview, "backfill Git repository identity") {
		t.Fatalf("backfill action is not explicit: %+v", plan.ActionIntent)
	}

	bound, err := store.Advance(legacy.ID, AdvanceInput{
		To:          legacy.State,
		Actor:       "operator:test",
		GitDelivery: requested,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("apply exact repository identity backfill: %v", err)
	}
	if bound.State != legacy.State || bound.GitDelivery == nil || bound.GitDelivery.RepositoryFingerprint == "" || !sameGitDeliveryIgnoringFingerprint(before, *bound.GitDelivery) {
		t.Fatalf("backfilled work = %+v", bound)
	}
	if bound.ExpectedBaseSHA != "" || len(bound.ActionIntentIDs) != len(legacy.ActionIntentIDs)+1 || bound.PolicyDecisionID == legacy.PolicyDecisionID {
		t.Fatalf("backfill did not use the governed advance journal: %+v", bound)
	}
	_, afterEvents, err := store.Show(bound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents)+1 || len(afterEvents[len(afterEvents)-1].Attached) < 2 {
		t.Fatalf("backfill lifecycle evidence = %+v", afterEvents)
	}
	if err := store.validateBoundGitDelivery(bound); err != nil {
		t.Fatalf("backfilled repository identity is not authoritative: %v", err)
	}
	if _, err := store.Advance(bound.ID, AdvanceInput{To: bound.State, Actor: "operator:test", GitDelivery: requested}, now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "already has git_delivery") {
		t.Fatalf("second backfill must be rejected as rebinding, got %v", err)
	}
}

func TestPreFingerprintGitDeliveryRejectsUnsafeMigrationPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 4, 0, 0, 0, time.UTC)
	t.Run("executor", func(t *testing.T) {
		store, legacy, repo, base := preFingerprintGitDeliveryFixture(t, now, "running", "codex")
		_, err := store.Advance(legacy.ID, AdvanceInput{To: legacy.State, Actor: "codex", GitDelivery: localGitDelivery(repo, base)}, now.Add(3*time.Minute))
		if err == nil || !strings.Contains(err.Error(), "operator provenance") {
			t.Fatalf("executor backfill error = %v", err)
		}
	})
	t.Run("lifecycle transition", func(t *testing.T) {
		store, legacy, repo, base := preFingerprintGitDeliveryFixture(t, now, "running", "operator:test")
		_, err := store.AdvancePlan(legacy.ID, now.Add(3*time.Minute), AdvancePlanOptions{Actor: "operator:test", To: "verifying", GitDelivery: localGitDelivery(repo, base)})
		if err == nil || !strings.Contains(err.Error(), "same-state") {
			t.Fatalf("transition plus backfill error = %v", err)
		}
	})
	t.Run("delivery field drift", func(t *testing.T) {
		store, legacy, repo, base := preFingerprintGitDeliveryFixture(t, now, "running", "operator:test")
		landingGit(t, repo, "branch", "other", base)
		requested := localGitDelivery(repo, base)
		requested.TargetLocalRef = "refs/heads/other"
		_, err := store.AdvancePlan(legacy.ID, now.Add(3*time.Minute), AdvancePlanOptions{Actor: "operator:test", To: legacy.State, GitDelivery: requested})
		if err == nil || !strings.Contains(err.Error(), "exactly match") {
			t.Fatalf("delivery drift backfill error = %v", err)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		store, legacy, repo, base := preFingerprintGitDeliveryFixture(t, now, "running", "operator:test")
		legacy.State = "completed"
		if err := store.writeJSON(store.workPath(legacy.ID), &legacy); err != nil {
			t.Fatal(err)
		}
		_, err := store.AdvancePlan(legacy.ID, now.Add(3*time.Minute), AdvancePlanOptions{Actor: "operator:test", To: legacy.State, GitDelivery: localGitDelivery(repo, base)})
		if err == nil || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("terminal backfill error = %v", err)
		}
	})
	t.Run("ordinary advance before backfill", func(t *testing.T) {
		store, legacy, _, _ := preFingerprintGitDeliveryFixture(t, now, "running", "codex")
		_, err := store.Advance(legacy.ID, AdvanceInput{To: "verifying", Actor: "codex"}, now.Add(3*time.Minute))
		if err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
			t.Fatalf("ordinary advance before backfill error = %v", err)
		}
	})
}

func TestLegacyNonterminalWorkBindsGitDeliveryThroughAdvance(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	repo, base := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "legacy work gains exact delivery authority"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	contract := localGitDelivery(repo, base)
	if _, err := store.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "codex",
		GitDelivery: contract,
	}, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("ordinary executor must not bind immutable delivery authority, got %v", err)
	}

	plan, err := store.AdvancePlan(w.ID, now.Add(4*time.Minute), AdvancePlanOptions{
		Actor:       "operator:test",
		To:          "running",
		GitDelivery: contract,
		Steal:       true,
		StealReason: "operator-approved legacy delivery migration",
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GitDelivery == nil || plan.GitDelivery.TargetWorktreeRoot != canonicalRepo || plan.GitDelivery.ExpectedBaseSHA != base || plan.GitDelivery.RepositoryFingerprint == "" {
		t.Fatalf("normalized delivery binding plan = %+v", plan)
	}
	if !commandHasFlagValue(plan.AdvanceCommand, "--target-worktree-root", canonicalRepo) ||
		!commandHasFlagValue(plan.AdvanceCommand, "--expected-base-sha", base) ||
		!contains(plan.AdvanceCommand, "--local-only") {
		t.Fatalf("advance command dropped delivery binding: %v", plan.AdvanceCommand)
	}

	bound, err := store.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "operator:test",
		GitDelivery: contract,
		Steal:       true,
		StealReason: "operator-approved legacy delivery migration",
	}, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != "running" || bound.GitDelivery == nil || *bound.GitDelivery != *plan.GitDelivery || bound.ExpectedBaseSHA != "" {
		t.Fatalf("bound legacy work = %+v", bound)
	}
	var action envelope.ActionIntent
	if err := store.readJSON(store.envPath(bound.ActionIntentIDs[len(bound.ActionIntentIDs)-1]), &action); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(action.Preview, "bind Git delivery") || !strings.Contains(action.Preview, canonicalRepo) || !strings.Contains(action.Preview, base) || !strings.Contains(action.Preview, plan.GitDelivery.RepositoryFingerprint) || action.Reversibility != "manual" {
		t.Fatalf("binding action does not carry exact authority: %+v", action)
	}

	if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: "operator:test", GitDelivery: contract}, now.Add(6*time.Minute)); err == nil || !strings.Contains(err.Error(), "already has git_delivery") {
		t.Fatalf("delivery rebinding must fail closed, got %v", err)
	}
}

func TestDeliveryBindingIsSameStateAndCompleteOnly(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	repo, base := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "binding cannot hide a lifecycle transition"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	if _, err := store.AdvancePlan(w.ID, now.Add(3*time.Minute), AdvancePlanOptions{
		Actor:       "operator:test",
		To:          "verifying",
		GitDelivery: localGitDelivery(repo, base),
	}); err == nil || !strings.Contains(err.Error(), "same-state") {
		t.Fatalf("delivery binding plus lifecycle transition should fail, got %v", err)
	}
	if _, err := store.AdvancePlan(w.ID, now.Add(4*time.Minute), AdvancePlanOptions{
		Actor: "operator:test",
		To:    "running",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
		},
	}); err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("partial delivery binding should fail, got %v", err)
	}
	unchanged, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.GitDelivery != nil || unchanged.State != "running" {
		t.Fatalf("rejected binding mutated work: %+v", unchanged)
	}
}

func TestRejectedDeliveryBindingDoesNotRecordLeaseSteal(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	repo, base := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "rejected binding leaves no successful override evidence"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	before, beforeEvents, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSHA, err := workEnvelopeSHA256(before)
	if err != nil {
		t.Fatal(err)
	}
	beforeLease, err := store.readLease(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	eventFiles := func() string {
		entries, err := os.ReadDir(filepath.Join(store.home, "events"))
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return strings.Join(names, "\n")
	}
	beforeEventFiles := eventFiles()

	if _, err := store.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "rented-executor",
		GitDelivery: localGitDelivery(repo, base),
		Steal:       true,
		StealReason: "attempt invalid legacy delivery binding",
	}, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("executor delivery binding should fail operator provenance, got %v", err)
	}

	after, afterEvents, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterSHA, err := workEnvelopeSHA256(after)
	if err != nil {
		t.Fatal(err)
	}
	afterLease, err := store.readLease(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSHA != beforeSHA {
		t.Fatalf("rejected binding mutated WorkEnvelope: before=%s after=%s", beforeSHA, afterSHA)
	}
	if afterLease != beforeLease {
		t.Fatalf("rejected binding mutated lease: before=%+v after=%+v", beforeLease, afterLease)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("rejected binding appended lifecycle evidence: before=%d after=%d events=%+v", len(beforeEvents), len(afterEvents), afterEvents)
	}
	if afterEventFiles := eventFiles(); afterEventFiles != beforeEventFiles {
		t.Fatalf("rejected binding wrote action/policy evidence:\nbefore:\n%s\nafter:\n%s", beforeEventFiles, afterEventFiles)
	}
}

func preFingerprintGitDeliveryFixture(t *testing.T, now time.Time, state, holder string) (*Store, envelope.WorkEnvelope, string, string) {
	t.Helper()
	repo, base := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "pre-fingerprint delivery requires governed migration",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if state == "running" {
		if _, err := store.Claim(w.ID, holder, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: holder}, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	} else if state != "ready" {
		t.Fatalf("unsupported pre-fingerprint fixture state %q", state)
	}
	w, err = store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := w.GitDelivery.RepositoryFingerprint
	body, err := os.ReadFile(store.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	needle := "    \"repository_fingerprint\": \"" + fingerprint + "\",\n"
	if strings.Count(string(body), needle) != 1 {
		t.Fatalf("persisted work did not contain one fingerprint field:\n%s", body)
	}
	body = []byte(strings.Replace(string(body), needle, "", 1))
	if err := os.WriteFile(store.workPath(w.ID), body, 0o644); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.GitDelivery == nil || legacy.GitDelivery.RepositoryFingerprint != "" {
		t.Fatalf("pre-fingerprint fixture = %+v", legacy.GitDelivery)
	}
	if err := legacy.Validate(); err != nil {
		t.Errorf("pre-fingerprint fixture must remain readable: %v", err)
	}
	return store, legacy, repo, base
}

func sameGitDeliveryIgnoringFingerprint(a, b envelope.GitDeliveryContract) bool {
	a.RepositoryFingerprint = ""
	b.RepositoryFingerprint = ""
	return a == b
}

func TestRepoScopedContextAloneDoesNotInventGitDeliveryAuthority(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	repo, _ := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contextID := "ctx_repo_scoped_close"
	writeContextPack(t, store, contextID)
	var pack envelope.ContextPack
	packPath := filepath.Join(store.home, "context", "packs", contextID+".json")
	if err := store.readJSON(packPath, &pack); err != nil {
		t.Fatal(err)
	}
	pack.Scope.Surface = repo
	if err := store.writeJSON(packPath, &pack); err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "repo orientation alone is not mutation evidence",
		ContextPackID:    contextID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	plan, err := store.ClosePlan(w.ID, now.Add(3*time.Minute), ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "control_plane_repair",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "close_policy_allowed" || len(plan.CloseCommand) == 0 || plan.NoAction != "" {
		t.Fatalf("repo-scoped context alone must not invent delivery authority: %+v", plan)
	}
	closed, err := store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "orientation-only work closed without a false delivery hold",
		ProofRefs: []string{"check:focused-tests"},
	}, now.Add(4*time.Minute))
	if err != nil || closed.Work.State != "completed" {
		t.Fatalf("repo-scoped context-only close = %+v, err=%v", closed, err)
	}
}

func TestRepoTargetActionRequiresDeclaredDeliveryForEveryNonAuditMove(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	repo, _ := initLandingRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "repo target action cannot hide delivery"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	action := envelope.ActionIntent{
		Header:        newHeader(envelope.SVActionIntent, "act_repo_target_close", now.Add(3*time.Minute), w.ID, "", "private", "tier1"),
		Actor:         "codex",
		Capability:    "source.edit",
		Action:        "edit_repo_file",
		Target:        envelope.ActionTarget{Kind: "file", ID: "README.md", Path: filepath.Join(repo, "README.md")},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "edit a tracked repository file",
		RollbackPlan:  "revert the file edit",
	}
	if err := action.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.writeJSON(store.envPath(action.ID), &action); err != nil {
		t.Fatal(err)
	}
	// S1: ActionIntentIDs is now act_*-only and the legacy RunID seam is gone.
	// The pre-written action envelope above is what the close gate needs to
	// find to detect a git-scoped target; wire it onto the work envelope by
	// hand so the gate sees it the same way the pre-S1 RunID side-effect did.
	var attributed envelope.WorkEnvelope
	if err := store.readJSON(store.workPath(w.ID), &attributed); err != nil {
		t.Fatal(err)
	}
	attributed.ActionIntentIDs = append(attributed.ActionIntentIDs, action.ID)
	if err := store.writeJSON(store.workPath(w.ID), &attributed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, CloseInput{
		Outcome:          "completed",
		MoveType:         "memory_delta",
		Actor:            "codex",
		Summary:          "must bind repo delivery",
		ProofRefs:        []string{"check:focused-tests"},
		MemoryCandidates: []string{"candidate:repo-target"},
	}, now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "git_delivery") || !strings.Contains(err.Error(), action.ID) {
		t.Fatalf("repo-target action omission should fail closed, got %v", err)
	}

	audit, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "repo audit remains non-mutating"}, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, audit.ID, now.Add(6*time.Minute))
	auditAction := action
	auditAction.Header = newHeader(envelope.SVActionIntent, "act_repo_target_audit", now.Add(9*time.Minute), audit.ID, "", "private", "tier1")
	if err := auditAction.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.writeJSON(store.envPath(auditAction.ID), &auditAction); err != nil {
		t.Fatal(err)
	}
	// S1: same attribution note as above -- attach the pre-written audit
	// ActionIntent to the audit work envelope so the gate sees it. The
	// audit case expects this NOT to require git_delivery (moveType is
	// audit_only), so the gate must iterate the action and hit the
	// audit_only short-circuit, not bail before reading the action.
	var attributedAudit envelope.WorkEnvelope
	if err := store.readJSON(store.workPath(audit.ID), &attributedAudit); err != nil {
		t.Fatal(err)
	}
	attributedAudit.ActionIntentIDs = append(attributedAudit.ActionIntentIDs, auditAction.ID)
	if err := store.writeJSON(store.workPath(audit.ID), &attributedAudit); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Close(audit.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "audit_only",
		Actor:     "codex",
		Summary:   "read-only audit",
		ProofRefs: []string{"check:audit"},
	}, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("audit-only work should not require Git delivery: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("audit close = %+v", closed.Work)
	}
}

func TestRepoTargetActionEvidenceCannotDisappearBeforeClose(t *testing.T) {
	for _, mutate := range []string{"missing", "mismatched"} {
		t.Run(mutate, func(t *testing.T) {
			now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
			repo, _ := initLandingRepo(t)
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "recorded repo action remains inspectable"}, now)
			if err != nil {
				t.Fatal(err)
			}
			activateLandingWork(t, store, w.ID, now)
			action := envelope.ActionIntent{
				Header:        newHeader(envelope.SVActionIntent, "act_repo_evidence_"+mutate, now.Add(3*time.Minute), w.ID, "", "private", "tier1"),
				Actor:         "codex",
				Capability:    "source.edit",
				Action:        "edit_repo_file",
				Target:        envelope.ActionTarget{Kind: "file", ID: "future.txt", Path: filepath.Join(repo, "future.txt")},
				MutationClass: "local_reversible",
				Reversibility: "easy",
				BlastRadius:   "local",
				Preview:       "edit a future repository file",
				RollbackPlan:  "revert the file edit",
			}
			if err := action.Validate(); err != nil {
				t.Fatal(err)
			}
			// Load pre-admission historical evidence for the late-close corruption check.
			policyDecision := envelope.PolicyDecision{
				Header:         newHeader(envelope.SVPolicyDecision, "pol_repo_evidence_"+mutate, now.Add(3*time.Minute), w.ID, "", "private", "tier1"),
				ActionIntentID: action.ID,
				Outcome:        "allow",
			}
			if err := policyDecision.Validate(); err != nil {
				t.Fatal(err)
			}
			attachHistoricalSourceAction(t, store, w, action, policyDecision, now.Add(3*time.Minute))
			// The action is now in ActionIntentIDs and the action event is on disk. Mutate
			// the event without rewriting older append-only history, and Close must catch
			// the corruption.
			switch mutate {
			case "missing":
				if err := store.removeEvent(action.ID); err != nil {
					t.Fatal(err)
				}
			case "mismatched":
				action.IDs.WorkID = "work_other"
				if err := store.replaceEvent(action.ID, &action); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Close(w.ID, CloseInput{
				Outcome:          "completed",
				MoveType:         "memory_delta",
				Actor:            "codex",
				Summary:          "corrupt action evidence must not erase delivery scope",
				ProofRefs:        []string{"check:focused-tests"},
				MemoryCandidates: []string{"candidate:repo-evidence"},
			}, now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), action.ID) {
				t.Fatalf("%s ActionIntent should fail loudly, got %v", mutate, err)
			}
		})
	}
}
