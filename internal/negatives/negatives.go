// Package negatives is the read-only, rebuildable projection over evidence that an
// approach was already tried and failed: inbox/failure-patterns/ (candidates.FailurePattern,
// written by the distiller — internal/distiller) and reviews/route-remorse/
// (routeevidence.RouteOutcomeReview, written by internal/routeevidence). See
// docs_private/plan/packets/2026-07-10-negatives-at-orient.md ("cite, don't
// redesign"): both producers already exist and are untouched by this package.
//
// It closes Weng harness-RSI map item 6 — "negative results first-class recallable"
// — by giving orient (internal/contract's ContextPack) something to join against.
// Before this package the only propose-time reader of route-remorse was
// internal/director's loadRouteRemorseReviews, which turns remorse into harness-edit
// PROPOSALS for the operator (a PROPOSER). This package WARNS an executor about to
// walk a path a prior lane already burned — read-only display, never routing.
//
// Projection discipline mirrors internal/mem/temperature.go: rebuilt from scratch on
// every call (a rejected/resolved source record disappears on rebuild, never a stale
// tombstone), written only under projections/ (never the ledger). This package reads
// inbox/failure-patterns and reviews/route-remorse as plain JSON directories rather
// than importing internal/distiller or internal/routeevidence — negatives sits below
// both object families (it is a Leaf, like internal/attention), so it takes its own
// minimal decode of the record shapes it needs instead of betting on an upward
// import; the route-remorse schema version is duplicated as a local constant on
// purpose (see routeOutcomeReviewSchemaVersion), the same tradeoff temperature.go
// documents for capabilityUsageRelPath.
package negatives

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const (
	// SchemaVersion versions the projection body.
	SchemaVersion = "compound.negatives.v0.1"
	// ProjectionRef is the stable ref for this generated view.
	ProjectionRef = "compound://projections/negatives"
	// RelPath is the home-relative path the projection is written to.
	RelPath = "projections/negatives.json"

	// SourceFailurePattern / SourceRouteRemorse tag which producer a Record came
	// from — the falsifier that both producers surface through the same section
	// reads this field to prove it is one reader, two producers.
	SourceFailurePattern = "failure_pattern"
	SourceRouteRemorse   = "route_remorse"

	// routeRemorseReviewArea mirrors routeevidence's own (unexported)
	// routeOutcomeReviewArea constant: reviews/route-remorse/*.json core artifacts
	// written by routeevidence.RecordRouteOutcomeReview.
	routeRemorseReviewArea = "route-remorse"

	// routeOutcomeReviewSchemaVersion duplicates routeevidence.RouteOutcomeReviewSchemaVersion
	// without an upward import (see package doc).
	routeOutcomeReviewSchemaVersion = "compound.route_outcome_review.v0.1"

	// minScopeTokenLen matches the codebase's established ambient-recall token floor
	// (internal/contract's memoryTextTokens): short tokens are dropped as noise.
	minScopeTokenLen = 3

	// maxApproachKeySlugLen bounds a derived approach key so a long Statement never
	// turns into an unwieldy ref.
	maxApproachKeySlugLen = 60
)

