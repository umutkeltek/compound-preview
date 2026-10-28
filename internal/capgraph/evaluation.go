package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const CapabilityEvaluationSetSchemaVersion = "compound.capability_evaluations.v0.1"

type CapabilityEvaluation struct {
	SchemaVersion    int            `json:"schema_version"`
	ID               string         `json:"id"`
	CreatedAt        string         `json:"created_at"`
	SourceReceipt    string         `json:"source_receipt"`
	Capability       string         `json:"capability"`
	Verdict          string         `json:"verdict"`
	Signals          map[string]any `json:"signals,omitempty"`
	RubricScores     map[string]any `json:"rubric_scores,omitempty"`
	RubricNormalized float64        `json:"rubric_normalized,omitempty"`
	LocalStateOnly   bool           `json:"local_state_only"`
}

type CapabilityEvaluationSet struct {
	SchemaVersion string                 `json:"schema_version"`
	ID            string                 `json:"id"`
	AdapterID     string                 `json:"adapter_id"`
	SourceSystem  string                 `json:"source_system"`
	SourcePath    string                 `json:"source_path"`
	SourceKind    string                 `json:"source_kind"`
	Revision      string                 `json:"revision"`
	EvidenceRefs  []string               `json:"evidence_refs"`
	Evaluations   []CapabilityEvaluation `json:"evaluations"`
}

type CapabilityEvaluationReport struct {
	Status             string   `json:"status"`
	EvaluationSetID    string   `json:"evaluation_set_id"`
	EvaluationsRead    int      `json:"evaluations_read"`
	AppliedEvaluations int      `json:"applied_evaluations"`
	EvaluationNodeIDs  []string `json:"evaluation_node_ids"`
	EdgeIDs            []string `json:"edge_ids"`
	SkippedUnknown     []string `json:"skipped_unknown_capabilities,omitempty"`
}

func LoadCapabilityEvaluations(path string) (CapabilityEvaluationSet, error) {
	info, err := os.Stat(path)
	if err != nil {
		return CapabilityEvaluationSet{}, fmt.Errorf("stat capability evaluations %s: %w", path, err)
	}
	if info.IsDir() {
		return loadCapabilityEvaluationDir(path)
	}
	eval, data, err := loadCapabilityEvaluationFile(path)
	if err != nil {
		return CapabilityEvaluationSet{}, err
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	return CapabilityEvaluationSet{
		SchemaVersion: CapabilityEvaluationSetSchemaVersion,
		ID:            "capability_evaluations_" + revision[7:19],
		AdapterID:     "os.capability-evaluations",
		SourceSystem:  "capability-evaluations",
		SourcePath:    path,
		SourceKind:    "state_record",
		Revision:      revision,
		EvidenceRefs:  []string{"capability_evaluation:" + eval.ID, "artifact_sha256:" + revision[7:]},
		Evaluations:   []CapabilityEvaluation{eval},
	}, nil
}

func loadCapabilityEvaluationDir(path string) (CapabilityEvaluationSet, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return CapabilityEvaluationSet{}, fmt.Errorf("read capability evaluations dir %s: %w", path, err)
	}
	var evals []CapabilityEvaluation
	var evidence []string
	h := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		filePath := filepath.Join(path, entry.Name())
		eval, data, err := loadCapabilityEvaluationFile(filePath)
		if err != nil {
			return CapabilityEvaluationSet{}, err
		}
		_, _ = h.Write([]byte(entry.Name()))
		_, _ = h.Write(data)
		evals = append(evals, eval)
		evidence = append(evidence, "capability_evaluation:"+eval.ID)
	}
	if len(evals) == 0 {
		return CapabilityEvaluationSet{}, fmt.Errorf("capability evaluations dir %s has no json records", path)
	}
	sort.Slice(evals, func(i, j int) bool { return evals[i].ID < evals[j].ID })
	sum := h.Sum(nil)
	revision := "sha256:" + hex.EncodeToString(sum)
	return CapabilityEvaluationSet{
		SchemaVersion: CapabilityEvaluationSetSchemaVersion,
		ID:            "capability_evaluations_" + revision[7:19],
		AdapterID:     "os.capability-evaluations",
		SourceSystem:  "capability-evaluations",
		SourcePath:    path,
		SourceKind:    "state_records_dir",
		Revision:      revision,
		EvidenceRefs:  evidence,
		Evaluations:   evals,
	}, nil
}

