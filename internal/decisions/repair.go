package decisions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const RepairPlanSchemaVersion = "compound.decision_lifecycle_repair_plan.v0.1"

// LifecycleRepairPlan is a reviewable owner-source repair artifact. It does not
// mutate source files or promote decisions; it records the exact lifecycle debt
// an owner must repair before accepted records can become current truth.
type LifecycleRepairPlan struct {
	SchemaVersion      string            `json:"schema_version"`
	ID                 string            `json:"id"`
	CreatedAt          string            `json:"created_at"`
	SourceRoot         string            `json:"source_root"`
	SourceKind         string            `json:"source_kind,omitempty"`
	Status             string            `json:"status"`
	RepairCount        int               `json:"repair_count"`
	MissingFieldCounts map[string]int    `json:"missing_field_counts"`
	Repairs            []LifecycleRepair `json:"repairs"`
	EvidenceRefs       []string          `json:"evidence_refs"`
}

type LifecycleRepair struct {
	DecisionID  string                 `json:"decision_id"`
	Title       string                 `json:"title"`
	Status      string                 `json:"status"`
	SourcePath  string                 `json:"source_path"`
	Missing     []string               `json:"missing"`
	Fields      []LifecycleRepairField `json:"fields"`
	BodySignals []string               `json:"body_signals,omitempty"`
	EvidenceRef string                 `json:"evidence_ref"`
}

type LifecycleRepairField struct {
	Name           string `json:"name"`
	SuggestedValue string `json:"suggested_value"`
	Basis          string `json:"basis"`
	ReviewNote     string `json:"review_note"`
}

type RepairArtifactPaths struct {
	PlanPath   string
	ReviewPath string
}

type LifecycleRepairApplyReport struct {
	SchemaVersion string            `json:"schema_version"`
	ID            string            `json:"id"`
	CreatedAt     string            `json:"created_at"`
	PlanID        string            `json:"plan_id"`
	PlanStatus    string            `json:"plan_status,omitempty"`
	SourceRoot    string            `json:"source_root"`
	DryRun        bool              `json:"dry_run"`
	Status        string            `json:"status"`
	AppliedFields int               `json:"applied_fields"`
	ChangedFiles  []string          `json:"changed_files"`
	Skipped       []RepairApplySkip `json:"skipped,omitempty"`
	EvidenceRefs  []string          `json:"evidence_refs"`
}

type RepairApplySkip struct {
	DecisionID string `json:"decision_id"`
	SourcePath string `json:"source_path"`
	Field      string `json:"field,omitempty"`
	Reason     string `json:"reason"`
}

type RepairApplyOptions struct {
	DryRun bool
}

func BuildLifecycleRepairPlan(root string, now time.Time) (LifecycleRepairPlan, error) {
	files, err := EligiblePolicyDecisionFiles(root)
	if err != nil {
		return LifecycleRepairPlan{}, err
	}
	return buildLifecycleRepairPlan(root, "policy", files, now)
}

func BuildBrainLifecycleRepairPlan(paths []string, now time.Time) (LifecycleRepairPlan, error) {
	return BuildSourceLifecycleRepairPlan(paths, "brain", now)
}

func BuildSourceLifecycleRepairPlan(paths []string, sourceKind string, now time.Time) (LifecycleRepairPlan, error) {
	if len(paths) == 0 {
		return LifecycleRepairPlan{}, fmt.Errorf("decision source markdown path required")
	}
	sourceKind = strings.TrimSpace(sourceKind)
	if sourceKind == "" {
		sourceKind = "note"
	}
	abs := make([]string, 0, len(paths))
	for _, path := range paths {
		got, err := filepath.Abs(path)
		if err != nil {
			return LifecycleRepairPlan{}, err
		}
		abs = append(abs, got)
	}
	return buildLifecycleRepairPlan(commonSourceRoot(abs), sourceKind, abs, now)
}