// Record is one already-tried-and-failed approach, scoped for a deterministic join at
// orient time. Owner/TaskClass/ApproachKey are the scope key the packet specifies;
// Owner defaults to "" (unscoped — matches any owner) because neither producer
// carries an owner field today, so a record narrows on owner only when its evidence
// explicitly tags one (an "owner:<value>" evidence ref) — see ownerFromEvidenceRefs.
// WhatWasTried/WhyItFailed are reduced to "" at build time when the source free text
// looked operator-personal, so a stale personal string can never leak through a
// rebuild — ApproachKey and Ref remain so the record still cites its source.
type Record struct {
	Source       string  `json:"source"`
	RefID        string  `json:"ref_id"`
	Owner        string  `json:"owner,omitempty"`
	TaskClass    string  `json:"task_class,omitempty"`
	ApproachKey  string  `json:"approach_key"`
	WhatWasTried string  `json:"what_was_tried,omitempty"`
	WhyItFailed  string  `json:"why_it_failed,omitempty"`
	Ref          string  `json:"ref"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// Projection is the full derived surface written under projections/.
type Projection struct {
	SchemaVersion string              `json:"schema_version"`
	ProjectionRef string              `json:"projection_ref"`
	ComputedAt    string              `json:"computed_at"`
	Records       []Record            `json:"records"`
	Warnings      []ProjectionWarning `json:"warnings,omitempty"`
}

// ProjectionWarning is a provider-safe aggregate for source rows that a
// rebuild excluded. The exact filename remains available from Build's returned
// candidates.Corruption error, while the read-side ContextPack can account for
// the exclusion without injecting operator-controlled filenames or paths.
type ProjectionWarning struct {
	Source string                    `json:"source"`
	Code   candidates.CorruptionCode `json:"code"`
	Count  int                       `json:"count"`
}

// BuildOptions parameterizes the build. Now is the ComputedAt stamp; inject it for
// determinism in tests.
type BuildOptions struct {
	Now time.Time
}

// Build reads the two existing producers fresh from homePath and returns the
// negatives projection. It never mutates either producer; the caller persists the
// result via WriteProjection. Rebuilding is lossless for unchanged inputs and drops
// any record whose source has since been dismissed/resolved (never a stale
// tombstone) — see readFailurePatternRecords / readRouteRemorseRecords.
func Build(homePath string, opts BuildOptions) (Projection, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	proj := Projection{
		SchemaVersion: SchemaVersion,
		ProjectionRef: ProjectionRef,
		ComputedAt:    now.UTC().Format(time.RFC3339),
		Records:       []Record{},
		Warnings:      []ProjectionWarning{},
	}
	fp, fpErr := readFailurePatternRecords(homePath)
	rr, rrErr := readRouteRemorseRecords(homePath)
	proj.Warnings = candidateCorruptionWarnings(fpErr)
	proj.Records = append(proj.Records, fp...)
	proj.Records = append(proj.Records, rr...)
	sort.Slice(proj.Records, func(i, j int) bool {
		if proj.Records[i].Source != proj.Records[j].Source {
			return proj.Records[i].Source < proj.Records[j].Source
		}
		return proj.Records[i].RefID < proj.Records[j].RefID
	})
	return proj, errors.Join(fpErr, rrErr)
}

// WriteProjection persists the projection under projections/, returning the absolute
// path written. This is derived state; it must never touch the ledger.
func WriteProjection(homePath string, proj Projection) (string, error) {
	path := filepath.Join(homePath, filepath.FromSlash(RelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ReadProjection loads the persisted projection. The bool is false (with no error)
// when the projection has not been built yet — an expected, not exceptional, state
// (fail-open, mirroring mem.ReadTemperature).
func ReadProjection(homePath string) (Projection, bool, error) {
	path := filepath.Join(homePath, filepath.FromSlash(RelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Projection{}, false, nil
		}
		return Projection{}, false, err
	}
	var proj Projection
	if err := json.Unmarshal(data, &proj); err != nil {
		return Projection{}, false, err
	}
	return proj, true, nil
}

// StaleAfter bounds how old a PERSISTED projection may be before a reader must
// rebuild rather than trust it. Nothing in-repo schedules `negatives build` (the
// job roster is the operator's external cron registry, not a compound scheduler),
// so without this bound the persisted file is only as fresh as the last manual
// build — observed 2026-07-25: 15 days stale holding 0 records while two real
// failure patterns sat in inbox/failure-patterns, which made every scope-matched
// negative invisible to every session. Mirrors the 6h window
// internal/routeevidence already uses for account-projection freshness.
const StaleAfter = 6 * time.Hour

// ReadFresh returns the projection a READER should use: the persisted body when it
// is present and younger than StaleAfter, otherwise a fresh in-memory Build.
//
// It NEVER persists. Orient is a read path — a hidden write there would surprise
// concurrent lanes and make a read-only projection a mutation. The cost of the
// common (fresh) case is one file read plus a timestamp compare; the cost of the
// stale case is Build's two small directory scans, which is strictly cheaper than
// the failure it removes (silently serving an empty view of real evidence).
//
// The bool is false only when there is nothing to offer at all, preserving
// ReadProjection's fail-open contract for callers.
func ReadFresh(homePath string, now time.Time) (Projection, bool, error) {
	proj, ok, err := ReadProjection(homePath)
	if err != nil {
		return Projection{}, false, err
	}
	if ok && !projectionStale(proj, now) {
		return proj, true, nil
	}
	built, buildErr := Build(homePath, BuildOptions{Now: now})
	if buildErr != nil {
		// Canonical candidate corruption is an accounted source exclusion, not
		// a reason to hide valid sibling records. Build keeps the exact named
		// corruption in its error for operators and records a provider-safe
		// aggregate in the projection for ContextPack readers.
		if onlyCandidateCorruptions(buildErr) {
			return built, true, nil
		}
		// Fail open to whatever was persisted rather than failing orient: a stale
		// view still beats no view, and a build error must not break the pack.
		if ok {
			return proj, true, nil
		}
		return Projection{}, false, nil
	}
	return built, true, nil
}

func candidateCorruptionWarnings(err error) []ProjectionWarning {
	counts := map[candidates.CorruptionCode]int{}
	visitCandidateCorruptions(err, func(corruption *candidates.Corruption) {
		counts[corruption.Code]++
	})
	codes := make([]string, 0, len(counts))
	for code := range counts {
		codes = append(codes, string(code))
	}
	sort.Strings(codes)
	warnings := make([]ProjectionWarning, 0, len(codes))
	for _, code := range codes {
		warnings = append(warnings, ProjectionWarning{
			Source: SourceFailurePattern,
			Code:   candidates.CorruptionCode(code),
			Count:  counts[candidates.CorruptionCode(code)],
		})
	}
	return warnings
}

func onlyCandidateCorruptions(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyCandidateCorruptions(child) {
				return false
			}
		}
		return true
	}
	var corruption *candidates.Corruption
	return errors.As(err, &corruption)
}

func visitCandidateCorruptions(err error, visit func(*candidates.Corruption)) {
	if err == nil {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			visitCandidateCorruptions(child, visit)
		}
		return
	}
	var corruption *candidates.Corruption
	if errors.As(err, &corruption) {
		visit(corruption)
	}
}

// projectionStale reports whether a persisted projection's ComputedAt is older than
// StaleAfter. An unparseable or empty ComputedAt counts as stale: a body that cannot
// prove its own age must never be trusted as fresh.
func projectionStale(proj Projection, now time.Time) bool {
	computed, err := time.Parse(time.RFC3339, strings.TrimSpace(proj.ComputedAt))
	if err != nil {
		return true
	}
	return now.UTC().Sub(computed.UTC()) > StaleAfter
}

// ScopeTokens tokenizes free text into the deterministic, no-LLM scope-match
// vocabulary: lowercased content runs of length >= minScopeTokenLen. It mirrors
// internal/contract's own memoryTextTokens tokenizer so callers reuse the same
// "is this in scope" primitive the ambient-memory ranker already established.
func ScopeTokens(values ...string) map[string]bool {
	tokens := map[string]bool{}
	for _, value := range values {
		for _, part := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		}) {
			if len(part) >= minScopeTokenLen {
				tokens[part] = true
			}
		}
	}
	return tokens
}

// inScope is the deterministic "scope match, not dump" gate: a record with a
// non-empty Owner only surfaces when the query's owner tokens name it; every record
// must also share at least one token with the query's task tokens (derived from its
// own ApproachKey + TaskClass) — a disjoint task never matches, no matter how
// severe the recorded failure was.
func (r Record) inScope(ownerTokens, taskTokens map[string]bool) bool {
	if owner := strings.TrimSpace(r.Owner); owner != "" && !ownerTokens[owner] {
		return false
	}
	scope := ScopeTokens(r.ApproachKey, r.TaskClass)
	if len(scope) == 0 {
		return false
	}
	for token := range scope {
		if taskTokens[token] {
			return true
		}
	}
	return false
}

// MatchingRecords returns the records in scope for the given owner/task query
// tokens, deterministically ordered (source, then ref id) and capped to limit (<=0
// means unbounded) — the read-only orient-time join. It never inspects or affects
// route/dispatch/claim state; it is a pure filter over already-built records.
func MatchingRecords(records []Record, ownerTokens, taskTokens map[string]bool, limit int) []Record {
	var out []Record
	for _, r := range records {
		if r.inScope(ownerTokens, taskTokens) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].RefID < out[j].RefID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// personalPatterns is a minimal, deterministic "belongs to the operator's machine,
// not to the finding" heuristic: an email-like token, an absolute home-directory
// path, or a scratch/temp root. It is intentionally narrow (no NLP, no model call) —
// good enough to catch a planted personal string and reduce free text to structured
// form before it is ever persisted or displayed.
//
// The scratch roots are here because a negative card is provider-bound: it renders
// into the harness digest, so anything it carries is sent to a model provider.
// Observed live before this gate existed (find_7c177ad1dd32): a card shipped a
// `tried=method:/private/tmp/<run-scratch-dir>/<file>` value into a real prompt
// injection, disclosing machine layout and a session-random directory name for a
// path no reader can act on anyway. A home-path gate that stops at $HOME misses
// every macOS temp path, because they resolve under /private, not the home tree.
//
// Fail closed: a prose mention of a scratch root blanks the field rather than
// attempting to excise the path. A negative card with no `tried=` still warns; a
// card that leaks the operator's filesystem cannot be unsent.
var personalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`),
	regexp.MustCompile(`/Users/[A-Za-z0-9_.-]+`),
	regexp.MustCompile(`(?i)/home/[A-Za-z0-9_.-]+`),
	// macOS reports temp dirs under /private (/private/tmp, /private/var/folders/...);
	// Linux and CI use /tmp and /var/tmp. Matched with or without the /private prefix.
	regexp.MustCompile(`(?i)(/private)?/(tmp|var/tmp)(/|\b)`),
	regexp.MustCompile(`(?i)(/private)?/var/folders(/|\b)`),
	// Executor worktrees are per-lane scratch checkouts; their names carry lane and
	// date identifiers that are machine state, not portable evidence.
	regexp.MustCompile(`(?i)/worktrees/[A-Za-z0-9_.-]+`),
}

