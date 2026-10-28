package work

import (
	"compound/internal/authority/gitrepository"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

func remoteHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func remoteRequestFor(r ProofResourceReservation) (RemoteProofRequest, error) {
	d := r.Profile.RemoteGoTest
	if d == nil {
		return RemoteProofRequest{}, fmt.Errorf("reservation does not require remote go-test")
	}
	if err := validateRemoteProofDescriptor(*d); err != nil {
		return RemoteProofRequest{}, err
	}
	if r.ID == "" || r.WorkID == "" || r.RunAttemptID == "" || r.Profile.Digest == "" {
		return RemoteProofRequest{}, fmt.Errorf("exact native reservation identity required")
	}
	idDigest, _ := remoteDigest([]string{r.ID, r.Profile.Digest})
	return RemoteProofRequest{SchemaVersion: remoteRequestSchema, ID: "rgp_" + idDigest[:32], WorkID: r.WorkID, ReservationID: r.ID, RunAttemptID: r.RunAttemptID, RepositoryFingerprint: d.Source.RepositoryFingerprint, BaseSHA: d.Source.BaseSHA, HeadSHA: d.Source.HeadSHA, SourceTree: d.Source.TreeSHA, GateDigest: d.GateDigest, InputsDigest: d.InputsDigest, RunnerSHA256: d.Route.RunnerSHA256, GoVersion: d.Route.GoVersion, TimeoutSeconds: d.Route.TimeoutSeconds, InputRoots: d.InputRoots, Inputs: d.Inputs}, nil
}
func validateRemoteProofDescriptor(d RemoteProofDescriptor) error {
	if err := gitrepository.ValidateSnapshotShape(d.Source); err != nil {
		return err
	}
	if !d.Source.Clean {
		return fmt.Errorf("remote source must be clean")
	}
	if err := validateRemoteProofRoute(d.Route); err != nil {
		return err
	}
	roots := append([]string{}, ProofCeremonyOwnedIgnoredInputs...)
	slices.Sort(roots)
	if !reflect.DeepEqual(roots, d.InputRoots) {
		return fmt.Errorf("remote input inventory differs from ceremony owner")
	}
	if err := validateRemoteProofInputs(d.InputRoots, d.Inputs); err != nil {
		return err
	}
	inputDigest, err := remoteInputsDigest(d.InputRoots, d.Inputs)
	if err != nil {
		return err
	}
	gateDigest, err := remoteGateDigest(d.Source.HeadSHA)
	if err != nil {
		return err
	}
	if inputDigest != d.InputsDigest || gateDigest != d.GateDigest {
		return fmt.Errorf("remote input or gate digest mismatch")
	}
	digest := d.Digest
	d.Digest = ""
	expected, err := remoteDigest(d)
	if err != nil {
		return err
	}
	if digest != expected {
		return fmt.Errorf("remote descriptor digest mismatch")
	}
	return nil
}
func validateRemoteProofReceipt(r RemoteProofReceipt, request RemoteProofRequest) error {
	digest, err := remoteDigest(request)
	if err != nil {
		return err
	}
	if r.SchemaVersion != remoteReceiptSchema || r.ID != request.ID || r.RequestDigest != digest {
		return fmt.Errorf("remote receipt identity mismatch")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.FinishedAt); err != nil {
		return fmt.Errorf("remote receipt finished_at invalid")
	}
	if !r.Quiescent || r.Cleanup.Status != "ok" || r.Cleanup.Leftovers == nil || *r.Cleanup.Leftovers != 0 || len(r.Cleanup.Errors) != 0 {
		return fmt.Errorf("remote receipt does not prove clean quiescence")
	}
	if r.Status != "succeeded" && r.Status != "failed" && r.Status != "canceled" {
		return fmt.Errorf("remote receipt not terminal")
	}
	if r.Cleanup.FinishedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.Cleanup.FinishedAt); err != nil {
			return fmt.Errorf("remote cleanup timestamp invalid")
		}
	}
	if r.Request == nil {
		if r.Inherited != nil || r.Verified != nil || r.Execution.ExitCode != nil || len(r.Execution.Command) != 0 {
			return fmt.Errorf("invalid request-free receipt")
		}
		if r.Execution.Launched == nil {
			recoveredCancel := r.Status == "canceled" && r.Execution.Reason == "exact owned resources recovered after coordinator loss"
			recoveredCheckpoint := r.Status == "failed" && r.Execution.Reason == "recovered durable exact cleanup checkpoint; gate outcome unproved" && r.Cleanup.FinishedAt != ""
			if (!recoveredCancel && !recoveredCheckpoint) || r.Execution.StageCommandsStarted != nil {
				return fmt.Errorf("unproved request-free recovery variant")
			}
		} else {
			if *r.Execution.Launched {
				return fmt.Errorf("request-free receipt cannot claim launched gate")
			}
			if r.Status == "failed" {
				if r.Execution.StageCommandsStarted == nil || *r.Execution.StageCommandsStarted != 0 {
					return fmt.Errorf("failed receive lacks zero stage launches")
				}
			} else if r.Status != "canceled" || r.Execution.StageCommandsStarted != nil {
				return fmt.Errorf("invalid unseen tombstone")
			}
		}
		return nil
	}
	if !reflect.DeepEqual(*r.Request, request) {
		return fmt.Errorf("remote receipt substituted request")
	}
	inherited := r.Inherited
	if inherited == nil || inherited.WorkID != request.WorkID || inherited.ReservationID != request.ReservationID || inherited.RunAttemptID != request.RunAttemptID || inherited.RepositoryFingerprint != request.RepositoryFingerprint {
		return fmt.Errorf("remote receipt inherited identity mismatch")
	}
	if v := r.Verified; v != nil {
		if v.HeadSHA != request.HeadSHA || v.BaseSHA != request.BaseSHA || v.SourceTree != request.SourceTree || v.InputsDigest != request.InputsDigest || v.RunnerSHA256 != request.RunnerSHA256 || v.GoVersion != request.GoVersion || v.GateDigest != request.GateDigest {
			return fmt.Errorf("remote receipt verified identity mismatch")
		}
	}
	if r.Status == "succeeded" {
		if r.Verified == nil || r.Execution.Launched == nil || !*r.Execution.Launched || r.Execution.ExitCode == nil || *r.Execution.ExitCode != 0 || !r.Execution.PhasesComplete || !reflect.DeepEqual(r.Execution.Command, remoteGateCommand(request.HeadSHA)) {
			return fmt.Errorf("remote green receipt lacks exact successful gate execution")
		}
	} else if r.Execution.Launched != nil && *r.Execution.Launched && r.Verified == nil {
		return fmt.Errorf("launched remote receipt lacks verified identity")
	}
	return nil
}
func validateRemoteProofEvidence(e *RemoteProofEvidence, reservation ProofResourceReservation) error {
	if e == nil {
		return fmt.Errorf("required remote proof evidence missing")
	}
	expected, err := remoteRequestFor(reservation)
	if err != nil {
		return err
	}
	if e.ProfileDigest != reservation.Profile.Digest || e.DescriptorDigest != reservation.Profile.RemoteGoTest.Digest || !reflect.DeepEqual(e.Request, expected) {
		return fmt.Errorf("remote proof evidence profile/request mismatch")
	}
	if err := validateRemoteProofReceipt(e.Receipt, expected); err != nil {
		return err
	}
	var rawReceipt RemoteProofReceipt
	if err := remoteStrictJSON(e.ReceiptJSON, &rawReceipt); err != nil {
		return err
	}
	if !reflect.DeepEqual(rawReceipt, e.Receipt) {
		return fmt.Errorf("remote raw receipt differs from typed evidence")
	}
	for _, item := range []struct {
		ref string
		v   any
	}{{e.RequestArtifactRef, e.Request}, {e.ReceiptArtifactRef, e.ReceiptJSON}} {
		digest, err := remoteDigest(item.v)
		if err != nil {
			return err
		}
		if item.ref != "artifact:sha256:"+digest {
			return fmt.Errorf("remote evidence custody digest mismatch")
		}
	}
	return nil
}
func validateRemoteReportSchema(report ownedProofReport) error {
	remote := 0
	for _, step := range report.Steps {
		if step.RemoteEvidence != nil {
			if step.RemoteEvidence.Recovery != nil {
				return fmt.Errorf("recovery evidence cannot certify a gate")
			}
			remote++
			if step.Name != "go-test" || step.ProofRef != "go-test-count-1-general-release" {
				return fmt.Errorf("unexpected remote gate evidence")
			}
		}
	}
	if report.SchemaVersion == RemoteProofReportSchemaVersion {
		if remote != 1 {
			return fmt.Errorf("remote report requires exactly one remote gate evidence")
		}
	} else if remote != 0 {
		return fmt.Errorf("remote evidence requires v0.7 report schema")
	}
	return nil
}
func validateRemoteReportReservation(report ownedProofReport, r ProofResourceReservation) error {
	if err := validateRemoteReportSchema(report); err != nil {
		return err
	}
	d := r.Profile.RemoteGoTest
	if d == nil {
		if report.SchemaVersion == RemoteProofReportSchemaVersion {
			return fmt.Errorf("unexpected remote report for local reservation")
		}
		return nil
	}
	if report.SchemaVersion != RemoteProofReportSchemaVersion {
		return fmt.Errorf("remote reservation cannot complete with local report")
	}
	for _, step := range report.Steps {
		if step.Name == "go-test" {
			if err := validateRemoteProofEvidence(step.RemoteEvidence, r); err != nil {
				return err
			}
			if step.Status == "ok" && step.RemoteEvidence.Receipt.Status != "succeeded" {
				return fmt.Errorf("failed remote receipt reported green")
			}
			return nil
		}
	}
	return fmt.Errorf("remote reservation missing canonical go-test gate")
}
func remoteArtifactDigest(ref string) bool {
	return strings.HasPrefix(ref, "artifact:sha256:") && remoteHex(strings.TrimPrefix(ref, "artifact:sha256:"), 64)
}
