package work

import (
	"bytes"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteProofRecoveredReceiptWireShapes(t *testing.T) {
	_, e := remoteProofTestFixture(t)
	for _, variant := range []string{"failed-receive", "recovered-receive", "checkpoint"} {
		t.Run(variant, func(t *testing.T) {
			var wire map[string]any
			if err := json.Unmarshal(e.ReceiptJSON, &wire); err != nil {
				t.Fatal(err)
			}
			wire["request"] = nil
			wire["inherited_identity"] = nil
			wire["verified_evidence"] = nil
			execution := map[string]any{"launched": false, "command": []string{}, "exit_code": nil, "reason": "upload rejected"}
			wire["status"] = "failed"
			switch variant {
			case "failed-receive":
				execution["stage_commands_started"] = 0
			case "recovered-receive":
				wire["status"] = "canceled"
				execution["launched"] = nil
				execution["reason"] = "exact owned resources recovered after coordinator loss"
			case "checkpoint":
				execution["launched"] = nil
				execution["reason"] = "recovered durable exact cleanup checkpoint; gate outcome unproved"
				wire["cleanup"].(map[string]any)["finished_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			}
			wire["execution"] = execution
			body, _ := remoteCanonicalJSON(wire)
			var receipt RemoteProofReceipt
			if err := remoteStrictJSON(body, &receipt); err != nil {
				t.Fatal(err)
			}
			if err := validateRemoteProofReceipt(receipt, e.Request); err != nil {
				t.Fatal(err)
			}
			receipt.Status = "succeeded"
			if err := validateRemoteProofReceipt(receipt, e.Request); err == nil {
				t.Fatal("recovered outcome donated green")
			}
		})
	}
}
func TestRemoteProofRecoveryFailsOriginalAndReleasesNativeResource(t *testing.T) {
	for _, journal := range []bool{true, false} {
		t.Run(fmt.Sprintf("journal=%t", journal), func(t *testing.T) {
			s := coordinatedStoreFixture(t)
			now := time.Now().UTC()
			w := claimedProofWork(t, s, "remote interrupted fixture", proofActorAlpha, now)
			local := capacitySystemProfile(t, false)
			template, _ := remoteProofTestFixture(t)
			descriptor := *template.Profile.RemoteGoTest
			descriptor.Source = *local.SourceSnapshot
			descriptor.GateDigest, _ = remoteGateDigest(descriptor.Source.HeadSHA)
			descriptor.Digest = ""
			descriptor.Digest, _ = remoteDigest(descriptor)
			local.Gates = []ProofResourceGateProfile{{Name: "go-test", ProofRef: "go-test-count-1-general-release", Command: remoteGateCommand(descriptor.Source.HeadSHA)}}
			local.RemoteGoTest = &descriptor
			local, _ = normalizeProofResourceProfile(local)
			reserved, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: local.Repo, Command: "compound proof-check --fixture-remote", Profile: local}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer reserved.ExecutionBarrier.Close()
			request, err := remoteRequestFor(reserved.Reservation)
			if err != nil {
				t.Fatal(err)
			}
			digest, _ := remoteDigest(request)
			resource := landlease.ProofResourceForIdentity(s.home, reserved.Reservation.ProofCoordination)
			if err := resource.PrepareRemoteExecution(reserved.Reservation.Holder, landlease.RemoteExecutionRequest{ID: request.ID, Digest: digest}, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := reserved.ExecutionBarrier.Close(); err != nil {
				t.Fatal(err)
			}
			if journal {
				_, err = s.CompleteProofResource(w.ID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, FailureReason: "remote transport disconnected"}, time.Now())
				if err == nil {
					t.Fatal("unresolved remote fence released")
				}
			} else {
				if _, err := s.runAttempts().CompleteWithReason(reserved.Reservation.RunAttemptID, runattempt.StatusAbandoned, "caller died before finalizer", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			original, err := s.runAttempts().Get(reserved.Reservation.RunAttemptID)
			if err != nil || journal && original.Status != runattempt.StatusFailed || !journal && original.Status != runattempt.StatusAbandoned {
				t.Fatalf("original=%+v err=%v", original, err)
			}
			receipt := map[string]any{"schema_version": remoteReceiptSchema, "id": request.ID, "request_digest": digest, "request": nil, "status": "canceled", "finished_at": time.Now().UTC().Format(time.RFC3339Nano), "inherited_identity": nil, "verified_evidence": nil, "execution": map[string]any{"launched": false, "command": []string{}, "exit_code": nil, "reason": "cancel-before-start tombstone"}, "cleanup": map[string]any{"status": "ok", "leftovers": 0, "errors": []string{}}, "quiescent": true}
			state := map[string]any{"schema_version": remoteStateSchema, "id": request.ID, "request_digest": digest, "request": nil, "status": "terminal", "receipt": receipt}
			body, _ := json.Marshal(state)
			old := remoteProofTransportCommand
			remoteProofTransportCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name != "ssh" {
					t.Fatal(name)
				}
				return exec.CommandContext(ctx, "python3", "-c", "import sys; print(sys.argv[1])", string(body))
			}
			defer func() { remoteProofTransportCommand = old }()
			if _, err := s.ReconcileRemoteProof(context.Background(), "work_other", proofActorAlpha, reserved.Reservation.ID, true, time.Now()); err == nil {
				t.Fatal("wrong work reconciled")
			}
			if _, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorBeta, reserved.Reservation.ID, true, time.Now()); err == nil {
				t.Fatal("wrong actor reconciled")
			}
			evidence, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, true, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			status, err := resource.StatusReadOnly(time.Now())
			if err != nil || journal && status.Held || !journal && !status.Held {
				t.Fatalf("resource retained: %+v %v", status, err)
			}
			original, _ = s.runAttempts().Get(reserved.Reservation.RunAttemptID)
			if journal && original.Status != runattempt.StatusFailed || !journal && original.Status != runattempt.StatusAbandoned {
				t.Fatal("original was rewritten")
			}
			current, _ := s.Get(w.ID)
			if len(current.VerificationIDs) != 0 {
				t.Fatal("recovery donated verification", current.VerificationIDs)
			}
			artifacts, _ := home.NewArtifactStore(s.home)
			if body, err := artifacts.Get(evidence.ReceiptArtifactRef); err != nil || len(body) == 0 {
				t.Fatalf("receipt custody: %v", err)
			}
			if !journal {
				if evidence.Recovery == nil || evidence.Recovery.Status != "waiting_for_expiry" || evidence.Recovery.ResourceReleased || evidence.Recovery.EligibleAfter != reserved.Reservation.ExpiresAt || evidence.Recovery.NextCommand != reserved.Reservation.Command {
					t.Fatalf("missing precise next state: %+v", evidence.Recovery)
				}
				_, err := time.Parse(time.RFC3339Nano, evidence.Recovery.EligibleAfter)
				if err != nil {
					t.Fatal(err)
				}
				// Move only the synthetic resource clock; admission deliberately reads its own live clock.
				if _, err := resource.Refresh(reserved.Reservation.Holder, time.Second, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				next, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: local.Repo, Command: reserved.Reservation.Command, Profile: local}, time.Now())
				if err != nil {
					t.Fatal("normal expired recovery:", err)
				}
				defer next.ExecutionBarrier.Close()
				if next.Reservation.ID == reserved.Reservation.ID || next.Reservation.RunAttemptID == reserved.Reservation.RunAttemptID {
					t.Fatal("fresh proof reused original")
				}
				original, _ = s.runAttempts().Get(reserved.Reservation.RunAttemptID)
				if original.Status != runattempt.StatusAbandoned {
					t.Fatal("rewrote terminal predecessor")
				}
				if _, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, true, time.Now()); err == nil {
					t.Fatal("old reconciliation accepted newer holder")
				}
			}
		})
	}
}

