package capgraph

import (
	"sort"
	"strings"
)

// FeedbackKindSummary is the deterministic, private-body-free read model of
// receipt feedback grouped by node kind (the orient operator's per-kind
// projection consumes it). Counts are distinct (node, source-receipt) pairs:
// one receipt declaring use of a node counts once regardless of context-token
// edges the feedback fanned out into.
type FeedbackKindSummary struct {
	NodeCount        int      `json:"node_count"`
	ValidatedBy      int      `json:"validated_by"`
	DeclaredUsedBy   int      `json:"declared_used_by"`
	MissedWhen       int      `json:"missed_when"`
	FalseTriggeredBy int      `json:"false_triggered_by"`
	ReceiptIDs       []string `json:"receipt_ids"`
	// RawText is deliberately never populated: this summary's field set is the
	// allowlist — receipt summary text, evidence bodies, and prompt content
	// are private and must never ride this projection.
	RawText string `json:"raw_text,omitempty"`
}

// FeedbackCountsByKind folds the receipt-sourced evolution-feedback edges
// (declared_used_by / validated_by / missed_when / false_triggered_by carrying a SourceReceipt)
// into a per-node-kind summary. Edges without receipt provenance are not
// counted: this read model answers "which receipts fed back on which kind".
// It is read-only and never materializes state in the home.
func FeedbackCountsByKind(homePath string) map[string]FeedbackKindSummary {
	out := map[string]FeedbackKindSummary{}
	s, err := OpenReadOnly(homePath)
	if err != nil {
		return out
	}
	nodes, err := s.loadNodes()
	if err != nil {
		return out
	}
	kindOf := make(map[string]string, len(nodes))
	for _, n := range nodes {
		kindOf[n.ID] = n.Kind
		sum := out[n.Kind]
		sum.NodeCount++
		out[n.Kind] = sum
	}
	edges, err := s.loadEdges()
	if err != nil {
		return out
	}
	type nodeReceipt struct{ node, receipt string }
	counted := map[string]map[nodeReceipt]bool{}
	receiptsByKind := map[string]map[string]bool{}
	for _, e := range edges {
		if e.Kind != EdValidatedBy && e.Kind != EdDeclaredUsedBy && e.Kind != EdMissedWhen && e.Kind != EdFalseTriggeredBy {
			continue
		}
		receipt := strings.TrimSpace(e.SourceReceipt)
		if receipt == "" {
			continue
		}
		// Exactly one end of a receipt-feedback edge is a graph node: the
		// declared-use form is node → receipt, the context feedback forms are
		// context:token → node.
		nodeID, kind := "", ""
		if k, ok := kindOf[e.From]; ok {
			nodeID, kind = e.From, k
		} else if k, ok := kindOf[e.To]; ok {
			nodeID, kind = e.To, k
		} else {
			continue
		}
		key := kind + "|" + e.Kind
		if counted[key] == nil {
			counted[key] = map[nodeReceipt]bool{}
		}
		pair := nodeReceipt{nodeID, receipt}
		if counted[key][pair] {
			continue
		}
		counted[key][pair] = true
		sum := out[kind]
		switch e.Kind {
		case EdDeclaredUsedBy:
			sum.DeclaredUsedBy++
		case EdValidatedBy:
			sum.ValidatedBy++
		case EdMissedWhen:
			sum.MissedWhen++
		case EdFalseTriggeredBy:
			sum.FalseTriggeredBy++
		}
		out[kind] = sum
		if receiptsByKind[kind] == nil {
			receiptsByKind[kind] = map[string]bool{}
		}
		receiptsByKind[kind][receipt] = true
	}
	for kind, set := range receiptsByKind {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		sum := out[kind]
		sum.ReceiptIDs = ids
		out[kind] = sum
	}
	return out
}