// cleanApproachKey returns a readable scope key from the first field that is safe to
// slugify, or "" when neither is. Keeping a clean subject is what preserves the
// card's ability to match a task at all; see the per-field redaction note in
// loadFailurePatterns.
func cleanApproachKey(raw failurePatternRaw, subjectPersonal, statementPersonal bool) string {
	if !subjectPersonal {
		if key := reviewartifact.Slugify(raw.Subject, 0); key != "" {
			return key
		}
	}
	if !statementPersonal {
		if key := reviewartifact.Slugify(raw.Statement, maxApproachKeySlugLen); key != "" {
			return key
		}
	}
	return ""
}

func looksPersonal(values ...string) bool {
	for _, value := range values {
		if strings.Contains(value, "~/") {
			return true
		}
		for _, p := range personalPatterns {
			if p.MatchString(value) {
				return true
			}
		}
	}
	return false
}

// ownerFromEvidenceRefs derives the optional Owner scope dimension from an
// "owner:<value>" evidence ref, a lightweight additive convention on the existing
// free-form evidence_refs[] field — no producer schema change required. Absent the
// convention, Owner stays "" (unscoped: the record narrows on task-class only).
func ownerFromEvidenceRefs(refs []string) string {
	for _, ref := range refs {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ref), "owner:"); ok {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				return v
			}
		}
	}
	return ""
}

