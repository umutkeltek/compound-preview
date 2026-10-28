package policy

import "strings"

// HasOperatorProvenance recognizes the actor shapes Compound already treats
// as an operator or human act. It is provenance, not OS-level authentication;
// authority-bearing adapters use it to prevent ordinary executor identities
// from accidentally minting or releasing operator-only controls.
func HasOperatorProvenance(actor string) bool {
	actor = strings.ToLower(strings.TrimSpace(actor))
	return actor == "operator" ||
		strings.HasPrefix(actor, "operator-") ||
		strings.HasPrefix(actor, "operator:") ||
		strings.HasPrefix(actor, "human-") ||
		strings.HasPrefix(actor, "human:")
}
