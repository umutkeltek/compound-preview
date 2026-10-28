package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const PanelCapabilityFeedbackImpactSchemaVersion = "compound.panel_capability_feedback_impact.v0.1"

type PanelCapabilityFeedbackImpact struct {
	SchemaVersion      string                                `json:"schema_version"`
	GeneratedAt        string                                `json:"generated_at"`
	AuthorityRef       string                                `json:"authority_ref"`
	Status             string                                `json:"status"`
	PendingReviewCount int                                   `json:"pending_review_count"`
	AppliedReviewCount int                                   `json:"applied_review_count"`
	RejectedCount      int                                   `json:"rejected_count"`
	KeptReviewCount    int                                   `json:"kept_review_count"`
	AppliedEdges       int                                   `json:"applied_edges"`
	EdgeKindCounts     map[string]int                        `json:"edge_kind_counts,omitempty"`
	Recent             []PanelCapabilityFeedbackImpactRecord `json:"recent,omitempty"`
}

type PanelCapabilityFeedbackImpactRecord struct {
	ID             string         `json:"id"`
	CreatedAt      string         `json:"created_at,omitempty"`
	Status         string         `json:"status"`
	PlanID         string         `json:"plan_id"`
	VerdictID      string         `json:"verdict_id"`
	Decision       string         `json:"decision"`
	ReviewedBy     string         `json:"reviewed_by,omitempty"`
	AppliedEdges   int            `json:"applied_edges"`
	EdgeRefs       []string       `json:"edge_refs,omitempty"`
	EdgeKindCounts map[string]int `json:"edge_kind_counts,omitempty"`
}

func BuildPanelCapabilityFeedbackImpact(homePath string, reviews reviewartifact.QueueReport, limit int, now time.Time) (PanelCapabilityFeedbackImpact, error) {
	if limit <= 0 {
		limit = 5
	}
	out := PanelCapabilityFeedbackImpact{
		SchemaVersion:  PanelCapabilityFeedbackImpactSchemaVersion,
		GeneratedAt:    reviewartifact.Timestamp(now),
		AuthorityRef:   "compound://reviews/" + PanelCapabilityFeedbackReviewArea,
		Status:         "empty",
		EdgeKindCounts: map[string]int{},
	}
	for _, item := range reviews.Items {
		if item.Area != PanelCapabilityFeedbackReviewArea || !capabilityFeedbackReviewNeedsOwner(item) {
			continue
		}
		out.PendingReviewCount++
	}
	reports, err := loadPanelCapabilityFeedbackReviewReports(homePath)
	if err != nil {
		return out, err
	}
	for _, report := range reports {
		switch report.Status {
		case "applied_feedback":
			out.AppliedReviewCount++
		case "rejected":
			out.RejectedCount++
		case "kept_needs_review":
			out.KeptReviewCount++
		}
		out.AppliedEdges += report.AppliedEdges
		record := PanelCapabilityFeedbackImpactRecord{
			ID:             report.ID,
			CreatedAt:      report.CreatedAt,
			Status:         report.Status,
			PlanID:         report.PlanID,
			VerdictID:      report.VerdictID,
			Decision:       report.Decision,
			ReviewedBy:     report.ReviewedBy,
			AppliedEdges:   report.AppliedEdges,
			EdgeRefs:       append([]string(nil), report.EdgeRefs...),
			EdgeKindCounts: edgeKindCountsFromRefs(report.EdgeRefs),
		}
		for kind, count := range record.EdgeKindCounts {
			out.EdgeKindCounts[kind] += count
		}
		out.Recent = append(out.Recent, record)
	}
	sort.SliceStable(out.Recent, func(i, j int) bool {
		ai, aok := parseImpactTime(out.Recent[i].CreatedAt)
		aj, bok := parseImpactTime(out.Recent[j].CreatedAt)
		if aok && bok && !ai.Equal(aj) {
			return ai.After(aj)
		}
		if aok != bok {
			return aok
		}
		return out.Recent[i].ID < out.Recent[j].ID
	})
	if len(out.Recent) > limit {
		out.Recent = out.Recent[:limit]
	}
	switch {
	case out.PendingReviewCount > 0:
		out.Status = "review_required"
	case out.AppliedReviewCount > 0 || out.RejectedCount > 0 || out.KeptReviewCount > 0:
		out.Status = "recorded"
	}
	if len(out.EdgeKindCounts) == 0 {
		out.EdgeKindCounts = nil
	}
	return out, nil
}

func loadPanelCapabilityFeedbackReviewReports(homePath string) ([]PanelCapabilityFeedbackReviewReport, error) {
	dir := filepath.Join(homePath, "reviews", PanelCapabilityFeedbackReviewArea)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PanelCapabilityFeedbackReviewReport
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return out, err
		}
		var report PanelCapabilityFeedbackReviewReport
		if err := json.Unmarshal(data, &report); err != nil {
			continue
		}
		if report.SchemaVersion != PanelCapabilityFeedbackReportSchemaVersion || strings.TrimSpace(report.ID) == "" {
			continue
		}
		out = append(out, report)
	}
	return out, nil
}

func capabilityFeedbackReviewNeedsOwner(item reviewartifact.QueueItem) bool {
	switch strings.TrimSpace(item.Readiness) {
	case "needs_review", "ready_to_apply":
		return true
	}
	switch strings.TrimSpace(item.Status) {
	case "review_required", "ready_to_apply":
		return true
	default:
		return false
	}
}

func edgeKindCountsFromRefs(refs []string) map[string]int {
	out := map[string]int{}
	for _, ref := range refs {
		kind, _, ok := strings.Cut(strings.TrimSpace(ref), ":")
		if !ok || strings.TrimSpace(kind) == "" {
			continue
		}
		out[kind]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseImpactTime(raw string) (time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
