package act

import (
	"compound/internal/policy"
	"encoding/hex"
	"reflect"
	"strconv"
)

const runtimeConfigInstallObservationSchema = "compound.runtime_config_installation_observation.v0.2"

type RuntimeConfigInstallObservation struct {
	Operation             string                              `json:"operation"`
	Changed               bool                                `json:"changed"`
	SchemaVersion         string                              `json:"schema_version"`
	RequestSHA256         string                              `json:"request_sha256"`
	ProgramSHA256         string                              `json:"program_sha256"`
	Transaction           string                              `json:"transaction"`
	State                 string                              `json:"state"`
	HostBefore            policy.RuntimeConfigInstallHost     `json:"host_before"`
	HostAfter             policy.RuntimeConfigInstallHost     `json:"host_after"`
	Preimages             [4]*policy.RuntimeConfigInstallStat `json:"preimages"`
	Postimages            [4]*policy.RuntimeConfigInstallStat `json:"postimages"`
	Written               [4]bool                             `json:"written"`
	FencesRetained        bool                                `json:"fences_retained"`
	ConfigurationAccepted bool                                `json:"configuration_accepted"`
	ApplicationActivated  bool                                `json:"application_activated"`
	ServiceActivated      bool                                `json:"service_activated"`
}

func decodeRuntimeConfigInstallObservation(raw []byte, r policy.RuntimeConfigInstallRequest) (RuntimeConfigInstallObservation, error) {
	var o RuntimeConfigInstallObservation
	if len(raw) > 32768 || runtimeConfigStrict(raw, &o) != nil || !credentialVerifyRequiredFields(raw, reflect.TypeOf(o)) {
		return o, runtimeConfigInstallError("observation_shape")
	}
	return o, validateRuntimeConfigInstallObservation(o, r)
}
func validateRuntimeConfigInstallObservation(o RuntimeConfigInstallObservation, r policy.RuntimeConfigInstallRequest) error {
	bad := runtimeConfigInstallError("observation_invalid")
	if r.Files[0].Stat == nil {
		return bad
	}
	if o.SchemaVersion != runtimeConfigInstallObservationSchema || o.RequestSHA256 != policy.RuntimeConfigInstallDigest(r) || o.ProgramSHA256 != RuntimeConfigInstallProgramSHA256() || o.Transaction != ".rci-"+r.AttemptID || o.HostBefore != r.Host || o.HostAfter != r.Host || !o.FencesRetained || o.ApplicationActivated || o.ServiceActivated {
		return bad
	}
	if o.Operation != "install" && o.Operation != "reconcile" && o.Operation != "rollback" {
		return bad
	}
	if o.Operation == "reconcile" && o.Changed {
		return bad
	}
	if o.Operation == "install" && o.State == "installed_inactive" && !o.Changed {
		return bad
	}
	if o.State != "installed_inactive" && o.State != "recovery_required" && o.State != "rolled_back" {
		return bad
	}
	if o.ConfigurationAccepted != (o.State == "installed_inactive") {
		return bad
	}
	for i, pre := range o.Preimages {
		if !reflect.DeepEqual(pre, r.Files[i].Stat) {
			return bad
		}
	}
	seen := map[[2]uint64]bool{}
	for i, post := range o.Postimages {
		if o.Written[i] != (o.State == "installed_inactive") {
			return bad
		}
		if o.State == "installed_inactive" && post == nil {
			return bad
		}
		if post != nil && (post.UID != 0 || post.GID != 0 || post.Mode != "0600" || post.Links != 1 || post.Device != r.Root.Device || post.Inode == 0 || post.Bytes <= 0 || post.Bytes > 16384 || len(post.SHA256) != 64) {
			return bad
		}
		if post != nil {
			hash, err := hex.DecodeString(post.SHA256)
			if err != nil || len(hash) != 32 {
				return bad
			}
			for _, value := range []string{post.MtimeNS, post.CtimeNS} {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
					return bad
				}
			}
			key := [2]uint64{post.Device, post.Inode}
			if seen[key] {
				return bad
			}
			seen[key] = true
			if o.State == "installed_inactive" && i == 0 && post.Inode == r.Files[0].Stat.Inode {
				return bad
			}
		}
		if o.State == "rolled_back" && i > 0 && post != nil {
			return bad
		}
	}
	if o.State == "rolled_back" && (o.Postimages[0] == nil || o.Postimages[0].SHA256 != r.Files[0].Stat.SHA256) {
		return bad
	}
	return nil
}