// An exact approval for one target revision never authorizes another, and a
// pending preparation of the SAME Work is never completed history: only a
// distinct Work's completed preparation is predecessor history. A same-Work
// request for a different exact raw target is an exact preparation mismatch.
func TestRemoteProofMigrationExactTargetApprovalAndPending(t *testing.T) {
	t.Run("v3-prepared-v2-requested", func(t *testing.T) {
		s, _, w, in := proofMigrationStoreFixture(t)
		approveProofMigrationPreparation(t, s, w, in, fixedNow)
		in.ProofTargetRevision = landlease.ProofDomainFencedV3
		if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(time.Second)); err == nil {
			t.Fatal("v0.2 approval authorized v0.3")
		}
		approveProofMigrationPreparation(t, s, w, in, fixedNow.Add(2*time.Second))
		interruptProofMigrationAtPrepared(t, s, w, in, fixedNow.Add(3*time.Second))
		changed := in
		changed.ProofTargetRevision = landlease.ProofDomainLocalV2
		assertProofMigrationTargetRefusal(t, s, w, in, changed, landlease.ProofDomainFencedV3, fixedNow.Add(4*time.Second))
	})
	t.Run("v2-prepared-v3-requested", func(t *testing.T) {
		s, _, w, in := proofMigrationStoreFixture(t)
		in.ProofTargetRevision = landlease.ProofDomainLocalV2
		approveProofMigrationPreparation(t, s, w, in, fixedNow)
		interruptProofMigrationAtPrepared(t, s, w, in, fixedNow.Add(time.Second))
		changed := in
		changed.ProofTargetRevision = landlease.ProofDomainFencedV3
		assertProofMigrationTargetRefusal(t, s, w, in, changed, landlease.ProofDomainLocalV2, fixedNow.Add(2*time.Second))
	})
	t.Run("empty-prepared-explicit-v2-requested", func(t *testing.T) {
		s, _, w, in := proofMigrationStoreFixture(t)
		approveProofMigrationPreparation(t, s, w, in, fixedNow)
		interruptProofMigrationAtPrepared(t, s, w, in, fixedNow.Add(time.Second))
		changed := in
		changed.ProofTargetRevision = landlease.ProofDomainLocalV2
		assertProofMigrationTargetRefusal(t, s, w, in, changed, landlease.ProofDomainLocalV2, fixedNow.Add(2*time.Second))
	})
}

