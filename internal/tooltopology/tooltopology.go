// Package tooltopology imports observed tool/MCP topology into the capability
// graph. It is deterministic adapter code: it makes tools legible and routable,
// but never executes them and never mutates the source owner.
package tooltopology

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/capgraph"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = "compound.tool_topology.v0.1"

type Topology struct {
	SchemaVersion   string           `json:"schema_version" yaml:"schema_version"`
	ID              string           `json:"id" yaml:"id"`
	AdapterID       string           `json:"adapter_id" yaml:"adapter_id"`
	SourceSystem    string           `json:"source_system" yaml:"source_system"`
	ObservedAt      string           `json:"observed_at" yaml:"observed_at"`
	SourceRef       SourceRef        `json:"source_ref" yaml:"source_ref"`
	Entries         []Entry          `json:"entries" yaml:"entries"`
	Recommendations []Recommendation `json:"recommendations,omitempty" yaml:"recommendations,omitempty"`
	EvidenceRefs    []string         `json:"evidence_refs" yaml:"evidence_refs"`
}

type SourceRef struct {
	Path        string `json:"path" yaml:"path"`
	Kind        string `json:"kind" yaml:"kind"`
	Owner       string `json:"owner" yaml:"owner"`
	SourceClass string `json:"source_class" yaml:"source_class"`
	Revision    string `json:"revision" yaml:"revision"`
}

type Entry struct {
	ID           string   `json:"id" yaml:"id"`
	Kind         string   `json:"kind" yaml:"kind"`
	Name         string   `json:"name" yaml:"name"`
	Interface    string   `json:"interface" yaml:"interface"`
	Status       string   `json:"status" yaml:"status"`
	Triggers     []string `json:"triggers" yaml:"triggers"`
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Constraints  []string `json:"constraints,omitempty" yaml:"constraints,omitempty"`
	Weight       float64  `json:"weight,omitempty" yaml:"weight,omitempty"`
	Retired      bool     `json:"retired,omitempty" yaml:"retired,omitempty"`
	// Health is the observed operational state when it is not fine. It is kept
	// apart from Retired because an adapter observing a source can only ever
	// learn health -- whether something is WITHDRAWN is a decision the source
	// makes explicitly, never something to infer from a failed probe.
	Health       string   `json:"health,omitempty" yaml:"health,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
	// Connection carries the access facts an adapter already read, so Apply can
	// mint a capgraph ConnectionSurface instead of discarding them. Before this
	// existed the adapters flattened transport and config-path facts into free
	// strings in Constraints ("transport:stdio"), which preserved the CLASS of
	// access and threw away the ADDRESS -- leaving every tool and MCP node named
	// but unreachable, and every consuming harness to infer an invocation.
	// Optional: an entry with no access facts leaves it nil and mints nothing.
	Connection *EntryConnection `json:"connection,omitempty" yaml:"connection,omitempty"`
}

// EntryConnection is the "how do I reach this entry" half of a topology entry.
// It is a narrow projection of capgraph.ConnectionSurface on purpose: adapters
// supply observed access facts only, and never credential material -- the
// capgraph write door refuses value-shaped input, and this type gives an adapter
// no field to put one in.
type EntryConnection struct {
	ConfigPaths   []string `json:"config_paths,omitempty" yaml:"config_paths,omitempty"`
	Endpoint      string   `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	LaunchCommand string   `json:"launch_command,omitempty" yaml:"launch_command,omitempty"`
	AuthMode      string   `json:"auth_mode,omitempty" yaml:"auth_mode,omitempty"`
}

// hasFacts reports whether this connection carries anything worth persisting.
// An all-empty connection mints no surface, so a node never becomes "reachable"
// on the strength of an empty record -- that would move the reachability number
// without moving the reality it measures.
func (c *EntryConnection) hasFacts() bool {
	if c == nil {
		return false
	}
	return len(c.ConfigPaths) > 0 ||
		strings.TrimSpace(c.Endpoint) != "" ||
		strings.TrimSpace(c.LaunchCommand) != "" ||
		strings.TrimSpace(c.AuthMode) != ""
}

