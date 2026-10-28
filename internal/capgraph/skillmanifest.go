package capgraph

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const SkillManifestSchemaVersion = "compound.skill_manifest.v0.1"

type SkillManifest struct {
	SchemaVersion string               `json:"schema_version" yaml:"schema_version"`
	ID            string               `json:"id" yaml:"id"`
	AdapterID     string               `json:"adapter_id" yaml:"adapter_id"`
	SourceSystem  string               `json:"source_system" yaml:"source_system"`
	ObservedAt    string               `json:"observed_at" yaml:"observed_at"`
	SourceRef     SkillSourceRef       `json:"source_ref" yaml:"source_ref"`
	Skills        []SkillManifestEntry `json:"skills" yaml:"skills"`
	EvidenceRefs  []string             `json:"evidence_refs" yaml:"evidence_refs"`
}

type SkillSourceRef struct {
	Path        string `json:"path" yaml:"path"`
	Kind        string `json:"kind" yaml:"kind"`
	Owner       string `json:"owner" yaml:"owner"`
	SourceClass string `json:"source_class" yaml:"source_class"`
	Revision    string `json:"revision" yaml:"revision"`
}

type SkillManifestEntry struct {
	ID                  string                `json:"id" yaml:"id"`
	Kind                string                `json:"kind,omitempty" yaml:"kind,omitempty"`
	Name                string                `json:"name" yaml:"name"`
	Status              string                `json:"status" yaml:"status"`
	Triggers            []string              `json:"triggers" yaml:"triggers"`
	Capabilities        []string              `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	CompatibleHarnesses []string              `json:"compatible_harnesses,omitempty" yaml:"compatible_harnesses,omitempty"`
	Requires            []string              `json:"requires,omitempty" yaml:"requires,omitempty"`
	Specializes         []string              `json:"specializes,omitempty" yaml:"specializes,omitempty"`
	ConflictsWith       []string              `json:"conflicts_with,omitempty" yaml:"conflicts_with,omitempty"`
	ValidatedBy         []string              `json:"validated_by,omitempty" yaml:"validated_by,omitempty"`
	ReplacementFor      []string              `json:"replacement_for,omitempty" yaml:"replacement_for,omitempty"`
	Weight              float64               `json:"weight,omitempty" yaml:"weight,omitempty"`
	Retired             bool                  `json:"retired,omitempty" yaml:"retired,omitempty"`
	EvidenceRefs        []string              `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
	When                []InstructionRuleWhen `json:"when,omitempty" yaml:"when,omitempty"`
	Read                []string              `json:"read,omitempty" yaml:"read,omitempty"`
	Route               []InstructionRoute    `json:"route,omitempty" yaml:"route,omitempty"`
	Boundary            []string              `json:"boundary,omitempty" yaml:"boundary,omitempty"`
	Tier                int                   `json:"tier,omitempty" yaml:"tier,omitempty"`
	// SkillPath is the per-skill on-disk anchor ApplySkillManifest uses to
	// stamp Node.Source via skill_source_doctor. Loaders populate it where
	// they know the absolute path to the skill's SKILL.md (currently
	// LoadOSSkillManifest). When SkillPath is empty, ApplySkillManifest does
	// NOT stamp Source — the node stays in the pre-slice-2 state
	// (Source=nil → ComputeReachability treats it the same as a node that
	// has a connection surface). Raw YAML manifests get SkillPath from the
	// YAML itself; brain_recipe_tree doesn't carry per-skill paths so no
	// stamping happens for those imports (recorded as a deliberate limit
	// in the report — next slice's follow-up).
	SkillPath string `json:"skill_path,omitempty" yaml:"skill_path,omitempty"`
}

type SkillManifestCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type SkillManifestReport struct {
	ManifestID string               `json:"manifest_id"`
	Status     string               `json:"status"`
	Checks     []SkillManifestCheck `json:"checks"`
	NodeIDs    []string             `json:"node_ids"`
	EdgeIDs    []string             `json:"edge_ids"`
}

func LoadSkillManifest(path string) (SkillManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SkillManifest{}, err
	}
	var manifest SkillManifest
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&manifest); err != nil {
		return SkillManifest{}, err
	}
	return manifest, nil
}

