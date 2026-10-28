package mem

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	BakeoffQuerySchemaVersion  = "compound.memory_bakeoff_queries.v0.1"
	BakeoffReportSchemaVersion = "compound.memory_bakeoff_report.v0.1"
	DefaultBakeoffQueriesPath  = "fixtures/bakeoff/queries.yaml"
)

type BakeoffQuerySet struct {
	SchemaVersion string         `json:"schema_version" yaml:"schema_version"`
	Queries       []BakeoffQuery `json:"queries" yaml:"queries"`
}

type BakeoffQuery struct {
	ID               string   `json:"id" yaml:"id"`
	Lane             string   `json:"lane" yaml:"lane"`
	Query            string   `json:"query" yaml:"query"`
	ExpectedClaimIDs []string `json:"expected_claim_ids" yaml:"expected_claim_ids"`
	MustHitClaimIDs  []string `json:"-" yaml:"must_hit_claim_ids"`
}

type BakeoffInitReport struct {
	Path       string   `json:"path"`
	Created    bool     `json:"created"`
	QueryCount int      `json:"query_count"`
	Lanes      []string `json:"lanes"`
}

// BakeoffSeedReport summarises one SeedBakeoffClaims pass: each seed action
// is either "created" (a new claim + evidence landed) or "already_seeded" (the
// literal claim id was already on disk, so the verb is idempotent). The
// evidence ids are the content-addressed ev_ records each promoted claim
// references, recorded through the canonical RecordEvidence door so the
// evidence refs are durable and content-validatable.
type BakeoffSeedReport struct {
	SchemaVersion string             `json:"schema_version"`
	Seeded        []BakeoffSeedClaim `json:"seeded"`
	AlreadySeeded []string           `json:"already_seeded"`
	Errors        []string           `json:"errors,omitempty"`
	ClinicCause   string             `json:"clinic_cause,omitempty"`
}

// BakeoffSeedClaim is the per-claim row of a seed report. IDs are LITERAL
// ("claim_memory_spine" etc.) because the bakeoff fixture queries those
// exact strings — derived IDs would never resolve the fixture.
type BakeoffSeedClaim struct {
	ClaimID    string `json:"claim_id"`
	EvidenceID string `json:"evidence_id"`
	Created    bool   `json:"created"`
}

const BakeoffSeedReportSchemaVersion = "compound.memory_bakeoff_seed.v0.1"

type BakeoffRunOptions struct {
	QueriesPath    string
	K              int
	Candidate      Engine
	AllowExpansion bool
	Now            time.Time
}

type BakeoffReport struct {
	Status string `json:"status,omitempty"`
	reviewartifact.OperatorCard
	SchemaVersion         string               `json:"schema_version"`
	GeneratedAt           string               `json:"generated_at"`
	QueriesPath           string               `json:"queries_path"`
	QueryCount            int                  `json:"query_count"`
	K                     int                  `json:"k"`
	CompareMode           string               `json:"compare_mode"`
	CandidateEngine       string               `json:"candidate_engine"`
	LedgerDigestBefore    string               `json:"ledger_digest_before"`
	LedgerDigestAfter     string               `json:"ledger_digest_after"`
	LedgerDigestUnchanged bool                 `json:"ledger_digest_unchanged"`
	Determinism           BakeoffDeterminism   `json:"determinism"`
	Metrics               BakeoffMetrics       `json:"metrics"`
	Queries               []BakeoffQueryReport `json:"queries"`
	Clinic                BakeoffClinicReport  `json:"clinic"`
	ReportPath            string               `json:"report_path,omitempty"`
}

type BakeoffClinicReport struct {
	Misses           []BakeoffClinicMiss  `json:"misses,omitempty"`
	TopCauses        []BakeoffClinicCause `json:"top_causes,omitempty"`
	RecommendedFixes []string             `json:"recommended_fixes,omitempty"`
}

type BakeoffClinicMiss struct {
	QueryID  string `json:"query_id"`
	ClaimID  string `json:"claim_id"`
	Cause    string `json:"cause"`
	Evidence string `json:"evidence"`
}

