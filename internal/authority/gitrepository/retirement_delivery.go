package gitrepository

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"compound/internal/policy"
	"gopkg.in/yaml.v3"
)

// ValidateWorktreeRetirementAuthorization consumes the captured YAML bytes,
// never a second filesystem read. Authority and historical delivery share it.
func ValidateWorktreeRetirementAuthorization(plan WorktreeRetirementPlan, planPath string, report WorktreeRetirementReviewReport, reportPath string, sourceBytes []byte) error {
	if report.SchemaVersion != WorktreeRetirementReviewReportSchemaVersion || report.Status != "authorized" || report.Decision != "authorize_retirement" || !policy.HasOperatorProvenance(report.ReviewedBy) {
		return fmt.Errorf("worktree retirement authorization report invalid")
	}
	if report.PlanID != plan.ID || report.PlanDigest != plan.PlanDigest || filepath.Clean(report.PlanPath) != filepath.Clean(planPath) {
		return fmt.Errorf("worktree retirement authorization plan digest/path mismatch")
	}
	if !ValidRetirementDigest(report.AuthorizationDigest) || report.AuthorizationDigest != WorktreeRetirementAuthorizationDigest(report) {
		return fmt.Errorf("worktree retirement authorization digest mismatch")
	}
	if filepath.Clean(report.ReviewPath) != filepath.Clean(reportPath) {
		return fmt.Errorf("worktree retirement authorization report path mismatch")
	}
	if report.SourceDigest != "sha256:"+DigestRetirementHex(sourceBytes) {
		return fmt.Errorf("worktree retirement reviewed YAML digest mismatch")
	}
	var review WorktreeRetirementReview
	decoder := yaml.NewDecoder(bytes.NewReader(sourceBytes))
	decoder.KnownFields(true)
	err := decoder.Decode(&review)
	if err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("worktree retirement reviewed YAML must contain exactly one document")
	}
	if err := ValidateWorktreeRetirementReview(review); err != nil {
		return err
	}
	if review.ID != report.ReviewID || review.PlanID != report.PlanID || review.PlanDigest != report.PlanDigest || review.Decision != report.Decision || strings.TrimSpace(review.Rationale) != report.Rationale || strings.TrimSpace(review.ReviewedBy) != report.ReviewedBy || review.ReviewedAt != report.ReviewedAt {
		return fmt.Errorf("worktree retirement review report does not match reviewed YAML")
	}
	if !containsRetirementEvidence(review.EvidenceRefs, WorktreeRetirementEvidenceRefs(plan)) {
		return fmt.Errorf("worktree retirement reviewed YAML lost plan evidence")
	}
	return nil
}

func containsRetirementEvidence(have, required []string) bool {
	for _, ref := range required {
		if !slices.Contains(have, ref) {
			return false
		}
	}
	return true
}

type RetirementDelivery struct {
	Plan          WorktreeRetirementPlan
	Authorization WorktreeRetirementReviewReport
	Result        WorktreeRetirementResult
}

// ValidateRetirementDelivery accepts the existing producer's exact wire records.
// This is historical effect evidence, not new permission or live-state readiness.
func ValidateRetirementDelivery(planBody, reportBody, reviewedYAML, resultBody []byte) (RetirementDelivery, error) {
	var out RetirementDelivery
	for _, item := range []struct {
		body   []byte
		target any
	}{{planBody, &out.Plan}, {reportBody, &out.Authorization}, {resultBody, &out.Result}} {
		if len(item.body) == 0 || len(item.body) > 1<<20 {
			return out, fmt.Errorf("retirement evidence exceeds metadata bound")
		}
		decoder := json.NewDecoder(bytes.NewReader(item.body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(item.target); err != nil {
			return out, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return out, fmt.Errorf("retirement evidence trailing JSON")
		}
	}
	if len(reviewedYAML) == 0 || len(reviewedYAML) > 1<<20 {
		return out, fmt.Errorf("retirement review exceeds metadata bound")
	}
	if err := ValidateWorktreeRetirementPlan(out.Plan); err != nil {
		return out, err
	}
	if !out.Plan.Eligible {
		return out, fmt.Errorf("retirement plan was not eligible")
	}
	if err := ValidateWorktreeRetirementAuthorization(out.Plan, out.Authorization.PlanPath, out.Authorization, out.Authorization.ReviewPath, reviewedYAML); err != nil {
		return out, err
	}
	if err := ValidateWorktreeRetirementResult(out.Result, out.Plan, out.Authorization, out.Result.ResultPath); err != nil {
		return out, err
	}
	if out.Result.Status != "completed" || out.Result.Phase != "verified" || out.Result.Error != "" ||
		out.Result.WorktreeRemoved != (out.Plan.Worktree != nil) || out.Result.RefDeleted != (out.Plan.CandidateRef != "") ||
		out.Result.AuthorityTip != out.Plan.AuthorityTip || out.Result.PublicationTip != out.Plan.PublicationTip {
		return out, fmt.Errorf("retirement result does not prove exact completed effect")
	}
	return out, nil
}
