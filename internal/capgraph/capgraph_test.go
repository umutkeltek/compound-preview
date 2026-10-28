package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

var t0 = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (string, *Store) {
	t.Helper()
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return home, s
}

func TestAddNodeSkipsIdenticalReplayButKeepsUpdates(t *testing.T) {
	home, s := newStore(t)
	node := Node{ID: "eval:capability-evaluation-external-caveman", Kind: "eval", Name: "Capability evaluation: token-economy", Triggers: []string{"token", "economy"}, Weight: 0.25, EvidenceOnly: true}
	if err := s.Add(node); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(node); err != nil {
		t.Fatal(err)
	}
	if got := len(readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))); got != 1 {
		t.Fatalf("identical replay appended duplicate node rows: got %d", got)
	}
	node.Triggers = append(node.Triggers, "compression")
	if err := s.Add(node); err != nil {
		t.Fatal(err)
	}
	// Add is upsert-by-id (slice 1): a same-id drift REPLACES in place, so the
	// row count stays at 1 -- the update is visible via List(), not via a new
	// file row. See TestStoreAddReplacesDriftedSameIDRow for the full assertion
	// including order preservation.
	if got := len(readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))); got != 1 {
		t.Fatalf("drift Add should replace in place (one row), got %d", got)
	}
	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !contains(nodes[0].Triggers, "compression") {
		t.Fatalf("latest node update not loaded: %+v", nodes)
	}
}

func readNonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestInstructionRuleManifestSurfacesByContext(t *testing.T) {
	home, store := newStore(t)
	manifest, err := LoadSkillManifest(filepath.Join("..", "..", "fixtures", "skills", "instruction-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	matched, err := store.SurfaceForContext(SurfaceContext{Task: "repair memory recall", RepoKind: "control-plane", Harness: "codex", Phase: "execute"})
	if err != nil {
		t.Fatal(err)
	}
	rule := surfacedByID(matched, "instruction_rule:memory-recall-adjudication")
	if rule == nil {
		t.Fatalf("matched context did not surface instruction rule: %+v", matched)
	}
	if rule.Node.Tier != 1 || len(rule.Node.Read) == 0 || len(rule.Node.Route) == 0 || len(rule.Node.Boundary) == 0 {
		t.Fatalf("instruction rule metadata not preserved on node: %+v", rule.Node)
	}
	if !contains(rule.MatchedTokens, "memory") && !contains(rule.MatchedTokens, "recall") {
		t.Fatalf("instruction rule should expose matched trigger tokens: %+v", rule.MatchedTokens)
	}

	unmatched, err := store.SurfaceForContext(SurfaceContext{Task: "update pricing copy", RepoKind: "product", Harness: "codex", Phase: "execute"})
	if err != nil {
		t.Fatal(err)
	}
	if surfacedByID(unmatched, "instruction_rule:memory-recall-adjudication") != nil {
		t.Fatalf("unmatched context must not surface memory rule: %+v", unmatched)
	}
}

func surfacedByID(values []Surfaced, id string) *Surfaced {
	for i := range values {
		if values[i].Node.ID == id {
			return &values[i]
		}
	}
	return nil
}
func TestRecordSourceAuthorityDedupesSameRevisionAcrossSnapshotPaths(t *testing.T) {
	home, _ := newStore(t)
	first, err := RecordSourceAuthority(home, SourceAuthorityInput{
		SourcePath:     "imports/mcporter-list-20260701T000000Z.json",
		SourceKind:     "catalog",
		SourceOwner:    "tool-topology",
		SourceClass:    "mcporter-json",
		SourceRevision: "sha256:same-snapshot",
		ImportedVia:    "compound capability import-topology",
		NodeIDs:        []string{"mcp:context7"},
		EvidenceRefs:   []string{"mcporter_json:same"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordSourceAuthority(home, SourceAuthorityInput{
		SourcePath:     "imports/mcporter-list-20260704T000000Z.json",
		SourceKind:     "catalog",
		SourceOwner:    "tool-topology",
		SourceClass:    "mcporter-json",
		SourceRevision: "sha256:same-snapshot",
		ImportedVia:    "compound capability import-topology",
		NodeIDs:        []string{"mcp:context7"},
		EvidenceRefs:   []string{"mcporter_json:same"},
	}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same source revision should reuse source authority id: first=%s second=%s", first.ID, second.ID)
	}
	records, err := LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("same source revision should keep one source record, got %d: %+v", len(records), records)
	}
	if records[0].SourcePath != "imports/mcporter-list-20260704T000000Z.json" {
		t.Fatalf("latest source path should win, got %q", records[0].SourcePath)
	}
}

func TestAuthorityRevisionTracksGraphStateOnly(t *testing.T) {
	home, store := newStore(t)
	initial, err := AuthorityRevision(home)
	if err != nil {
		t.Fatal(err)
	}
	node := Node{ID: "tool:revision-fixture", Kind: "tool", Name: "Revision fixture", Triggers: []string{"revision"}}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	afterNode, err := AuthorityRevision(home)
	if err != nil {
		t.Fatal(err)
	}
	if afterNode == initial {
		t.Fatalf("node mutation did not change authority revision: %s", initial)
	}
	if _, err := RecordSourceAuthority(home, SourceAuthorityInput{
		SourcePath:     "fixtures/revision-source.yaml",
		SourceKind:     "synthetic_fixture",
		SourceClass:    "capability",
		SourceRevision: "sha256:source-evidence-only",
		ImportedVia:    "authority revision test",
		NodeIDs:        []string{node.ID},
	}, t0); err != nil {
		t.Fatal(err)
	}
	afterSource, err := AuthorityRevision(home)
	if err != nil {
		t.Fatal(err)
	}
	if afterSource != afterNode {
		t.Fatalf("source handoff evidence changed serving revision: before=%s after=%s", afterNode, afterSource)
	}
	if err := store.AddEdge(Edge{Kind: EdRequires, From: node.ID, To: "tool:dependency"}, t0); err != nil {
		t.Fatal(err)
	}
	afterEdge, err := AuthorityRevision(home)
	if err != nil {
		t.Fatal(err)
	}
	if afterEdge == afterNode {
		t.Fatalf("edge mutation did not change authority revision: %s", afterNode)
	}
}

func TestLoadSourceAuthorityBackfillsLegacyObservedAt(t *testing.T) {
	home, _ := newStore(t)
	logPath := sourceAuthorityLog(home)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"compound.capability_source_authority.v0.1","id":"capsrc_legacy","created_at":"2026-07-01T00:00:00Z","source_path":"imports/mcporter-list.json","source_kind":"catalog","source_owner":"tool-topology","source_class":"mcporter-json","source_revision":"sha256:legacy","imported_via":"compound capability import-topology","authority_ref":"compound://registries/capability-graph","authority_status":"compound_authoritative","source_role":"evidence_only_input","retires":"source_view_authority:imports/mcporter-list.json","rollback":"re-import source","node_ids":["mcp:context7"],"edge_ids":[],"evidence_refs":["mcporter_json:legacy"]}` + "\n"
	if err := os.WriteFile(logPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	if records[0].SourceObservedAt != "2026-07-01T00:00:00Z" || records[0].SourceAgeHoursAtImport != 0 {
		t.Fatalf("legacy source timing not backfilled: %+v", records[0])
	}
}

func TestConsumeReceiptsDeclaresUsedCapabilitiesOnly(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "skill:offered-only", Kind: "skill", Name: "Offered", Triggers: []string{"pricing"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "skill:missed", Kind: "skill", Name: "Missed", Triggers: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	writeReceipt(t, home, testReceipt("rec_used", "completed", true,
		[]string{"capability://skill:pricing", "capability://skill:missing"},
		nil,
		[]string{"capability://skill:offered-only"}))
	writeReceipt(t, home, testReceipt("rec_failed", "failed", true,
		[]string{"capability://skill:offered-only"},
		[]string{"capability://skill:missed"},
		nil))
	writeReceipt(t, home, testReceipt("rec_unchanged", "completed", false,
		[]string{"capability://skill:offered-only"}, nil, nil))
	invalidOld := testReceipt("rec_invalid_old", "completed", true,
		[]string{"capability://skill:offered-only"}, nil, nil)
	invalidOld.MoveType = "control_plane_repair"
	invalidOld.ProofRefs = nil
	b, err := json.MarshalIndent(invalidOld, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "events", invalidOld.ID+".json"), string(b))
	writeFile(t, filepath.Join(home, "events", "tr_not_receipt.json"), `{
  "schema_version": "compound.transition.v0.1"
}`)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReceiptsScanned != 3 || report.UsedCapabilitiesSeen != 3 || report.MissedCapabilitiesSeen != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.AppliedDeclaredUseEdges != 6 || report.AppliedMissedEdges != 2 || report.AppliedEdges != 8 {
		t.Fatalf("report = %+v", report)
	}
	if report.SkippedNonCompleted != 1 || report.SkippedUnchanged != 0 {
		t.Fatalf("skip counts = %+v", report)
	}
	if report.SkippedInvalidReceipt != 1 {
		t.Fatalf("invalid receipt skip count = %+v", report)
	}
	if len(report.SkippedUnknown) != 1 || report.SkippedUnknown[0] != "skill:missing" {
		t.Fatalf("skipped unknown = %v", report.SkippedUnknown)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdDeclaredUsedBy, "skill:pricing", "rec_used") {
		t.Fatalf("edges = %+v, want used skill declared by receipt", edges)
	}
	contextEdge := findEdge(edges, EdDeclaredUsedBy, "context:receipt", "skill:pricing")
	if contextEdge == nil {
		t.Fatalf("edges = %+v, want used skill declared by receipt context", edges)
	}
	if contextEdge.SourceReceipt != "rec_used" || contextEdge.Provenance != "receipt_declared" || contextEdge.Weight <= 0 {
		t.Fatalf("declared context edge missing receipt provenance: %+v", contextEdge)
	}
	if !hasEdge(edges, EdDeclaredUsedBy, "skill:offered-only", "rec_unchanged") {
		t.Fatalf("edges = %+v, want legacy proof-bearing receipt to validate used skill", edges)
	}
	if !hasEdge(edges, EdMissedWhen, "context:receipt", "skill:missed") ||
		!hasEdge(edges, EdMissedWhen, "context:test", "skill:missed") {
		t.Fatalf("edges = %+v, want missed skill context edges from failed receipt summary", edges)
	}
	if hasEdge(edges, EdDeclaredUsedBy, "skill:offered-only", "rec_used") ||
		hasEdge(edges, EdDeclaredUsedBy, "skill:offered-only", "rec_failed") {
		t.Fatalf("offered/failed capability should not validate: %+v", edges)
	}

	replay, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if replay.AppliedEdges != 0 {
		t.Fatalf("replay should be idempotent, got %+v", replay)
	}
	if replay.SkippedInvalidReceipt != 1 {
		t.Fatalf("replay invalid receipt skip count = %+v", replay)
	}
	if got := len(readCapEdges(t, home)); got != len(edges) {
		t.Fatalf("replay appended duplicate edges: before %d after %d", len(edges), got)
	}
}

func TestReceiptScannersPreserveMalformedRecordPolicies(t *testing.T) {
	home, store := newStore(t)
	writeFile(t, filepath.Join(home, "events", "rec_malformed.json"), "{\n")

	if _, err := store.ConsumeReceipts(home, t0); err == nil ||
		!strings.Contains(err.Error(), "parse event envelope rec_malformed.json") {
		t.Fatalf("ConsumeReceipts malformed error = %v", err)
	}
	usage, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil || usage.ReceiptsScanned != 0 {
		t.Fatalf("usage malformed policy = receipts:%d err:%v", usage.ReceiptsScanned, err)
	}
	injection, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil || injection.ReceiptsScanned != 0 {
		t.Fatalf("injection malformed policy = receipts:%d err:%v", injection.ReceiptsScanned, err)
	}
}

func TestConsumeReceiptsDeclaredSkillRaisesFutureSurfacing(t *testing.T) {
	home, s := newStore(t)
	const nodeID = "skill:capability-writeback"
	// Matches the task on purpose: this test measures that declaration RAISES an
	// already-relevant capability, which is what reputation is for. It is no
	// longer able to surface an irrelevant one from nothing.
	if err := s.Add(Node{ID: nodeID, Kind: "skill", Name: "Capability Writeback", Triggers: []string{"writeback"}}); err != nil {
		t.Fatal(err)
	}
	task := "capability writeback edge follow-up"
	before := surfaced(s, task)[nodeID]
	// The claim this test makes is in its name: declaration RAISES surfacing.
	// It used to assert the node scored ZERO beforehand, which quietly depended
	// on reputation being able to surface an irrelevant capability from nothing.
	// That is the behavior that put a video-generation skill on a "fix the flaky
	// swift test" list, so it was removed; the node must now be relevant first
	// and declaration lifts it from there. The strict comparison below is what
	// actually guards the feature, and it is unchanged.
	if before <= 0 {
		t.Fatalf("precondition: a relevant skill should surface before receipts, got score %.1f", before)
	}

	rec1 := testReceipt("rec_validate_one", "completed", true, []string{"capability://" + nodeID}, nil, nil)
	rec1.StateDelta.Summary = "capability writeback edge"
	writeReceipt(t, home, rec1)
	rec2 := testReceipt("rec_validate_two", "completed", true, []string{"capability://" + nodeID}, nil, nil)
	rec2.StateDelta.Summary = "capability writeback edge"
	writeReceipt(t, home, rec2)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedDeclaredUseEdges < 4 {
		t.Fatalf("report = %+v, want declaring receipt context edges", report)
	}
	after := surfaced(s, task)[nodeID]
	if after <= before {
		t.Fatalf("declared receipts should raise surfacing score for %s: before %.1f after %.1f", nodeID, before, after)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdDeclaredUsedBy, "context:writeback", nodeID) {
		t.Fatalf("edges = %+v, want declared_by context edge on real node", edges)
	}
}

// TestValidatedByContributionIsBoundedNotLifetimeEdgeCount is the E9 audit
// falsifier: every declared_by edge is a distinct, permanent row (edgeKey
// includes SourceReceipt, so repeat declarations never dedupe -- that is
// correct, each is a real historical event). But surface()'s declaredAdd
// accumulator sums edgeScore over EVERY such edge with no ceiling, so a node's
// surfacing score becomes its lifetime usage count rather than a bounded
// relevance signal (live evidence: one skill node carries 404 declared_by
// edges). This proves the accumulated contribution must be capped per node:
// a node declared exactly maxValidatedByAdd times (raw sum already at the
// ceiling) and a node declared 404 times in the identical context must score
// IDENTICALLY -- more historical declarations beyond the cap must not keep
// inflating the score.
func TestValidatedByContributionIsBoundedNotLifetimeEdgeCount(t *testing.T) {
	const nodeID = "skill:overvalidated"
	const task = "pricing followup task"
	const atCapCount = 5        // raw sum already at the intended ceiling, no clipping needed
	const wayOverCapCount = 404 // the live-evidence figure from the audit

	scoreAfterNValidations := func(n int) float64 {
		_, s := newStore(t)
		// The trigger MATCHES the task on purpose. This test measures that a
		// large validated_by history is capped, not that reputation alone can
		// surface an irrelevant capability -- reputation is now gated on some
		// relevance, because ungated it put a video-generation skill on a
		// "fix the flaky swift test" list at the same score as the right answer.
		if err := s.Add(Node{ID: nodeID, Kind: "skill", Name: "Overvalidated", Triggers: []string{"pricing"}, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			edge := Edge{
				Kind:          EdValidatedBy,
				From:          "context:pricing",
				To:            nodeID,
				Confidence:    1,
				Weight:        1,
				Provenance:    "cheap_lane",
				SourceReceipt: "review_hist_" + strconv.Itoa(i),
			}
			if err := s.AddEdge(edge, t0); err != nil {
				t.Fatal(err)
			}
		}
		hits, err := s.Surface(task)
		if err != nil {
			t.Fatal(err)
		}
		hit := findSurfaceHit(hits, nodeID)
		if hit == nil {
			t.Fatalf("node with %d validated_by edges did not surface at all: %+v", n, hits)
		}
		return hit.Score
	}

	atCap := scoreAfterNValidations(atCapCount)
	wayOverCap := scoreAfterNValidations(wayOverCapCount)

	if wayOverCap > atCap {
		t.Fatalf("validated_by contribution is unbounded by lifetime edge count: %d edges scored %.1f, %d edges scored %.1f (want capped, equal) -- surfacing score must not equal lifetime usage count (audit E9)", atCapCount, atCap, wayOverCapCount, wayOverCap)
	}
}

func TestConsumeReceiptsWithoutSkillIDsDoesNotCreateBlankFeedbackEdges(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:present", Kind: "skill", Name: "Present", Triggers: []string{"present"}}); err != nil {
		t.Fatal(err)
	}
	rec := testReceipt("rec_no_skills", "completed", true, nil, nil, nil)
	rec.StateDelta.Summary = "blank provenance should not create edges"
	rec.Skills.Used = []string{"", "   ", "capability://"}
	writeReceipt(t, home, rec)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedEdges != 0 || report.UsedCapabilitiesSeen != 0 || report.MissedCapabilitiesSeen != 0 || report.FalseTriggersSeen != 0 {
		t.Fatalf("blank skill ids must not create feedback edges: %+v", report)
	}
	edgePath := filepath.Join(home, "registries", "capability-edges.jsonl")
	if _, err := os.Stat(edgePath); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	for _, edge := range readCapEdges(t, home) {
		if strings.TrimSpace(edge.From) == "" || strings.TrimSpace(edge.To) == "" || strings.TrimSpace(edge.Provenance) == "" {
			t.Fatalf("blank-provenance edge created: %+v", edge)
		}
	}
}

func TestSurfaceIncludesCapabilityConstraints(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}, Weight: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "tool:revenuecat", Kind: "tool", Name: "RevenueCat", Triggers: []string{"subscription"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{ID: "skill:legacy-paywall", Kind: "skill", Name: "Legacy Paywall", Triggers: []string{"paywall"}}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range []Edge{
		{Kind: EdRequires, From: "skill:pricing", To: "tool:revenuecat", Confidence: 0.9},
		{Kind: EdConflictsWith, From: "skill:pricing", To: "skill:legacy-paywall", Confidence: 0.8},
		{Kind: EdSurfacesWith, From: "skill:pricing", To: "harness:codex", Confidence: 1},
		{Kind: EdMissedWhen, From: "context:pricing", To: "skill:pricing", Confidence: 0.4},
		{Kind: EdFalseTriggeredBy, From: "context:paywall", To: "skill:pricing", Confidence: 0.3},
		{Kind: EdValidatedBy, From: "skill:pricing", To: "rec_pricing_success", Confidence: 1},
	} {
		if err := s.AddEdge(edge, t0); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := s.SurfaceForHarness("pricing paywall", "codex")
	if err != nil {
		t.Fatal(err)
	}
	hit := findSurfaceHit(hits, "skill:pricing")
	if hit == nil {
		t.Fatalf("pricing skill not surfaced: %+v", hits)
	}
	if !hasConstraint(hit.Constraints.Required, EdRequires, "tool:revenuecat") {
		t.Fatalf("required constraints = %+v", hit.Constraints.Required)
	}
	if !hasConstraint(hit.Constraints.Conflicts, EdConflictsWith, "skill:legacy-paywall") {
		t.Fatalf("conflict constraints = %+v", hit.Constraints.Conflicts)
	}
	if !hasConstraint(hit.Constraints.Harnesses, EdSurfacesWith, "harness:codex") {
		t.Fatalf("harness constraints = %+v", hit.Constraints.Harnesses)
	}
	if !hasConstraint(hit.Constraints.KnownMissedWhen, EdMissedWhen, "context:pricing") {
		t.Fatalf("missed constraints = %+v", hit.Constraints.KnownMissedWhen)
	}
	if !hasConstraint(hit.Constraints.KnownFalseTriggeredBy, EdFalseTriggeredBy, "context:paywall") {
		t.Fatalf("false-trigger constraints = %+v", hit.Constraints.KnownFalseTriggeredBy)
	}
	if !hasConstraint(hit.Constraints.Evidence, EdDeclaredUsedBy, "rec_pricing_success") {
		t.Fatalf("evidence constraints = %+v", hit.Constraints.Evidence)
	}
	if hit.Constraints.CostRiskNote == "" {
		t.Fatalf("missing cost/risk note: %+v", hit.Constraints)
	}
}

func TestSurfaceUsesMatchedEvidenceOnlyEvaluationsToLiftOwners(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "procedure:skill-ops",
		Kind:     "procedure",
		Name:     "skill-ops",
		Triggers: []string{"adopting", "external", "github"},
		Weight:   1.25,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:           "eval:capability-evaluation-external-steipete-agent-scripts-validate-skills-doctor-20260701",
		Kind:         "eval",
		Name:         "Capability evaluation: portable-agent-script-tool-policy-reference",
		Triggers:     []string{"steipete", "scripts", "evaluate", "external"},
		Weight:       0.25,
		EvidenceOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "tool:hermes-tools.skill-view",
		Kind:     "tool",
		Name:     "hermes-tools.skill_view",
		Triggers: []string{"scripts"},
		Weight:   1.2,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range []Edge{
		{Kind: EdEvaluatedBy, From: "procedure:skill-ops", To: "eval:capability-evaluation-external-steipete-agent-scripts-validate-skills-doctor-20260701", Confidence: 0.89},
		{Kind: EdValidatedBy, From: "procedure:skill-ops", To: "eval:capability-evaluation-external-steipete-agent-scripts-validate-skills-doctor-20260701", Confidence: 0.89},
	} {
		if err := s.AddEdge(edge, t0); err != nil {
			t.Fatal(err)
		}
	}

	hits := mustSurface(t, s, "evaluate skills from steipete agent-scripts")
	if hasSurfaced(hits, "eval:capability-evaluation-external-steipete-agent-scripts-validate-skills-doctor-20260701") {
		t.Fatalf("evidence-only eval node must not surface as executable offer: %+v", hits)
	}
	if hits[0].Node.ID != "procedure:skill-ops" {
		t.Fatalf("matched evaluation owner should outrank generic script token, got first %+v from %+v", hits[0], hits)
	}
	hit := findSurfaceHit(hits, "procedure:skill-ops")
	if hit == nil {
		t.Fatalf("matched eval evidence should lift its owner, got %+v", hits)
	}
	if !strings.Contains(hit.Why, "+evidence=") {
		t.Fatalf("surface reason should explain evidence lift: %+v", hit)
	}
	if !stringSliceContains(hit.MatchedTokens, "evidence") {
		t.Fatalf("matched tokens should flag evidence lift: %+v", hit.MatchedTokens)
	}
	if !hasConstraint(hit.Constraints.Evidence, "matched_evidence", "eval:capability-evaluation-external-steipete-agent-scripts-validate-skills-doctor-20260701") {
		t.Fatalf("matched evidence ref missing from constraints: %+v", hit.Constraints.Evidence)
	}
}

func TestSurfaceUsesMeaningfulTriggerTokens(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "skill:noisy-meta",
		Kind:     "skill",
		Name:     "Noisy Meta",
		Triggers: []string{"skill", "for", "and"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "skill:go-testing",
		Kind:     "skill",
		Name:     "Go Testing",
		Triggers: []string{"go testing practices"},
	}); err != nil {
		t.Fatal(err)
	}

	hits := surfaced(s, "choose a skill for Go testing and home doctor hygiene")
	if _, ok := hits["skill:noisy-meta"]; ok {
		t.Fatalf("stopword/meta-word-only trigger overlap surfaced noisy node: %+v", hits["skill:noisy-meta"])
	}
	if score, ok := hits["skill:go-testing"]; !ok {
		t.Fatalf("meaningful phrase trigger did not surface: %+v", hits)
	} else if score != 2 {
		t.Fatalf("go/testing trigger score = %.1f, want 2.0", score)
	}
}

func TestSurfaceIgnoresGenericMCPPluginKindTokens(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "tool:noisy-mcp",
		Kind:     "tool",
		Name:     "Noisy MCP",
		Triggers: []string{"mcp plugin plugins"},
		Weight:   3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "tool:codegraph",
		Kind:     "tool",
		Name:     "Codegraph",
		Triggers: []string{"code graph dependency trace"},
		Weight:   1.5,
	}); err != nil {
		t.Fatal(err)
	}

	hits := surfaced(s, "evaluate skills and MCP plugins")
	if _, ok := hits["tool:noisy-mcp"]; ok {
		t.Fatalf("generic MCP/plugin kind-token overlap surfaced noisy node: %+v", hits["tool:noisy-mcp"])
	}
	hits = surfaced(s, "trace code graph dependencies")
	if score, ok := hits["tool:codegraph"]; !ok {
		t.Fatalf("concrete graph task did not surface codegraph: %+v", hits)
	} else if score <= 0 {
		t.Fatalf("codegraph score = %.1f, want positive", score)
	}
}

func TestSurfaceIgnoresGenericBuildToken(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "procedure:agent-harness-doctrine",
		Kind:     "procedure",
		Name:     "agent-harness-doctrine",
		Triggers: []string{"agent harness doctrine adapter"},
		Weight:   1.25,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "skill:churn-prevention",
		Kind:     "skill",
		Name:     "Churn Prevention",
		Triggers: []string{"build cancellation flows"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "skill:prototype",
		Kind:     "skill",
		Name:     "Prototype",
		Triggers: []string{"building throwaway app"},
	}); err != nil {
		t.Fatal(err)
	}

	hits := mustSurface(t, s, "build Agent Workbench harness adapter")
	if !hasSurfaced(hits, "procedure:agent-harness-doctrine") {
		t.Fatalf("harness adapter owner should surface: %+v", hits)
	}
	if hasSurfaced(hits, "skill:churn-prevention") || hasSurfaced(hits, "skill:prototype") {
		t.Fatalf("unrelated capabilities should not surface from generic build token: %+v", hits)
	}
}

func TestSurfaceTreatsGoAsLanguageInEngineeringContext(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "skill:launch",
		Kind:     "skill",
		Name:     "Launch",
		Triggers: []string{"launch product feature go market"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "skill:golang-patterns",
		Kind:     "skill",
		Name:     "Golang Patterns",
		Triggers: []string{"golang patterns writing reviewing refactoring go code"},
	}); err != nil {
		t.Fatal(err)
	}

	offers, err := s.Surface("implement a Go distiller request-grounded replay feature")
	if err != nil {
		t.Fatal(err)
	}
	goHit := findSurfaceHit(offers, "skill:golang-patterns")
	if goHit == nil {
		t.Fatalf("golang patterns did not surface: %+v", offers)
	}
	if got := strings.Join(goHit.MatchedTokens, ","); got != "go,golang" {
		t.Fatalf("golang matched tokens = %q, want go,golang", got)
	}
	if launchHit := findSurfaceHit(offers, "skill:launch"); launchHit != nil {
		t.Fatalf("launch should not surface from generic feature/go tokens in engineering context: %+v", launchHit)
	}
}

func TestSurfaceStillFindsLaunchForLaunchContext(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "skill:launch",
		Kind:     "skill",
		Name:     "Launch",
		Triggers: []string{"launch product feature announcement release strategy hunt go market"},
	}); err != nil {
		t.Fatal(err)
	}

	offers, err := s.Surface("launch product on product hunt")
	if err != nil {
		t.Fatal(err)
	}
	launchHit := findSurfaceHit(offers, "skill:launch")
	if launchHit == nil {
		t.Fatalf("launch did not surface for launch context: %+v", offers)
	}
	if got := strings.Join(launchHit.MatchedTokens, ","); got != "hunt,launch,product" {
		t.Fatalf("launch matched tokens = %q, want hunt,launch,product", got)
	}
}

func TestSurfaceReportCarriesMatchedTriggerTokens(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{
		ID:       "skill:go-testing",
		Kind:     "skill",
		Name:     "Go Testing",
		Triggers: []string{"go testing practices"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Node{
		ID:       "tool:go-doctor",
		Kind:     "tool",
		Name:     "Go Doctor",
		Triggers: []string{"go doctor"},
	}); err != nil {
		t.Fatal(err)
	}
	task := "choose skill Go testing home doctor hygiene"
	offers, err := s.Surface(task)
	if err != nil {
		t.Fatal(err)
	}
	hit := findSurfaceHit(offers, "skill:go-testing")
	if hit == nil {
		t.Fatalf("missing full surface hit: %+v", offers)
	}
	if got := strings.Join(hit.MatchedTokens, ","); got != "go,testing" {
		t.Fatalf("matched tokens = %q, want go,testing", got)
	}
	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	report := NewSurfaceReport(task, "codex", offers, stats, 1)
	if len(report.OfferSummaries) == 0 || strings.Join(report.OfferSummaries[0].MatchedTokens, ",") != "go,testing" {
		t.Fatalf("summary matched tokens not carried: %+v", report.OfferSummaries)
	}
	if got := matchedTokenCountString(report.MatchedTokenCounts); got != "go=2,doctor=1,testing=1" {
		t.Fatalf("matched token counts = %q, want go=2,doctor=1,testing=1", got)
	}
}

func TestConsumeReceiptsFalseTriggeredLowersSurfacing(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:noisy", Kind: "skill", Name: "Noisy", Triggers: []string{"receipt"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := surfaced(s, "receipt")["skill:noisy"]; !ok {
		t.Fatal("precondition: noisy skill should surface before explicit false-trigger receipt")
	}

	rec := testReceipt("rec_false", "completed", true, nil, nil, nil)
	rec.Skills.FalseTriggered = []string{"capability://skill:noisy"}
	writeReceipt(t, home, rec)

	report, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.FalseTriggersSeen != 1 || report.AppliedFalseEdges == 0 {
		t.Fatalf("report = %+v, want false-trigger feedback applied", report)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdFalseTriggeredBy, "context:receipt", "skill:noisy") {
		t.Fatalf("edges = %+v, want false_triggered_by edge from receipt summary", edges)
	}
	if _, ok := surfaced(s, "receipt")["skill:noisy"]; ok {
		t.Fatal("explicit false-trigger receipt should lower future surfacing for same context")
	}

	replay, err := s.ConsumeReceipts(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if replay.AppliedEdges != 0 {
		t.Fatalf("replay should be idempotent, got %+v", replay)
	}
}

func TestFalseTriggeredContextMatchesPluralTaskToken(t *testing.T) {
	_, s := newStore(t)
	const nodeID = "skill:design-noise"
	if err := s.Add(Node{ID: nodeID, Kind: "skill", Name: "Design Noise", Triggers: []string{"surface"}}); err != nil {
		t.Fatal(err)
	}
	task := "adapt anti-pattern instructions from operating-system into consumed Compound surface"
	if _, ok := surfaced(s, task)[nodeID]; !ok {
		t.Fatal("precondition: broad surface trigger should surface before false-trigger feedback")
	}
	if err := s.AddEdge(Edge{Kind: EdFalseTriggeredBy, From: "context:instruction", To: nodeID, Confidence: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := surfaced(s, task)[nodeID]; ok {
		t.Fatal("false-trigger feedback for instruction should suppress plural instructions task context")
	}
}

func TestFalseTriggeredContextUsesUnprunedLegacyAdaptationToken(t *testing.T) {
	_, s := newStore(t)
	const nodeID = "skill:context-noise"
	if err := s.Add(Node{ID: nodeID, Kind: "skill", Name: "Context Noise", Triggers: []string{"surface"}}); err != nil {
		t.Fatal(err)
	}
	task := "adapt anti-pattern context from operating-system into consumed Compound surface"
	if _, ok := surfaced(s, task)[nodeID]; !ok {
		t.Fatal("precondition: broad surface trigger should surface before false-trigger feedback")
	}
	if err := s.AddEdge(Edge{Kind: EdFalseTriggeredBy, From: "context:context", To: nodeID, Confidence: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := surfaced(s, task)[nodeID]; ok {
		t.Fatal("false-trigger feedback should still suppress pruned adaptation meta-token context")
	}
}

func TestConsumeReceiptsIfGraphExistsNoopsWithoutGraph(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, testReceipt("rec_no_graph", "completed", true,
		[]string{"capability://skill:missing"}, nil, nil))

	report, ok, err := ConsumeReceiptsIfGraphExists(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("expected no graph state, got ok with report %+v", report)
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("no-op must not create graph state, stat err=%v", err)
	}
}

func TestConsumeReceiptsIfGraphExistsConsumes(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:auto", Kind: "skill", Name: "Auto", Triggers: []string{"auto"}}); err != nil {
		t.Fatal(err)
	}
	writeReceipt(t, home, testReceipt("rec_auto", "completed", true,
		[]string{"capability://skill:auto"}, nil, nil))

	report, ok, err := ConsumeReceiptsIfGraphExists(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || report.AppliedDeclaredUseEdges != 3 {
		t.Fatalf("report = %+v ok=%v", report, ok)
	}
	if !hasEdge(readCapEdges(t, home), EdDeclaredUsedBy, "skill:auto", "rec_auto") {
		t.Fatal("expected automatic receipt consumption to validate graph node")
	}
}

func TestApplySkillManifestImportsSkillsAndEdges(t *testing.T) {
	home, s := newStore(t)
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: SkillSourceRef{
			Path:        "fixtures/skills/skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_fixture",
		},
		EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
		Skills: []SkillManifestEntry{
			{
				ID:                  "skill:pricing",
				Name:                "Pricing",
				Status:              "active",
				Triggers:            []string{"pricing", "paywall"},
				Capabilities:        []string{"monetization"},
				CompatibleHarnesses: []string{"codex", "claude_code"},
				Requires:            []string{"tool:analytics"},
				ValidatedBy:         []string{"eval:pricing-copy"},
				Weight:              2,
			},
			{
				ID:             "lens:paywall-risk",
				Kind:           "lens",
				Name:           "Paywall Risk Lens",
				Status:         "active",
				Triggers:       []string{"paywall"},
				Specializes:    []string{"skill:pricing"},
				ConflictsWith:  []string{"skill:old-paywall"},
				ReplacementFor: []string{"skill:old-paywall"},
			},
			{
				ID:       "skill:old-paywall",
				Name:     "Old Paywall",
				Status:   "retired",
				Triggers: []string{"paywall"},
			},
		},
	}
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 3 || len(report.EdgeIDs) != 7 {
		t.Fatalf("report = %+v", report)
	}
	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if byID["skill:pricing"].Kind != "skill" || byID["skill:pricing"].Weight != 2 {
		t.Fatalf("pricing node = %+v", byID["skill:pricing"])
	}
	if byID["lens:paywall-risk"].Kind != "lens" {
		t.Fatalf("lens node = %+v", byID["lens:paywall-risk"])
	}
	if !byID["skill:old-paywall"].Retired {
		t.Fatalf("old skill should be retired: %+v", byID["skill:old-paywall"])
	}
	hits := surfaced(s, "improve paywall monetization")
	if _, ok := hits["skill:pricing"]; !ok {
		t.Fatal("imported skill should surface for paywall monetization task")
	}
	if _, ok := hits["lens:paywall-risk"]; !ok {
		t.Fatal("imported lens should surface for paywall task")
	}
	if _, ok := hits["skill:old-paywall"]; ok {
		t.Fatal("retired imported skill must not surface")
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdRequires, "skill:pricing", "tool:analytics") ||
		!hasEdge(edges, EdValidatedBy, "skill:pricing", "eval:pricing-copy") ||
		!hasEdge(edges, EdSurfacesWith, "skill:pricing", "harness:codex") ||
		!hasEdge(edges, EdSurfacesWith, "skill:pricing", "harness:claude_code") ||
		!hasEdge(edges, EdSpecializes, "lens:paywall-risk", "skill:pricing") ||
		!hasEdge(edges, EdConflictsWith, "lens:paywall-risk", "skill:old-paywall") ||
		!hasEdge(edges, EdRetiredBy, "skill:old-paywall", "lens:paywall-risk") {
		t.Fatalf("edges = %+v", edges)
	}
}

func TestValidateSkillManifestRejectsUnknownKind(t *testing.T) {
	manifest := validSkillManifestForTest()
	manifest.Skills[0].Kind = "plugin"
	report := ValidateSkillManifest(manifest)
	if report.Status != "fail" {
		t.Fatalf("status = %s, want fail", report.Status)
	}
}

func TestLoadSkillManifestRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(`
schema_version: compound.skill_manifest.v0.1
id: skill_manifest_fixture
adapter_id: fixture.skill-manifest
source_system: fixture-os
observed_at: "2026-06-29T00:00:00Z"
source_ref:
  path: fixtures/skills/skill-manifest.yaml
  kind: generated_view
  owner: capability-fixture
  source_class: generated
  revision: rev_fixture
evidence_refs:
  - artifact_sha256:skill-fixture
skills:
  - id: skill:pricing
    name: Pricing
    status: active
    triggers:
      - pricing
    hidden_payload: should-not-pass
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSkillManifest(path); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func TestLoadOSSkillManifestAdaptsGeneratedManifest(t *testing.T) {
	skillRoot := t.TempDir()
	for _, name := range []string{
		"ai-seo",
		"app-store-screenshots",
		"pricing",
		"skill-creator-a",
		"skill-creator-b",
	} {
		writeSkillSource(t, skillRoot, name, `---
name: synthetic-fixture
description: "Synthetic skill source fixture"
---
# Synthetic fixture
`)
	}
	path := filepath.Join(t.TempDir(), "skill-manifest.json")
	writeFile(t, path, strings.ReplaceAll(`{
  "_generated": "test",
  "counts": {"skills": 4, "recipes": 1},
  "duplicate_names": [{"name": "skill-creator", "count": 2}],
  "entries": [
    {
      "name": "ai-seo",
      "kind": "skill",
      "trigger": "Optimize content for AI search engines and SEO.",
      "path": "/private/skills/ai-seo",
      "home": "agents"
    },
    {
      "name": "app-store-screenshots",
      "kind": "skill",
      "trigger": "Use when building App Store or Google Play screenshot pages, generating exportable marketing screenshots.",
      "path": "/private/skills/app-store-screenshots",
      "home": "agents"
    },
    {
      "name": "pricing",
      "kind": "skill",
      "trigger": "When the user wants help with pricing decisions, packaging, or monetization strategy.",
      "path": "/private/skills/pricing",
      "home": "codex"
    },
    {
      "name": "skill-creator",
      "kind": "skill",
      "trigger": "Create new skills, modify existing skills, and measure skill performance.",
      "path": "/private/skills/skill-creator-a",
      "home": "agents"
    },
    {
      "name": "skill-creator",
      "kind": "skill",
      "trigger": "Create new skills for Codex.",
      "path": "/private/skills/skill-creator-b",
      "home": "codex"
    },
    {
      "name": "audit",
      "kind": "recipe",
      "trigger": "TRIGGER: audit code, system, or strategy.",
      "path": "/private/brain/recipes/audit.md",
      "home": "brain-recipes"
    }
  ]
}`, "/private/skills", filepath.ToSlash(skillRoot)))
	manifest, err := LoadOSSkillManifest(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SkillManifestSchemaVersion || manifest.AdapterID != "os.skill-manifest-json" || len(manifest.EvidenceRefs) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Skills) != 6 {
		t.Fatalf("skills = %d", len(manifest.Skills))
	}
	byID := map[string]SkillManifestEntry{}
	for _, skill := range manifest.Skills {
		byID[skill.ID] = skill
	}
	if byID["skill:ai-seo"].Kind != "skill" ||
		byID["skill:app-store-screenshots"].Kind != "skill" ||
		byID["procedure:audit"].Kind != "procedure" ||
		byID["skill:agents:skill-creator"].ID == "" ||
		byID["skill:codex:skill-creator"].ID == "" {
		t.Fatalf("ids = %+v", byID)
	}
	if len(byID["skill:pricing"].CompatibleHarnesses) != 1 || byID["skill:pricing"].CompatibleHarnesses[0] != "codex" {
		t.Fatalf("pricing harnesses = %+v", byID["skill:pricing"].CompatibleHarnesses)
	}
	home, store := newStore(t)
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 6 {
		t.Fatalf("report = %+v", report)
	}
	if !hasSurfaced(mustSurface(t, store, "app store screenshots"), "skill:app-store-screenshots") {
		t.Fatal("app store screenshots should surface imported OS skill")
	}
}

func TestApplySkillManifestDoesNotSurfaceNeedsReviewSkill(t *testing.T) {
	home, s := newStore(t)
	manifest := validSkillManifestForTest()
	manifest.Skills[0].Status = "needs_review"
	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	if hits := surfaced(s, "pricing"); hits["skill:pricing"] != 0 {
		t.Fatalf("needs_review skill surfaced: %+v", hits)
	}
}

func TestValidateSkillManifestRejectsUnknownStatus(t *testing.T) {
	manifest := validSkillManifestForTest()
	manifest.Skills[0].Status = "ambient"
	report := ValidateSkillManifest(manifest)
	if report.Status != "fail" {
		t.Fatalf("status = %s, want fail", report.Status)
	}
}

func TestValidateSkillManifestRejectsBadObservedAt(t *testing.T) {
	manifest := validSkillManifestForTest()
	manifest.ObservedAt = "today"
	report := ValidateSkillManifest(manifest)
	if report.Status != "fail" {
		t.Fatalf("status = %s, want fail", report.Status)
	}
}

func validSkillManifestForTest() SkillManifest {
	return SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: SkillSourceRef{
			Path:        "fixtures/skills/skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_fixture",
		},
		EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
		Skills: []SkillManifestEntry{{
			ID:       "skill:pricing",
			Name:     "Pricing",
			Status:   "active",
			Triggers: []string{"pricing"},
		}},
	}
}

func readCapEdges(t *testing.T, home string) []Edge {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "registries", "capability-edges.jsonl"))
	if os.IsNotExist(err) {
		return nil // no edges written yet — the file is the append-only edge log
	}
	if err != nil {
		t.Fatal(err)
	}
	var edges []Edge
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var edge Edge
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			t.Fatalf("parse edge %q: %v", line, err)
		}
		edges = append(edges, edge)
	}
	return edges
}

