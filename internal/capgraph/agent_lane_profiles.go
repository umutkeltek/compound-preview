package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

const agentLaneNodeKind = "agent-lane"

var validAgentLaneTendencies = map[string]bool{
	"falsifier-first":  true,
	"receipt-faithful": true,
	"scope-creep":      true,
	"adjacent-fixer":   true,
	"ceremony-skipper": true,
}

type AgentLaneImportReport struct {
	Status         string         `json:"status"`
	SourceRevision string         `json:"source_revision,omitempty"`
	SourceRef      SkillSourceRef `json:"source_ref,omitempty"`
	NodeIDs        []string       `json:"node_ids,omitempty"`
	EdgeIDs        []string       `json:"edge_ids,omitempty"`
	AppliedNodes   int            `json:"applied_nodes"`
	AppliedEdges   int            `json:"applied_edges"`
}

type agentLaneProfileSeed struct {
	revision string
	source   SkillSourceRef
	nodes    []Node
	edges    []Edge
}

type agentLaneProfilesOverlay struct {
	Roles []agentLaneProfileRole `json:"roles"`
	Lanes []agentLaneProfileLane `json:"lanes"`
}

type agentLaneProfileRole struct {
	ID              string            `json:"id"`
	Family          string            `json:"family"`
	Description     string            `json:"description"`
	CapabilityClass string            `json:"capability_class"`
	QualityTier     string            `json:"quality_tier"`
	Emit            []string          `json:"emit"`
	Models          map[string]string `json:"models"`
	Tendencies      []string          `json:"tendencies"`
	ValidatedBy     []string          `json:"validated_by"`
}

type agentLaneProfileLane struct {
	ID          string   `json:"id"`
	Harness     string   `json:"harness"`
	Route       string   `json:"route"`
	CostClass   string   `json:"cost_class"`
	Note        string   `json:"note"`
	Tendencies  []string `json:"tendencies"`
	ValidatedBy []string `json:"validated_by"`
}

// AgentLaneTuple is the canonical lane identity input. Keep every component
// faithful: AgentLaneNodeID is the sole owner of agent-lane node IDs.
type AgentLaneTuple struct {
	Harness string
	Route   string
	Model   string
	Effort  string
}

type agentLaneTuple = AgentLaneTuple

// ImportAgentLaneProfiles derives evidence-only agent-lane nodes from the
// private profiles registry overlay. Missing seed files are a clean no-op so
// public clones can run the importer without carrying docs_private.
func ImportAgentLaneProfiles(homePath, registryPath, overlayPath string, now time.Time) (AgentLaneImportReport, error) {
	return ImportAgentLaneProfilesWithLaunchProfiles(homePath, registryPath, overlayPath, "", now)
}

// ImportAgentLaneProfilesWithLaunchProfiles additionally mints lane IDENTITY from the
// launch-profile authority at launchProfilesPath (see agent_lane_launch_profiles.go). An empty
// path is the overlay-only behaviour ImportAgentLaneProfiles preserves.
func ImportAgentLaneProfilesWithLaunchProfiles(homePath, registryPath, overlayPath, launchProfilesPath string, now time.Time) (AgentLaneImportReport, error) {
	seed, ok, err := loadAgentLaneProfiles(registryPath, overlayPath, launchProfilesPath, now)
	if err != nil {
		return AgentLaneImportReport{Status: "fail"}, err
	}
	if !ok {
		return AgentLaneImportReport{Status: "skipped"}, nil
	}
	report := AgentLaneImportReport{
		Status:         "ok",
		SourceRevision: seed.revision,
		SourceRef:      seed.source,
	}
	store, err := Open(homePath)
	if err != nil {
		return report, err
	}
	existingNodes, err := store.loadNodes()
	if err != nil {
		return report, err
	}
	nodesByID := map[string]Node{}
	for _, node := range existingNodes {
		nodesByID[node.ID] = node
	}
	var nodes []Node
	var edges []Edge
	for _, node := range seed.nodes {
		report.NodeIDs = append(report.NodeIDs, node.ID)
		if !reflect.DeepEqual(nodesByID[node.ID], node) {
			nodes = append(nodes, node)
			nodesByID[node.ID] = node
			report.AppliedNodes++
		}
	}

	existingEdges, err := store.loadEdges()
	if err != nil {
		return report, err
	}
	edgesByKey := map[string]bool{}
	for _, edge := range existingEdges {
		edgesByKey[edgeKey(edge)] = true
	}
	for _, edge := range seed.edges {
		if added, err := addEdgeOnceCollect(&edges, edgesByKey, edge); err != nil {
			return report, err
		} else if added {
			report.EdgeIDs = append(report.EdgeIDs, edgeID(edge))
			report.AppliedEdges++
		}
	}
	if len(nodes) > 0 || len(edges) > 0 {
		if err := ApplyNodesAndEdgesGoverned(homePath, nodes, edges, now); err != nil {
			return report, err
		}
	}
	return report, nil
}

