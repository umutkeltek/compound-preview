package act

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	"compound/internal/policy"
)

const credentialVerifyObservationSchema = "compound.credential_verification_observation.v0.1"

// Only this closed observation may cross the remote boundary or enter artifacts.
// Passwords, DSNs, password lengths/hashes and native diagnostics have no field.
type CredentialVerificationObservation struct {
	SchemaVersion             string                      `json:"schema_version"`
	RequestSHA256             string                      `json:"request_sha256"`
	ProgramSHA256             string                      `json:"program_sha256"`
	HostBefore                policy.CredentialVerifyHost `json:"host_before"`
	HostAfter                 policy.CredentialVerifyHost `json:"host_after"`
	FileMatched               bool                        `json:"file_matched"`
	Before                    credentialVerifyCatalog     `json:"before"`
	After                     credentialVerifyCatalog     `json:"after"`
	Authentication            []credentialVerifyAuth      `json:"authentication"`
	ConfigurationAccepted     bool                        `json:"configuration_accepted"`
	ApplicationAccessAccepted bool                        `json:"application_access_accepted"`
	SourceCaptureAccepted     bool                        `json:"source_capture_accepted"`
	MigrationAccepted         bool                        `json:"migration_accepted"`
}

type credentialVerifyCatalog struct {
	SystemIdentifier string                          `json:"system_identifier"`
	VersionNum       string                          `json:"version_num"`
	Port             int                             `json:"port"`
	Privileged       bool                            `json:"privileged"`
	Roles            []credentialVerifyCatalogRole   `json:"roles"`
	Database         credentialVerifyCatalogDatabase `json:"database"`
	Sessions         []credentialVerifySession       `json:"sessions"`
}
type credentialVerifyCatalogRole struct {
	Name            string   `json:"name"`
	Login           bool     `json:"login"`
	SCRAMPresent    bool     `json:"scram_present"`
	Superuser       bool     `json:"superuser"`
	Createdb        bool     `json:"createdb"`
	Createrole      bool     `json:"createrole"`
	Replication     bool     `json:"replication"`
	BypassRLS       bool     `json:"bypassrls"`
	Inherit         bool     `json:"inherit"`
	ConnectionLimit int      `json:"connection_limit"`
	RoleConfig      []string `json:"role_config"`
	ValidUntil      *string  `json:"valid_until"`
	MembershipRows  int      `json:"membership_rows"`
}
type credentialVerifyCatalogDatabase struct {
	Name               string `json:"name"`
	Owner              string `json:"owner"`
	ConnectionLimit    int    `json:"connection_limit"`
	AllowConnections   bool   `json:"allow_connections"`
	ApplicationConnect bool   `json:"application_connect"`
	PublicConnect      bool   `json:"public_connect"`
}
type credentialVerifySession struct {
	PID      int    `json:"pid"`
	User     string `json:"user"`
	Database string `json:"database"`
	Address  string `json:"address"`
}
type credentialVerifyAuth struct {
	Kind     string                    `json:"kind"`
	Role     string                    `json:"role"`
	Outcome  string                    `json:"outcome"`
	Identity *credentialVerifyIdentity `json:"identity"`
}
type credentialVerifyIdentity struct {
	User        string `json:"user"`
	SessionUser string `json:"session_user"`
	Database    string `json:"database"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
	VersionNum  string `json:"version_num"`
	PID         int    `json:"pid"`
}

func decodeCredentialVerificationObservation(raw []byte, r policy.CredentialVerificationRequest) (CredentialVerificationObservation, error) {
	var o CredentialVerificationObservation
	if len(raw) == 0 || len(raw) > 32768 {
		return o, errors.New("credential_verify:observation_size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&o) != nil {
		return o, errors.New("credential_verify:observation_shape")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !credentialVerifyUniqueJSON(raw) || !credentialVerifyRequiredFields(raw, reflect.TypeOf(o)) {
		return o, errors.New("credential_verify:observation_ambiguous")
	}
	if validateCredentialVerificationObservation(o, r) != nil {
		return o, errors.New("credential_verify:observation_invalid")
	}
	return o, nil
}

// False, null and zero are meaningful protocol facts; absence is not evidence
// of those facts. Require the full closed shape before validating values.
func credentialVerifyRequiredFields(raw json.RawMessage, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		return credentialVerifyRequiredFields(raw, t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		count := 0
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				return false
			}
			count++
			value, ok := fields[name]
			if !ok || !credentialVerifyRequiredFields(value, f.Type) {
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
			if !credentialVerifyRequiredFields(value, t.Elem()) {
				return false
			}
		}
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validateCredentialVerificationObservation(o CredentialVerificationObservation, r policy.CredentialVerificationRequest) error {
	bad := errors.New("credential_verify:observation_invalid")
	if o.SchemaVersion != credentialVerifyObservationSchema || o.RequestSHA256 != policy.CredentialVerificationDigest(r) || o.ProgramSHA256 != CredentialVerificationProgramSHA256() || o.HostBefore != r.Host || o.HostAfter != r.Host || !o.FileMatched || o.ConfigurationAccepted || o.ApplicationAccessAccepted || o.SourceCaptureAccepted || o.MigrationAccepted {
		return bad
	}
	for _, c := range []credentialVerifyCatalog{o.Before, o.After} {
		if c.SystemIdentifier != r.Host.SystemIdentifier || c.VersionNum != r.Host.VersionNum || c.Port != 5432 || !c.Privileged || len(c.Roles) != 2 {
			return bad
		}
		for i, role := range c.Roles {
			if role.Name != r.Roles[i].Name || !role.Login || !role.SCRAMPresent || role.Superuser || role.Createdb || role.Createrole || role.Replication || role.BypassRLS || !role.Inherit || role.ConnectionLimit != -1 || role.RoleConfig != nil || role.ValidUntil != nil || role.MembershipRows != 0 {
				return bad
			}
		}
		d := c.Database
		if d.Name != r.Database.Name || d.Owner != r.Roles[1].Name || d.ConnectionLimit != 0 || d.AllowConnections != r.Database.AllowConnections || d.ApplicationConnect || d.PublicConnect {
			return bad
		}
	}
	if o.Before.Sessions == nil || len(o.Before.Sessions) != 0 || len(o.After.Sessions) != 2 || len(o.Authentication) != 4 {
		return bad
	}
	for i, a := range o.Authentication {
		role := r.Roles[i/2]
		if a.Role != role.Name {
			return bad
		}
		if i%2 == 1 {
			if a.Kind != role.Kind+"_wrong" || a.Outcome != "password_rejected" || a.Identity != nil {
				return bad
			}
			continue
		}
		if a.Kind != role.Kind+"_existing" || a.Outcome != "authenticated" || a.Identity == nil {
			return bad
		}
		v := a.Identity
		if v.User != role.Name || v.SessionUser != role.Name || v.Database != "postgres" || v.Address != "127.0.0.1" || v.Port != 5432 || v.VersionNum != r.Host.VersionNum || v.PID <= 0 || v.PID > 2_147_483_647 {
			return bad
		}
		s := o.After.Sessions[i/2]
		if s.PID != v.PID || s.User != v.User || s.Database != v.Database || s.Address != v.Address {
			return bad
		}
	}
	if o.Authentication[0].Identity.PID == o.Authentication[2].Identity.PID {
		return bad
	}
	return nil
}

func credentialVerifyUniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var visit func() bool
	visit = func() bool {
		t, e := d.Token()
		if e != nil {
			return false
		}
		v, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch v {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return false
				}
				seen[s] = true
				if !visit() {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !visit() {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	return visit()
}
