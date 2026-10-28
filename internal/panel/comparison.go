package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/capgraph"
	"time"
)

const ComparisonCandidateSchemaVersion = "compound.comparison_candidate.v0.1"

const (
	comparisonStateAwaitingVerdict = "awaiting_verdict"
	comparisonStateJudged          = "judged"
	comparisonLedgerFile           = "comparison-evidence.jsonl"
)

type comparisonCandidate struct {
	SchemaVersion   string           `json:"schema_version"`
	ID              string           `json:"id"`
	CapabilityClass string           `json:"capability_class"`
	InputSummary    string           `json:"input_summary"`
	InputDigest     string           `json:"input_digest"`
	Baseline        comparisonOutput `json:"baseline"`
	Candidate       comparisonOutput `json:"candidate"`
	State           string           `json:"state"`
	CreatedAt       string           `json:"created_at"`
	Verdict         string           `json:"verdict,omitempty"`
	VerdictArtifact string           `json:"verdict_artifact,omitempty"`
}

type comparisonOutput struct {
	LaneID      string `json:"lane_id"`
	Output      string `json:"output"`
	TotalTokens int    `json:"total_tokens"`
	LatencyMS   int64  `json:"latency_ms"`
}

type comparisonEvidenceRow struct {
	ComparisonID    string  `json:"comparison_id"`
	CapabilityClass string  `json:"capability_class"`
	BaselineLane    string  `json:"baseline_lane"`
	CandidateLane   string  `json:"candidate_lane"`
	Decision        string  `json:"decision"`
	Confidence      float64 `json:"confidence"`
	RunID           string  `json:"run_id"`
	VerdictArtifact string  `json:"verdict_artifact"`
	CreatedAt       string  `json:"created_at"`
}

