package harvest

// Backfill is the ruling harvester pointed BACKWARDS over the historical decision
// ledgers (Phase 2 of the ruling-harvest packet). It rides the exact slice-A
// rails — Classify and Reconcile are reused unchanged — and adds only what the
// input shape demands: a deterministic parser for the distilled
// `decision-ledger.generated.md` grammar and a slot-identity reconcile pass for
// the short-historical-utterance vs long-accepted-record asymmetry that
// token-coverage alone cannot bridge.
//
// The organ is the same, the direction is reversed:
//   - Parse (no LLM): a ledger entry -> a normalized candidate row.
//   - Route deterministically: a system/harness ruling flows to the reconcile
//     pipeline; a product decision is collected into a per-product report (NOT a
//     compound record, NOT a brain write); an ephemeral one-off is counted and
//     skipped WITH a reason (no silent drops).
//   - Order backwards from now (updated_at desc): a cluster's head is the newest
//     statement; older instances attach as corroboration/provenance, so
//     supersession resolves by construction and stale law is never proposed live.
//   - Reconcile-vs-accepted FIRST: a restatement of an accepted record appends
//     corroboration evidence (no card, no duplicate); a reversal surfaces as a
//     parked conflict; a genuinely novel ruling clusters among the run's own
//     novel heads and emits ONE parked candidate per cluster carrying the
//     corroboration count + instance provenance (cluster-granularity, never
//     per-instance — decisions-down).
//   - Bounded honesty: novel candidates are capped per run; what was skipped and
//     the novel-per-500 yield curve are logged, never silently truncated.
//
// Nothing here ever auto-accepts a ruling: novel/conflict candidates land
// verdict=park, and only the operator ratification door can accept them
// (ruling-harvest falsifier e). On a dry-run NOTHING is written — that is the
// live-proof path the packet's falsifier f exercises against the real home.
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

const BackfillReportSchemaVersion = "compound.ruling_backfill_report.v0.1"

// DefaultNovelCap bounds how many novel-cluster candidates a single run may emit
// (packet: "a few dozen genuinely novel candidates, not 9,262"). The rest are
// logged as skipped-by-cap, never silently dropped.
const DefaultNovelCap = 40

// yieldBucket is the entry-count window the novel-per-N yield curve is reported
// over (packet: "novel-per-500-entries").
const yieldBucket = 500

// BackfillRoute is which of the three doors a parsed ledger entry takes.
type BackfillRoute string

const (
	// RouteSystem: a system/harness ruling — goes through the reconcile pipeline
	// against the accepted decision records.
	RouteSystem BackfillRoute = "system"
	// RouteProduct: a product decision — collected into a per-product report,
	// never a compound record or a brain write.
	RouteProduct BackfillRoute = "product"
	// RouteEphemeral: a one-off with no durable system/product meaning — counted
	// and skipped with a reason.
	RouteEphemeral BackfillRoute = "ephemeral"
)

