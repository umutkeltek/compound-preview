package tooltopology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// LoadOSResolver adapts a generated tool-resolver JSON view into Compound's
// generic ToolTopology contract. It is read-only: the generated view is
// evidence, never the runtime owner.
func LoadOSResolver(path string, now time.Time) (Topology, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Topology{}, err
	}
	var src osResolverDoc
	if err := json.Unmarshal(data, &src); err != nil {
		return Topology{}, fmt.Errorf("parse OS tool resolver %s: %w", path, err)
	}
	if len(src.Tools) == 0 {
		return Topology{}, fmt.Errorf("OS tool resolver %s has no tools", path)
	}
	if now.IsZero() {
		now = time.Now()
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	evidenceRef := "tool_resolver_json:" + revision[:19]
	topo := Topology{
		SchemaVersion: SchemaVersion,
		ID:            "topo_os_tool_resolver_" + revision[7:19],
		AdapterID:     "os.tool-resolver-json",
		SourceSystem:  "tool-resolver",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SourceRef{
			Path:        path,
			Kind:        "generated_view",
			Owner:       "tool-topology",
			SourceClass: "os-tool-resolver-json",
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}
	sourceToNode := map[string]string{}
	for _, tool := range src.Tools {
		if strings.TrimSpace(tool.ID) == "" {
			continue
		}
		kind := resolverNodeKind(tool)
		id := kind + ":" + slug(tool.ID)
		sourceToNode[tool.ID] = id
		entry := Entry{
			ID:           id,
			Kind:         kind,
			Name:         firstNonEmpty(tool.Title, tool.ID),
			Interface:    resolverInterface(tool),
			Status:       resolverStatus(tool),
			Triggers:     resolverTriggers(tool),
			Capabilities: resolverCapabilities(tool),
			Constraints:  resolverConstraints(tool),
			Weight:       resolverWeight(tool),
			EvidenceRefs: []string{evidenceRef},
		}
		topo.Entries = append(topo.Entries, entry)
	}
	for _, tool := range src.Tools {
		replacement := strings.TrimSpace(tool.Replacement)
		if replacement == "" {
			continue
		}
		fallback := sourceToNode[tool.ID]
		primary := sourceToNode[replacement]
		if fallback == "" || primary == "" {
			continue
		}
		topo.Recommendations = append(topo.Recommendations, Recommendation{
			Primary:    primary,
			Fallback:   fallback,
			Reason:     fmt.Sprintf("source resolver replacement: %s -> %s", tool.ID, replacement),
			Confidence: 0.95,
		})
	}
	return topo, nil
}

type osResolverDoc struct {
	Marker        string         `json:"marker"`
	GeneratedFrom []string       `json:"generated_from"`
	Tools         []osToolRecord `json:"tools"`
}

type osToolRecord struct {
	ID                 string                 `json:"id"`
	Title              string                 `json:"title"`
	Summary            string                 `json:"summary"`
	Topology           string                 `json:"topology"`
	Owner              string                 `json:"owner"`
	MCPorterServer     string                 `json:"mcporter_server"`
	BridgeAllowed      bool                   `json:"bridge_allowed"`
	Replacement        string                 `json:"replacement"`
	Functions          []string               `json:"functions"`
	EffectClass        []string               `json:"effect_class"`
	PreferredModelTier string                 `json:"preferred_model_tier"`
	HostAvailability   map[string]interface{} `json:"host_availability"`
	ActualSurfaces     []string               `json:"actual_surfaces"`
}

func resolverNodeKind(tool osToolRecord) string {
	switch tool.Topology {
	case "gateway_core":
		return "adapter"
	case "mcporter_catalog_on_demand":
		return "mcp"
	default:
		return "tool"
	}
}

func resolverInterface(tool osToolRecord) string {
	switch tool.Topology {
	case "mcporter_catalog_on_demand":
		return "mcp"
	case "host_on_demand_only":
		return "host_on_demand"
	case "heavy_local_runtime":
		return "local_runtime"
	default:
		if tool.MCPorterServer != "" {
			return "mcp"
		}
		return "cli"
	}
}

func resolverStatus(tool osToolRecord) string {
	switch tool.Topology {
	case "legacy_or_last_resort":
		return "fallback"
	case "hot_core", "gateway_core":
		return "active"
	default:
		return "on_demand"
	}
}

func resolverWeight(tool osToolRecord) float64 {
	switch tool.Topology {
	case "hot_core", "gateway_core":
		return 2.0
	case "deterministic_cli":
		return 1.5
	case "legacy_or_last_resort":
		return 0.25
	default:
		return 1.0
	}
}

func resolverTriggers(tool osToolRecord) []string {
	values := []string{tool.ID, tool.Title, tool.Topology, resolverInterface(tool)}
	values = append(values, tool.Functions...)
	values = append(values, tool.ActualSurfaces...)
	return normalizedTokens(values...)
}

func resolverCapabilities(tool osToolRecord) []string {
	values := append([]string{}, tool.Functions...)
	values = append(values, tool.EffectClass...)
	values = append(values, tool.PreferredModelTier)
	return normalizedTokens(values...)
}

func resolverConstraints(tool osToolRecord) []string {
	var values []string
	for host, raw := range tool.HostAvailability {
		values = append(values, host)
		switch v := raw.(type) {
		case string:
			values = append(values, v)
		case bool:
			if v {
				values = append(values, host+"_enabled")
			} else {
				values = append(values, host+"_disabled")
			}
		}
	}
	sort.Strings(values)
	return normalizedTokens(values...)
}

func normalizedTokens(values ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		for _, part := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

func slug(s string) string {
	parts := normalizedTokens(s)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "-")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
