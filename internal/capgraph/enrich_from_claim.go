// enrich_from_claim.go — loop piece 2/4 bridge for work_57f80593eb2d.
//
// Goal: a memory Claim that was just promoted via `compound memory adjudicate`
// automatically writes / strengthens a matching capability-graph node's
// validated_by edge so the next orient offer scoring picks up the new
// evidence (and the surgeon-style audit trail stays complete via
// SourceReceipt=claim.ID).
//
// Layering: capgraph stays a derived artifact and does NOT import
// internal/mem (the ledger is the lower layer). The seam takes primitives
// (strings) so any caller that already knows the Claim's identity can
// drive it without an upward dependency. The CLI verb
// `compound memory adjudicate` and any governed adjudication hook call
// EnrichFromPromotedClaim once the lock-held path emits the Claim.
//
// Governance: writes go through ApplyNodesAndEdgesGoverned so every
// enrichment writes a paired ActionIntent + PolicyDecision in the
// governance log — same fail-closed policy gate as every other graph
// mutation. A policy deny prevents phantom evidence; the caller is
// responsible for its own retry semantics.
package capgraph

import (
	"fmt"
	"strings"
	"time"
)

// ProvenanceAdjudicateMemEnrich is the canonical provenance label written
// on enrichment edges. It names the source event ("memory adjudicate")
// and the loop piece ("compound/2") so an audit reader can grep for
// "what kind of write was this" without re-deriving from the action intent.
const ProvenanceAdjudicateMemEnrich = "compound/memory.adjudicate"

// EnrichFromPromotedClaim wires a freshly promoted memory Claim onto the
// capability-graph node it names by writing one validated_by edge gated
// through ApplyNodesAndEdgesGoverned.
//
// Resolution rules (first non-empty hit wins):
//  1. capabilityHint — a node id slug, a node name, or a "capability://..."
//     prefixed URI. Resolved via resolveCapabilityNode.
//  2. contextToken — a fallback surfaced when the hint already resolved
//     the node but the caller wants a different From token, OR when the
//     hint was empty.
//
// If neither input resolves to a live, non-retired node, the call returns
// (zero Edge, false, nil) so the caller can proceed: a Claim about a topic
// with no matching capability must NOT mint a phantom node or edge.
//
// Inputs:
//   - homePath: COMPOUND_HOME (or a stage home) where the capgraph lives.
//   - claimID: the promoted Claim's identity; written to edge.SourceReceipt
//     so the edge is traceable from claim back to graph and graph back to claim.
//   - provenance: caller-controlled provenance label, defaults to
//     ProvenanceAdjudicateMemEnrich when blank.
//   - capabilityHint: optional node id / name / capability:// URI.
//   - contextToken: short token used in edge.From = "context:<token>".
//     Must be non-empty when capabilityHint is empty; otherwise the seam
//     uses the cleaned capabilityHint as a slug fallback.
//
// Returns (edge, wrote, err). A clean no-match returns (zero, false, nil).
func EnrichFromPromotedClaim(homePath, claimID, provenance, capabilityHint, contextToken string, now time.Time) (Edge, bool, error) {
	if strings.TrimSpace(homePath) == "" {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: home path required")
	}
	if strings.TrimSpace(claimID) == "" {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: claimID required for evidence chain")
	}
	capabilityHint = strings.TrimSpace(capabilityHint)
	contextToken = strings.TrimSpace(contextToken)
	if capabilityHint == "" && contextToken == "" {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: at least one of capabilityHint / contextToken required")
	}
	if strings.TrimSpace(provenance) == "" {
		provenance = ProvenanceAdjudicateMemEnrich
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	store, err := Open(homePath)
	if err != nil {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: open store: %w", err)
	}
	nodes, err := store.loadNodes()
	if err != nil {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: load nodes: %w", err)
	}
	resolved := resolveCapabilityNode(nodes, capabilityHint)
	if resolved == "" {
		resolved = resolveCapabilityNode(nodes, contextToken)
	}
	if resolved == "" {
		return Edge{}, false, nil
	}

	// Prefer the context token for From — it is the dim the surface()
	// scorer indexes on (context:<token>). The token is NORMALIZED to the
	// same alphanumeric-only, lowercase, dash-stripped form Surface uses
	// for task tokenization (see capgraph.tokenList / rawContextTokenSet)
	// so a future orient round that tokenizes a task with the same words
	// does index this edge — otherwise the edge would be persisted but
	// invisible to the scorer, the worst of both worlds.
	fromToken := normalizeContextToken(contextToken)
	if fromToken == "" {
		fromToken = normalizeContextToken(nodeSlugForContext(resolved))
	}
	if strings.TrimSpace(fromToken) == "" {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: resolved node %s has no usable slug for context token", resolved)
	}

	edge := Edge{
		Kind:          EdValidatedBy,
		From:          "context:" + fromToken,
		To:            resolved,
		Confidence:    1.0,
		Provenance:    provenance,
		SourceReceipt: claimID,
		CreatedAt:     now.Format(time.RFC3339),
	}
	// Defensive: Validate edge before governance dispatch so a malformed
	// shape never reaches the policy gate (fail-closed, fail-loud).
	if err := validateEnrichmentEdge(edge); err != nil {
		return Edge{}, false, err
	}
	if err := ApplyNodesAndEdgesGoverned(homePath, nil, []Edge{edge}, now); err != nil {
		return Edge{}, false, fmt.Errorf("capgraph enrichment: governed apply: %w", err)
	}
	return edge, true, nil
}

// normalizeContextToken collapses the caller-supplied (or fall-back slug)
// context token to the same alphanumeric-only, lowercase form Surface's
// task-token tokenizer (tokenList / rawContextTokenSet) emits, so a
// future orient round that tokenizes a task with the same words indexes
// this edge via contextTokenMatches. Non-alphanumeric characters are
// stripped (no joiner): "execute-ceremony" and "execute ceremony" both
// become "executeceremony", matching the single token Surface sees in
// "execute-ceremony" trigger tokens and "execute-ceremony follow-up"
// task tokens alike.
func normalizeContextToken(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	if in == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// nodeSlugForContext derives a stable, lowercase slug from the node id
// (the part after the kind prefix like "procedure:" or "skill:") so the
// context token is human-meaningful when the caller did not supply one.
// Empty result means the id had no slug to offer.
func nodeSlugForContext(nodeID string) string {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ""
	}
	if idx := strings.LastIndex(nodeID, ":"); idx >= 0 && idx+1 < len(nodeID) {
		nodeID = nodeID[idx+1:]
	}
	nodeID = strings.ToLower(nodeID)
	if nodeID == "" {
		return ""
	}
	return nodeID
}

// validateEnrichmentEdge keeps the seam's contract local; it asserts the
// minimum invariants ApplyNodesAndEdgesGoverned also checks, but earlier
// in the call so the failure reason is the seam's, not the governance's.
func validateEnrichmentEdge(edge Edge) error {
	if edge.Kind != EdValidatedBy {
		return fmt.Errorf("edge kind must be %q, got %q", EdValidatedBy, edge.Kind)
	}
	if !strings.HasPrefix(edge.From, "context:") || len(edge.From) <= len("context:") {
		return fmt.Errorf("edge From must be context:<token>, got %q", edge.From)
	}
	if strings.TrimSpace(edge.To) == "" {
		return fmt.Errorf("edge To must be a resolved node id, got %q", edge.To)
	}
	if edge.SourceReceipt == "" {
		return fmt.Errorf("edge SourceReceipt required (claim id)")
	}
	return nil
}
