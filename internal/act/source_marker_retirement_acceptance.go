package act

import (
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
)

// installationAcceptance consumes real native accepted proof. No environment,
// capture or credential body is opened; referenced artifacts are typed metadata.
func (v *SourceMarkerRetirer) installationAcceptance(r policy.SourceMarkerRetirementRequest) error {
	bad := sourceMarkerRetirementError("accepted_installation_required")
	attempts := runattempt.Open(v.home)
	a, e := attempts.Get(r.Installation.AttemptID)
	if e != nil || a.Status != runattempt.StatusSucceeded || a.Harness != "compound" || a.WorkID != r.Installation.WorkID || a.ActionIntentID != r.Installation.ActionID || a.TerminalizationOwner != runtimeConfigInstallOwner || a.TerminalizationReceiptID != r.Installation.ReceiptID || a.TerminalizationTransitionID != r.Installation.TransitionID || a.Launch == nil || a.Launch.CommittedAt == nil || a.Launch.StartErrorAt != nil {
		return bad
	}
	installer, e := NewRuntimeConfigInstaller(v.home)
	if e != nil {
		return bad
	}
	p, ai, e := installer.loadPlan(r.Installation.PlanRef, a.Actor)
	if e != nil || p.Operation != "install" || p.WorkID != r.Installation.WorkID || ai.ID != r.Installation.ActionID || p.RequestSHA256 != r.Installation.RequestSHA256 {
		return bad
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		return bad
	}
	rr, e1 := events.Read(r.Installation.ReceiptID)
	tr, e2 := events.Read(r.Installation.TransitionID)
	var receipt envelope.Receipt
	var transition envelope.Transition
	if e1 != nil || e2 != nil || json.Unmarshal(rr.Bytes, &receipt) != nil || json.Unmarshal(tr.Bytes, &transition) != nil || receipt.Validate() != nil || transition.Validate() != nil || receipt.Outcome != "completed" || receipt.StateDelta.ChangeKnowledge != "" || receipt.IDs.WorkID != p.WorkID || transition.IDs.WorkID != p.WorkID || !slices.Contains(receipt.ProofRefs, a.ID) || !slices.Contains(receipt.ProofRefs, ai.ID) || !slices.Contains(receipt.ProofRefs, r.Installation.ObservationRef) || !slices.Contains(transition.ActionRefs, ai.ID) || !slices.Contains(transition.AfterRefs, receipt.ID) {
		return bad
	}
	approved := false
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, "policy-decision:") {
			continue
		}
		record, e := events.Read(strings.TrimPrefix(ref, "policy-decision:"))
		if e != nil {
			return bad
		}
		var decision envelope.PolicyDecision
		if json.Unmarshal(record.Bytes, &decision) != nil || !policy.Allowed(decision) || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, decision) != nil {
			return bad
		}
		if approved {
			return bad
		}
		approved = true
	}
	if !approved {
		return bad
	}
	artifacts, e := NewArtifactStore(v.home)
	if e != nil {
		return bad
	}
	raw, e := artifacts.Get(r.Installation.ObservationRef)
	if e != nil || runtimeConfigBytesHash(raw) != r.Installation.ObservationSHA256 {
		return bad
	}
	observation, e := decodeRuntimeConfigInstallObservation(raw, p.Request)
	if e != nil || observation.Operation != "install" || observation.State != "installed_inactive" || !observation.ConfigurationAccepted || observation.ApplicationActivated || observation.ServiceActivated {
		return bad
	}
	if !reflect.DeepEqual(r.Resource, p.Request.Resource) || !reflect.DeepEqual(r.Host, p.Request.Host) || r.Root.Path != p.Request.Root.Path || r.Database.Name != p.Request.Database.Name || r.Database.AllowConnections != p.Request.Database.AllowConnections || r.Executables.ClientPath != p.Request.SSH.ClientPath || r.Executables.ClientSHA256 != p.Request.SSH.ClientSHA256 || r.Executables.ClientVersion != p.Request.SSH.ClientVersion {
		return bad
	}
	for i, f := range r.Files {
		if f.Basename != p.Request.Files[i].Basename || f.Path != p.Request.Files[i].Path || f.Stat == nil || !reflect.DeepEqual(f.Stat, observation.Postimages[i]) {
			return bad
		}
	}
	if r.Inactive.Basename != p.Request.Marker.Basename || !reflect.DeepEqual(sourceMarkerWithoutHash(*r.Inactive.Stat), sourceMarkerWithoutHash(p.Request.Marker.Stat)) {
		return bad
	}
	if r.SSH.Alias != p.Request.SSH.Alias || r.SSH.User != p.Request.SSH.User || r.SSH.Port != p.Request.SSH.Port || r.SSH.IdentityFile != p.Request.SSH.IdentityFile || r.SSH.KnownHostsFile != p.Request.SSH.KnownHostsFile || r.SSH.KnownHostsSHA256 != p.Request.SSH.KnownHostsSHA256 || r.SSH.HostKeySHA256 != p.Request.SSH.HostKeySHA256 || r.SSH.InterpreterSHA256 != p.Request.SSH.InterpreterSHA256 || r.SSH.SudoSHA256 != p.Request.SSH.SudoSHA256 {
		return bad
	}
	for _, name := range p.Request.Profile.Containers {
		if !strings.HasPrefix(name, r.ContainerPrefix) {
			return bad
		}
	}
	return nil
}
