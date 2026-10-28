package act

import (
	"encoding/json"
	"testing"

	"compound/internal/policy"
)

func TestCredentialPasswordAlignmentObservationRequiresEveryFactAndFalseAcceptance(t *testing.T) {
	r := policy.CredentialPasswordAlignmentRequest{Operation: "align"}
	o := CredentialPasswordAlignmentObservation{SchemaVersion: "compound.credential_password_alignment_observation.v0.1", Operation: "align", ActionID: "act_0123456789ab", OriginalActionID: "act_0123456789ab", RequestSHA256: policy.CredentialPasswordAlignmentDigest(r), Classification: "new_new", Effect: "committed", Closed: true, ProtectedFileUnchanged: true, AttributesUnchanged: true, ResolvedRecoveryActions: []string{}}
	raw, _ := json.Marshal(o)
	if _, e := decodeCredentialPasswordAlignmentObservation(raw, r); e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for key := range fields {
		t.Run("missing_"+key, func(t *testing.T) {
			var missing map[string]json.RawMessage
			_ = json.Unmarshal(raw, &missing)
			delete(missing, key)
			body, _ := json.Marshal(missing)
			if _, e := decodeCredentialPasswordAlignmentObservation(body, r); e == nil {
				t.Fatal("missing fact was accepted")
			}
		})
	}
	for _, key := range []string{"configuration_accepted", "credential_qualification_accepted", "resolved_recovery_actions", "closed"} {
		var changed map[string]json.RawMessage
		_ = json.Unmarshal(raw, &changed)
		changed[key] = json.RawMessage("null")
		body, _ := json.Marshal(changed)
		if _, e := decodeCredentialPasswordAlignmentObservation(body, r); e == nil {
			t.Fatal("null was confused with explicit false/empty fact", key)
		}
	}
	for _, key := range []string{"configuration_accepted", "credential_qualification_accepted"} {
		var changed map[string]json.RawMessage
		_ = json.Unmarshal(raw, &changed)
		changed[key] = json.RawMessage("true")
		body, _ := json.Marshal(changed)
		if _, e := decodeCredentialPasswordAlignmentObservation(body, r); e == nil {
			t.Fatal("repair became credential qualification", key)
		}
	}
}
