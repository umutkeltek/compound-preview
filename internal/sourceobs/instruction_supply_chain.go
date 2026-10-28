package sourceobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	instructionSupplyChainSurfaceOverlay        = "instruction_supply_chain_surface"
	instructionSupplyChainSchemaOverlay         = "instruction_supply_chain_schema"
	instructionSupplyChainSchemaRevisionOverlay = "instruction_supply_chain_schema_revision"
	instructionSupplyChainSchemaRevision        = "compound.instruction_structure.v0.1"
)

// CanonAdmissionHold is a deterministic source-boundary violation. It is
// emitted only when an admitted instruction/control surface changes shape,
// disappears, or becomes unparsable. Plain value/content rotation remains an
// observed canon drift and does not create operator babysitting.
type CanonAdmissionHold struct {
	SourceRef   string `json:"source_ref"`
	Surface     string `json:"surface"`
	Reason      string `json:"reason"`
	PriorSchema string `json:"prior_schema,omitempty"`
	LiveSchema  string `json:"live_schema,omitempty"`
}

func instructionSupplyChainOverlays(sourcePath string, data []byte) ([]SourceStructureOverlay, error) {
	surface := inferInstructionSupplyChainSurface(sourcePath)
	if surface == "" {
		return nil, nil
	}
	syntax := inferSourceKind(sourcePath)
	shape, err := instructionSupplyChainShape(syntax, data)
	if err != nil {
		return nil, fmt.Errorf("%s %s schema: %w", surface, sourcePath, err)
	}
	sum := sha256.Sum256([]byte(strings.Join(shape, "\n")))
	return []SourceStructureOverlay{
		{
			Kind:   instructionSupplyChainSurfaceOverlay,
			Count:  1,
			Detail: surface,
		},
		{
			Kind:   instructionSupplyChainSchemaOverlay,
			Count:  len(shape),
			Detail: "sha256:" + hex.EncodeToString(sum[:]),
		},
		{
			Kind:   instructionSupplyChainSchemaRevisionOverlay,
			Count:  1,
			Detail: instructionSupplyChainSchemaRevision,
		},
	}, nil
}

func inferInstructionSupplyChainSurface(sourcePath string) string {
	normalized := strings.ToLower(filepath.ToSlash(sourcePath))
	base := strings.ToLower(filepath.Base(sourcePath))
	switch base {
	case "agents.md":
		return "agents"
	case "claude.md":
		return "claude"
	case "skill.md":
		return "skill"
	}
	if strings.Contains(normalized, "/mcp") ||
		strings.Contains(base, "mcp") {
		return "mcp_manifest"
	}
	if strings.Contains(normalized, "/plugins/") ||
		strings.Contains(normalized, "/.codex/plugins/") ||
		strings.Contains(normalized, "/.claude-plugin/") ||
		base == "plugin.json" ||
		base == "plugin.yaml" ||
		base == "plugin.yml" {
		return "plugin"
	}
	return ""
}

func instructionSupplyChainShape(syntax string, data []byte) ([]string, error) {
	switch syntax {
	case "markdown":
		return markdownInstructionShape(string(data)), nil
	case "json":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("multiple JSON values")
			}
			return nil, fmt.Errorf("trailing JSON content: %w", err)
		}
		return structuredInstructionShape(value), nil
	case "yaml":
		var value any
		if err := yaml.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return structuredInstructionShape(value), nil
	default:
		return nil, fmt.Errorf("unsupported instruction manifest syntax %q", syntax)
	}
}

func markdownInstructionShape(text string) []string {
	shape := []string{"document:markdown"}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end > 0 {
			var frontmatter any
			if yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &frontmatter) == nil {
				for _, row := range structuredInstructionShape(frontmatter) {
					shape = append(shape, "frontmatter."+row)
				}
			} else {
				shape = append(shape, "frontmatter:invalid")
			}
		}
	}
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			language := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
			if !inFence {
				shape = append(shape, "fence:"+language)
			}
			inFence = !inFence
			continue
		}
		level := markdownHeadingLevel(line)
		if level > 0 && !inFence {
			shape = append(shape, "heading:"+strconv.Itoa(level))
		}
	}
	return shape
}