func loadCapabilityEvaluationFile(path string) (CapabilityEvaluation, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityEvaluation{}, nil, fmt.Errorf("read capability evaluation %s: %w", path, err)
	}
	var eval CapabilityEvaluation
	if err := json.Unmarshal(data, &eval); err != nil {
		return CapabilityEvaluation{}, nil, fmt.Errorf("parse capability evaluation %s: %w", path, err)
	}
	if err := eval.Validate(); err != nil {
		return CapabilityEvaluation{}, nil, fmt.Errorf("validate capability evaluation %s: %w", path, err)
	}
	return eval, data, nil
}

func (e CapabilityEvaluation) Validate() error {
	if e.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1, got %d", e.SchemaVersion)
	}
	if strings.TrimSpace(e.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(e.CreatedAt) == "" {
		return fmt.Errorf("created_at is required")
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(e.CreatedAt)); err != nil {
		return fmt.Errorf("created_at must be RFC3339: %w", err)
	}
	if strings.TrimSpace(e.SourceReceipt) == "" {
		return fmt.Errorf("source_receipt is required")
	}
	if strings.TrimSpace(e.Capability) == "" {
		return fmt.Errorf("capability is required")
	}
	if !capabilityEvaluationVerdicts[strings.TrimSpace(e.Verdict)] {
		return fmt.Errorf("unknown verdict %q", e.Verdict)
	}
	return nil
}

func ApplyCapabilityEvaluations(homePath string, set CapabilityEvaluationSet, now time.Time) (CapabilityEvaluationReport, error) {
	report := CapabilityEvaluationReport{
		Status:          "ok",
		EvaluationSetID: set.ID,
		EvaluationsRead: len(set.Evaluations),
	}
	if len(set.Evaluations) == 0 {
		report.Status = "fail"
		return report, fmt.Errorf("capability evaluation set has no evaluations")
	}
	store, err := Open(homePath)
	if err != nil {
		return report, err
	}
	nodes, err := store.loadNodes()
	if err != nil {
		return report, err
	}
	edges, err := store.loadEdges()
	if err != nil {
		return report, err
	}
	existingEdges := map[string]bool{}
	for _, edge := range edges {
		existingEdges[edgeKey(edge)] = true
	}
	unknown := map[string]bool{}
	var evalNodes []Node
	var evalEdges []Edge
	for _, eval := range set.Evaluations {
		if err := eval.Validate(); err != nil {
			report.Status = "fail"
			return report, err
		}
		evalNode := Node{
			ID:           capabilityEvaluationNodeID(eval),
			Kind:         "eval",
			Name:         "Capability evaluation: " + eval.Capability,
			Triggers:     evaluationTriggers(eval),
			Weight:       0.25,
			EvidenceOnly: true,
		}
		evalNodes = append(evalNodes, evalNode)
		report.EvaluationNodeIDs = append(report.EvaluationNodeIDs, evalNode.ID)
		capabilityNodeID := resolveCapabilityNode(nodes, eval.Capability)
		if capabilityNodeID == "" {
			key := strings.TrimSpace(eval.Capability)
			if !unknown[key] {
				report.SkippedUnknown = append(report.SkippedUnknown, key)
				unknown[key] = true
			}
			continue
		}
		evaluated := Edge{
			Kind:       EdEvaluatedBy,
			From:       capabilityNodeID,
			To:         evalNode.ID,
			Confidence: evaluationConfidence(eval),
			CreatedAt:  now.UTC().Format(time.RFC3339),
		}
		if added, err := addEdgeOnceCollect(&evalEdges, existingEdges, evaluated); err != nil {
			report.Status = "fail"
			return report, err
		} else if added {
			report.EdgeIDs = append(report.EdgeIDs, edgeID(evaluated))
		}
		if positiveEvaluationVerdicts[strings.TrimSpace(eval.Verdict)] {
			validated := Edge{
				Kind:       EdValidatedBy,
				From:       capabilityNodeID,
				To:         evalNode.ID,
				Confidence: evaluationConfidence(eval),
				CreatedAt:  now.UTC().Format(time.RFC3339),
			}
			if added, err := addEdgeOnceCollect(&evalEdges, existingEdges, validated); err != nil {
				report.Status = "fail"
				return report, err
			} else if added {
				report.EdgeIDs = append(report.EdgeIDs, edgeID(validated))
			}
		}
		report.AppliedEvaluations++
	}
	if len(evalNodes) > 0 || len(evalEdges) > 0 {
		if err := ApplyNodesAndEdgesGoverned(homePath, evalNodes, evalEdges, now); err != nil {
			report.Status = "fail"
			return report, err
		}
	}
	sort.Strings(report.SkippedUnknown)
	if len(report.SkippedUnknown) > 0 {
		report.Status = "review"
	}
	return report, nil
}

