package tooltopology

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"compound/internal/capgraph"
)

const BoundaryDoctorSchemaVersion = "compound.tool_boundary_doctor.v0.1"

type BoundaryDoctorReport struct {
	SchemaVersion         string            `json:"schema_version"`
	AuthorityRef          string            `json:"authority_ref"`
	Status                string            `json:"status"`
	NodeCount             int               `json:"node_count"`
	MCPCount              int               `json:"mcp_count"`
	ToolCount             int               `json:"tool_count"`
	PluginAdapterCount    int               `json:"plugin_adapter_count"`
	RetiredPluginAdapters int               `json:"retired_plugin_adapters"`
	PluginSurfaceCount    int               `json:"plugin_surface_count"`
	StaleSourceCount      int               `json:"stale_source_count"`
	Checks                []Check           `json:"checks"`
	SourceFreshness       []SourceFreshness `json:"source_freshness,omitempty"`
	BoundaryNotes         []string          `json:"boundary_notes"`
}

type SourceFreshness struct {
	SourceAuthorityID string `json:"source_authority_id"`
	SourceClass       string `json:"source_class"`
	SourceKind        string `json:"source_kind"`
	SourcePath        string `json:"source_path"`
	ImportedAt        string `json:"imported_at"`
	SourceObservedAt  string `json:"source_observed_at"`
	AgeHours          int    `json:"age_hours"`
	MaxAgeHours       int    `json:"max_age_hours,omitempty"`
	Status            string `json:"status"`
	RefreshCommand    string `json:"refresh_command,omitempty"`
}

func DoctorBoundary(homePath string) (BoundaryDoctorReport, error) {
	report := BoundaryDoctorReport{
		SchemaVersion: BoundaryDoctorSchemaVersion,
		AuthorityRef:  "compound://registries/capability-graph",
		Status:        "ok",
		BoundaryNotes: []string{
			"mcporter and plugin inventories are observed evidence; they do not execute tools or mutate MCP/plugin configs",
			"adapter:plugin:* nodes must remain retired/non-surfacing until a separate reviewed tool contract promotes a concrete owner",
		},
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		return report, err
	}
	nodes, err := store.List()
	if err != nil {
		return report, err
	}
	report.NodeCount = len(nodes)
	for _, node := range nodes {
		switch node.Kind {
		case "mcp":
			report.MCPCount++
		case "tool":
			report.ToolCount++
		case "adapter":
			if strings.HasPrefix(node.ID, "adapter:plugin:") {
				report.PluginAdapterCount++
				if node.Retired || node.EvidenceOnly {
					report.RetiredPluginAdapters++
				}
			}
		}
	}
	surfaced, err := store.Surface("plugin marketplace mcp hook adapter")
	if err != nil {
		return report, err
	}
	for _, hit := range surfaced {
		if strings.HasPrefix(hit.Node.ID, "adapter:plugin:") {
			report.PluginSurfaceCount++
		}
	}
	report.addCheck("capability_graph_present", report.NodeCount > 0, intDetail(report.NodeCount))
	report.addCheck("mcp_nodes_observed", report.MCPCount > 0, intDetail(report.MCPCount))
	report.addCheck("tool_nodes_observed", report.ToolCount > 0, intDetail(report.ToolCount))
	report.addCheck("plugin_adapters_retired", report.PluginAdapterCount == report.RetiredPluginAdapters, intDetail(report.RetiredPluginAdapters)+"/"+intDetail(report.PluginAdapterCount))
	report.addCheck("plugin_adapters_not_surfacing", report.PluginSurfaceCount == 0, intDetail(report.PluginSurfaceCount))
	if err := report.addSourceFreshness(homePath, time.Now().UTC()); err != nil {
		return report, err
	}
	return report, nil
}

func (r *BoundaryDoctorReport) addCheck(name string, ok bool, detail string) {
	status := "ok"
	if !ok {
		status = "fail"
	}
	r.addCheckStatus(name, status, detail)
}