func LoadComparisonTargets(homePath string, limit int) ([]TargetInput, error) {
	dir := comparisonInboxDir(homePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := []TargetInput{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		candidate, err := decodeComparisonCandidate(raw, path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(candidate.State) != comparisonStateAwaitingVerdict {
			continue
		}
		out = append(out, TargetInput{
			Kind:       "comparison",
			Ref:        candidate.ID,
			Visibility: "internal",
			Summary:    comparisonSummary(candidate),
			Payload:    json.RawMessage(append([]byte(nil), raw...)),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func loadComparisonTarget(homePath, ref string) (TargetInput, error) {
	path := comparisonCandidatePath(homePath, ref)
	raw, err := os.ReadFile(path)
	if err != nil {
		return TargetInput{}, err
	}
	candidate, err := decodeComparisonCandidate(raw, path)
	if err != nil {
		return TargetInput{}, err
	}
	return TargetInput{Kind: "comparison", Ref: candidate.ID, Visibility: "internal", Summary: comparisonSummary(candidate), Payload: json.RawMessage(append([]byte(nil), raw...))}, nil
}

func decodeComparisonCandidate(data []byte, path string) (comparisonCandidate, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var candidate comparisonCandidate
	if err := dec.Decode(&candidate); err != nil {
		return comparisonCandidate{}, fmt.Errorf("parse comparison candidate %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return comparisonCandidate{}, fmt.Errorf("parse comparison candidate %s: trailing JSON", path)
		}
		return comparisonCandidate{}, fmt.Errorf("parse comparison candidate %s trailing JSON: %w", path, err)
	}
	if candidate.SchemaVersion != ComparisonCandidateSchemaVersion {
		return comparisonCandidate{}, fmt.Errorf("comparison candidate %s schema_version = %q, want %q", path, candidate.SchemaVersion, ComparisonCandidateSchemaVersion)
	}
	if strings.TrimSpace(candidate.ID) == "" {
		return comparisonCandidate{}, fmt.Errorf("comparison candidate %s id required", path)
	}
	if strings.TrimSpace(candidate.CapabilityClass) == "" {
		return comparisonCandidate{}, fmt.Errorf("comparison candidate %s capability_class required", path)
	}
	if strings.TrimSpace(candidate.Baseline.LaneID) == "" || strings.TrimSpace(candidate.Candidate.LaneID) == "" {
		return comparisonCandidate{}, fmt.Errorf("comparison candidate %s baseline and candidate lane_id required", path)
	}
	if strings.TrimSpace(candidate.State) == "" {
		return comparisonCandidate{}, fmt.Errorf("comparison candidate %s state required", path)
	}
	return candidate, nil
}

func comparisonSummary(candidate comparisonCandidate) string {
	tokenDelta := candidate.Candidate.TotalTokens - candidate.Baseline.TotalTokens
	latencyDelta := candidate.Candidate.LatencyMS - candidate.Baseline.LatencyMS
	return fmt.Sprintf("%s %s baseline=%s candidate=%s tokens_delta=%d latency_delta_ms=%d", candidate.CapabilityClass, candidate.InputSummary, candidate.Baseline.LaneID, candidate.Candidate.LaneID, tokenDelta, latencyDelta)
}

func applyComparisonVerdict(homePath string, run Run, finding *TargetFinding, now func() time.Time) (mapped string, applied bool, note string, err error) {
	path := comparisonCandidatePath(homePath, finding.TargetRef)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, "", err
	}
	candidate, err := decodeComparisonCandidate(raw, path)
	if err != nil {
		return "", false, "", err
	}
	mapped = mapComparisonDecision(finding.Synthesis.Decision)
	if candidate.State == comparisonStateJudged {
		return mapped, false, "comparison candidate already judged; skipped", nil
	}
	// Append the evidence row BEFORE flipping the candidate to judged: the
	// append is idempotent (rowExists), so a crash between the two steps
	// retries cleanly, while the reverse order strands a judged candidate
	// with no ledger row (observed live 2026-07-02).
	row := comparisonEvidenceRow{
		ComparisonID:    candidate.ID,
		CapabilityClass: candidate.CapabilityClass,
		BaselineLane:    candidate.Baseline.LaneID,
		CandidateLane:   candidate.Candidate.LaneID,
		Decision:        mapped,
		Confidence:      finding.Synthesis.Confidence,
		RunID:           run.ID,
		VerdictArtifact: finding.VerdictArtifact,
		CreatedAt:       now().UTC().Format(time.RFC3339),
	}
	if err := appendComparisonEvidenceRow(homePath, row); err != nil {
		return "", false, "", err
	}
	candidate.State = comparisonStateJudged
	candidate.Verdict = mapped
	candidate.VerdictArtifact = finding.VerdictArtifact
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		return "", false, "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", false, "", err
	}
	return mapped, true, "comparison evidence recorded: " + candidate.ID, nil
}

func mapComparisonDecision(decision string) string {
	switch normalizePanelPosition(decision) {
	case "promote":
		return "prefer_candidate"
	case "reject":
		return "prefer_baseline"
	case "park":
		return "inconclusive"
	case "needs_human":
		return "needs_human"
	case "needs_trial":
		return "needs_trial"
	default:
		return normalizePanelPosition(decision)
	}
}

func appendComparisonEvidenceRow(homePath string, row comparisonEvidenceRow) error {
	path := comparisonEvidenceLedgerPath(homePath)
	if exists, err := comparisonEvidenceRowExists(path, row.ComparisonID, row.RunID); err != nil {
		return err
	} else if exists {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func comparisonEvidenceRowExists(path, comparisonID, runID string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row comparisonEvidenceRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return false, err
		}
		if row.ComparisonID == comparisonID && row.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

func comparisonInboxDir(homePath string) string {
	return filepath.Join(homePath, "inbox", "comparisons")
}

func comparisonCandidatePath(homePath, ref string) string {
	return filepath.Join(comparisonInboxDir(homePath), ref+".json")
}

func comparisonEvidenceLedgerPath(homePath string) string {
	return filepath.Join(homePath, "ledger", comparisonLedgerFile)
}

func canonicalComparisonLaneNodeID(laneID string) (string, bool) {
	return capgraph.AgentLaneNodeIDFromShortLaneID(laneID)
}