func addEdgeOnceCollect(edges *[]Edge, existing map[string]bool, edge Edge) (bool, error) {
	key := edgeKey(edge)
	if existing[key] {
		return false, nil
	}
	if edge.Confidence == 0 {
		edge.Confidence = 1.0
	}
	if edge.Confidence < 0 {
		return false, fmt.Errorf("edge confidence must be >= 0, got %.2f", edge.Confidence)
	}
	if strings.TrimSpace(edge.CreatedAt) == "" {
		return false, fmt.Errorf("edge needs created_at")
	}
	existing[key] = true
	*edges = append(*edges, edge)
	return true, nil
}

var capabilityEvaluationVerdicts = map[string]bool{
	"useful": true, "needs_improvement": true, "keep": true, "adapt": true,
	"merge": true, "evaluate_further": true, "park": true, "retire": true,
	"skip_duplicate": true, "publicize_candidate": true, "defer_off_stack": true,
	"downgrade_to_index": true,
}

var positiveEvaluationVerdicts = map[string]bool{
	"useful": true, "keep": true, "adapt": true, "merge": true, "publicize_candidate": true,
}

func resolveCapabilityNode(nodes []Node, capability string) string {
	capability = strings.TrimSpace(strings.TrimPrefix(capability, "capability://"))
	if capability == "" {
		return ""
	}
	lower := strings.ToLower(capability)
	for _, node := range nodes {
		if strings.EqualFold(node.ID, capability) {
			return node.ID
		}
		if strings.EqualFold(node.Name, capability) {
			return node.ID
		}
	}
	slug := slugSkill(lower)
	for _, node := range nodes {
		nodeID := strings.ToLower(node.ID)
		nodeSuffix := nodeID
		if idx := strings.Index(nodeSuffix, ":"); idx >= 0 {
			nodeSuffix = nodeSuffix[idx+1:]
		}
		if nodeSuffix == slug || slugSkill(node.Name) == slug {
			return node.ID
		}
	}
	return ""
}

func capabilityEvaluationNodeID(eval CapabilityEvaluation) string {
	return "eval:" + slugSkill(firstEvalNonEmpty(eval.ID, eval.Capability))
}

func evaluationTriggers(eval CapabilityEvaluation) []string {
	tokens := collectSkillTokens(true, eval.Capability, eval.Verdict, eval.ID)
	if len(tokens) == 0 {
		return []string{"evaluation"}
	}
	return tokens
}

func evaluationConfidence(eval CapabilityEvaluation) float64 {
	if eval.RubricNormalized > 0 {
		return eval.RubricNormalized
	}
	return 1.0
}

func edgeID(edge Edge) string {
	return edge.Kind + ":" + edge.From + "->" + edge.To
}

func firstEvalNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}