// LedgerEntry is one parsed `- **[SELECT]** ...` decision, normalized off the
// distilled ledger grammar. Kind is the bracket token (always SELECT in this
// corpus) kept verbatim so a future ledger with other action tokens parses too.
type LedgerEntry struct {
	Body             string `json:"body"`
	Kind             string `json:"kind"`
	Confidence       string `json:"confidence,omitempty"`
	Domain           string `json:"domain,omitempty"`
	Quote            string `json:"quote,omitempty"`
	SourceTranscript string `json:"source_transcript,omitempty"`
	LedgerPath       string `json:"ledger_path"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	LineNo           int    `json:"line_no"`
}

// EvidenceRef is the citable provenance of one historical instance.
func (e LedgerEntry) EvidenceRef() string {
	return fmt.Sprintf("ledger:%s#L%d", e.LedgerPath, e.LineNo)
}

// BackfillOptions controls one backfill run over a ledger directory.
type BackfillOptions struct {
	LedgerDir string
	DryRun    bool
	NovelCap  int
	Now       time.Time
}

// RouteCounts is the no-silent-drops accounting: every parsed entry lands in
// exactly one route.
type RouteCounts struct {
	System    int `json:"system"`
	Product   int `json:"product"`
	Ephemeral int `json:"ephemeral"`
}

// CorroborationRecord is one accepted record the backfill reinforced, with the
// historical instances that restated it.
type CorroborationRecord struct {
	ClaimID    string   `json:"claim_id"`
	DecisionID string   `json:"decision_id,omitempty"`
	Object     string   `json:"object,omitempty"`
	Via        string   `json:"via"` // "object" | "slot"
	Count      int      `json:"count"`
	Instances  []string `json:"instances"`
	EvidenceID string   `json:"evidence_id,omitempty"`
}

// NovelCluster is one same-meaning family of novel rulings. The head is the
// NEWEST instance (entries are processed updated_at desc); older instances attach
// as corroboration/provenance. One parked candidate is emitted per cluster.
type NovelCluster struct {
	Head               string   `json:"head"`
	Kind               string   `json:"kind"`
	Domain             string   `json:"domain,omitempty"`
	CorroborationCount int      `json:"corroboration_count"`
	Instances          []string `json:"instances"`
	CandidateID        string   `json:"candidate_id,omitempty"`
	Emitted            bool     `json:"emitted"`
}

// ConflictRecord is a historical entry that topically matches an accepted record
// but carries a reversal marker — surfaced loudly, never a silent record.
type ConflictRecord struct {
	ClaimID     string `json:"claim_id"`
	DecisionID  string `json:"decision_id,omitempty"`
	Body        string `json:"body"`
	Instance    string `json:"instance"`
	CandidateID string `json:"candidate_id,omitempty"`
}

// ProductReport is the per-product collection (report only, never a record).
type ProductReport struct {
	Product   string         `json:"product"`
	Count     int            `json:"count"`
	Entries   []ProductEntry `json:"entries"`
	WrittenTo string         `json:"written_to,omitempty"`
}

// ProductEntry is one product decision kept for the product's own reconciliation.
type ProductEntry struct {
	Body       string `json:"body"`
	Domain     string `json:"domain,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Instance   string `json:"instance"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// EphemeralReason counts why entries were skipped (no silent drops).
type EphemeralReason struct {
	Reason  string   `json:"reason"`
	Count   int      `json:"count"`
	Samples []string `json:"samples,omitempty"`
}

// YieldPoint is one window of the novel-per-500 yield curve.
type YieldPoint struct {
	EntriesProcessed int `json:"entries_processed"`
	NovelClusters    int `json:"novel_clusters"`
}

// ParseIssue records an entry line that did not parse cleanly (no silent drops).
type ParseIssue struct {
	LedgerPath string `json:"ledger_path"`
	LineNo     int    `json:"line_no"`
	Detail     string `json:"detail"`
	Sample     string `json:"sample,omitempty"`
}

// BackfillReport is the full run output.
type BackfillReport struct {
	SchemaVersion      string                `json:"schema_version"`
	Status             string                `json:"status"`
	GeneratedAt        string                `json:"generated_at"`
	LedgerDir          string                `json:"ledger_dir"`
	DryRun             bool                  `json:"dry_run"`
	NovelCap           int                   `json:"novel_cap"`
	FilesParsed        int                   `json:"files_parsed"`
	EntriesTotal       int                   `json:"entries_total"`
	RouteCounts        RouteCounts           `json:"route_counts"`
	AcceptedRecords    int                   `json:"accepted_records"`
	Corroborations     []CorroborationRecord `json:"corroborations"`
	Conflicts          []ConflictRecord      `json:"conflicts"`
	NovelClustersFound int                   `json:"novel_clusters_found"`
	NovelEmitted       int                   `json:"novel_emitted"`
	NovelSkippedByCap  int                   `json:"novel_skipped_by_cap"`
	NovelSamples       []NovelCluster        `json:"novel_samples"`
	ProductReports     []ProductReport       `json:"product_reports"`
	Ephemeral          []EphemeralReason     `json:"ephemeral"`
	YieldCurve         []YieldPoint          `json:"yield_curve"`
	ParseIssues        []ParseIssue          `json:"parse_issues,omitempty"`
	ParseIssueCount    int                   `json:"parse_issue_count"`
	ProofRequired      []string              `json:"proof_required"`
}

var (
	entryLineRe    = regexp.MustCompile(`^\s*-\s+\*\*\[([A-Z]+)\]\*\*\s+(.*)$`)
	inlineMetaRe   = regexp.MustCompile(`(?i)\s*(?:—|--|-)\s*confidence:\s*(high|medium|low)\s*·\s*domain:\s*([a-z0-9][a-z0-9-]*)\s*$`)
	sublineConfRe  = regexp.MustCompile(`(?i)^-\s*confidence:\s*(high|medium|low)(?:\s*·\s*domain:\s*([a-z0-9][a-z0-9-]*))?\s*$`)
	sublineDomRe   = regexp.MustCompile(`(?i)^-\s*domain:\s*([a-z0-9][a-z0-9-]*)\s*$`)
	sublineQuoteRe = regexp.MustCompile(`(?i)^-\s*quote:\s*(.*)$`)
	fmGeneratedRe  = regexp.MustCompile(`(?i)^generated_from:\s*(.+)$`)
	fmUpdatedRe    = regexp.MustCompile(`(?i)^updated_at:\s*(.+)$`)
)

// ParseLedgerFile parses one decision-ledger.generated.md into normalized entries.
// It never errors on a malformed entry line: it records a ParseIssue and moves on
// (no silent drops). A file with no readable entries returns an empty slice.
func ParseLedgerFile(path string) ([]LedgerEntry, []ParseIssue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(data), "\n")

	// Frontmatter: the block between the first two `---` fences.
	var generatedFrom, updatedAt string
	fmEnd := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				fmEnd = i + 1
				break
			}
			if m := fmGeneratedRe.FindStringSubmatch(lines[i]); m != nil {
				generatedFrom = strings.TrimSpace(m[1])
			}
			if m := fmUpdatedRe.FindStringSubmatch(lines[i]); m != nil {
				updatedAt = strings.TrimSpace(m[1])
			}
		}
	}

	var entries []LedgerEntry
	var issues []ParseIssue
	var cur *LedgerEntry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for i := fmEnd; i < len(lines); i++ {
		raw := lines[i]
		lineNo := i + 1
		if m := entryLineRe.FindStringSubmatch(raw); m != nil {
			flush()
			body := strings.TrimSpace(m[2])
			conf, dom := "", ""
			if meta := inlineMetaRe.FindStringSubmatch(body); meta != nil {
				conf = strings.ToLower(meta[1])
				dom = meta[2]
				body = strings.TrimSpace(inlineMetaRe.ReplaceAllString(body, ""))
			}
			if body == "" {
				issues = append(issues, ParseIssue{LedgerPath: path, LineNo: lineNo, Detail: "empty body after [SELECT]", Sample: clip(raw, 120)})
				continue
			}
			cur = &LedgerEntry{
				Body:             body,
				Kind:             m[1],
				Confidence:       conf,
				Domain:           dom,
				SourceTranscript: generatedFrom,
				LedgerPath:       path,
				UpdatedAt:        updatedAt,
				LineNo:           lineNo,
			}
			continue
		}
		if cur == nil {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if m := sublineConfRe.FindStringSubmatch(trimmed); m != nil {
			if cur.Confidence == "" {
				cur.Confidence = strings.ToLower(m[1])
			}
			if cur.Domain == "" && m[2] != "" {
				cur.Domain = m[2]
			}
			continue
		}
		if m := sublineDomRe.FindStringSubmatch(trimmed); m != nil {
			if cur.Domain == "" {
				cur.Domain = m[1]
			}
			continue
		}
		if m := sublineQuoteRe.FindStringSubmatch(trimmed); m != nil {
			cur.Quote = strings.Trim(strings.TrimSpace(m[1]), `"`)
			continue
		}
		// A markdown heading or prose paragraph ends the current entry's sublines.
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			flush()
		}
	}
	flush()
	return entries, issues, nil
}