func markdownHeadingLevel(line string) int {
	trimmed := strings.TrimLeft(line, " \t")
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0
	}
	return level
}

func structuredInstructionShape(value any) []string {
	rows := []string{}
	collectInstructionShape("$", value, &rows)
	sort.Strings(rows)
	return rows
}

func collectInstructionShape(path string, value any, rows *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		*rows = append(*rows, path+":object")
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectInstructionShape(path+"."+key, typed[key], rows)
		}
	case map[any]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[fmt.Sprint(key)] = item
		}
		collectInstructionShape(path, normalized, rows)
	case []any:
		*rows = append(*rows, path+":array")
		seen := map[string]struct{}{}
		for _, item := range typed {
			itemRows := []string{}
			collectInstructionShape(path+"[]", item, &itemRows)
			for _, row := range itemRows {
				seen[row] = struct{}{}
			}
		}
		for row := range seen {
			*rows = append(*rows, row)
		}
	case nil:
		*rows = append(*rows, path+":null")
	case bool:
		*rows = append(*rows, path+":bool")
	case json.Number, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		*rows = append(*rows, path+":number")
	default:
		*rows = append(*rows, path+":string")
	}
}

func instructionSupplyChainSurface(overlays []SourceStructureOverlay) string {
	return instructionSupplyChainOverlayDetail(overlays, instructionSupplyChainSurfaceOverlay)
}

func instructionSupplyChainSchema(overlays []SourceStructureOverlay) string {
	return instructionSupplyChainOverlayDetail(overlays, instructionSupplyChainSchemaOverlay)
}

func instructionSupplyChainStructureRevision(overlays []SourceStructureOverlay) string {
	return instructionSupplyChainOverlayDetail(overlays, instructionSupplyChainSchemaRevisionOverlay)
}

func instructionSupplyChainOverlayDetail(overlays []SourceStructureOverlay, kind string) string {
	for _, overlay := range overlays {
		if overlay.Kind == kind {
			return strings.TrimSpace(overlay.Detail)
		}
	}
	return ""
}

func compareInstructionSupplyChainStructure(
	sourceRef string,
	baseline []SourceStructureOverlay,
	live []SourceStructureOverlay,
	liveErr error,
) *CanonAdmissionHold {
	priorSurface := instructionSupplyChainSurface(baseline)
	if priorSurface == "" {
		return nil
	}
	priorSchema := instructionSupplyChainSchema(baseline)
	priorRevision := instructionSupplyChainStructureRevision(baseline)
	if liveErr != nil {
		return &CanonAdmissionHold{
			SourceRef:   sourceRef,
			Surface:     priorSurface,
			Reason:      "instruction surface became unparsable",
			PriorSchema: priorSchema,
			LiveSchema:  "invalid",
		}
	}
	liveSurface := instructionSupplyChainSurface(live)
	liveSchema := instructionSupplyChainSchema(live)
	liveRevision := instructionSupplyChainStructureRevision(live)
	switch {
	case liveSurface != priorSurface:
		return &CanonAdmissionHold{
			SourceRef:   sourceRef,
			Surface:     priorSurface,
			Reason:      "instruction surface lineage changed",
			PriorSchema: priorSchema,
			LiveSchema:  liveSchema,
		}
	case priorRevision == "" || liveRevision == "" || priorRevision != liveRevision:
		return &CanonAdmissionHold{
			SourceRef:   sourceRef,
			Surface:     priorSurface,
			Reason:      "instruction schema revision changed or missing",
			PriorSchema: priorSchema,
			LiveSchema:  liveSchema,
		}
	case priorSchema == "" || liveSchema == "":
		return &CanonAdmissionHold{
			SourceRef:   sourceRef,
			Surface:     priorSurface,
			Reason:      "instruction surface schema attestation missing",
			PriorSchema: priorSchema,
			LiveSchema:  liveSchema,
		}
	case priorSchema != liveSchema:
		return &CanonAdmissionHold{
			SourceRef:   sourceRef,
			Surface:     priorSurface,
			Reason:      "instruction surface schema changed",
			PriorSchema: priorSchema,
			LiveSchema:  liveSchema,
		}
	default:
		return nil
	}
}
