package capgraph

import (
	"sort"
	"strings"
)

// ReachabilitySchemaVersion tags the reachability projection.
const ReachabilitySchemaVersion = "compound.capability_reachability.v0.1"

// Reachability answers a question the capability graph could not previously be
// asked: of everything Compound can NAME, how much can an agent actually USE?
//
// The graph is good at existence and bad at access. A node says a capability
// exists; a ConnectionSurface says how to reach it; a CredentialHandle says what
// unlocks it. Only the first is populated at scale, which is why a harness that
// is told a tool exists still ends up reading a wrapper script to work out how
// to invoke it -- the served answer stopped one field short of useful, and
// improvisation is what fills that gap.
//
// This projection exists so that shortfall is a number in a doctor rather than a
// recurring surprise in a session. It reads; it never writes.
type Reachability struct {
	SchemaVersion string             `json:"schema_version"`
	Kinds         []KindReachability `json:"kinds"`
	Total         KindReachability   `json:"total"`
	Gaps          []ReachabilityGap  `json:"gaps,omitempty"`
	// Harnesses reports what each executor can actually see. Scoping is the
	// half of this problem that is invisible in the totals above.
	Harnesses []HarnessReachability `json:"harnesses,omitempty"`
	// Corrections is the operator-correction signal already sitting in the
	// graph. It is the cheapest available evidence of where routing is worst.
	Corrections CorrectionSignal `json:"corrections"`
}

// HarnessReachability reports the pool one executor is offered.
//
// The important number is Generic, not Bound. A node with no surfaces_with edge
// is not hidden from a harness -- it is offered to EVERY harness, because
// unscoped means generic. So a graph that is mostly unscoped does not fail by
// showing an executor too little; it fails by showing every executor the same
// undifferentiated pile, including entries it cannot run. The operator then
// does the filtering by hand, one "use this, not that" at a time, and that
// instruction is the routing decision the graph should have made.
type HarnessReachability struct {
	Harness string `json:"harness"`
	// Bound counts capabilities explicitly scoped to this harness.
	Bound int `json:"bound"`
	// Generic counts unscoped capabilities, which this harness is offered by
	// default along with every other harness.
	Generic int `json:"generic"`
	// ScopedShare is Bound / (Bound + Generic): how much of what this executor
	// sees was actually chosen for it. Low means the offer list is a pile.
	ScopedShare float64 `json:"scoped_share"`
}

// CorrectionSignal counts the graph edges that record routing having gone wrong
// -- a capability that fired when it should not have, or that should have fired
// and did not.
//
// These edges are the accumulated residue of an operator saying "use this, not
// that". The signal already exists; nothing consumes it as a routing input. It
// is surfaced here because an unread signal and an absent one are the same
// thing at the point of use, and this one is already paid for.
type CorrectionSignal struct {
	// FalseTriggeredNodes carried at least one false_triggered_by edge: they
	// fired in a situation where they were wrong.
	FalseTriggeredNodes int `json:"false_triggered_nodes"`
	// MissedNodes carried at least one missed_when edge: they should have fired
	// and did not.
	MissedNodes int `json:"missed_nodes"`
	// Observations is the raw edge count behind those nodes.
	Observations int `json:"observations"`
	// UnscopedWithCorrections counts corrected capabilities that STILL carry no
	// harness scope. Each one is a recorded routing mistake that the graph has
	// not been taught to avoid, so it remains free to repeat.
	UnscopedWithCorrections int `json:"unscoped_with_corrections"`
}

// KindReachability is the per-kind tally. Named counts rather than percentages
// so a caller can re-derive any ratio without trusting ours.
type KindReachability struct {
	Kind string `json:"kind"`
	// Nodes is how many capabilities of this kind Compound can name.
	Nodes int `json:"nodes"`
	// Reachable is how many carry a ConnectionSurface -- an answer to "how do I
	// get to it".
	Reachable int `json:"reachable"`
	// Unlockable is how many carry a credential handle -- an answer to "and what
	// authenticates me". A capability can be reachable without being unlockable,
	// which is the state that sends an agent looking for a key by hand.
	Unlockable int `json:"unlockable"`
	// ValueTransiting counts handles whose mechanism causes the secret to pass
	// through a process Compound started. Operators auditing exposure want this
	// small, and want it visible rather than inferred.
	ValueTransiting int `json:"value_transiting"`
	// Degraded counts nodes whose last observed health was not fine. They are
	// still named, still offered, still reachable -- this is the number that used
	// to be invisible, because degradation was expressed by retiring the node,
	// and a retired node is excluded from this tally entirely. A capability that
	// is down reads exactly like a capability that never existed, which is the
	// one thing an operator most needs to tell apart.
	Degraded int `json:"degraded"`
}

