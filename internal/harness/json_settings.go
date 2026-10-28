package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// JSON-target settings ownership.
//
// A PermissionAuthority for a TOML target already declares owned keys as dotted
// paths (`features.guardian_approval`), and renderTOMLPermissions edits exactly
// those lines. JSON targets had no equivalent: the render path managed only the
// `permissions` and `autoMode` objects, so every other key in a settings.json --
// model, env, statusLine, output style -- was unownable and drifted silently.
//
// This gives the JSON branch the same surgical contract. Ownership is per-path,
// never whole-file: an owned path is written, a path Compound previously owned
// and no longer declares is removed, and every other key in the file is left
// exactly as the harness wrote it. That boundary is what makes it safe to share
// settings.json with an application that also writes to it.
const jsonManagedSettingPrefix = "settings:"

// jsonSettingScalar renders one declared value for storage in the managed-entry
// record. Only scalars are accepted: the dotted path IS the nesting mechanism,
// so a map or slice value would express structure twice and make removal
// ambiguous (which leaf did Compound own?).
func jsonSettingScalar(path string, value any) (string, error) {
	switch value.(type) {
	case string, bool, int, int32, int64, float32, float64, nil:
	default:
		return "", fmt.Errorf("settings path %q must be a scalar; use a dotted path for nested keys", path)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("settings path %q: %w", path, err)
	}
	return string(encoded), nil
}

// ValidateJSONSettingsPaths rejects an authority whose declared settings cannot
// be rendered, at load time rather than at write time.
func ValidateJSONSettingsPaths(owned map[string]any) error {
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("settings path must not be empty")
		}
		for _, segment := range strings.Split(path, ".") {
			if strings.TrimSpace(segment) == "" {
				return fmt.Errorf("settings path %q has an empty segment", path)
			}
		}
		if _, err := jsonSettingScalar(path, owned[path]); err != nil {
			return err
		}
	}
	return nil
}

// setJSONPath writes value at a dotted path, creating intermediate objects.
// A non-object sitting where an intermediate object must go is replaced: the
// authority declares the shape, and refusing here would leave the target
// permanently un-reconcilable with no operator-visible way forward.
func setJSONPath(root map[string]any, path string, value any) {
	segments := strings.Split(path, ".")
	cursor := root
	for _, segment := range segments[:len(segments)-1] {
		next, ok := cursor[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			cursor[segment] = next
		}
		cursor = next
	}
	cursor[segments[len(segments)-1]] = value
}

// deleteJSONPath removes a dotted path and any intermediate objects it leaves
// empty, so relinquishing the last owned key under `env` does not strand an
// empty `env: {}` that reads as deliberate configuration.
func deleteJSONPath(root map[string]any, path string) {
	segments := strings.Split(path, ".")
	chain := []map[string]any{root}
	cursor := root
	for _, segment := range segments[:len(segments)-1] {
		next, ok := cursor[segment].(map[string]any)
		if !ok {
			return
		}
		chain = append(chain, next)
		cursor = next
	}
	delete(cursor, segments[len(segments)-1])
	for i := len(chain) - 1; i > 0; i-- {
		if len(chain[i]) > 0 {
			return
		}
		delete(chain[i-1], segments[i-1])
	}
}

// applyJSONSettingsPaths reconciles owned dotted paths into a decoded settings
// object and returns the managed-entry record for the rendered artifact.
//
// Two invariants carry the whole safety argument and each has a falsifier:
//
//  1. Only declared paths and previously-declared paths are touched. A key the
//     harness owns is never read, written, or removed, which is what allows
//     Compound and Claude Code to write the same file.
//  2. A path Compound previously owned and no longer declares is REMOVED, not
//     abandoned. Abandoning it would leave a Compound-authored value in place
//     with nothing claiming it -- indistinguishable from an operator's own
//     setting on the next read, and the way an authority silently accumulates
//     orphans.
func applyJSONSettingsPaths(settings map[string]any, owned map[string]any, previous map[string][]string) (map[string][]string, error) {
	for key := range previous {
		if !strings.HasPrefix(key, jsonManagedSettingPrefix) {
			continue
		}
		path := strings.TrimPrefix(key, jsonManagedSettingPrefix)
		if _, stillOwned := owned[path]; stillOwned {
			continue
		}
		deleteJSONPath(settings, path)
	}

	managed := map[string][]string{}
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		encoded, err := jsonSettingScalar(path, owned[path])
		if err != nil {
			return nil, err
		}
		setJSONPath(settings, path, owned[path])
		managed[jsonManagedSettingPrefix+path] = []string{encoded}
	}
	return managed, nil
}

// jsonSettingsDrift reports declared paths whose live value does not match the
// authority. It reads the same decoded object the caller validated, never the
// file again.
func jsonSettingsDrift(settings map[string]any, owned map[string]any) []string {
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var details []string
	for _, path := range paths {
		want, err := jsonSettingScalar(path, owned[path])
		if err != nil {
			details = append(details, fmt.Sprintf("settings.%s is not renderable: %v", path, err))
			continue
		}
		got, found := lookupJSONPath(settings, path)
		if !found {
			details = append(details, fmt.Sprintf("settings.%s missing, want %s", path, want))
			continue
		}
		encoded, err := json.Marshal(got)
		if err != nil || string(encoded) != want {
			details = append(details, fmt.Sprintf("settings.%s is %s, want %s", path, string(encoded), want))
		}
	}
	return details
}

func lookupJSONPath(root map[string]any, path string) (any, bool) {
	segments := strings.Split(path, ".")
	cursor := root
	for _, segment := range segments[:len(segments)-1] {
		next, ok := cursor[segment].(map[string]any)
		if !ok {
			return nil, false
		}
		cursor = next
	}
	value, ok := cursor[segments[len(segments)-1]]
	return value, ok
}
