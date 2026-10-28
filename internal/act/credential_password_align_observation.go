package act

import (
	"bytes"
	"compound/internal/policy"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strings"
)

func credentialPasswordAlignmentObservationDigest(o CredentialPasswordAlignmentObservation) string {
	raw, _ := json.Marshal(o)
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&v)
	raw, _ = json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type CredentialPasswordAlignmentObservation struct {
	Changed                         bool                                 `json:"-"`
	SchemaVersion                   string                               `json:"schema_version"`
	Operation                       string                               `json:"operation"`
	ActionID                        string                               `json:"action_intent_id"`
	OriginalActionID                string                               `json:"original_action_intent_id"`
	RequestSHA256                   string                               `json:"request_sha256"`
	Classification                  string                               `json:"classification"`
	Effect                          string                               `json:"effect"`
	Closed                          bool                                 `json:"closed"`
	ProtectedFileUnchanged          bool                                 `json:"protected_file_unchanged"`
	AttributesUnchanged             bool                                 `json:"attributes_unchanged"`
	ConfigurationAccepted           bool                                 `json:"configuration_accepted"`
	CredentialQualificationAccepted bool                                 `json:"credential_qualification_accepted"`
	Catalog                         *policy.CredentialPasswordCatalogPin `json:"catalog"`
	ResolvedRecoveryActions         []string                             `json:"resolved_recovery_actions"`
}

func decodeCredentialPasswordAlignmentObservation(raw []byte, r policy.CredentialPasswordAlignmentRequest) (CredentialPasswordAlignmentObservation, error) {
	var o CredentialPasswordAlignmentObservation
	bad := func(stage ...string) (CredentialPasswordAlignmentObservation, error) {
		code := "observation_invalid"
		if len(stage) == 1 {
			code += ":" + stage[0]
		}
		return o, credentialPasswordAlignError(code)
	}
	if len(raw) == 0 || len(raw) > 32768 || !credentialVerifyUniqueJSON(raw) || !credentialPasswordAlignRequiredFields(raw, reflect.TypeOf(o)) {
		return bad("json_shape")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&o) != nil {
		return bad("json_decode")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return bad("trailing_json")
	}
	if o.RequestSHA256 != policy.CredentialPasswordAlignmentDigest(r) {
		return bad("request_digest")
	}
	if o.SchemaVersion != "compound.credential_password_alignment_observation.v0.1" || o.Operation != r.Operation || o.RequestSHA256 != policy.CredentialPasswordAlignmentDigest(r) || !o.Closed || !o.ProtectedFileUnchanged || !o.AttributesUnchanged || o.ConfigurationAccepted || o.CredentialQualificationAccepted {
		return bad("protocol")
	}
	actionName := regexp.MustCompile(`^act_[0-9a-f]{12}$`)
	if !actionName.MatchString(o.ActionID) || !actionName.MatchString(o.OriginalActionID) || o.ResolvedRecoveryActions == nil || len(o.ResolvedRecoveryActions) > 64 {
		return bad("action_identity")
	}
	seen := map[string]bool{}
	for _, action := range o.ResolvedRecoveryActions {
		if !actionName.MatchString(action) || action == o.ActionID || action == o.OriginalActionID || seen[action] {
			return bad()
		}
		seen[action] = true
	}
	if (r.Operation == "inspect" || r.Operation == "align") && len(o.ResolvedRecoveryActions) != 0 {
		return bad()
	}
	switch r.Operation {
	case "inspect":
		if o.Classification != "metadata_only" || o.Effect != "observed" || o.OriginalActionID != o.ActionID || o.Catalog == nil || o.Catalog.DatabaseOID == 0 || o.Catalog.Roles[0].OID == 0 || o.Catalog.Roles[1].OID == 0 || o.Catalog.DockerSHA256 != r.Catalog.DockerSHA256 {
			return bad()
		}
	case "align":
		if o.Classification != "new_new" || o.Effect != "committed" || o.OriginalActionID != o.ActionID {
			return bad()
		}
	case "reconcile":
		if (o.Classification != "new_new" && o.Classification != "old_old") || o.Effect != "reconciled_"+o.Classification || r.Origin == nil || o.OriginalActionID != r.Origin.ActionID {
			return bad()
		}
	case "restore":
		if o.Classification != "new_new" || o.Effect != "restored" || r.Origin == nil || o.OriginalActionID != r.Origin.ActionID {
			return bad()
		}
	default:
		return bad()
	}
	if r.Operation != "inspect" && o.Catalog != nil {
		return bad()
	}
	o.Changed = r.Operation == "align" || r.Operation == "restore"
	return o, nil
}

func credentialPasswordAlignRequiredFields(raw json.RawMessage, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		return credentialPasswordAlignRequiredFields(raw, t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		count := 0
		for _, f := range reflect.VisibleFields(t) {
			if f.Anonymous && f.Tag.Get("json") == "" {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				return false
			}
			count++
			value, ok := fields[name]
			if !ok || !credentialPasswordAlignRequiredFields(value, f.Type) {
				return false
			}
		}
		return len(fields) == count
	case reflect.Array, reflect.Slice:
		if t.Kind() == reflect.Slice && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		if t.Kind() == reflect.Array && len(values) != t.Len() {
			return false
		}
		for _, value := range values {
			if !credentialPasswordAlignRequiredFields(value, t.Elem()) {
				return false
			}
		}
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