type BakeoffClinicCause struct {
	Cause        string   `json:"cause"`
	Count        int      `json:"count"`
	EvidenceRefs []string `json:"evidence_refs"`
}

type BakeoffDeterminism struct {
	Passed     bool     `json:"passed"`
	Mismatches []string `json:"mismatches,omitempty"`
}

type BakeoffMetrics struct {
	RecallAt5        float64 `json:"recall_at_5"`
	RecallAt10       float64 `json:"recall_at_10"`
	MRR              float64 `json:"mrr"`
	ReferenceCovered float64 `json:"reference_covered"`
	LatencyP50MS     float64 `json:"latency_p50_ms"`
	LatencyP95MS     float64 `json:"latency_p95_ms"`
}

type BakeoffQueryReport struct {
	ID                       string              `json:"id"`
	Lane                     string              `json:"lane"`
	Query                    string              `json:"query"`
	ExpectedClaimIDs         []string            `json:"expected_claim_ids"`
	ReferenceClaimIDs        []string            `json:"reference_claim_ids"`
	CandidateClaimIDs        []string            `json:"candidate_claim_ids"`
	CandidateRankedClaimIDs  []string            `json:"candidate_ranked_claim_ids"`
	MissingExpectedClaimIDs  []string            `json:"missing_expected_claim_ids,omitempty"`
	RecallAt5                float64             `json:"recall_at_5"`
	RecallAt10               float64             `json:"recall_at_10"`
	MRR                      float64             `json:"mrr"`
	ReferenceCovered         bool                `json:"reference_covered"`
	ReferenceCoveredFraction float64             `json:"reference_covered_fraction"`
	LatencyMS                float64             `json:"latency_ms"`
	Compare                  EngineCompareReport `json:"compare"`
}

const bakeoffClinicRankDepth = 50

func WriteDefaultBakeoffQueries(path string, force bool) (BakeoffInitReport, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultBakeoffQueriesPath
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			set, loadErr := LoadBakeoffQueries(path)
			if loadErr != nil {
				return BakeoffInitReport{}, loadErr
			}
			return BakeoffInitReport{Path: path, Created: false, QueryCount: len(set.Queries), Lanes: bakeoffLanes(set.Queries)}, nil
		} else if !os.IsNotExist(err) {
			return BakeoffInitReport{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return BakeoffInitReport{}, err
	}
	if err := os.WriteFile(path, []byte(defaultBakeoffQueriesYAML), 0o644); err != nil {
		return BakeoffInitReport{}, err
	}
	set, err := LoadBakeoffQueries(path)
	if err != nil {
		return BakeoffInitReport{}, err
	}
	return BakeoffInitReport{Path: path, Created: true, QueryCount: len(set.Queries), Lanes: bakeoffLanes(set.Queries)}, nil
}

