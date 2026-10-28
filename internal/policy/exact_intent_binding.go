package policy

import (
	"encoding/json"
	"sort"

	"compound/internal/envelope"
)

// CanonicalFullIntentDigest binds the whole intent, including input trust and
// confidentiality. Only the retry-volatile intent ID and timestamp are omitted.
// Copying the envelope keeps future fields bound by default.
func CanonicalFullIntentDigest(intent envelope.ActionIntent) string {
	intent.ID = ""
	intent.CreatedAt = ""
	intent.EvidenceRefs = sortedIntentStrings(intent.EvidenceRefs)
	intent.SecurityReasons = sortedIntentStrings(intent.SecurityReasons)
	intent.InputSources = append([]envelope.SourceBinding{}, intent.InputSources...)
	sort.Slice(intent.InputSources, func(i, j int) bool {
		left, _ := json.Marshal(intent.InputSources[i])
		right, _ := json.Marshal(intent.InputSources[j])
		return string(left) < string(right)
	})
	data, err := json.Marshal(intent)
	if err != nil {
		// Non-JSON costs (NaN/Inf) cannot form a persisted source or an exact
		// approval. The matching edge rejects an empty digest explicitly.
		return ""
	}
	return actionHash(data)
}

func sortedIntentStrings(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}