func ValidateSkillManifest(manifest SkillManifest) SkillManifestReport {
	r := SkillManifestReport{ManifestID: manifest.ID, Status: "ok"}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			r.Status = "fail"
		}
		r.Checks = append(r.Checks, SkillManifestCheck{Name: name, Status: status, Detail: detail})
	}
	add("schema_version", manifest.SchemaVersion == SkillManifestSchemaVersion, manifest.SchemaVersion)
	add("id", strings.TrimSpace(manifest.ID) != "", manifest.ID)
	add("adapter", strings.TrimSpace(manifest.AdapterID) != "" && strings.TrimSpace(manifest.SourceSystem) != "", manifest.AdapterID+"/"+manifest.SourceSystem)
	add("observed_at", skillObservedAtOK(manifest.ObservedAt), manifest.ObservedAt)
	add("source_ref", skillSourceRefOK(manifest.SourceRef), manifest.SourceRef.Path)
	add("evidence", len(manifest.EvidenceRefs) > 0, strings.Join(manifest.EvidenceRefs, ","))
	add("skills", len(manifest.Skills) > 0, fmt.Sprintf("%d", len(manifest.Skills)))
	seen := map[string]bool{}
	for _, skill := range manifest.Skills {
		kind := skillKind(skill)
		add("skill:"+skill.ID, skillOK(skill, manifest.EvidenceRefs), kind)
		add("skill_kind:"+skill.ID, contains(NodeKinds, kind), kind)
		add("skill_status:"+skill.ID, skillStatusOK(skill.Status), skill.Status)
		if kind == "skill" {
			add("skill_when:"+skill.ID, skillWhenOK(skill.When), "known dimensions with nonempty values")
		}
		if kind == "instruction_rule" {
			add("instruction_rule:"+skill.ID, instructionRuleEntryOK(skill), "when/read/route/boundary/tier")
		}
		if seen[skill.ID] {
			add("duplicate:"+skill.ID, false, "duplicate skill id")
		}
		seen[skill.ID] = true
	}
	return r
}

// ApplySkillManifest is the governed commit boundary for importing a skill
// manifest. It validates the manifest, mints an ActionIntent, obtains a
// PolicyDecision, records the governance pair, and only then appends nodes and
// edges. A manifest that would resurrect a previously retired node is refused
// before any state change.
func ApplySkillManifest(homePath string, manifest SkillManifest, now time.Time) (SkillManifestReport, error) {
	report := ValidateSkillManifest(manifest)
	if report.Status != "ok" {
		return report, fmt.Errorf("skill manifest invalid")
	}

	store, err := Open(homePath)
	if err != nil {
		return report, err
	}
	existingNodes, err := store.loadNodes()
	if err != nil {
		return report, err
	}
	retired := map[string]bool{}
	for _, n := range existingNodes {
		if n.Retired {
			retired[n.ID] = true
		}
	}

	var nodeIDs, edgeIDs []string
	for _, skill := range manifest.Skills {
		if retired[skill.ID] && !skillRetired(skill) {
			return report, fmt.Errorf("skill manifest would resurrect retired node %s; use a new identity or an explicit lifecycle review", skill.ID)
		}
		nodeIDs = append(nodeIDs, skill.ID)
		for _, edge := range skillEdges(skill, manifest.ID, now) {
			edgeIDs = append(edgeIDs, edge.Kind+":"+edge.From+"->"+edge.To)
		}
	}
	// A surfaces_with edge to a harness node that does not exist is not fatal
	// (the filter compares ids, so the skill still surfaces for that harness
	// when asked), but it is how the dashed-spelling drift stayed invisible
	// for six weeks. Say so in the report.
	if detail := missingHarnessNodes(existingNodes, manifest, nodeIDs, now); detail != "" {
		report.Checks = append(report.Checks, SkillManifestCheck{Name: "harness_nodes", Status: "warn", Detail: detail})
	}

	mutation := capabilityGraphMutation{
		kind:       "apply_skill_manifest",
		nodeIDs:    nodeIDs,
		edgeIDs:    edgeIDs,
		manifestID: manifest.ID,
		apply: func(s *Store) error {
			for _, skill := range manifest.Skills {
				node := Node{
					ID:       skill.ID,
					Kind:     skillKind(skill),
					Name:     skill.Name,
					Triggers: skillTriggers(skill),
					Weight:   skill.Weight,
					Retired:  skillRetired(skill),
					When:     cleanOptionalInstructionWhen(skill.When),
					Read:     cleanOptionalStrings(skill.Read),
					Route:    cleanOptionalInstructionRoutes(skill.Route),
					Boundary: cleanOptionalStrings(skill.Boundary),
					Tier:     skill.Tier,
				}
				// Slice-2 reachability stamp: where the manifest carries a
				// SkillPath (currently LoadOSSkillManifest only; brain_recipe_tree
				// has no per-skill dirs, by design), run the doctor on the parent
				// dir + this dir's basename so a dangling symlink or absent dir
				// makes Node.Source.Reachable=false WITHIN ONE IMPORT. A nil
				// Source means "no disk evidence at import time"; ComputeReachability
				// treats it the same as a pre-slice-2 node (reachable iff a
				// connection surface exists).
				if p := strings.TrimSpace(skill.SkillPath); p != "" {
					root := filepath.Dir(p)
					rel := filepath.Base(p)
					rep := DoctorSkillSourceEntry(root, rel)
					node.Source = &NodeSource{
						Root:      root,
						Path:      p,
						Reachable: rep.Reachable,
						Reason:    rep.Reason,
						CheckedAt: now.UTC().Format(time.RFC3339),
					}
				}
				if err := s.Add(node); err != nil {
					return err
				}
				report.NodeIDs = append(report.NodeIDs, skill.ID)
				for _, edge := range skillEdges(skill, manifest.ID, now) {
					if err := s.AddEdge(edge, now); err != nil {
						return err
					}
					report.EdgeIDs = append(report.EdgeIDs, edge.Kind+":"+edge.From+"->"+edge.To)
				}
			}
			return nil
		},
	}
	if err := applyGovernedCapabilityGraphMutation(homePath, mutation, now); err != nil {
		report.NodeIDs = nil
		report.EdgeIDs = nil
		return report, err
	}
	return report, nil
}