func loadAgentLaneProfiles(registryPath, overlayPath, launchProfilesPath string, now time.Time) (agentLaneProfileSeed, bool, error) {
	if strings.TrimSpace(registryPath) == "" || strings.TrimSpace(overlayPath) == "" {
		return agentLaneProfileSeed{}, false, nil
	}
	registryData, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return agentLaneProfileSeed{}, false, nil
		}
		return agentLaneProfileSeed{}, false, err
	}
	overlayData, err := os.ReadFile(overlayPath)
	if err != nil {
		if os.IsNotExist(err) {
			return agentLaneProfileSeed{}, false, nil
		}
		return agentLaneProfileSeed{}, false, err
	}
	var overlay agentLaneProfilesOverlay
	if err := json.Unmarshal(overlayData, &overlay); err != nil {
		return agentLaneProfileSeed{}, false, fmt.Errorf("parse agent-lane profiles overlay %s: %w", overlayPath, err)
	}
	launchAuthority, hasLaunchProfiles, err := loadLaunchProfiles(launchProfilesPath)
	if err != nil {
		return agentLaneProfileSeed{}, false, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	revision := agentLaneProfilesRevision(registryData, overlayData)
	if hasLaunchProfiles {
		// The launch-profile authority's own revision joins the seed revision: a profile change
		// with an unchanged overlay is still a different lane graph, and a revision that missed
		// it would report "no drift" while identities moved underneath.
		revision = agentLaneProfilesRevision([]byte(revision), []byte(launchAuthority.AuthorityRevision))
	}
	seed := agentLaneProfileSeed{
		revision: revision,
		source: SkillSourceRef{
			Path:        overlayPath,
			Kind:        "profile_overlay",
			Owner:       "capability-graph",
			SourceClass: "agent-lane-profiles-json",
			Revision:    revision,
		},
	}
	if err := populateAgentLaneSeed(&seed, overlay, launchAuthority.Profiles, now); err != nil {
		return agentLaneProfileSeed{}, false, err
	}
	return seed, true, nil
}

