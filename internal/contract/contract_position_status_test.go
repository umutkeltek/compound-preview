package contract

import (
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/mem"
)

// positionStatusSnapshot builds a runtime snapshot carrying exactly one folded position, so a
// test can vary contested-ness and nothing else.
func positionStatusSnapshot(contested bool) (runtimeSnapshot, string) {
	ref := "position://project:widget::retention::slot-abc123"
	position := envelope.ActivePosition{
		Ref:         ref,
		Entity:      "project:widget",
		Topic:       "retention",
		Stance:      "the nightly reclaim is armed",
		SinceSeq:    41,
		ChainLength: 2,
		Contested:   contested,
		Line:        "slot-abc123 [predicate=posture] -> the nightly reclaim is armed",
	}
	return runtimeSnapshot{
		positionRefs: []contextPositionRef{{
			Ref:        ref,
			Reason:     "folded position for the task entity",
			Line:       position.Line,
			Position:   position,
			Confidence: 0.9,
			Seq:        41,
		}},
	}, ref
}

func includedRefByName(pack envelope.ContextPack, ref string) (envelope.IncludedRef, bool) {
	for _, inc := range pack.Included {
		if inc.Ref == ref {
			return inc, true
		}
	}
	return envelope.IncludedRef{}, false
}

func positionStatusInput() Input {
	return Input{
		Harness: "codex", Cwd: "/repo/widget", Task: "check the retention stance",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}
}

// A contested position must be SERVED as disputed, not current.
//
// The pack already knew: ActivePosition.Contested is set from the fold, and the summary line
// renders "[contested]". But every IncludedRef was minted "current" regardless, so the
// machine-readable half of the same pack asserted the opposite of the prose half. The one
// consumer that acts on it -- internal/hook includedRefTrustTier, which demotes a non-current
// ref to trust=evidence-only -- could therefore never fire on status grounds, and a reader was
// told to trust a claim the pack itself described as disputed.
func TestContestedPositionIsServedDisputedNotCurrent(t *testing.T) {
	in := positionStatusInput()
	snapshot, ref := positionStatusSnapshot(true)

	pack := projectRuntimePack(Pack(in, DefaultPolicy(), fixedNow), in, snapshot)

	inc, ok := includedRefByName(pack, ref)
	if !ok {
		t.Fatalf("contested position ref was not included at all: %+v", pack.Included)
	}
	if inc.Status != "disputed" {
		t.Fatalf("contested position served with status %q, want \"disputed\" (the pack cannot say contested in prose and current on the wire)", inc.Status)
	}
}

// The other half of the gate: only genuinely contested positions may be demoted. Marking every
// position disputed would demote the whole recall surface to evidence-only and make the signal
// meaningless -- the same as never emitting it.
func TestUncontestedPositionStaysCurrent(t *testing.T) {
	in := positionStatusInput()
	snapshot, ref := positionStatusSnapshot(false)

	pack := projectRuntimePack(Pack(in, DefaultPolicy(), fixedNow), in, snapshot)

	inc, ok := includedRefByName(pack, ref)
	if !ok {
		t.Fatalf("position ref was not included at all: %+v", pack.Included)
	}
	if inc.Status != "current" {
		t.Fatalf("uncontested position served with status %q, want \"current\"", inc.Status)
	}
}

// The durable invariant, stated once so it cannot drift apart again: for every served position,
// the contested flag on ActivePositions and the status on the matching IncludedRef are two
// renderings of ONE fact. A pack in which they disagree is self-contradicting, whichever way
// round the disagreement runs.
func TestServedPositionContestedFlagAndRefStatusNeverDisagree(t *testing.T) {
	for _, contested := range []bool{true, false} {
		in := positionStatusInput()
		snapshot, ref := positionStatusSnapshot(contested)

		pack := projectRuntimePack(Pack(in, DefaultPolicy(), fixedNow), in, snapshot)

		inc, ok := includedRefByName(pack, ref)
		if !ok {
			t.Fatalf("contested=%v: position ref missing from Included: %+v", contested, pack.Included)
		}
		var served *envelope.ActivePosition
		for i := range pack.ActivePositions {
			if pack.ActivePositions[i].Ref == ref {
				served = &pack.ActivePositions[i]
			}
		}
		if served == nil {
			t.Fatalf("contested=%v: position missing from ActivePositions: %+v", contested, pack.ActivePositions)
		}
		if served.Contested != (inc.Status == "disputed") {
			t.Fatalf("contested=%v: ActivePositions says contested=%v while the included ref says status=%q -- one fact, two answers", contested, served.Contested, inc.Status)
		}
	}
}

// A contested position whose ACTIVE claim is the one under review is dropped from the current
// view by mem.visibleCurrentViewClaims, so loadPositionRefsFromView cannot resolve it and used
// to `continue` in silence. Measured on the live ledger at the time this was written: 25 of 26
// contested positions land in exactly that branch, so the disputed status alone would have been
// almost entirely dormant.
//
// The claim stays OUT of ambient recall -- an unadjudicated stance must not be served as truth.
// What must change is that the omission is reported rather than invisible, so a reader can tell
// "this was adjudicated away" from "nobody ever looked". Same rule every other skip in that loop
// already follows.
func TestContestedPositionWithUnservableActiveClaimIsReportedNotDropped(t *testing.T) {
	position := memPositionFixture("contested")
	entities := map[string]bool{memoryKey("widget"): true}
	tokens := memoryTextTokens("retention posture for widget")

	reason := unservedPositionExclusionReason(position, entities, tokens)
	if reason == "" {
		t.Fatal("a task-relevant contested position must be reported as excluded, not dropped in silence")
	}
	if !strings.Contains(strings.ToLower(reason), "contested") {
		t.Fatalf("the exclusion reason must name the dispute, got %q", reason)
	}
}

// The flood guard. Every ledger carries positions no task is asking about; reporting all of them
// would bury the signal in the very surface meant to carry it, which is how an operator learns
// to stop reading exclusions.
func TestUnrelatedContestedPositionIsNotReported(t *testing.T) {
	position := memPositionFixture("contested")
	entities := map[string]bool{memoryKey("somethingelse"): true}
	tokens := memoryTextTokens("an unrelated task about billing")

	if reason := unservedPositionExclusionReason(position, entities, tokens); reason != "" {
		t.Fatalf("an unrelated position must stay silent, got %q", reason)
	}
}

// Only the contested case is claimed here. A position can also be unservable because its active
// claim was retired; that is a different condition and this branch must not pretend to explain
// it -- a wrong reason is worse than a documented gap.
func TestNonContestedUnservablePositionIsNotClaimedAsDisputed(t *testing.T) {
	position := memPositionFixture("active")
	entities := map[string]bool{memoryKey("widget"): true}
	tokens := memoryTextTokens("retention posture for widget")

	if reason := unservedPositionExclusionReason(position, entities, tokens); reason != "" {
		t.Fatalf("only a contested position may be reported by this branch, got %q", reason)
	}
}

func memPositionFixture(status string) mem.Position {
	return mem.Position{
		ID:            "widget::retention::slot-abc123",
		Entity:        "widget",
		Topic:         "retention",
		Predicate:     "posture",
		ClaimType:     "status",
		ActiveClaimID: "claim_missing_from_view",
		Status:        status,
		AsOfSeq:       41,
	}
}
