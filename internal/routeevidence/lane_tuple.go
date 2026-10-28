package routeevidence

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ResolveModelCapabilityTuple resolves a Compound-owned executable lane tuple
// by strict precedence: model id, alias, then harness id. Each level must have
// exactly one match; ambiguity fails closed instead of selecting whichever map
// row happened to iterate first.
func ResolveModelCapabilityTuple(manifest ModelCapabilitiesManifest, selector string) (string, ModelCapabilityEntry, error) {
	type match struct {
		id    string
		entry ModelCapabilityEntry
	}
	levels := make([][]match, 3)
	wanted := normalizeModelCapabilitySelector(selector)
	ids := make([]string, 0, len(manifest.Models))
	for id := range manifest.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := manifest.Models[id]
		candidate := match{id: id, entry: entry}
		if normalizeModelCapabilitySelector(id) == wanted {
			levels[0] = append(levels[0], candidate)
		}
		for _, alias := range entry.Aliases {
			if normalizeModelCapabilitySelector(alias) == wanted {
				levels[1] = append(levels[1], candidate)
				break
			}
		}
		if normalizeModelCapabilitySelector(entry.Harness) == wanted {
			levels[2] = append(levels[2], candidate)
		}
	}
	for _, matches := range levels {
		if len(matches) == 1 {
			return matches[0].id, matches[0].entry, nil
		}
		if len(matches) > 1 {
			matchedIDs := make([]string, 0, len(matches))
			for _, match := range matches {
				matchedIDs = append(matchedIDs, match.id)
			}
			return "", ModelCapabilityEntry{}, fmt.Errorf("ambiguous Compound lane tuple for %q: %s", selector, strings.Join(matchedIDs, ", "))
		}
	}
	return "", ModelCapabilityEntry{}, fmt.Errorf("no Compound lane tuple for %q", selector)
}

// ResolveModelCapabilityExecutable selects the first declared alias that is
// executable in the current process environment. Aliases are the tuple's
// launch contract; no harness-specific wrapper table exists beside it.
func ResolveModelCapabilityExecutable(aliases []string) (string, error) {
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if command, err := exec.LookPath(alias); err == nil {
			return command, nil
		}
	}
	return "", fmt.Errorf("no executable tuple alias found in PATH (aliases: %s)", strings.Join(aliases, ", "))
}

func normalizeModelCapabilitySelector(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
}