func buildLifecycleRepairPlan(root, sourceKind string, files []string, now time.Time) (LifecycleRepairPlan, error) {
	plan := LifecycleRepairPlan{
		SchemaVersion:      RepairPlanSchemaVersion,
		CreatedAt:          now.UTC().Format(time.RFC3339),
		SourceRoot:         root,
		SourceKind:         sourceKind,
		Status:             "ok",
		MissingFieldCounts: map[string]int{},
	}
	for _, path := range files {
		repair, ok, err := lifecycleRepairForFile(path, sourceKind)
		if err != nil {
			return LifecycleRepairPlan{}, err
		}
		if !ok {
			continue
		}
		plan.Repairs = append(plan.Repairs, repair)
		plan.EvidenceRefs = append(plan.EvidenceRefs, repair.EvidenceRef)
		for _, field := range repair.Missing {
			plan.MissingFieldCounts[field]++
		}
	}
	plan.RepairCount = len(plan.Repairs)
	if plan.RepairCount > 0 {
		plan.Status = "review_required"
	}
	plan.ID = "dlrp_" + digest12(sourceKind+"\n"+root+"\n"+repairDigest(plan))
	return plan, nil
}

func RecordLifecycleRepairPlan(homePath string, plan LifecycleRepairPlan) (RepairArtifactPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return RepairArtifactPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	paths, err := reviewartifact.Write(homePath, "decision-lifecycle-repairs", plan.ID, plan, "  ", RenderLifecycleRepairReview(plan))
	return RepairArtifactPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func LoadLifecycleRepairPlan(path string) (LifecycleRepairPlan, error) {
	var plan LifecycleRepairPlan
	b, err := os.ReadFile(path)
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(b, &plan); err != nil {
		return plan, fmt.Errorf("parse lifecycle repair plan %s: %w", path, err)
	}
	if plan.SchemaVersion != RepairPlanSchemaVersion {
		return plan, fmt.Errorf("repair plan schema_version=%q, want %q", plan.SchemaVersion, RepairPlanSchemaVersion)
	}
	if strings.TrimSpace(plan.ID) == "" {
		return plan, fmt.Errorf("repair plan id required")
	}
	if strings.TrimSpace(plan.SourceRoot) == "" {
		return plan, fmt.Errorf("repair plan source_root required")
	}
	return plan, nil
}

func ApplyLifecycleRepairPlan(plan LifecycleRepairPlan, opts RepairApplyOptions, now time.Time) (LifecycleRepairApplyReport, error) {
	createdAt := now.UTC().Format(time.RFC3339Nano)
	mode := "write"
	if opts.DryRun {
		mode = "dry-run"
	}
	report := LifecycleRepairApplyReport{
		SchemaVersion: "compound.decision_lifecycle_repair_apply.v0.1",
		ID:            "dlra_" + digest12(plan.ID+"\n"+mode+"\n"+createdAt),
		CreatedAt:     createdAt,
		PlanID:        plan.ID,
		PlanStatus:    strings.TrimSpace(plan.Status),
		SourceRoot:    plan.SourceRoot,
		DryRun:        opts.DryRun,
		Status:        "ok",
		EvidenceRefs:  append([]string(nil), plan.EvidenceRefs...),
	}
	if !opts.DryRun && !LifecycleRepairPlanReviewed(plan.Status) {
		report.Status = "blocked"
		report.Skipped = append(report.Skipped, RepairApplySkip{
			Reason: "repair plan status is not reviewed; set status to reviewed, ready_to_apply, or approved after source-owner review",
		})
		return report, fmt.Errorf("repair plan %s status %q is not reviewed", plan.ID, plan.Status)
	}
	rootClean, err := filepath.Abs(plan.SourceRoot)
	if err != nil {
		return report, err
	}
	changed := map[string]bool{}
	for _, repair := range plan.Repairs {
		path, err := filepath.Abs(repair.SourcePath)
		if err != nil {
			return report, err
		}
		if !pathWithin(rootClean, path) {
			report.Skipped = append(report.Skipped, RepairApplySkip{
				DecisionID: repair.DecisionID,
				SourcePath: repair.SourcePath,
				Reason:     "source path outside repair plan source_root",
			})
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return report, err
		}
		next, applied, skips, err := applyLifecycleRepairToContent(string(content), repair)
		if err != nil {
			return report, fmt.Errorf("apply lifecycle repair %s: %w", repair.DecisionID, err)
		}
		report.Skipped = append(report.Skipped, skips...)
		if len(applied) == 0 {
			continue
		}
		report.AppliedFields += len(applied)
		changed[path] = true
		if !opts.DryRun {
			if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
				return report, err
			}
		}
	}
	for path := range changed {
		report.ChangedFiles = append(report.ChangedFiles, path)
	}
	sort.Strings(report.ChangedFiles)
	if report.AppliedFields == 0 && len(report.Skipped) > 0 {
		report.Status = "no_changes"
	}
	return report, nil
}

