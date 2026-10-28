// Package decisions imports decision sources into the spine memory family. A decision
// source becomes a mem candidate; promotion remains a separate adjudication step.
package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/candidates"
	homepkg "compound/internal/home"
	"compound/internal/mem"
	"compound/internal/writeback"
)

const (
	KindPolicy      = "policy"
	KindDoctrine    = "doctrine"
	KindOperational = "operational"

	// Grant/freeze/ruling are the operational-enforcement kinds `compound decisions
	// record` writes (grants-as-gates, 2026-07-10): an operator answer to a card
	// becomes a decision record the runtime consumes at an admission seam, not
	// prose. They share the same lifecycle machinery as policy/doctrine/operational
	// decisions above — same store, same qualifiers, same undo-via-supersede.
	KindGrant  = "grant"
	KindFreeze = "freeze"
	KindRuling = "ruling"

	// AutonomyGrantScope is the exact operational grant scope consumed by the
	// evidence-threshold admission path. It is separate from director-apply:
	// one authorizes filing suggestions, the other authorizes one class to
	// cross proposed -> admitted after deterministic evidence proves its bound.
	AutonomyGrantScope = "autonomy-evidence-threshold"

	// WorktreeKeepScope is the exact operator-decision scope that pins a worktree
	// branch against retirement cleanup (worktree-tracking slice 2). A keep is a
	// KindGrant record whose scope is this value; it carries the protected branch
	// ref (worktree_branch_ref) plus a required revisit condition (review_trigger)
	// and reason (the decision choice), and only counts with operator ReviewedBy
	// provenance — an agent-authored keep is inert (no self-keep), mirroring the
	// autonomy grant's operator-provenance gate.
	WorktreeKeepScope = "worktree-keep"

	StatusProposed   = "proposed"
	StatusAccepted   = "accepted"
	StatusSuperseded = "superseded"
	StatusDeprecated = "deprecated"
	StatusRejected   = "rejected"
)

var (
	reReviewTrigger   = regexp.MustCompile(`(?i)review[ \-]?trigger[:*\s]+\s*([^\n]+)`)
	reFalsifier       = regexp.MustCompile(`(?i)falsifier[:*\s]+\s*([^\n]+)`)
	reDatedBrainTitle = regexp.MustCompile(`^20\d\d\s+\d{1,2}\s+\d{1,2}\s+`)
	reNarrativeReview = regexp.MustCompile(`(?i)\b(?:I should revisit|Revisit before|Promote this decision|Promote decision|When [^\n]*(?:ships|blocked|release|review|R[0-9]+))[^\n.]*[.!]?`)
	reInlineHeading   = regexp.MustCompile(`\s+(#{2,6}\s+)`)
	reVerdictQuote    = regexp.MustCompile(`(?is)\bverdict:\s*(?:>\s*)?(?:\*\*)?\s*["“]([^"”]+)["”]`)
)

// Lifecycle is the DecisionLifecycle contract projected onto spine memory.
// The claim remains the durable ledger object; these fields make the decision
// lifecycle typed and auditable instead of a loose prose qualifier.
type Lifecycle struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Choice        string   `json:"choice"`
	Status        string   `json:"status"`
	Scope         string   `json:"scope,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Falsifier     string   `json:"falsifier,omitempty"`
	ReviewTrigger string   `json:"review_trigger,omitempty"`
	Supersedes    []string `json:"supersedes,omitempty"`
	ReplacedBy    string   `json:"replaced_by,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	// ReviewedBy is the operator provenance `compound decisions record` requires
	// (falsifier d, 664e1a7 parity). Grant/freeze resolution treats an active
	// record without it as not having provenance present.
	ReviewedBy string `json:"reviewed_by,omitempty"`
	// FreezeOwnerGlobs / FreezeKeywords are a freeze record's EXPLICIT scope
	// (settled design decision 2, grants-as-gates 2026-07-10): literal owner
	// globs and/or predicate keywords. Matching is literal, never inferred.
	FreezeOwnerGlobs   []string `json:"freeze_owner_globs,omitempty"`
	FreezeKeywords     []string `json:"freeze_keywords,omitempty"`
	AutoAdmitClass     string   `json:"auto_admit_class,omitempty"`
	MinTerminalSamples int      `json:"min_terminal_samples,omitempty"`
	MinWindowDays      int      `json:"min_window_days,omitempty"`
	MaxFailures        int      `json:"max_failures,omitempty"`
	// WorktreeBranchRef is a worktree-keep record's protected branch ref
	// (refs/heads/<name>) — the stable worktree identity slice 1 renders. Empty
	// for every other decision scope (additive; no existing source sets it).
	WorktreeBranchRef string `json:"worktree_branch_ref,omitempty"`
}

