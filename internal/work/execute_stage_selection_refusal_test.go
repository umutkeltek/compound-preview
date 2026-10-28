package work

import (
	"errors"
	"testing"
)

// A selection the harness cannot apply is a deterministic refusal, so the
// unattended seam must skip it like adapter_missing -- never claim the work
// and then burn it as a failed launch.
func TestParseLaunchRefusalRecognizesSelectionUnsupported(t *testing.T) {
	reason, detail := parseLaunchRefusal(errors.New("selection_unsupported:claude_minimax:effort"))
	if reason != "selection_unsupported" || detail != "claude_minimax:effort" {
		t.Fatalf("parseLaunchRefusal = (%q, %q), want (selection_unsupported, claude_minimax:effort)", reason, detail)
	}
}