func LifecycleRepairPlanReviewed(status string) bool {
	switch strings.TrimSpace(status) {
	case "reviewed", "ready_to_apply", "approved":
		return true
	default:
		return false
	}
}

func LifecycleRepairApplyTerminal(report LifecycleRepairApplyReport) bool {
	if report.Status == "ok" {
		return true
	}
	if report.Status != "no_changes" || len(report.Skipped) == 0 {
		return false
	}
	for _, skip := range report.Skipped {
		if skip.Reason != "frontmatter field already present" {
			return false
		}
	}
	return true
}

func RecordLifecycleRepairApplyReport(homePath string, report LifecycleRepairApplyReport) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("COMPOUND_HOME required")
	}
	name := report.PlanID + "." + report.ID + ".apply.json"
	return reviewartifact.WriteJSON(homePath, lifecycleRepairArea, name, report, "  ")
}

// RecordAppliedLifecycleRepairPlan terminalizes a reviewed repair plan after a
// successful writeback. The apply report keeps the before/after detail; the
// core queue artifact should stop advertising another write action.
func RecordAppliedLifecycleRepairPlan(homePath string, plan LifecycleRepairPlan) (RepairArtifactPaths, error) {
	plan.Status = "ok"
	plan.RepairCount = 0
	plan.MissingFieldCounts = map[string]int{}
	plan.Repairs = nil
	return RecordLifecycleRepairPlan(homePath, plan)
}

func RenderLifecycleRepairReview(plan LifecycleRepairPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Decision Lifecycle Repair Plan\n\n")
	fmt.Fprintf(&b, "- plan: `%s`\n", plan.ID)
	fmt.Fprintf(&b, "- status: `%s`\n", plan.Status)
	fmt.Fprintf(&b, "- repairs: %d\n", plan.RepairCount)
	fmt.Fprintf(&b, "- source_root: `%s`\n", plan.SourceRoot)
	if plan.SourceKind != "" {
		fmt.Fprintf(&b, "- source_kind: `%s`\n", plan.SourceKind)
	}
	if len(plan.MissingFieldCounts) > 0 {
		keys := make([]string, 0, len(plan.MissingFieldCounts))
		for k := range plan.MissingFieldCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- missing_%s: %d\n", k, plan.MissingFieldCounts[k])
		}
	}
	fmt.Fprintf(&b, "\nThese are owner-review suggestions. Applying them blindly does not replace source-owner review.\n\n")
	for _, repair := range plan.Repairs {
		fmt.Fprintf(&b, "## %s\n\n", repair.DecisionID)
		fmt.Fprintf(&b, "- source: `%s`\n", repair.SourcePath)
		fmt.Fprintf(&b, "- title: %s\n", repair.Title)
		fmt.Fprintf(&b, "- missing: `%s`\n", strings.Join(repair.Missing, "`, `"))
		if len(repair.BodySignals) > 0 {
			fmt.Fprintf(&b, "- body_signals: `%s`\n", strings.Join(repair.BodySignals, "`, `"))
		}
		fmt.Fprintf(&b, "\nSuggested frontmatter additions:\n\n")
		fmt.Fprintf(&b, "```yaml\n")
		for _, field := range repair.Fields {
			fmt.Fprintf(&b, "%s: %q\n", field.Name, field.SuggestedValue)
		}
		fmt.Fprintf(&b, "```\n\n")
		for _, field := range repair.Fields {
			fmt.Fprintf(&b, "- `%s` basis: %s\n", field.Name, field.Basis)
			fmt.Fprintf(&b, "- `%s` review: %s\n", field.Name, field.ReviewNote)
		}
		fmt.Fprintf(&b, "\n")
	}
	return b.String()
}