// failurePatternRaw is the minimal decode shape of an inbox/failure-patterns/*.json
// candidate record (compound/internal/distiller.Candidate's JSON projection) — see
// the package doc for why this duplicates rather than imports distiller.
type failurePatternRaw struct {
	ID           string   `json:"id"`
	Subject      string   `json:"subject"`
	Statement    string   `json:"statement"`
	Detail       string   `json:"detail"`
	Confidence   float64  `json:"confidence"`
	EvidenceRefs []string `json:"evidence_refs"`
	Verdict      string   `json:"verdict"`
}

// readFailurePatternRecords reads failure patterns through candidates' descriptor-
// rooted custody walk. Each corrupt entry is excluded and returned as its named
// custody finding, while valid later entries remain eligible for the projection.
// Park/Operationalize verdicts become Records; Reject (dismissed — "not a real
// failure") is dropped, so a rejected candidate never becomes a stale tombstone.
func readFailurePatternRecords(homePath string) ([]Record, error) {
	page, err := candidates.ReadPage(homePath, candidates.FailurePattern, "", 0)
	if err != nil {
		return nil, err
	}
	var out []Record
	var corruptions []error
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			corruptions = append(corruptions, entry.Corruption)
			continue
		}
		if entry.Record == nil {
			corruptions = append(corruptions, &candidates.Corruption{
				EntryCursor: entry.EntryCursor,
				Code:        candidates.CorruptionRaced,
				Detail:      "candidate page entry has no custody outcome",
			})
			continue
		}
		var raw failurePatternRaw
		if err := json.Unmarshal(entry.Record.Body, &raw); err != nil {
			corruptions = append(corruptions, &candidates.Corruption{
				EntryCursor: entry.EntryCursor,
				Code:        candidates.CorruptionInvalidJSON,
				Detail:      fmt.Sprintf("decode failure-pattern candidate body: %v", err),
			})
			continue
		}
		if raw.ID != entry.Record.ID {
			corruptions = append(corruptions, &candidates.Corruption{
				EntryCursor: entry.EntryCursor,
				Code:        candidates.CorruptionIDMismatch,
				Detail:      fmt.Sprintf("decoded id %q, want %q", raw.ID, entry.Record.ID),
			})
			continue
		}
		if raw.Verdict == string(candidates.Reject) {
			continue // dismissed: this was not a valid failure — drop, never surface
		}
		// Redact PER FIELD, not per record. A record is a mix of independent fields
		// and one dirty field does not make the others unsafe. Blanking all three
		// together also blanks ApproachKey and TaskClass, which are the ONLY inputs
		// to inScope — so an all-or-nothing gate does not redact the card, it
		// silently deletes it: the record survives in the projection but can never
		// match a task again. Observed on a live record whose clean, matchable
		// subject was discarded because its statement carried a scratch path.
		//
		// A personal Subject/Statement must still never seed the approach key, since
		// Slugify only strips separators and does not redact — hence the per-source
		// checks below rather than one slug of whichever field is non-empty.
		subjectPersonal := looksPersonal(raw.Subject)
		statementPersonal := looksPersonal(raw.Statement)
		detailPersonal := looksPersonal(raw.Detail)

		approachKey := "fp-" + reviewartifact.Digest12(raw.ID)
		if candidate := cleanApproachKey(raw, subjectPersonal, statementPersonal); candidate != "" {
			approachKey = candidate
		}

		whatTried := strings.TrimSpace(raw.Statement)
		if statementPersonal {
			whatTried = ""
		}
		whyFailed := reviewartifact.FirstNonEmpty(raw.Detail, "flagged as a recorded failure pattern")
		taskClass := strings.TrimSpace(raw.Detail)
		if detailPersonal {
			whyFailed, taskClass = "", ""
		}
		out = append(out, Record{
			Source:       SourceFailurePattern,
			RefID:        raw.ID,
			Owner:        ownerFromEvidenceRefs(raw.EvidenceRefs),
			TaskClass:    taskClass,
			ApproachKey:  approachKey,
			WhatWasTried: whatTried,
			WhyItFailed:  whyFailed,
			Ref:          "candidate://" + string(candidates.FailurePattern) + "/" + raw.ID,
			Confidence:   raw.Confidence,
		})
	}
	return out, errors.Join(corruptions...)
}

