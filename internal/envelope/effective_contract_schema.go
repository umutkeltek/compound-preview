package envelope

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ValidateSchemaVersion classifies the digest codec before a caller hashes an
// EffectiveContract. Older unbound envelopes remain historical evidence; the
// instruction-pack reader separately requires a nonempty current context key.
func (e *EffectiveContract) ValidateSchemaVersion() error {
	switch e.SchemaVersion {
	case SVEffectiveContractV01:
		if e.ContextKey != "" {
			return fmt.Errorf("effective_contract v0.1 cannot carry context_key")
		}
	case SVEffectiveContract:
		if e.ContextKey != "" {
			decoded, err := hex.DecodeString(e.ContextKey)
			if err != nil || len(decoded) != 32 || e.ContextKey != strings.ToLower(e.ContextKey) {
				return fmt.Errorf("effective_contract context_key must be a lowercase SHA-256 digest")
			}
		}
	default:
		return fmt.Errorf("unsupported effective_contract schema_version %q", e.SchemaVersion)
	}
	return nil
}
