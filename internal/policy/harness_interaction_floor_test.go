package policy

import (
	"path/filepath"
	"testing"

	"compound/internal/envelope"
)

// The defect these tests pin: AskUserQuestion and the session-bookkeeping
// tools fell through ClassifyToolAction into the unknown default, and the
// unknown default raised three floors at once -- unknown classification,
// missing schema, missing destination -- plus one source-deny per lineage
// source ("source does not allow sink unknown"). The envelope can clear the
// first floor only, so a single operator question produced a wall of holds
// and denies. Harness-internal interaction tools carry no destination
// outside the live session and write to no persistent sink, so they must
// classify known, read-class, sink-free, and destination-free. The
// fail-closed default for tools NOT on that closed list is pinned at the
// bottom of this file and must stay.

func TestHarnessInteractionToolsClassifyKnownReadSinkFree(t *testing.T) {
	for _, tool := range []string{
		"AskUserQuestion", "ask_user_question",
		"TodoWrite", "todo_write",
		"TodoRead", "todo_read",
		"clockcurr_time", "clocksleep", "wait", "listagents", "todo", "request_user_input_async",
	} {
		t.Run(tool, func(t *testing.T) {
			got := ClassifyToolAction(ToolActionRequest{Tool: tool, Arguments: map[string]any{"question": "fixture"}})
			if !got.Known {
				t.Fatalf("%q classified unknown: %+v", tool, got)
			}
			if got.Domain != ActionDomainHarness {
				t.Fatalf("%q domain = %q, want %q", tool, got.Domain, ActionDomainHarness)
			}
			if got.MutationClass != "read" {
				t.Fatalf("%q mutation = %q, want read", tool, got.MutationClass)
			}
			if got.Reversibility != "none" {
				t.Fatalf("%q reversibility = %q, want none", tool, got.Reversibility)
			}
			if got.BlastRadius != "local" {
				t.Fatalf("%q blast radius = %q, want local", tool, got.BlastRadius)
			}
			if got.RequiredSink != "" {
				t.Fatalf("%q required sink = %q; interaction tools write to no persistent sink", tool, got.RequiredSink)
			}
			if got.NeedsDestination {
				t.Fatalf("%q demands a destination; the operator-facing session IS its destination", tool)
			}
			if got.NeedsSchema {
				t.Fatalf("%q demands a schema revision; harness interaction carries none", tool)
			}
		})
	}
}

// TestMonitorHarnessInteractionToolsRaiseNoSecurityFloor is the regression for
// the live storm: the SAME call with the SAME lineage that produced
// "unknown tool/action classification fails closed", "missing tool schema
// revision fails closed", "missing or unresolved destination fails closed",
// and N x "source ... does not allow sink unknown" must come back allowed
// with no security floor on the intent.
func TestMonitorHarnessInteractionToolsRaiseNoSecurityFloor(t *testing.T) {
	source := envelope.NewSourceEnvelope(
		testSourceHeader("src_live_harness_interaction"),
		envelope.SourceOrigin{Kind: "session", Locator: "claude_qwen:fixture"},
		[]byte("conversation context the question draws on"),
		envelope.SourceTrustMixed,
		envelope.ConfidentialityInternal,
		[]string{"reader"},
	)
	for _, tool := range []string{"AskUserQuestion", "TodoWrite", "TodoRead", "clockcurr_time", "clocksleep", "wait", "listagents", "todo", "request_user_input_async"} {
		t.Run(tool, func(t *testing.T) {
			request := monitorRequest(tool, map[string]any{"question": "fixture"})
			request.Sources = []envelope.SourceEnvelope{source}
			monitored := MonitorToolAction(request, DefaultConfig(), fixedNow)
			if !Allowed(monitored.Decision) {
				t.Fatalf("%s outcome=%q; harness interaction must not be held or denied: %v",
					tool, monitored.Decision.Outcome, monitored.Decision.Reasons)
			}
			if monitored.Intent.SecurityFloorOutcome != "" {
				t.Fatalf("%s raised security floor %q: %v",
					tool, monitored.Intent.SecurityFloorOutcome, monitored.Intent.SecurityReasons)
			}
		})
	}
}

// TestFileWriteOntoSessionOwnedDirtIsNotCapture pins the companion gate fix:
// a single-file write onto a file THIS SESSION already dirtied rewrites its
// own work in progress and cannot capture or destroy the foreign dirt that
// lives on other paths. Unowned dirt and unresolvable targets stay held.
func TestFileWriteOntoSessionOwnedDirtIsNotCapture(t *testing.T) {
	scope := t.TempDir()
	repo := filepath.Join(scope, "repo")
	cfg := envelopeConfig(scope)
	target := filepath.Join(repo, "owned.go")

	write := func(targetStatus string, sessionOwnsTarget bool) envelope.PolicyDecision {
		sc := foreignDirtyContext(repo, 8)
		sc.TargetPath = target
		sc.TargetStatus = targetStatus
		sc.SessionOwnsTarget = sessionOwnsTarget
		ai := envelope.ActionIntent{
			Header: envelope.Header{ID: "act_owned_write", IDs: envelope.IDs{WorkID: "work_wip"}},
			Actor:  "claude_minimax", Capability: "filesystem.write", Action: "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: target},
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
		}
		return EvaluateWithContext(ai, cfg, EvaluationContext{SourceControl: &sc}, envelopeNow())
	}

	if pd := write("dirty", true); !Allowed(pd) {
		t.Fatalf("outcome=%q; continuing an edit this session began is not capturing foreign work: %v",
			pd.Outcome, pd.Reasons)
	}
	if pd := write("dirty", false); Allowed(pd) {
		t.Fatalf("outcome=%q; a dirty target nobody attributes to this session must stay held", pd.Outcome)
	}
	if pd := write("", true); Allowed(pd) {
		t.Fatalf("outcome=%q; an unresolvable target fails closed even under an ownership claim", pd.Outcome)
	}
}

// TestHarnessSideEffectToolsStayFailClosed: the interaction list is CLOSED.
// Orchestration, scheduling, publication, and resource tools carry host or
// outbound side effects (or are not session-scoped at all) and keep the
// unknown default until each gets an explicit domain of its own. Unpinning a
// line here is a doctrine change, not a test fix.
func TestHarnessSideEffectToolsStayFailClosed(t *testing.T) {
	for _, tool := range []string{
		"Workflow",
		"Artifact",
		"ScheduleWakeup",
		"readnotifications",
		"pushnotification",
		"ListMcpResourcesTool",
		"ReadMcpResourceTool",
	} {
		got := ClassifyToolAction(ToolActionRequest{Tool: tool})
		if got.Known {
			t.Fatalf("%q unexpectedly known: %+v", tool, got)
		}
	}
}
