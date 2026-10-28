// volatile_sweep.go — the clock loop for volatile claims: the ladder promise
// "useful now, expires unless reinforced" enforced by the SAME sweep +
// temperature machinery that already expires parked candidates, lifted onto
// promoted claims.
//
// Reinforcement is read from the memory-temperature projection AS DATA (the
// layering boundary temperature.go documents): reinforced := net consumption
// (Consumed - Ignored) > 0 AND LastInjected within one temperature half-life of
// sweep-now. Consumed alone is cumulative and would make an injected-but-
// always-ignored claim immortal — injection recency is not consumption.
//
// Both moves reuse supersession (MVCC-by-fold): an extension appends a new
// version with a later clock; expiry appends a cooled version (visibility
// retired + cooled_reason) — decay, never delete, always undoable via
// `compound memory volatile-restore <cooled_claim_id>`. Idempotency is
// mechanical: the extension target is a deterministic function of LastInjected
// (strictly-later guard), and a cooled claim is no longer volatile-current.
package mem

import (
	"fmt"
	"strings"
	"time"
)

// CooledReasonVolatileExpired is stamped on the cooled version an expired,
// unreinforced volatile claim supersedes to.
const CooledReasonVolatileExpired = "volatile_expired_unreinforced"

const (
	// volatileReinforcedWindow: consumption older than one temperature
	// half-life (temperature.go) no longer counts as "reinforced now".
	volatileReinforcedWindow = 14 * 24 * time.Hour
	// volatileTTL is the clock granted by an extension or a restore — mirrors
	// the admission decay_park TTL (30d).
	volatileTTL = 30 * 24 * time.Hour
)

// Volatile sweep action verbs.
const (
	VolatileActionExtended = "extended"
	VolatileActionCooled   = "cooled"
	VolatileActionCarded   = "carded"
)