// ReachabilityGap names a kind whose access facts are materially missing, worst
// first. It is deliberately a list of kinds rather than of nodes: the fix is
// always an import door for a whole kind, never a hand-written record per node.
// Anything else would make the map the work.
type ReachabilityGap struct {
	Kind        string `json:"kind"`
	Nodes       int    `json:"nodes"`
	Missing     int    `json:"missing"`
	Consequence string `json:"consequence"`
}

// ComputeReachability joins nodes against connection surfaces. A kind with zero
// nodes never appears; a kind with full coverage produces no gap.
func (s *Store) ComputeReachability() (Reachability, error) {
	nodes, err := s.loadNodes()
	if err != nil {
		return Reachability{}, err
	}
	surfaces, err := s.loadConnectionSurfaces()
	if err != nil {
		return Reachability{}, err
	}
	byNode := make(map[string]ConnectionSurface, len(surfaces))
	for _, cs := range surfaces {
		byNode[cs.NodeID] = cs
	}

	tally := map[string]*KindReachability{}
	for _, n := range nodes {
		// A retired capability is not a gap. Counting one would inflate the
		// shortfall and make the number the operator is meant to act on the
		// first thing they learn to discount.
		if n.Retired {
			continue
		}
		kind := strings.TrimSpace(n.Kind)
		if kind == "" {
			kind = "unknown"
		}
		k, ok := tally[kind]
		if !ok {
			k = &KindReachability{Kind: kind}
			tally[kind] = k
		}
		k.Nodes++
		if strings.TrimSpace(n.Health) != "" {
			k.Degraded++
		}
		cs, has := byNode[n.ID]
		if !has {
			continue
		}
		// A surface with no endpoint AND no config path answers nothing, so it
		// does not count as reachable. Counting a placeholder would report the
		// gap as closed while the agent still cannot act -- the failure mode
		// this projection exists to prevent.
		if strings.TrimSpace(cs.Endpoint) == "" && len(cs.ConfigPaths) == 0 {
			continue
		}
		// Slice-2: a skill node stamped with Source.Reachable=false (its on-disk
		// skill dir is gone or a dangling symlink) is gated out of `reachable`
		// regardless of its connection surface. A nil Source means "no disk
		// evidence at import time" and falls through to the pre-slice-2
		// behaviour (reachable iff a connection surface exists). Non-skill
		// kinds are unaffected.
		if n.Kind == "skill" && n.Source != nil && !n.Source.Reachable {
			continue
		}
		k.Reachable++
		if strings.TrimSpace(cs.CredentialHandle.Ref) != "" {
			k.Unlockable++
			if transits, known := CredentialValueTransits(cs.CredentialHandle); known && transits {
				k.ValueTransiting++
			}
		}
	}

	out := Reachability{SchemaVersion: ReachabilitySchemaVersion}
	for _, k := range tally {
		out.Kinds = append(out.Kinds, *k)
		out.Total.Nodes += k.Nodes
		out.Total.Reachable += k.Reachable
		out.Total.Unlockable += k.Unlockable
		out.Total.ValueTransiting += k.ValueTransiting
		out.Total.Degraded += k.Degraded
	}
	out.Total.Kind = "total"
	// Worst coverage first, then largest, then name: the operator reads the top
	// of this list and nothing else, so ordering IS the recommendation.
	sort.Slice(out.Kinds, func(i, j int) bool {
		a, b := out.Kinds[i], out.Kinds[j]
		ac := coverageRatio(a.Reachable, a.Nodes)
		bc := coverageRatio(b.Reachable, b.Nodes)
		if ac != bc {
			return ac < bc
		}
		if a.Nodes != b.Nodes {
			return a.Nodes > b.Nodes
		}
		return a.Kind < b.Kind
	})
	for _, k := range out.Kinds {
		missing := k.Nodes - k.Reachable
		if missing == 0 {
			continue
		}
		out.Gaps = append(out.Gaps, ReachabilityGap{
			Kind: k.Kind, Nodes: k.Nodes, Missing: missing,
			Consequence: gapConsequence(k.Kind),
		})
	}

	edges, err := s.loadEdges()
	if err != nil {
		return Reachability{}, err
	}
	// Indexed once: the edge log is thousands of rows and a linear scan per
	// edge turns a read-only report into something nobody runs.
	liveByID := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		liveByID[n.ID] = !n.Retired
	}
	live := func(id string) bool { return liveByID[id] }
	scopedTo := map[string]map[string]bool{} // node -> harnesses
	corrected := map[string]bool{}
	var observations int
	falseTriggered, missed := map[string]bool{}, map[string]bool{}
	harnessNames := map[string]bool{}
	for _, e := range edges {
		switch e.Kind {
		case EdSurfacesWith:
			if !strings.HasPrefix(e.To, "harness:") || !live(e.From) {
				continue
			}
			if scopedTo[e.From] == nil {
				scopedTo[e.From] = map[string]bool{}
			}
			to := harnessID(e.To)
			scopedTo[e.From][to] = true
			harnessNames[to] = true
		case EdFalseTriggeredBy, EdMissedWhen:
			// Direction matters and is the opposite of the intuitive reading:
			// these edges run SITUATION -> CAPABILITY ("in this context, that
			// capability triggered wrongly / should have triggered"). Reading From as
			// the capability silently yields zero corrections on a real home,
			// which looks like "nothing to learn from" rather than a bug.
			if !live(e.To) {
				continue
			}
			if e.Kind == EdFalseTriggeredBy {
				falseTriggered[e.To] = true
			} else {
				missed[e.To] = true
			}
			corrected[e.To] = true
			observations++
		}
	}
	genericCount := out.Total.Nodes - len(scopedTo)
	for name := range harnessNames {
		h := HarnessReachability{Harness: name, Generic: genericCount}
		for _, scopes := range scopedTo {
			if scopes[name] {
				h.Bound++
			}
		}
		if seen := h.Bound + h.Generic; seen > 0 {
			h.ScopedShare = float64(h.Bound) / float64(seen)
		}
		out.Harnesses = append(out.Harnesses, h)
	}
	sort.Slice(out.Harnesses, func(i, j int) bool {
		if out.Harnesses[i].Bound != out.Harnesses[j].Bound {
			return out.Harnesses[i].Bound > out.Harnesses[j].Bound
		}
		return out.Harnesses[i].Harness < out.Harnesses[j].Harness
	})
	out.Corrections = CorrectionSignal{
		FalseTriggeredNodes: len(falseTriggered),
		MissedNodes:         len(missed),
		Observations:        observations,
	}
	for id := range corrected {
		if len(scopedTo[id]) == 0 {
			out.Corrections.UnscopedWithCorrections++
		}
	}
	return out, nil
}

func coverageRatio(have, total int) float64 {
	if total == 0 {
		return 1
	}
	return float64(have) / float64(total)
}

// gapConsequence states what the missing facts cost at the point of use. Kinds
// fail differently, and a generic "no connection surface" line teaches nobody
// which door to build next.
func gapConsequence(kind string) string {
	switch kind {
	case "tool":
		return "a harness is told the tool exists but not how to invoke it, so it infers a command or reads a wrapper script"
	case "mcp":
		return "a server is listed but carries no transport or launch facts, so a harness cannot connect to it from the served answer alone"
	case "skill":
		return "a skill is discoverable by description but its host prerequisites are not served, so it fires in a harness that cannot run it"
	case "adapter", "service", "gateway":
		return "an integration is named without an address, so reaching it depends on someone remembering where it lives"
	case "model_profile", "agent-lane", "harness":
		return "a lane is nameable but not launchable from served state, so lane selection falls back to hardcoded knowledge"
	default:
		return "the capability can be named but not acted on from served state alone"
	}
}
