package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStopHookActiveStillShortCircuitsForCodex (F6 second half) pins that the Stop
// short-circuit at hook.go:2103 keeps its teeth after slice 3 added a codex-specific
// rollout derivation to buildWorklessSessionReceipt. Without the short-circuit a codex
// Stop with stop_hook_active:true and a populated transcript_path would slip into the
// receipt-build branch and silently emit a SessionEnd receipt the harness did not
// ask for (Stop is end-of-turn continuation, not a session receipt).
//
// The packet pins this as "green-at-birth -- prove its teeth by a scratch mutation":
// the scratch mutation here is the inverse -- we set transcript_path to a real file
// (so buildWorklessSessionReceipt would derive a receipt on codex if it ran) AND a
// real session_meta line on the wire, and assert the short-circuit fires anyway. If
// a future change moves the short-circuit below the receipt-build branch, this test
// turns RED because a receipt is produced with Skills.Used populated.
func TestStopHookActiveStillShortCircuitsForCodex(t *testing.T) {
	const sessionID = "sess-stop-circuit"
	codexHome := t.TempDir()
	rollout := writeCodexRollout(t, codexHome, sessionID, []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/product"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:05:00Z","payload":{"type":"function_call","name":"Skill","arguments":"{\"skill\":\"example-skill\"}"}}`,
	})

	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{
		Hook:           "stop",
		Harness:        "codex",
		SessionID:      sessionID,
		TurnID:         "turn-stop-circuit",
		Cwd:            "/repo/product",
		TranscriptPath: rollout,
		StopHookActive: true,
		Summary:        "turn stopped after adapter handling",
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 0 {
		t.Fatalf("produced = %v, want [] (Stop short-circuit must suppress receipt)", got)
	}
	want := "recorded handled Stop event without replaying a session receipt"
	if !strings.Contains(res.Summary, want) {
		t.Fatalf("summary = %q, want substring %q", res.Summary, want)
	}
	if res.Event.Status != "ok" {
		t.Fatalf("Event.Status = %q, want ok (handled Stop is informational, not a failure)", res.Event.Status)
	}

	// Now mutate: clear stop_hook_active AND set StopTriageHandled -- the
	// StopTriageHandled branch must also short-circuit. This is the second
	// stop-hook-active-style flag the runtime can carry; the original test only
	// covered StopHookActive. If a future refactor collapses these into one
	// boolean, both assertions still hold.
	res, err = Run(Fixture{
		Hook:              "stop",
		Harness:           "codex",
		SessionID:         sessionID,
		TurnID:            "turn-stop-circuit",
		Cwd:               "/repo/product",
		TranscriptPath:    rollout,
		StopTriageHandled: true,
		Summary:           "turn stopped after triage",
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("triage run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 0 {
		t.Fatalf("StopTriageHandled produced = %v, want []", got)
	}
	if !strings.Contains(res.Summary, want) {
		t.Fatalf("StopTriageHandled summary = %q, want substring %q", res.Summary, want)
	}

	// Third mutation: NO short-circuit flag, no work_id -- the receipt path runs.
	// This is the same fixture minus the StopHookActive/TriageHandled flips, so it
	// would produce a receipt (verifying the short-circuit flag is what suppresses
	// the receipt, not anything intrinsic to the harness+transcript combo).
	res, err = Run(Fixture{
		Hook:           "stop",
		Harness:        "codex",
		SessionID:      sessionID,
		TurnID:         "turn-stop-circuit",
		Cwd:            "/repo/product",
		TranscriptPath: rollout,
		Summary:        "stop without handled marker",
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("unmarked run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("unmarked Stop produced = %v, want [receipt] (no short-circuit => receipt path runs)", got)
	}
}

// TestSessionEndCodexWithoutShortCircuitProducesReceipt is the symmetry proof:
// SessionEnd (NOT Stop) on the same codex fixture produces a receipt, never
// short-circuits. Together with TestStopHookActiveStillShortCircuitsForCodex this
// pins the routing: Stop = end-of-turn continuation (short-circuit when handled);
// SessionEnd = end-of-session receipt (always builds when transcript is on hand).
func TestSessionEndCodexWithoutShortCircuitProducesReceipt(t *testing.T) {
	const sessionID = "sess-end-receipt"
	transcriptHome := t.TempDir()
	rollout := writeCodexRollout(t, transcriptHome, sessionID, []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/product"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:05:00Z","payload":{"type":"function_call","name":"Skill","arguments":"{\"skill\":\"example-skill\"}"}}`,
	})
	// Ensure no leftover writeable file would let the resolver fire for SessionEnd.
	if err := os.RemoveAll(filepath.Join(t.TempDir(), "never")); err != nil {
		t.Fatal(err)
	}

	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "codex",
		SessionID:      sessionID,
		Cwd:            "/repo/product",
		TranscriptPath: rollout,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
}