func lifecycleRepairForFile(path, sourceKind string) (LifecycleRepair, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return LifecycleRepair{}, false, err
	}
	fm, body := splitFrontmatter(string(b))
	if strings.TrimSpace(fm) == "" {
		return LifecycleRepair{}, false, nil
	}
	var f decFrontmatter
	_ = yaml.Unmarshal([]byte(fm), &f)
	prepareLifecycleRepairFrontmatter(&f, body, path, sourceKind)
	if f.Falsifier == "" {
		if mm := reFalsifier.FindStringSubmatch(body); mm != nil {
			f.Falsifier = strings.TrimSpace(mm[1])
		}
	}
	if f.ReviewTrigger == "" {
		if mm := reReviewTrigger.FindStringSubmatch(body); mm != nil {
			f.ReviewTrigger = strings.TrimSpace(mm[1])
		}
	}
	if f.ReviewTrigger == "" && sourceKind == "brain" {
		f.ReviewTrigger = extractNarrativeReviewTrigger(body)
	}
	if CanonicalStatus(f.Status) != StatusAccepted {
		return LifecycleRepair{}, false, nil
	}
	l := Lifecycle{
		ID:            f.ID,
		Kind:          kindFromQualifiers(lifecycleQualifiers(f.Kind, "decisions-repo", f.ID, f.Status)),
		Choice:        f.Title,
		Status:        CanonicalStatus(f.Status),
		Falsifier:     strings.TrimSpace(f.Falsifier),
		ReviewTrigger: strings.TrimSpace(f.ReviewTrigger),
	}
	missing := MissingRequired(l)
	if len(missing) == 0 {
		return LifecycleRepair{}, false, nil
	}
	signals := lifecycleBodySignals(body)
	repair := LifecycleRepair{
		DecisionID:  f.ID,
		Title:       f.Title,
		Status:      l.Status,
		SourcePath:  path,
		Missing:     missing,
		BodySignals: signals,
		EvidenceRef: repairEvidenceRef(sourceKind, path),
	}
	for _, field := range missing {
		repair.Fields = append(repair.Fields, suggestedLifecycleField(field, f, signals))
	}
	return repair, true, nil
}