// ParseLedgerDir walks a directory for every decision-ledger.generated.md and
// parses each. It returns entries in a deterministic order (path, then line).
func ParseLedgerDir(dir string) ([]LedgerEntry, []ParseIssue, int, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "decision-ledger.generated.md" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	sort.Strings(files)
	var all []LedgerEntry
	var issues []ParseIssue
	for _, f := range files {
		entries, iss, err := ParseLedgerFile(f)
		if err != nil {
			return nil, nil, 0, err
		}
		all = append(all, entries...)
		issues = append(issues, iss...)
	}
	return all, issues, len(files), nil
}

// productNameMarkers is the private-overlay extension point for product-NAME
// attribution. The public core stays name-agnostic (this repo is public-destined:
// no real product name may live in a tracked file) and routes product decisions
// by the GENERIC commercial-concern vocabulary below. A running instance's private
// overlay may populate this list to attribute entries to a specific product; when
// empty, product entries are keyed by their commercial concern instead.
var productNameMarkers []string

// productDomainMarkers are per-entry domain slugs that name a product/commercial
// concern (monetization, pricing, ASO, app-store, onboarding, screenshots,
// localization, product-*). Distinct from system/harness domains — generic
// vocabulary, no product names.
var productDomainMarkers = []string{
	"monetization", "pricing", "aso", "app-store", "appstore", "paywall",
	"onboarding", "retention", "screenshot", "localization", "localisation",
	"product-", "revenue", "subscription-tier", "store-listing", "conversion",
	"go-to-market", "gtm", "positioning",
}

// productBodyMarkers catch product-commercial language when the domain is generic.
var productBodyMarkers = []string{
	"app store", "app-store", "paywall", "monetization", "monetisation",
	"pricing tier", "onboarding flow", "screenshot", "aso ", " aso", "keyword ranking",
}

