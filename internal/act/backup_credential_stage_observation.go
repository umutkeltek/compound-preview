package act

import (
	"compound/internal/policy"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

const backupCredentialStageObservationSchema = "compound.backup_credential_stage_observation.v0.1"

type BackupCredentialStageObservation struct {
	Operation            string                              `json:"operation"`
	Changed              bool                                `json:"changed"`
	SchemaVersion        string                              `json:"schema_version"`
	RequestSHA256        string                              `json:"request_sha256"`
	ProgramSHA256        string                              `json:"program_sha256"`
	Transaction          string                              `json:"transaction"`
	State                string                              `json:"state"`
	HostBefore           policy.BackupCredentialStageHost    `json:"host_before"`
	HostAfter            policy.BackupCredentialStageHost    `json:"host_after"`
	Preimages            [4]*policy.RuntimeConfigInstallStat `json:"preimages"`
	Postimages           [4]*policy.RuntimeConfigInstallStat `json:"postimages"`
	Written              [4]bool                             `json:"written"`
	JournalRetained      bool                                `json:"journal_retained"`
	CredentialsStaged    bool                                `json:"credentials_staged"`
	ApplicationActivated bool                                `json:"application_activated"`
	ServiceActivated     bool                                `json:"service_activated"`
}

func decodeBackupCredentialStageObservation(raw []byte, r policy.BackupCredentialStageRequest) (BackupCredentialStageObservation, error) {
	var o BackupCredentialStageObservation
	if len(raw) > 32768 || runtimeConfigStrict(raw, &o) != nil || !credentialVerifyRequiredFields(raw, reflect.TypeOf(o)) {
		return o, backupCredentialStageError("observation_shape")
	}
	var shape struct {
		Pre     []json.RawMessage `json:"preimages"`
		Post    []json.RawMessage `json:"postimages"`
		Written []json.RawMessage `json:"written"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Pre) != 4 || len(shape.Post) != 4 || len(shape.Written) != 4 {
		return o, backupCredentialStageError("observation_shape")
	}
	return o, validateBackupCredentialStageObservation(o, r)
}
func validateBackupCredentialStageObservation(o BackupCredentialStageObservation, r policy.BackupCredentialStageRequest) error {
	bad := backupCredentialStageError("observation_invalid")
	if o.SchemaVersion != backupCredentialStageObservationSchema || o.RequestSHA256 != policy.BackupCredentialStageDigest(r) || o.ProgramSHA256 != BackupCredentialStageProgramSHA256() || o.Transaction != ".bcs-"+r.AttemptID || o.HostBefore != r.Host || o.HostAfter != r.Host || !o.JournalRetained || !o.CredentialsStaged || o.ApplicationActivated || o.ServiceActivated || o.State != "staged_backup_credentials" || (o.Operation != "stage" && o.Operation != "reconcile") {
		return bad
	}
	changed := false
	seen := map[[2]uint64]bool{}
	for i, s := range o.Postimages {
		expected := r.Files[i].Stat
		if expected == nil || s == nil || o.Preimages[i] != nil || s.UID != 0 || s.GID != 0 || s.Mode != "0600" || s.Links != 1 || s.Bytes != expected.Bytes || (expected.SHA256 != "" && s.SHA256 != expected.SHA256) || s.Device == 0 || s.Inode == 0 || s.MtimeNS == "" || s.CtimeNS == "" {
			return bad
		}
		if h, e := hex.DecodeString(s.SHA256); e != nil || len(h) != 32 {
			return bad
		}
		key := [2]uint64{s.Device, s.Inode}
		if seen[key] {
			return bad
		}
		seen[key] = true
		changed = changed || o.Written[i]
	}
	if changed != o.Changed {
		return bad
	}
	return nil
}
