package actuator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HeldMoveCardSchemaVersion identifies a persisted held-move action card.
const HeldMoveCardSchemaVersion = "compound.actuator_held_move.v0.1"

// HeldMoveCard is the actuator's answer to "this move was NOT auto-executed". A
// hold or deny is never just a refusal: it becomes a concrete, human-gated action
// card that says exactly what a person must do to release the move (the release
// gate), what inputs are required, and what must be true to stop. The actuator
// owns these cards under actuator/held-moves/ — it stages and explains, but the
// consequential release (spend, public publish, destructive, security) always
// stays a human act.
type HeldMoveCard struct {
	SchemaVersion  string   `json:"schema_version"`
	ID             string   `json:"id"`
	ProductID      string   `json:"product_id,omitempty"`
	Channel        string   `json:"channel"`
	WorkID         string   `json:"work_id,omitempty"`
	Capability     string   `json:"capability,omitempty"`
	MutationClass  string   `json:"mutation_class"`
	Reversibility  string   `json:"reversibility"`
	PolicyOutcome  string   `json:"policy_outcome"`
	Decision       string   `json:"decision"` // "hold" | "deny"
	Reasons        []string `json:"reasons,omitempty"`
	ReleaseGate    string   `json:"release_gate"`
	RequiredInputs []string `json:"required_inputs,omitempty"`
	StopConditions []string `json:"stop_conditions,omitempty"`
	CreatedAt      string   `json:"created_at"`
}

// ReleaseGateFor builds the human-gated action card for a non-auto Plan. It is pure
// (no write), so a dry-run `plan` can show the card inline without persisting it;
// the real execution paths (Post/Run) persist the card via WriteHeldMoveCard.
func ReleaseGateFor(plan Plan, now time.Time) HeldMoveCard {
	card := HeldMoveCard{
		SchemaVersion: HeldMoveCardSchemaVersion,
		ProductID:     plan.ProductID,
		Channel:       plan.Channel,
		WorkID:        plan.Intent.IDs.WorkID,
		Capability:    plan.Intent.Capability,
		MutationClass: plan.Intent.MutationClass,
		Reversibility: plan.Intent.Reversibility,
		PolicyOutcome: plan.PolicyDecision.Outcome,
		Decision:      strings.ToLower(string(plan.Decision.Mode)),
		Reasons:       append([]string{}, plan.Decision.Reasons...),
		CreatedAt:     now.UTC().Format(time.RFC3339),
	}
	card.ID = heldMoveID(plan)
	card.ReleaseGate, card.RequiredInputs, card.StopConditions = releaseGate(plan)
	return card
}

// releaseGate maps a held/denied move to the exact human action that releases it.
// Class wins (it names the kind of consequence), then the budget for auto-eligible
// classes that were held only for headroom, then a generic human-review fallback.
func releaseGate(plan Plan) (gate string, required, stop []string) {
	if plan.Decision.Mode == ModeDeny {
		return "DENIED by policy — do not proceed. Re-declare the intent (class/reversibility) or change policy before retrying.",
			nil,
			[]string{"policy refuses this move as declared"}
	}
	switch plan.Intent.MutationClass {
	case "financial":
		return "Spend is human-gated. The actuator prepares and budgets but NEVER spends; a human approves and executes the payment. Run a dry-run first.",
			[]string{"human approval of the spend", "confirmed amount within the approved budget", "dry-run receipt"},
			[]string{"no human approval", "amount exceeds the approved budget", "dry-run not run"}
	case "external_side_effect", "public":
		return "External/public mutation is human-gated. The actuator stages locally (outbox) only; a human releases it to the real channel. Tier-3 policy + dry-run first (outbound-distribution doctrine).",
			[]string{"human release of the staged artifact", "Tier-3 policy review", "dry-run receipt"},
			[]string{"content not reviewed by a human", "dry-run not run", "channel auth not confirmed by a human"}
	case "destructive":
		return "Destructive mutation is human-gated and requires a verified backup/rollback path first.",
			[]string{"human approval", "verified backup"},
			[]string{"no backup", "no approval"}
	case "security":
		return "Security mutation is human-gated and requires security review.",
			[]string{"human approval", "security review"},
			[]string{"security review not done"}
	}
	// Auto-eligible class (read/local_reversible) that still held: the gate is budget
	// headroom or reversibility — both operator-resolvable.
	if !plan.Headroom.Allowed {
		if plan.Headroom.RequiresProviderAccounting {
			return "Harness spend/token accounting is not supported by this consumer. Keep the move held until a supported provider accounting owner can bound and settle its cost; waiting or raising an action limit cannot repair this.",
				[]string{"supported bounded provider accounting within the approved spend/token ceiling"},
				[]string{"provider cost remains unbounded or unsettled"}
		}
		if len(plan.Headroom.PendingReservationIDs) > 0 {
			return fmt.Sprintf("Budget reservations %v still require execution reconciliation. Inspect their work/action and terminal attempt or receipt evidence. Do not delete ledger rows, raise limits, or wait for an unresolved reservation to expire.", plan.Headroom.PendingReservationIDs),
				[]string{"exact terminal execution evidence and matching reservation settlement"},
				[]string{"execution or external effects remain unresolved"}
		}
		return fmt.Sprintf("Budget exhausted on channel %q. Wait for the rolling window to reset, or raise the limit with: compound actuator budget-set --channel %s --window-hours H --max-actions N", plan.Channel, plan.Channel),
			[]string{fmt.Sprintf("budget headroom on channel %s", plan.Channel)},
			[]string{"still over the channel budget"}
	}
	return "Human review required before this move can execute (held for reversibility or policy obligation).",
		[]string{"human review of the move"},
		[]string{"move not reviewed"}
}

func heldMoveID(plan Plan) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{plan.Intent.ID, string(plan.Decision.Mode), plan.Channel}, "\x00")))
	return "heldmove_" + hex.EncodeToString(sum[:])[:16]
}

// WriteHeldMoveCard persists a held-move card to actuator/held-moves/<id>.json and
// returns its path. Persisting is idempotent by id: the same held move overwrites
// its own card rather than accumulating duplicates.
func WriteHeldMoveCard(homePath string, card HeldMoveCard) (string, error) {
	dir := filepath.Join(homePath, "actuator", "held-moves")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, card.ID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ListHeldMoves returns the persisted held-move cards for a home, optionally
// filtered to a product. This is the operator's queue of "moves waiting on a human".
func ListHeldMoves(homePath, productID string) ([]HeldMoveCard, error) {
	dir := filepath.Join(homePath, "actuator", "held-moves")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []HeldMoveCard
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c HeldMoveCard
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		if productID != "" && c.ProductID != productID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
