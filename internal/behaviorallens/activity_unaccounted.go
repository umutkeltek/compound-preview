package behaviorallens

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ActivityUnaccountedLensID is the id of the Tier-0 activity reconciliation lens.
const ActivityUnaccountedLensID = "activity-unaccounted"

// MethodLedgerReconciliation names the measurement: contiguous activity blocks reconciled against the
// compound work ledger; a block with no overlapping envelope/receipt is "unaccounted".
const MethodLedgerReconciliation = "ledger_reconciliation"

// ActivityUnaccountedLens is an OPERATOR_PRIVATE lens: it may infer the operator's real activity, so
// it is default-off (a stranger clone runs it only if this home opts in — the Compose visibility gate
// is the single enforcement point). It is EVIDENCE ONLY — no threshold, no gate. It infers over a
// day's PRE-COMPUTED unaccounted blocks (produced deterministically by internal/activity and passed
// as the Observation payload); the lens itself makes no provider call and does no I/O.
type ActivityUnaccountedLens struct{}

func (ActivityUnaccountedLens) ID() string             { return ActivityUnaccountedLensID }
func (ActivityUnaccountedLens) Visibility() Visibility { return VisibilityOperatorPrivate }

// ActivityUnaccountedPayload is the Observation payload the lens reads: one day's unaccounted blocks
// plus the work refs reconciled against. It mirrors internal/activity.DayInference by field name (the
// lens decodes, it does not import activity — the two stay decoupled).
type ActivityUnaccountedPayload struct {
	Date          string                     `json:"date"`
	Blocks        []ActivityUnaccountedBlock `json:"blocks"`
	AccountedRefs []string                   `json:"accounted_refs"`
}

// ActivityUnaccountedBlock is one contiguous unaccounted activity block.
type ActivityUnaccountedBlock struct {
	Start       string `json:"start"`
	End         string `json:"end"`
	ActiveMin   int    `json:"active_min"`
	DurationMin int    `json:"duration_min"`
	CategoryMix string `json:"category_mix"`
}

// Infer computes the day's unaccounted-time signal. Value is total unaccounted minutes across blocks;
// Confidence scales with the total (2h+ unaccounted saturates to 1.0 — a longer unexplained stretch
// is stronger evidence a real thread went unrecorded). A payload with no unaccounted blocks is
// ErrNotApplicable, so composition skips it rather than emitting a zero signal.
func (l ActivityUnaccountedLens) Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error) {
	if err := ctx.Err(); err != nil {
		return InferredSignal{}, err
	}
	var payload ActivityUnaccountedPayload
	if len(obs.Payload) > 0 {
		if err := json.Unmarshal(obs.Payload, &payload); err != nil {
			return InferredSignal{}, fmt.Errorf("activity-unaccounted lens: decode payload: %w", err)
		}
	}
	if len(payload.Blocks) == 0 {
		return InferredSignal{}, ErrNotApplicable
	}
	subject := strings.TrimSpace(payload.Date)
	if subject == "" {
		subject = strings.TrimSpace(obs.Subject)
	}
	if subject == "" {
		return InferredSignal{}, fmt.Errorf("activity-unaccounted lens: a date (payload.date or observation subject) is required")
	}
	total := 0
	largest := 0
	for _, b := range payload.Blocks {
		total += b.DurationMin
		if b.DurationMin > largest {
			largest = b.DurationMin
		}
	}
	confidence := clampUnit(float64(total) / 120.0)
	detail := fmt.Sprintf("%d min unaccounted activity across %d block(s) on %s (largest %d min); no compound work accounts for it (evidence, not a gate)",
		total, len(payload.Blocks), subject, largest)
	return newSignal(ActivityUnaccountedLensID, subject, float64(total), confidence, VisibilityOperatorPrivate, MethodLedgerReconciliation, detail, obs.EvidenceRefs, now)
}
