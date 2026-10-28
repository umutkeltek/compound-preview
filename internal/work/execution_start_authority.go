package work

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// ValidateExecutionStartAuthority is a read-only launch boundary. The caller
// holds the autonomy-admission lock through the subprocess start callback.
// Admission evidence is preserved; a stale grant never persists new authority here.
func (s *Store) ValidateExecutionStartAuthority(workID, expectedPolicyDecisionID, expectedActionIntentID string, clock func() time.Time) error {
	workID = strings.TrimSpace(workID)
	expectedPolicyDecisionID = strings.TrimSpace(expectedPolicyDecisionID)
	expectedActionIntentID = strings.TrimSpace(expectedActionIntentID)
	if workID == "" || expectedPolicyDecisionID == "" || expectedActionIntentID == "" {
		return fmt.Errorf("governed execution reservation identity is incomplete")
	}
	current, err := s.Get(workID)
	if err != nil {
		return fmt.Errorf("revalidate governed execution reservation: %w", err)
	}
	if current.State != "running" || current.PolicyDecisionID != expectedPolicyDecisionID || latestActionIntentID(current) != expectedActionIntentID {
		return fmt.Errorf("governed execution reservation changed before launch: state=%s policy=%s action_intent=%s",
			current.State, current.PolicyDecisionID, latestActionIntentID(current))
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(expectedActionIntentID, &action); err != nil {
		return fmt.Errorf("governed execution action authority: %w", err)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(expectedPolicyDecisionID, &decision); err != nil {
		return fmt.Errorf("governed execution policy authority: %w", err)
	}
	if action.ID != expectedActionIntentID || action.IDs.WorkID != workID ||
		decision.ID != expectedPolicyDecisionID || decision.IDs.WorkID != workID ||
		decision.ActionIntentID != action.ID || !policy.Allowed(decision) {
		return fmt.Errorf("governed execution action/policy binding is invalid")
	}
	if err := action.Validate(); err != nil {
		return err
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return err
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	if err := validatePreEvaluatedReceiptedActionAt(action, decision, now); err != nil {
		return fmt.Errorf("governed execution authorization is no longer current: %w", err)
	}
	fresh := policy.Evaluate(action, cfg, now)
	if !policy.Allowed(fresh) {
		return fmt.Errorf("governed execution current policy %s before launch", fresh.Outcome)
	}
	return nil
}