// RouteEntry decides which door a parsed entry takes, deterministically.
// Precedence: an utterance the live classifier already recognizes as a
// standing-law system ruling wins (Classify fires) — otherwise a product-shaped
// decision routes to the product report, then a system/harness-flavored decision
// routes to reconcile, and everything else is an ephemeral one-off (with reason).
func RouteEntry(e LedgerEntry) (BackfillRoute, string) {
	if _, ok := Classify(e.Body); ok {
		return RouteSystem, "classifier fired (standing-law system ruling)"
	}
	if key := productKey(e); key != "" {
		return RouteProduct, "product marker: " + key
	}
	body := strings.ToLower(e.Body)
	dom := strings.ToLower(e.Domain)
	if sig := firstSystemMarker(body + " " + dom); sig != "" {
		return RouteSystem, "system/harness marker: " + sig
	}
	// A decision-shaped body with no system or product signal is an ephemeral
	// one-off (a bare approval, a session-local action). Classify the reason.
	if isTrivialApproval(e.Quote) {
		return RouteEphemeral, "one-off approval, no durable system/product meaning"
	}
	if strings.HasPrefix(dom, "rollout-") || strings.HasPrefix(dom, "transcript-") {
		return RouteEphemeral, "session-local rollout/transcript decision"
	}
	return RouteEphemeral, "no standing-law, system, or product signal"
}

func productKey(e LedgerEntry) string {
	body := strings.ToLower(e.Body)
	for _, name := range productNameMarkers {
		if name != "" && strings.Contains(body, strings.ToLower(name)) {
			return "product:" + portfolioSlug(name)
		}
	}
	dom := strings.ToLower(e.Domain)
	for _, m := range productDomainMarkers {
		if strings.Contains(dom, m) {
			return portfolioKey(m)
		}
	}
	for _, m := range productBodyMarkers {
		if strings.Contains(body, m) {
			return portfolioKey(m)
		}
	}
	return ""
}

// portfolioKey canonicalizes a commercial-concern marker into one stable product
// key ("app store" and "app-store" collapse to portfolio:app-store).
func portfolioKey(marker string) string {
	k := strings.TrimSpace(strings.ToLower(marker))
	k = strings.ReplaceAll(k, " ", "-")
	k = strings.Trim(k, "-")
	return "portfolio:" + k
}

func portfolioSlug(name string) string {
	s := strings.TrimSpace(strings.ToLower(name))
	s = strings.ReplaceAll(s, " ", "-")
	return strings.Trim(s, "-")
}

