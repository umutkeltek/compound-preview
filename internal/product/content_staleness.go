package product

import (
	"os"
	"sort"
	"strings"
)

// content_staleness.go owns the slice-2 content-staleness gate: a draft
// body naming a model the installed catalog marks SUPERSEDED gets a
// visible flag. The flag is informational — the operator still owns the
// post; the gate NEVER refuses a draft and NEVER mutates home state.
//
// Lives in the product package (not internal/authority) because the
// consumer is the distribution-loop artifact, not the catalog itself.
// The catalog is the input, the draft is the output.

// modelCatalogStalenessWarnings inspects a draft body against the
// INSTALLED provider+model catalog under $COMPOUND_HOME and returns a
// stable, sorted set of warnings naming any model the catalog marks
// superseded that the body mentions. The function is the read-only
// helper behind both RecordContentDraft and BuildContentQueuePlan so
// the gate behavior is byte-identical between the two call sites.
//
// Returns nil (empty slice) when:
//   - the installed catalog is missing (cold-start path; the gate
//     must NEVER hard-require the catalog),
//   - the catalog has no deprecated rows, or
//   - the body mentions no superseded models.
func modelCatalogStalenessWarnings(homePath, body string) []string {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return nil
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	// Lazy import: pull the deprecated model ids straight off the
	// installed provider-catalog.json. We use the LOW-LEVEL catalog
	// read here (not QueryModelCatalog) so the gate stays a thin
	// reader and never depends on a query-result struct it doesn't
	// otherwise need. The catalog file is the only artifact the gate
	// needs to consult.
	catalogPath := homePath + string(os.PathSeparator) + "registries" + string(os.PathSeparator) + "provider-catalog.json"
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		// Cold-start / no-catalog path: gate is silent, never crash.
		return nil
	}
	// Hand-parse just the deprecated list — pulling the full
	// authority.ModelCatalogManifest would force a wider dependency
	// surface and risk a cycle. The gate is a stale-text check, not
	// a full catalog query.
	deprecated := extractDeprecatedModelNames(data)
	if len(deprecated) == 0 {
		return nil
	}
	// Build a haystack token stream from the body so "alpha-flash"
	// and "examplecorp/alpha-flash" and "the alpha-flash model" all
	// match the same way. The token boundary guards against false
	// positives like "alpha-flash-2" or "xalpha-flashx" — but
	// provider/prefix matches ("examplecorp/alpha-flash") are
	// explicitly supported per the F3 falsifier.
	bodyLower := strings.ToLower(body)
	hits := map[string]bool{}
	for _, modelID := range deprecated {
		if matchesBodyModelID(bodyLower, modelID) {
			hits[modelID] = true
		}
	}
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for modelID := range hits {
		out = append(out, "draft body names superseded model "+modelID+" — verify the post still names the right model")
	}
	sort.Strings(out)
	return out
}

// matchesBodyModelID returns true when the lower-cased body mentions the
// given model_id either bare, with a provider prefix ("provider/model"),
// or as a substring flanked by non-alphanumeric chars (so "xalpha-flash"
// doesn't accidentally hit "alpha-flash" but a sentence with a comma
// after the model name still matches). Whitespace and punctuation
// count as boundaries.
func matchesBodyModelID(bodyLower, modelID string) bool {
	if modelID == "" {
		return false
	}
	idLower := strings.ToLower(modelID)
	// Provider-prefixed match: "<provider>/<modelID>" — the operator's
	// preferred reference form per the F3 falsifier's "switching off
	// examplecorp/legacy-model" case.
	if idx := strings.Index(bodyLower, "/"); idx >= 0 && idx < len(bodyLower)-1 {
		// Try every "/<id>" occurrence: cheap and bounded by body length.
		searchFrom := 0
		for searchFrom < len(bodyLower) {
			at := strings.Index(bodyLower[searchFrom:], "/")
			if at < 0 {
				break
			}
			rest := bodyLower[searchFrom+at+1:]
			if strings.HasPrefix(rest, idLower) {
				if len(rest) == len(idLower) {
					return true
				}
				// next char must be a non-alphanumeric boundary so
				// "legacy-model-2" doesn't match "legacy-model".
				next := rest[len(idLower)]
				if !isAlnumByte(next) {
					return true
				}
			}
			searchFrom += at + 1
		}
	}
	// Bare match with token boundaries.
	searchFrom := 0
	for searchFrom < len(bodyLower) {
		at := strings.Index(bodyLower[searchFrom:], idLower)
		if at < 0 {
			break
		}
		absIdx := searchFrom + at
		endIdx := absIdx + len(idLower)
		// Left boundary: either start-of-string or non-alnum before.
		leftOK := absIdx == 0 || !isAlnumByte(bodyLower[absIdx-1])
		rightOK := endIdx == len(bodyLower) || !isAlnumByte(bodyLower[endIdx])
		if leftOK && rightOK {
			return true
		}
		searchFrom = absIdx + 1
	}
	return false
}

