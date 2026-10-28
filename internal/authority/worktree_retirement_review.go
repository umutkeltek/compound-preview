package authority

import (
	"bytes"
	"compound/internal/authority/gitrepository"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	WorktreeRetirementReviewSchemaVersion       = gitrepository.WorktreeRetirementReviewSchemaVersion
	WorktreeRetirementReviewReportSchemaVersion = gitrepository.WorktreeRetirementReviewReportSchemaVersion

	// WorktreeRetirementReviewArea owns worktree-retirement artifacts. They used
	// to share the source-control-cleanup area, where the source-control queue
	// annotator typed every retirement plan as an invalid dirty-plan and counted
	// it against that area's operator queue. A retirement plan is its own review
	// family — its own review YAML, its own authorization report, its own apply
	// path — so it is routed, not terminally disposed: the operator decision it
	// asks for is real and still owed.
	WorktreeRetirementReviewArea = "worktree-retirement"
)

type WorktreeRetirementPlanPaths struct {
	PlanPath       string `json:"plan_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type WorktreeRetirementReview = gitrepository.WorktreeRetirementReview

type WorktreeRetirementReviewReport = gitrepository.WorktreeRetirementReviewReport

func RecordWorktreeRetirementPlan(homePath string, plan WorktreeRetirementPlan) (WorktreeRetirementPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return WorktreeRetirementPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := ValidateWorktreeRetirementPlan(plan); err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	planBytes, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	dir, err := worktreeRetirementArtifactDir(homePath, plan.ID)
	if err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	planPath, err := writeExclusiveWorktreeRetirementArtifact(dir, plan.ID+".retirement.json", append(planBytes, '\n'))
	if err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	reviewPath, err := writeExclusiveWorktreeRetirementArtifact(dir, plan.ID+".retirement.md", []byte(RenderWorktreeRetirementPlan(plan)))
	if err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	reviewYAMLPath, err := writeExclusiveWorktreeRetirementArtifact(dir, plan.ID+".retirement.review.yaml", []byte(RenderWorktreeRetirementReviewYAML(plan)))
	if err != nil {
		return WorktreeRetirementPlanPaths{}, err
	}
	return WorktreeRetirementPlanPaths{PlanPath: planPath, ReviewPath: reviewPath, ReviewYAMLPath: reviewYAMLPath}, nil
}

// worktreeRetirementArtifactDir resolves the directory that owns one plan's
// whole artifact group. Retirement artifacts written before the area split are
// immutable evidence sitting in the source-control-cleanup area, and their
// review and apply flow must keep resolving them there; a plan whose core
// artifact already exists in that legacy area therefore keeps its entire group
// there, and only a plan with no legacy body lands in the retirement area.
func worktreeRetirementArtifactDir(homePath, planID string) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(planID) == "" || filepath.Base(planID) != planID {
		return "", fmt.Errorf("worktree retirement plan id invalid")
	}
	legacy := filepath.Join(homePath, "reviews", SourceControlCleanupReviewArea)
	if _, err := os.Stat(filepath.Join(legacy, planID+".retirement.json")); err == nil {
		return legacy, nil
	}
	return reviewartifact.Dir(homePath, WorktreeRetirementReviewArea)
}

func ValidateWorktreeRetirementPlan(plan WorktreeRetirementPlan) error {
	return gitrepository.ValidateWorktreeRetirementPlan(plan)
}

func RenderWorktreeRetirementPlan(plan WorktreeRetirementPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Worktree Retirement Review: %s\n\n", plan.ID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- eligible: %v\n", plan.Eligible)
	fmt.Fprintf(&b, "- repo_root: %s\n", plan.RepoRoot)
	if plan.Worktree != nil {
		fmt.Fprintf(&b, "- worktree: %s\n", plan.Worktree.Path)
	}
	if plan.CandidateRef != "" {
		fmt.Fprintf(&b, "- candidate_ref: %s\n", plan.CandidateRef)
	}
	fmt.Fprintf(&b, "- candidate_tip: %s\n", plan.CandidateTip)
	fmt.Fprintf(&b, "- relation: %s\n", plan.CandidateRelation)
	fmt.Fprintf(&b, "- authority: %s @ %s\n", plan.AuthorityRef, plan.AuthorityTip)
	if plan.PublicationRef == "" {
		fmt.Fprintln(&b, "- publication: local_only; retain the bound authority ref")
	} else {
		fmt.Fprintf(&b, "- publication: %s @ %s\n", plan.PublicationRef, plan.PublicationTip)
	}
	fmt.Fprintf(&b, "- owner_work: %s\n", plan.OwnerProof.WorkID)
	fmt.Fprintf(&b, "- source_work: %s\n", plan.SourceProof.WorkID)
	fmt.Fprintf(&b, "- plan_digest: %s\n", plan.PlanDigest)
	if len(plan.BlockReasons) > 0 {
		fmt.Fprintln(&b, "\n## Block reasons")
		for _, reason := range plan.BlockReasons {
			fmt.Fprintf(&b, "\n- %s", reason)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "\nPlanning writes review evidence only. Apply is the sole Git mutator and revalidates every sealed observation.")
	return b.String()
}

func RenderWorktreeRetirementReviewYAML(plan WorktreeRetirementPlan) string {
	review := WorktreeRetirementReview{
		SchemaVersion: WorktreeRetirementReviewSchemaVersion,
		ID:            "wtrr_" + digestRetirementHex([]byte(plan.ID + "|" + plan.PlanDigest))[:12],
		PlanID:        plan.ID,
		PlanDigest:    plan.PlanDigest,
		Decision:      "TODO_authorize_retirement",
		Rationale:     "",
		ReviewedBy:    "TODO_operator_provenance",
		EvidenceRefs:  worktreeRetirementEvidenceRefs(plan),
	}
	data, _ := yaml.Marshal(review)
	return string(data)
}

func LoadWorktreeRetirementPlan(path string) (WorktreeRetirementPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeRetirementPlan{}, err
	}
	var plan WorktreeRetirementPlan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil {
		return WorktreeRetirementPlan{}, err
	}
	if err := ValidateWorktreeRetirementPlan(plan); err != nil {
		return WorktreeRetirementPlan{}, err
	}
	return plan, nil
}

func LoadWorktreeRetirementReview(path string) (WorktreeRetirementReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeRetirementReview{}, err
	}
	var review WorktreeRetirementReview
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&review); err != nil {
		return WorktreeRetirementReview{}, err
	}
	return review, nil
}

func RecordWorktreeRetirementReview(homePath string, review WorktreeRetirementReview, sourcePath string, now time.Time) (WorktreeRetirementReviewReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	review.ReviewedBy = strings.TrimSpace(review.ReviewedBy)
	review.Rationale = strings.TrimSpace(review.Rationale)
	review.ReviewedAt = strings.TrimSpace(review.ReviewedAt)
	review.EvidenceRefs = sortedUniqueNonEmpty(review.EvidenceRefs)
	if err := validateWorktreeRetirementReview(review); err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	dir, err := worktreeRetirementArtifactDir(homePath, review.PlanID)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	expectedTemplatePath := filepath.Join(dir, review.PlanID+".retirement.review.yaml")
	canonicalSource, err := filepath.Abs(filepath.Clean(sourcePath))
	if err != nil || canonicalSource != filepath.Clean(expectedTemplatePath) {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement review source path must be %s", expectedTemplatePath)
	}
	planPath := filepath.Join(dir, review.PlanID+".retirement.json")
	plan, err := LoadWorktreeRetirementPlan(planPath)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	if plan.SchemaVersion != WorktreeRetirementPlanSchemaVersion {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("historical retirement plan requires fresh v0.2 planning and review")
	}
	if review.PlanDigest != plan.PlanDigest {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement review plan digest mismatch")
	}
	if !plan.Eligible {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement plan is not eligible: %s", strings.Join(plan.BlockReasons, "; "))
	}
	requiredEvidence := worktreeRetirementEvidenceRefs(plan)
	if !containsEvery(review.EvidenceRefs, requiredEvidence) {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement review must preserve every plan evidence ref")
	}
	reviewedSourceName := plan.ID + "." + review.ID + ".retirement.reviewed.yaml"
	reviewedSourcePath := filepath.Join(dir, reviewedSourceName)
	if review.ReviewedAt == "" {
		existing, err := LoadWorktreeRetirementReview(reviewedSourcePath)
		switch {
		case err == nil:
			if err := validateWorktreeRetirementReview(existing); err != nil {
				return WorktreeRetirementReviewReport{}, fmt.Errorf("existing worktree retirement reviewed source invalid: %w", err)
			}
			if !worktreeRetirementReviewEqualIgnoringReviewedAt(review, existing) {
				return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement reviewed source already exists with different requested content")
			}
			review.ReviewedAt = existing.ReviewedAt
		case os.IsNotExist(err):
			review.ReviewedAt = now.UTC().Format(time.RFC3339Nano)
		default:
			return WorktreeRetirementReviewReport{}, err
		}
	}
	if err := validateWorktreeRetirementReview(review); err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	reviewBytes, err := yaml.Marshal(review)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	reviewedSourcePath, err = writeExclusiveWorktreeRetirementArtifact(dir, reviewedSourceName, reviewBytes)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	sourceDigest := "sha256:" + digestRetirementHex(reviewBytes)
	report := WorktreeRetirementReviewReport{
		SchemaVersion: WorktreeRetirementReviewReportSchemaVersion,
		ID:            "wtrrr_" + digestRetirementHex([]byte(plan.ID + "|" + review.ID + "|" + sourceDigest))[:12],
		CreatedAt:     review.ReviewedAt,
		Status:        "authorized",
		PlanID:        plan.ID,
		PlanDigest:    plan.PlanDigest,
		PlanPath:      planPath,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		Rationale:     review.Rationale,
		ReviewedBy:    review.ReviewedBy,
		ReviewedAt:    review.ReviewedAt,
		SourcePath:    reviewedSourcePath,
		SourceDigest:  sourceDigest,
		EvidenceRefs:  sortedUniqueNonEmpty(append(requiredEvidence, sourceDigest, "worktree-retirement-reviewer:"+review.ReviewedBy)),
	}
	report.ReviewPath = filepath.Join(dir, plan.ID+"."+review.ID+".retirement.reviewed.json")
	report.AuthorizationDigest = worktreeRetirementAuthorizationDigest(report)
	reportBytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	path, err := writeExclusiveWorktreeRetirementArtifact(dir, filepath.Base(report.ReviewPath), append(reportBytes, '\n'))
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	if filepath.Clean(path) != filepath.Clean(report.ReviewPath) {
		return WorktreeRetirementReviewReport{}, fmt.Errorf("worktree retirement review path mismatch")
	}
	return report, nil
}

// writeExclusiveWorktreeRetirementArtifact takes the resolved artifact
// directory, not the home, so every write in one plan's group lands beside its
// core plan — retirement area for a new plan, legacy area for a plan that
// already lives there.
func writeExclusiveWorktreeRetirementArtifact(dir, name string, data []byte) (string, error) {
	if filepath.Base(name) != name || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("worktree retirement artifact name invalid")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := writeWorktreeRetirementArtifactTemp(tmp, data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(existing, data) {
			return "", fmt.Errorf("worktree retirement artifact %s already exists with different bytes", path)
		}
	}
	if err := os.Remove(tmpPath); err != nil {
		return "", err
	}
	removeTemp = false
	if err := syncWorktreeRetirementArtifactDirectory(dir); err != nil {
		return "", fmt.Errorf("sync worktree retirement artifact directory: %w", err)
	}
	return path, nil
}

var writeWorktreeRetirementArtifactTemp = writeWorktreeRetirementArtifactTempFully

func writeWorktreeRetirementArtifactTempFully(file *os.File, data []byte) error {
	for remaining := data; len(remaining) > 0; {
		n, err := file.Write(remaining)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	return nil
}

func syncWorktreeRetirementArtifactDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

func validateWorktreeRetirementReview(review WorktreeRetirementReview) error {
	return gitrepository.ValidateWorktreeRetirementReview(review)
}

func worktreeRetirementReviewEqualIgnoringReviewedAt(a, b WorktreeRetirementReview) bool {
	return a.SchemaVersion == b.SchemaVersion && a.ID == b.ID && a.PlanID == b.PlanID && a.PlanDigest == b.PlanDigest &&
		a.Decision == b.Decision && a.Rationale == b.Rationale && a.ReviewedBy == b.ReviewedBy &&
		stringSlicesEqual(a.EvidenceRefs, b.EvidenceRefs)
}

func ValidateAuthorizedWorktreeRetirement(planPath, reportPath string) (WorktreeRetirementReviewReport, error) {
	plan, err := LoadWorktreeRetirementPlan(planPath)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	report, err := LoadWorktreeRetirementReviewReport(reportPath)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	sourceBytes, err := os.ReadFile(report.SourcePath)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	if err := gitrepository.ValidateWorktreeRetirementAuthorization(plan, planPath, report, reportPath, sourceBytes); err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	return report, nil
}

func LoadWorktreeRetirementReviewReport(path string) (WorktreeRetirementReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	var report WorktreeRetirementReviewReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return WorktreeRetirementReviewReport{}, err
	}
	return report, nil
}

func worktreeRetirementAuthorizationDigest(report WorktreeRetirementReviewReport) string {
	return gitrepository.WorktreeRetirementAuthorizationDigest(report)
}

func worktreeRetirementEvidenceRefs(plan WorktreeRetirementPlan) []string {
	return gitrepository.WorktreeRetirementEvidenceRefs(plan)
}
