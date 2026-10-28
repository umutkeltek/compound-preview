package decisions

// Record is the decide->move edge (grants-as-gates, 2026-07-10): an operator
// answer to a card becomes a decision object with --reviewed-by provenance
// and per-record undo, instead of a worklog line or a brain paragraph a
// future session has to remember. `compound decisions record` writes a
// DEC-style markdown file under the HOME's decisions/operator/ dir (never
// the repo, never a caller's real ~/.compound outside a test temp home) and
// imports it through the SAME ImportPolicyDecision path every other policy
// decision uses, so it lands in the store with real lifecycle qualifiers —
// grant/freeze-class records the existing admission seams can consume.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	homepkg "compound/internal/home"
	"compound/internal/mem"
	"compound/internal/policy"
)

// defaultRecordFalsifier / defaultRecordReviewTrigger let an operator record
// a grant/freeze/ruling without hand-authoring lifecycle proof every time;
// VerdictForCandidate still requires both fields non-empty to promote, so a
// record without an explicit --falsifier/--review-trigger still promotes
// (falsifier a needs an ACTIVE, i.e. promoted, grant to resolve).
const (
	defaultRecordFalsifier     = "A later decisions record explicitly supersedes this one (--undo-of); prose/worklog edits never change it."
	defaultRecordReviewTrigger = "Re-check when a superseding decisions record names this record's id."
)

// RecordInput carries the operator-facing fields for `compound decisions
// record --kind grant|freeze|ruling --reviewed-by <operator>`.
type RecordInput struct {
	Kind               string   // grant | freeze | ruling (required)
	ID                 string   // optional; a stable slug is generated from kind+title when empty
	Title              string   // required; the decision choice/title
	ReviewedBy         string   // required; refused without it (falsifier d, 664e1a7 parity)
	Scope              string   // e.g. "director-apply" for a grant
	OwnerGlobs         []string // freeze scope: literal owner globs (settled design decision 2)
	Keywords           []string // freeze scope: literal predicate/evidence keywords
	Domains            []string
	Falsifier          string
	ReviewTrigger      string
	UndoOf             string // optional; this record supersedes the named decision id (falsifier c)
	AutoAdmitClass     string // autonomy grant: exact director class (first live class: job_health)
	MinTerminalSamples int    // autonomy grant: deterministic latest-terminal sample count
	MinWindowDays      int    // autonomy grant: minimum span between oldest/newest selected sample
	MaxFailures        int    // autonomy grant: maximum failed/blocked/no-op receipts in that window
	// A worktree-keep record (Scope == WorktreeKeepScope, Kind grant) reuses the
	// existing operator flags — no new CLI surface: the single Keywords entry is
	// the protected branch ref (refs/heads/<name>), Title is the required reason,
	// and ReviewTrigger is the required revisit condition. Admission validation
	// enforces all three below; the branch ref lands in WorktreeBranchRef, not in
	// the freeze keyword scope.
	Now time.Time
}

// RecordReport is what `compound decisions record` prints/returns: where the
// markdown landed and the resulting decision import (evidence/candidate/
// claim + typed lifecycle).
type RecordReport struct {
	Path   string               `json:"path"`
	Import DecisionImportReport `json:"import"`
}

