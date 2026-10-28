package actuator

import (
	"testing"

	"compound/internal/envelope"
)

func testIntent(class, reversibility string) envelope.ActionIntent {
	return envelope.ActionIntent{
		MutationClass: class,
		Reversibility: reversibility,
		BlastRadius:   "local",
	}
}

func openHeadroom() Headroom { return Headroom{Allowed: true, Channel: "*"} }
func closedHeadroom(reason string) Headroom {
	return Headroom{Allowed: false, Channel: "*", Reasons: []string{reason}}
}

func TestRoutePolicyDenyWins(t *testing.T) {
	d := Route(testIntent("local_reversible", "easy"), "deny", openHeadroom())
	if d.Mode != ModeDeny {
		t.Fatalf("mode = %s, want deny", d.Mode)
	}
}

func TestRoutePolicyHoldWins(t *testing.T) {
	d := Route(testIntent("local_reversible", "easy"), "hold", openHeadroom())
	if d.Mode != ModeHold {
		t.Fatalf("mode = %s, want hold", d.Mode)
	}
}

func TestRouteAutoForLocalReversibleEasy(t *testing.T) {
	d := Route(testIntent("local_reversible", "easy"), "allow", openHeadroom())
	if d.Mode != ModeAuto {
		t.Fatalf("mode = %s, want auto (reasons: %v)", d.Mode, d.Reasons)
	}
}

func TestRouteAutoForRead(t *testing.T) {
	d := Route(testIntent("read", "none"), "allow", openHeadroom())
	if d.Mode != ModeAuto {
		t.Fatalf("mode = %s, want auto (reasons: %v)", d.Mode, d.Reasons)
	}
}

func TestRouteAllowWithObligationsStillFlows(t *testing.T) {
	// DefaultConfig maps local_reversible to allow_with_obligations; the
	// actuator treats an allow-with-obligations move as flowable once its own
	// safety gates pass (act.Execute satisfies the obligations by construction).
	d := Route(testIntent("local_reversible", "easy"), "allow_with_obligations", openHeadroom())
	if d.Mode != ModeAuto {
		t.Fatalf("mode = %s, want auto (reasons: %v)", d.Mode, d.Reasons)
	}
}

func TestRouteHoldsExternalClass(t *testing.T) {
	for _, class := range []string{"external_side_effect", "public", "destructive", "financial", "security"} {
		d := Route(testIntent(class, "easy"), "allow", openHeadroom())
		if d.Mode != ModeHold {
			t.Errorf("class %s: mode = %s, want hold", class, d.Mode)
		}
	}
}

func TestRouteHoldsNonEasyReversibility(t *testing.T) {
	for _, rev := range []string{"manual", "hard", "irreversible", "none"} {
		d := Route(testIntent("local_reversible", rev), "allow", openHeadroom())
		if d.Mode != ModeHold {
			t.Errorf("reversibility %s: mode = %s, want hold", rev, d.Mode)
		}
	}
}

func TestRouteHoldsWhenBudgetExhausted(t *testing.T) {
	d := Route(testIntent("local_reversible", "easy"), "allow", closedHeadroom("action rate limit exhausted"))
	if d.Mode != ModeHold {
		t.Fatalf("mode = %s, want hold", d.Mode)
	}
	found := false
	for _, r := range d.Reasons {
		if r != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a budget reason on the hold")
	}
}

func TestRouteUnknownPolicyOutcomeFailsClosed(t *testing.T) {
	d := Route(testIntent("local_reversible", "easy"), "bogus", openHeadroom())
	if d.Mode != ModeHold {
		t.Fatalf("mode = %s, want hold (fail-closed)", d.Mode)
	}
}