type Recommendation struct {
	Primary    string  `json:"primary" yaml:"primary"`
	Fallback   string  `json:"fallback" yaml:"fallback"`
	Reason     string  `json:"reason" yaml:"reason"`
	Confidence float64 `json:"confidence,omitempty" yaml:"confidence,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	TopologyID string   `json:"topology_id"`
	Status     string   `json:"status"`
	Checks     []Check  `json:"checks"`
	NodeIDs    []string `json:"node_ids"`
	EdgeIDs    []string `json:"edge_ids"`
	// ConnectionSurfaceIDs names the entries that arrived with access facts, and
	// ConnectionSurfacesAdded counts the ones that were NEW. The two differ on a
	// re-import with unchanged sources (every id present, zero added), which is
	// the signal that distinguishes "this door is idempotent" from "this door did
	// nothing" -- indistinguishable if only one number were reported.
	ConnectionSurfaceIDs    []string `json:"connection_surface_ids,omitempty"`
	ConnectionSurfacesAdded int      `json:"connection_surfaces_added"`
}

func Load(path string) (Topology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Topology{}, err
	}
	var topo Topology
	if err := yaml.Unmarshal(b, &topo); err != nil {
		return Topology{}, err
	}
	return topo, nil
}

func Validate(topo Topology) Report {
	r := Report{TopologyID: topo.ID, Status: "ok"}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			r.Status = "fail"
		}
		r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
	}
	add("schema", topo.SchemaVersion == SchemaVersion, topo.SchemaVersion)
	add("identity", strings.TrimSpace(topo.ID) != "" && strings.TrimSpace(topo.AdapterID) != "" && strings.TrimSpace(topo.SourceSystem) != "", topo.ID)
	add("source_ref", strings.TrimSpace(topo.SourceRef.Path) != "" && strings.TrimSpace(topo.SourceRef.Kind) != "" && strings.TrimSpace(topo.SourceRef.Owner) != "" && strings.TrimSpace(topo.SourceRef.SourceClass) != "" && strings.TrimSpace(topo.SourceRef.Revision) != "", topo.SourceRef.Path)
	add("evidence", len(topo.EvidenceRefs) > 0, strings.Join(topo.EvidenceRefs, ","))
	add("entries", len(topo.Entries) > 0, fmt.Sprintf("%d", len(topo.Entries)))

	ids := map[string]bool{}
	for _, e := range topo.Entries {
		add("entry:"+e.ID, entryOK(e), e.Kind)
		add("entry_kind:"+e.ID, contains(capgraph.NodeKinds, e.Kind), e.Kind)
		ids[e.ID] = true
	}
	for _, rec := range topo.Recommendations {
		add("recommendation:"+rec.Fallback+"->"+rec.Primary, strings.TrimSpace(rec.Primary) != "" && strings.TrimSpace(rec.Fallback) != "" && ids[rec.Primary] && ids[rec.Fallback], rec.Reason)
	}
	return r
}

func Apply(homePath string, topo Topology, now time.Time) (Report, error) {
	report := Validate(topo)
	if report.Status != "ok" {
		return report, fmt.Errorf("tool topology invalid")
	}

	var nodes []capgraph.Node
	var edges []capgraph.Edge
	var surfaces []capgraph.ConnectionSurface
	for _, e := range topo.Entries {
		n := capgraph.Node{
			ID:       e.ID,
			Kind:     e.Kind,
			Name:     e.Name,
			Triggers: nodeTriggers(e),
			Weight:   e.Weight,
			// Only genuinely withdrawn states retire a node. A transient health
			// state must never reach this field: retirement removes the node from
			// surfacing, and a capability deleted because a probe failed cannot
			// report that it is missing.
			Retired: e.Retired || e.Status == "retired" || e.Status == "deprecated",
			Health:  e.Health,
		}
		nodes = append(nodes, n)
		report.NodeIDs = append(report.NodeIDs, e.ID)
		if !e.Connection.hasFacts() {
			continue
		}
		// Fail closed. A rejected surface means the adapter produced something the
		// capgraph write door refuses (credential-shaped input is the case that
		// matters), and importing the NAME while silently dropping the ACCESS is
		// precisely the failure this whole change exists to remove.
		surfaces = append(surfaces, capgraph.ConnectionSurface{
			NodeID:         e.ID,
			ConfigPaths:    e.Connection.ConfigPaths,
			Endpoint:       e.Connection.Endpoint,
			LaunchCommand:  e.Connection.LaunchCommand,
			AuthMode:       e.Connection.AuthMode,
			AssertingOwner: topo.SourceSystem,
			SourceRevision: topo.SourceRef.Revision,
		})
		report.ConnectionSurfaceIDs = append(report.ConnectionSurfaceIDs, e.ID)
	}
	for _, rec := range topo.Recommendations {
		conf := rec.Confidence
		if conf == 0 {
			conf = 1.0
		}
		edges = append(edges, capgraph.Edge{
			Kind:       capgraph.EdFallbackFor,
			From:       rec.Fallback,
			To:         rec.Primary,
			Confidence: conf,
			CreatedAt:  now.UTC().Format(time.RFC3339),
		})
		report.EdgeIDs = append(report.EdgeIDs, capgraph.EdFallbackFor+":"+rec.Fallback+"->"+rec.Primary)
	}

	if len(nodes) > 0 || len(edges) > 0 {
		if err := capgraph.ApplyNodesAndEdgesGoverned(homePath, nodes, edges, now); err != nil {
			return report, err
		}
	}
	for _, cs := range surfaces {
		added, err := capgraph.AddConnectionSurfaceGoverned(homePath, cs, now)
		if err != nil {
			return report, fmt.Errorf("connection surface for %s: %w", cs.NodeID, err)
		}
		if added {
			report.ConnectionSurfacesAdded++
		}
	}
	return report, nil
}

func entryOK(e Entry) bool {
	return strings.TrimSpace(e.ID) != "" &&
		strings.TrimSpace(e.Kind) != "" &&
		strings.TrimSpace(e.Name) != "" &&
		strings.TrimSpace(e.Interface) != "" &&
		len(nodeTriggers(e)) > 0 &&
		(len(e.EvidenceRefs) > 0 || strings.TrimSpace(e.Status) != "")
}

func nodeTriggers(e Entry) []string {
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
	for _, v := range e.Triggers {
		add(v)
	}
	for _, v := range e.Capabilities {
		add(v)
	}
	add(e.Interface)
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
