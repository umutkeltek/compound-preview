// Package actuator is compound's execution router: the thin layer that turns
// KNOWN work into governed executions within declared limits, and closes the
// feedback loop afterwards. It is NOT a new governance path — it composes the
// seams that already own authority:
//
//	internal/policy   Evaluate(ActionIntent) -> PolicyDecision (allow/hold/deny)
//	internal/act      the one governed outbound-write gate (PolicyDecision -> receipt + transition)
//	internal/harness  Adapter.Run() drives a real harness (Claude/Codex) and mints a RunAttempt
//	internal/work     the WorkEnvelope state machine the actuator operates WITHIN
//
// The actuator adds exactly the two pieces that were missing:
//
//  1. A BudgetEnvelope + ledger — per-channel action reservations are durable
//     before dispatch; configured harness spend/token ceilings hold when this
//     consumer cannot prove bounded provider accounting.
//  2. A Route() classifier — reads the mutation_class + reversibility an intent
//     ALREADY declares, plus the policy outcome and budget headroom, and routes
//     reversible + under-budget work to AUTO while holding everything
//     external/public/financial/irreversible for a human. This turns the policy
//     from a wall (everything held) into a gate (safe work flows).
//
// It owns no meaning authority object: envelope/policy/work/act remain
// authority. The only state it persists is the budget envelope and a
// consumption ledger under COMPOUND_HOME/actuator/budget — a thin persistence
// primitive, the same posture as internal/runattempt and internal/landlease.
package actuator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/home"
)

const (
	// BudgetSchemaVersion identifies a BudgetEnvelope on disk.
	BudgetSchemaVersion = "compound.actuator_budget.v0.1"
	// ConsumptionSchemaVersion identifies a consumption ledger row.
	ConsumptionSchemaVersion = "compound.actuator_consumption.v0.2"
	// WildcardChannel matches every channel when no channel-specific limit exists.
	WildcardChannel = "*"
	// DefaultBudgetID is the id of the built-in conservative budget used when a
	// home has no budget envelope installed.
	DefaultBudgetID = "budget_default"
)

// ChannelLimits is the enforced ceiling for one channel of one product over a
// rolling window. A zero Max* field means "unlimited on that axis" — but note
// the Route classifier independently holds every external/public/financial/
// irreversible class regardless of budget, so an open budget never opens an
// unsafe class.
type ChannelLimits struct {
	Channel     string  `json:"channel"`
	WindowHours int     `json:"window_hours"`
	MaxActions  int     `json:"max_actions"`
	MaxSpendUSD float64 `json:"max_spend_usd"`
	MaxTokens   int     `json:"max_tokens"`
}

// BudgetEnvelope declares the hard per-channel execution limits for a product.
// ProductID "" is the fallback budget applied when no product-specific budget
// is installed. It is envelope-shaped (schema_version + id + created_at) and
// validated on load, following the product.LoopModel pattern: its own schema
// version and Validate(), not a registry entry in internal/envelope.
type BudgetEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	ProductID     string          `json:"product_id,omitempty"`
	Channels      []ChannelLimits `json:"channels"`
	CreatedAt     string          `json:"created_at"`
}

// Validate enforces the envelope's invariants. A zero WindowHours is illegal —
// a limit with no window is meaningless and would divide the rolling sum by an
// empty range.
func (b BudgetEnvelope) Validate() error {
	if b.SchemaVersion != BudgetSchemaVersion {
		return fmt.Errorf("actuator budget schema_version = %q, want %q", b.SchemaVersion, BudgetSchemaVersion)
	}
	if strings.TrimSpace(b.ID) == "" {
		return fmt.Errorf("actuator budget id required")
	}
	if len(b.Channels) == 0 {
		return fmt.Errorf("actuator budget %s: at least one channel limit required", b.ID)
	}
	seen := map[string]bool{}
	for _, ch := range b.Channels {
		if strings.TrimSpace(ch.Channel) == "" {
			return fmt.Errorf("actuator budget %s: channel name required", b.ID)
		}
		if seen[ch.Channel] {
			return fmt.Errorf("actuator budget %s: duplicate channel %q", b.ID, ch.Channel)
		}
		seen[ch.Channel] = true
		if ch.WindowHours <= 0 {
			return fmt.Errorf("actuator budget %s: channel %q window_hours must be > 0", b.ID, ch.Channel)
		}
		if ch.MaxActions < 0 || ch.MaxSpendUSD < 0 || ch.MaxTokens < 0 {
			return fmt.Errorf("actuator budget %s: channel %q limits must be >= 0 (0 = unlimited)", b.ID, ch.Channel)
		}
	}
	if _, err := time.Parse(time.RFC3339, b.CreatedAt); err != nil {
		return fmt.Errorf("actuator budget %s: created_at invalid: %w", b.ID, err)
	}
	return nil
}

