package hook

// fail_open carries two meanings, and only one of them is alarming.
//
// Three of the four writers mean CAPABILITY ABSENT — the harness has no native hook, so
// enforcement or delivery is delegated to an adapter. That is a static, expected property
// of the harness and is exactly what the status was designed to say (see the comment on
// `status`: "the honest signal that the harness can't natively honor it"):
//
//	hook.go:314   policy hold, harness cannot block natively
//	hook.go:782   policy deny, harness cannot block natively
//	hook.go:1653  status(): harness does not support this hook
//
// The fourth means CONTEXT BUILD FAILED — construction errored or blew its budget, so NO
// context reached the model. That is a runtime failure, and it is the alarming one:
//
//	hook.go:619   contextBreakerResult
//
// In memory the two are separable, because the breaker sets Result.BreakerTripped. But
// BreakerTripped lives on the RESULT and never reaches the persisted HookEvent, so every
// consumer reading events back — the event store, projections, an operator audit — sees
// one indistinguishable `fail_open`. A session where the model silently received nothing
// looks exactly like a harness that simply lacks a hook.
//
// hookStatusRank (hook.go:977) is the proof that consumers branch on this value alone.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// TestFailOpenDistinguishesContextFailureFromAbsentCapability is the headline falsifier:
// the persisted event must say WHICH fail_open this is, without the reader needing the
// in-memory Result.
//
// Falsifier: drop the reason stamp from contextBreakerResult and this goes RED, because a
// context-build failure again presents as a bare capability-absent fail_open.
func TestFailOpenDistinguishesContextFailureFromAbsentCapability(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	deps := baseDeps()

	fxB := Fixture{Harness: "codex", SessionID: "sess_failopen", Cwd: t.TempDir()}
	breaker := contextBreakerResult("UserPromptSubmit", fxB, deps, "", now)
	if breaker.Event.Status != "fail_open" {
		t.Fatalf("precondition: breaker must still fail open, got %q", breaker.Event.Status)
	}
	if !breaker.BreakerTripped {
		t.Fatal("precondition: breaker must trip")
	}
	if breaker.Event.FailOpenReason != envelope.FailOpenContextUnavailable {
		t.Errorf("context-build failure must be readable from the EVENT: fail_open_reason = %q, want %q\n"+
			"  BreakerTripped lives on the Result and never reaches the persisted event, so an\n"+
			"  operator reading events back cannot tell 'the model got no context' from\n"+
			"  'this harness has no native hook'.",
			breaker.Event.FailOpenReason, envelope.FailOpenContextUnavailable)
	}

	fxA := Fixture{Harness: "opencode", SessionID: "sess_absent", Cwd: t.TempDir()}
	st := status(deps, fxA.Harness, "SessionEnd")
	if st != "fail_open" {
		t.Fatalf("precondition: unsupported hook must fail open, got %q", st)
	}
	absent := newCapabilityEvent("SessionEnd", fxA, fxA.Harness, deps, "h", nil, now)
	if absent.FailOpenReason != envelope.FailOpenCapabilityAbsent {
		t.Errorf("an absent native capability must state its exact reason: got %q, want %q",
			absent.FailOpenReason, envelope.FailOpenCapabilityAbsent)
	}

	if breaker.Event.FailOpenReason == absent.FailOpenReason {
		t.Errorf("both fail_open meanings carry reason %q; a consumer branching on status+reason still cannot tell them apart",
			breaker.Event.FailOpenReason)
	}
}

// TestFailOpenReasonIsHashNeutralWhenAbsent pins the additive contract: an event that
// names no reason must marshal exactly as it did before the field existed, so stored
// events and fixtures keep validating and re-marshal byte-identically.
func TestFailOpenReasonIsHashNeutralWhenAbsent(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ev := newEvent("SessionEnd", Fixture{Harness: "codex", Cwd: t.TempDir()}, "ok", "h", nil, now)
	if ev.FailOpenReason != "" {
		t.Fatalf("an ok event must name no fail-open reason, got %q", ev.FailOpenReason)
	}
	blob, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "fail_open_reason") {
		t.Errorf("unset reason must be omitted from the wire, got: %s", blob)
	}
	if err := ev.Validate(); err != nil {
		t.Errorf("an event with no reason must still validate: %v", err)
	}
}