// routeOutcomeReviewRaw is the minimal decode shape of a
// reviews/route-remorse/*.json core artifact (routeevidence.RouteOutcomeReview's
// JSON projection) — see the package doc for why this duplicates rather than
// imports routeevidence.
type routeOutcomeReviewRaw struct {
	SchemaVersion     string   `json:"schema_version"`
	ID                string   `json:"id"`
	CapabilityClasses []string `json:"capability_classes"`
	EvidenceRefs      []string `json:"evidence_refs"`
	ReceiptOutcome    string   `json:"receipt_outcome"`
	SelectedTarget    struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	} `json:"selected_target"`
	Assessment struct {
		Class              string `json:"class"`
		ShouldReviewPolicy bool   `json:"should_review_policy"`
		Rationale          string `json:"rationale"`
	} `json:"assessment"`
}

// readRouteRemorseRecords reads every reviews/route-remorse/*.json core artifact
// (skipping the .reviewed./.apply./.decision. siblings, the same convention
// internal/director's loadRouteRemorseReviews uses) and turns should_review_policy
// reviews into Records; a review whose assessment no longer flags a policy concern
// (should_review_policy=false — resolved) is dropped, so it never becomes a stale
// tombstone.
func readRouteRemorseRecords(homePath string) ([]Record, error) {
	dir := filepath.Join(homePath, "reviews", routeRemorseReviewArea)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.Contains(name, ".reviewed.") || strings.Contains(name, ".apply.") || strings.Contains(name, ".decision.") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var raw routeOutcomeReviewRaw
		if json.Unmarshal(data, &raw) != nil {
			continue
		}
		if raw.SchemaVersion != routeOutcomeReviewSchemaVersion || strings.TrimSpace(raw.ID) == "" {
			continue
		}
		if !raw.Assessment.ShouldReviewPolicy {
			continue // resolved: no policy concern anymore — drop, never surface
		}
		routeKey := strings.Trim(strings.Join([]string{raw.SelectedTarget.Provider, raw.SelectedTarget.Model, raw.SelectedTarget.Protocol}, "/"), "/")
		approachKey := reviewartifact.Slugify(routeKey, 0)
		if approachKey == "" {
			approachKey = "ror-" + reviewartifact.Digest12(raw.ID)
		}
		taskClass := ""
		if len(raw.CapabilityClasses) > 0 {
			taskClass = strings.TrimSpace(raw.CapabilityClasses[0])
		}
		whatTried := strings.TrimSpace(fmt.Sprintf("routed to %s", reviewartifact.FirstNonEmpty(routeKey, raw.ID)))
		whyFailed := reviewartifact.FirstNonEmpty(raw.Assessment.Rationale, raw.ReceiptOutcome, "route outcome flagged for policy review")
		// Per field, for the same reason as loadFailurePatterns: whatTried is built
		// from registry names (provider/model/protocol) while whyFailed carries a
		// free-text rationale, so the rationale is the field that can go dirty. One
		// check over both blanked the clean route name along with it.
		if looksPersonal(whatTried) {
			whatTried = ""
		}
		if looksPersonal(whyFailed) {
			whyFailed = ""
		}
		out = append(out, Record{
			Source:       SourceRouteRemorse,
			RefID:        raw.ID,
			Owner:        ownerFromEvidenceRefs(raw.EvidenceRefs),
			TaskClass:    taskClass,
			ApproachKey:  approachKey,
			WhatWasTried: whatTried,
			WhyItFailed:  whyFailed,
			Ref:          "route-remorse://" + raw.ID,
			Confidence:   1,
		})
	}
	return out, nil
}