// LimitsFor resolves the effective limit for a channel: an exact channel match
// wins, else the wildcard channel, else not-ok.
func (b BudgetEnvelope) LimitsFor(channel string) (ChannelLimits, bool) {
	channel = strings.TrimSpace(channel)
	var wildcard ChannelLimits
	hasWildcard := false
	for _, ch := range b.Channels {
		if ch.Channel == channel {
			return ch, true
		}
		if ch.Channel == WildcardChannel {
			wildcard = ch
			hasWildcard = true
		}
	}
	return wildcard, hasWildcard
}

// DefaultBudget is the built-in conservative budget used when a home has none
// installed. It rate-limits the wildcard channel to a small daily action count
// and forbids spend/tokens by leaving them at 0 (unlimited) — the safety for
// external/spend classes comes from Route holding them regardless, not from the
// default budget. The default exists so the very first auto-execution is already
// rate-bounded rather than unbounded.
func DefaultBudget(now time.Time) BudgetEnvelope {
	return BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion,
		ID:            DefaultBudgetID,
		Channels: []ChannelLimits{
			{Channel: WildcardChannel, WindowHours: 24, MaxActions: 20, MaxSpendUSD: 0, MaxTokens: 0},
		},
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
}

// Consumption is one ledger row recording that an execution consumed budget.
// Rows are append-only under COMPOUND_HOME/actuator/budget/consumption.jsonl;
// the rolling usage sum is rebuilt from rows inside each channel's window.
type Consumption struct {
	SchemaVersion  string  `json:"schema_version"`
	At             string  `json:"at"`
	ProductID      string  `json:"product_id,omitempty"`
	Channel        string  `json:"channel"`
	Actions        int     `json:"actions"`
	SpendUSD       float64 `json:"spend_usd,omitempty"`
	Tokens         int     `json:"tokens,omitempty"`
	WorkID         string  `json:"work_id,omitempty"`
	ReceiptRef     string  `json:"receipt_ref,omitempty"`
	ReservationID  string  `json:"reservation_id,omitempty"`
	Reserved       bool    `json:"reserved,omitempty"`
	ActionIntentID string  `json:"action_intent_id,omitempty"`
}

// Usage is the rolled-up consumption for one (product, channel) inside a window.
type Usage struct {
	Actions               int      `json:"actions"`
	SpendUSD              float64  `json:"spend_usd"`
	Tokens                int      `json:"tokens"`
	PendingReservationIDs []string `json:"pending_reservation_ids,omitempty"`
}

// Headroom is the result of checking a budget against current usage. Allowed is
// the hard gate; the Remaining fields and Reasons explain the decision so the
// operator (and Route) can surface exactly which axis is tight.
type Headroom struct {
	Allowed                    bool     `json:"allowed"`
	Channel                    string   `json:"channel"`
	WindowHours                int      `json:"window_hours"`
	ActionsRemaining           int      `json:"actions_remaining"`
	SpendRemainingUSD          float64  `json:"spend_remaining_usd"`
	TokensRemaining            int      `json:"tokens_remaining"`
	UnlimitedActions           bool     `json:"unlimited_actions"`
	UnlimitedSpend             bool     `json:"unlimited_spend"`
	UnlimitedTokens            bool     `json:"unlimited_tokens"`
	Reasons                    []string `json:"reasons,omitempty"`
	PendingReservationIDs      []string `json:"pending_reservation_ids,omitempty"`
	RequiresProviderAccounting bool     `json:"requires_provider_accounting,omitempty"`
}

// Ledger persists consumption rows under COMPOUND_HOME/actuator/budget. It is a
// thin append-only primitive: it never decides policy and never mutates work —
// it only records usage and answers "how much is left".
type Ledger struct{ home string }

// OpenLedger returns a Ledger rooted at home. It does not create directories;
// Record lazily mkdirs on first write, matching runattempt/landlease.
func OpenLedger(home string) *Ledger { return &Ledger{home: home} }