// SeedBakeoffClaims writes the three fixture-shaped claims the bakeoff
// queries.yaml references (claim_memory_spine / claim_reference_engine /
// claim_no_mutation) so a freshly-initialized home can run `memory bakeoff
// run` end-to-end. Idempotent: re-running on an already-seeded home reports
// AlreadySeeded for each claim instead of minting duplicates.
//
// Path: this is a trusted-deterministic adapter write (the same door the
// knowledge-page importer uses), not raw intake. The packet's promotion-
// basis strip lives ONLY on the Capture() intake door — adapter writes via
// RecordEvidence + the canonical appendJSONL ledger door are unaffected.
// Each seeded claim carries:
//   - a real, content-addressed EvidenceItem ("tool" actor, source_kind
//     "note"), so the claim's EvidenceRefs are durable and revalidate;
//   - PromotionBasis = trusted_deterministic_import — the canonical basis
//     for adapter-side writes;
//   - Qualifiers["bakeoff_seed"] = "true" so an operator who seeds a scratch
//     home can grep/filter the fixture content away from their real ambient
//     recall lanes (the claims are operational visibility, but the qualifier
//     marks them as synthetic fixture material);
//   - literal Claim IDs (claim_memory_spine etc.), because the bakeoff
//     fixture's expected_claim_ids are those exact strings — derived IDs
//     from the Capture/Adjudicate path would never resolve the fixture.
//
// The Claims are appended through appendJSONL(s.claimsLog(), ...) — the
// same canonical append door Adjudicate uses — so they land in the same
// byte stream the ledger reader folds. Re-running SeedBakeoffClaims does
// NOT re-append: each claim is checked against the existing ledger first.
func (s *Store) SeedBakeoffClaims(now time.Time) (BakeoffSeedReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report := BakeoffSeedReport{SchemaVersion: BakeoffSeedReportSchemaVersion}
	for _, def := range defaultBakeoffSeedClaims {
		existing, err := s.findBakeoffSeedClaim(def.ID)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("lookup %s: %v", def.ID, err))
			continue
		}
		if existing != nil {
			report.AlreadySeeded = append(report.AlreadySeeded, def.ID)
			// Surface the existing evidence id so callers can audit which
			// record the already-seeded claim points at.
			evidenceID := ""
			if len(existing.EvidenceRefs) > 0 {
				evidenceID = existing.EvidenceRefs[0]
			}
			report.Seeded = append(report.Seeded, BakeoffSeedClaim{ClaimID: def.ID, EvidenceID: evidenceID, Created: false})
			continue
		}
		evidence, err := s.RecordEvidence(EvidenceItem{
			SourceKind: "note",
			SourceID:   "bakeoff_seed:" + def.ID,
			QuoteText:  def.EvidenceQuote,
			Actor:      "tool",
			Visibility: "operational",
		}, now)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("record evidence %s: %v", def.ID, err))
			continue
		}
		claim := Claim{
			ID:             def.ID,
			Seq:            def.Seq,
			Entity:         def.Entity,
			Predicate:      def.Predicate,
			Object:         def.Object,
			ValidFrom:      rfc3339(now),
			Confidence:     0.95,
			Derivation:     "deterministic",
			ProvenanceLane: def.Lane,
			PromotionBasis: candidates.BasisTrustedDeterministic,
			EvidenceRefs:   []string{evidence.ID},
			Qualifiers: map[string]string{
				"bakeoff_seed": "true",
				"source":       "bakeoff_seed",
				"seed_kind":    def.Kind,
			},
			Visibility: "operational",
			CreatedAt:  rfc3339(now),
		}
		if err := appendJSONL(s.claimsLog(), &claim); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("append claim %s: %v", def.ID, err))
			continue
		}
		report.Seeded = append(report.Seeded, BakeoffSeedClaim{ClaimID: def.ID, EvidenceID: evidence.ID, Created: true})
	}
	if len(report.Seeded) == 0 && len(report.AlreadySeeded) == 0 && len(report.Errors) > 0 {
		return report, fmt.Errorf("bakeoff seed: no claims seeded (%s)", strings.Join(report.Errors, "; "))
	}
	return report, nil
}

// bakeoffSeedClaim is the static definition of one fixture-shaped claim. The
// Entity/Predicate/Object are deliberately truthful-shaped: each claim
// literally describes what its id says (the memory spine is owned by the
// compound ledger; the reference engine baseline-checks candidate answers;
// the bakeoff recall pass mutates no claim). Object content is short,
// neutral, and free of real product names per the privacy law.
type bakeoffSeedClaim struct {
	ID            string
	Seq           int64
	Entity        string
	Predicate     string
	Object        string
	Lane          string
	Kind          string // semantic category for the seed_kind qualifier
	EvidenceQuote string // the durable quote the content-addressed evidence item carries
}