func populateAgentLaneSeed(seed *agentLaneProfileSeed, overlay agentLaneProfilesOverlay, launchProfiles []LaunchProfile, now time.Time) error {
	roles := append([]agentLaneProfileRole(nil), overlay.Roles...)
	lanes := append([]agentLaneProfileLane(nil), overlay.Lanes...)
	sort.SliceStable(roles, func(i, j int) bool { return roles[i].ID < roles[j].ID })
	sort.SliceStable(lanes, func(i, j int) bool { return lanes[i].ID < lanes[j].ID })

	nodesByID := map[string]Node{}
	edgesByKey := map[string]Edge{}
	for _, role := range roles {
		for _, tendency := range cleanStrings(role.Tendencies) {
			if err := validateAgentLaneTendency(tendency); err != nil {
				return err
			}
		}
	}
	for _, lane := range lanes {
		for _, tendency := range cleanStrings(lane.Tendencies) {
			if err := validateAgentLaneTendency(tendency); err != nil {
				return err
			}
		}
	}
	for _, role := range roles {
		if strings.TrimSpace(role.Family) != "lane" {
			continue
		}
		capabilityClass := strings.TrimSpace(role.CapabilityClass)
		if capabilityClass == "" {
			return fmt.Errorf("agent-lane role %q missing capability_class", role.ID)
		}
		for _, emit := range cleanStrings(role.Emit) {
			model := agentLaneRoleModel(role, emit)
			for _, lane := range lanes {
				if !agentLaneEmitMatches(lane, emit) {
					continue
				}
				tuple := agentLaneTuple{
					Harness: strings.TrimSpace(lane.Harness),
					Route:   strings.TrimSpace(lane.Route),
					Model:   model,
					Effort:  strings.TrimSpace(role.QualityTier),
				}
				if tuple.Effort == "" {
					tuple.Effort = "unknown"
				}
				nodeID := agentLaneNodeID(tuple)
				node := nodesByID[nodeID]
				if node.ID == "" {
					node = Node{
						ID:           nodeID,
						Kind:         agentLaneNodeKind,
						Name:         agentLaneName(tuple),
						Weight:       0.25,
						EvidenceOnly: true,
					}
				}
				node.Triggers = mergeAgentLaneTriggers(node.Triggers, tuple, role, lane)
				nodesByID[nodeID] = node
				addAgentLaneEdge(edgesByKey, Edge{Kind: EdCapableOf, From: nodeID, To: "task-class:" + slugSkill(capabilityClass), Confidence: 1, CreatedAt: now.UTC().Format(time.RFC3339)})
				for _, tendency := range append(cleanStrings(lane.Tendencies), cleanStrings(role.Tendencies)...) {
					if err := validateAgentLaneTendency(tendency); err != nil {
						return err
					}
					addAgentLaneEdge(edgesByKey, Edge{Kind: EdTendsTo, From: nodeID, To: "tendency:" + strings.TrimSpace(tendency), Confidence: 1, CreatedAt: now.UTC().Format(time.RFC3339)})
				}
				for _, ref := range append(cleanStrings(lane.ValidatedBy), cleanStrings(role.ValidatedBy)...) {
					addAgentLaneEdge(edgesByKey, Edge{Kind: EdValidatedBy, From: nodeID, To: ref, Confidence: 1, CreatedAt: now.UTC().Format(time.RFC3339)})
				}
			}
		}
	}
	// Launch-profile lanes mint their identity from the authority, not from the role that binds
	// to them, so they run after the overlay pass and share its node/edge maps.
	if err := appendLaunchProfileLanes(nodesByID, edgesByKey, roles, launchProfiles, now); err != nil {
		return err
	}
	for _, node := range nodesByID {
		seed.nodes = append(seed.nodes, node)
	}
	sort.Slice(seed.nodes, func(i, j int) bool { return seed.nodes[i].ID < seed.nodes[j].ID })
	for _, edge := range edgesByKey {
		seed.edges = append(seed.edges, edge)
	}
	sort.Slice(seed.edges, func(i, j int) bool { return edgeKey(seed.edges[i]) < edgeKey(seed.edges[j]) })
	return nil
}

func agentLaneProfilesRevision(registryData, overlayData []byte) string {
	data := append([]byte{}, registryData...)
	data = append(data, 0)
	data = append(data, overlayData...)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func agentLaneRoleModel(role agentLaneProfileRole, emit string) string {
	if role.Models != nil {
		for _, key := range []string{emit, agentLaneHarnessAlias(emit)} {
			if model := strings.TrimSpace(role.Models[key]); model != "" {
				return model
			}
		}
	}
	return "inherit"
}

func agentLaneEmitMatches(lane agentLaneProfileLane, emit string) bool {
	emit = agentLaneEmitFamily(emit)
	candidates := []string{lane.ID, lane.Harness, agentLaneHarnessAlias(lane.ID), agentLaneHarnessAlias(lane.Harness)}
	for _, candidate := range candidates {
		if agentLaneEmitFamily(candidate) == emit {
			return true
		}
	}
	return false
}

// agentLaneEmitFamily lets a provider/profile variant consume the base harness's authored roles
// without collapsing the lane tuple itself. For example, a role emitted for "claude" can bind to
// a "claude-zai" lane, while the resulting node still carries Harness="claude-zai" for receipts
// and doctor attribution.
func agentLaneEmitFamily(value string) string {
	value = agentLaneHarnessAlias(value)
	if strings.HasPrefix(value, "claude-") {
		return "claude"
	}
	return value
}

func agentLaneHarnessAlias(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimSuffix(value, "-code")
	return value
}

func AgentLaneNodeID(tuple AgentLaneTuple) string {
	parts := []string{
		slugSkill(tuple.Harness),
		slugSkill(tuple.Route),
		slugSkill(tuple.Model),
		slugSkill(tuple.Effort),
	}
	return agentLaneNodeKind + ":" + strings.Join(parts, "--")
}

func agentLaneNodeID(tuple agentLaneTuple) string {
	return AgentLaneNodeID(tuple)
}

// ResolveLaneRef maps a lane reference to its canonical agent-lane node id, or reports
// (_, false) when the ref does not name exactly one imported agent-lane node. It is the single
// no-guessing lane resolver (B0, resolving MR-2/MR-3): every emitted lane identity flows
// through here, byte-identical to slice A's AgentLaneNodeID output, never re-slugged.
//
// Resolution (store-backed truth, mirroring routeevidence's preparation-lane pattern):
//  1. Node-id passthrough — a ref that IS an imported (non-retired) agent-lane node id
//     resolves to itself.
//  2. Short-id mapping — a ref mapped by AgentLaneNodeIDFromShortLaneID (the MR-3 short↔node
//     map) resolves ONLY when that mapped node is itself imported.
//  3. Everything else → ("", false).
//
// Store membership is the discriminator that satisfies falsifier (l): a bare harness ("codex")
// and an actor string ("agent-harness:0f0f0f0f") both *do* have a deterministic short-id
// mapping, but they map to synthetic nodes no import ever produced, so they never resolve —
// harness-level and actor-level evidence stays zero-evidence, never a guess. A store read
// error fails closed (unresolvable) so a declaration gate rejects rather than mis-attributes.
func (s *Store) ResolveLaneRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	nodes, err := s.AgentLaneNodes()
	if err != nil {
		return "", false
	}
	imported := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.Retired {
			continue
		}
		imported[n.ID] = true
	}
	if imported[ref] {
		return ref, true
	}
	if nodeID, ok := AgentLaneNodeIDFromShortLaneID(ref); ok && imported[nodeID] {
		return nodeID, true
	}
	return "", false
}

