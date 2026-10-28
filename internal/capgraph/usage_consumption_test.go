package capgraph

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// addUsageGraphNode writes a capability node into the graph's node log, so
// the usage projection can attribute a tool invocation to it.
func addUsageGraphNode(t *testing.T, home string, n Node) {
	t.Helper()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(n); err != nil {
		t.Fatal(err)
	}
}

// TestCapabilityUsageCountsToolUsesAndNonSkillOffers is the falsifier for the
// consumption-measurement defect.
//
// The defect: a capability that is not a skill could never reach a fired or
// declared-use count. Every consumer funnelled through normalizeSkillRef,
// which accepts skill refs only, and the hook emitted a use event only when
// the native tool name was "skill". So a tool offered hundreds of times and
// invoked thousands of times still read as never fired -- the evidence
// EXISTED and the reader could not hold it.
//
// This test is red on the previous code (the tool's use is dropped, and the
// non-skill pack offer is dropped) and green after.
func TestCapabilityUsageCountsToolUsesAndNonSkillOffers(t *testing.T) {
	home := t.TempDir()
	const (
		toolRef    = "capability://tool:headroom.headroom-retrieve"
		serverRef  = "capability://mcp:headroom"
		skillRef   = "capability://skill:diagnosing-bugs"
		packID     = "ctx_tool_use"
		sessionID  = "sess_tool_use"
		workID     = "work_0123456789ab"
		nativeTool = "mcp__headroom__headroom_retrieve"
	)
	writeContextPack(t, home, packID, nil)
	addUsagePackOffers(t, home, packID, []envelope.CapabilityOffer{
		{Ref: toolRef, Kind: "tool", Name: "headroom.headroom_retrieve", Score: 1.5, Why: "matched large context"},
		{Ref: serverRef, Kind: "mcp", Name: "headroom", Score: 1.0, Why: "matched context optimization"},
		{Ref: skillRef, Kind: "skill", Name: "diagnosing-bugs", Score: 1.2, Why: "matched flaky regression"},
	})
	// The graph holds the tool node and its server. A tool the graph does
	// not hold stays unmeasurable -- see the sibling test.
	addUsageGraphNode(t, home, Node{ID: "tool:headroom.headroom-retrieve", Kind: "tool", Name: "headroom.headroom_retrieve"})
	addUsageGraphNode(t, home, Node{ID: "mcp:headroom", Kind: "mcp", Name: "headroom"})

	header := func(id, created string) envelope.Header {
		return envelope.Header{
			SchemaVersion: envelope.SVHookEvent,
			ID:            id,
			CreatedAt:     created,
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID, SessionID: sessionID, ContextPackID: packID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		}
	}
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_tool_offer", t0.Format(time.RFC3339)),
		Harness: "claude_code", HookName: "SessionStart", HookCapability: "context_injection",
		SideEffects: []string{}, Status: "ok",
	})
	// The tool invocation. A real PostToolUse event records the invoked
	// native tool name in its side_effects, alongside the adapter's
	// governed artifacts -- that is where the evidence has always lived.
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_tool_fire", t0.Add(time.Second).Format(time.RFC3339)),
		Harness: "claude_code", HookName: "PostToolUse", HookCapability: "telemetry",
		SideEffects: []string{nativeTool}, Status: "ok",
	})
	// A second event whose side_effects also carry a non-tool ref. The
	// non-tool ref must not be read as a tool call.
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_tool_fire_legacy", t0.Add(2*time.Second).Format(time.RFC3339)),
		Harness: "claude_code", HookName: "PostToolUse", HookCapability: "telemetry",
		SideEffects: []string{"compound://events/src_live_abc", nativeTool}, Status: "ok",
	})

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}

	tool := findCap(proj, toolRef)
	if tool == nil {
		t.Fatalf("tool capability %q missing from the projection", toolRef)
	}
	if tool.OfferedCount != 1 {
		t.Fatalf("tool offered count = %d, want 1 (a non-skill pack offer must be counted)", tool.OfferedCount)
	}
	if tool.FiredCount != 2 {
		t.Fatalf("tool fired count = %d, want 2 (a recorded tool invocation must count as a use, in the current event and in a legacy event that carries the name only in side_effects)", tool.FiredCount)
	}
	if tool.LastFired == "" || len(tool.SourceEvents) != 2 {
		t.Fatalf("tool fire evidence = %+v, want both invoking events", tool)
	}
	if inSummary(proj.Summary.NeverFired, toolRef) {
		t.Fatalf("a tool that fired is reported as never fired: %+v", proj.Summary.NeverFired)
	}

	// The server that carries the tool is a separate capability, and the
	// invocation is evidence for it too.
	server := findCap(proj, serverRef)
	if server == nil || server.FiredCount != 2 {
		t.Fatalf("server capability usage = %+v, want two fires from the same invocations", server)
	}

	// The control: a skill use is counted exactly as before.
	skill := findCap(proj, skillRef)
	if skill == nil || skill.OfferedCount != 1 {
		t.Fatalf("skill capability usage = %+v, want one offer", skill)
	}
}

// TestCapabilityUsageDoesNotInventAToolTheGraphDoesNotHold is the other half
// of the contract: making consumption measurable must not make the projection
// claim a capability the graph never declared. A server with no node is
// UNMEASURABLE, and inventing a node for it would put a capability in the
// projection that no catalog ever declared.
func TestCapabilityUsageDoesNotInventAToolTheGraphDoesNotHold(t *testing.T) {
	home := t.TempDir()
	const (
		packID     = "ctx_unheld_tool"
		sessionID  = "sess_unheld_tool"
		workID     = "work_0123456789ab"
		nativeTool = "mcp__cua_repl__js"
	)
	writeContextPack(t, home, packID, nil)
	header := func(id, created string) envelope.Header {
		return envelope.Header{
			SchemaVersion: envelope.SVHookEvent,
			ID:            id,
			CreatedAt:     created,
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID, SessionID: sessionID, ContextPackID: packID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		}
	}
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_unheld_tool", t0.Format(time.RFC3339)),
		Harness: "claude_code", HookName: "PostToolUse", HookCapability: "telemetry",
		SideEffects: []string{nativeTool}, Status: "ok",
	})

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range proj.Capabilities {
		if strings.Contains(c.Ref, "cua-repl") || strings.Contains(c.Ref, "cua_repl") {
			t.Fatalf("projection invented a capability for a tool the graph does not hold: %+v", c)
		}
	}
}
