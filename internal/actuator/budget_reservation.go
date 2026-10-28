package actuator

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

// reservePlan rechecks current budget and usage, then durably reserves one
// action in the existing consumption ledger under a short home mutation lock.
// The lock is never held over harness execution or a governed act callback.
func reservePlan(homePath string, plan Plan, now time.Time) (Plan, Consumption, error) {
	var reservation Consumption
	ledger := OpenLedger(homePath)
	err := home.WithHomeMutationGuard(homePath, func(*home.HomeMutationGuard) error {
		budget, err := ledger.LoadBudget(plan.ProductID, now)
		if err != nil {
			return err
		}
		headroom, err := ledger.Check(budget, plan.ProductID, plan.Channel, now)
		if err != nil {
			return err
		}
		headroom = enforceRunBudgetAccounting(plan.Intent, headroom)
		plan.Headroom = headroom
		plan.Decision = Route(plan.Intent, plan.PolicyDecision.Outcome, headroom)
		plan.Decision.Channel = plan.Channel
		if plan.Decision.Mode != ModeAuto {
			return nil
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		reservation = Consumption{
			SchemaVersion: ConsumptionSchemaVersion, At: now.UTC().Format(time.RFC3339),
			ProductID: plan.ProductID, Channel: plan.Channel, Actions: 1,
			WorkID: plan.Intent.IDs.WorkID, ReservationID: "bres_" + hex.EncodeToString(nonce[:]), Reserved: true,
			ActionIntentID: plan.Intent.ID,
		}
		return ledger.record(reservation)
	})
	if err != nil {
		return plan, reservation, fmt.Errorf("reserve budget consumption before dispatch (reservation=%s): %w", reservation.ReservationID, err)
	}
	return plan, reservation, nil
}

func enforceRunBudgetAccounting(intent envelope.ActionIntent, headroom Headroom) Headroom {
	// These harness calls have neither a trustworthy provider bound nor actual
	// billing settlement. A configured finite cost ceiling must therefore hold.
	if intent.Capability == "harness_run" && headroom.WindowHours > 0 && (!headroom.UnlimitedSpend || !headroom.UnlimitedTokens) {
		headroom.Allowed = false
		headroom.RequiresProviderAccounting = true
		headroom.Reasons = append(headroom.Reasons, "harness run has no bounded provider spend/token accounting; finite spend/token budget requires a supported accounting owner")
	}
	return headroom
}

func settledConsumption(reservation Consumption, receiptRef string) Consumption {
	reservation.Reserved = false
	reservation.ReceiptRef = receiptRef
	return reservation
}

func sameReservation(a, b Consumption) bool {
	return a.ReservationID == b.ReservationID && a.At == b.At && a.ProductID == b.ProductID &&
		a.Channel == b.Channel && a.Actions == b.Actions && a.SpendUSD == b.SpendUSD &&
		a.Tokens == b.Tokens && a.WorkID == b.WorkID && a.ActionIntentID == b.ActionIntentID
}

func validateConsumption(c Consumption) error {
	if c.SchemaVersion != "" && c.SchemaVersion != "compound.actuator_consumption.v0.1" && c.SchemaVersion != ConsumptionSchemaVersion {
		return fmt.Errorf("unsupported consumption schema %q", c.SchemaVersion)
	}
	if c.Channel == "" {
		return fmt.Errorf("consumption channel required")
	}
	if _, err := time.Parse(time.RFC3339, c.At); err != nil {
		return fmt.Errorf("consumption timestamp invalid: %w", err)
	}
	if c.Actions < 0 || c.Tokens < 0 || c.SpendUSD < 0 || math.IsNaN(c.SpendUSD) || math.IsInf(c.SpendUSD, 0) {
		return fmt.Errorf("consumption amounts must be finite and nonnegative")
	}
	if c.Reserved && (c.ReservationID == "" || c.WorkID == "" || c.ActionIntentID == "" || c.ReceiptRef != "" || c.Actions != 1) {
		return fmt.Errorf("invalid budget reservation")
	}
	if c.ReservationID != "" && c.SchemaVersion != ConsumptionSchemaVersion {
		return fmt.Errorf("budget reservations require consumption schema v0.2")
	}
	if c.ReservationID != "" && !c.Reserved && c.ReceiptRef == "" {
		return fmt.Errorf("budget settlement requires execution evidence")
	}
	return nil
}

func (l *Ledger) syncConsumption() error {
	return l.syncBudgetFile(l.consumptionPath())
}

func (l *Ledger) syncBudgetFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return err
	}
	return l.syncBudgetDirectories()
}

var syncBudgetDirectory = func(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (l *Ledger) syncBudgetDirectories() error {
	// MkdirAll may have created both descendants. Syncing only budget does not
	// durably publish their parent entries on the first default-budget run.
	for _, path := range []string{l.dir(), filepath.Dir(l.dir()), l.home} {
		if err := syncBudgetDirectory(path); err != nil {
			return fmt.Errorf("sync budget directory %s: %w", path, err)
		}
	}
	return nil
}

func addUsage(usage *Usage, c Consumption, productID, channel string, since time.Time) {
	at, _ := time.Parse(time.RFC3339, c.At) // validated before folding
	// Unknown/in-flight execution cannot recover allowance merely by aging.
	if (!c.Reserved && at.Before(since)) || c.ProductID != productID || (c.Channel != channel && c.Channel != WildcardChannel) {
		return
	}
	usage.Actions += c.Actions
	usage.SpendUSD += c.SpendUSD
	usage.Tokens += c.Tokens
	if c.Reserved {
		usage.PendingReservationIDs = append(usage.PendingReservationIDs, c.ReservationID)
	}
}
