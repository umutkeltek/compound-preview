package actuator

import (
	"context"
	"fmt"
	"strings"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

// GovernedHarnessSourceRefusal owns the explicit execution source-state rule.
// The CLI renders this refusal; the reservation enforces the same rule again.
func GovernedHarnessSourceRefusal(wenv envelope.WorkEnvelope, resumeVerifying bool) *LaneRefusal {
	expectedState := "claimed"
	if resumeVerifying {
		expectedState = "verifying"
	}
	if wenv.State != expectedState {
		return &LaneRefusal{
			Command:  "compound work advance --execute",
			Gate:     "execution_reservation",
			Refused:  fmt.Sprintf("work %s is %s; governed execution requires currently %s envelope", wenv.ID, wenv.State, expectedState),
			Tried:    []string{fmt.Sprintf("expected state %s, observed %s", expectedState, wenv.State)},
			DoThis:   []string{fmt.Sprintf("compound work show %s", wenv.ID), "use --resume-verifying for a fresh governed execution of verifying work; otherwise claim ready work"},
			Prevents: "implicit replay or concurrent execution; resume is an explicit fresh reservation, never a same-state launch.",
		}
	}
	return nil
}

// A verifying envelope may still reference a live or damaged execution. Refuse
// before reserving a new generation; neither sweep nor historical success grants
// authority. Terminal attempts cannot re-enter, and the work CAS catches changes
// to its linked history after this read.
func validateGovernedResumeHistory(in GovernedHarnessReservationInput) (string, error) {
	if in.ClaimedWork.State != "verifying" {
		return "", fmt.Errorf("governed resume requires verifying source state, got %s", in.ClaimedWork.State)
	}
	if in.RunInput.WorkID != in.ClaimedWork.ID {
		return "", fmt.Errorf("governed resume run input does not match source work")
	}
	current, err := in.Store.Get(in.ClaimedWork.ID)
	if err != nil {
		return "", fmt.Errorf("read current resume history: %w", err)
	}
	store := runattempt.Open(in.HomePath)
	var predecessorID string
	for _, id := range current.RunAttemptIDs {
		if !strings.HasPrefix(id, "ratt_") {
			return "", fmt.Errorf("governed resume has invalid attempt identity %q", id)
		}
		attempt, err := store.Get(id)
		if err != nil {
			return "", fmt.Errorf("governed resume cannot read prior attempt %s: %w", id, err)
		}
		if attempt.ID != id || attempt.WorkID != in.ClaimedWork.ID || !runattempt.IsTerminal(attempt.Status) {
			return "", fmt.Errorf("governed resume requires same-work terminal history: attempt=%s work=%s status=%s", id, attempt.WorkID, attempt.Status)
		}
		if attempt.Origin == runattempt.OriginHarnessExecution {
			predecessorID = id
		}
	}
	return predecessorID, nil
}

// The live autonomy halt must win at start as well as reservation. A read-only
// Claude lane check composes inside the same admission lock; wrapping two guarded
// callbacks would recursively acquire the lock. OS resumes need no lane callback.
func newGovernedResumeStartGuard(homePath, workID, policyID, actionID string, laneCheck func(context.Context) error) func(context.Context, func() error) error {
	return func(ctx context.Context, start func() error) error {
		if start == nil {
			return fmt.Errorf("governed resume start operation required")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			halted, reason, err := policy.AutonomyHalted(homePath, "build-implement")
			if err != nil {
				return fmt.Errorf("revalidate resume autonomy halt: %w", err)
			}
			if halted {
				return fmt.Errorf("autonomy halted before resume launch: %s", reason)
			}
			if laneCheck != nil {
				if err := laneCheck(ctx); err != nil {
					return err
				}
			}
			if err := RevalidateGovernedExecutionReservation(homePath, workID, policyID, actionID); err != nil {
				return err
			}
			return start()
		})
	}
}
