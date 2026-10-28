package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// canonProjectionPrefix is the capability-graph node namespace a canon-evolution transition rides in
// the flat Projections.Skill slice (D3: one node namespace, no new ProjectionKind).
const canonProjectionPrefix = "capability://"

// CanonEvolutionInput is what the harness-canon Importer hands the ObjectFamily after observing drift:
// the node refs the changed files carry, the causal class and cause refs of the change, who consumes
// it next, the changed file paths, and the parked candidate/no-promote evidence.
type CanonEvolutionInput struct {
	NodeRefs     []string
	DeltaCause   string
	CausedByRefs []string
	NextConsumer string
	ChangedRefs  []string
	EvidenceRefs []string
}

// CanonEvolutionReport is the non-transition summary of a RecordCanonEvolution call: the transition
// id minted (empty when zero nodes were valid), the nodes ensured, and the refs skipped for an
// unknown kind (never silent).
type CanonEvolutionReport struct {
	TransitionID    string   `json:"transition_id,omitempty"`
	EnsuredNodeIDs  []string `json:"ensured_node_ids"`
	SkippedNodeRefs []string `json:"skipped_node_refs,omitempty"`
}

// canonNodeKindForRef derives the capability-graph node kind from a canon node ref's id prefix.
// Only harness: and tool: are first-class canon kinds in v1; any other prefix is skipped, not
// invented as a node.
func canonNodeKindForRef(ref string) (string, bool) {
	switch {
	case strings.HasPrefix(ref, "harness:"):
		return "harness", true
	case strings.HasPrefix(ref, "tool:"):
		return "tool", true
	default:
		return "", false
	}
}

// RecordCanonEvolution is the ObjectFamily half of harness co-evolution: it ensures the touched
// capability-graph nodes (idempotent, non-clobbering; an unknown kind is reported skipped, never
// aborts), mints ONE cause-carrying Transition projected onto those nodes via capability://<id>, and
// records the source-authority handoff (the canon files remain source-in-place evidence). Zero valid
// nodes => no transition, report only. Importers observe; only this ObjectFamily mints.
func RecordCanonEvolution(homePath string, in CanonEvolutionInput, now time.Time) (envelope.Transition, CanonEvolutionReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return envelope.Transition{}, CanonEvolutionReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !contains(envelope.DeltaCauses, strings.TrimSpace(in.DeltaCause)) {
		return envelope.Transition{}, CanonEvolutionReport{}, fmt.Errorf("delta_cause must be one of %v, got %q", envelope.DeltaCauses, in.DeltaCause)
	}
	if len(cleanSorted(in.CausedByRefs)) == 0 {
		return envelope.Transition{}, CanonEvolutionReport{}, fmt.Errorf("caused_by_refs requires at least one ref")
	}

	store, err := Open(homePath)
	if err != nil {
		return envelope.Transition{}, CanonEvolutionReport{}, err
	}
	existing, err := store.List()
	if err != nil {
		return envelope.Transition{}, CanonEvolutionReport{}, err
	}
	present := map[string]bool{}
	for _, n := range existing {
		present[n.ID] = true
	}

	report := CanonEvolutionReport{}
	var ensured []string
	seen := map[string]bool{}
	var nodes []Node
	for _, ref := range in.NodeRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		kind, ok := canonNodeKindForRef(ref)
		if !ok {
			report.SkippedNodeRefs = append(report.SkippedNodeRefs, ref)
			continue
		}
		if !present[ref] {
			// First-of-kind ensure: register a minimal evidence-only node. Never clobber a node an
			// upstream importer already owns with richer fields.
			nodes = append(nodes, Node{ID: ref, Kind: kind, Name: ref, Triggers: []string{ref}, EvidenceOnly: true})
			present[ref] = true
		}
		ensured = append(ensured, ref)
	}
	if len(nodes) > 0 {
		if err := ApplyNodesAndEdgesGoverned(homePath, nodes, nil, now); err != nil {
			return envelope.Transition{}, report, err
		}
	}
	sort.Strings(ensured)
	sort.Strings(report.SkippedNodeRefs)
	report.EnsuredNodeIDs = ensured

	// Zero valid nodes => nothing to project; return the report so the skip is never silent.
	if len(ensured) == 0 {
		return envelope.Transition{}, report, nil
	}

	tr, err := buildCanonEvolutionTransition(in, ensured, now)
	if err != nil {
		return envelope.Transition{}, report, err
	}
	if err := recordCanonTransition(homePath, tr); err != nil {
		return envelope.Transition{}, report, err
	}
	report.TransitionID = tr.ID

	if _, err := RecordSourceAuthority(homePath, SourceAuthorityInput{
		SourcePath:     "compound://harness-canon",
		SourceKind:     "harness-canon-file",
		SourceOwner:    "harness-canon",
		SourceClass:    "harness-canon-file",
		SourceRevision: canonSourceRevision(in, ensured),
		ImportedVia:    "compound source canon-observe",
		NodeIDs:        ensured,
		EvidenceRefs:   cleanSorted(append(append([]string{}, in.EvidenceRefs...), in.CausedByRefs...)),
	}, now); err != nil {
		return envelope.Transition{}, report, err
	}

	return tr, report, nil
}