func (l *Ledger) dir() string { return filepath.Join(l.home, "actuator", "budget") }

func (l *Ledger) consumptionPath() string { return filepath.Join(l.dir(), "consumption.jsonl") }

// BudgetPath is where the budget envelope for a product lives. ProductID "" maps
// to the default budget file.
func (l *Ledger) BudgetPath(productID string) string {
	name := normalizeBudgetName(productID)
	return filepath.Join(l.dir(), name+".budget.json")
}

// SaveBudget persists a validated budget envelope atomically (write temp +
// rename) so a partial write never leaves a corrupt budget on disk.
func (l *Ledger) SaveBudget(b BudgetEnvelope) error {
	if err := b.Validate(); err != nil {
		return err
	}
	return home.WithHomeMutationGuard(l.home, func(*home.HomeMutationGuard) error {
		return l.saveBudget(b)
	})
}

func (l *Ledger) saveBudget(b BudgetEnvelope) error {
	if err := os.MkdirAll(l.dir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := atomicWrite(l.BudgetPath(b.ProductID), data); err != nil {
		return err
	}
	return l.syncBudgetFile(l.BudgetPath(b.ProductID))
}

// LoadBudget returns the budget envelope for a product, or DefaultBudget when
// none is installed. A corrupt or invalid installed budget is an error (fail
// closed) rather than a silent fallback — a broken budget must not silently
// become permissive.
func (l *Ledger) LoadBudget(productID string, now time.Time) (BudgetEnvelope, error) {
	path := l.BudgetPath(productID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultBudget(now), nil
		}
		return BudgetEnvelope{}, err
	}
	var b BudgetEnvelope
	if err := json.Unmarshal(data, &b); err != nil {
		return BudgetEnvelope{}, fmt.Errorf("actuator budget %s: %w", path, err)
	}
	if err := b.Validate(); err != nil {
		return BudgetEnvelope{}, err
	}
	return b, nil
}

// Record appends one consumption row. It validates the row's timestamp and
// defaults SchemaVersion. Append-only: existing rows are never rewritten.
func (l *Ledger) Record(c Consumption) error {
	return home.WithHomeMutationGuard(l.home, func(*home.HomeMutationGuard) error {
		return l.record(c)
	})
}

// record requires the home mutation guard and syncs before dispatch.
func (l *Ledger) record(c Consumption) error {
	if c.SchemaVersion == "" {
		c.SchemaVersion = ConsumptionSchemaVersion
	}
	if err := validateConsumption(c); err != nil {
		return err
	}
	_, reservations, err := l.consumptions()
	if err != nil {
		return err
	}
	if c.ReservationID != "" {
		prior, exists := reservations[c.ReservationID]
		if exists && sameReservation(prior, c) && prior.Reserved == c.Reserved && prior.ReceiptRef == c.ReceiptRef {
			// A sync/close failure after append is uncertain, not permission to
			// append a second charge or manufacture a fresh reservation.
			return l.syncConsumption()
		}
		if (c.Reserved && exists) || (!c.Reserved && (!exists || !prior.Reserved || !sameReservation(prior, c))) {
			return fmt.Errorf("invalid budget reservation/settlement %s", c.ReservationID)
		}
	}
	if err := os.MkdirAll(l.dir(), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.consumptionPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return l.syncBudgetDirectories()
}

// Used rolls up consumption for (productID, channel) at or after `since`. Rows
// with a different product are ignored; rows on the wildcard channel count
// toward every channel (a global cap is global).
func (l *Ledger) Used(productID, channel string, since time.Time) (Usage, error) {
	var usage Usage
	legacy, reservations, err := l.consumptions()
	if err != nil {
		return Usage{}, err
	}
	for _, c := range legacy {
		addUsage(&usage, c, productID, channel, since)
	}
	for _, c := range reservations {
		addUsage(&usage, c, productID, channel, since)
	}
	sort.Strings(usage.PendingReservationIDs)
	return usage, nil
}

func (l *Ledger) consumptions() ([]Consumption, map[string]Consumption, error) {
	var legacy []Consumption
	reservations := map[string]Consumption{}
	data, err := os.ReadFile(l.consumptionPath())
	if err != nil {
		if os.IsNotExist(err) {
			return legacy, reservations, nil
		}
		return nil, nil, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c Consumption
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			// A malformed row fails CLOSED: skipping it would under-count usage and
			// silently loosen the budget. Naming the line lets the operator repair
			// the ledger; the caller (Check) turns this into a hold, not zero usage.
			return nil, nil, fmt.Errorf("actuator consumption ledger %s: corrupt row at line %d: %w", l.consumptionPath(), i+1, err)
		}
		if err := validateConsumption(c); err != nil {
			return nil, nil, fmt.Errorf("actuator consumption ledger line %d: %w", i+1, err)
		}
		if c.ReservationID != "" {
			prior, exists := reservations[c.ReservationID]
			if c.Reserved {
				if exists {
					return nil, nil, fmt.Errorf("duplicate budget reservation %s", c.ReservationID)
				}
			} else if !exists || !prior.Reserved || !sameReservation(prior, c) {
				return nil, nil, fmt.Errorf("invalid budget settlement %s", c.ReservationID)
			}
			reservations[c.ReservationID] = c
			continue
		}
		legacy = append(legacy, c)
	}
	return legacy, reservations, nil
}

