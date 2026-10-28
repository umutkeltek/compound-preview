// volatile.go — the volatile claim visibility (transition.md ladder: "useful
// now, expires unless reinforced").
//
// A volatile claim IS promoted — recallable under matching scope like any other
// claim — but it carries a mandatory clock (Claim.ExpiresAt) and never enters
// ambient memory (the orient visibility whitelist excludes it). The sweep
// extends the clock of reinforced volatile claims (reading the memory-temperature
// projection as data) and supersedes unreinforced ones past their clock to a
// cooled (retired-visibility) version: decay, never delete, always undoable.
//
// The write guard here is the slice-1 invariant: a volatile write with no clock
// is a contradiction and is refused fail-loud at EVERY door (Capture, stable
// candidate, knowledge-page import, and the Adjudicate promote path as
// defense-in-depth). Doctrine-class claims (canon operator_permanent tier:
// decision / principle / behavior) may never be volatile at all — doctrine
// never auto-cools (memory-canon Ruling 2).
package mem

import (
	"fmt"
	"strings"
	"time"
)

// VisibilityVolatile is the ladder row this package implements.
const VisibilityVolatile = "volatile"

// isVolatileVisibility reports whether a visibility string denotes the volatile
// tier (trimmed, case-insensitive — matching how the ambient gate reads it).
func isVolatileVisibility(visibility string) bool {
	return strings.EqualFold(strings.TrimSpace(visibility), VisibilityVolatile)
}

// validateVolatileWrite is the shared fail-loud write guard. ref names the
// refused object in the error (candidate id/entity); non-volatile visibilities
// pass through untouched.
func validateVolatileWrite(ref, visibility, expiresAt, claimType, canonClass string) error {
	if !isVolatileVisibility(visibility) {
		return nil
	}
	clock := strings.TrimSpace(expiresAt)
	if clock == "" {
		return fmt.Errorf("%s: volatile requires expires_at — a volatile claim with no clock is a contradiction (useful now, expires unless reinforced)", ref)
	}
	if _, err := time.Parse(time.RFC3339, clock); err != nil {
		return fmt.Errorf("%s: volatile expires_at %q is not RFC3339 — an unreadable clock is no clock: %v", ref, clock, err)
	}
	if class := EffectiveCanonClass(claimType, canonClass); CanonClassOperatorPermanent(class) {
		return fmt.Errorf("%s: volatile is refused for %s-class claims (%s tier) — doctrine never auto-cools; use a non-volatile visibility", ref, class, CanonRiskOperatorPermanent)
	}
	return nil
}