func AgentLaneNodeIDFromShortLaneID(laneID string) (string, bool) {
	tuple, ok := AgentLaneTupleFromShortLaneID(laneID)
	if !ok {
		return "", false
	}
	return AgentLaneNodeID(tuple), true
}

func AgentLaneTupleFromShortLaneID(laneID string) (AgentLaneTuple, bool) {
	laneID = strings.TrimSpace(laneID)
	if laneID == "" || laneID == "*" {
		return AgentLaneTuple{}, false
	}
	return AgentLaneTuple{Harness: shortLaneHarness(laneID), Route: laneID, Model: laneID, Effort: "unknown"}, true
}

func shortLaneHarness(laneID string) string {
	if idx := strings.IndexAny(laneID, "/:"); idx > 0 {
		return laneID[:idx]
	}
	return laneID
}

func agentLaneName(tuple agentLaneTuple) string {
	return strings.Join([]string{
		strings.TrimSpace(tuple.Harness),
		strings.TrimSpace(tuple.Route),
		strings.TrimSpace(tuple.Model),
		strings.TrimSpace(tuple.Effort),
	}, " / ")
}

func mergeAgentLaneTriggers(existing []string, tuple agentLaneTuple, role agentLaneProfileRole, lane agentLaneProfileLane) []string {
	values := []string{tuple.Harness, tuple.Route, tuple.Model, tuple.Effort, role.CapabilityClass, role.Description, lane.CostClass, lane.Note}
	out := append([]string{}, existing...)
	for _, token := range skillTokenList(values...) {
		out = appendUniqueString(out, token)
	}
	sort.Strings(out)
	return out
}

func validateAgentLaneTendency(tendency string) error {
	tendency = strings.TrimSpace(tendency)
	if validAgentLaneTendencies[tendency] {
		return nil
	}
	return fmt.Errorf("unknown tendency %q (allowed: %s)", tendency, strings.Join(agentLaneTendencyVocabulary(), ", "))
}

func agentLaneTendencyVocabulary() []string {
	out := make([]string, 0, len(validAgentLaneTendencies))
	for tendency := range validAgentLaneTendencies {
		out = append(out, tendency)
	}
	sort.Strings(out)
	return out
}

func addAgentLaneEdge(edges map[string]Edge, edge Edge) {
	if strings.TrimSpace(edge.To) == "" {
		return
	}
	edges[edgeKey(edge)] = edge
}

// AgentLaneNode is a read-only view of one agent-lane capability node: its identity plus the
// raw harness token parsed from the lane name. It is the shape a coverage/surfacing reader
// needs without exporting the store's whole node set or its internals. The node is evidence-
// only and never executes.
type AgentLaneNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Harness string `json:"harness"` // raw harness token (first segment of the lane name)
	Retired bool   `json:"retired,omitempty"`
}

