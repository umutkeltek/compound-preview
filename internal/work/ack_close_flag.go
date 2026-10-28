package work

import (
	"fmt"
	"strings"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// verificationRejectedOutcomeCompatible consumes only the exact sealed closing
// receipt loaded by the doctor. A work result rejected by verification may
// preserve successful execution; an acknowledgement is not evaluator identity.
func verificationRejectedOutcomeCompatible(w envelope.WorkEnvelope, ra runattempt.RunAttempt, closing exactTerminalClose) bool {
	if w.State != "failed" || ra.Status != runattempt.StatusSucceeded || ra.WorkID != w.ID ||
		closing.Receipt.Outcome != "failed" || closing.Receipt.IDs.WorkID != w.ID {
		return false
	}
	prefix := "verification_rejected:" + ra.ID + ":"
	for _, ref := range closing.Receipt.ProofRefs {
		if strings.HasPrefix(ref, prefix) && strings.TrimSpace(strings.TrimPrefix(ref, prefix)) != "" {
			return true
		}
	}
	return false
}

// ParseAcknowledgeAbandonedFlagPairs scans args linearly for
// --acknowledge-abandoned or --verification-rejected ratt_X --reason R
// pairs and returns one
// caller-owned terminalization reservation per pair. The reason is required
// and is later stamped on the receipt as `abandon_ack:<id>:<reason>` or
// `verification_rejected:<id>:<reason>` so the
// operator-visible audit carries the explicit acknowledgement (see
// buildRunAttemptTerminalizationPlans). Refused if --acknowledge-abandoned
// is present without a following ratt_ id, or if the --reason paired with
// it is empty. The CLI does not consult RunAttempt status here -- the
// close-time gate is the only authority on whether the acknowledgement is
// legal for the specific attempt status; this parser only guarantees the
// pair is well-formed at the door.
//
// stealReason is unused; the steal --reason (if any) is independent of
// acknowledgement reasons and is stamped separately by the close-time gate
// via CloseInput.StealReason. The parameter is preserved verbatim so the
// call site signature does not change.
// When --verification-rejected is present, each acknowledgement takes its
// first following --reason before the next acknowledgement flag. Each pair
// needs its own reason; abandoned-only parsing retains its existing behavior.
func ParseAcknowledgeAbandonedFlagPairs(args []string, stealReason string) ([]RunAttemptTerminalizationRequest, error) {
	var out []RunAttemptTerminalizationRequest
	boundedReasons := false
	for _, arg := range args {
		boundedReasons = boundedReasons || arg == "--verification-rejected"
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if flag != "--acknowledge-abandoned" && flag != "--verification-rejected" {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s requires a following ratt_ id", flag)
		}
		id := strings.TrimSpace(args[i+1])
		if id == "" {
			return nil, fmt.Errorf("%s ratt_ id must be non-empty", flag)
		}
		i++
		// Scan ahead for the nearest --reason R whose R is non-empty and
		// whose R is not itself a flag (so the value can carry colons).
		reason := ""
		for j := i + 1; j+1 < len(args); j++ {
			if boundedReasons && (args[j] == "--acknowledge-abandoned" || args[j] == "--verification-rejected") {
				break
			}
			if args[j] == "--reason" {
				if strings.HasPrefix(args[j+1], "--") {
					return nil, fmt.Errorf("%s %s: --reason value must not start with --", flag, id)
				}
				reason = strings.TrimSpace(args[j+1])
				break
			}
		}
		if reason == "" {
			return nil, fmt.Errorf("%s %s requires a non-empty --reason", flag, id)
		}
		owner, status := runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned, runattempt.StatusAbandoned
		if flag == "--verification-rejected" {
			owner, status = runattempt.TerminalizationOwnerWorkVerificationRejected, runattempt.StatusSucceeded
		}
		out = append(out, RunAttemptTerminalizationRequest{
			RunAttemptID: id,
			Owner:        owner,
			TargetStatus: status,
			Reason:       reason,
		})
	}
	_ = stealReason
	return out, nil
}

// validateVerificationRejectedRequests keeps execution status separate from
// verification outcome. It runs before close's state transition check.
func (s *Store) validateVerificationRejectedRequests(w envelope.WorkEnvelope, in CloseInput) error {
	for _, request := range in.RunAttemptTerminalizations {
		if strings.TrimSpace(request.Owner) != runattempt.TerminalizationOwnerWorkVerificationRejected {
			continue
		}
		if in.Outcome != "failed" {
			return fmt.Errorf("--verification-rejected requires outcome failed, got %q", in.Outcome)
		}
		if w.State != "verifying" {
			return fmt.Errorf("--verification-rejected requires work state verifying, got %q", w.State)
		}
		id := strings.TrimSpace(request.RunAttemptID)
		if strings.TrimSpace(request.Reason) == "" {
			return fmt.Errorf("--verification-rejected %s requires non-empty --reason", id)
		}
		attached := false
		for _, attachedID := range w.RunAttemptIDs {
			attached = attached || attachedID == id
		}
		if !attached {
			return fmt.Errorf("--verification-rejected %s is not attached to work %s", id, w.ID)
		}
		ra, err := s.runAttempts().Get(id)
		if err != nil {
			return fmt.Errorf("--verification-rejected %s unreadable: %w", id, err)
		}
		if ra.Status != runattempt.StatusSucceeded || strings.TrimSpace(request.TargetStatus) != runattempt.StatusSucceeded {
			return fmt.Errorf("--verification-rejected %s requires terminal succeeded attempt and target_status succeeded, got %q/%q", id, ra.Status, request.TargetStatus)
		}
	}
	return nil
}