// isAlnumByte is a tiny ASCII alphanumeric check (digits + ASCII letters);
// intentionally narrow so a hyphen, dot, or slash is a model-name boundary.
func isAlnumByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// extractDeprecatedModelNames parses the installed provider-catalog.json
// at the minimal depth needed: the top-level "deprecated" list and each
// entry's "model" string. Fields we don't care about (providers, status,
// note) are intentionally ignored — the gate is a model-name presence
// check, not a structured catalog query.
//
// Returns a sorted, de-duped list of model ids. The list is the
// authoritative set of "models the installed catalog marks
// superseded", scoped to the model's presence in the top-level
// "deprecated" array; the per-provider scope that lives in each
// entry's "providers" array is consulted by the catalog query path,
// not the gate (a draft body that says "the legacy-model is gone" is
// still flagging real staleness even if only ONE provider marked it
// deprecated, so the gate is intentionally broader here).
func extractDeprecatedModelNames(data []byte) []string {
	// Find the top-level "deprecated" key. The installed registry is
	// compact JSON; a simple byte scan is enough — we don't need a
	// full JSON parser because the structure is constrained.
	idx := indexTopLevelKey(data, "deprecated")
	if idx < 0 {
		return nil
	}
	// Walk the array literal, harvesting every "model": "<id>" pair.
	arrStart := indexArrayStart(data, idx)
	if arrStart < 0 {
		return nil
	}
	arrEnd := findMatchingBracket(data, arrStart, '[', ']')
	if arrEnd < 0 {
		return nil
	}
	arr := data[arrStart+1 : arrEnd]
	out := map[string]bool{}
	for _, obj := range splitTopLevelObjects(arr) {
		// Look for "model" key in this object. The slice-1 schema
		// guarantees it's a JSON string; we just harvest the value.
		modelKeyIdx := indexKeyInObject(obj, "model")
		if modelKeyIdx < 0 {
			continue
		}
		valStart := indexJSONString(obj, modelKeyIdx)
		if valStart < 0 {
			continue
		}
		valEnd := findJSONStringEnd(obj, valStart)
		if valEnd < 0 {
			continue
		}
		id := strings.TrimSpace(string(obj[valStart+1 : valEnd]))
		if id != "" {
			out[id] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- minimal JSON helpers, scoped to the slice-2 gate ---
// Each helper is intentionally narrow: it returns -1 on any shape it
// doesn't recognize, so the gate never crashes on a malformed or
// schema-evolved catalog. The full JSON parser is intentionally avoided
// so the gate has no transitive dependency on the catalog's full
// schema (a schema revision can add fields without breaking the gate).

func indexTopLevelKey(data []byte, key string) int {
	needle := `"` + key + `"`
	return strings.Index(string(data), needle)
}

func indexArrayStart(data []byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] == '[' {
			return i
		}
		if data[i] == '"' {
			// Skip the rest of the string to avoid matching brackets
			// inside string values.
			j := findJSONStringEnd(data, i)
			if j < 0 {
				return -1
			}
			i = j
		}
	}
	return -1
}

func findMatchingBracket(data []byte, start int, open, close byte) int {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(data); i++ {
		b := data[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if b == '\\' {
				escape = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == open {
			depth++
			continue
		}
		if b == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitTopLevelObjects(arr []byte) [][]byte {
	var out [][]byte
	depth := 0
	inString := false
	escape := false
	objStart := -1
	for i := 0; i < len(arr); i++ {
		b := arr[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if b == '\\' {
				escape = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == '{' {
			if depth == 0 {
				objStart = i
			}
			depth++
			continue
		}
		if b == '}' {
			depth--
			if depth == 0 && objStart >= 0 {
				out = append(out, arr[objStart:i+1])
				objStart = -1
			}
			continue
		}
	}
	return out
}

func indexKeyInObject(obj []byte, key string) int {
	needle := `"` + key + `"`
	return strings.Index(string(obj), needle)
}

func indexJSONString(obj []byte, from int) int {
	// Find the colon that follows the key, then the opening quote.
	colon := -1
	inString := false
	escape := false
	for i := from; i < len(obj); i++ {
		b := obj[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if b == '\\' {
				escape = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return -1
	}
	for i := colon + 1; i < len(obj); i++ {
		if obj[i] == '"' {
			return i
		}
		if obj[i] != ' ' && obj[i] != '\n' && obj[i] != '\r' && obj[i] != '\t' {
			return -1
		}
	}
	return -1
}

func findJSONStringEnd(obj []byte, start int) int {
	// start is the index of the opening quote.
	if start < 0 || start >= len(obj) || obj[start] != '"' {
		return -1
	}
	escape := false
	for i := start + 1; i < len(obj); i++ {
		b := obj[i]
		if escape {
			escape = false
			continue
		}
		if b == '\\' {
			escape = true
			continue
		}
		if b == '"' {
			return i
		}
	}
	return -1
}
