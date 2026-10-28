package work

import (
	"bytes"
	"compound/internal/home"
	"compound/internal/landlease"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

type remoteProofContextKey struct{}
type remoteProofContext struct {
	home        string
	reservation ProofResourceReservation
	command     RemoteProofCommand
}

func ContextWithRemoteProof(ctx context.Context, homePath string, r ProofResourceReservation, command RemoteProofCommand) context.Context {
	return context.WithValue(ctx, remoteProofContextKey{}, remoteProofContext{homePath, r, command})
}
func RemoteProofRequired(ctx context.Context) bool {
	v, ok := ctx.Value(remoteProofContextKey{}).(remoteProofContext)
	return ok && v.reservation.Profile.RemoteGoTest != nil
}
func RemoteProofReportSchema(ctx context.Context) string {
	if RemoteProofRequired(ctx) {
		return RemoteProofReportSchemaVersion
	}
	return ProofCheckSchemaVersion
}

type remoteProofState struct {
	SchemaVersion     string              `json:"schema_version,omitempty"`
	ID                string              `json:"id"`
	RequestDigest     string              `json:"request_digest"`
	Request           *RemoteProofRequest `json:"request,omitempty"`
	Status            string              `json:"status"`
	Owner             json.RawMessage     `json:"owner,omitempty"`
	LaunchPossible    bool                `json:"launch_possible,omitempty"`
	CancelRequested   bool                `json:"cancel_requested,omitempty"`
	OwnershipRunID    string              `json:"ownership_run_id,omitempty"`
	VerifiedEvidence  json.RawMessage     `json:"verified_evidence,omitempty"`
	CleanupCheckpoint json.RawMessage     `json:"cleanup_checkpoint,omitempty"`
	RecoveryError     string              `json:"recovery_error,omitempty"`
	Quiescent         bool                `json:"quiescent,omitempty"`
	Receipt           *RemoteProofReceipt `json:"receipt,omitempty"`
	ReceiptJSON       json.RawMessage     `json:"-"`
}
type remoteOutputBuffer struct{ bytes.Buffer }

func (b *remoteOutputBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<20 {
		return 0, fmt.Errorf("remote protocol output exceeds bound")
	}
	return b.Buffer.Write(p)
}
func remoteShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func remoteTransport(ctx context.Context, route RemoteProofRoute, request RemoteProofRequest, operation, archive string, command RemoteProofCommand, launch ...func(*exec.Cmd) error) (remoteProofState, error) {
	digest, err := remoteDigest(request)
	if err != nil {
		return remoteProofState{}, err
	}
	if err := validateRemoteProofRoute(route); err != nil {
		return remoteProofState{}, err
	}
	if operation != "receive" && operation != "start" && operation != "inspect" && operation != "cancel" {
		return remoteProofState{}, fmt.Errorf("unknown remote operation")
	}
	args := []string{"python3", route.RunnerPath, operation, "--request-id", request.ID, "--request-digest", digest, "--config", route.ConfigBase64}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = remoteShellQuote(arg)
	}
	timeout := time.Duration(route.TimeoutSeconds+60) * time.Second
	if operation == "receive" {
		timeout = 900 * time.Second
	}
	if operation == "inspect" || operation == "cancel" {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := command(callCtx, "ssh", "-T", "-oBatchMode=yes", "-oConnectTimeout=20", "--", route.Host, strings.Join(quoted, " "))
	if operation == "receive" {
		f, err := os.Open(archive)
		if err != nil {
			return remoteProofState{}, err
		}
		defer f.Close()
		cmd.Stdin = f
	}
	var stdout, stderr remoteOutputBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	run := cmd.Run
	if len(launch) > 0 {
		run = func() error { return launch[0](cmd) }
	}
	if err := run(); err != nil {
		return remoteProofState{}, fmt.Errorf("remote %s transport failed: %w: %s", operation, err, ProofGateOutputTail(stderr.Bytes(), 2000))
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if len(lines) == 0 {
		return remoteProofState{}, fmt.Errorf("remote protocol response absent")
	}
	var state remoteProofState
	if err := remoteStrictJSON(lines[len(lines)-1], &state); err != nil {
		return state, fmt.Errorf("remote protocol response: %w", err)
	}
	if state.ID != request.ID || state.RequestDigest != digest {
		return state, fmt.Errorf("remote response identity mismatch")
	}
	if state.Status != "busy" && state.SchemaVersion != remoteStateSchema {
		return state, fmt.Errorf("remote response schema mismatch")
	}
	if state.Request != nil && !reflect.DeepEqual(*state.Request, request) {
		return state, fmt.Errorf("remote state substituted request")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(lines[len(lines)-1], &wire); err != nil {
		return state, err
	}
	state.ReceiptJSON = wire["receipt"]
	if state.Status == "terminal" {
		if state.Receipt == nil {
			return state, fmt.Errorf("terminal state missing receipt")
		}
		if err := validateRemoteProofReceipt(*state.Receipt, request); err != nil {
			return state, err
		}
	} else if state.Receipt != nil && state.Receipt.Quiescent {
		return state, fmt.Errorf("nonterminal state contradicts quiescent receipt")
	}
	return state, nil
}
func (s *Store) remoteProofAuthority(r ProofResourceReservation) error {
	var current ProofResourceReservation
	if err := s.readProofReservationInto(r.ID, &current); err != nil {
		return err
	}
	if !reflect.DeepEqual(current, r) {
		return fmt.Errorf("remote reservation changed")
	}
	_, err := s.activeProofRunAttempt(r, r.WorkID, r.Actor)
	return err
}
func (s *Store) remoteProofCustody(r ProofResourceReservation, request RemoteProofRequest, state remoteProofState, validate func() error) (*RemoteProofEvidence, error) {
	if state.Status != "terminal" || state.Receipt == nil {
		return nil, fmt.Errorf("remote remains %s; exact reconciliation required", state.Status)
	}
	if err := validateRemoteProofReceipt(*state.Receipt, request); err != nil {
		return nil, err
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return nil, err
	}
	requestBody, err := remoteCanonicalJSON(request)
	if err != nil {
		return nil, err
	}
	requestRef, err := artifacts.Put(requestBody)
	if err != nil {
		return nil, err
	}
	receiptBody, err := remoteCanonicalJSON(state.ReceiptJSON)
	if err != nil {
		return nil, err
	}
	receiptRef, err := artifacts.Put(receiptBody)
	if err != nil {
		return nil, err
	}
	evidence := &RemoteProofEvidence{ProfileDigest: r.Profile.Digest, DescriptorDigest: r.Profile.RemoteGoTest.Digest, RequestArtifactRef: requestRef, ReceiptArtifactRef: receiptRef, Request: request, Receipt: *state.Receipt, ReceiptJSON: receiptBody}
	if err := validateRemoteProofEvidence(evidence, r); err != nil {
		return nil, err
	}
	digest, _ := remoteDigest(request)
	receiptDigest, _ := remoteDigest(state.ReceiptJSON)
	resource := landlease.ProofResourceForIdentity(s.home, r.ProofCoordination)
	err = resource.ResolveRemoteExecution(r.Holder, landlease.RemoteExecutionRequest{ID: request.ID, Digest: digest}, receiptDigest, func() error {
		if validate == nil {
			return fmt.Errorf("remote receipt authority validator required")
		}
		if err := validate(); err != nil {
			return err
		}
		stored, err := artifacts.Get(receiptRef)
		if err != nil {
			return err
		}
		if !bytes.Equal(stored, receiptBody) {
			return fmt.Errorf("remote receipt custody changed")
		}
		return validateRemoteProofReceipt(*state.Receipt, request)
	})
	if err != nil {
		return nil, err
	}
	return evidence, nil
}
func RunRemoteProofGate(ctx context.Context, repo string, step ProofCheckStep) (output []byte, evidence *RemoteProofEvidence, err error) {
	bound, ok := ctx.Value(remoteProofContextKey{}).(remoteProofContext)
	if !ok || bound.reservation.Profile.RemoteGoTest == nil {
		return nil, nil, fmt.Errorf("remote proof context absent")
	}
	r := bound.reservation
	request, err := remoteRequestFor(r)
	if err != nil {
		return nil, nil, err
	}
	if step.Name != "go-test" || step.ProofRef != "go-test-count-1-general-release" || !reflect.DeepEqual(step.Command, remoteGateCommand(request.HeadSHA)) {
		return nil, nil, fmt.Errorf("remote route only supports exact canonical general-release gate")
	}
	s, err := Open(bound.home)
	if err != nil {
		return nil, nil, err
	}
	if err := s.remoteProofAuthority(r); err != nil {
		return nil, nil, err
	}
	archive, cleanup, timings, counts, err := remoteProofArchive(ctx, repo, request, bound.command)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return nil, nil, err
	}
	body, err := remoteCanonicalJSON(request)
	if err != nil {
		return nil, nil, err
	}
	if _, err := artifacts.Put(body); err != nil {
		return nil, nil, err
	}
	digest, _ := remoteDigest(request)
	if err := landlease.ProofResourceForIdentity(s.home, r.ProofCoordination).PrepareRemoteExecution(r.Holder, landlease.RemoteExecutionRequest{ID: request.ID, Digest: digest}, func() error { return s.remoteProofAuthority(r) }); err != nil {
		return nil, nil, err
	}
	started := time.Now()
	state, err := remoteTransport(ctx, r.Profile.RemoteGoTest.Route, request, "receive", archive, bound.command)
	timings["upload_receive"] = time.Since(started).Milliseconds()
	if err != nil {
		return nil, nil, err
	}
	if state.Status == "ready" {
		if err := s.remoteProofAuthority(r); err != nil {
			return nil, nil, err
		}
		started = time.Now()
		state, err = remoteTransport(ctx, r.Profile.RemoteGoTest.Route, request, "start", "", bound.command)
		timings["execution_receipt_cleanup"] = time.Since(started).Milliseconds()
		if err != nil {
			return nil, nil, err
		}
	}
	evidence, err = s.remoteProofCustody(r, request, state, func() error { return s.remoteProofAuthority(r) })
	if err != nil {
		return nil, nil, err
	}
	evidence.Timings = timings
	evidence.Bytes = counts
	output, _ = json.Marshal(evidence)
	if evidence.Receipt.Status != "succeeded" {
		return output, evidence, fmt.Errorf("remote canonical gate %s", evidence.Receipt.Status)
	}
	return output, evidence, nil
}
func (s *Store) validateRemoteProofArtifacts(report ownedProofReport) error {
	remote := false
	for _, step := range report.Steps {
		remote = remote || step.RemoteEvidence != nil
	}
	if !remote {
		return nil
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return err
	}
	for _, step := range report.Steps {
		if step.RemoteEvidence == nil {
			continue
		}
		e := step.RemoteEvidence
		for _, item := range []struct {
			ref string
			v   any
		}{{e.RequestArtifactRef, e.Request}, {e.ReceiptArtifactRef, e.ReceiptJSON}} {
			expected, err := remoteCanonicalJSON(item.v)
			if err != nil {
				return err
			}
			body, err := artifacts.Get(item.ref)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, expected) {
				return fmt.Errorf("remote proof artifact differs from report")
			}
		}
	}
	return nil
}

func ValidateRemoteProofDomain(homePath string) error {
	layout, err := landlease.LayoutStatus(homePath)
	if err != nil {
		return err
	}
	if layout.Mode != "scoped" {
		return fmt.Errorf("remote proof requires explicit fenced v0.3 cutover")
	}
	status, err := landlease.ReadProofDomainStatus(layout.BackingRoot)
	if err != nil {
		return err
	}
	if status.Phase != "active" || status.SchemaVersion != string(landlease.ProofDomainFencedV3) {
		return fmt.Errorf("remote proof requires explicit fenced v0.3 cutover")
	}
	return nil
}
