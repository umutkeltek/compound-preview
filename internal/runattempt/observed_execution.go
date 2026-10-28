package runattempt

import (
	"errors"
	"fmt"
	"strings"
)

// ObservedExecution is the OBSERVED leg of an attempt's selection: what the
// launch could read back about the process it was about to start (argv the
// adapter built, env the wrapper exports, the hook posture installed for the
// harness). It sits next to ExecutionSelection, which records only what was
// requested, and it never rewrites that snapshot: requested and observed are
// two facts, and a reader compares them rather than trusting either alone.
//
// The block is append-once. A launch that observed nothing records nothing --
// the field stays nil (absent from the JSON), never a zeroed struct, so a
// reader cannot mistake "not observed" for "observed empty".
type ObservedExecution struct {
	ObservedModelID   string `json:"observed_model_id,omitempty"`
	ObservedEffort    string `json:"observed_effort,omitempty"`
	ObservedSessionID string `json:"observed_session_id,omitempty"`
	// ObservedHookPosture is the live Compound PreToolUse posture installed
	// for the harness: enforce | observe | none.
	ObservedHookPosture string `json:"observed_hook_posture,omitempty"`
	// Source names where each value was read, e.g.
	// "argv:-m,argv:model_reasoning_effort,hook_config:<path>".
	Source string `json:"source"`
}

// ObservedHookPostures are the legal ObservedHookPosture values.
var ObservedHookPostures = []string{"enforce", "observe", "none"}

var errObservedExecutionSet = errors.New("observed execution already set with different content")

// Empty reports whether nothing was observed; an empty observation is never stamped.
func (o ObservedExecution) Empty() bool {
	return o.ObservedModelID == "" && o.ObservedEffort == "" && o.ObservedSessionID == "" && o.ObservedHookPosture == ""
}

// Validate refuses an observation that names no value or no source.
func (o ObservedExecution) Validate() error {
	var errs []string
	if o.Empty() {
		errs = append(errs, "at least one observed value is required")
	}
	if strings.TrimSpace(o.Source) == "" {
		errs = append(errs, "source is required")
	}
	if o.ObservedHookPosture != "" && !contains(ObservedHookPostures, o.ObservedHookPosture) {
		errs = append(errs, fmt.Sprintf("observed_hook_posture must be one of %v, got %q", ObservedHookPostures, o.ObservedHookPosture))
	}
	if len(errs) > 0 {
		return errors.New("observed execution: " + strings.Join(errs, "; "))
	}
	return nil
}

// SetObservedExecution stamps the observed leg once, before the subprocess
// starts. Like SetExecutionSelectionSnapshot it refuses a terminal attempt and
// is append-once: an identical repeat is a no-op, different content is refused.
func (s *Store) SetObservedExecution(id string, obs ObservedExecution) (RunAttempt, error) {
	if err := obs.Validate(); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt %s: %w", id, err)
	}
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		if ra.Observed != nil {
			if *ra.Observed == obs {
				return false, nil
			}
			return false, fmt.Errorf("runattempt %s: %w", id, errObservedExecutionSet)
		}
		copy := obs
		ra.Observed = &copy
		return true, nil
	})
}
