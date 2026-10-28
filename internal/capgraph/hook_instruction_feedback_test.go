// hook_instruction_feedback_test.go is the red-first falsifier seam for the
// instruction-and-hook feedback burst (work_f8aa6916e763). It proves, before
// any production code lands:
//
//  1. a completed-changed receipt writes validated_by edges onto a known
//     instruction_rule node AND a known hook node, with the receipt id
//     carried in SourceReceipt — an offer alone cannot install one.
//  2. an explicit missed_when / false_triggered_by feedback from a receipt
//     reaches the same known owners and preserves evidence provenance.
//  3. an unknown hook / instruction identity (no node on disk) stays review
//     debt: receipt feedback never installs the node, and the unknown is
//     surfaced in SkippedUnknown so an operator can repair it.
//  4. FeedbackCountsByKind is a deterministic, private-body-free summary
//     of validated_by / missed_when / false_triggered_by counts grouped
//     by node kind (the read model the orient operator's per-kind
//     projection consumes).
//
// All four tests are scoped to internal/capgraph per the packet's hard
// scope. They are red against the current source (no "hook" kind, no
// FeedbackCountsByKind, and ConsumeReceipts treats instruction_rule and
// hook nodes identically to skills but never explicitly proves receipt
// feedback reaches them with source-receipt provenance).
package capgraph

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestConsumeReceiptsReachesInstructionAndHookOwnersWithProvenance is the
// F1+F2 falsifier: a completed-changed receipt mentioning a known
// instruction_rule and a known hook owner must add validated_by edges to
// BOTH with SourceReceipt populated. The first receipt also carries
// explicit missed / false-triggered feedback that must reach the same
// owners with provenance preserved.
func TestConsumeReceiptsReachesInstructionAndHookOwnersWithProvenance(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{
		ID:       "instruction_rule:act-inside-envelope",
		Kind:     "instruction_rule",
		Name:     "Act Inside the Envelope",
		Triggers: []string{"envelope", "scope", "permission"},
		Weight:   1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "hook:claude-code:session-start-blocker",
		Kind:     "hook",
		Name:     "session-start-blocker",
		Triggers: []string{"session_start", "blocker"},
		Weight:   1,
	}); err != nil {
		t.Fatal(err)
	}
	rec := testReceipt("rec_inst_hook", "completed", true,
		[]string{
			"capability://instruction_rule:act-inside-envelope",
			"capability://hook:claude-code:session-start-blocker",
		},
		[]string{"capability://hook:claude-code:session-start-blocker"},
		nil,
	)
	rec.Skills.FalseTriggered = []string{"capability://instruction_rule:act-inside-envelope"}
	rec.StateDelta.Summary = "instruction envelope and session-start blocker"
	writeReceipt(t, home, rec)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedDeclaredUseEdges < 2 {
		t.Fatalf("report = %+v, want >=2 declared edges (one per owner)", report)
	}
	if report.AppliedMissedEdges < 1 || report.AppliedFalseEdges < 1 {
		t.Fatalf("report = %+v, want missed and false-trigger edges applied", report)
	}
	if len(report.SkippedUnknown) != 0 {
		t.Fatalf("known owners must not be in SkippedUnknown: %v", report.SkippedUnknown)
	}

	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdDeclaredUsedBy, "instruction_rule:act-inside-envelope", "rec_inst_hook") {
		t.Fatalf("edges = %+v, want instruction_rule declared by rec_inst_hook", edges)
	}
	if !hasEdge(edges, EdDeclaredUsedBy, "hook:claude-code:session-start-blocker", "rec_inst_hook") {
		t.Fatalf("edges = %+v, want hook declared by rec_inst_hook", edges)
	}
	hookValidated := findEdge(edges, EdDeclaredUsedBy, "context:envelope", "hook:claude-code:session-start-blocker")
	if hookValidated == nil {
		t.Fatalf("edges = %+v, want context:envelope->hook declared edge with provenance", edges)
	}
	if hookValidated.SourceReceipt != "rec_inst_hook" {
		t.Fatalf("hook declared context edge missing source receipt: %+v", hookValidated)
	}
	if strings.TrimSpace(hookValidated.Provenance) == "" {
		t.Fatalf("hook declared context edge missing provenance: %+v", hookValidated)
	}
	hookMissed := findEdge(edges, EdMissedWhen, "context:blocker", "hook:claude-code:session-start-blocker")
	if hookMissed == nil {
		t.Fatalf("edges = %+v, want missed_when context edge on hook from receipt summary", edges)
	}
	if hookMissed.SourceReceipt != "rec_inst_hook" {
		t.Fatalf("hook missed edge missing source receipt: %+v", hookMissed)
	}
	instFalse := findEdge(edges, EdFalseTriggeredBy, "context:envelope", "instruction_rule:act-inside-envelope")
	if instFalse == nil {
		t.Fatalf("edges = %+v, want false_triggered_by edge on instruction_rule from receipt summary", edges)
	}
	if instFalse.SourceReceipt != "rec_inst_hook" {
		t.Fatalf("instruction_rule false_trigger edge missing source receipt: %+v", instFalse)
	}
}