func skillSourceRefOK(ref SkillSourceRef) bool {
	return strings.TrimSpace(ref.Path) != "" &&
		strings.TrimSpace(ref.Kind) != "" &&
		strings.TrimSpace(ref.Owner) != "" &&
		strings.TrimSpace(ref.SourceClass) != "" &&
		strings.TrimSpace(ref.Revision) != ""
}

func skillObservedAtOK(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339, v)
	return err == nil
}

// Validate before normalization: dropping a malformed clause would turn a
// restricted skill into an unrestricted offer. No conditions remains compatible.
func skillWhenOK(conditions []InstructionRuleWhen) bool {
	for _, when := range conditions {
		if !instructionRuleWhenKindOK(when.Kind) || len(cleanStrings(when.Values)) == 0 {
			return false
		}
	}
	return true
}

func instructionRuleEntryOK(skill SkillManifestEntry) bool {
	if skill.Tier <= 0 || len(cleanInstructionWhen(skill.When)) == 0 || len(cleanStrings(skill.Read)) == 0 || len(cleanInstructionRoutes(skill.Route)) == 0 || len(cleanStrings(skill.Boundary)) == 0 {
		return false
	}
	for _, when := range cleanInstructionWhen(skill.When) {
		if !instructionRuleWhenKindOK(when.Kind) {
			return false
		}
	}
	return true
}

func instructionRuleWhenKindOK(kind string) bool {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "task-class", "repo-kind", "harness", "file-path", "phase":
		return true
	default:
		return false
	}
}

func skillOK(skill SkillManifestEntry, manifestEvidence []string) bool {
	return strings.TrimSpace(skill.ID) != "" &&
		strings.TrimSpace(skill.Name) != "" &&
		contains(NodeKinds, skillKind(skill)) &&
		skillStatusOK(skill.Status) &&
		len(skillTriggers(skill)) > 0 &&
		(len(skill.EvidenceRefs) > 0 || len(manifestEvidence) > 0 || strings.TrimSpace(skill.Status) != "")
}

func skillStatusOK(status string) bool {
	switch strings.TrimSpace(status) {
	case "", "active", "needs_review", "deprecated", "retired":
		return true
	default:
		return false
	}
}

func skillRetired(skill SkillManifestEntry) bool {
	switch strings.TrimSpace(skill.Status) {
	case "needs_review", "deprecated", "retired":
		return true
	default:
		return skill.Retired
	}
}

func skillKind(skill SkillManifestEntry) string {
	if strings.TrimSpace(skill.Kind) == "" {
		return "skill"
	}
	return strings.TrimSpace(skill.Kind)
}

func skillTriggers(skill SkillManifestEntry) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(strings.ToLower(v))
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range skill.Triggers {
		add(v)
	}
	for _, v := range skill.Capabilities {
		add(v)
	}
	for _, when := range skill.When {
		add(when.Kind)
		for _, v := range when.Values {
			add(v)
		}
	}
	return out
}