// VolatileSweepAction is one receipt row of the volatile stage.
type VolatileSweepAction struct {
	ClaimID     string `json:"claim_id"`
	NewClaimID  string `json:"new_claim_id,omitempty"` // appended successor version (extended/cooled)
	Action      string `json:"action"`                 // extended | cooled | carded
	Reason      string `json:"reason"`
	CanonClass  string `json:"canon_class,omitempty"` // carded rows: why the valve held
	ExpiresAt   string `json:"expires_at,omitempty"`  // the clock after the action
	UndoCommand string `json:"undo_command,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// volatileSweep runs the clock loop over current volatile claims. The whole
// pass holds the ledger lock so the currency fold and the version appends are
// one consistent critical section (loadClaims/supersededAsOf are lock-free
// reads; appendClaimVersionLocked allocates seq without re-locking).
func (s *Store) volatileSweep(now time.Time) ([]VolatileSweepAction, error) {
	proj, _, err := ReadTemperature(s.home)
	if err != nil {
		return nil, err
	}
	temps := make(map[string]TemperatureEntry, len(proj.Entries))
	for _, e := range proj.Entries {
		temps[e.ClaimID] = e
	}

	var actions []VolatileSweepAction
	lockErr := s.withLock(func() error {
		claims, err := s.loadClaims()
		if err != nil {
			return err
		}
		superseded, err := s.supersededAsOf(s.maxSeq(claims))
		if err != nil {
			return err
		}
		for _, c := range claims { // ledger order — deterministic
			if superseded[c.ID] || !isVolatileVisibility(c.Visibility) {
				continue
			}
			// Contested claims are already in the operator's review lane;
			// cooling one mid-review would race it. They stay volatile and are
			// re-examined once the review resolves.
			if c.State == ClaimStateNeedsReview {
				continue
			}
			class := EffectiveCanonClass(c.ClaimType, c.CanonClass)
			if CanonClassOperatorPermanent(class) {
				actions = append(actions, cardAction(c, class, "operator-permanent volatile claim must not exist (write guard bypassed) — doctrine never auto-cools; operator review required", now))
				continue
			}

			entry, hasEntry := temps[c.ID]
			if hasEntry && volatileReinforced(entry, now) {
				last, _ := time.Parse(time.RFC3339, strings.TrimSpace(entry.LastInjected)) // parse checked in volatileReinforced
				target := last.Add(volatileTTL)
				cur, curErr := time.Parse(time.RFC3339, strings.TrimSpace(c.ExpiresAt))
				if curErr != nil {
					actions = append(actions, cardAction(c, class, fmt.Sprintf("unreadable expires_at %q — an unreadable clock is no clock; operator repair required", c.ExpiresAt), now))
					continue
				}
				if !target.After(cur) {
					continue // strictly-later guard: re-runs are no-ops (falsifier e)
				}
				next, err := s.appendClaimVersionLocked(c, now, func(n *Claim) {
					n.ExpiresAt = rfc3339(target)
				})
				if err != nil {
					return err
				}
				actions = append(actions, VolatileSweepAction{
					ClaimID: c.ID, NewClaimID: next.ID, Action: VolatileActionExtended,
					Reason:    "reinforced within window (net consumption > 0); clock extended to last_injected+30d",
					ExpiresAt: next.ExpiresAt, CreatedAt: rfc3339(now),
				})
				continue
			}

			exp, expErr := time.Parse(time.RFC3339, strings.TrimSpace(c.ExpiresAt))
			if expErr != nil {
				actions = append(actions, cardAction(c, class, fmt.Sprintf("unreadable expires_at %q — an unreadable clock is no clock; operator repair required", c.ExpiresAt), now))
				continue
			}
			if !now.After(exp) {
				continue // clock still running
			}
			if !CanonClassLowRisk(class) {
				// gated tier (product_fact/unclassified): only low_risk is
				// authorized for silent state change — surface, don't cool.
				actions = append(actions, cardAction(c, class, "expired unreinforced, but gated-tier claims cool only after operator surfacing", now))
				continue
			}
			next, err := s.appendClaimVersionLocked(c, now, func(n *Claim) {
				n.Visibility = "retired"
				n.CooledReason = CooledReasonVolatileExpired
				// ExpiresAt kept for audit (mirrors candidate decay).
			})
			if err != nil {
				return err
			}
			actions = append(actions, VolatileSweepAction{
				ClaimID: c.ID, NewClaimID: next.ID, Action: VolatileActionCooled,
				Reason:      CooledReasonVolatileExpired,
				ExpiresAt:   next.ExpiresAt,
				UndoCommand: "compound memory volatile-restore " + next.ID,
				CreatedAt:   rfc3339(now),
			})
		}
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return actions, nil
}

// volatileReinforced is the packet's "reinforced" predicate, amended per the
// design review: net consumption must be positive (Consumed alone is cumulative
// — an injected-but-ignored claim would be immortal) AND the last injection
// must be recent (consumed-long-ago-never-since is not reinforcement).
func volatileReinforced(e TemperatureEntry, now time.Time) bool {
	if e.Consumed-e.Ignored <= 0 {
		return false
	}
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(e.LastInjected))
	if err != nil {
		return false
	}
	return now.Sub(last) <= volatileReinforcedWindow
}

func cardAction(c Claim, class, reason string, now time.Time) VolatileSweepAction {
	return VolatileSweepAction{
		ClaimID: c.ID, Action: VolatileActionCarded, Reason: reason,
		CanonClass: class, ExpiresAt: c.ExpiresAt, CreatedAt: rfc3339(now),
	}
}

// appendClaimVersionLocked appends a successor version of prior with mutate
// applied, plus the supersedes edge. MUST be called under s.withLock (it
// allocates seq and appends without re-locking). Derived decorations
// (State/ReviewRefs/ConfidenceDelta) never persist on raw rows.
func (s *Store) appendClaimVersionLocked(prior Claim, now time.Time, mutate func(*Claim)) (Claim, error) {
	seq, err := s.nextSeq()
	if err != nil {
		return Claim{}, err
	}
	next := prior
	next.Seq = seq
	next.Supersedes = prior.ID
	next.CreatedAt = rfc3339(now)
	next.State = ""
	next.ReviewRefs = nil
	next.ConfidenceDelta = 0
	mutate(&next)
	next.ID = idFor("claim_", next.Entity, next.Predicate, next.Object, fmt.Sprintf("%d", seq))
	if err := appendJSONL(s.claimsLog(), &next); err != nil {
		return Claim{}, err
	}
	edge := Edge{Seq: seq, Kind: EdgeKindSupersedes, From: next.ID, To: prior.ID, Confidence: prior.Confidence, CreatedAt: rfc3339(now)}
	if err := appendJSONL(s.edgesLog(), &edge); err != nil {
		return Claim{}, err
	}
	return next, nil
}

// VolatileRestore is the one-line undo for a volatile expiry: it appends a new
// version of the COOLED claim restoring volatile visibility with a FRESH clock
// (now+30d — restoring the stale expired clock would re-cool on the next sweep).
// Guards: the id must name the current, volatile-cooled version (double-undo
// refuses; hand-retired claims are not restorable through this door).
func (s *Store) VolatileRestore(claimID string, now time.Time) (Claim, error) {
	var restored Claim
	err := s.withLock(func() error {
		claims, err := s.loadClaimsRaw()
		if err != nil {
			return err
		}
		superseded, err := s.supersededAsOf(s.maxSeq(claims))
		if err != nil {
			return err
		}
		var target Claim
		found := false
		for _, c := range claims {
			if c.ID == claimID {
				target, found = c, true
			}
		}
		if !found {
			return fmt.Errorf("claim %s not found", claimID)
		}
		if superseded[claimID] {
			return fmt.Errorf("claim %s is not the current version (already superseded — double undo?)", claimID)
		}
		if !strings.EqualFold(strings.TrimSpace(target.Visibility), "retired") || target.CooledReason != CooledReasonVolatileExpired {
			return fmt.Errorf("claim %s is not a volatile-cooled version (visibility=%q cooled_reason=%q); only %s rows restore through this door", claimID, target.Visibility, target.CooledReason, CooledReasonVolatileExpired)
		}
		next, err := s.appendClaimVersionLocked(target, now, func(n *Claim) {
			n.Visibility = VisibilityVolatile
			n.CooledReason = ""
			n.ExpiresAt = rfc3339(now.Add(volatileTTL))
		})
		if err != nil {
			return err
		}
		restored = next
		return nil
	})
	return restored, err
}
