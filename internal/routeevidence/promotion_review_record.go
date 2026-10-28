package routeevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const PromotionHoldReviewReportSchemaVersion = "compound.route_promotion_hold_review.v0.1"

type PromotionHoldReviewReport struct {
	SchemaVersion   string   `json:"schema_version"`
	ID              string   `json:"id"`
	CreatedAt       string   `json:"created_at"`
	Status          string   `json:"status"`
	ReviewID        string   `json:"review_id"`
	Decision        string   `json:"decision"`
	Recommendation  string   `json:"recommendation"`
	LaneID          string   `json:"lane_id"`
	CapabilityClass string   `json:"capability_class"`
	Reason          string   `json:"reason"`
	Rationale       string   `json:"rationale"`
	NextAction      string   `json:"next_action"`
	SourceRef       string   `json:"source_ref"`
	SourceHash      string   `json:"source_hash"`
	EvidenceRefs    []string `json:"evidence_refs"`
	Boundaries      []string `json:"boundaries"`
	ReviewPath      string   `json:"review_path,omitempty"`
}

func LoadPromotionReview(path string) (PromotionReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PromotionReview{}, err
	}
	var review PromotionReview
	if err := json.Unmarshal(data, &review); err != nil {
		return PromotionReview{}, fmt.Errorf("parse route promotion review %s: %w", path, err)
	}
	if strings.TrimSpace(review.ID) == "" {
		return PromotionReview{}, fmt.Errorf("route promotion review %s missing id", path)
	}
	if review.SchemaVersion != PromotionReviewSchemaVersion {
		return PromotionReview{}, fmt.Errorf("route promotion review %s schema_version = %q, want %q", path, review.SchemaVersion, PromotionReviewSchemaVersion)
	}
	return review, nil
}

func RecordPromotionHoldReview(homePath, sourcePath, rationale string, now time.Time) (PromotionHoldReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return PromotionHoldReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return PromotionHoldReviewReport{}, fmt.Errorf("rationale required")
	}
	review, err := LoadPromotionReview(sourcePath)
	if err != nil {
		return PromotionHoldReviewReport{}, err
	}
	if strings.TrimSpace(review.Recommendation) != promotionRecommendationHoldInsufficient {
		return PromotionHoldReviewReport{}, fmt.Errorf("route promotion review %s recommendation %q cannot be recorded as hold; separate authority review required", review.ID, review.Recommendation)
	}
	_, hash, err := reviewartifact.HashFileTarget(sourcePath)
	if err != nil {
		return PromotionHoldReviewReport{}, err
	}
	report := PromotionHoldReviewReport{
		SchemaVersion:   PromotionHoldReviewReportSchemaVersion,
		ID:              reviewartifact.ID("rphr_", review.ID, hash, rationale),
		CreatedAt:       reviewartifact.Timestamp(now),
		Status:          "recorded",
		ReviewID:        review.ID,
		Decision:        "hold_recorded",
		Recommendation:  review.Recommendation,
		LaneID:          review.LaneID,
		CapabilityClass: review.CapabilityClass,
		Reason:          review.Reason,
		Rationale:       rationale,
		NextAction:      "keep lane promotion held; rerun route promotion-review after new route evidence meets criteria",
		SourceRef:       "compound://reviews/route-promotions/" + review.ID,
		SourceHash:      "sha256:" + hash,
		EvidenceRefs: reviewartifact.UniqueSorted(append(append([]string{}, review.EvidenceRefs...),
			review.ModelCapabilitiesAuthorityRef,
			review.ModelCapabilitiesSourceID,
			review.CriteriaAuthorityRef,
			review.CriteriaSourceID,
		)),
		Boundaries: []string{
			"no model-capabilities mutation",
			"no route-policy mutation",
			"no provider or gateway config mutation",
			"no lane promotion or demotion applied",
		},
	}
	path, err := reviewartifact.WriteJSON(homePath, routePromotionReviewArea, review.ID+".reviewed.json", report, " ")
	if err != nil {
		return PromotionHoldReviewReport{}, err
	}
	report.ReviewPath = path
	if _, err := reviewartifact.WriteJSON(homePath, routePromotionReviewArea, review.ID+".reviewed.json", report, " "); err != nil {
		return PromotionHoldReviewReport{}, err
	}
	return report, nil
}

func LoadPromotionHoldReviewReport(path string) (PromotionHoldReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PromotionHoldReviewReport{}, err
	}
	var report PromotionHoldReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return PromotionHoldReviewReport{}, fmt.Errorf("parse route promotion hold review %s: %w", path, err)
	}
	if report.SchemaVersion != PromotionHoldReviewReportSchemaVersion {
		return PromotionHoldReviewReport{}, fmt.Errorf("route promotion hold review %s schema_version = %q, want %q", path, report.SchemaVersion, PromotionHoldReviewReportSchemaVersion)
	}
	return report, nil
}
