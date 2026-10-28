package envelope

import "strings"

// ActionDeliveryBinding names immutable evidence for one non-source effect.
// It is not a work-wide exemption from Git delivery. The Work owner must resolve
// and validate the typed evidence and its binding action before using it at close.
// Omission preserves the byte shape of historical hash-bound WorkEnvelopes.
type ActionDeliveryBinding struct {
	ActionIntentID        string `json:"action_intent_id"`
	Kind                  string `json:"kind"`
	EvidenceRef           string `json:"evidence_ref"`
	BindingActionIntentID string `json:"binding_action_intent_id"`
}

const (
	DeliveryPrivateRuntime     = "private_runtime"
	DeliveryWorktreeRetirement = "worktree_retirement"
	DeliveryInstalledPackage   = "installed_package"
)

func validateActionDeliveryBindings(v *verr, bindings []ActionDeliveryBinding, actions []string) {
	owned := make(map[string]bool, len(actions))
	for _, id := range actions {
		owned[id] = true
	}
	seen := make(map[string]bool, len(bindings))
	for i, binding := range bindings {
		if binding.Kind != DeliveryPrivateRuntime && binding.Kind != DeliveryWorktreeRetirement && binding.Kind != DeliveryInstalledPackage {
			v.addf("non_source_delivery[%d].kind must be private_runtime, worktree_retirement or installed_package", i)
		}
		for _, field := range []struct{ name, id string }{
			{"action_intent_id", binding.ActionIntentID},
			{"binding_action_intent_id", binding.BindingActionIntentID},
		} {
			if !strings.HasPrefix(field.id, "act_") || strings.TrimSpace(field.id) != field.id || !owned[field.id] {
				v.addf("non_source_delivery[%d].%s must name an owned ActionIntent", i, field.name)
			}
		}
		if binding.ActionIntentID == binding.BindingActionIntentID {
			v.addf("non_source_delivery[%d] cannot bind its own action", i)
		}
		if seen[binding.ActionIntentID] {
			v.addf("non_source_delivery[%d] duplicates action %q", i, binding.ActionIntentID)
		}
		seen[binding.ActionIntentID] = true
		const prefix = "artifact:sha256:"
		if !strings.HasPrefix(binding.EvidenceRef, prefix) || !validSHA256(strings.TrimPrefix(binding.EvidenceRef, prefix)) {
			v.addf("non_source_delivery[%d].evidence_ref must name a canonical SHA-256 artifact", i)
		}
	}
}
