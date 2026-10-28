package distiller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/mem"
	"compound/internal/writeback"
)

// PriorCandidate is the bounded shape distiller needs from typed candidate
// inboxes. It intentionally does not copy full source bodies.
type PriorCandidate struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Subject      string   `json:"subject,omitempty"`
	Statement    string   `json:"statement,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	Entity       string   `json:"entity,omitempty"`
	Predicate    string   `json:"predicate,omitempty"`
	Object       string   `json:"object,omitempty"`
	ProductID    string   `json:"product_id,omitempty"`
	ChangeEvent  string   `json:"change_event,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type PriorCapabilityNode struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Triggers []string `json:"triggers,omitempty"`
	Retired  bool     `json:"retired,omitempty"`
}

type PriorNoPromote struct {
	ID            string `json:"id"`
	CandidateRef  string `json:"candidate_ref"`
	CandidateKind string `json:"candidate_kind,omitempty"`
	Basis         string `json:"basis"`
	Status        string `json:"status"`
}

// PriorCoverage is a compact coverage report for CLI/proof surfaces.
type PriorCoverage struct {
	CurrentClaims      int `json:"current_claims"`
	Decisions          int `json:"decisions"`
	Preferences        int `json:"preferences"`
	Architectures      int `json:"architectures"`
	Statuses           int `json:"statuses"`
	Constraints        int `json:"constraints"`
	SkillCandidates    int `json:"skill_candidates"`
	DecisionCandidates int `json:"decision_candidates"`
	FailurePatterns    int `json:"failure_patterns"`
	TasteObservations  int `json:"taste_observations"`
	ProductHypotheses  int `json:"product_hypotheses"`
	RouteObservations  int `json:"route_observations"`
	CapabilityObs      int `json:"capability_observations"`
	EvalCases          int `json:"eval_cases"`
	MethodUpgrades     int `json:"method_upgrades"`
	PolicyPatches      int `json:"policy_patches"`
	CapabilityNodes    int `json:"capability_nodes"`
	NoPromoteDecisions int `json:"no_promote_decisions"`
}

// LoadPriorStateFromHome loads the richer RetrievedPriorState from owned
// ledgers/inboxes under COMPOUND_HOME.
func LoadPriorStateFromHome(homePath string, store *mem.Store, policy Policy) (PriorState, error) {
	cur, err := store.AllCurrent()
	if err != nil {
		return PriorState{}, err
	}
	prior := PriorState{
		PolicyVersion:    policy.Stamp(),
		Current:          cur,
		ClaimsByType:     map[string][]mem.Claim{},
		CandidatesByKind: map[string][]PriorCandidate{},
	}
	for _, claim := range cur {
		if strings.TrimSpace(claim.ClaimType) != "" {
			prior.ClaimsByType[claim.ClaimType] = append(prior.ClaimsByType[claim.ClaimType], claim)
		}
	}
	if strings.TrimSpace(homePath) == "" {
		return prior, nil
	}
	for _, kind := range []string{
		KindMemory,
		KindFailure,
		KindTaste,
		KindProduct,
		KindCapabilityObs,
		KindEvalCase,
		KindMethodUpgrade,
		KindPolicyPatch,
		KindSkill,
		KindDecision,
		KindRoute,
	} {
		got, err := loadPriorCandidates(homePath, kind)
		if err != nil {
			return prior, err
		}
		prior.CandidatesByKind[kind] = got
	}
	nodes, err := loadCapabilityNodes(homePath)
	if err != nil {
		return prior, err
	}
	prior.CapabilityNodes = nodes
	noPromotes, err := readNoPromoteDecisions(filepath.Join(homePath, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		return prior, err
	}
	prior.NoPromoteDecisions = noPromotePrior(noPromotes)
	return prior, nil
}

func (p PriorState) Coverage() PriorCoverage {
	return PriorCoverage{
		CurrentClaims:      len(p.Current),
		Decisions:          len(p.ClaimsByType["decision"]),
		Preferences:        len(p.ClaimsByType["preference"]),
		Architectures:      len(p.ClaimsByType["architecture"]),
		Statuses:           len(p.ClaimsByType["status"]),
		Constraints:        len(p.ClaimsByType["constraint"]),
		SkillCandidates:    len(p.CandidatesByKind[KindSkill]),
		DecisionCandidates: len(p.CandidatesByKind[KindDecision]),
		FailurePatterns:    len(p.CandidatesByKind[KindFailure]),
		TasteObservations:  len(p.CandidatesByKind[KindTaste]),
		ProductHypotheses:  len(p.CandidatesByKind[KindProduct]),
		RouteObservations:  len(p.CandidatesByKind[KindRoute]),
		CapabilityObs:      len(p.CandidatesByKind[KindCapabilityObs]),
		EvalCases:          len(p.CandidatesByKind[KindEvalCase]),
		MethodUpgrades:     len(p.CandidatesByKind[KindMethodUpgrade]),
		PolicyPatches:      len(p.CandidatesByKind[KindPolicyPatch]),
		CapabilityNodes:    len(p.CapabilityNodes),
		NoPromoteDecisions: len(p.NoPromoteDecisions),
	}
}

func loadPriorCandidates(homePath, kind string) ([]PriorCandidate, error) {
	rel := candidates.InboxDir(candidates.Kind(kind))
	if rel == "" {
		return nil, fmt.Errorf("candidate kind %q has no inbox mapping", kind)
	}
	dir := filepath.Join(homePath, rel)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PriorCandidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := readPriorCandidate(filepath.Join(dir, entry.Name()), kind)
		if err != nil {
			return nil, err
		}
		if c.ID != "" {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func readPriorCandidate(path, fallbackKind string) (PriorCandidate, error) {
	var c PriorCandidate
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse prior candidate %s: %w", filepath.Base(path), err)
	}
	if c.Kind == "" {
		c.Kind = fallbackKind
	}
	return c, nil
}

func loadCapabilityNodes(homePath string) ([]PriorCapabilityNode, error) {
	store, err := capgraph.Open(homePath)
	if err != nil {
		return nil, err
	}
	nodes, err := store.List()
	if err != nil {
		return nil, err
	}
	out := make([]PriorCapabilityNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, PriorCapabilityNode{
			ID: node.ID, Kind: node.Kind, Name: node.Name,
			Triggers: append([]string(nil), node.Triggers...), Retired: node.Retired,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func noPromotePrior(rows []writeback.NoPromoteDecision) []PriorNoPromote {
	out := make([]PriorNoPromote, 0, len(rows))
	for _, row := range rows {
		out = append(out, PriorNoPromote{
			ID: row.ID, CandidateRef: row.CandidateRef, CandidateKind: row.CandidateKind,
			Basis: row.Basis, Status: row.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
