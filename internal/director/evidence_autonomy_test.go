package director

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

type jobHealthSampleProof struct {
	ReceiptID string
	RouteID   string
}

func TestAutonomyRoutePlanSnapshotCarriesProviderRouteKind(t *testing.T) {
	got := autonomyRoutePlanSnapshot(routeevidence.CapabilityRoutePlan{
		Chain: []routeevidence.CapabilityRouteLane{
			{
				ModelID:           "fixture-model",
				ProviderRoute:     "provider.example/model-primary",
				ProviderRouteKind: routeevidence.ProviderRouteKindDirectModel,
			},
		},
	})
	if len(got.Chain) != 1 || got.Chain[0].ProviderRouteKind != routeevidence.ProviderRouteKindDirectModel {
		t.Fatalf("director projection omitted provider route kind: %+v", got.Chain)
	}
}

func seedJobHealthAutonomyGrant(t *testing.T, home, class string, minSamples, minDays, maxFailures int, now time.Time) string {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := decisions.Record(store, home, decisions.RecordInput{
		Kind:               decisions.KindGrant,
		ID:                 "autonomy-grant-" + strings.ReplaceAll(class, "_", "-"),
		Title:              "Permit evidence-threshold admission for " + class,
		ReviewedBy:         "operator:test",
		Scope:              "autonomy-evidence-threshold",
		AutoAdmitClass:     class,
		MinTerminalSamples: minSamples,
		MinWindowDays:      minDays,
		MaxFailures:        maxFailures,
		Now:                now,
	})
	if err != nil {
		t.Fatalf("seed autonomy grant: %v", err)
	}
	return report.Import.Lifecycle.ID
}

func seedJobHealthAutonomySample(t *testing.T, home, id string, closedAt time.Time, outcome, model string, includeRouteProof bool) jobHealthSampleProof {
	t.Helper()
	plan, err := routeevidence.PlanCapabilityRoute(home, "judge-critique", routeevidence.CapabilityRoutePlanOptions{}, closedAt)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		WorkID:           "work_sample_" + id,
		Owner:            "compound-authority",
		Actor:            "operator-test",
		CanonicalSurface: "director:job-health:sample:" + id,
		SuccessPredicate: "review historical job health sample " + id,
		EvidenceRefs: []string{
			"proposed_by:director",
			"director_trigger:job_health:sample-" + id,
			"route_plan:" + plan.ID,
		},
		RouteDecisionID: plan.ID,
		State:           "proposed",
	}, closedAt.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, closedAt.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "sample-executor", closedAt.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{Actor: "sample-executor", To: "running"}, closedAt.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	resolvedModel := model
	resolvedProvider := "scout"
	if model == "auto/judge" {
		resolvedModel = "deepseek/deepseek-v4-pro"
		resolvedProvider = "deepseek"
	}
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        closedAt.Add(-time.Minute),
		RequestID:        "request-" + id,
		WorkID:           w.ID,
		ClientProfile:    "compound.panel.judge",
		Protocol:         "panel-lane",
		Provider:         "compound-route-plan",
		Model:            model,
		ResolvedProvider: resolvedProvider,
		ResolvedModel:    resolvedModel,
		ResolutionSource: "gateway_route",
		GatewayRequestID: "gateway-" + id,
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := routeevidence.RecordPanelRuntimeJoined(home, ev)
	if err != nil {
		t.Fatal(err)
	}
	proofRefs := []string{}
	if includeRouteProof {
		proofRefs = append(proofRefs, joined.Evidence.ID)
	} else {
		proofRefs = append(proofRefs, "check:synthetic-route-proof-intentionally-absent")
	}
	proofRefs = append(proofRefs, "check:job-health-state-delta:"+id)
	closed, err := store.Close(w.ID, work.CloseInput{
		Outcome:   outcome,
		MoveType:  "control_plane_repair",
		Actor:     "sample-executor",
		Summary:   "historical job-health outcome " + outcome,
		Changed:   outcome == "completed",
		ProofRefs: proofRefs,
	}, closedAt)
	if err != nil {
		t.Fatal(err)
	}
	return jobHealthSampleProof{ReceiptID: closed.Receipt.ID, RouteID: joined.Evidence.ID}
}

