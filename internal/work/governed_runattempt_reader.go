package work

import (
	"fmt"
	"path/filepath"
	"strings"

	"compound/internal/runattempt"
)

// LoadGovernedRunAttempt reads owned execution provenance through the same
// authority resolver used at close. It does not require success, committed
// launch, or a forward attachment; admission must check those separately.
// The resolver's exact side-effect-free policy-refusal exception is preserved.
func (s *Store) LoadGovernedRunAttempt(id string) (runattempt.RunAttempt, error) {
	if !runattempt.Valid(id) {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt: invalid owned id %q", id)
	}
	ra, err := runattempt.Open(s.home).Get(id)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt: %w", err)
	}
	if ra.ID != id || !strings.HasPrefix(ra.WorkID, "work_") || filepath.Base(ra.WorkID) != ra.WorkID {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt: stored identity mismatch")
	}
	if ra.Origin != runattempt.OriginHarnessExecution ||
		strings.TrimSpace(ra.ActionIntentID) == "" ||
		ra.ExecutionPosture == nil ||
		strings.TrimSpace(ra.ExecutionPosture.PolicyDecisionID) == "" {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt: explicit execution authority required")
	}
	w, err := s.Get(ra.WorkID)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt work: %w", err)
	}
	if w.ID != ra.WorkID {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt: work identity mismatch")
	}
	if err := w.Validate(); err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt work: %w", err)
	}
	if _, err := s.resolveRunAttemptAuthority(w, ra); err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("governed run attempt authority: %w", err)
	}
	return ra, nil
}
