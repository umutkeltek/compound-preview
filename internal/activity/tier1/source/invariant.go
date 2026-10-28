package source

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"compound/internal/activity"
)

// ExtendedForbiddenKeys is the superset of banned keys we check at the Tier-1 boundary.
// It starts from activity.ForbiddenMetaKeys (the schema-level F5 gate) and ADDS keys that
// would otherwise leak content through a Tier-1 envelope:
//
//   - "file" — VS Code's `aw-watcher-vscode` payload includes `file` (the open file path).
//     Even the file PATH is content-bearing per compound doctrine (paths can leak
//     usernames, project structure, sensitive filenames).
//   - "command_args" / "script" / "initial_command" — terminal command lines are content.
//   - "env" — environment variables can carry secrets.
//   - "language" — VS Code's `language` field is content-adjacent (e.g., reveals project type).
//
// The list is a closed set per the F5 contract; additions are a schema-level decision.
var ExtendedForbiddenKeys = func() map[string]bool {
	m := make(map[string]bool, len(activity.ForbiddenMetaKeys)+5)
	for k := range activity.ForbiddenMetaKeys {
		m[k] = true
	}
	m["file"] = true
	m["command_args"] = true
	m["script"] = true
	m["initial_command"] = true
	m["env"] = true
	m["language"] = true
	return m
}()

// CheckEnvelopeContentFree marshals the envelope to JSON and walks the resulting tree
// recursively. Any key in ExtendedForbiddenKeys at any depth causes a return value that
// names the offending path (e.g. "meta.title" or "fields[2].url"). Returns nil on a
// clean envelope.
//
// Cost: one json.Marshal + one map traversal. Acceptable for startup-time and per-call
// runtime checks.
func CheckEnvelopeContentFree(env Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}
	return walkForBannedKeys(generic, "")
}

// walkForBannedKeys recurses through generic JSON-decoded data, comparing each key
// (case-insensitive, trimmed) against ExtendedForbiddenKeys. Returns the first
// violation's path. Empty path is the root; segments are joined with ".".
//
// Walks maps and JSON arrays; scalars are leaf nodes.
func walkForBannedKeys(v any, path string) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			if ExtendedForbiddenKeys[strings.ToLower(strings.TrimSpace(k))] {
				return fmt.Errorf("banned key %q at path %q", k, childPath)
			}
			if err := walkForBannedKeys(x[k], childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range x {
			if err := walkForBannedKeys(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckRawContentFree applies the same invariant to a raw JSON byte slice. Useful when
// a source has already marshaled its envelope and wants to verify before returning.
// Same first-violation-wins semantics as CheckEnvelopeContentFree.
func CheckRawContentFree(b []byte) error {
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return fmt.Errorf("unmarshal raw: %w", err)
	}
	return walkForBannedKeys(generic, "")
}