func TestJobHealthEvidenceThresholdAutonomyAdmitsAndRetainsExactProofs(t *testing.T) {
	home := seededDirectorHome(t)
	now := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
	grantID := seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
	first := seedJobHealthAutonomySample(t, home, "clean-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
	second := seedJobHealthAutonomySample(t, home, "clean-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
	seedFailingJobHealth(t, home)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-not-autonomous")

	report, err := Propose(home, Options{Apply: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	job := findProposalByKind(report, "job_health")
	if job == nil {
		t.Fatalf("missing job_health proposal: %+v", report.Proposals)
	}
	if job.WorkState != "admitted" || job.AutoAdmitResult == nil || !job.AutoAdmitResult.Admitted {
		t.Fatalf("threshold-met job_health proposal was not admitted: %+v", job)
	}
	if !strings.HasPrefix(job.EligibilityRef, "eligibility:autelig_") || job.AutoAdmit == nil || job.AutoAdmit.EligibilityRef != job.EligibilityRef {
		t.Fatalf("proposal missing typed eligibility ref: %+v", job)
	}

	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Get(job.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"grant:" + grantID,
		job.EligibilityRef,
		first.ReceiptID,
		second.ReceiptID,
		first.RouteID,
		second.RouteID,
		"route_plan:" + job.RoutePlan.ID,
		"router_revision:" + job.RoutePlan.SourceRevision,
		"threshold:min_terminal_samples=2",
		"threshold:min_window_days=1",
		"threshold:max_failures=0",
	}
	for _, want := range wants {
		if !containsDirectorString(w.EvidenceRefs, want) {
			t.Errorf("admitted work missing evidence ref %q: %v", want, w.EvidenceRefs)
		}
	}
	_, events, err := store.Show(job.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	var admission work.Event
	for _, event := range events {
		if event.From == "proposed" && event.To == "admitted" {
			admission = event
		}
	}
	if admission.To == "" {
		t.Fatalf("missing proposed->admitted event: %+v", events)
	}
	for _, want := range wants {
		if !containsDirectorString(admission.Attached, want) {
			t.Errorf("admission event missing proof ref %q: %v", want, admission.Attached)
		}
	}

	// The same run may still suggest other classes, but the first autonomy
	// grant is intentionally class-scoped: review_debt remains preview-only.
	review := findProposalByKind(report, "review_debt")
	if review == nil {
		t.Fatalf("missing review_debt control proposal: %+v", report.Proposals)
	}
	if review.AutoAdmit != nil || review.WorkState != "proposed" {
		t.Fatalf("non-job class received autonomy: %+v", review)
	}
	if _, err := store.Get(review.WorkID); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-job proposal should remain a create-plan preview, err=%v", err)
	}
}

func TestJobHealthEvidenceAutonomyReconsidersExistingProposalAfterNewEvidence(t *testing.T) {
	home := seededDirectorHome(t)
	firstNow := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, firstNow.Add(-72*time.Hour))
	seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, firstNow.Add(-72*time.Hour))
	seedJobHealthAutonomySample(t, home, "reconsider-a", firstNow.Add(-48*time.Hour), "completed", "auto/judge", true)
	seedFailingJobHealth(t, home)

	first, err := Propose(home, Options{Apply: true, Now: firstNow})
	if err != nil {
		t.Fatal(err)
	}
	firstJob := findProposalByKind(first, "job_health")
	if firstJob == nil || firstJob.WorkState != "proposed" || firstJob.AutoAdmitResult == nil || firstJob.AutoAdmitResult.Admitted {
		t.Fatalf("insufficient first pass must leave job_health proposed: %+v", firstJob)
	}

	seedJobHealthAutonomySample(t, home, "reconsider-b", firstNow.Add(24*time.Hour), "completed", "auto/judge", true)
	second, err := Propose(home, Options{Apply: true, Now: firstNow.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	secondJob := findProposalByKind(second, "job_health")
	if secondJob == nil || secondJob.WorkID != firstJob.WorkID || secondJob.WorkState != "admitted" || secondJob.AutoAdmitResult == nil || !secondJob.AutoAdmitResult.Admitted {
		t.Fatalf("new evidence must reconsider and admit the existing proposal: first=%+v second=%+v report=%+v", firstJob, secondJob, second)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(firstJob.WorkID)
	if err != nil || got.State != "admitted" {
		t.Fatalf("reconsidered work must be admitted in place: state=%q err=%v", got.State, err)
	}
}

func TestJobHealthEvidenceAutonomyRejectsTamperedDuplicateActionContext(t *testing.T) {
	home := seededDirectorHome(t)
	firstNow := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, firstNow.Add(-72*time.Hour))
	seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, firstNow.Add(-72*time.Hour))
	seedJobHealthAutonomySample(t, home, "context-a", firstNow.Add(-48*time.Hour), "completed", "auto/judge", true)
	seedFailingJobHealth(t, home)

	first, err := Propose(home, Options{Apply: true, Now: firstNow})
	if err != nil {
		t.Fatal(err)
	}
	firstJob := findProposalByKind(first, "job_health")
	if firstJob == nil || firstJob.WorkState != "proposed" {
		t.Fatalf("insufficient first pass must leave job_health proposed: %+v", firstJob)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Show(firstJob.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := ""
	for _, event := range events {
		for _, ref := range event.Attached {
			if strings.HasPrefix(ref, "action_context_snapshot:") {
				snapshotID = strings.TrimPrefix(ref, "action_context_snapshot:")
			}
		}
	}
	if snapshotID == "" {
		t.Fatalf("first proposal missing action-context snapshot: %+v", events)
	}
	snapshotPath := filepath.Join(home, "work", "action-contexts", snapshotID+".json")
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	actionContext, ok := snapshot["action_context"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing action_context: %+v", snapshot)
	}
	actionContext["action_summary"] = "run stale attacker-selected semantics"
	tampered, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, append(tampered, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	seedJobHealthAutonomySample(t, home, "context-b", firstNow.Add(24*time.Hour), "completed", "auto/judge", true)
	second, err := Propose(home, Options{Apply: true, Now: firstNow.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(firstJob.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "proposed" {
		t.Fatalf("tampered duplicate action context reached %q, want proposed: %+v", got.State, second)
	}
	foundWarning := false
	for _, warning := range second.Warnings {
		foundWarning = foundWarning || strings.Contains(warning, "action context")
	}
	if !foundWarning {
		t.Fatalf("tampered duplicate refusal did not name action context: %+v", second)
	}
}

func TestJobHealthEvidenceAutonomyWritesOnlyAdmissionSurfaces(t *testing.T) {
	home := seededDirectorHome(t)
	now := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
	seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
	seedJobHealthAutonomySample(t, home, "boundary-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
	seedJobHealthAutonomySample(t, home, "boundary-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
	seedFailingJobHealth(t, home)

	before := snapshotHomeTree(t, home)
	report, err := Propose(home, Options{Apply: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	job := findProposalByKind(report, "job_health")
	if job == nil || job.WorkState != "admitted" || job.AutoAdmitResult == nil || !job.AutoAdmitResult.Admitted {
		t.Fatalf("qualified job health must stop at admitted: %+v", job)
	}

	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, events, err := store.Show(job.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "admitted" || w.IDs.TransitionID != "" {
		t.Fatalf("autonomy must stop before execution/closure: %+v", w)
	}
	if _, err := os.Stat(filepath.Join(home, "work", "leases", job.WorkID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admitted suggestion must not acquire a task lease: %v", err)
	}
	for _, event := range events {
		if event.To == "claimed" || event.To == "running" || event.To == "completed" || event.To == "failed" {
			t.Fatalf("suggested work crossed the admission boundary: %+v", event)
		}
	}
	eventFiles, err := os.ReadDir(filepath.Join(home, "events"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range eventFiles {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(home, "events", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var artifact struct {
			SchemaVersion string `json:"schema_version"`
			IDs           struct {
				WorkID string `json:"work_id"`
			} `json:"ids"`
		}
		if json.Unmarshal(data, &artifact) == nil && artifact.IDs.WorkID == job.WorkID &&
			(artifact.SchemaVersion == envelope.SVReceipt || artifact.SchemaVersion == envelope.SVTransition) {
			t.Fatalf("suggested work received execution/closure artifact %s", entry.Name())
		}
	}

	writeSet := diffHomeTree(before, snapshotHomeTree(t, home))
	allowed := []string{
		"artifacts/autonomy-eligibility/",
		"create-plans/",
		"events/",
		"lock/capability-lifecycle-work-admission/", // shared CreatePlan admission serialization
		"projections/director/",
		"work/action-contexts/",
		"work/leases/", // enclosing Director run only; job lease absence is asserted above
		"work/work/",
		"run_attempts/",         // C1: every claim mints a RunAttempt; the enclosing Director run claims, so a run-attempt write is legitimate, not an escape
		"lock/runattempts.lock", // v0.1 compatibility fence for old and new RunAttempt writers
		"lock/runattempts/",     // validated per-attempt companion beneath the compatibility fence
	}
	var escaped []string
	for _, rel := range writeSet {
		if !hasAllowedPrefix(rel, allowed) {
			escaped = append(escaped, rel)
		}
	}
	if len(escaped) > 0 {
		t.Fatalf("evidence autonomy escaped admission/run-proof surfaces: %v\nfull write set: %v", escaped, writeSet)
	}
	for _, prefix := range []string{"artifacts/autonomy-eligibility/", "create-plans/", "events/", "work/work/"} {
		if !hasAllowedPrefixInSet(writeSet, prefix) {
			t.Fatalf("successful autonomy did not exercise %s; write set: %v", prefix, writeSet)
		}
	}
}

func hasAllowedPrefixInSet(paths []string, prefix string) bool {
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func TestJobHealthEvidenceThresholdFailuresRemainProposed(t *testing.T) {
	tests := []struct {
		name        string
		firstAt     time.Duration
		secondAt    time.Duration
		firstOut    string
		secondOut   string
		secondModel string
		secondProof bool
		wantReason  string
	}{
		{name: "n-minus-one", firstAt: -48 * time.Hour, secondAt: 0, firstOut: "completed", wantReason: "terminal samples"},
		{name: "short-window", firstAt: -2 * time.Hour, secondAt: -time.Hour, firstOut: "completed", secondOut: "completed", secondModel: "auto/judge", secondProof: true, wantReason: "window"},
		{name: "failed-receipt", firstAt: -48 * time.Hour, secondAt: -24 * time.Hour, firstOut: "completed", secondOut: "failed", secondModel: "auto/judge", secondProof: true, wantReason: "failures"},
		{name: "tripwire-lane", firstAt: -48 * time.Hour, secondAt: -24 * time.Hour, firstOut: "completed", secondOut: "completed", secondModel: "scout/chat", secondProof: true, wantReason: "tripwire_only"},
		{name: "missing-route-proof", firstAt: -48 * time.Hour, secondAt: -24 * time.Hour, firstOut: "completed", secondOut: "completed", secondModel: "auto/judge", secondProof: false, wantReason: "route evidence"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := seededDirectorHome(t)
			now := directorTestNow.Add(96 * time.Hour)
			seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
			seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
			seedJobHealthAutonomySample(t, home, tc.name+"-a", now.Add(tc.firstAt), tc.firstOut, "auto/judge", true)
			if tc.secondAt != 0 {
				seedJobHealthAutonomySample(t, home, tc.name+"-b", now.Add(tc.secondAt), tc.secondOut, tc.secondModel, tc.secondProof)
			}
			seedFailingJobHealth(t, home)

			report, err := Propose(home, Options{Apply: true, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			job := findProposalByKind(report, "job_health")
			if job == nil {
				t.Fatalf("missing job_health proposal: %+v", report.Proposals)
			}
			if job.WorkState != "proposed" || job.AutoAdmitResult == nil || job.AutoAdmitResult.Admitted {
				t.Fatalf("ineligible evidence escaped proposed: %+v", job)
			}
			if !strings.Contains(strings.ToLower(job.AutoAdmitError), strings.ToLower(tc.wantReason)) {
				t.Fatalf("reason %q missing %q: %+v", job.AutoAdmitError, tc.wantReason, job)
			}
			store, _ := work.Open(home)
			got, err := store.Get(job.WorkID)
			if err != nil || got.State != "proposed" {
				t.Fatalf("ineligible work must remain proposed: state=%q err=%v", got.State, err)
			}
		})
	}
}

func TestJobHealthWrongOrRevokedGrantRemainsProposed(t *testing.T) {
	for _, mode := range []string{"wrong-class", "revoked", "revoked-needs-review"} {
		t.Run(mode, func(t *testing.T) {
			home := seededDirectorHome(t)
			now := directorTestNow.Add(96 * time.Hour)
			seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
			if mode == "wrong-class" {
				seedJobHealthAutonomyGrant(t, home, "review_debt", 2, 1, 0, now.Add(-72*time.Hour))
			} else {
				grantID := seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
				store, err := mem.Open(home)
				if err != nil {
					t.Fatal(err)
				}
				revocationID := grantID + "-revoked"
				revocation, err := decisions.Record(store, home, decisions.RecordInput{
					Kind:       decisions.KindGrant,
					ID:         revocationID,
					Title:      "Revoke job health autonomy",
					ReviewedBy: "operator:test",
					UndoOf:     grantID,
					Now:        now.Add(-time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "revoked-needs-review" {
					captured, err := store.Capture([]mem.Candidate{{
						Entity:         "decision:" + revocationID,
						Predicate:      "decided",
						Object:         "generic evidence disputes the operator revocation",
						Confidence:     1,
						Derivation:     "deterministic",
						ClaimType:      "decision",
						Qualifiers:     map[string]string{"kind": decisions.KindDoctrine, "source": "generic-memory", "decision_id": revocationID, "lifecycle_status": decisions.StatusAccepted},
						SourceTraceRef: "generic-memory:test",
						SnippetHash:    "revocation-needs-review",
						Visibility:     "operational",
					}}, now.Add(-30*time.Minute))
					if err != nil || len(captured) != 1 {
						t.Fatalf("capture generic contradiction: candidates=%+v err=%v", captured, err)
					}
					sweep, err := store.SynthesisSweep([]string{"contradict"}, now.Add(-15*time.Minute))
					if err != nil {
						t.Fatal(err)
					}
					if len(sweep.ReopenedClaims) != 1 || sweep.ReopenedClaims[0].ClaimID != revocation.Import.ClaimID {
						t.Fatalf("revocation contradiction was not preserved for review: %+v", sweep.ReopenedClaims)
					}
				}
			}
			seedJobHealthAutonomySample(t, home, mode+"-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
			seedJobHealthAutonomySample(t, home, mode+"-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
			seedFailingJobHealth(t, home)

			report, err := Propose(home, Options{Apply: true, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			job := findProposalByKind(report, "job_health")
			if job == nil || job.WorkState != "proposed" || job.AutoAdmit != nil || job.EligibilityRef != "" {
				t.Fatalf("%s grant escaped proposed: %+v", mode, job)
			}
			if !strings.Contains(job.AutoAdmitError, "no active class-scoped autonomy grant") {
				t.Fatalf("%s refusal did not name missing active grant: %+v", mode, job)
			}
		})
	}
}

func TestJobHealthAutonomyRequiresExactlyOneTypedGrant(t *testing.T) {
	for _, mode := range []string{"ruling-without-thresholds", "multiple-grants"} {
		t.Run(mode, func(t *testing.T) {
			home := seededDirectorHome(t)
			now := directorTestNow.Add(96 * time.Hour)
			seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ruling-without-thresholds" {
				if _, err := decisions.Record(store, home, decisions.RecordInput{
					Kind:       decisions.KindRuling,
					ID:         "harness-evidence-autonomy-test-ruling",
					Title:      "Harnesses may decide after enough evidence",
					ReviewedBy: "operator:test",
					Scope:      decisions.AutonomyGrantScope,
					Now:        now.Add(-72 * time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
				if _, err := decisions.Record(store, home, decisions.RecordInput{
					Kind:               decisions.KindGrant,
					ID:                 "second-job-health-autonomy-grant",
					Title:              "Second conflicting job-health autonomy grant",
					ReviewedBy:         "operator:test",
					Scope:              decisions.AutonomyGrantScope,
					AutoAdmitClass:     "job_health",
					MinTerminalSamples: 3,
					MinWindowDays:      2,
					MaxFailures:        0,
					Now:                now.Add(-71 * time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			seedJobHealthAutonomySample(t, home, mode+"-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
			seedJobHealthAutonomySample(t, home, mode+"-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
			seedFailingJobHealth(t, home)

			report, err := Propose(home, Options{Apply: true, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			job := findProposalByKind(report, "job_health")
			if job == nil || job.WorkState != "proposed" || job.AutoAdmit != nil || job.EligibilityRef != "" {
				t.Fatalf("%s escaped typed-grant cardinality gate: %+v", mode, job)
			}
			want := "no active class-scoped autonomy grant"
			if mode == "multiple-grants" {
				want = "ambiguous"
			}
			if !strings.Contains(job.AutoAdmitError, want) {
				t.Fatalf("%s refusal missing %q: %+v", mode, want, job)
			}
		})
	}
}

func TestJobHealthDuplicateRouteEvidenceIDIsAmbiguous(t *testing.T) {
	home := seededDirectorHome(t)
	now := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
	seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
	seedJobHealthAutonomySample(t, home, "duplicate-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
	second := seedJobHealthAutonomySample(t, home, "duplicate-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
	ledgerPath := routeevidence.ExecutionRouteEvidenceLedgerPath(home)
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var duplicate []byte
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"id":"`+second.RouteID+`"`) {
			duplicate = []byte(line + "\n")
		}
	}
	if len(duplicate) == 0 {
		t.Fatalf("route row %s not found", second.RouteID)
	}
	file, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(duplicate); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	seedFailingJobHealth(t, home)
	report, err := Propose(home, Options{Apply: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	job := findProposalByKind(report, "job_health")
	if job == nil || job.WorkState != "proposed" || !strings.Contains(job.AutoAdmitError, "ambiguous") {
		t.Fatalf("duplicate route id did not fail closed: %+v", job)
	}
}

func containsDirectorString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
