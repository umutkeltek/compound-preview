package panel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const VerdictHumanReviewReportSchemaVersion = "compound.panel_verdict_human_review.v0.1"
const VerdictHumanReviewGroupReportSchemaVersion = "compound.panel_verdict_human_review_group.v0.1"

type VerdictHumanReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	VerdictID     string   `json:"verdict_id"`
	RunID         string   `json:"run_id"`
	Decision      string   `json:"decision"`
	TargetKind    string   `json:"target_kind"`
	TargetRef     string   `json:"target_ref"`
	Rationale     string   `json:"rationale"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	NextAction    string   `json:"next_action"`
	SourceRef     string   `json:"source_ref"`
	SourceHash    string   `json:"source_hash"`
	EvidenceRefs  []string `json:"evidence_refs"`
	Boundaries    []string `json:"boundaries"`
	ReviewPath    string   `json:"review_path,omitempty"`
}

type VerdictHumanReviewGroupReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	GroupKey      string   `json:"group_key"`
	Count         int      `json:"count"`
	VerdictIDs    []string `json:"verdict_ids"`
	ReviewPaths   []string `json:"review_paths"`
	Rationale     string   `json:"rationale"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	NextAction    string   `json:"next_action"`
	EvidenceRefs  []string `json:"evidence_refs"`
	Boundaries    []string `json:"boundaries"`
	ReviewPath    string   `json:"review_path,omitempty"`
}

type VerdictHumanReviewInput struct {
	Rationale  string
	ReviewedBy string
}

func LoadVerdictArtifact(path string) (verdictArtifactPayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return verdictArtifactPayload{}, err
	}
	var verdict verdictArtifactPayload
	if err := json.Unmarshal(data, &verdict); err != nil {
		return verdictArtifactPayload{}, fmt.Errorf("parse panel verdict %s: %w", path, err)
	}
	if strings.TrimSpace(verdict.ID) == "" {
		return verdictArtifactPayload{}, fmt.Errorf("panel verdict %s missing id", path)
	}
	if verdict.SchemaVersion != "compound.panel_verdict.v0.1" {
		return verdictArtifactPayload{}, fmt.Errorf("panel verdict %s schema_version = %q, want %q", path, verdict.SchemaVersion, "compound.panel_verdict.v0.1")
	}
	return verdict, nil
}

