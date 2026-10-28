package act

import (
	"compound/internal/policy"
	"encoding/json"
	"reflect"
	"strconv"
)

const sourceMarkerRetirementObservationSchema = "compound.source_marker_retirement_observation.v0.1"

type SourceMarkerRetirementObservation struct {
	Operation                   string                             `json:"operation"`
	Changed                     bool                               `json:"changed"`
	SchemaVersion               string                             `json:"schema_version"`
	RequestSHA256               string                             `json:"request_sha256"`
	ProgramSHA256               string                             `json:"program_sha256"`
	Transaction                 string                             `json:"transaction"`
	State                       string                             `json:"state"`
	HostBefore                  policy.RuntimeConfigInstallHost    `json:"host_before"`
	HostAfter                   policy.RuntimeConfigInstallHost    `json:"host_after"`
	RuntimeBefore               [4]policy.RuntimeConfigInstallStat `json:"runtime_before"`
	RuntimeAfter                [4]policy.RuntimeConfigInstallStat `json:"runtime_after"`
	MarkerBefore                *policy.RuntimeConfigInstallStat   `json:"marker_before"`
	MarkerAfter                 policy.RuntimeConfigInstallStat    `json:"marker_after"`
	InactiveBefore              policy.RuntimeConfigInstallStat    `json:"inactive_before"`
	InactiveAfter               policy.RuntimeConfigInstallStat    `json:"inactive_after"`
	JournalRetained             bool                               `json:"journal_retained"`
	SourceMarkerRetired         bool                               `json:"source_marker_retired"`
	MarkerInodePreserved        bool                               `json:"marker_inode_preserved"`
	DatabaseClosed              bool                               `json:"database_closed"`
	ApplicationContainersAbsent bool                               `json:"application_containers_absent"`
	CredentialBodiesOpened      bool                               `json:"credential_bodies_opened"`
	ConfigurationRewritten      bool                               `json:"configuration_rewritten"`
	DatabaseMutated             bool                               `json:"database_mutated"`
	ApplicationActivated        bool                               `json:"application_activated"`
	ServiceActivated            bool                               `json:"service_activated"`
}

func sourceMarkerWithoutHash(s policy.RuntimeConfigInstallStat) policy.RuntimeConfigInstallStat {
	s.SHA256 = ""
	return s
}
func decodeSourceMarkerRetirementObservation(raw []byte, r policy.SourceMarkerRetirementRequest) (SourceMarkerRetirementObservation, error) {
	var o SourceMarkerRetirementObservation
	bad := sourceMarkerRetirementError("observation_invalid")
	if len(raw) > 32768 || runtimeConfigStrict(raw, &o) != nil || !credentialVerifyRequiredFields(raw, reflect.TypeOf(o)) {
		return o, bad
	}
	var shape struct {
		Before []json.RawMessage `json:"runtime_before"`
		After  []json.RawMessage `json:"runtime_after"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Before) != 4 || len(shape.After) != 4 {
		return o, bad
	}
	if o.SchemaVersion != sourceMarkerRetirementObservationSchema || o.RequestSHA256 != policy.SourceMarkerRetirementDigest(r) || o.ProgramSHA256 != SourceMarkerRetirementProgramSHA256() || o.Transaction != ".smr-"+r.AttemptID || o.State != "source_marker_retired" || (o.Operation != "retire" && o.Operation != "reconcile") || o.HostBefore != r.Host || o.HostAfter != r.Host || !o.JournalRetained || !o.SourceMarkerRetired || !o.MarkerInodePreserved || !o.DatabaseClosed || !o.ApplicationContainersAbsent || o.CredentialBodiesOpened || o.ConfigurationRewritten || o.DatabaseMutated || o.ApplicationActivated || o.ServiceActivated || o.Changed != (o.MarkerBefore != nil) {
		return o, bad
	}
	for i, f := range r.Files {
		if f.Stat == nil || o.RuntimeBefore[i] != sourceMarkerWithoutHash(*f.Stat) || o.RuntimeAfter[i] != o.RuntimeBefore[i] {
			return o, bad
		}
	}
	if r.Marker.Stat == nil || r.Inactive.Stat == nil || (o.MarkerBefore != nil && *o.MarkerBefore != sourceMarkerWithoutHash(*r.Marker.Stat)) || o.InactiveBefore != sourceMarkerWithoutHash(*r.Inactive.Stat) || o.InactiveAfter != o.InactiveBefore {
		return o, bad
	}
	before := sourceMarkerWithoutHash(*r.Marker.Stat)
	after := o.MarkerAfter
	ct1, e1 := strconv.ParseInt(before.CtimeNS, 10, 64)
	ct2, e2 := strconv.ParseInt(after.CtimeNS, 10, 64)
	after.CtimeNS = before.CtimeNS
	if e1 != nil || e2 != nil || ct2 < ct1 || after != before {
		return o, bad
	}
	return o, nil
}