// AgentLaneNodes returns every agent-lane node in the store, sorted by id. It is the single
// additive read-only door onto lane rows for consumers (e.g. `compound doctor`'s surfacing
// matrix): it filters to the agent-lane kind and parses the harness from the lane name, so no
// consumer has to reimplement the lane-name format or reach past this reader into the store.
func (s *Store) AgentLaneNodes() ([]AgentLaneNode, error) {
	nodes, err := s.loadNodes()
	if err != nil {
		return nil, err
	}
	out := make([]AgentLaneNode, 0)
	for _, n := range nodes {
		if n.Kind != agentLaneNodeKind {
			continue
		}
		out = append(out, AgentLaneNode{
			ID:      n.ID,
			Name:    n.Name,
			Harness: agentLaneHarnessFromName(n.Name),
			Retired: n.Retired,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// agentLaneHarnessFromName extracts the harness token from a lane name built by agentLaneName
// ("harness / route / model / effort"). It is the inverse of that join for the first segment.
func agentLaneHarnessFromName(name string) string {
	parts := strings.SplitN(name, " / ", 2)
	return strings.TrimSpace(parts[0])
}

// AgentLaneCard is a read-only display summary of one agent-lane node, built from slice A's
// capability-graph edges: the lane identity, the capability domains it is `capable_of`, the
// tendency slugs it `tends_to`, and a lane-level count of `validated_by` receipts. It exposes
// the graph's edge conventions ("task-class:" / "tendency:" prefixes) as clean slugs so display
// consumers (e.g. the orient-time peers section) never reimplement the edge format or reach past
// this reader into the store. Evidence-only; the reader writes nothing.
type AgentLaneCard struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Harness          string   `json:"harness"`
	Capabilities     []string `json:"capabilities"`
	Tendencies       []string `json:"tendencies"`
	ValidatedByCount int      `json:"validated_by_count"`
	Retired          bool     `json:"retired,omitempty"`
}

// AgentLaneCards returns one read-only card per agent-lane node, sorted by id, joining each node
// with its outgoing capable_of / tends_to / validated_by edges. Capability domains and tendency
// slugs are sorted deterministically by slug. validated_by edges point at receipt/eval refs, not
// per-domain targets, so their tally is lane-level (surfaced as ValidatedByCount) rather than
// weighting one domain over another — per-domain evidence weighting is slice-B territory. This is
// the single additive read-only edge reader the orient peers section needs; it never writes.
func (s *Store) AgentLaneCards() ([]AgentLaneCard, error) {
	nodes, err := s.loadNodes()
	if err != nil {
		return nil, err
	}
	edges, err := s.loadEdges()
	if err != nil {
		return nil, err
	}
	capByNode := map[string]map[string]bool{}
	tendByNode := map[string]map[string]bool{}
	validatedByNode := map[string]int{}
	for _, e := range edges {
		switch e.Kind {
		case EdCapableOf:
			if slug := strings.TrimPrefix(e.To, "task-class:"); slug != e.To && slug != "" {
				laneCardAddSlug(capByNode, e.From, slug)
			}
		case EdTendsTo:
			if slug := strings.TrimPrefix(e.To, "tendency:"); slug != e.To && slug != "" {
				laneCardAddSlug(tendByNode, e.From, slug)
			}
		case EdValidatedBy:
			validatedByNode[e.From]++
		}
	}
	out := make([]AgentLaneCard, 0)
	for _, n := range nodes {
		if n.Kind != agentLaneNodeKind {
			continue
		}
		out = append(out, AgentLaneCard{
			ID:               n.ID,
			Name:             n.Name,
			Harness:          agentLaneHarnessFromName(n.Name),
			Capabilities:     laneCardSortedSlugs(capByNode[n.ID]),
			Tendencies:       laneCardSortedSlugs(tendByNode[n.ID]),
			ValidatedByCount: validatedByNode[n.ID],
			Retired:          n.Retired,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func laneCardAddSlug(m map[string]map[string]bool, node, slug string) {
	if m[node] == nil {
		m[node] = map[string]bool{}
	}
	m[node][slug] = true
}

func laneCardSortedSlugs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for slug := range set {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}