func buildCanonEvolutionTransition(in CanonEvolutionInput, ensured []string, now time.Time) (envelope.Transition, error) {
	causedBy := cleanSorted(in.CausedByRefs)
	changed := cleanSorted(in.ChangedRefs)
	skillRefs := make([]string, 0, len(ensured))
	for _, id := range ensured {
		skillRefs = append(skillRefs, canonProjectionPrefix+id)
	}
	sort.Strings(skillRefs)
	evidence := cleanSorted(append(append([]string{}, in.EvidenceRefs...), causedBy...))
	if len(evidence) == 0 {
		evidence = skillRefs
	}
	// Identity carries content, not just topology: the evidence refs (live-hash-derived candidate ids)
	// and caused-by refs are folded into both the transition and work ids so the same file drifting
	// A->B then B->C under the same cause mints DISTINCT ids and never clobbers the prior events/<id>.json.
	// No timestamp participates — a byte-identical re-run (e.g. after a failed mint) must reproduce the
	// same id so transaction-safe retries stay deterministic.
	evidenceKey := strings.Join(cleanSorted(in.EvidenceRefs), ",")
	causedByKey := strings.Join(causedBy, ",")
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            canonEvolutionID("tr_canon_", strings.Join(ensured, ","), in.DeltaCause, strings.Join(changed, ","), evidenceKey, causedByKey),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: canonEvolutionID("work_canon_", strings.Join(ensured, ","), in.DeltaCause, evidenceKey, causedByKey)},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  evidence,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SourceTraceID: "canon-observe:" + strings.TrimSpace(in.DeltaCause),
		BeforeRefs:    changed,
		ActionRefs:    causedBy,
		AfterRefs:     skillRefs,
		StateDelta:    fmt.Sprintf("harness canon %s across %d file(s) projected onto %d node(s)", strings.TrimSpace(in.DeltaCause), len(changed), len(ensured)),
		Projections: envelope.Projections{
			Skill: skillRefs,
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
		Cause: &envelope.TransitionCause{
			DeltaCause:   strings.TrimSpace(in.DeltaCause),
			CausedByRefs: causedBy,
			NextConsumer: strings.TrimSpace(in.NextConsumer),
		},
	}
	if err := tr.Validate(); err != nil {
		return envelope.Transition{}, err
	}
	return tr, nil
}

// recordCanonTransition persists the minted transition through the shared event-store seam.
func recordCanonTransition(homePath string, tr envelope.Transition) error {
	if err := tr.Validate(); err != nil {
		return err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	_, err = store.PutJSON(tr.ID, &tr, "  ")
	return err
}

func canonSourceRevision(in CanonEvolutionInput, ensured []string) string {
	parts := append([]string{}, cleanSorted(in.ChangedRefs)...)
	parts = append(parts, ensured...)
	parts = append(parts, strings.TrimSpace(in.DeltaCause))
	// Evidence refs carry the live-hash-derived candidate ids, so distinct content produces a distinct
	// source-authority revision — otherwise A->B and B->C on the same file would share a revision id and
	// the second RecordSourceAuthority record would shadow the first.
	parts = append(parts, cleanSorted(in.EvidenceRefs)...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "canon:" + hex.EncodeToString(sum[:])[:12]
}

func canonEvolutionID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}