// LifecycleGap identifies a promoted decision claim missing lifecycle proof.
type LifecycleGap struct {
	ClaimID      string   `json:"claim_id,omitempty"`
	CandidateID  string   `json:"candidate_id,omitempty"`
	DecisionID   string   `json:"decision_id,omitempty"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	State        string   `json:"state,omitempty"`
	Missing      []string `json:"missing"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// DoctorReport is the typed report behind `compound decisions doctor`.
type DoctorReport struct {
	Total                  int            `json:"total"`
	Current                int            `json:"current"`
	Parked                 int            `json:"parked"`
	Counts                 map[string]int `json:"counts"`
	Statuses               map[string]int `json:"statuses"`
	MissingLifecycleFields int            `json:"missing_lifecycle_fields"`
	LifecycleGaps          []LifecycleGap `json:"lifecycle_gaps,omitempty"`
}

func promotionBasisForStatus(s string) string {
	switch CanonicalStatus(s) {
	case StatusAccepted:
		return candidates.BasisDecisionRecord
	default:
		return ""
	}
}

func CanonicalStatus(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "supersed"):
		return StatusSuperseded
	case strings.Contains(s, "propos"), strings.Contains(s, "open"), strings.Contains(s, "draft"):
		return StatusProposed
	case strings.Contains(s, "deprecat"), strings.Contains(s, "retired"):
		return StatusDeprecated
	case strings.Contains(s, "reject"):
		return StatusRejected
	default:
		return StatusAccepted
	}
}

func lifecycleQualifiers(kind, source, id, status string) map[string]string {
	return map[string]string{
		"kind":             kind,
		"source":           source,
		"decision_id":      id,
		"lifecycle_status": CanonicalStatus(status),
	}
}

func decisionEntity(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "decision:unknown"
	}
	return "decision:" + id
}

func splitFrontmatter(s string) (fm, body string) {
	if !strings.HasPrefix(s, "---") {
		return "", s
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", s
	}
	return rest[:end], rest[end+4:]
}

type decFrontmatter struct {
	ID            string   `yaml:"id"`
	Title         string   `yaml:"title"`
	Kind          string   `yaml:"kind"`
	Status        string   `yaml:"status"`
	Date          string   `yaml:"date"`
	EffectiveDate string   `yaml:"effective_date"`
	Domains       []string `yaml:"domains"`
	Supersedes    []string `yaml:"supersedes"`
	ReplacedBy    string   `yaml:"replaced_by"`
	Falsifier     string   `yaml:"falsifier"`
	ReviewTrigger string   `yaml:"review_trigger"`
	Owner         string   `yaml:"owner"`
	Scope         string   `yaml:"scope"`
	// ReviewedBy / FreezeOwnerGlobs / FreezeKeywords are additive fields for the
	// grants-as-gates record verb (Slice 1); every other decision source leaves
	// them empty, so this is additive-only for existing sources.
	ReviewedBy         string   `yaml:"reviewed_by"`
	FreezeOwnerGlobs   []string `yaml:"freeze_owner_globs"`
	FreezeKeywords     []string `yaml:"freeze_keywords"`
	AutoAdmitClass     string   `yaml:"auto_admit_class"`
	MinTerminalSamples int      `yaml:"min_terminal_samples"`
	MinWindowDays      int      `yaml:"min_window_days"`
	MaxFailures        int      `yaml:"max_failures"`
	// WorktreeBranchRef mirrors the worktree-keep record's protected branch ref;
	// additive and empty for every other decision source.
	WorktreeBranchRef string `yaml:"worktree_branch_ref"`
}

type DecisionImportReport struct {
	SourcePath  string    `json:"source_path"`
	SourceKind  string    `json:"source_kind,omitempty"`
	EvidenceID  string    `json:"evidence_id"`
	CandidateID string    `json:"candidate_id"`
	ClaimID     string    `json:"claim_id,omitempty"`
	Verdict     string    `json:"verdict"`
	Lifecycle   Lifecycle `json:"lifecycle"`
}

type BrainDecisionReport = DecisionImportReport

// ImportPolicyDecision imports one decisions-repo markdown record through the
// same evidence-backed lifecycle path used by curated knowledge decisions.
func ImportPolicyDecision(store *mem.Store, path string, now time.Time) (DecisionImportReport, error) {
	if store == nil {
		return DecisionImportReport{}, fmt.Errorf("policy decision import: mem store required")
	}
	var report DecisionImportReport
	var importErr error
	err := homepkg.WithAutonomyAdmissionLock(store.HomePath(), func() error {
		report, importErr = importPolicyDecisionLocked(store, path, now)
		return importErr
	})
	return report, err
}