func prepareLifecycleRepairFrontmatter(f *decFrontmatter, body, path, sourceKind string) {
	heading := firstHeading(body)
	if sourceKind == "brain" {
		if f.ID == "" {
			f.ID = stableBrainDecisionID(*f, heading, path)
		}
		if f.Title == "" || preferBrainHeading(f.Title, heading) {
			f.Title = heading
		}
		if choice := brainDecisionChoice(f.Title, body); choice != "" {
			f.Title = choice
		}
		if f.Kind == "" {
			f.Kind = KindDoctrine
		}
		return
	}
	if f.ID == "" {
		f.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if f.Title == "" {
		f.Title = heading
	}
}

func repairEvidenceRef(sourceKind, path string) string {
	switch sourceKind {
	case "brain":
		return "brain_source:" + filepath.ToSlash(path)
	default:
		return "decision_source:" + filepath.ToSlash(path)
	}
}

func suggestedLifecycleField(field string, f decFrontmatter, signals []string) LifecycleRepairField {
	title := strings.TrimSpace(f.Title)
	if title == "" {
		title = strings.TrimSpace(f.ID)
	}
	basis := "owner-review suggestion derived from decision id/title"
	if len(signals) > 0 {
		basis += " and body sections: " + strings.Join(signals, ", ")
	}
	switch field {
	case "falsifier":
		return LifecycleRepairField{
			Name:           field,
			SuggestedValue: fmt.Sprintf("Evidence from a governed run shows %q no longer prevents its stated failure mode or creates more owner/source fragmentation than it removes.", title),
			Basis:          basis,
			ReviewNote:     "Tighten this to the concrete failure evidence that would invalidate or retire the decision before applying.",
		}
	case "review_trigger":
		return LifecycleRepairField{
			Name:           field,
			SuggestedValue: fmt.Sprintf("Re-check when a governed workflow, owner, tool, platform, or recurring failure related to %q changes.", title),
			Basis:          basis,
			ReviewNote:     "Replace with the smallest concrete event that should force a future review if the source owner knows one.",
		}
	default:
		return LifecycleRepairField{
			Name:           field,
			SuggestedValue: "OWNER_REVIEW_REQUIRED",
			Basis:          basis,
			ReviewNote:     "Unknown lifecycle field; source owner must fill this manually.",
		}
	}
}

func applyLifecycleRepairToContent(content string, repair LifecycleRepair) (string, []string, []RepairApplySkip, error) {
	if !strings.HasPrefix(content, "---") {
		return content, nil, []RepairApplySkip{{
			DecisionID: repair.DecisionID,
			SourcePath: repair.SourcePath,
			Reason:     "frontmatter missing",
		}}, nil
	}
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return content, nil, nil, fmt.Errorf("closing frontmatter delimiter missing")
	}
	fm := rest[:end]
	var current decFrontmatter
	_ = yaml.Unmarshal([]byte(fm), &current)
	fieldSet := map[string]LifecycleRepairField{}
	for _, field := range repair.Fields {
		fieldSet[field.Name] = field
	}
	var additions []LifecycleRepairField
	var applied []string
	var skipped []RepairApplySkip
	for _, name := range repair.Missing {
		field, ok := fieldSet[name]
		if !ok {
			skipped = append(skipped, RepairApplySkip{
				DecisionID: repair.DecisionID,
				SourcePath: repair.SourcePath,
				Field:      name,
				Reason:     "repair field missing from plan",
			})
			continue
		}
		if lifecycleFrontmatterFieldPresent(current, name) {
			skipped = append(skipped, RepairApplySkip{
				DecisionID: repair.DecisionID,
				SourcePath: repair.SourcePath,
				Field:      name,
				Reason:     "frontmatter field already present",
			})
			continue
		}
		additions = append(additions, field)
		applied = append(applied, name)
	}
	if len(additions) == 0 {
		return content, nil, skipped, nil
	}
	var b strings.Builder
	b.WriteString(content[:3+end])
	if !strings.HasSuffix(fm, "\n") {
		b.WriteString("\n")
	}
	for _, field := range additions {
		b.WriteString(field.Name)
		b.WriteString(": ")
		b.WriteString(yamlQuote(field.SuggestedValue))
		b.WriteString("\n")
	}
	b.WriteString(content[3+end:])
	return b.String(), applied, skipped, nil
}

func lifecycleFrontmatterFieldPresent(f decFrontmatter, name string) bool {
	switch name {
	case "falsifier":
		return strings.TrimSpace(f.Falsifier) != ""
	case "review_trigger":
		return strings.TrimSpace(f.ReviewTrigger) != ""
	default:
		return false
	}
}

func yamlQuote(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSpace(string(b))
}

func pathWithin(root, path string) bool {
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".."
}

func commonSourceRoot(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	root := filepath.Dir(paths[0])
	for _, path := range paths[1:] {
		dir := filepath.Dir(path)
		for root != dir && !pathWithin(root, dir) {
			parent := filepath.Dir(root)
			if parent == root {
				return root
			}
			root = parent
		}
	}
	return root
}

func lifecycleBodySignals(body string) []string {
	headings := []string{
		"## Requirements",
		"## Non-Negotiables",
		"## Agent Checklist",
		"## Acceptance Criteria",
		"## Verification Commands",
		"## Agent Behavior",
		"## Runtime Policy",
	}
	var out []string
	for _, h := range headings {
		if strings.Contains(body, h) {
			out = append(out, strings.TrimPrefix(h, "## "))
		}
	}
	return out
}

func repairDigest(plan LifecycleRepairPlan) string {
	var parts []string
	for _, r := range plan.Repairs {
		parts = append(parts, r.DecisionID+":"+strings.Join(r.Missing, ",")+":"+r.SourcePath)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