func testReceipt(id, outcome string, changed bool, used, missed, candidate []string) envelope.Receipt {
	proofRefs := []string{}
	if outcome == "completed" && !changed {
		proofRefs = []string{"artifact:unchanged-review"}
	}
	return envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     t0.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_" + id},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:  outcome,
		MoveType: "outward_delta",
		StateDelta: envelope.StateDelta{
			Changed:      changed,
			Summary:      "receipt test",
			FilesChanged: []string{},
			Artifacts:    []string{},
			Verification: []string{},
		},
		Cost:      envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:    envelope.ReceiptSkills{Used: used, Missed: missed, Candidate: candidate, ApplicationMethod: "explicit_declaration"},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: proofRefs,
	}
}

func writeReceipt(t *testing.T, home string, rec envelope.Receipt) {
	t.Helper()
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "events", rec.ID+".json"), string(b))
}

func hasEdge(edges []Edge, kind, from, to string) bool {
	return findEdge(edges, kind, from, to) != nil
}

func findEdge(edges []Edge, kind, from, to string) *Edge {
	for _, edge := range edges {
		if edge.Kind == kind && edge.From == from && edge.To == to {
			return &edge
		}
	}
	return nil
}

func surfaced(s *Store, task string) map[string]float64 {
	hits, _ := s.Surface(task)
	m := map[string]float64{}
	for _, h := range hits {
		m[h.Node.ID] = h.Score
	}
	return m
}

