package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/reviewqueue"
	"compound/internal/work"
)

const dirtyReviewDigestDriftSubEnv = "COMPOUND_DIRTY_REVIEW_DIGEST_DRIFT_SUB"

func TestBuildReviewQueueReportStampsCurrentOwnerAuthority(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store := openWorkbenchAuthorityWorkStore(t, homePath)
	workID := "work_review_authority_live"
	if _, err := store.Create(work.CreateInput{
		WorkID:           workID,
		Owner:            "source-control-cleanup",
		SuccessPredicate: "recorded review report is linked to its owner work",
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	recordWorkbenchAuthorityReview(t, homePath, workID, cliTestNow.Add(time.Minute))
	before := workbenchAuthorityWorkCount(t, homePath)

	first, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	action := workbenchAuthorityAction(t, first)
	again := workbenchAuthorityAction(t, second)
	if action.ActionIntent == nil || action.PolicyDecision == nil || action.PolicyAuthority == nil {
		t.Fatalf("live owner action did not receive a complete current authority bundle: %+v", action)
	}
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, *action.PolicyDecision, *action.PolicyAuthority); err != nil {
		t.Fatalf("stamped authority bundle invalid: %v", err)
	}
	if action.ActionIntent.IDs.WorkID != workID || !action.PolicyAuthority.Current || !action.PolicyAuthority.Allowed || action.PolicyAuthority.PolicySource != "built-in" {
		t.Fatalf("stamped authority did not come from current owner/policy: %+v", action)
	}
	if action.ActionIntent.ID != again.ActionIntent.ID || action.PolicyAuthority.Revision != again.PolicyAuthority.Revision {
		t.Fatalf("equivalent fresh renders changed stable owner/policy identity: first=%+v second=%+v", action, again)
	}
	if after := workbenchAuthorityWorkCount(t, homePath); after != before {
		t.Fatalf("review rendering created WorkEnvelopes: before=%d after=%d", before, after)
	}
}

func TestBuildReviewQueueReportChangesAuthorityWithReviewMeaning(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	workID := "work_review_authority_meaning"
	store := openWorkbenchAuthorityWorkStore(t, homePath)
	if _, err := store.Create(work.CreateInput{WorkID: workID, Owner: "source-control-cleanup", SuccessPredicate: "authority follows exact reviewed meaning"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	paths := recordWorkbenchAuthorityReview(t, homePath, workID, cliTestNow.Add(time.Minute))
	first, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	firstAction := workbenchAuthorityAction(t, first)
	review, err := authority.LoadSourceControlReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	review.NextAction = "record a materially different reviewed next action"
	writeWorkbenchSourceControlReview(t, paths.ReviewYAMLPath, review)
	second, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	secondAction := workbenchAuthorityAction(t, second)
	if firstAction.ActionIntent.ID == secondAction.ActionIntent.ID || firstAction.PolicyAuthority.Revision == secondAction.PolicyAuthority.Revision {
		t.Fatalf("semantic review drift retained authority identity: first=%+v second=%+v", firstAction, secondAction)
	}
}

func TestBuildReviewQueueReportLeavesMissingAndTerminalOwnersUnstamped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		workID   string
		terminal bool
	}{
		{name: "missing", workID: "work_review_authority_missing"},
		{name: "terminal", workID: "work_review_authority_terminal", terminal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := initializedTestHome(t)
			if tc.terminal {
				store := openWorkbenchAuthorityWorkStore(t, homePath)
				if _, err := store.Create(work.CreateInput{WorkID: tc.workID, Owner: "source-control-cleanup", SuccessPredicate: "terminal owner stays non-runnable"}, cliTestNow); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Claim(tc.workID, "test-executor", cliTestNow.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Advance(tc.workID, work.AdvanceInput{Actor: "test-executor", To: "running"}, cliTestNow.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Close(tc.workID, work.CloseInput{
					Outcome:   "completed",
					MoveType:  "control_plane_repair",
					Actor:     "test-executor",
					Summary:   "terminal authority owner fixture",
					Changed:   true,
					ProofRefs: []string{"proof:terminal-authority-owner"},
				}, cliTestNow.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			recordWorkbenchAuthorityReview(t, homePath, tc.workID, cliTestNow.Add(time.Minute))
			report, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
			if err != nil {
				t.Fatal(err)
			}
			action := workbenchAuthorityAction(t, report)
			if action.ActionIntent == nil || action.AuthorityContext == nil {
				t.Fatalf("fixture owner seed missing: %+v", action)
			}
			if action.PolicyDecision != nil || action.PolicyAuthority != nil {
				t.Fatalf("%s WorkEnvelope received runnable authority: %+v", tc.name, action)
			}
		})
	}
}

func TestStampReviewActionAuthorityFailsClosedForIncompleteSeeds(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	workID := "work_review_authority_seed"
	store := openWorkbenchAuthorityWorkStore(t, homePath)
	if _, err := store.Create(work.CreateInput{WorkID: workID, Owner: "source-control-cleanup", SuccessPredicate: "only complete owner seeds are stamped"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	recordWorkbenchAuthorityReview(t, homePath, workID, cliTestNow.Add(time.Minute))
	base, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: authority.SourceControlCleanupReviewArea}, cliTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	authority.AnnotateSourceControlCleanupQueue(&base)
	seed := workbenchAuthorityAction(t, base)

	tests := []struct {
		name   string
		mutate func(*reviewartifact.Action)
	}{
		{name: "missing action intent", mutate: func(action *reviewartifact.Action) { action.ActionIntent = nil }},
		{name: "missing authority context", mutate: func(action *reviewartifact.Action) { action.AuthorityContext = nil }},
		{name: "malformed action intent", mutate: func(action *reviewartifact.Action) { action.ActionIntent.Capability = "" }},
		{name: "malformed authority context", mutate: func(action *reviewartifact.Action) { action.AuthorityContext.TargetScope = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := cloneWorkbenchAuthoritySeed(seed)
			action.PolicyDecision = &envelope.PolicyDecision{Header: envelope.Header{ID: "pol_stale"}}
			action.PolicyAuthority = &policy.AuthorityFace{SchemaVersion: policy.AuthorityFaceSchema, Current: true, Revision: "stale"}
			tc.mutate(&action)
			report := reviewartifact.QueueReport{Items: []reviewartifact.QueueItem{{Actions: []reviewartifact.Action{action}}}}
			if err := reviewqueue.StampReviewActionAuthority(homePath, &report, cliTestNow.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			got := report.Items[0].Actions[0]
			if got.PolicyDecision != nil || got.PolicyAuthority != nil {
				t.Fatalf("incomplete seed retained stale or runnable authority: %+v", got)
			}
		})
	}
}

func TestStampReviewActionAuthorityRejectsSyntheticOwnerRecord(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	syntheticWorkID := "work_live_review_authority_record"
	// The durable create door now REFUSES the ephemeral work_live_ namespace,
	// so the synthetic record can only arrive out-of-band (an older binary, a
	// hand edit) — which is exactly the scenario the authority door must keep
	// refusing. Seed the envelope JSON directly, bypassing Create.
	syntheticEnvelope := map[string]any{
		"schema_version":    "compound.work.v0.1",
		"id":                syntheticWorkID,
		"created_at":        cliTestNow.UTC().Format(time.RFC3339),
		"state":             "proposed",
		"owner":             "source-control-cleanup",
		"success_predicate": "synthetic correlation never becomes durable action authority",
	}
	syntheticBody, err := json.Marshal(syntheticEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	syntheticDir := filepath.Join(homePath, "work", "work")
	if err := os.MkdirAll(syntheticDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syntheticDir, syntheticWorkID+".json"), append(syntheticBody, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	recordWorkbenchAuthorityReview(t, homePath, "work_review_authority_seed_source", cliTestNow.Add(time.Minute))
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: authority.SourceControlCleanupReviewArea}, cliTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	authority.AnnotateSourceControlCleanupQueue(&report)
	action := workbenchAuthorityAction(t, report)
	if action.ActionIntent == nil || action.AuthorityContext == nil {
		t.Fatalf("durable source fixture was not seeded: %+v", action)
	}
	action.ActionIntent.IDs.WorkID = syntheticWorkID
	report.Items[0].Actions = []reviewartifact.Action{action}
	if err := reviewqueue.StampReviewActionAuthority(homePath, &report, cliTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := report.Items[0].Actions[0]
	if got.PolicyDecision != nil || got.PolicyAuthority != nil {
		t.Fatalf("synthetic work correlation received durable action authority: %+v", got)
	}
}

func TestAuthorityDirtyReviewRejectsExpectedSourceDigestDrift(t *testing.T) {
	t.Parallel()
	if os.Getenv(dirtyReviewDigestDriftSubEnv) == "1" {
		cmdAuthority([]string{
			"dirty-review",
			os.Getenv("COMPOUND_DIRTY_REVIEW_DIGEST_PATH"),
			"--expected-source-digest",
			os.Getenv("COMPOUND_DIRTY_REVIEW_DIGEST_EXPECTED"),
			"--json",
		})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	workID := "work_review_authority_digest_drift"
	store := openWorkbenchAuthorityWorkStore(t, homePath)
	if _, err := store.Create(work.CreateInput{WorkID: workID, Owner: "source-control-cleanup", SuccessPredicate: "stale reviewed meaning cannot execute"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	paths := recordWorkbenchAuthorityReview(t, homePath, workID, cliTestNow.Add(time.Minute))
	report, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	action := workbenchAuthorityAction(t, report)
	expectedDigest := workbenchActionArgValue(t, action, "--expected-source-digest")
	review, err := authority.LoadSourceControlReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	review.Decision = "owner_action_required"
	writeWorkbenchSourceControlReview(t, paths.ReviewYAMLPath, review)
	before, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAuthorityDirtyReviewRejectsExpectedSourceDigestDrift$")
	cmd.Env = append(os.Environ(),
		dirtyReviewDigestDriftSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_DIRTY_REVIEW_DIGEST_PATH="+paths.ReviewYAMLPath,
		"COMPOUND_DIRTY_REVIEW_DIGEST_EXPECTED="+expectedDigest,
	)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 || !strings.Contains(string(output), "source digest drift") {
		t.Fatalf("dirty-review drift gate = err=%v output=%s", err, output)
	}
	after, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("digest drift refusal rewrote review source\nbefore=%s\nafter=%s", before, after)
	}
	reviewed, err := filepath.Glob(filepath.Join(filepath.Dir(paths.ReviewYAMLPath), "*.reviewed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed) != 0 {
		t.Fatalf("digest drift refusal recorded reports: %v", reviewed)
	}
}

func TestBuildReviewQueueReportCarriesCurrentPolicyHold(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	workID := "work_review_authority_held"
	store := openWorkbenchAuthorityWorkStore(t, homePath)
	if _, err := store.Create(work.CreateInput{WorkID: workID, Owner: "source-control-cleanup", SuccessPredicate: "held current policy stays visible"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	recordWorkbenchAuthorityReview(t, homePath, workID, cliTestNow.Add(time.Minute))
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome:\n  local_reversible:\n    outcome: hold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := buildReviewQueueReport(homePath, authority.SourceControlCleanupReviewArea, false)
	if err != nil {
		t.Fatal(err)
	}
	action := workbenchAuthorityAction(t, report)
	if action.PolicyDecision == nil || action.PolicyAuthority == nil {
		t.Fatalf("valid held evaluation was erased: %+v", action)
	}
	if action.PolicyDecision.Outcome != "hold" || !action.PolicyAuthority.Current || action.PolicyAuthority.Allowed {
		t.Fatalf("current policy hold was not carried exactly: %+v", action)
	}
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, *action.PolicyDecision, *action.PolicyAuthority); err != nil {
		t.Fatalf("held authority bundle invalid: %v", err)
	}
}

func TestBuildReviewQueueReportFailsWhenPolicyCannotLoad(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte("class_outcome: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildReviewQueueReport(homePath, "", false); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("invalid current policy did not fail the review report: %v", err)
	}
}

func openWorkbenchAuthorityWorkStore(t *testing.T, homePath string) *work.Store {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func recordWorkbenchAuthorityReview(t *testing.T, homePath, workID string, now time.Time) authority.SourceControlReviewPlanPaths {
	t.Helper()
	repo := initCompoundCommandGitRepo(t)
	writeFileAt(t, filepath.Join(repo, "README.md"), "reviewed dirty state\n")
	snapshot, err := authority.ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := authority.BuildSourceControlReviewPlan(snapshot, workID, now)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := authority.RecordSourceControlReviewPlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkbenchSourceControlReview(t, paths.ReviewYAMLPath, authority.SourceControlReview{
		SchemaVersion: authority.SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "parked",
		Rationale:     "reviewed source-control state remains deliberately parked",
		Confidence:    1,
		NextAction:    "keep the reviewed state parked under its owner",
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
	})
	return paths
}

func writeWorkbenchSourceControlReview(t *testing.T, path string, review authority.SourceControlReview) {
	t.Helper()
	data, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func workbenchActionArgValue(t *testing.T, action reviewartifact.Action, flag string) string {
	t.Helper()
	for i, arg := range action.Argv {
		if arg == flag && i+1 < len(action.Argv) {
			return action.Argv[i+1]
		}
	}
	t.Fatalf("action missing %s: %+v", flag, action)
	return ""
}

func workbenchAuthorityAction(t *testing.T, report reviewartifact.QueueReport) reviewartifact.Action {
	t.Helper()
	for _, item := range report.Items {
		if item.Area == authority.SourceControlCleanupReviewArea && len(item.Actions) == 1 {
			return item.Actions[0]
		}
	}
	t.Fatalf("source-control authority action missing from report: %+v", report.Items)
	return reviewartifact.Action{}
}

func workbenchAuthorityWorkCount(t *testing.T, homePath string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(homePath, "work", "work", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

func cloneWorkbenchAuthoritySeed(in reviewartifact.Action) reviewartifact.Action {
	out := in
	if in.ActionIntent != nil {
		intent := *in.ActionIntent
		intent.EvidenceRefs = append([]string{}, in.ActionIntent.EvidenceRefs...)
		out.ActionIntent = &intent
	}
	if in.AuthorityContext != nil {
		context := *in.AuthorityContext
		context.ExpectedProof = append([]string{}, in.AuthorityContext.ExpectedProof...)
		context.EvidenceRefs = append([]string{}, in.AuthorityContext.EvidenceRefs...)
		out.AuthorityContext = &context
	}
	return out
}