// TestConsumeReceiptsUnknownHookAndInstructionIdentitiesAreReviewDebt is
// the F4 falsifier: a receipt that names a hook or instruction identity
// the graph has never seen must NOT install a node. The identity is
// surfaced as SkippedUnknown so an operator can repair it (import the
// installed-hooks manifest, register the instruction rule, etc.).
// Receipt consumption never crosses the boundary that is the spine's
// whole point — "any new node is an adjudication outcome, not a
// consequence of a receipt arriving".
func TestConsumeReceiptsUnknownHookAndInstructionIdentitiesAreReviewDebt(t *testing.T) {
	home, s := newStore(t)
	rec := testReceipt("rec_unknown", "completed", true,
		[]string{
			"capability://hook:claude-code:unregistered-blocker",
			"capability://instruction_rule:never-imported-rule",
		},
		[]string{"capability://hook:claude-code:unregistered-blocker"},
		nil,
	)
	rec.Skills.FalseTriggered = []string{"capability://instruction_rule:never-imported-rule"}
	rec.StateDelta.Summary = "ghost identities must remain review debt"
	writeReceipt(t, home, rec)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedEdges != 0 {
		t.Fatalf("unknown identities must not produce edges: %+v", report)
	}
	if !sortIncludesString(report.SkippedUnknown, "hook:claude-code:unregistered-blocker") ||
		!sortIncludesString(report.SkippedUnknown, "instruction_rule:never-imported-rule") {
		t.Fatalf("expected both unknown identities in SkippedUnknown, got %v", report.SkippedUnknown)
	}

	// The graph MUST NOT have grown any node.
	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if strings.Contains(n.ID, "unregistered-blocker") || strings.Contains(n.ID, "never-imported-rule") {
			t.Fatalf("receipt feedback must not install a node, found %+v", n)
		}
	}
	// Edges file must be absent or empty: no feedback, no nodes.
	if edges := readCapEdges(t, home); len(edges) != 0 {
		t.Fatalf("unknown identities must not create any edge, got %+v", edges)
	}
}