func firstSystemMarker(text string) string {
	padded := " " + text + " "
	for _, m := range systemBehaviorMarkers {
		if strings.Contains(padded, m) {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

var trivialApprovalRe = regexp.MustCompile(`(?i)^\s*(okay|ok|yes|yep|sure|go|go ahead|go for it|do it|approved?|approve|proceed|continue|please|thanks?|thank you|lgtm|ship it|👍|👌)[\s.!,]*(\d+)?\s*$`)

func isTrivialApproval(quote string) bool {
	q := strings.TrimSpace(quote)
	if q == "" {
		return false
	}
	return trivialApprovalRe.MatchString(q)
}

// ReconcileBackfill maps a historical entry to one of the three doors against the
// accepted records. It runs the UNCHANGED Reconcile first (object-token coverage);
// only when that yields NOVEL does it try the additive slot-identity pass, which
// exists because the input shape differs: a short distilled decision body cannot
// cover a 600-char accepted-record object under token coverage, but it DOES name
// the record's slot (entity slug) — the meaning/slot identity the packet keys
// clusters on. A slot match requires the utterance to cover the record's
// meaningful slot tokens (>= slotCoverage, >= matchMinAnchors anchors) so it stays
// precision-first; opaque slugs (DEC-0006, note-<hash>) carry too few meaningful
// tokens to ever slot-match and fall back to object coverage.
func ReconcileBackfill(text string, kind Kind, active []mem.Claim) (Outcome, Match, string) {
	outcome, match := Reconcile(text, kind, active)
	if outcome != OutcomeNovel {
		return outcome, match, "object"
	}
	if slot, ok := slotMatch(text, active); ok {
		if hasReversal(text, slot) {
			return OutcomeConflict, slot, "slot"
		}
		return OutcomeCorroboration, slot, "slot"
	}
	return OutcomeNovel, Match{}, ""
}

// slotCoverage is the fraction of an accepted record's MEANINGFUL slot tokens an
// utterance must name to count as restating that slot. Precision-first: 0.6 keeps
// a 2-token slug (autonomy-by-envelope) an all-or-nothing match while letting a
// longer slug tolerate one absent token.
const slotCoverage = 0.6

// slotMinTokens is the minimum meaningful slot-token count a record needs before
// slot matching applies. Below this the slug is opaque/structural (an id or a
// bare "note"/"brain" prefix) and would over-match, so it is never slot-matched.
const slotMinTokens = 2

func slotMatch(text string, active []mem.Claim) (Match, bool) {
	uttTokens := significantTokens(text)
	if len(uttTokens) == 0 {
		return Match{}, false
	}
	var best Match
	for _, claim := range active {
		if claim.ClaimType != "decision" {
			continue
		}
		slotToks := slotTokens(claim)
		if len(slotToks) < slotMinTokens {
			continue
		}
		hit := intersectionCount(mapOf(slotToks), uttTokens)
		if hit < matchMinAnchors {
			continue
		}
		cov := float64(hit) / float64(len(slotToks))
		if cov < slotCoverage {
			continue
		}
		if cov > best.Score {
			lc := decisions.LifecycleFromClaim(claim)
			best = Match{
				ClaimID:    claim.ID,
				DecisionID: lc.ID,
				Object:     claim.Object,
				Kind:       lc.Kind,
				Score:      cov,
				Anchors:    hit,
			}
		}
	}
	return best, best.ClaimID != ""
}

// slotStopTokens are structural id/prefix fragments that carry no slot meaning.
var slotStopTokens = map[string]bool{
	"note": true, "brain": true, "decision": true, "claim": true,
	"rollout": true, "transcript": true, "session": true,
}

var allDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// isHexIDFragment reports whether a slug token is an id fragment rather than a
// word: all hex chars, length >= 4, AND containing at least one digit. Pure-letter
// hex words ("cafe", "beef", "decade") are kept — the digit is what marks an id
// (2d1c, 4f57, bb96, a6268f728d2a).
func isHexIDFragment(tok string) bool {
	if len(tok) < 4 {
		return false
	}
	hasDigit := false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return hasDigit
}

// slotTokens are the record's meaningful identity tokens, drawn from the entity
// slug (or decision_id). Date fragments, long hex hashes, pure numbers, and
// structural prefixes are dropped so the slot key is the decision's MEANING, not
// its filing id.
func slotTokens(claim mem.Claim) []string {
	// The slot key is the DECISION slug, by contract carried as a "decision:<slug>"
	// entity (or a decision_id qualifier). A generic namespace entity (e.g. a legacy
	// project/domain name used as the entity) is NOT a slot key — using it would
	// over-match every entry that names the namespace — so such records fall back to
	// object coverage only.
	slug := ""
	if strings.HasPrefix(claim.Entity, "decision:") {
		slug = strings.TrimPrefix(claim.Entity, "decision:")
	} else if claim.Qualifiers != nil {
		slug = strings.TrimSpace(claim.Qualifiers["decision_id"])
	}
	if strings.TrimSpace(slug) == "" {
		return nil
	}
	norm := mem.NormalizeClaimValue(slug)
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.FieldsFunc(norm, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(tok) < 4 && !tokenKeep[tok] {
			continue
		}
		if allDigitsRe.MatchString(tok) || isHexIDFragment(tok) {
			continue
		}
		if slotStopTokens[tok] || tokenStopwords[tok] {
			continue
		}
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

func mapOf(tokens []string) map[string]bool {
	m := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		m[t] = true
	}
	return m
}

// Backfill runs one deterministic backfill pass. A nil store is valid only with
// DryRun (no accepted records to reconcile against, nothing written). On a
// non-dry run it appends corroboration evidence to matched records, writes parked
// candidates for conflicts and novel clusters, and writes per-product report
// files — never auto-accepting anything.
func Backfill(store *mem.Store, homePath string, opts BackfillOptions) (BackfillReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	novelCap := opts.NovelCap
	if novelCap <= 0 {
		novelCap = DefaultNovelCap
	}
	report := BackfillReport{
		SchemaVersion: BackfillReportSchemaVersion,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		LedgerDir:     opts.LedgerDir,
		DryRun:        opts.DryRun,
		NovelCap:      novelCap,
		ProofRequired: []string{
			"every parsed entry lands in exactly one route (system|product|ephemeral); no silent drops",
			"a restatement of an accepted record appends corroboration evidence, never a duplicate record",
			"novel rulings emit ONE parked candidate per cluster (cluster-granularity), capped and logged",
			"product decisions go to a per-product report, never a compound record or brain write",
			"no grant/freeze/ruling is ever auto-accepted; dry-run writes nothing",
		},
	}
	if strings.TrimSpace(opts.LedgerDir) == "" {
		return report, fmt.Errorf("backfill: ledger dir required")
	}

	entries, issues, files, err := ParseLedgerDir(opts.LedgerDir)
	if err != nil {
		return report, fmt.Errorf("backfill: parse ledgers: %w", err)
	}
	report.FilesParsed = files
	report.EntriesTotal = len(entries)
	report.ParseIssues = clipIssues(issues, 10)
	report.ParseIssueCount = len(issues)

	var active []mem.Claim
	if store != nil {
		claims, err := store.AllCurrent()
		if err != nil {
			return report, fmt.Errorf("backfill: load current claims: %w", err)
		}
		active = decisions.AllActiveDecisions(claims)
	}
	report.AcceptedRecords = countDecisions(active)

	// Route every entry; collect the system-bound ones for reconciliation.
	var systemEntries []LedgerEntry
	products := map[string]*ProductReport{}
	ephemeral := map[string]*EphemeralReason{}
	for _, e := range entries {
		route, reason := RouteEntry(e)
		switch route {
		case RouteSystem:
			report.RouteCounts.System++
			systemEntries = append(systemEntries, e)
		case RouteProduct:
			report.RouteCounts.Product++
			key := reason
			if strings.HasPrefix(reason, "product marker: ") {
				key = strings.TrimPrefix(reason, "product marker: ")
			} else {
				key = "portfolio-unattributed"
			}
			pr := products[key]
			if pr == nil {
				pr = &ProductReport{Product: key}
				products[key] = pr
			}
			pr.Count++
			pr.Entries = append(pr.Entries, ProductEntry{
				Body: e.Body, Domain: e.Domain, Confidence: e.Confidence,
				Instance: e.EvidenceRef(), UpdatedAt: e.UpdatedAt,
			})
		default:
			report.RouteCounts.Ephemeral++
			er := ephemeral[reason]
			if er == nil {
				er = &EphemeralReason{Reason: reason}
				ephemeral[reason] = er
			}
			er.Count++
			if len(er.Samples) < 3 {
				er.Samples = append(er.Samples, clip(e.Body, 100))
			}
		}
	}

	// Order backwards from now: newest updated_at first, so a cluster's head is
	// the newest statement and older instances attach as corroboration.
	sort.SliceStable(systemEntries, func(i, j int) bool {
		if systemEntries[i].UpdatedAt != systemEntries[j].UpdatedAt {
			return systemEntries[i].UpdatedAt > systemEntries[j].UpdatedAt
		}
		if systemEntries[i].LedgerPath != systemEntries[j].LedgerPath {
			return systemEntries[i].LedgerPath < systemEntries[j].LedgerPath
		}
		return systemEntries[i].LineNo < systemEntries[j].LineNo
	})

	// Reconcile-vs-accepted FIRST; novel entries cluster among themselves.
	corrByClaim := map[string]*CorroborationRecord{}
	var conflicts []ConflictRecord
	var clusters []*NovelCluster
	yieldNext := yieldBucket
	processed := 0
	for _, e := range systemEntries {
		processed++
		outcome, match, via := ReconcileBackfill(e.Body, KindRuling, active)
		switch outcome {
		case OutcomeCorroboration:
			cr := corrByClaim[match.ClaimID]
			if cr == nil {
				cr = &CorroborationRecord{ClaimID: match.ClaimID, DecisionID: match.DecisionID, Object: clip(match.Object, 160), Via: via}
				corrByClaim[match.ClaimID] = cr
			}
			cr.Count++
			cr.Instances = append(cr.Instances, e.EvidenceRef())
		case OutcomeConflict:
			conflicts = append(conflicts, ConflictRecord{
				ClaimID: match.ClaimID, DecisionID: match.DecisionID,
				Body: clip(e.Body, 200), Instance: e.EvidenceRef(),
			})
		default: // novel — cluster among the run's own heads
			attachToCluster(&clusters, e)
		}
		if processed >= yieldNext {
			report.YieldCurve = append(report.YieldCurve, YieldPoint{EntriesProcessed: processed, NovelClusters: len(clusters)})
			yieldNext += yieldBucket
		}
	}
	if processed > 0 && (len(report.YieldCurve) == 0 || report.YieldCurve[len(report.YieldCurve)-1].EntriesProcessed != processed) {
		report.YieldCurve = append(report.YieldCurve, YieldPoint{EntriesProcessed: processed, NovelClusters: len(clusters)})
	}

	// Sort clusters by corroboration count desc (most-restated novel law first).
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].CorroborationCount != clusters[j].CorroborationCount {
			return clusters[i].CorroborationCount > clusters[j].CorroborationCount
		}
		return clusters[i].Head < clusters[j].Head
	})
	report.NovelClustersFound = len(clusters)

	// Apply side effects (corroboration, conflict/novel candidates, product files)
	// only on a non-dry run against a real store.
	if !opts.DryRun && store != nil {
		for _, cr := range corrByClaim {
			evID, err := backfillCorroborate(store, cr, now)
			if err != nil {
				return report, err
			}
			cr.EvidenceID = evID
		}
		for i := range conflicts {
			candID, err := backfillConflictCandidate(store, homePath, conflicts[i], now)
			if err != nil {
				return report, err
			}
			conflicts[i].CandidateID = candID
		}
	}

	// Emit ONE parked candidate per novel cluster, up to the cap.
	for i, c := range clusters {
		if i >= novelCap {
			c.Emitted = false
			continue
		}
		c.Emitted = true
		if !opts.DryRun && store != nil {
			candID, err := backfillNovelCandidate(store, homePath, c, now)
			if err != nil {
				return report, err
			}
			c.CandidateID = candID
		}
	}
	report.NovelEmitted = min(len(clusters), novelCap)
	report.NovelSkippedByCap = max(0, len(clusters)-novelCap)

	// Product report files (non-dry run only).
	productList := make([]*ProductReport, 0, len(products))
	for _, pr := range products {
		productList = append(productList, pr)
	}
	sort.SliceStable(productList, func(i, j int) bool {
		if productList[i].Count != productList[j].Count {
			return productList[i].Count > productList[j].Count
		}
		return productList[i].Product < productList[j].Product
	})
	if !opts.DryRun && homePath != "" {
		for _, pr := range productList {
			p, err := writeProductReport(homePath, pr, now)
			if err != nil {
				return report, err
			}
			pr.WrittenTo = p
		}
	}

	// Assemble report projections.
	report.Corroborations = collectCorroborations(corrByClaim)
	report.Conflicts = conflicts
	report.NovelSamples = clusterSamples(clusters, 5)
	report.ProductReports = clipProductReports(productList, 25)
	report.Ephemeral = collectEphemeral(ephemeral)
	return report, nil
}

// attachToCluster attaches e to an existing novel cluster whose head restates the
// same meaning (reusing Reconcile's matching against the formed heads as synthetic
// claims), or starts a new cluster with e as head. Because systemEntries are
// processed updated_at desc, the first entry of any cluster is the NEWEST -> head.
func attachToCluster(clusters *[]*NovelCluster, e LedgerEntry) {
	heads := make([]mem.Claim, 0, len(*clusters))
	for i, c := range *clusters {
		heads = append(heads, mem.Claim{
			ID:        fmt.Sprintf("cluster-%d", i),
			Object:    c.Head,
			ClaimType: "decision",
		})
	}
	outcome, match := Reconcile(e.Body, KindRuling, heads)
	if outcome == OutcomeCorroboration && strings.HasPrefix(match.ClaimID, "cluster-") {
		var idx int
		fmt.Sscanf(match.ClaimID, "cluster-%d", &idx)
		if idx >= 0 && idx < len(*clusters) {
			c := (*clusters)[idx]
			c.CorroborationCount++
			c.Instances = append(c.Instances, e.EvidenceRef())
			return
		}
	}
	*clusters = append(*clusters, &NovelCluster{
		Head:      e.Body,
		Kind:      string(KindRuling),
		Domain:    e.Domain,
		Instances: []string{e.EvidenceRef()},
	})
}

func backfillCorroborate(store *mem.Store, cr *CorroborationRecord, now time.Time) (string, error) {
	// Attribute to the newest instance (first, since sorted desc).
	inst := ""
	if len(cr.Instances) > 0 {
		inst = cr.Instances[0]
	}
	ev := mem.EvidenceItem{
		SourceKind: "decision-ledger",
		SourceID:   inst,
		QuoteText:  cr.Object,
		Actor:      "user",
		Visibility: "operational",
	}
	recorded, _, err := store.CorroborateClaim(cr.ClaimID, ev, 1.0, now)
	if err != nil {
		return "", fmt.Errorf("backfill: corroborate %s: %w", cr.ClaimID, err)
	}
	return recorded.ID, nil
}

func backfillConflictCandidate(store *mem.Store, homePath string, cf ConflictRecord, now time.Time) (string, error) {
	return writeBackfillCandidate(store, homePath, backfillCandidateSpec{
		kind:              KindRuling,
		outcome:           OutcomeConflict,
		body:              cf.Body,
		instances:         []string{cf.Instance},
		conflictsID:       cf.ClaimID,
		conflictsDecision: cf.DecisionID,
	}, now)
}

func backfillNovelCandidate(store *mem.Store, homePath string, c *NovelCluster, now time.Time) (string, error) {
	return writeBackfillCandidate(store, homePath, backfillCandidateSpec{
		kind:               KindRuling,
		outcome:            OutcomeNovel,
		body:               c.Head,
		instances:          c.Instances,
		corroborationCount: c.CorroborationCount,
	}, now)
}

type backfillCandidateSpec struct {
	kind               Kind
	outcome            Outcome
	body               string
	instances          []string
	corroborationCount int
	conflictsID        string
	conflictsDecision  string
}

func writeBackfillCandidate(store *mem.Store, homePath string, spec backfillCandidateSpec, now time.Time) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("backfill: COMPOUND_HOME required to file a decision candidate")
	}
	provenance := strings.Join(spec.instances, "\n")
	ev := mem.EvidenceItem{
		SourceKind: "decision-ledger",
		SourceID:   firstOr(spec.instances, "backfill:"+reviewartifact.Digest12(spec.body)),
		QuoteText:  spec.body,
		Actor:      "user",
		Visibility: "operational",
	}
	recorded, err := store.RecordEvidence(ev, now)
	if err != nil {
		return "", fmt.Errorf("backfill: record evidence: %w", err)
	}
	id := "backfill-" + string(spec.kind) + "-" + reviewartifact.Digest12(spec.body)
	decisionID := "session-backfill-" + string(spec.kind) + "-" + reviewartifact.Digest12(spec.body)
	q := map[string]string{
		"kind":                string(spec.kind),
		"source":              "sessions_backfill",
		"lifecycle_status":    "proposed",
		"verbatim":            clip(spec.body, 400),
		"decision_id":         decisionID,
		"harvest_outcome":     string(spec.outcome),
		"corroboration_count": fmt.Sprintf("%d", spec.corroborationCount),
		"instances":           clip(provenance, 1200),
		"instance_count":      fmt.Sprintf("%d", len(spec.instances)),
	}
	if spec.outcome == OutcomeConflict && spec.conflictsID != "" {
		q["conflicts_with_claim"] = spec.conflictsID
		q["conflicts_with_decision"] = spec.conflictsDecision
		q["needs_operator"] = "true"
	}
	rec := decisionCandidate{
		ID:           id,
		Kind:         string(candidates.Decision),
		Entity:       "decision:" + decisionID,
		Predicate:    "decided",
		Object:       clip(rulingTitle(spec.body), 200),
		Subject:      restate(spec.kind, spec.body),
		Statement:    restate(spec.kind, spec.body),
		Detail:       spec.body,
		Confidence:   0.6,
		Qualifiers:   q,
		EvidenceRefs: append([]string{recorded.ID}, spec.instances...),
		Verdict:      candidates.Park,
	}
	if err := candidates.WriteCandidate(homePath, candidates.Decision, id, &rec); err != nil {
		return "", fmt.Errorf("backfill: write decision candidate: %w", err)
	}
	return id, nil
}