// Record writes the operator decision markdown and imports it through
// ImportPolicyDecision. It refuses (writing nothing) without --kind in
// grant|freeze|ruling, without --title, or without --reviewed-by.
func Record(store *mem.Store, homePath string, in RecordInput) (RecordReport, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return RecordReport{}, fmt.Errorf("decisions record: COMPOUND_HOME required")
	}
	kind := strings.TrimSpace(in.Kind)
	switch kind {
	case KindGrant, KindFreeze, KindRuling:
	default:
		return RecordReport{}, fmt.Errorf("decisions record: --kind must be one of grant|freeze|ruling, got %q", in.Kind)
	}
	reviewedBy := strings.TrimSpace(in.ReviewedBy)
	if reviewedBy == "" {
		return RecordReport{}, fmt.Errorf("decisions record: --reviewed-by required")
	}
	if !policy.HasOperatorProvenance(reviewedBy) {
		return RecordReport{}, fmt.Errorf("decisions record: operational authority requires operator --reviewed-by provenance")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return RecordReport{}, fmt.Errorf("decisions record: --title required")
	}
	if store == nil {
		return RecordReport{}, fmt.Errorf("decisions record: mem store required")
	}
	autoClass := strings.TrimSpace(in.AutoAdmitClass)
	if autoClass != "" || in.MinTerminalSamples != 0 || in.MinWindowDays != 0 || in.MaxFailures != 0 {
		if kind != KindGrant {
			return RecordReport{}, fmt.Errorf("decisions record: autonomy thresholds require --kind grant")
		}
		if strings.TrimSpace(in.Scope) != AutonomyGrantScope {
			return RecordReport{}, fmt.Errorf("decisions record: autonomy thresholds require --scope %s", AutonomyGrantScope)
		}
		if autoClass == "" {
			return RecordReport{}, fmt.Errorf("decisions record: autonomy thresholds require --auto-admit-class")
		}
		if in.MinTerminalSamples <= 0 {
			return RecordReport{}, fmt.Errorf("decisions record: --min-terminal-samples must be positive")
		}
		if in.MinWindowDays <= 0 {
			return RecordReport{}, fmt.Errorf("decisions record: --min-window-days must be positive")
		}
		if in.MaxFailures < 0 || in.MaxFailures >= in.MinTerminalSamples {
			return RecordReport{}, fmt.Errorf("decisions record: --max-failures must be >= 0 and less than --min-terminal-samples")
		}
	}
	// Worktree-keep scope validation mirrors the autonomy-scope block: it reuses
	// existing flags (Keywords=branch ref, Title=reason, ReviewTrigger=revisit)
	// and refuses a keep that cannot pin an exact branch or lacks a revisit
	// condition. Reason non-emptiness is already guaranteed by the --title check.
	var worktreeKeepBranchRef string
	if strings.TrimSpace(in.Scope) == WorktreeKeepScope {
		if kind != KindGrant {
			return RecordReport{}, fmt.Errorf("decisions record: --scope %s requires --kind grant", WorktreeKeepScope)
		}
		keepKeywords := cleanRecordStrings(in.Keywords)
		if len(keepKeywords) != 1 {
			return RecordReport{}, fmt.Errorf("decisions record: --scope %s requires exactly one --keyword branch ref", WorktreeKeepScope)
		}
		worktreeKeepBranchRef = keepKeywords[0]
		if !strings.HasPrefix(worktreeKeepBranchRef, "refs/heads/") || strings.TrimSpace(strings.TrimPrefix(worktreeKeepBranchRef, "refs/heads/")) == "" {
			return RecordReport{}, fmt.Errorf("decisions record: --scope %s branch ref must start with refs/heads/, got %q", WorktreeKeepScope, worktreeKeepBranchRef)
		}
		if strings.TrimSpace(in.ReviewTrigger) == "" {
			return RecordReport{}, fmt.Errorf("decisions record: --scope %s requires a --review-trigger revisit condition", WorktreeKeepScope)
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		if worktreeKeepBranchRef != "" {
			// Key the stable id by branch ref so keeps for distinct branches with an
			// identical reason never collide onto one decision id.
			id = "operator-worktree-keep-" + digest12(worktreeKeepBranchRef+"|"+title)
		} else {
			id = "operator-" + kind + "-" + digest12(title+"|"+kind)
		}
	}
	falsifier := strings.TrimSpace(in.Falsifier)
	if falsifier == "" {
		falsifier = defaultRecordFalsifier
	}
	reviewTrigger := strings.TrimSpace(in.ReviewTrigger)
	if reviewTrigger == "" {
		reviewTrigger = defaultRecordReviewTrigger
	}

	// For a worktree-keep the single keyword IS the protected branch ref, not a
	// freeze predicate keyword — it lands in WorktreeBranchRef and the freeze
	// scope stays empty so the record never reads as a freeze.
	freezeOwnerGlobs := cleanRecordStrings(in.OwnerGlobs)
	freezeKeywords := cleanRecordStrings(in.Keywords)
	if worktreeKeepBranchRef != "" {
		freezeOwnerGlobs = nil
		freezeKeywords = nil
	}
	fm := decFrontmatter{
		ID:                 id,
		Title:              title,
		Kind:               kind,
		Status:             StatusAccepted,
		Owner:              reviewedBy,
		ReviewedBy:         reviewedBy,
		Scope:              strings.TrimSpace(in.Scope),
		Domains:            cleanRecordStrings(in.Domains),
		FreezeOwnerGlobs:   freezeOwnerGlobs,
		FreezeKeywords:     freezeKeywords,
		AutoAdmitClass:     autoClass,
		MinTerminalSamples: in.MinTerminalSamples,
		MinWindowDays:      in.MinWindowDays,
		MaxFailures:        in.MaxFailures,
		Falsifier:          falsifier,
		ReviewTrigger:      reviewTrigger,
		WorktreeBranchRef:  worktreeKeepBranchRef,
	}
	if undoOf := strings.TrimSpace(in.UndoOf); undoOf != "" {
		fm.Supersedes = []string{undoOf}
	}
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return RecordReport{}, err
	}
	var body strings.Builder
	body.WriteString("---\n")
	body.Write(fmBytes)
	body.WriteString("---\n\n")
	fmt.Fprintf(&body, "# %s\n\n", title)
	fmt.Fprintf(&body, "Recorded via `compound decisions record --kind %s --reviewed-by %s`.\n", kind, reviewedBy)

	var out RecordReport
	err = homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		dir := filepath.Join(homePath, "decisions", "operator")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, id+".md")
		if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
			return err
		}
		report, err := importPolicyDecisionLocked(store, path, now)
		if err != nil {
			return err
		}
		out = RecordReport{Path: path, Import: report}
		return nil
	})
	return out, err
}

func cleanRecordStrings(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