func RecordVerdictHumanReview(homePath, sourcePath string, input VerdictHumanReviewInput, now time.Time) (VerdictHumanReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return VerdictHumanReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	rationale := strings.TrimSpace(input.Rationale)
	if rationale == "" {
		return VerdictHumanReviewReport{}, fmt.Errorf("rationale required")
	}
	reviewedBy := strings.TrimSpace(input.ReviewedBy)
	if reviewedBy == "" {
		return VerdictHumanReviewReport{}, fmt.Errorf("reviewed_by required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	verdict, err := LoadVerdictArtifact(sourcePath)
	if err != nil {
		return VerdictHumanReviewReport{}, err
	}
	decision := normalizePanelPosition(verdict.Synthesis.Decision)
	switch decision {
	case "needs_human", "needs_trial":
	default:
		return VerdictHumanReviewReport{}, fmt.Errorf("panel verdict %s decision %q requires panel apply or separate owner mutation path; human review record only supports needs_human/needs_trial", verdict.ID, verdict.Synthesis.Decision)
	}
	sourceExists, sourceHash, err := reviewartifact.HashFileTarget(sourcePath)
	if err != nil {
		return VerdictHumanReviewReport{}, err
	}
	if !sourceExists {
		return VerdictHumanReviewReport{}, fmt.Errorf("panel verdict source not found: %s", sourcePath)
	}
	report := VerdictHumanReviewReport{
		SchemaVersion: VerdictHumanReviewReportSchemaVersion,
		ID:            reviewartifact.ID("pvr", verdict.ID, rationale),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		VerdictID:     verdict.ID,
		RunID:         verdict.RunID,
		Decision:      decision,
		TargetKind:    strings.TrimSpace(verdict.Target.Kind),
		TargetRef:     strings.TrimSpace(verdict.Target.Ref),
		Rationale:     rationale,
		ReviewedBy:    reviewedBy,
		NextAction:    "owner reviewed panel request; keep target parked until human evidence/action exists",
		SourceRef:     "file:" + sourcePath,
		SourceHash:    sourceHash,
		EvidenceRefs:  reviewartifact.UniqueSorted(verdict.EvidenceRefs),
		Boundaries: []string{
			"human review record only; no panel apply",
			"no memory candidate adjudication",
			"no capability observation mutation",
			"no route/model/provider mutation",
		},
	}
	dir, err := reviewartifact.Dir(homePath, verdictReviewArea)
	if err != nil {
		return VerdictHumanReviewReport{}, err
	}
	report.ReviewPath = filepath.Join(dir, verdict.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, verdictReviewArea, verdict.ID+".reviewed.json", report, " "); err != nil {
		return VerdictHumanReviewReport{}, err
	}
	return report, nil
}

func RecordVerdictHumanReviewGroup(homePath, groupKey string, input VerdictHumanReviewInput, now time.Time) (VerdictHumanReviewGroupReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	groupKey = strings.TrimSpace(groupKey)
	if groupKey == "" {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("group required")
	}
	rationale := strings.TrimSpace(input.Rationale)
	if rationale == "" {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("rationale required")
	}
	reviewedBy := strings.TrimSpace(input.ReviewedBy)
	if reviewedBy == "" {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("reviewed_by required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: verdictReviewArea}, now)
	if err != nil {
		return VerdictHumanReviewGroupReport{}, err
	}
	AnnotateVerdictQueue(&queue)
	reviewartifact.Recount(&queue)
	memberIDs := map[string]bool{}
	for _, group := range queue.Groups {
		if group.Area != verdictReviewArea || group.Key != groupKey {
			continue
		}
		for _, id := range group.ItemIDs {
			memberIDs[id] = true
		}
		break
	}
	if len(memberIDs) == 0 {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("panel verdict group %q has no open members", groupKey)
	}
	var reports []VerdictHumanReviewReport
	for _, item := range queue.Items {
		if item.Area != verdictReviewArea || !memberIDs[item.ID] {
			continue
		}
		if item.Status == "recorded" || item.Readiness == "review_recorded" {
			continue
		}
		report, err := RecordVerdictHumanReview(homePath, item.JSONPath, input, now)
		if err != nil {
			return VerdictHumanReviewGroupReport{}, err
		}
		reports = append(reports, report)
	}
	if len(reports) == 0 {
		return VerdictHumanReviewGroupReport{}, fmt.Errorf("panel verdict group %q has no recordable members", groupKey)
	}
	groupReport := VerdictHumanReviewGroupReport{
		SchemaVersion: VerdictHumanReviewGroupReportSchemaVersion,
		ID:            reviewartifact.ID("pvrg_", groupKey, rationale),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		GroupKey:      groupKey,
		Count:         len(reports),
		Rationale:     rationale,
		ReviewedBy:    reviewedBy,
		NextAction:    "group human-review records written; apply/adjudicate still requires the canonical follow-up path",
		Boundaries: []string{
			"group human review record only; no panel apply",
			"no memory candidate adjudication",
			"no capability observation mutation",
			"no route/model/provider mutation",
			"member review sidecars remain individually auditable",
		},
	}
	for _, report := range reports {
		groupReport.VerdictIDs = append(groupReport.VerdictIDs, report.VerdictID)
		groupReport.ReviewPaths = append(groupReport.ReviewPaths, report.ReviewPath)
		groupReport.EvidenceRefs = append(groupReport.EvidenceRefs, report.EvidenceRefs...)
	}
	groupReport.VerdictIDs = reviewartifact.UniqueSorted(groupReport.VerdictIDs)
	groupReport.ReviewPaths = reviewartifact.UniqueSorted(groupReport.ReviewPaths)
	groupReport.EvidenceRefs = reviewartifact.UniqueSorted(groupReport.EvidenceRefs)
	dir, err := reviewartifact.Dir(homePath, verdictReviewArea)
	if err != nil {
		return VerdictHumanReviewGroupReport{}, err
	}
	groupReport.ReviewPath = filepath.Join(dir, groupReport.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, verdictReviewArea, groupReport.ID+".reviewed.json", groupReport, " "); err != nil {
		return VerdictHumanReviewGroupReport{}, err
	}
	return groupReport, nil
}

func LoadVerdictHumanReviewReport(path string) (VerdictHumanReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return VerdictHumanReviewReport{}, err
	}
	var report VerdictHumanReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return VerdictHumanReviewReport{}, fmt.Errorf("parse panel verdict human review %s: %w", path, err)
	}
	if report.SchemaVersion != VerdictHumanReviewReportSchemaVersion {
		return VerdictHumanReviewReport{}, fmt.Errorf("panel verdict human review %s schema_version = %q, want %q", path, report.SchemaVersion, VerdictHumanReviewReportSchemaVersion)
	}
	return report, nil
}