// TestFeedbackCountsByKindIsDeterministicNoPrivateBody is the F5
// read-model falsifier. The orient operator must be able to surface
// per-node-kind feedback counts without round-tripping receipt bodies,
// so an operator can repair a bad rule or hook from a glance. The
// summary groups validated_by / missed_when / false_triggered_by edge
// counts by the kind of the receiving node, with the listed kinds
// sorted alphabetically and the receipt set sorted by id (deterministic
// across runs, comparable across machines).
func TestFeedbackCountsByKindIsDeterministicNoPrivateBody(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "instruction_rule:rule-a", Kind: "instruction_rule", Name: "Rule A", Weight: 1, Triggers: []string{"rule"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "instruction_rule:rule-b", Kind: "instruction_rule", Name: "Rule B", Weight: 1, Triggers: []string{"rule"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "hook:claude-code:pre-tool-blocker", Kind: "hook", Name: "pre-tool-blocker", Weight: 1, Triggers: []string{"pre_tool"}}); err != nil {
		t.Fatal(err)
	}
	// Three receipts, deterministic timestamps, so receipt order and
	// per-receipt aggregation is reproducible.
	rec1 := testReceipt("rec_inst_a", "completed", true,
		[]string{"capability://instruction_rule:rule-a"}, nil, nil)
	rec1.StateDelta.Summary = "rule a declared once"
	writeReceipt(t, home, rec1)
	rec2 := testReceipt("rec_inst_b", "completed", true,
		[]string{"capability://instruction_rule:rule-b"}, nil, nil)
	rec2.StateDelta.Summary = "rule b declared once"
	writeReceipt(t, home, rec2)
	rec3 := testReceipt("rec_hook_validation", "completed", true,
		[]string{"capability://hook:claude-code:pre-tool-blocker"},
		[]string{"capability://hook:claude-code:pre-tool-blocker"},
		nil,
	)
	rec3.Skills.FalseTriggered = []string{"capability://instruction_rule:rule-b"}
	rec3.StateDelta.Summary = "hook used; hook missed; rule b false triggered"
	writeReceipt(t, home, rec3)
	if _, err := s.ConsumeReceipts(home, t0); err != nil {
		t.Fatal(err)
	}

	summary := FeedbackCountsByKind(home)
	if len(summary) == 0 {
		t.Fatalf("feedback counts by kind empty after three receipts")
	}
	// Kinds must be sorted alphabetically.
	kinds := make([]string, 0, len(summary))
	for k := range summary {
		kinds = append(kinds, k)
	}
	// Map iteration order is random; determinism is pinned by comparing the
	// exact kind set after sorting, and by ReceiptIDs sorting below.
	sort.Strings(kinds)
	if !reflect.DeepEqual(kinds, []string{"hook", "instruction_rule"}) {
		t.Fatalf("summary kinds = %v, want [hook instruction_rule]", kinds)
	}
	// Required kinds must be present.
	wantKinds := map[string]bool{"hook": true, "instruction_rule": true}
	for k := range wantKinds {
		if _, ok := summary[k]; !ok {
			t.Fatalf("missing kind %q in summary: %+v", k, summary)
		}
	}

	hook := summary["hook"]
	if hook.NodeCount != 1 {
		t.Fatalf("hook node count = %d, want 1 (%+v)", hook.NodeCount, hook)
	}
	if hook.DeclaredUsedBy != 1 {
		t.Fatalf("hook declared_used_by = %d, want 1 (%+v)", hook.DeclaredUsedBy, hook)
	}
	if hook.MissedWhen != 1 {
		t.Fatalf("hook missed_when = %d, want 1 (%+v)", hook.MissedWhen, hook)
	}
	if hook.FalseTriggeredBy != 0 {
		t.Fatalf("hook false_triggered_by = %d, want 0 (%+v)", hook.FalseTriggeredBy, hook)
	}
	// Receipts are sorted by id (deterministic, comparable).
	wantHookReceipts := []string{"rec_hook_validation"}
	if !reflect.DeepEqual(hook.ReceiptIDs, wantHookReceipts) {
		t.Fatalf("hook receipt ids = %v, want %v", hook.ReceiptIDs, wantHookReceipts)
	}

	rule := summary["instruction_rule"]
	if rule.NodeCount != 2 {
		t.Fatalf("instruction_rule node count = %d, want 2 (%+v)", rule.NodeCount, rule)
	}
	if rule.DeclaredUsedBy != 2 {
		t.Fatalf("instruction_rule declared_used_by = %d, want 2 (%+v)", rule.DeclaredUsedBy, rule)
	}
	if rule.FalseTriggeredBy != 1 {
		t.Fatalf("instruction_rule false_triggered_by = %d, want 1 (%+v)", rule.FalseTriggeredBy, rule)
	}
	// Source receipt ids are sorted ascending (deterministic).
	wantRuleReceipts := []string{"rec_hook_validation", "rec_inst_a", "rec_inst_b"}
	if !reflect.DeepEqual(rule.ReceiptIDs, wantRuleReceipts) {
		t.Fatalf("instruction_rule receipt ids = %v, want %v", rule.ReceiptIDs, wantRuleReceipts)
	}

	// No private body leakage: the summary must not carry receipt
	// summary text, raw evidence refs from the receipt, or any prompt
	// content. The field set is the allowlist.
	if got := strings.TrimSpace(rule.RawText); got != "" {
		t.Fatalf("summary leaked private text: %q", got)
	}
	if got := strings.TrimSpace(hook.RawText); got != "" {
		t.Fatalf("summary leaked private text: %q", got)
	}
}

// sortIncludesString is a tiny helper used in the unknown-identity test
// so the assertion reads as "the unknown set MUST contain X" without
// assuming JSONL edge enumeration order.
func sortIncludesString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// ensure unused imports don't drift away if the test surface above is
// trimmed; these are referenced by the production code we are about to
// add in installed_hooks.go.
var _ = filepath.Join
var _ = time.Now
