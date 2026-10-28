package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
)

func approvePrelinkedPublicationTTL(t *testing.T, p *privatePublisherFixture, request PrivateSourcePublishRequest, ttl time.Duration) policy.ExactActionApproval {
	t.Helper()
	approvals := policy.NewExactActionApprovalStore(p.f.store.home)
	cfg, _, err := policy.LoadConfig(p.f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := approvals.Plan(request.Action.ID, cfg, ttl, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	approval, _, err := approvals.Approve(plan.ID, "operator:synthetic-reviewer", cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func prelinkedPrivatePublisher(t *testing.T) (*privatePublisherFixture, PrivateSourcePublishRequest, envelope.PolicyDecision) {
	t.Helper()
	p := newPrivatePublisherFixture(t, false)
	request := p.plan(t)
	cfg, _, err := policy.LoadConfig(p.f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	baselineAt := time.Now()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-pre-admission", Capability: request.Action.Capability,
		MutationClass: request.Action.MutationClass, Reversibility: request.Action.Reversibility,
		BlastRadius: request.Action.BlastRadius, TargetPath: request.Repo,
		Destination: request.Action.Destination, ExpiresAt: baselineAt.Add(time.Hour).Format(time.RFC3339Nano),
	})
	baseline := policy.Evaluate(request.Action, cfg, baselineAt)
	if !policy.Allowed(baseline) {
		t.Fatalf("fixture pre-admission policy must allow publication: %+v", baseline)
	}
	if _, err := p.f.store.RecordAction(p.f.workID, request.Action, baseline, baselineAt); err != nil {
		t.Fatal(err)
	}
	return p, request, baseline
}

func TestPrivateSourcePublisherRecoversSameActionPreAdmissionPolicyWithoutRepush(t *testing.T) {
	p, request, baseline := prelinkedPrivatePublisher(t)
	approvePrivatePublication(t, p, request)
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || result.Status != "published" || result.Action == nil || p.pushes != 1 {
		t.Fatalf("pre-admitted publication did not complete: status=%s error=%v pushes=%d", result.Status, err, p.pushes)
	}
	if result.Action.Decision.ID == baseline.ID {
		t.Fatal("baseline policy replaced exact one-use authorization")
	}
	replayed, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || replayed.Action == nil || !replayed.Action.Replayed || p.pushes != 1 {
		t.Fatalf("pre-admitted publication replay changed its effect: error=%v pushes=%d", err, p.pushes)
	}
}

func interruptedPrelinkedPrivatePublisher(t *testing.T, boundary string) (*privatePublisherFixture, PrivateSourcePublishRequest, envelope.PolicyDecision, ReceiptedActionInput, time.Time) {
	t.Helper()
	p, request, baseline := prelinkedPrivatePublisher(t)
	approvePrivatePublication(t, p, request)
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == boundary {
			return errors.New("synthetic post-fsync interruption")
		}
		return previous(stage)
	}
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	receiptedActionAfterWrite = previous
	if err == nil || p.pushes != 1 {
		t.Fatalf("interruption did not retain one completed effect: status=%s error=%v pushes=%d", result.Status, err, p.pushes)
	}
	in, authorizedAt, present, err := p.f.store.ReadReceiptedActionReservationInput(p.f.workID, request.Action.ID)
	if err != nil || !present || in.OutcomePending {
		t.Fatalf("interruption lost sealed pending authority: present=%t error=%v", present, err)
	}
	return p, request, baseline, in, authorizedAt
}

func TestPrivateSourcePublisherPrelinkCompletionSurvivesInterruptionAndExpiredApproval(t *testing.T) {
	for _, boundary := range []string{"reservation_outcome", "pending", "receipt", "transition", "work", "event", "pending_clear"} {
		t.Run(boundary, func(t *testing.T) {
			p, request, baseline, in, authorizedAt := interruptedPrelinkedPrivatePublisher(t, boundary)
			baselineRecord, err := p.f.store.events.Read(baseline.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := newReceiptedActionReservation(p.f.workID, in, authorizedAt)
			if err != nil {
				t.Fatal(err)
			}
			expiresAt, err := time.Parse(time.RFC3339Nano, in.PreEvaluatedDecision.ExpiresAt)
			if err != nil {
				t.Fatal(err)
			}
			// Reopen the native owner; all recovery authority must come from disk.
			p.f.store, err = Open(p.f.store.home)
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, expiresAt.Add(time.Hour))
			if err != nil || result.Action == nil || result.Status != "receipted" || p.pushes != 1 {
				t.Fatalf("sealed completion did not survive expiry/interruption: status=%s error=%v pushes=%d", result.Status, err, p.pushes)
			}
			expectedProofs := mergeUnique(in.ProofRefs, in.Action.EvidenceRefs, in.BeforeRefs, in.AfterRefs, []string{"work:" + p.f.workID})
			if !reflect.DeepEqual(result.Action.Receipt.ProofRefs, expectedProofs) || result.Action.Decision.ID != in.PreEvaluatedDecision.ID {
				t.Fatal("recovery replaced original observation or exact decision")
			}
			completion, present, _, err := p.f.store.preflightReceiptedActionCompletion(p.f.workID, request.Action.ID)
			if err != nil || !present || !receiptedActionReservationEqual(completion.reservation(), expected) {
				t.Fatalf("recovery changed original reservation/input digests: %v", err)
			}
			after, err := p.f.store.events.Read(baseline.ID)
			if err != nil || !bytes.Equal(after.Bytes, baselineRecord.Bytes) {
				t.Fatal("recovery changed immutable baseline policy history")
			}
			replay, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, expiresAt.Add(2*time.Hour))
			if err != nil || replay.Action == nil || !replay.Action.Replayed || p.pushes != 1 {
				t.Fatalf("completed transaction was not idempotent: %v pushes=%d", err, p.pushes)
			}
		})
	}
}

func TestPrivateSourcePublisherPrelinkRecoveryIsNotGenericPolicyRelinking(t *testing.T) {
	p, request, _, in, authorizedAt := interruptedPrelinkedPrivatePublisher(t, "reservation_outcome")
	if _, err := p.f.store.RecordReservedReceiptedAction(p.f.workID, in, authorizedAt); err == nil || !strings.Contains(err.Error(), "latest action policy differs") {
		t.Fatalf("generic reservation receiver acquired private publisher authority: %v", err)
	}
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || result.Action == nil || p.pushes != 1 {
		t.Fatalf("typed owner could not finish unchanged pending transaction: %v", err)
	}
}

func TestPrivateSourcePublisherOrdinaryHistoricalReplayKeepsGenericSemantics(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	request := p.plan(t)
	approvePrivatePublication(t, p, request)
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || result.Action == nil || p.pushes != 1 {
		t.Fatalf("ordinary publication failed: %v", err)
	}
	if _, err := p.f.store.Advance(p.f.workID, AdvanceInput{To: "verifying", Actor: proofActorAlpha}, time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || replay.Action == nil || !replay.Action.Replayed || replay.Action.Receipt.ID != result.Action.Receipt.ID || p.pushes != 1 {
		t.Fatalf("ordinary historical replay changed: %v pushes=%d", err, p.pushes)
	}
}

func TestPrivateSourcePublisherPrelinkRejectsDriftBeforeApprovalConsumptionAndPush(t *testing.T) {
	for _, drift := range []string{"work-preimage", "disappeared-action", "disappeared-action-and-policy", "duplicate", "foreign-action"} {
		t.Run(drift, func(t *testing.T) {
			p, request, _ := prelinkedPrivatePublisher(t)
			approval := approvePrelinkedPublicationTTL(t, p, request, 5*time.Minute)
			w, err := p.f.store.Get(p.f.workID)
			if err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "work-preimage":
				w.VerificationIDs = append(w.VerificationIDs, "proof-check:pfrs_unapproved")
			case "disappeared-action", "disappeared-action-and-policy":
				w.ActionIntentIDs = w.ActionIntentIDs[:len(w.ActionIntentIDs)-1]
				if drift == "disappeared-action-and-policy" {
					w.PolicyDecisionID = ""
				}
			case "duplicate":
				w.ActionIntentIDs = append(w.ActionIntentIDs, request.Action.ID)
			case "foreign-action":
				w.ActionIntentIDs = append(w.ActionIntentIDs, "act_foreign")
			}
			if err := p.f.store.writeJSON(p.f.store.workPath(w.ID), w); err != nil {
				t.Fatal(err)
			}
			before, err := workEnvelopeSHA256(w)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now()); err == nil || p.pushes != 0 {
				t.Fatalf("unsafe first publication accepted: %v pushes=%d", err, p.pushes)
			}
			status, err := policy.NewExactActionApprovalStore(p.f.store.home).Check(approval.ID, time.Now())
			if err != nil || status.ConsumedAt != "" {
				t.Fatalf("unsafe prelink consumed approval: %+v %v", status, err)
			}
			_, _, reserved, err := p.f.store.ReadReceiptedActionReservationInput(w.ID, request.Action.ID)
			if err != nil || reserved {
				t.Fatalf("unsafe prelink reserved publication: %t %v", reserved, err)
			}
			after, err := p.f.store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterDigest, err := workEnvelopeSHA256(after)
			if err != nil || afterDigest != before {
				t.Fatal("refused publication changed Work")
			}
		})
	}
}