func importPolicyDecisionLocked(store *mem.Store, path string, now time.Time) (DecisionImportReport, error) {
	var report DecisionImportReport
	b, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	fm, body := splitFrontmatter(string(b))
	var f decFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &f); err != nil {
		return report, fmt.Errorf("policy decision %s: parse frontmatter: %w", path, err)
	}
	if f.ID == "" {
		f.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if f.Title == "" {
		f.Title = firstHeading(body)
	}
	if strings.TrimSpace(f.Title) == "" {
		return report, fmt.Errorf("policy decision %s: title required", path)
	}
	if f.Kind == "" {
		f.Kind = KindPolicy
	}
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
	if err := validateOperationalAuthorityFrontmatter(f); err != nil {
		return report, fmt.Errorf("policy decision %s: %w", path, err)
	}
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "repo",
		SourceID:   "policy-decision:" + filepath.ToSlash(path),
		QuoteText:  decisionQuote(f.Title, body),
		Actor:      "tool",
		Visibility: "operational",
	}, now)
	if err != nil {
		return report, err
	}
	q := lifecycleQualifiers(f.Kind, "decisions-repo", f.ID, f.Status)
	// Only operator-authenticated operational kinds enter the dedicated
	// authority door. Ordinary policy/doctrine imports remain evidence-backed
	// memory and never receive the reserved runtime stamp.
	if isOperationalAuthorityKind(f.Kind) {
		q[mem.OperationalDecisionAuthorityQualifier] = mem.OperationalDecisionAuthorityValue
	}
	if len(f.Domains) > 0 {
		q["domain"] = strings.Join(f.Domains, ",")
	}
	if len(f.Supersedes) > 0 {
		q["supersedes"] = strings.Join(f.Supersedes, ",")
	}
	if f.ReplacedBy != "" {
		q["replaced_by"] = strings.TrimSpace(f.ReplacedBy)
	}
	if f.Falsifier != "" {
		q["falsifier"] = strings.TrimSpace(f.Falsifier)
	}
	if f.ReviewTrigger != "" {
		q["review_trigger"] = strings.TrimSpace(f.ReviewTrigger)
	}
	if f.Owner != "" {
		q["owner"] = strings.TrimSpace(f.Owner)
	}
	if f.Scope != "" {
		q["scope"] = strings.TrimSpace(f.Scope)
	}
	if f.ReviewedBy != "" {
		q["reviewed_by"] = strings.TrimSpace(f.ReviewedBy)
	}
	if f.WorktreeBranchRef != "" {
		q["worktree_branch_ref"] = strings.TrimSpace(f.WorktreeBranchRef)
	}
	if len(f.FreezeOwnerGlobs) > 0 {
		q["freeze_owner_globs"] = strings.Join(f.FreezeOwnerGlobs, ",")
	}
	if len(f.FreezeKeywords) > 0 {
		q["freeze_keywords"] = strings.Join(f.FreezeKeywords, ",")
	}
	if f.AutoAdmitClass != "" {
		q["auto_admit_class"] = strings.TrimSpace(f.AutoAdmitClass)
		q["min_terminal_samples"] = strconv.Itoa(f.MinTerminalSamples)
		q["min_window_days"] = strconv.Itoa(f.MinWindowDays)
		q["max_failures"] = strconv.Itoa(f.MaxFailures)
	}
	cand := mem.Candidate{
		Entity:         decisionEntity(f.ID),
		Predicate:      "decided",
		Object:         f.Title,
		Confidence:     1.0,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     q,
		PromotionBasis: promotionBasisForStatus(f.Status),
		SourceTraceRef: evidence.ID,
		SnippetHash:    digest12(f.ID + "\n" + f.Title),
		Visibility:     "operational",
	}
	if existing, ok, err := existingDecisionImportReport(store, cand, path, "policy", evidence.ID); err != nil {
		return report, err
	} else if ok {
		return existing, nil
	}
	cands, err := store.Capture([]mem.Candidate{cand}, now)
	if err != nil {
		return report, err
	}
	verdict := VerdictForCandidate(cands[0])
	var res mem.AdjudicateResult
	if isOperationalAuthorityKind(f.Kind) {
		res, err = store.AdjudicateOperationalDecision(cands[0].ID, verdict, "policy-decision-import", now)
	} else {
		res, err = store.Adjudicate(cands[0].ID, verdict, "policy-decision-import", now)
	}
	if err != nil {
		return report, err
	}
	report = DecisionImportReport{
		SourcePath:  path,
		SourceKind:  "policy",
		EvidenceID:  evidence.ID,
		CandidateID: cands[0].ID,
		Verdict:     verdict,
		Lifecycle:   LifecycleFromCandidate(cands[0]),
	}
	if res.Claim != nil {
		report.ClaimID = res.Claim.ID
		report.Lifecycle = LifecycleFromClaim(*res.Claim)
	} else if claimID := alreadyCurrentClaimID(res); claimID != "" {
		report.ClaimID = claimID
	}
	return report, nil
}