// approveProofMigrationPreparation holds one exact migration action and
// approves it, so the next call owns the exact approval for that request.
func approveProofMigrationPreparation(t *testing.T, s *Store, w envelope.WorkEnvelope, in ResourceMigrationInput, at time.Time) {
	t.Helper()
	_, hold := s.MigrateCoordinationResources(w.ID, in, at)
	approveResourceAction(t, s, hold, at)
}

// interruptProofMigrationAtPrepared interrupts the migration exactly at the
// prepared stage and proves that stage was reached. The preparation journal
// keeps the pending preparation of this exact Work and request.
func interruptProofMigrationAtPrepared(t *testing.T, s *Store, w envelope.WorkEnvelope, in ResourceMigrationInput, at time.Time) {
	t.Helper()
	prepared := 0
	s.resourceMigrationFault = func(stage string) error {
		if stage == "proof-prepared" {
			prepared++
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, at); err == nil {
		t.Fatal("expected prepared failure")
	}
	if prepared == 0 {
		t.Fatal("the proof-prepared stage was never reached")
	}
	s.resourceMigrationFault = nil
}

// assertProofMigrationTargetRefusal proves that a same-Work request for a
// different exact raw target is refused as an exact preparation mismatch,
// leaves the preparation journal, the Work and the proof domain untouched,
// and that the original exact request still resumes its own preparation.
func assertProofMigrationTargetRefusal(t *testing.T, s *Store, w envelope.WorkEnvelope, in, changed ResourceMigrationInput, wantRevision landlease.ProofDomainRevision, at time.Time) {
	t.Helper()
	domainPath := filepath.Join(in.BackingHome, "work", "resource-leases", "landlease", "proof", "domain.json")
	before := map[string][]byte{}
	for _, path := range []string{s.proofMigrationPendingPath(), s.workPath(w.ID), domainPath} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		before[path] = body
	}
	if _, err := s.MigrateCoordinationResources(w.ID, changed, at); err == nil || !strings.Contains(err.Error(), "preparation differs") {
		t.Fatalf("changed pending target %q: %v", changed.ProofTargetRevision, err)
	}
	for path, want := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(want, after) {
			t.Fatalf("refusal changed %s: %v", path, err)
		}
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, at.Add(time.Second)); err != nil {
		t.Fatalf("original exact request did not resume: %v", err)
	}
	status, err := landlease.ReadProofDomainStatus(filepath.Join(in.BackingHome, "work", "resource-leases"))
	if err != nil || status.Phase != "active" || status.SchemaVersion != string(wantRevision) {
		t.Fatalf("resumed proof domain %+v err=%v", status, err)
	}
}

func TestRemoteProofRouteOptionsRejectLightweightObligations(t *testing.T) {
	for _, mode := range []string{"--dev", "--dev=true", "--dry-run", "--ci", "--ci-console", "--recover-legacy-retained", "--timings"} {
		for _, flag := range []string{"--remote-go-test", "--redact-review"} {
			if _, err := ParseProofCheckRouteOptions([]string{flag, "exact", mode}); err == nil {
				t.Fatalf("%s admitted %s", flag, mode)
			}
		}
	}
	for _, args := range [][]string{{"--remote-go-test"}, {"--remote-go-test", "--json"}, {"--redact-review"}, {"--redact-review", "--json"}} {
		if _, err := ParseProofCheckRouteOptions(args); err == nil {
			t.Fatal("missing exact value accepted", args)
		}
	}
	got, err := ParseProofCheckRouteOptions([]string{"--remote-go-test", "route.json", "--redact-review", "act_exact", "--work", "work_exact"})
	if err != nil || got.RemoteRoute != "route.json" || got.PublicRedactionActionID != "act_exact" {
		t.Fatal(got, err)
	}
	if _, err := ParseProofCheckRouteOptions([]string{"--ci"}); err != nil {
		t.Fatal("unchanged lightweight mode refused", err)
	}
}