func TestPrivateSourcePublisherPrelinkCompletesAfterActualApprovalExpiry(t *testing.T) {
	p, request, _ := prelinkedPrivatePublisher(t)
	approval := approvePrelinkedPublicationTTL(t, p, request, 10*time.Second)
	previous := receiptedActionAfterWrite
	receiptedActionAfterWrite = func(stage string) error {
		if stage == "reservation_outcome" {
			return errors.New("synthetic sealed-outcome interruption")
		}
		return previous(stage)
	}
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	receiptedActionAfterWrite = previous
	if err == nil || p.pushes != 1 {
		t.Fatalf("publication did not stop after its first sealed effect: %s %v pushes=%d", result.Status, err, p.pushes)
	}
	in, authorizedAt, present, err := p.f.store.ReadReceiptedActionReservationInput(p.f.workID, request.Action.ID)
	if err != nil || !present || in.OutcomePending {
		t.Fatalf("missing sealed original authority: %v", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !authorizedAt.Before(expiresAt) || !time.Now().Before(expiresAt) {
		t.Fatal("fixture did not seal publication while approval was live")
	}
	// This bounded timer expires the actual store grant, not only an API clock.
	timer := time.NewTimer(time.Until(expiresAt) + 20*time.Millisecond)
	defer timer.Stop()
	<-timer.C
	status, err := policy.NewExactActionApprovalStore(p.f.store.home).Check(approval.ID, time.Now())
	if err != nil || time.Now().Before(expiresAt) || status.ConsumedAt == "" {
		t.Fatalf("fixture did not reach actual consumed-grant expiry: %+v %v", status, err)
	}
	p.f.store, err = Open(p.f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	result, err = p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || result.Action == nil || result.Action.Decision.ID != in.PreEvaluatedDecision.ID || p.pushes != 1 {
		t.Fatalf("expired sealed publication did not recover original authority: %v pushes=%d", err, p.pushes)
	}
	if err := p.f.store.recheckPrivatePublication(request, *in.PreEvaluatedDecision, time.Now()); err == nil {
		t.Fatal("first-effect authorization remained live after actual expiry")
	}
}

func TestPrivateSourcePublisherPrelinkAuthenticatesObservationAndSealedOutcome(t *testing.T) {
	for _, drift := range []string{"foreign-action", "foreign-source", "unconfirmed-remote", "unconfirmed-tracking", "foreign-destination", "observation-before-authorization", "unexpected-json-field", "summary", "before", "after", "extra-proof"} {
		t.Run(drift, func(t *testing.T) {
			p, request, _, in, authorizedAt := interruptedPrelinkedPrivatePublisher(t, "reservation_outcome")
			w, err := p.f.store.Get(p.f.workID)
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := home.NewArtifactStore(p.f.store.home)
			if err != nil {
				t.Fatal(err)
			}
			body, err := artifacts.GetPrivate(strings.TrimPrefix(in.ProofRefs[1], "evidence:"))
			if err != nil {
				t.Fatal(err)
			}
			var observation privatePublicationObservation
			if err := json.Unmarshal(body, &observation); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "foreign-action":
				observation.ActionID = "act_foreign"
			case "foreign-source":
				observation.RemoteSHA = strings.Repeat("b", 40)
			case "unconfirmed-remote":
				observation.RemoteConfirmed = false
			case "unconfirmed-tracking":
				observation.TrackingConfirmed = false
			case "foreign-destination":
				observation.Destination.FullName = "other-owner/other-repo"
			case "observation-before-authorization":
				observation.ObservedAt = authorizedAt.Add(-time.Second).Format(time.RFC3339Nano)
			case "summary":
				in.Summary = "changed sealed summary"
			case "before":
				in.BeforeRefs = []string{"git:other-owner/other-repo@" + request.ExpectedRemoteSHA}
			case "after":
				in.AfterRefs = []string{"git:other-owner/other-repo@" + request.Source.HeadSHA}
			case "extra-proof":
				in.ProofRefs = append(in.ProofRefs, "evidence:unapproved")
			}
			body, err = json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			if drift == "unexpected-json-field" {
				body = append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"unapproved":true}`)...)
			}
			ref, err := artifacts.PutPrivate(body)
			if err != nil {
				t.Fatal(err)
			}
			in.ProofRefs[1] = "evidence:" + ref
			if err := p.f.store.validatePrivatePublicationCompletion(w, request, in, authorizedAt); err == nil || p.pushes != 1 {
				t.Fatalf("changed observation/outcome accepted: %v pushes=%d", err, p.pushes)
			}
		})
	}
}

func TestPrivateSourcePublisherPrelinkRefusesCorruptObservationArtifact(t *testing.T) {
	p, request, _, in, _ := interruptedPrelinkedPrivatePublisher(t, "reservation_outcome")
	path := filepath.Join(p.f.store.home, "private", "artifacts", "sha256", strings.TrimPrefix(in.ProofRefs[1], "evidence:artifact:sha256:"))
	if err := os.WriteFile(path, []byte(`{"schema_version":"corrupt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now()); err == nil || p.pushes != 1 {
		t.Fatalf("corrupt immutable observation accepted: %v pushes=%d", err, p.pushes)
	}
}

func TestPrivateSourcePublisherPrelinkRefusesWorkAndPolicyDrift(t *testing.T) {
	for _, drift := range []string{"work-preimage", "state", "duplicate", "foreign-action", "foreign-policy", "denied-policy", "replacement-policy", "later-event", "policy-postdates-authorization"} {
		t.Run(drift, func(t *testing.T) {
			p, request, baseline, _, authorizedAt := interruptedPrelinkedPrivatePublisher(t, "reservation_outcome")
			w, err := p.f.store.Get(p.f.workID)
			if err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "work-preimage":
				w.VerificationIDs = append(w.VerificationIDs, "proof-check:pfrs_unapproved")
			case "state":
				w.State = "verifying"
			case "duplicate":
				w.ActionIntentIDs = append(w.ActionIntentIDs, request.Action.ID)
			case "foreign-action":
				w.ActionIntentIDs = append(w.ActionIntentIDs, "act_foreign")
			case "later-event":
				if err := p.f.store.appendEvent(Event{TS: ts(time.Now()), WorkID: w.ID, From: w.State, To: w.State, Note: "later unrelated event"}); err != nil {
					t.Fatal(err)
				}
			default:
				baseline.ID = "pol_synthetic_replacement"
				switch drift {
				case "foreign-policy":
					baseline.ActionIntentID = "act_foreign"
				case "denied-policy":
					baseline.Outcome = "deny"
				case "policy-postdates-authorization":
					baseline.CreatedAt = authorizedAt.Add(time.Hour).Format(time.RFC3339Nano)
				}
				if err := p.f.store.writeEventJSON(baseline.ID, baseline); err != nil {
					t.Fatal(err)
				}
				w.PolicyDecisionID = baseline.ID
			}
			// These writes are adversarial fixture corruption, never a recovery path.
			if err := p.f.store.writeJSON(p.f.store.workPath(w.ID), w); err != nil {
				t.Fatal(err)
			}
			before, err := workEnvelopeSHA256(w)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now()); err == nil || p.pushes != 1 {
				t.Fatalf("pending drift admitted or effect repeated: error=%v pushes=%d", err, p.pushes)
			}
			after, err := p.f.store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := workEnvelopeSHA256(after)
			if err != nil || digest != before {
				t.Fatal("refused recovery mutated Work")
			}
		})
	}
}
