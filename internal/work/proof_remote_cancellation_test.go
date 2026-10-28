package work

import (
	"compound/internal/runattempt"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/landlease"
)

// A bounded execution deadline can interrupt the remote transport. That local
// interruption is NOT termination evidence: the exact remote fence stays
// unresolved, the local completion cannot release the scarce resource, and only
// the existing authorized reconciliation against an exact quiescent receipt
// releases it.
func TestRemoteProofCancelledTransportRetainsUnresolvedFence(t *testing.T) {
	s := coordinatedStoreFixture(t)
	now := time.Now().UTC()
	w := claimedProofWork(t, s, "remote cancelled transport fixture", proofActorAlpha, now)
	local := capacitySystemProfile(t, false)
	template, _ := remoteProofTestFixture(t)
	descriptor := *template.Profile.RemoteGoTest
	descriptor.Source = *local.SourceSnapshot
	descriptor.GateDigest, _ = remoteGateDigest(descriptor.Source.HeadSHA)
	descriptor.Digest = ""
	descriptor.Digest, _ = remoteDigest(descriptor)
	local.Gates = []ProofResourceGateProfile{{
		Name:     "go-test",
		ProofRef: "go-test-count-1-general-release",
		Command:  remoteGateCommand(descriptor.Source.HeadSHA),
	}}
	local.RemoteGoTest = &descriptor
	local, _ = normalizeProofResourceProfile(local)
	reserved, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: local.Repo,
		Command:   "compound proof-check --fixture-remote",
		Profile:   local,
	}, time.Now())
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
	if err := resource.PrepareRemoteExecution(
		reserved.Reservation.Holder,
		landlease.RemoteExecutionRequest{ID: request.ID, Digest: digest},
		func() error { return nil },
	); err != nil {
		t.Fatal(err)
	}

	// Wait for actual transport process launch before cancellation.
	marker := filepath.Join(t.TempDir(), "started")
	old := remoteProofTransportCommand
	defer func() { remoteProofTransportCommand = old }()
	remoteProofTransportCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "ssh" {
			t.Fatalf("unexpected transport %q", name)
		}
		return exec.CommandContext(ctx, "python3", "-c", "import pathlib,sys,time; pathlib.Path(sys.argv[1]).write_text('started'); time.sleep(300)", marker)
	}
	transportCtx, cancelTransport := context.WithCancel(context.Background())
	defer cancelTransport()
	done := make(chan error, 1)
	go func() {
		_, err := remoteTransport(transportCtx, reserved.Reservation.Profile.RemoteGoTest.Route, request, "start", "", remoteProofTransportCommand)
		done <- err
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("transport never launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelTransport()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled launched transport succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not stop")
	}
	if err := reserved.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}

	// The exact remote fence survives the local interruption.
	pending, err := resource.ExecutionPending(reserved.Reservation.Holder)
	if err != nil || !pending {
		t.Fatalf("the cancelled transport cleared the remote fence: pending=%v err=%v", pending, err)
	}
	// A local exit cannot release the scarce resource while the fence is open.
	if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor:         proofActorAlpha,
		ReservationID: reserved.Reservation.ID,
		FailureReason: "proof-check execution budget expired during remote transport",
	}, time.Now()); err == nil {
		t.Fatal("a local exit released the unresolved remote fence")
	}

	// The existing authorized reconciliation releases the fence only against an
	// exact quiescent receipt.
	if err := reserved.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	remoteProofTransportCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 255")
	}
	if _, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, false, time.Now()); err == nil {
		t.Fatal("offline reconciliation succeeded")
	}
	if pending, err := resource.ExecutionPending(reserved.Reservation.Holder); err != nil || !pending {
		t.Fatalf("offline reconciliation cleared fence: %v %v", pending, err)
	}

	receipt := map[string]any{
		"schema_version":     remoteReceiptSchema,
		"id":                 request.ID,
		"request_digest":     digest,
		"request":            nil,
		"status":             "canceled",
		"finished_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"inherited_identity": nil,
		"execution": map[string]any{
			"launched":  false,
			"command":   []string{},
			"exit_code": nil,
			"reason":    "cancel-before-start tombstone",
		},
		"cleanup":   map[string]any{"status": "ok", "leftovers": 0, "errors": []string{}},
		"quiescent": true,
	}
	state := map[string]any{
		"schema_version": remoteStateSchema,
		"id":             request.ID,
		"request_digest": digest,
		"request":        nil,
		"status":         "terminal",
		"receipt":        receipt,
	}
	body, _ := json.Marshal(state)
	remoteProofTransportCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "ssh" {
			t.Fatalf("unexpected remote transport %q", name)
		}
		return exec.CommandContext(ctx, "python3", "-c", "import sys; print(sys.argv[1])", string(body))
	}
	evidence, err := s.ReconcileRemoteProof(context.Background(), w.ID, proofActorAlpha, reserved.Reservation.ID, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil || evidence.Recovery == nil {
		t.Fatalf("reconciliation returned no recovery state: %+v", evidence)
	}
	status, err := resource.StatusReadOnly(time.Now())
	if err != nil || status.Held {
		t.Fatalf("the reconciled fence still holds the resource: %+v %v", status, err)
	}
	current, _ := s.Get(w.ID)
	if len(current.VerificationIDs) != 0 {
		t.Fatalf("reconciliation donated verification: %v", current.VerificationIDs)
	}
	attempt, err := s.runAttempts().Get(reserved.Reservation.RunAttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed {
		t.Fatalf("original interrupted attempt not failed: %+v %v", attempt, err)
	}

}