func findSurfaceHit(hits []Surfaced, id string) *Surfaced {
	for i := range hits {
		if hits[i].Node.ID == id {
			return &hits[i]
		}
	}
	return nil
}

func matchedTokenCountString(counts []MatchedTokenCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.Token+"="+strconv.Itoa(count.Count))
	}
	return strings.Join(parts, ",")
}

func hasConstraint(values []SurfacingConstraint, kind, ref string) bool {
	for _, value := range values {
		if value.Kind == kind && value.Ref == ref {
			return true
		}
	}
	return false
}

func hasSurfaced(hits []Surfaced, id string) bool {
	for _, hit := range hits {
		if hit.Node.ID == id {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mustSurface(t *testing.T, store *Store, task string) []Surfaced {
	t.Helper()
	hits, err := store.Surface(task)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSurfaceByTrigger(t *testing.T) {
	_, s := newStore(t)
	s.Add(Node{ID: "skill:pricing", Kind: "skill", Name: "pricing", Triggers: []string{"pricing", "paywall"}})
	s.Add(Node{ID: "skill:onboarding", Kind: "skill", Name: "onboarding", Triggers: []string{"signup"}})
	got := surfaced(s, "improve the pricing page")
	if _, ok := got["skill:pricing"]; !ok {
		t.Error("a node whose trigger matches the task should surface")
	}
	if _, ok := got["skill:onboarding"]; ok {
		t.Error("a node with no matching trigger should NOT surface")
	}
}

func TestSurfaceIgnoresGenericAgentRoleToken(t *testing.T) {
	_, s := newStore(t)
	mustAdd := func(n Node) {
		t.Helper()
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(Node{
		ID:       "procedure:agent-harness-doctrine",
		Kind:     "procedure",
		Name:     "agent-harness-doctrine",
		Triggers: []string{"agent harness doctrine", "harness adapter"},
		Weight:   1.25,
	})
	mustAdd(Node{
		ID:       "tool:episode-agent-logs",
		Kind:     "tool",
		Name:     "episode-agent-logs",
		Triggers: []string{"agent logs"},
		Weight:   1.2,
	})

	hits := surfaced(s, "evaluate Agent Workbench harness adapter skills")
	if _, ok := hits["procedure:agent-harness-doctrine"]; !ok {
		t.Fatalf("specific harness capability should surface, got %+v", hits)
	}
	if _, ok := hits["tool:episode-agent-logs"]; ok {
		t.Fatalf("unrelated tool should not surface from generic agent token alone: %+v", hits)
	}
}

func TestSurfaceIgnoresControlPlaneAdaptationMetaTokens(t *testing.T) {
	_, s := newStore(t)
	mustAdd := func(n Node) {
		t.Helper()
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(Node{
		ID:       "procedure:adaptation-proof",
		Kind:     "procedure",
		Name:     "Adaptation proof",
		Triggers: []string{"adapt"},
		Weight:   1,
	})
	mustAdd(Node{ID: "tool:system-design", Kind: "tool", Name: "system design", Triggers: []string{"system"}})
	mustAdd(Node{ID: "tool:instruction-loader", Kind: "tool", Name: "instruction loader", Triggers: []string{"instructions"}})
	mustAdd(Node{ID: "tool:prompt-image", Kind: "tool", Name: "prompt image", Triggers: []string{"prompts"}})
	mustAdd(Node{ID: "tool:everything-cache", Kind: "tool", Name: "everything cache", Triggers: []string{"everything"}})
	mustAdd(Node{ID: "tool:operating-notes", Kind: "tool", Name: "operating notes", Triggers: []string{"operating"}})
	mustAdd(Node{ID: "tool:anti-helper", Kind: "tool", Name: "anti helper", Triggers: []string{"anti"}})
	mustAdd(Node{ID: "tool:pattern-helper", Kind: "tool", Name: "pattern helper", Triggers: []string{"pattern"}})

	hits := surfaced(s, "adapt anti-pattern everything in operating-system into Compound without copying instructions skills or old prompts")
	if _, ok := hits["procedure:adaptation-proof"]; !ok {
		t.Fatalf("specific adaptation capability should still surface, got %+v", hits)
	}
	for _, id := range []string{
		"tool:system-design",
		"tool:instruction-loader",
		"tool:prompt-image",
		"tool:everything-cache",
		"tool:operating-notes",
		"tool:anti-helper",
		"tool:pattern-helper",
	} {
		if _, ok := hits[id]; ok {
			t.Fatalf("generic adaptation meta token %s should not surface capability: %+v", id, hits)
		}
	}
}

func TestSurfacePrunesLegacyAdaptationFrameTokens(t *testing.T) {
	_, s := newStore(t)
	mustAdd := func(n Node) {
		t.Helper()
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(Node{ID: "procedure:legacy-adaptation", Kind: "procedure", Name: "legacy adaptation", Triggers: []string{"adaptation"}})
	mustAdd(Node{ID: "tool:context-cache", Kind: "tool", Name: "context cache", Triggers: []string{"context"}, Weight: 2})
	mustAdd(Node{ID: "tool:runtime-doctor", Kind: "tool", Name: "runtime doctor", Triggers: []string{"runtime"}, Weight: 2})
	mustAdd(Node{ID: "tool:proof-helper", Kind: "tool", Name: "proof helper", Triggers: []string{"proof"}, Weight: 2})
	mustAdd(Node{ID: "skill:material-design", Kind: "skill", Name: "material design", Triggers: []string{"material"}, Weight: 2})

	hits := surfaced(s, "continue goal prompt context-aware adaptation of operating-system material into consumed runtime proof")
	if _, ok := hits["procedure:legacy-adaptation"]; !ok {
		t.Fatalf("legacy adaptation task should keep specific adaptation token, got %+v", hits)
	}
	for _, id := range []string{
		"tool:context-cache",
		"tool:runtime-doctor",
		"tool:proof-helper",
		"skill:material-design",
	} {
		if _, ok := hits[id]; ok {
			t.Fatalf("legacy adaptation frame token %s should not surface capability: %+v", id, hits)
		}
	}

	ordinary := surfaced(s, "debug runtime proof for context cache")
	for _, id := range []string{"tool:context-cache", "tool:runtime-doctor", "tool:proof-helper"} {
		if _, ok := ordinary[id]; !ok {
			t.Fatalf("ordinary non-legacy task should still surface %s, got %+v", id, ordinary)
		}
	}
}

func TestSurfaceForHarnessFiltersExplicitHarnessEdges(t *testing.T) {
	_, s := newStore(t)
	mustAdd := func(n Node) {
		t.Helper()
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	mustEdge := func(e Edge) {
		t.Helper()
		if err := s.AddEdge(e, t0); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(Node{ID: "skill:generic", Kind: "skill", Name: "generic", Triggers: []string{"paywall"}})
	mustAdd(Node{ID: "skill:codex", Kind: "skill", Name: "codex", Triggers: []string{"paywall"}})
	mustAdd(Node{ID: "skill:claude", Kind: "skill", Name: "claude", Triggers: []string{"paywall"}})
	mustEdge(Edge{Kind: EdSurfacesWith, From: "skill:codex", To: "harness:codex"})
	mustEdge(Edge{Kind: EdSurfacesWith, From: "skill:claude", To: "harness:claude_code"})

	genericHits, err := s.Surface("fix paywall")
	if err != nil {
		t.Fatal(err)
	}
	if !hasSurfaced(genericHits, "skill:codex") || !hasSurfaced(genericHits, "skill:claude") || !hasSurfaced(genericHits, "skill:generic") {
		t.Fatalf("generic surface should include all task-relevant nodes, got %+v", genericHits)
	}
	codexHits, err := s.SurfaceForHarness("fix paywall", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !hasSurfaced(codexHits, "skill:codex") || !hasSurfaced(codexHits, "skill:generic") {
		t.Fatalf("codex should get codex-specific plus generic hits, got %+v", codexHits)
	}
	if hasSurfaced(codexHits, "skill:claude") {
		t.Fatalf("codex must not get claude-only capability, got %+v", codexHits)
	}
	claudeHits, err := s.SurfaceForHarness("fix paywall", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !hasSurfaced(claudeHits, "skill:claude") || !hasSurfaced(claudeHits, "skill:generic") {
		t.Fatalf("claude should get claude-specific plus generic hits, got %+v", claudeHits)
	}
	if hasSurfaced(claudeHits, "skill:codex") {
		t.Fatalf("claude must not get codex-only capability, got %+v", claudeHits)
	}
}

// The headline P8 proof: a missed_when observation CHANGES future surfacing probability — the
// graph learns which capability fits which context.
func TestMissedWhenChangesSurfacing(t *testing.T) {
	_, s := newStore(t)
	s.Add(Node{ID: "skill:cro", Kind: "skill", Name: "cro", Triggers: []string{"pricing"}}) // not "paywall"
	// before: "paywall" task does NOT surface the node (its only trigger is "pricing").
	if _, ok := surfaced(s, "fix the paywall")["skill:cro"]; ok {
		t.Fatal("precondition: node should not surface for 'paywall' before the observation")
	}
	// the distiller observed it was relevant-but-missed for the 'paywall' context.
	if _, err := s.Observe("skill:cro", EdMissedWhen, "paywall", 0.9, t0); err != nil {
		t.Fatal(err)
	}
	// after: the same task now surfaces it — surfacing probability changed.
	if _, ok := surfaced(s, "fix the paywall")["skill:cro"]; !ok {
		t.Error("after a missed_when observation, the node must surface for that context (the graph learned)")
	}
}

// A false_triggered_by observation lowers surfacing for that context.
func TestFalseTriggeredDownweights(t *testing.T) {
	_, s := newStore(t)
	s.Add(Node{ID: "skill:x", Kind: "skill", Name: "x", Triggers: []string{"deploy"}})
	if _, ok := surfaced(s, "deploy the app")["skill:x"]; !ok {
		t.Fatal("precondition: node surfaces for 'deploy'")
	}
	s.Observe("skill:x", EdFalseTriggeredBy, "deploy", 2.0, t0) // strong false signal
	if _, ok := surfaced(s, "deploy the app")["skill:x"]; ok {
		t.Error("a strong false_triggered_by should pull the node below the surfacing threshold")
	}
}

func TestRetiredNeverSurfaces(t *testing.T) {
	_, s := newStore(t)
	s.Add(Node{ID: "skill:old", Kind: "skill", Name: "old", Triggers: []string{"legacy"}})
	s.Add(Node{ID: "skill:old", Kind: "skill", Name: "old", Triggers: []string{"legacy"}, Retired: true}) // upsert: retire
	if _, ok := surfaced(s, "legacy thing")["skill:old"]; ok {
		t.Error("a retired node must never surface")
	}
}

// Surface OFFERS, it does not execute: it returns nodes and produces no side effects (no new
// edges/files), so a match can never auto-run a chain.
func TestSurfaceNeverExecutes(t *testing.T) {
	home, s := newStore(t)
	s.Add(Node{ID: "tool:rm", Kind: "tool", Name: "rm", Triggers: []string{"delete"}})
	before, _ := os.ReadDir(filepath.Join(home, "registries"))
	hits, err := s.Surface("delete the cache")
	if err != nil || len(hits) != 1 {
		t.Fatalf("surface should offer the tool, got %d (%v)", len(hits), err)
	}
	after, _ := os.ReadDir(filepath.Join(home, "registries"))
	if len(after) != len(before) {
		t.Error("Surface must have NO side effects (it offers, never executes)")
	}
}

// The distiller→graph bridge: an operationalized CapabilityObservation in the inbox is consumed
// into a feedback edge that changes surfacing.
func TestConsumeFromDistillerInbox(t *testing.T) {
	home, s := newStore(t)
	s.Add(Node{ID: "skill:cro", Kind: "skill", Name: "cro", Triggers: []string{"pricing"}})
	// write what the distiller operationalizes: subject=node, statement=context, detail=signal.
	dir := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs))
	os.MkdirAll(dir, 0o755)
	obs := map[string]any{"id": "cand_1", "kind": "capability_observation", "subject": "skill:cro", "statement": "paywall page", "detail": EdMissedWhen, "confidence": 0.9, "verdict": "operationalize"}
	b, _ := json.Marshal(obs)
	os.WriteFile(filepath.Join(dir, "cand_1.json"), b, 0o644)
	// a PARKED observation in the same inbox must NOT be applied (Consume respects adjudication).
	parked := map[string]any{"id": "cand_2", "kind": "capability_observation", "subject": "skill:other", "statement": "signup", "detail": EdMissedWhen, "confidence": 0.3, "verdict": "park"}
	pb, _ := json.Marshal(parked)
	os.WriteFile(filepath.Join(dir, "cand_2.json"), pb, 0o644)

	n, err := s.Consume(home, t0)
	if err != nil || n != 1 {
		t.Fatalf("consume should apply 1 (operationalized) observation, got %d (%v)", n, err)
	}
	// multi-word context "paywall page" must surface on a natural-language task (tokenized).
	if _, ok := surfaced(s, "improve the paywall page conversion")["skill:cro"]; !ok {
		t.Error("after consuming the observation, the node must surface for that (tokenized) context")
	}
	// re-consume must be IDEMPOTENT — surfacing score unchanged.
	score1 := surfaced(s, "fix the paywall")["skill:cro"]
	s.Consume(home, t0)
	if score2 := surfaced(s, "fix the paywall")["skill:cro"]; score2 != score1 {
		t.Errorf("re-consume must be idempotent: score %v -> %v", score1, score2)
	}
}

// TestInstructionRuleImportRefusesUnoperationalRule pins the principle quality bar
// (docs/architecture/universal-principles.md — "Writing principles that route behavior"):
// an instruction rule that does not declare when it fires and at least one boundary
// naming what it prevents is refused at the import door, never applied.
func TestInstructionRuleImportRefusesUnoperationalRule(t *testing.T) {
	home := t.TempDir()
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "instr_quality_bar_test",
		AdapterID:     "fixture.test",
		SourceSystem:  "test",
		ObservedAt:    "2026-07-10T00:00:00Z",
		SourceRef: SkillSourceRef{
			Path: "fixtures/test.yaml", Kind: "authority_fixture",
			Owner: "capability-graph", SourceClass: "source", Revision: "v1",
		},
		EvidenceRefs: []string{"test"},
		Skills: []SkillManifestEntry{{
			ID: "instruction_rule:unoperational", Kind: "instruction_rule",
			Name: "Be Careful", Status: "active", Triggers: []string{"careful"},
			// no when, no boundary, no read, no route, no tier — moral vocabulary without an operational test
		}},
	}
	if _, err := ApplySkillManifest(home, manifest, time.Now()); err == nil {
		t.Fatalf("unoperational instruction rule must be refused at import")
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := store.Surface("careful"); err != nil {
		t.Fatal(err)
	} else if surfacedByID(hits, "instruction_rule:unoperational") != nil {
		t.Fatalf("refused rule must not be applied/surfaced")
	}

	manifest.Skills[0] = SkillManifestEntry{
		ID: "instruction_rule:operational", Kind: "instruction_rule",
		Name: "Act Inside the Envelope", Status: "active", Triggers: []string{"permission"},
		When:     []InstructionRuleWhen{{Kind: "phase", Values: []string{"execute"}}},
		Read:     []string{"docs/architecture/universal-principles.md"},
		Route:    []InstructionRoute{{Output: "decision", Destination: "compound://policy"}},
		Boundary: []string{"reversible action inside an admitted envelope is done, not asked about"},
		Tier:     1, Weight: 2,
	}
	if _, err := ApplySkillManifest(home, manifest, time.Now()); err != nil {
		t.Fatalf("operational rule must import: %v", err)
	}
}