func (r *BoundaryDoctorReport) addCheckStatus(name, status, detail string) {
	switch status {
	case "fail":
		r.Status = "fail"
	case "warn":
		if r.Status == "ok" {
			r.Status = "warn"
		}
	}
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

func (r *BoundaryDoctorReport) addSourceFreshness(homePath string, now time.Time) error {
	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	var staleDetails []string
	for _, record := range records {
		maxAgeHours, refreshCommand, watched := sourceFreshnessPolicy(record)
		observedAt := strings.TrimSpace(record.SourceObservedAt)
		if observedAt == "" {
			observedAt = record.CreatedAt
		}
		observedTime, err := time.Parse(time.RFC3339, observedAt)
		if err != nil {
			item := sourceFreshnessItem(record, observedAt, 0, maxAgeHours, "warn", refreshCommand)
			r.SourceFreshness = append(r.SourceFreshness, item)
			r.StaleSourceCount++
			staleDetails = append(staleDetails, fmt.Sprintf("%s invalid source time %q refresh=%s", record.ID, observedAt, refreshCommand))
			continue
		}
		ageHours := int(now.Sub(observedTime.UTC()).Hours())
		if ageHours < 0 {
			ageHours = 0
		}
		status := "ok"
		if !watched {
			status = "exempt"
		} else if ageHours > maxAgeHours {
			status = "warn"
			r.StaleSourceCount++
			staleDetails = append(staleDetails, fmt.Sprintf("%s %s age=%dh max=%dh refresh=%s", record.ID, record.SourceClass, ageHours, maxAgeHours, refreshCommand))
		}
		r.SourceFreshness = append(r.SourceFreshness, sourceFreshnessItem(record, observedTime.UTC().Format(time.RFC3339), ageHours, maxAgeHours, status, refreshCommand))
	}
	if r.StaleSourceCount > 0 {
		r.addCheckStatus("source_freshness", "warn", strings.Join(staleDetails, "; "))
		return nil
	}
	r.addCheckStatus("source_freshness", "ok", fmt.Sprintf("%d source record(s)", len(records)))
	return nil
}

func sourceFreshnessItem(record capgraph.SourceAuthorityRecord, observedAt string, ageHours, maxAgeHours int, status, refreshCommand string) SourceFreshness {
	return SourceFreshness{
		SourceAuthorityID: record.ID,
		SourceClass:       record.SourceClass,
		SourceKind:        record.SourceKind,
		SourcePath:        record.SourcePath,
		ImportedAt:        record.CreatedAt,
		SourceObservedAt:  observedAt,
		AgeHours:          ageHours,
		MaxAgeHours:       maxAgeHours,
		Status:            status,
		RefreshCommand:    refreshCommand,
	}
}

func sourceFreshnessPolicy(record capgraph.SourceAuthorityRecord) (int, string, bool) {
	switch record.SourceClass {
	case "mcporter-json":
		return 7 * 24, "mcporter list --json > ~/.compound/imports/mcporter-list-$(date +%Y%m%dT%H%M%SZ).json && compound capability import-topology --mcporter-json ~/.compound/imports/mcporter-list-<ts>.json", true
	case "os-tool-resolver-json":
		return 14 * 24, "refresh source owner, then compound capability import-topology --os-resolver-json " + record.SourcePath, true
	case "os-skill-manifest-json":
		return 14 * 24, "refresh source owner, then compound capability import-skills --os-skill-json " + record.SourcePath, true
	case "os-plugin-serving-board-json":
		return 14 * 24, "refresh source owner, then compound capability import-skills --os-plugin-board-json " + record.SourcePath, true
	case "harness-canon-file":
		return 14 * 24, "compound source canon-observe --family harness-canon", true
	default:
		return 0, "", false
	}
}

func intDetail(value int) string {
	return strconv.Itoa(value)
}