// Check compares the effective channel limit against current rolling usage and
// returns the headroom verdict. A channel with no matching limit (no exact, no
// wildcard) is fail-closed: not allowed, because an undeclared channel has no
// declared budget to execute within.
func (l *Ledger) Check(budget BudgetEnvelope, productID, channel string, now time.Time) (Headroom, error) {
	channel = strings.TrimSpace(channel)
	limits, ok := budget.LimitsFor(channel)
	if !ok {
		return Headroom{
			Allowed: false,
			Channel: channel,
			Reasons: []string{fmt.Sprintf("no budget limit declared for channel %q (and no %q fallback)", channel, WildcardChannel)},
		}, nil
	}
	window := time.Duration(limits.WindowHours) * time.Hour
	usage, err := l.Used(productID, channel, now.Add(-window))
	if err != nil {
		// Fail closed: an unreadable/corrupt ledger means we cannot prove the run is
		// under budget, so we HOLD (blocking) rather than treat missing data as zero
		// usage. The error is surfaced as the hold reason, not swallowed.
		return Headroom{
			Allowed:          false,
			Channel:          channel,
			WindowHours:      limits.WindowHours,
			UnlimitedActions: limits.MaxActions == 0,
			UnlimitedSpend:   limits.MaxSpendUSD == 0,
			UnlimitedTokens:  limits.MaxTokens == 0,
			Reasons:          []string{fmt.Sprintf("budget ledger unreadable — holding fail-closed: %v", err)},
		}, nil
	}
	h := Headroom{
		Allowed:               true,
		Channel:               channel,
		WindowHours:           limits.WindowHours,
		UnlimitedActions:      limits.MaxActions == 0,
		UnlimitedSpend:        limits.MaxSpendUSD == 0,
		UnlimitedTokens:       limits.MaxTokens == 0,
		PendingReservationIDs: usage.PendingReservationIDs,
	}
	if len(usage.PendingReservationIDs) > 0 {
		h.Reasons = append(h.Reasons, fmt.Sprintf("pending budget reservations require execution reconciliation: %v", usage.PendingReservationIDs))
	}
	if limits.MaxActions > 0 {
		h.ActionsRemaining = limits.MaxActions - usage.Actions
		if h.ActionsRemaining <= 0 {
			h.Allowed = false
			h.Reasons = append(h.Reasons, fmt.Sprintf("action rate limit exhausted: %d/%d in last %dh", usage.Actions, limits.MaxActions, limits.WindowHours))
		}
	}
	if limits.MaxSpendUSD > 0 {
		h.SpendRemainingUSD = limits.MaxSpendUSD - usage.SpendUSD
		if h.SpendRemainingUSD <= 0 {
			h.Allowed = false
			h.Reasons = append(h.Reasons, fmt.Sprintf("spend limit exhausted: %.2f/%.2f USD in last %dh", usage.SpendUSD, limits.MaxSpendUSD, limits.WindowHours))
		}
	}
	if limits.MaxTokens > 0 {
		h.TokensRemaining = limits.MaxTokens - usage.Tokens
		if h.TokensRemaining <= 0 {
			h.Allowed = false
			h.Reasons = append(h.Reasons, fmt.Sprintf("token limit exhausted: %d/%d in last %dh", usage.Tokens, limits.MaxTokens, limits.WindowHours))
		}
	}
	return h, nil
}

func normalizeBudgetName(productID string) string {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(productID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "default"
	}
	return name
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