// ImportPolicyDecisionDir imports policy markdown files through the same
// evidence-backed lifecycle path as ImportPolicyDecision.
func ImportPolicyDecisionDir(store *mem.Store, root string, now time.Time) ([]DecisionImportReport, error) {
	files, err := EligiblePolicyDecisionFiles(root)
	if err != nil {
		return nil, err
	}
	reports := make([]DecisionImportReport, 0, len(files))
	for _, path := range files {
		report, err := ImportPolicyDecision(store, path, now)
		if err != nil {
			return reports, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func EligiblePolicyDecisionFiles(root string) ([]string, error) {
	base := filepath.Join(root, "decisions")
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		base = root
	}
	var files []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files, err
}

// EligibleNoteDecisionFiles returns markdown decision pages from a generic note
// source. It excludes index/template prose so corpus import does not promote
// documentation as a decision source.
func EligibleNoteDecisionFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		if name == "readme" || strings.Contains(name, "template") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files, err
}

// EligibleBrainDecisionFiles is a compatibility alias for private brain-source
// callers; public corpus imports should use EligibleNoteDecisionFiles.
func EligibleBrainDecisionFiles(root string) ([]string, error) {
	return EligibleNoteDecisionFiles(root)
}

// ImportNoteDecisionDir imports every eligible generic note decision page.
func ImportNoteDecisionDir(store *mem.Store, root string, now time.Time) ([]BrainDecisionReport, error) {
	files, err := EligibleNoteDecisionFiles(root)
	if err != nil {
		return nil, err
	}
	reports := make([]BrainDecisionReport, 0, len(files))
	for _, path := range files {
		report, err := ImportNoteDecision(store, path, now)
		if err != nil {
			return reports, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// ImportBrainDecisionDir is a compatibility alias for private brain-source callers.
func ImportBrainDecisionDir(store *mem.Store, root string, now time.Time) ([]BrainDecisionReport, error) {
	files, err := EligibleBrainDecisionFiles(root)
	if err != nil {
		return nil, err
	}
	reports := make([]BrainDecisionReport, 0, len(files))
	for _, path := range files {
		report, err := ImportBrainDecision(store, path, now)
		if err != nil {
			return reports, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// ImportBrainDecision imports one curated knowledge-base decision page into the
// owned decision lifecycle path. The markdown page is evidence; deterministic
// frontmatter/body parsing proposes the decision, and code adjudicates it.
func ImportBrainDecision(store *mem.Store, path string, now time.Time) (BrainDecisionReport, error) {
	return importNoteDecision(store, path, "brain", "brain", "brain-decision-import", now)
}

// ImportNoteDecision imports one generic markdown note decision page into
// owned decision lifecycle path.
func ImportNoteDecision(store *mem.Store, path string, now time.Time) (BrainDecisionReport, error) {
	return importNoteDecision(store, path, "note", "note", "note-decision-import", now)
}

func importNoteDecision(store *mem.Store, path, sourceKind, idPrefix, workID string, now time.Time) (BrainDecisionReport, error) {
	var report BrainDecisionReport
	b, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	fm, body := splitFrontmatter(string(b))
	var f decFrontmatter
	_ = yaml.Unmarshal([]byte(fm), &f)
	heading := firstHeading(body)
	generatedID := strings.TrimSpace(f.ID) == ""
	if f.ID == "" {
		f.ID = stableDecisionID(idPrefix, f, heading, path)
	}
	if f.Title == "" || preferBrainHeading(f.Title, heading) {
		f.Title = heading
	}
	if choice := brainDecisionChoice(f.Title, body); choice != "" {
		f.Title = choice
	}
	if strings.TrimSpace(f.Title) == "" {
		return report, fmt.Errorf("%s decision %s: title required", sourceKind, path)
	}
	if f.Kind == "" {
		f.Kind = KindDoctrine
	}
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
	if f.ReviewTrigger == "" {
		f.ReviewTrigger = extractNarrativeReviewTrigger(body)
	}
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "note",
		SourceID:   sourceKind + ":" + filepath.ToSlash(path),
		QuoteText:  decisionQuote(f.Title, body),
		Actor:      "tool",
		Visibility: "private",
	}, now)
	if err != nil {
		return report, err
	}
	q := lifecycleQualifiers(f.Kind, sourceKind, f.ID, f.Status)
	if len(f.Domains) > 0 {
		q["domain"] = strings.Join(f.Domains, ",")
	}
	if len(f.Supersedes) > 0 {
		q["supersedes"] = strings.Join(f.Supersedes, ",")
	}
	if f.ReplacedBy != "" {
		q["replaced_by"] = strings.TrimSpace(f.ReplacedBy)
	}
	if f.Falsifier != "" {
		q["falsifier"] = strings.TrimSpace(f.Falsifier)
	}
	if f.ReviewTrigger != "" {
		q["review_trigger"] = strings.TrimSpace(f.ReviewTrigger)
	}
	if f.Owner != "" {
		q["owner"] = strings.TrimSpace(f.Owner)
	}
	if f.Scope != "" {
		q["scope"] = strings.TrimSpace(f.Scope)
	}
	cand := mem.Candidate{
		Entity:         decisionEntity(f.ID),
		Predicate:      "decided",
		Object:         f.Title,
		Confidence:     1.0,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     q,
		PromotionBasis: promotionBasisForStatus(f.Status),
		SourceTraceRef: evidence.ID,
		SnippetHash:    digest12(f.ID + "\n" + f.Title),
		Visibility:     "operational",
	}
	if existing, ok, err := existingDecisionImportReport(store, cand, path, sourceKind, evidence.ID, legacyBrainCandidatesForGenericNote(cand, generatedID)...); err != nil {
		return report, err
	} else if ok {
		return BrainDecisionReport(existing), nil
	}
	cands, err := store.Capture([]mem.Candidate{cand}, now)
	if err != nil {
		return report, err
	}
	verdict := VerdictForCandidate(cands[0])
	res, err := store.Adjudicate(cands[0].ID, verdict, workID, now)
	if err != nil {
		return report, err
	}
	report = BrainDecisionReport{
		SourcePath:  path,
		SourceKind:  sourceKind,
		EvidenceID:  evidence.ID,
		CandidateID: cands[0].ID,
		Verdict:     verdict,
		Lifecycle:   LifecycleFromCandidate(cands[0]),
	}
	if res.Claim != nil {
		report.ClaimID = res.Claim.ID
		report.Lifecycle = LifecycleFromClaim(*res.Claim)
	} else if claimID := alreadyCurrentClaimID(res); claimID != "" {
		report.ClaimID = claimID
	}
	return report, nil
}

func alreadyCurrentClaimID(res mem.AdjudicateResult) string {
	if !strings.HasPrefix(res.Note, "already current:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(res.Note, "already current:"))
}

func existingDecisionImportReport(store *mem.Store, cand mem.Candidate, sourcePath, sourceKind, evidenceID string, alternates ...mem.Candidate) (DecisionImportReport, bool, error) {
	all := append([]mem.Candidate{cand}, alternates...)
	claims, err := store.AllCurrent()
	if err != nil {
		return DecisionImportReport{}, false, err
	}
	for _, claim := range claims {
		for _, candidate := range all {
			if claim.ClaimType != "decision" ||
				claim.Entity != candidate.Entity ||
				claim.Predicate != candidate.Predicate ||
				claim.Object != candidate.Object ||
				!sameStringMap(claim.Qualifiers, candidate.Qualifiers) {
				continue
			}
			candidateID := ""
			if len(claim.EvidenceRefs) > 0 {
				candidateID = claim.EvidenceRefs[0]
			}
			return DecisionImportReport{
				SourcePath:  sourcePath,
				SourceKind:  sourceKind,
				EvidenceID:  evidenceID,
				CandidateID: candidateID,
				ClaimID:     claim.ID,
				Verdict:     candidates.Promote,
				Lifecycle:   LifecycleFromClaim(claim),
			}, true, nil
		}
	}
	cands, err := store.AllCandidates()
	if err != nil {
		return DecisionImportReport{}, false, err
	}
	for _, existing := range cands {
		if existing.State == "promoted" {
			continue
		}
		for i, candidate := range all {
			matches := sameDecisionCandidate(existing, candidate)
			if !matches && i > 0 {
				matches = sameDecisionCandidateIgnoringTrace(existing, candidate)
			}
			if !matches {
				continue
			}
			return DecisionImportReport{
				SourcePath:  sourcePath,
				SourceKind:  sourceKind,
				EvidenceID:  evidenceID,
				CandidateID: existing.ID,
				Verdict:     VerdictForCandidate(existing),
				Lifecycle:   LifecycleFromCandidate(existing),
			}, true, nil
		}
	}
	return DecisionImportReport{}, false, nil
}

func sameDecisionCandidate(a, b mem.Candidate) bool {
	return a.Entity == b.Entity &&
		a.Predicate == b.Predicate &&
		a.Object == b.Object &&
		a.ClaimType == b.ClaimType &&
		a.PromotionBasis == b.PromotionBasis &&
		a.SourceTraceRef == b.SourceTraceRef &&
		sameStringMap(a.Qualifiers, b.Qualifiers)
}

func sameDecisionCandidateIgnoringTrace(a, b mem.Candidate) bool {
	return a.Entity == b.Entity &&
		a.Predicate == b.Predicate &&
		a.Object == b.Object &&
		a.ClaimType == b.ClaimType &&
		a.PromotionBasis == b.PromotionBasis &&
		sameStringMap(a.Qualifiers, b.Qualifiers)
}

func legacyBrainCandidatesForGenericNote(cand mem.Candidate, generatedID bool) []mem.Candidate {
	if !generatedID || cand.Qualifiers == nil || cand.Qualifiers["source"] != "note" {
		return nil
	}
	id := strings.TrimSpace(cand.Qualifiers["decision_id"])
	if !strings.HasPrefix(id, "note-") {
		return nil
	}
	legacyID := "brain-" + strings.TrimPrefix(id, "note-")
	legacy := cand
	legacy.Entity = decisionEntity(legacyID)
	legacy.Qualifiers = cloneStringMap(cand.Qualifiers)
	legacy.Qualifiers["decision_id"] = legacyID
	legacy.Qualifiers["source"] = "brain"
	return []mem.Candidate{legacy}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if b[key] != av {
			return false
		}
	}
	return true
}

var reDecHeader = regexp.MustCompile(`(?m)^###\s+([DO]\d+)\b[^\n]*?—\s*(.+)$`)

// ImportOperatorLedger parses operator ledger headings into doctrine decision candidates.
func ImportOperatorLedger(path string) ([]mem.Candidate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	idx := reDecHeader.FindAllStringSubmatchIndex(text, -1)
	var out []mem.Candidate
	for i, m := range idx {
		id := text[m[2]:m[3]]
		title := strings.Trim(strings.TrimSpace(text[m[4]:m[5]]), "~ ")
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := text[m[1]:end]
		status := "accepted"
		if strings.HasPrefix(id, "O") {
			status = StatusProposed
		}
		if regexp.MustCompile(`(?i)superseded`).MatchString(block) {
			status = "superseded"
		}
		q := lifecycleQualifiers(KindDoctrine, "operator-ledger", id, status)
		q["scope"] = "personal"
		if mm := reReviewTrigger.FindStringSubmatch(block); mm != nil {
			q["review_trigger"] = strings.TrimSpace(mm[1])
		}
		if mm := reFalsifier.FindStringSubmatch(block); mm != nil {
			q["falsifier"] = strings.TrimSpace(mm[1])
		}
		basis := promotionBasisForStatus(status)
		out = append(out, mem.Candidate{
			Entity:         decisionEntity(id),
			Predicate:      "decided",
			Object:         title,
			Confidence:     0.9,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     q,
			PromotionBasis: basis,
			SourceTraceRef: path,
			SnippetHash:    id,
			Visibility:     "operational",
		})
	}
	return out, nil
}

// Kind returns a decision kind from a spine claim.
func Kind(c mem.Claim) string {
	if c.Qualifiers == nil {
		return KindOperational
	}
	if k := c.Qualifiers["kind"]; k != "" {
		return k
	}
	return KindOperational
}

// LifecycleFromClaim projects the typed DecisionLifecycle contract from a
// promoted spine claim. It does not create a second source of truth.
func LifecycleFromClaim(c mem.Claim) Lifecycle {
	q := c.Qualifiers
	return Lifecycle{
		ID:                 q["decision_id"],
		Kind:               kindFromQualifiers(q),
		Choice:             c.Object,
		Status:             statusFromQualifiers(q),
		Scope:              q["scope"],
		Owner:              q["owner"],
		Falsifier:          q["falsifier"],
		ReviewTrigger:      q["review_trigger"],
		Supersedes:         splitCSV(q["supersedes"]),
		ReplacedBy:         q["replaced_by"],
		EvidenceRefs:       append([]string(nil), c.EvidenceRefs...),
		ReviewedBy:         q["reviewed_by"],
		FreezeOwnerGlobs:   splitCSV(q["freeze_owner_globs"]),
		FreezeKeywords:     splitCSV(q["freeze_keywords"]),
		AutoAdmitClass:     q["auto_admit_class"],
		MinTerminalSamples: qualifierInt(q, "min_terminal_samples"),
		MinWindowDays:      qualifierInt(q, "min_window_days"),
		MaxFailures:        qualifierInt(q, "max_failures"),
		WorktreeBranchRef:  q["worktree_branch_ref"],
	}
}

// LifecycleFromCandidate exposes the same lifecycle before promotion.
func LifecycleFromCandidate(c mem.Candidate) Lifecycle {
	q := c.Qualifiers
	refs := []string{c.ID}
	if strings.TrimSpace(c.SourceTraceRef) != "" {
		refs = append(refs, c.SourceTraceRef)
	}
	return Lifecycle{
		ID:                 q["decision_id"],
		Kind:               kindFromQualifiers(q),
		Choice:             c.Object,
		Status:             statusFromQualifiers(q),
		Scope:              q["scope"],
		Owner:              q["owner"],
		Falsifier:          q["falsifier"],
		ReviewTrigger:      q["review_trigger"],
		Supersedes:         splitCSV(q["supersedes"]),
		ReplacedBy:         q["replaced_by"],
		EvidenceRefs:       refs,
		ReviewedBy:         q["reviewed_by"],
		FreezeOwnerGlobs:   splitCSV(q["freeze_owner_globs"]),
		FreezeKeywords:     splitCSV(q["freeze_keywords"]),
		AutoAdmitClass:     q["auto_admit_class"],
		MinTerminalSamples: qualifierInt(q, "min_terminal_samples"),
		MinWindowDays:      qualifierInt(q, "min_window_days"),
		MaxFailures:        qualifierInt(q, "max_failures"),
		WorktreeBranchRef:  q["worktree_branch_ref"],
	}
}

func qualifierInt(q map[string]string, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(q[key]))
	return n
}

// MissingRequired reports fields required for a durable decision artifact.
func MissingRequired(l Lifecycle) []string {
	var missing []string
	if strings.TrimSpace(l.Falsifier) == "" {
		missing = append(missing, "falsifier")
	}
	if strings.TrimSpace(l.ReviewTrigger) == "" {
		missing = append(missing, "review_trigger")
	}
	return missing
}

// VerdictForCandidate promotes only accepted decisions with complete lifecycle
// proof. Accepted-but-incomplete decisions stay parked until the source owner is
// updated; status alone is not enough to become ambient truth.
func VerdictForCandidate(c mem.Candidate) string {
	if c.PromotionBasis == "" {
		return candidates.Park
	}
	if c.ClaimType == "decision" && len(MissingRequired(LifecycleFromCandidate(c))) > 0 {
		return candidates.Park
	}
	return candidates.Promote
}

// DoctorReportFromClaims projects current decision claims into a lifecycle audit.
func DoctorReportFromClaims(claims []mem.Claim) DoctorReport {
	report := DoctorReport{
		Counts:   map[string]int{},
		Statuses: map[string]int{},
	}
	for _, c := range claims {
		if c.ClaimType != "decision" {
			continue
		}
		l := LifecycleFromClaim(c)
		report.Total++
		report.Current++
		report.Counts[l.Kind]++
		report.Statuses[l.Status]++
		missing := MissingRequired(l)
		if len(missing) == 0 {
			continue
		}
		report.MissingLifecycleFields++
		report.LifecycleGaps = append(report.LifecycleGaps, LifecycleGap{
			ClaimID:      c.ID,
			DecisionID:   l.ID,
			Kind:         l.Kind,
			Status:       l.Status,
			Missing:      append([]string(nil), missing...),
			EvidenceRefs: append([]string(nil), l.EvidenceRefs...),
		})
	}
	return report
}

// DoctorReportFromClaimsAndCandidates includes parked decision candidates, so
// source records that are accepted but missing lifecycle proof remain visible as
// owner writeback debt without becoming current claims.
func DoctorReportFromClaimsAndCandidates(claims []mem.Claim, cands []mem.Candidate) DoctorReport {
	report := DoctorReportFromClaims(claims)
	for _, c := range cands {
		if c.ClaimType != "decision" || c.State == "promoted" {
			continue
		}
		l := LifecycleFromCandidate(c)
		report.Total++
		report.Parked++
		report.Counts[l.Kind]++
		report.Statuses[l.Status]++
		missing := MissingRequired(l)
		if len(missing) == 0 {
			continue
		}
		report.MissingLifecycleFields++
		report.LifecycleGaps = append(report.LifecycleGaps, LifecycleGap{
			CandidateID:  c.ID,
			DecisionID:   l.ID,
			Kind:         l.Kind,
			Status:       l.Status,
			State:        c.State,
			Missing:      append([]string(nil), missing...),
			EvidenceRefs: append([]string(nil), l.EvidenceRefs...),
		})
	}
	return report
}

// NeedsWriteback reports whether decision doctor found owner-fixable gaps.
func NeedsWriteback(report DoctorReport) bool {
	return len(report.LifecycleGaps) > 0
}

// WritebackGate converts decision lifecycle gaps into generic owner writeback.
func WritebackGate(report DoctorReport, actionIntentRef, policyDecisionRef string, now time.Time) writeback.OwnerWritebackGate {
	return writeback.OwnerWritebackGate{
		SchemaVersion:     writeback.GateSchemaVersion,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		CandidateRef:      fmt.Sprintf("decisions:lifecycle_gaps:%d:%s", len(report.LifecycleGaps), lifecycleGapDigest(report)),
		CandidateKind:     string(candidates.Decision),
		OwnerTarget:       writeback.OwnerTarget{Kind: "decision_lifecycle", Ref: "spine-memory-decisions"},
		GateBasis:         fmt.Sprintf("decision doctor found %d lifecycle gap(s); owner decision records may need writeback", len(report.LifecycleGaps)),
		ActionIntentRef:   actionIntentRef,
		MutationClass:     "local_reversible",
		PolicyDecisionRef: policyDecisionRef,
		EvidenceRefs:      EvidenceRefs(report),
	}
}

// EvidenceRefs returns bounded metadata refs for decision doctor output.
func EvidenceRefs(report DoctorReport) []string {
	refs := []string{
		fmt.Sprintf("decisions:total:%d", report.Total),
		fmt.Sprintf("decisions:missing_lifecycle_fields:%d", report.MissingLifecycleFields),
	}
	for _, gap := range report.LifecycleGaps {
		id := gap.DecisionID
		if strings.TrimSpace(id) == "" {
			id = gap.ClaimID
		}
		if strings.TrimSpace(id) == "" {
			id = gap.CandidateID
		}
		refs = append(refs, "decision_gap:"+id+":"+strings.Join(gap.Missing, ","))
	}
	return refs
}

func lifecycleGapDigest(report DoctorReport) string {
	refs := EvidenceRefs(report)
	sort.Strings(refs)
	sum := sha256.Sum256([]byte(strings.Join(refs, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

func kindFromQualifiers(q map[string]string) string {
	if q == nil || q["kind"] == "" {
		return KindOperational
	}
	return q["kind"]
}

func statusFromQualifiers(q map[string]string) string {
	if q == nil {
		return StatusAccepted
	}
	return CanonicalStatus(q["lifecycle_status"])
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return ""
}

func stableBrainDecisionID(f decFrontmatter, heading, path string) string {
	return stableDecisionID("brain", f, heading, path)
}

func stableDecisionID(prefix string, f decFrontmatter, heading, path string) string {
	parts := []string{
		strings.TrimSpace(f.Title),
		strings.TrimSpace(f.Date),
		strings.TrimSpace(f.EffectiveDate),
		strings.TrimSpace(heading),
	}
	var stable []string
	for _, part := range parts {
		if part != "" {
			stable = append(stable, part)
		}
	}
	if len(stable) == 0 {
		stable = append(stable, filepath.Base(path))
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	if prefix == "" {
		prefix = "note"
	}
	return prefix + "-" + digest12(strings.Join(stable, "\n"))
}

func preferBrainHeading(frontmatterTitle, heading string) bool {
	frontmatterTitle = strings.TrimSpace(frontmatterTitle)
	heading = strings.TrimSpace(heading)
	if heading == "" {
		return false
	}
	if frontmatterTitle == "" {
		return true
	}
	if reDatedBrainTitle.MatchString(frontmatterTitle) {
		return true
	}
	return false
}

func brainDecisionChoice(pageTitle, body string) string {
	if quoted := labeledVerdictQuote(body); quoted != "" {
		return truncateText(quoted, 280)
	}
	for _, name := range []string{"decision", "core thesis", "what i actually shipped"} {
		section := markdownSection(body, name)
		if section == "" {
			continue
		}
		if quoted := firstQuotedOrBold(section); quoted != "" {
			return truncateText(quoted, 280)
		}
		if line := firstContentLine(section); line != "" {
			return truncateText(line, 280)
		}
	}
	return strings.TrimSpace(pageTitle)
}

func labeledVerdictQuote(body string) string {
	if mm := reVerdictQuote.FindStringSubmatch(body); len(mm) > 1 {
		return strings.TrimSpace(mm[1])
	}
	return ""
}

func markdownSection(body, name string) string {
	body = normalizeInlineHeadings(body)
	name = strings.ToLower(strings.TrimSpace(name))
	lines := strings.Split(body, "\n")
	start := -1
	level := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		title := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
		if start == -1 {
			if markdownHeadingMatches(title, name) {
				start = i
				level = hashes
			}
			continue
		}
		if hashes <= level {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start == -1 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

func normalizeInlineHeadings(body string) string {
	return reInlineHeading.ReplaceAllString(body, "\n$1")
}

func markdownHeadingMatches(title, want string) bool {
	title = strings.TrimSpace(strings.ToLower(title))
	want = strings.TrimSpace(strings.ToLower(want))
	return title == want || strings.HasPrefix(title, want+" ") || strings.HasPrefix(title, want+" -") || strings.HasPrefix(title, want+" --")
}

func firstQuotedOrBold(section string) string {
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">"))
		if line == "" {
			continue
		}
		if idx := strings.Index(line, "**"); idx >= 0 {
			rest := line[idx+2:]
			if end := strings.Index(rest, "**"); end >= 0 {
				return strings.Trim(strings.TrimSpace(rest[:end]), `"'`)
			}
		}
		if idx := strings.Index(line, `"`); idx >= 0 {
			rest := line[idx+1:]
			if end := strings.Index(rest, `"`); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
		}
	}
	return ""
}

func firstContentLine(section string) string {
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.Trim(line, "* ")
	}
	return ""
}

func extractNarrativeReviewTrigger(body string) string {
	if mm := reNarrativeReview.FindStringSubmatch(body); mm != nil {
		return truncateText(strings.TrimSpace(mm[0]), 240)
	}
	return ""
}

func truncateText(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if max > 0 && len(s) > max {
		return strings.TrimSpace(s[:max])
	}
	return s
}

func decisionQuote(title, body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
		if len(strings.Join(lines, " ")) >= 240 {
			break
		}
	}
	quote := strings.TrimSpace(strings.Join(lines, " "))
	if len(quote) > 320 {
		quote = quote[:320]
	}
	if quote == "" {
		return title
	}
	return title + "\n" + quote
}

func digest12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