func TestRemoteProofMigrationCompletionRequiresExactActiveRevision(t *testing.T) {
	s, _, w, in := proofMigrationStoreFixture(t)
	_, hold := s.MigrateCoordinationResources(w.ID, in, fixedNow)
	approveResourceAction(t, s, hold, fixedNow)
	s.resourceMigrationFault = func(stage string) error {
		if stage == "published" {
			return errors.New("after publication")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(time.Second)); err == nil {
		t.Fatal("missing fault")
	}
	s.resourceMigrationFault = nil
	body, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil {
		t.Fatal(err)
	}
	var pending resourceMigrationPending
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	pending.Input.ProofTargetRevision = landlease.ProofDomainFencedV3
	if err := s.finishResourceMigrationLocked(w, pending, fixedNow.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), "exact active proof-domain") {
		t.Fatalf("wrong active revision certified: %v", err)
	}
}

func TestRemoteProofRecoveryRefusesLiveLocalBarrierThenFinalizes(t *testing.T) {
	s := coordinatedStoreFixture(t)
	w := claimedProofWork(t, s, "remote live descendant", proofActorAlpha, time.Now())
	local := capacitySystemProfile(t, false)
	template, _ := remoteProofTestFixture(t)
	d := *template.Profile.RemoteGoTest
	d.Source = *local.SourceSnapshot
	d.GateDigest, _ = remoteGateDigest(d.Source.HeadSHA)
	d.Digest = ""
	d.Digest, _ = remoteDigest(d)
	local.Gates = []ProofResourceGateProfile{{Name: "go-test", ProofRef: "go-test-count-1-general-release", Command: remoteGateCommand(d.Source.HeadSHA)}}
	local.RemoteGoTest = &d
	local, _ = normalizeProofResourceProfile(local)
	reserved, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: local.Repo, Command: "compound proof-check --fixture-remote-live", Profile: local}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.ExecutionBarrier.Close()
	request, err := remoteRequestFor(reserved.Reservation)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := remoteDigest(request)
	resource := landlease.ProofResourceForIdentity(s.home, reserved.Reservation.ProofCoordination)
	if err := resource.PrepareRemoteExecution(reserved.Reservation.Holder, landlease.RemoteExecutionRequest{ID: request.ID, Digest: digest}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	wire := map[string]any{"schema_version": remoteReceiptSchema, "id": request.ID, "request_digest": digest, "request": nil, "inherited_identity": nil, "verified_evidence": nil, "status": "canceled", "finished_at": time.Now().UTC().Format(time.RFC3339Nano), "execution": map[string]any{"launched": false, "command": []string{}, "exit_code": nil, "reason": "cancel-before-start"}, "cleanup": map[string]any{"status": "ok", "errors": []string{}, "leftovers": 0}, "quiescent": true}
	state, _ := json.Marshal(map[string]any{"schema_version": remoteStateSchema, "id": request.ID, "request_digest": digest, "status": "terminal", "receipt": wire})
	old := remoteProofTransportCommand
	t.Cleanup(func() { remoteProofTransportCommand = old })
	remoteProofTransportCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "python3", "-c", "import sys; print(sys.argv[1])", string(state))
	}
	if _, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, true, time.Now()); err == nil || !strings.Contains(err.Error(), "original local proof must terminate") {
		t.Fatalf("live local owner accepted: %v", err)
	}
	if err := reserved.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Recovery == nil || evidence.Recovery.Status != "failed_proof_released" || !evidence.Recovery.ResourceReleased {
		t.Fatal("original running proof not failed and released", evidence.Recovery)
	}
	original, err := s.runAttempts().Get(reserved.Reservation.RunAttemptID)
	if err != nil || original.Status != runattempt.StatusFailed {
		t.Fatal(original, err)
	}
	current, _ := s.Get(w.ID)
	if len(current.VerificationIDs) != 0 {
		t.Fatal("interrupted request donated proof")
	}
}
