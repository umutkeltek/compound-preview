package product

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

func b2Fixture(t *testing.T) (string, ContentDraft, time.Time, string) {
	t.Helper()
	t.Setenv(DistributionExperimentEnv, "1")
	h := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := home.Init(h, "test", now); err != nil {
		t.Fatal(err)
	}
	d := recordQueueDraft(t, h, "reviewed synthetic body", now.Add(time.Hour), now)
	review := approveReview(t, h, "cqr_bound", d.ID)
	if report, err := ApplyContentQueueReview(h, review, "", now); err != nil || report.Counts.Approved != 1 {
		t.Fatalf("approval: %+v %v", report, err)
	}
	writeSocialRegistry(t, h, []SocialAccount{{ID: "account-synth", ProductID: d.ProductID, Platform: d.Platform, Transport: "fixture", Status: "active", PostizIntegrationID: "integration-synth"}})
	marker := filepath.Join(h, "invoked")
	exe := filepath.Join(h, "transport")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\ncat > \"$1\"\nprintf '{\"id\":\"untyped\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"argv": []string{exe, marker}, "content_contract": act.ContentTransportContract})
	dir := filepath.Join(h, "actuator", "transports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	grant := filepath.Join(h, "grant.yaml")
	body := "generated_at: \"" + now.Format(time.RFC3339) + "\"\nauthority:\n  decision_ref: \"grant:fixture\"\n  reviewed_by: \"operator:fixture\"\ngrants:\n  - grant_id: grant_content\n    capabilities: [\"" + ContentPostCapability + "\"]\n    target_scope: \"actuator/transports/fixture.json\"\n    mutation_class: external_side_effect\n    reversibility_ceiling: irreversible\n    blast_radius: local\n"
	if err := os.WriteFile(grant, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(h, grant, now); err != nil {
		t.Fatal(err)
	}
	return h, d, now, marker
}

func TestB2TypedObservationBindsEveryIdentityAndPositivePublication(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	req, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	digest := act.ContentTransportDigest(b)
	valid := ContentTransportObservation{SchemaVersion: ContentObservationSchemaVersion, Operation: "observe", AttemptID: req.AttemptID, IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, PackageDigest: req.PackageDigest, Transport: req.Transport, AccountID: req.AccountID, IntegrationID: req.IntegrationID, Binding: req.Binding, ObjectType: "publication", Status: "published", RemoteID: "post-synth", PublishedAt: now.Add(-time.Second).Format(time.RFC3339)}
	for _, field := range []string{"schema", "operation", "attempt", "idempotency", "request", "package", "transport", "account", "integration", "binding", "object", "time", "remote", "status"} {
		t.Run(field, func(t *testing.T) {
			o := valid
			switch field {
			case "schema":
				o.SchemaVersion = "legacy"
			case "operation":
				o.Operation = "schedule"
			case "attempt":
				o.AttemptID = "another"
			case "idempotency":
				o.IdempotencyKey = "another"
			case "request":
				o.RequestDigest = "another"
			case "package":
				o.PackageDigest = "another"
			case "transport":
				o.Transport = "another"
			case "account":
				o.AccountID = "another"
			case "integration":
				o.IntegrationID = "another"
			case "binding":
				o.Binding.ExecutableDigest = "another"
			case "object":
				o.ObjectType = "schedule"
			case "time":
				o.PublishedAt = now.Add(time.Second).Format(time.RFC3339)
			case "remote":
				o.RemoteID = ""
			case "status":
				o.Status = "ok"
			}
			data, _ := json.Marshal(o)
			if _, err := parseBoundContentObservation(data, req, "observe", digest, now); err == nil {
				t.Fatal("mismatched evidence accepted")
			}
		})
	}
	for _, status := range []string{"processing", "unknown", "rejected", "cancelled", "error", "scheduled", "published"} {
		o := valid
		o.Status = status
		if status != "published" {
			o.ObjectType = "attempt"
			o.RemoteID = ""
			o.PublishedAt = ""
		}
		if status == "scheduled" {
			o.ObjectType = "schedule"
			o.ScheduleID = "schedule-synth"
			o.ScheduledAt = req.ScheduleAt
		}
		data, _ := json.Marshal(o)
		if _, err := parseBoundContentObservation(data, req, "observe", digest, now); err != nil {
			t.Fatalf("typed %s: %v", status, err)
		}
	}
	data, _ := json.Marshal(valid)
	for _, bad := range [][]byte{[]byte(`{"id":"job"}`), append(append([]byte{}, data...), []byte(` {`)...), append(append([]byte{}, data...), []byte(` {}`)...)} {
		if _, err := parseBoundContentObservation(bad, req, "observe", digest, now); err == nil {
			t.Fatalf("malformed evidence accepted: %s", bad)
		}
	}
}

func TestB2ReviewDigestRefusesPackageChangeAndLegacyBareIDs(t *testing.T) {
	h := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(h, "test", now); err != nil {
		t.Fatal(err)
	}
	d := recordQueueDraft(t, h, "original body", now.Add(time.Hour), now)
	review := approveReview(t, h, "cqr_exact", d.ID)
	r, current, err := readContentOutbound(h, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Media = []ContentDraftMedia{{Kind: "image", Ref: current.BodyRef, AltText: "unreviewed alt", MIMEType: "image/png"}}
	if res, err := candidates.ReplaceCandidateIfCurrent(h, r, current); err != nil || !res.Replaced {
		t.Fatal(err)
	}
	if report, err := ApplyContentQueueReview(h, review, "", now); err != nil || report.Counts.Approved != 0 {
		t.Fatalf("changed package approved: %+v %v", report, err)
	}
	for _, body := range []string{`{"id":"job"}`, `{"id":"job","date":"` + now.Add(time.Minute).Format(time.RFC3339) + `"}`} {
		if _, err := parseContentPostImage([]byte(body), now); err == nil {
			t.Fatalf("bare identity accepted: %s", body)
		}
	}
}

func TestB2InterruptedApprovalRecoversFrozenIdentityAfterExpiry(t *testing.T) {
	h := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(h, "test", now); err != nil {
		t.Fatal(err)
	}
	d := recordQueueDraft(t, h, "exact approved body", now.Add(time.Minute), now)
	r := approveReview(t, h, "cqr_recover", d.ID)
	p := contentPostAuthorizationPath(h, d.ID)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	report, err := ApplyContentQueueReview(h, r, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Approved != 0 {
		t.Fatal("authorization write failure hidden")
	}
	frozen, err := ReadContentDraft(h, d.ID)
	if err != nil || frozen.Approval == nil {
		t.Fatalf("frozen recovery witness missing: %+v %v", frozen, err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	resumeBody, _ := json.Marshal(struct {
		Home   string
		Review ContentQueueReview
		Now    time.Time
	}{h, r, now.Add(2 * time.Minute)})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestB2ApprovalResumeProcess$")
	cmd.Env = append(os.Environ(), "COMPOUND_B2_APPROVAL_RESUME="+string(resumeBody))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh approval resume: %v %s", err, output)
	}
	auth, err := LoadContentPostAuthorization(h, d.ID)
	if err != nil || auth.ActionIntentID != frozen.Approval.Authorization.ActionIntentID || auth.PolicyDecisionID != frozen.Approval.Authorization.PolicyDecisionID {
		t.Fatalf("identity changed: %+v %v", auth, err)
	}
	if _, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", now.Add(time.Hour), now.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("recovery authorized stale send: %v", err)
	}
	r.ID = "cqr_other"
	if report, err := ApplyContentQueueReview(h, r, "", now); err != nil || report.Counts.Approved != 0 {
		t.Fatalf("different approval consumed witness: %+v %v", report, err)
	}
}

func TestB2ApprovalResumeProcess(t *testing.T) {
	body := os.Getenv("COMPOUND_B2_APPROVAL_RESUME")
	if body == "" {
		return
	}
	var input struct {
		Home   string
		Review ContentQueueReview
		Now    time.Time
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		t.Fatal(err)
	}
	if report, err := ApplyContentQueueReview(input.Home, input.Review, "", input.Now); err != nil || report.Counts.Approved != 1 {
		t.Fatalf("resume: %+v %v", report, err)
	}
}

func TestB2ConcurrentCreateCannotPassOwnedReservation(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	old := contentPostExecute
	t.Cleanup(func() { contentPostExecute = old })
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	contentPostExecute = func(*home.ArtifactStore, act.Adapter, envelope.ActionIntent, envelope.PolicyDecision, []byte, time.Time) (act.Result, error) {
		close(started)
		<-release
		return act.Result{}, errors.New("transport outcome unknown")
	}
	go func() {
		_, err := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(time.Minute), true, false, now)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("did not reach adapter: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("adapter not reached")
	}
	_, secondErr := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(2*time.Minute), true, false, now)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("unknown outcome hidden")
	}
	if secondErr == nil || !strings.Contains(secondErr.Error(), "already owns outbound attempt") {
		t.Fatalf("concurrent create was not refused: %v", secondErr)
	}
}

func TestB2ApprovedPackageCanBeRevokedBeforeReservation(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	revoked, err := RejectContentDraft(h, d.ID, now)
	if err != nil || revoked.State != ContentDraftStateRejected || revoked.Approval == nil {
		t.Fatalf("revoke lost state or witness: %+v %v", revoked, err)
	}
	if _, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", now.Add(time.Minute), now); err == nil {
		t.Fatal("revoked package still publishable")
	}
	r := approveReview(t, h, "cqr_bound", d.ID)
	if report, err := ApplyContentQueueReview(h, r, "", now); err != nil || report.Counts.Approved != 0 {
		t.Fatalf("frozen approval revived revoked package: %+v %v", report, err)
	}
	plan, err := BuildContentQueuePlan(h, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range plan.Rows {
		if row.DraftID == d.ID {
			t.Fatal("revoked package advertised as approved")
		}
	}
}

func TestB2QueueProjectsRecoveryBeforeReviewedShortcut(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	ready, err := BuildContentQueuePlan(h, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready.Rows) != 0 || ready.ReviewableCount != 0 {
		t.Fatal("durably authorized package remains in review queue")
	}
	if _, err := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(time.Minute), true, false, now); err == nil {
		t.Fatal("untyped result accepted")
	}
	pending, err := BuildContentQueuePlan(h, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Rows) != 1 || pending.ReviewableCount != 0 || pending.ID == ready.ID || pending.Status != "reconciliation_required" {
		t.Fatalf("pending projection: %+v", pending)
	}
	paths, err := RecordContentQueuePlan(h, pending)
	if err != nil {
		t.Fatal(err)
	}
	reviewedPath := filepath.Join(h, "fixture.reviewed.json")
	if err := os.WriteFile(reviewedPath, []byte(`{"id":"reviewed","status":"reviewed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	item := reviewartifact.QueueItem{JSONPath: paths.PlanPath, ExtraPaths: []string{reviewedPath}}
	annotateContentQueueItem(&item)
	if item.Readiness != "needs_review" || !strings.Contains(item.NextAction, "--draft "+d.ID) || !strings.Contains(item.NextAction, "--reconcile") {
		t.Fatalf("reviewed shortcut hid recovery: %+v", item)
	}
	if strings.Contains(RenderContentQueueReviewYAML(pending), "TODO_verdict") {
		t.Fatal("owned attempt asks for new approval")
	}
}

func TestB2PreReservationWriteFailureRetriesOriginalIntent(t *testing.T) {
	h, d, now, marker := b2Fixture(t)
	old := replaceContentOutbound
	t.Cleanup(func() { replaceContentOutbound = old })
	replaceContentOutbound = func(string, candidates.Record, any) (candidates.ReplaceCandidateResult, error) {
		return candidates.ReplaceCandidateResult{}, errors.New("synthetic reservation persistence failure")
	}
	at := now.Add(time.Minute)
	if _, err := ExecuteContentPost(h, d.ID, "fixture", "", at, true, false, now); err == nil {
		t.Fatal("write failure hidden")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("pre-reservation failure invoked transport")
	}
	current, _ := ReadContentDraft(h, d.ID)
	if current.Outbound != nil {
		t.Fatal("failed reservation became owner")
	}
	replaceContentOutbound = old
	// Same request at a later wall clock must pass immutable intent recording,
	// reserve once and reach the transport. Its untyped result remains unknown.
	if _, err := ExecuteContentPost(h, d.ID, "fixture", "", at, true, false, now.Add(2*time.Second)); err == nil {
		t.Fatal("untyped response accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("retry trapped behind old intent: %v", err)
	}
	current, _ = ReadContentDraft(h, d.ID)
	if current.Outbound == nil {
		t.Fatal("retry did not retain attempt")
	}
}

func TestB2ScheduleCannotOutliveReviewedPackage(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	if _, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", now.Add(time.Hour), now); err == nil {
		t.Fatal("schedule at expiry accepted")
	}
	req, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", now.Add(time.Minute), now)
	if err != nil || req.ExpiresAt != d.ExpiresAt {
		t.Fatalf("wire expiry lost: %+v %v", req, err)
	}
}

func TestB2ObservationCASPreservesNewerCandidate(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	if _, err := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(time.Minute), true, false, now); err == nil {
		t.Fatal("untyped result accepted")
	}
	_, current, err := readContentOutbound(h, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := home.NewArtifactStore(h)
	body, err := s.Get(current.Outbound.RequestRef)
	if err != nil {
		t.Fatal(err)
	}
	var req ContentPostTransportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	o := ContentTransportObservation{SchemaVersion: ContentObservationSchemaVersion, Operation: "observe", AttemptID: req.AttemptID, IdempotencyKey: req.IdempotencyKey, RequestDigest: current.Outbound.RequestDigest, PackageDigest: req.PackageDigest, Transport: req.Transport, AccountID: req.AccountID, IntegrationID: req.IntegrationID, Binding: req.Binding, ObjectType: "publication", Status: "published", RemoteID: "post-synth", PublishedAt: now.Format(time.RFC3339)}
	observation, _ := json.Marshal(o)
	old := replaceContentOutbound
	t.Cleanup(func() { replaceContentOutbound = old })
	var newer []byte
	replaceContentOutbound = func(path string, r candidates.Record, value any) (candidates.ReplaceCandidateResult, error) {
		_, latest, err := readContentOutbound(h, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		latest.Warnings = append(latest.Warnings, "newer owner generation")
		if result, err := old(path, r, latest); err != nil || !result.Replaced {
			t.Fatalf("concurrent writer: %+v %v", result, err)
		}
		saved, _ := candidates.ReadRecord(h, candidates.ContentDraft, d.ID)
		newer = saved.Body
		return old(path, r, value)
	}
	if _, err := recordBoundContentObservation(h, d.ID, req, "observe", observation, "", now); err == nil {
		t.Fatal("stale observation overwrote newer generation")
	}
	after, _ := candidates.ReadRecord(h, candidates.ContentDraft, d.ID)
	if string(after.Body) != string(newer) {
		t.Fatal("newer generation changed")
	}
}

func TestB2DriftBetweenReservationAndProcessStartIsRefused(t *testing.T) {
	for _, kind := range []string{"body", "alt", "expiry", "account", "config", "executable"} {
		t.Run(kind, func(t *testing.T) {
			h, d, now, marker := b2Fixture(t)
			old := contentPostExecute
			t.Cleanup(func() { contentPostExecute = old })
			contentPostExecute = func(s *home.ArtifactStore, a act.Adapter, ai envelope.ActionIntent, pd envelope.PolicyDecision, b []byte, n time.Time) (act.Result, error) {
				switch kind {
				case "body", "alt", "expiry":
					r, current, err := readContentOutbound(h, d.ID)
					if err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "body":
						current.BodyRef, _ = s.Put([]byte("new unreviewed body"))
					case "alt":
						current.Media = []ContentDraftMedia{{Ref: current.BodyRef, Kind: "image", AltText: "unreviewed alt"}}
					case "expiry":
						current.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339)
					}
					if res, err := candidates.ReplaceCandidateIfCurrent(h, r, current); err != nil || !res.Replaced {
						t.Fatalf("drift: %+v %v", res, err)
					}
				case "account":
					writeSocialRegistry(t, h, []SocialAccount{{ID: "new-account", ProductID: d.ProductID, Platform: d.Platform, Transport: "fixture", Status: "active", PostizIntegrationID: "new-integration"}})
				case "config":
					p := filepath.Join(h, "actuator", "transports", "fixture.json")
					data, _ := os.ReadFile(p)
					if err := os.WriteFile(p, append(data, ' '), 0600); err != nil {
						t.Fatal(err)
					}
				case "executable":
					if err := os.WriteFile(filepath.Join(h, "transport"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return old(s, a, ai, pd, b, n)
			}
			_, err := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(time.Minute), true, false, now)
			if err == nil {
				t.Fatal("drift accepted")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("transport started: %v", err)
			}
			current, _ := ReadContentDraft(h, d.ID)
			if current.Outbound == nil {
				t.Fatal("reservation lost")
			}
		})
	}
}

func TestB2ReservationSurvivesCrashAndLegacyDoorsCannotOverwrite(t *testing.T) {
	h, d, now, _ := b2Fixture(t)
	old := contentPostExecute
	t.Cleanup(func() { contentPostExecute = old })
	contentPostExecute = func(*home.ArtifactStore, act.Adapter, envelope.ActionIntent, envelope.PolicyDecision, []byte, time.Time) (act.Result, error) {
		return act.Result{}, errors.New("synthetic process loss immediately after reservation")
	}
	if _, err := ExecuteContentPost(h, d.ID, "fixture", "", now.Add(time.Minute), true, false, now); err == nil {
		t.Fatal("crash hidden")
	}
	for _, at := range []time.Time{now.Add(time.Minute), now.Add(2 * time.Minute)} {
		if _, _, err := BuildContentPostTransportRequest(h, d.ID, "fixture", at, now); err == nil {
			t.Fatal("reservation allowed another create")
		}
	}
	before, _ := candidates.ReadRecord(h, candidates.ContentDraft, d.ID)
	if _, err := RejectContentDraft(h, d.ID, now); err == nil {
		t.Fatal("reject overwrote attempt")
	}
	if _, err := MarkContentDraftPosted(h, d.ID, ContentDraftPostInput{RemoteID: "invented"}, now); err == nil {
		t.Fatal("legacy publication bypass")
	}
	if _, err := MarkContentDraftScheduled(h, d.ID, ContentDraftScheduleInput{ScheduleID: "invented", ScheduledAt: now.Format(time.RFC3339)}, now); err == nil {
		t.Fatal("legacy scheduling bypass")
	}
	after, _ := candidates.ReadRecord(h, candidates.ContentDraft, d.ID)
	if string(before.Body) != string(after.Body) {
		t.Fatal("refusal mutated owned attempt")
	}
}
