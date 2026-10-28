package narrate

import (
	"fmt"
	"strings"

	"compound/internal/reviewartifact"
)

// CheckScriptReadability enforces the shared eighth-grade readability ceiling
// over every prose field the daily narrate script serves: each beat body and
// unlock, the standing summary, and the quiet reason. It is the deterministic
// regression gate for the operator-legibility contract — daily narrate output
// must read without an agent translating. Prose attention echoes verbatim is
// owned upstream (the card writers and DerivedOperatorCard), so a failure
// here names the exact field and its grade, and a passing script guarantees
// nothing narrate composes drifted above the ceiling.
func CheckScriptReadability(script Script) error {
	check := func(label, text string) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if grade := reviewartifact.GradeLevel(text); grade > reviewartifact.ReadabilityGradeCeiling {
			return fmt.Errorf("narrate: %s reads at grade %.2f (ceiling %.1f): %q", label, grade, reviewartifact.ReadabilityGradeCeiling, text)
		}
		return nil
	}
	for _, beat := range script.Beats {
		if err := check("beat "+beat.ThreadID+" body", beat.Body); err != nil {
			return err
		}
		if err := check("beat "+beat.ThreadID+" unlock", beat.Unlock); err != nil {
			return err
		}
	}
	if err := check("standing summary", script.Standing.Summary); err != nil {
		return err
	}
	if script.Quiet != nil {
		if err := check("quiet reason", script.Quiet.Reason); err != nil {
			return err
		}
	}
	return nil
}