var defaultBakeoffSeedClaims = []bakeoffSeedClaim{
	{
		ID:            "claim_memory_spine",
		Seq:           1,
		Entity:        "system-memory-spine",
		Predicate:     "authority_owner",
		Object:        "compound ledger owns real memory truth and is the single meaning owner for promoted memory facts",
		Lane:          "exact",
		Kind:          "memory_spine",
		EvidenceQuote: "the compound memory ledger is the single authority owner for promoted memory facts; recall is a projection, not an authority",
	},
	{
		ID:            "claim_reference_engine",
		Seq:           2,
		Entity:        "reference-engine",
		Predicate:     "baseline_check",
		Object:        "the reference engine baseline-checks candidate answers against the lexical reference set and reports covered fraction",
		Lane:          "paraphrase",
		Kind:          "reference_engine",
		EvidenceQuote: "reference engine answers are baseline-checked against a lexical reference claim set; coverage is reported as reference_covered",
	},
	{
		ID:            "claim_no_mutation",
		Seq:           3,
		Entity:        "recall-bakeoff",
		Predicate:     "mutation_policy",
		Object:        "the bakeoff recall pass never mutates the real memory ledger; ledger digest is byte-identical before and after a run",
		Lane:          "casual",
		Kind:          "no_mutation",
		EvidenceQuote: "bakeoff run reports ledger_digest_unchanged=true; a query that mutates the ledger fails the bakeoff with an explicit mutation error",
	},
}

// findBakeoffSeedClaim locates an existing claim by literal ID. Used to
// make SeedBakeoffClaims idempotent without a separate index: a single
// fold over the ledger is bounded and cheap for the bakeoff scale (a
// seeded home has <100 claims at the fixture surface). Returns nil, nil
// when the claim is absent.
func (s *Store) findBakeoffSeedClaim(id string) (*Claim, error) {
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return nil, err
	}
	for i := range claims {
		if claims[i].ID == id {
			return &claims[i], nil
		}
	}
	return nil, nil
}

func LoadBakeoffQueries(path string) (BakeoffQuerySet, error) {
	if strings.TrimSpace(path) == "" {
		return BakeoffQuerySet{}, fmt.Errorf("queries path required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return BakeoffQuerySet{}, err
	}
	var set BakeoffQuerySet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return BakeoffQuerySet{}, err
	}
	if set.SchemaVersion == "" {
		set.SchemaVersion = BakeoffQuerySchemaVersion
	}
	for i := range set.Queries {
		q := &set.Queries[i]
		if len(q.ExpectedClaimIDs) == 0 && len(q.MustHitClaimIDs) > 0 {
			q.ExpectedClaimIDs = append([]string(nil), q.MustHitClaimIDs...)
		}
		q.ExpectedClaimIDs = uniqueNonEmpty(q.ExpectedClaimIDs)
	}
	if err := validateBakeoffQueries(set); err != nil {
		return BakeoffQuerySet{}, err
	}
	return set, nil
}