func cleanOptionalInstructionWhen(values []InstructionRuleWhen) []InstructionRuleWhen {
	out := cleanInstructionWhen(values)
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanOptionalInstructionRoutes(values []InstructionRoute) []InstructionRoute {
	out := cleanInstructionRoutes(values)
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanInstructionWhen(values []InstructionRuleWhen) []InstructionRuleWhen {
	out := make([]InstructionRuleWhen, 0, len(values))
	for _, value := range values {
		kind := strings.TrimSpace(strings.ToLower(value.Kind))
		var cleaned []string
		for _, raw := range value.Values {
			raw = strings.TrimSpace(raw)
			if raw != "" {
				cleaned = append(cleaned, raw)
			}
		}
		if kind != "" && len(cleaned) > 0 {
			out = append(out, InstructionRuleWhen{Kind: kind, Values: cleaned})
		}
	}
	return out
}

func cleanInstructionRoutes(values []InstructionRoute) []InstructionRoute {
	out := make([]InstructionRoute, 0, len(values))
	for _, value := range values {
		output := strings.TrimSpace(value.Output)
		destination := strings.TrimSpace(value.Destination)
		if output != "" && destination != "" {
			out = append(out, InstructionRoute{Output: output, Destination: destination})
		}
	}
	return out
}

func cleanOptionalStrings(values []string) []string {
	out := cleanStrings(values)
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// skillEdges derives the typed edges a manifest entry declares. Every edge
// carries "skill_manifest:<manifest id>" as provenance: the live ledger held
// 113 edges to misspelled harness nodes written between 2026-09-13 and
// 2026-10-06 by several importers, and nothing on the edge said which.
func skillEdges(skill SkillManifestEntry, manifestID string, now time.Time) []Edge {
	var edges []Edge
	provenance := "skill_manifest:" + strings.TrimSpace(manifestID)
	add := func(kind, from, to string) {
		to = strings.TrimSpace(to)
		if to == "" {
			return
		}
		edges = append(edges, Edge{Kind: kind, From: from, To: to, Confidence: 1, Provenance: provenance, CreatedAt: now.UTC().Format(time.RFC3339)})
	}
	for _, to := range skill.Requires {
		add(EdRequires, skill.ID, to)
	}
	for _, to := range skill.Specializes {
		add(EdSpecializes, skill.ID, to)
	}
	for _, to := range skill.ConflictsWith {
		add(EdConflictsWith, skill.ID, to)
	}
	for _, harness := range skill.CompatibleHarnesses {
		add(EdSurfacesWith, skill.ID, skillHarnessID(harness))
	}
	for _, to := range skill.ValidatedBy {
		add(EdValidatedBy, skill.ID, to)
	}
	for _, old := range skill.ReplacementFor {
		add(EdRetiredBy, old, skill.ID)
	}
	return edges
}

// skillHarnessID writes a compatible_harnesses entry with the graph's one
// harness spelling (see harnessID). A bundle may say "claude-code"; the node
// is "harness:claude_code".
func skillHarnessID(harness string) string {
	return harnessID(harness)
}

// missingHarnessNodes names the harness nodes a manifest's compatible_harnesses
// point at that exist neither in the graph nor in the manifest itself.
func missingHarnessNodes(existing []Node, manifest SkillManifest, manifestNodeIDs []string, now time.Time) string {
	known := map[string]bool{}
	for _, n := range existing {
		if !n.Retired {
			known[n.ID] = true
		}
	}
	for _, id := range manifestNodeIDs {
		known[id] = true
	}
	missing := map[string][]string{}
	for _, skill := range manifest.Skills {
		for _, edge := range skillEdges(skill, manifest.ID, now) {
			if edge.Kind != EdSurfacesWith || !strings.HasPrefix(edge.To, "harness:") || known[edge.To] {
				continue
			}
			missing[edge.To] = appendUniqueString(missing[edge.To], edge.From)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	targets := make([]string, 0, len(missing))
	for to := range missing {
		targets = append(targets, to)
	}
	sort.Strings(targets)
	parts := make([]string, 0, len(targets))
	for _, to := range targets {
		parts = append(parts, fmt.Sprintf("%s (%d skill(s), e.g. %s)", to, len(missing[to]), missing[to][0]))
	}
	return "no harness node for: " + strings.Join(parts, "; ")
}
