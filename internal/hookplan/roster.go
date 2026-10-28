package hookplan

import (
	"fmt"

	"compound/internal/lanemanifest"
)

// RosterEventSet returns the native hook events a roster entitles a lane to.
// The set is expressed in the Claude-Code event vocabulary; harness families
// that compile fewer events (e.g. codex has no SessionEnd) simply have those
// events absent from their compiled bindings, so intersecting the compiled
// bindings with this set is family-agnostic.
//
// minimal is the interactive default: session-start (authority attestation) plus
// pre-tool-use (landing/deny gates). full is the governed selection: every
// lifecycle event compound can materialize (SessionEnd and UserPromptSubmit
// included). Stop is deliberately absent -- compound never registers it.
func RosterEventSet(roster lanemanifest.Roster) (map[string]bool, error) {
	switch roster {
	case lanemanifest.RosterMinimal:
		return map[string]bool{"SessionStart": true, "PreToolUse": true}, nil
	case lanemanifest.RosterFull:
		return map[string]bool{
			"SessionStart":     true,
			"UserPromptSubmit": true,
			"PreToolUse":       true,
			"PostToolUse":      true,
			"SessionEnd":       true,
		}, nil
	default:
		return nil, fmt.Errorf("unknown hook roster %q (minimal | full)", roster)
	}
}

// selectBindings filters a compiled posture's bindings to the roster's event
// set. A nil allowedEvents means every compiled binding (the legacy default).
// The posture's intentions are untouched -- semantic validation reads the full
// posture, only the installed native bindings are narrowed.
func selectBindings(bindings []Binding, allowedEvents map[string]bool) []Binding {
	if allowedEvents == nil {
		return bindings
	}
	out := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		if allowedEvents[binding.Event] {
			out = append(out, binding)
		}
	}
	return out
}