func (s *Store) RunBakeoff(opts BakeoffRunOptions) (BakeoffReport, error) {
	if opts.Candidate == nil {
		return BakeoffReport{}, fmt.Errorf("candidate engine required")
	}
	if opts.K <= 0 {
		opts.K = 10
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	set, err := LoadBakeoffQueries(opts.QueriesPath)
	if err != nil {
		return BakeoffReport{}, err
	}
	before, err := s.LedgerDigest()
	if err != nil {
		return BakeoffReport{}, err
	}

	first, err := s.runBakeoffPass(set.Queries, opts)
	if err != nil {
		return BakeoffReport{}, err
	}
	second, err := s.runBakeoffPass(set.Queries, opts)
	if err != nil {
		return BakeoffReport{}, err
	}

	after, err := s.LedgerDigest()
	if err != nil {
		return BakeoffReport{}, err
	}
	report := BakeoffReport{
		SchemaVersion:         BakeoffReportSchemaVersion,
		GeneratedAt:           opts.Now.UTC().Format(time.RFC3339),
		QueriesPath:           opts.QueriesPath,
		QueryCount:            len(first.queries),
		K:                     opts.K,
		CompareMode:           "exact",
		LedgerDigestBefore:    before,
		LedgerDigestAfter:     after,
		LedgerDigestUnchanged: before == after,
		Queries:               first.queries,
	}
	if opts.AllowExpansion {
		report.CompareMode = "allow_expansion"
	}
	if len(first.queries) > 0 {
		report.CandidateEngine = first.queries[0].Compare.Candidate.Engine.Name
	}
	report.Determinism = compareBakeoffPasses(first.queries, second.queries)
	report.Metrics = bakeoffMetrics(first.queries)
	report.Clinic = s.buildBakeoffClinic(first.queries)
	report.Status = "recorded"
	if card, err := OperatorCardForBakeoffReport(report); err == nil {
		report.OperatorCard = card
	}

	writeErr := s.writeBakeoffReport(&report, opts.Now)
	var errs []string
	if writeErr != nil {
		errs = append(errs, writeErr.Error())
	}
	if !report.LedgerDigestUnchanged {
		errs = append(errs, "ledger digest changed during bakeoff")
	}
	if !report.Determinism.Passed {
		errs = append(errs, "bakeoff ranked order is nondeterministic")
	}
	if report.Metrics.ReferenceCovered < 1 {
		errs = append(errs, "candidate did not cover all reference claims")
	}
	for _, q := range report.Queries {
		if len(q.MissingExpectedClaimIDs) > 0 {
			errs = append(errs, fmt.Sprintf("%s missing expected claim(s): %s", q.ID, strings.Join(q.MissingExpectedClaimIDs, ",")))
		}
	}
	if len(errs) > 0 {
		return report, errors.New(strings.Join(errs, "; "))
	}
	return report, nil
}

var bakeoffClinicFix = map[string]string{
	"absent_claim": "ingest or restore the missing claim", "held_by_review_gate": "review the held claim; do not bypass the gate",
	"duplicate_twin": "retire or merge the duplicate claim", "wrong_entity": "repair canonical entity resolution",
	"synonym_alias_missing": "add the missing query alias", "semantic_lane_inactive": "activate or route through the configured semantic lane",
	"rank_miss": "improve ranking for the present claim", "privacy_filter": "keep private recall in an authorized scope",
	"stale_or_volatile_claim": "refresh or retire the expired claim", "expected_id_wrong": "repair the golden claim identifier",
	"noisy_claim_pollution": "remove noisy candidates or sharpen the query",
}

var legacyBakeoffClinicCause = map[string]string{
	"absent": "absent_claim", "gated": "held_by_review_gate", "twin": "duplicate_twin",
	"entity": "wrong_entity", "alias": "synonym_alias_missing", "lane": "semantic_lane_inactive",
	"rank": "rank_miss", "privacy": "privacy_filter", "volatile": "stale_or_volatile_claim",
	"id": "expected_id_wrong", "noise": "noisy_claim_pollution",
}

func (s *Store) buildBakeoffClinic(queries []BakeoffQueryReport) BakeoffClinicReport {
	claims, _ := s.loadClaimsRaw()
	byID := map[string]Claim{}
	for _, claim := range claims {
		byID[claim.ID] = claim
	}
	var report BakeoffClinicReport
	counts := map[string]int{}
	evidenceRefs := map[string][]string{}
	for _, query := range queries {
		for _, id := range query.MissingExpectedClaimIDs {
			classification := classifyBakeoffMiss(query, id, byID)
			report.Misses = append(report.Misses, BakeoffClinicMiss{QueryID: query.ID, ClaimID: id, Cause: classification.Cause, Evidence: classification.Evidence})
			counts[classification.Cause]++
			evidenceRefs[classification.Cause] = append(evidenceRefs[classification.Cause], query.ID+":"+id)
		}
	}
	for cause, count := range counts {
		sort.Strings(evidenceRefs[cause])
		report.TopCauses = append(report.TopCauses, BakeoffClinicCause{Cause: cause, Count: count, EvidenceRefs: evidenceRefs[cause]})
	}
	sort.Slice(report.TopCauses, func(i, j int) bool {
		if report.TopCauses[i].Count != report.TopCauses[j].Count {
			return report.TopCauses[i].Count > report.TopCauses[j].Count
		}
		return report.TopCauses[i].Cause < report.TopCauses[j].Cause
	})
	if len(report.TopCauses) > 3 {
		report.TopCauses = report.TopCauses[:3]
	}
	for _, cause := range report.TopCauses {
		report.RecommendedFixes = append(report.RecommendedFixes, bakeoffClinicFix[cause.Cause])
	}
	return report
}

type bakeoffMissClassification struct {
	Cause    string
	Evidence string
}

func classifyBakeoffMiss(query BakeoffQueryReport, id string, claims map[string]Claim) bakeoffMissClassification {
	claim, ok := claims[id]
	if !ok {
		if replacement := bakeoffExpectedIDReplacement(id, claims); replacement != "" {
			return bakeoffMissClassification{"expected_id_wrong", "expected id resolves through decision_id to current claim " + replacement}
		}
		if strings.Contains(strings.ToLower(id), "alias") {
			return bakeoffMissClassification{"synonym_alias_missing", "expected id is absent and names an alias expectation"}
		}
		return bakeoffMissClassification{"absent_claim", "expected claim id is absent from the owned ledger"}
	}
	if cause := normalizeBakeoffClinicCause(claim.Qualifiers["clinic_cause"]); cause != "" {
		return bakeoffMissClassification{cause, "claim qualifier clinic_cause=" + cause}
	}
	if claim.Visibility == "private" {
		return bakeoffMissClassification{"privacy_filter", "claim visibility is private"}
	}
	if claim.Visibility == "volatile" || claim.ExpiresAt != "" {
		return bakeoffMissClassification{"stale_or_volatile_claim", "claim is volatile or has an expiry"}
	}
	if !bakeoffContainsString(query.ReferenceClaimIDs, id) {
		return bakeoffMissClassification{"held_by_review_gate", "claim exists in the ledger but is absent from the visible reference set"}
	}
	if claim.Supersedes != "" || bakeoffClaimIsSuperseded(id, claims) {
		return bakeoffMissClassification{"duplicate_twin", "claim has a supersession twin in the owned ledger"}
	}
	if len(query.CandidateRankedClaimIDs) > 10 && bakeoffContainsString(query.CandidateRankedClaimIDs[10:], id) {
		return bakeoffMissClassification{"rank_miss", "claim is present below rank 10 in candidate results"}
	}
	if strings.TrimSpace(claim.Entity) == "" {
		return bakeoffMissClassification{"wrong_entity", "claim has no canonical entity"}
	}
	if claim.ProvenanceLane != "" && query.Lane != "" && claim.ProvenanceLane != query.Lane {
		return bakeoffMissClassification{"semantic_lane_inactive", fmt.Sprintf("claim lane %s differs from query lane %s", claim.ProvenanceLane, query.Lane)}
	}
	if strings.Contains(strings.ToLower(id), "stale") || strings.Contains(strings.ToLower(id), "missing") {
		return bakeoffMissClassification{"expected_id_wrong", "expected id is marked stale or missing"}
	}
	return bakeoffMissClassification{"noisy_claim_pollution", "claim is visible but absent from the candidate ranking without a narrower structural cause"}
}

func bakeoffExpectedIDReplacement(id string, claims map[string]Claim) string {
	var replacements []string
	for _, claim := range claims {
		if strings.EqualFold(strings.TrimSpace(claim.Qualifiers["decision_id"]), strings.TrimSpace(id)) {
			replacements = append(replacements, claim.ID)
		}
	}
	sort.Strings(replacements)
	if len(replacements) == 0 {
		return ""
	}
	return replacements[0]
}

func normalizeBakeoffClinicCause(raw string) string {
	cause := strings.ToLower(strings.TrimSpace(raw))
	if bakeoffClinicFix[cause] != "" {
		return cause
	}
	return legacyBakeoffClinicCause[cause]
}

func bakeoffClaimIsSuperseded(id string, claims map[string]Claim) bool {
	for _, claim := range claims {
		if claim.Supersedes == id {
			return true
		}
	}
	return false
}

func bakeoffContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type bakeoffPass struct {
	queries []BakeoffQueryReport
}

func (s *Store) runBakeoffPass(queries []BakeoffQuery, opts BakeoffRunOptions) (bakeoffPass, error) {
	out := make([]BakeoffQueryReport, 0, len(queries))
	for _, q := range queries {
		start := time.Now()
		compare, err := s.EngineCompareWithOptions(q.Query, opts.K, opts.Candidate, EngineCompareOptions{AllowExpansion: opts.AllowExpansion})
		if err != nil {
			return bakeoffPass{}, err
		}
		ranked := doctorRankedClaimIDs(compare.Candidate.Hits)
		if len(missingWithinK(q.ExpectedClaimIDs, ranked, 10)) > 0 && opts.K < bakeoffClinicRankDepth {
			clinicCandidate, err := s.EngineDoctor(q.Query, bakeoffClinicRankDepth, opts.Candidate)
			if err != nil {
				return bakeoffPass{}, err
			}
			ranked = doctorRankedClaimIDs(clinicCandidate.Hits)
		}
		latency := time.Since(start)
		qr := BakeoffQueryReport{
			ID:                       q.ID,
			Lane:                     q.Lane,
			Query:                    q.Query,
			ExpectedClaimIDs:         append([]string(nil), q.ExpectedClaimIDs...),
			ReferenceClaimIDs:        append([]string(nil), compare.ReferenceClaimIDs...),
			CandidateClaimIDs:        append([]string(nil), compare.CandidateClaimIDs...),
			CandidateRankedClaimIDs:  ranked,
			MissingExpectedClaimIDs:  missingWithinK(q.ExpectedClaimIDs, ranked, 10),
			RecallAt5:                recallAtK(q.ExpectedClaimIDs, ranked, 5),
			RecallAt10:               recallAtK(q.ExpectedClaimIDs, ranked, 10),
			MRR:                      reciprocalRank(q.ExpectedClaimIDs, ranked),
			ReferenceCovered:         compare.ReferenceCovered,
			ReferenceCoveredFraction: referenceCoveredFraction(compare.ReferenceClaimIDs, compare.CandidateClaimIDs),
			LatencyMS:                durationMS(latency),
			Compare:                  compare,
		}
		out = append(out, qr)
	}
	return bakeoffPass{queries: out}, nil
}

func (s *Store) writeBakeoffReport(report *BakeoffReport, now time.Time) error {
	dir := filepath.Join(s.home, "reviews", "memory-bakeoffs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("memory-bakeoff-%s-%s.json", stamp, shortHash(report.QueriesPath))
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	report.ReportPath = path
	data, err = json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func validateBakeoffQueries(set BakeoffQuerySet) error {
	if set.SchemaVersion != BakeoffQuerySchemaVersion {
		return fmt.Errorf("unsupported bakeoff query schema %q", set.SchemaVersion)
	}
	if len(set.Queries) == 0 {
		return fmt.Errorf("bakeoff query set is empty")
	}
	ids := map[string]bool{}
	for i, q := range set.Queries {
		if strings.TrimSpace(q.ID) == "" {
			return fmt.Errorf("query %d: id required", i+1)
		}
		if ids[q.ID] {
			return fmt.Errorf("query %q: duplicate id", q.ID)
		}
		ids[q.ID] = true
		if strings.TrimSpace(q.Lane) == "" {
			return fmt.Errorf("query %q: lane required", q.ID)
		}
		if strings.TrimSpace(q.Query) == "" {
			return fmt.Errorf("query %q: query required", q.ID)
		}
		if len(q.ExpectedClaimIDs) == 0 {
			return fmt.Errorf("query %q: expected_claim_ids required", q.ID)
		}
	}
	return nil
}

func bakeoffMetrics(queries []BakeoffQueryReport) BakeoffMetrics {
	if len(queries) == 0 {
		return BakeoffMetrics{}
	}
	var sum5, sum10, sumMRR, coveredRefs, totalRefs float64
	latencies := make([]time.Duration, 0, len(queries))
	for _, q := range queries {
		sum5 += q.RecallAt5
		sum10 += q.RecallAt10
		sumMRR += q.MRR
		coveredRefs += float64(intersectionCount(q.ReferenceClaimIDs, q.CandidateClaimIDs))
		totalRefs += float64(len(q.ReferenceClaimIDs))
		latencies = append(latencies, time.Duration(q.LatencyMS*float64(time.Millisecond)))
	}
	refCovered := 0.0
	if totalRefs > 0 {
		refCovered = coveredRefs / totalRefs
	}
	return BakeoffMetrics{
		RecallAt5:        sum5 / float64(len(queries)),
		RecallAt10:       sum10 / float64(len(queries)),
		MRR:              sumMRR / float64(len(queries)),
		ReferenceCovered: refCovered,
		LatencyP50MS:     durationMS(percentileDuration(latencies, 0.50)),
		LatencyP95MS:     durationMS(percentileDuration(latencies, 0.95)),
	}
}

func compareBakeoffPasses(first, second []BakeoffQueryReport) BakeoffDeterminism {
	d := BakeoffDeterminism{Passed: true}
	if len(first) != len(second) {
		return BakeoffDeterminism{Passed: false, Mismatches: []string{"query count differs"}}
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			d.Passed = false
			d.Mismatches = append(d.Mismatches, fmt.Sprintf("query %d id differs: %s != %s", i, first[i].ID, second[i].ID))
			continue
		}
		if !sameStrings(first[i].CandidateRankedClaimIDs, second[i].CandidateRankedClaimIDs) {
			d.Passed = false
			d.Mismatches = append(d.Mismatches, first[i].ID)
		}
	}
	return d
}

func doctorRankedClaimIDs(hits []EngineDoctorHit) []string {
	out := make([]string, 0, len(hits))
	seen := map[string]bool{}
	for _, hit := range hits {
		if hit.ClaimID == "" || seen[hit.ClaimID] {
			continue
		}
		seen[hit.ClaimID] = true
		out = append(out, hit.ClaimID)
	}
	return out
}

func missingWithinK(expected, ranked []string, k int) []string {
	if k > len(ranked) {
		k = len(ranked)
	}
	got := map[string]bool{}
	for _, id := range ranked[:k] {
		got[id] = true
	}
	var missing []string
	for _, id := range expected {
		if !got[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func recallAtK(expected, ranked []string, k int) float64 {
	if len(expected) == 0 {
		return 0
	}
	return float64(len(expected)-len(missingWithinK(expected, ranked, k))) / float64(len(expected))
}

func reciprocalRank(expected, ranked []string) float64 {
	want := map[string]bool{}
	for _, id := range expected {
		want[id] = true
	}
	for i, id := range ranked {
		if want[id] {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func referenceCoveredFraction(reference, candidate []string) float64 {
	if len(reference) == 0 {
		return 1
	}
	return float64(intersectionCount(reference, candidate)) / float64(len(reference))
}

func intersectionCount(a, b []string) int {
	set := map[string]bool{}
	for _, id := range b {
		set[id] = true
	}
	n := 0
	for _, id := range a {
		if set[id] {
			n++
		}
	}
	return n
}

func percentileDuration(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), values...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	if p <= 0 {
		return cp[0]
	}
	if p >= 1 {
		return cp[len(cp)-1]
	}
	idx := int(p*float64(len(cp)-1) + 0.5)
	return cp[idx]
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func uniqueNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func bakeoffLanes(queries []BakeoffQuery) []string {
	seen := map[string]bool{}
	var lanes []string
	for _, q := range queries {
		if q.Lane == "" || seen[q.Lane] {
			continue
		}
		seen[q.Lane] = true
		lanes = append(lanes, q.Lane)
	}
	sort.Strings(lanes)
	return lanes
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const defaultBakeoffQueriesYAML = `schema_version: compound.memory_bakeoff_queries.v0.1
queries:
  - id: exact-memory-spine
    lane: exact
    query: "memory spine compound ledger"
    expected_claim_ids:
      - claim_memory_spine
  - id: paraphrase-reference-engine
    lane: paraphrase
    query: "which baseline checks engine answers"
    expected_claim_ids:
      - claim_reference_engine
  - id: casual-no-mutation
    lane: casual
    query: "does recall mess with the real memory truth"
    expected_claim_ids:
      - claim_no_mutation
`