func writeProductReport(homePath string, pr *ProductReport, now time.Time) (string, error) {
	dir := filepath.Join(homePath, "reports", "sessions-backfill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "product-" + reviewartifact.Slugify(pr.Product, 48) + ".json"
	path := filepath.Join(dir, name)
	payload := struct {
		SchemaVersion string        `json:"schema_version"`
		GeneratedAt   string        `json:"generated_at"`
		ProductReport ProductReport `json:"product_report"`
	}{
		SchemaVersion: "compound.backfill_product_report.v0.1",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		ProductReport: *pr,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func collectCorroborations(m map[string]*CorroborationRecord) []CorroborationRecord {
	out := make([]CorroborationRecord, 0, len(m))
	for _, cr := range m {
		c := *cr
		c.Instances = clipStrings(c.Instances, 20)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ClaimID < out[j].ClaimID
	})
	return out
}

func clusterSamples(clusters []*NovelCluster, n int) []NovelCluster {
	out := make([]NovelCluster, 0, n)
	for i, c := range clusters {
		if i >= n {
			break
		}
		cc := *c
		cc.Head = clip(cc.Head, 200)
		cc.Instances = clipStrings(cc.Instances, 12)
		out = append(out, cc)
	}
	return out
}

func collectEphemeral(m map[string]*EphemeralReason) []EphemeralReason {
	out := make([]EphemeralReason, 0, len(m))
	for _, er := range m {
		out = append(out, *er)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

func clipProductReports(list []*ProductReport, n int) []ProductReport {
	out := make([]ProductReport, 0, len(list))
	for _, pr := range list {
		p := *pr
		if len(p.Entries) > n {
			p.Entries = p.Entries[:n]
		}
		out = append(out, p)
	}
	return out
}

func clipIssues(issues []ParseIssue, n int) []ParseIssue {
	if len(issues) <= n {
		return issues
	}
	return issues[:n]
}

func clipStrings(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func countDecisions(claims []mem.Claim) int {
	n := 0
	for _, c := range claims {
		if c.ClaimType == "decision" {
			n++
		}
	}
	return n
}

func firstOr(s []string, fallback string) string {
	if len(s) > 0 && strings.TrimSpace(s[0]) != "" {
		return s[0]
	}
	return fallback
}
