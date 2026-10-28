package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/candidates"
	"gopkg.in/yaml.v3"
)

const KnowledgePageSchemaVersion = "compound.knowledge_page.v0.1"

// KnowledgePageImportReport is the public result of importing one curated
// markdown knowledge page. The import records evidence and candidate memory
// only; it never writes promoted claims.
type KnowledgePageImportReport struct {
	SchemaVersion string   `json:"schema_version"`
	PageID        string   `json:"page_id"`
	Title         string   `json:"title,omitempty"`
	SourcePath    string   `json:"source_path"`
	EvidenceID    string   `json:"evidence_id"`
	CandidateIDs  []string `json:"candidate_ids"`
	Reused        int      `json:"reused"`
	Created       int      `json:"created"`
}

type knowledgePageFrontmatter struct {
	SchemaVersion string               `yaml:"schema_version"`
	ID            string               `yaml:"id"`
	Title         string               `yaml:"title"`
	Entity        string               `yaml:"entity"`
	Owner         string               `yaml:"owner"`
	Visibility    string               `yaml:"visibility"`
	Claims        []knowledgePageClaim `yaml:"claims"`
}

type knowledgePageClaim struct {
	Entity          string            `yaml:"entity"`
	Predicate       string            `yaml:"predicate"`
	Object          string            `yaml:"object"`
	ObservationDate string            `yaml:"observation_date"`
	Confidence      float64           `yaml:"confidence"`
	Derivation      string            `yaml:"derivation"`
	ClaimType       string            `yaml:"claim_type"`
	Qualifiers      map[string]string `yaml:"qualifiers"`
	TopicRefs       []string          `yaml:"topic_refs"`
	PromotionBasis  string            `yaml:"promotion_basis"`
	Visibility      string            `yaml:"visibility"`
	EvidenceQuote   string            `yaml:"evidence_quote"`
}

// ImportKnowledgePage adapts a curated markdown page into evidence plus
// explicit memory candidates. Claims must be structured in frontmatter; the
// adapter deliberately does not infer facts from prose.
func (s *Store) ImportKnowledgePage(path string, now time.Time) (KnowledgePageImportReport, error) {
	var report KnowledgePageImportReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, fmt.Errorf("read knowledge page %s: %w", path, err)
	}
	fmRaw, body := splitMarkdownFrontmatter(string(data))
	var fm knowledgePageFrontmatter
	if strings.TrimSpace(fmRaw) != "" {
		if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
			return report, fmt.Errorf("parse knowledge page frontmatter %s: %w", path, err)
		}
	}
	if fm.SchemaVersion != "" && fm.SchemaVersion != KnowledgePageSchemaVersion {
		return report, fmt.Errorf("knowledge page %s schema_version=%q, want %s", path, fm.SchemaVersion, KnowledgePageSchemaVersion)
	}
	title := firstMarkdownHeading(body)
	if strings.TrimSpace(fm.Title) != "" {
		title = strings.TrimSpace(fm.Title)
	}
	pageID := strings.TrimSpace(fm.ID)
	if pageID == "" {
		pageID = "knowledge_" + digestForPage(title, body)
	}
	if len(fm.Claims) == 0 {
		return report, fmt.Errorf("knowledge page %s has no explicit claims[] frontmatter", path)
	}

	evidence, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "note",
		SourceID:   "knowledge_page:" + pageID,
		QuoteText:  evidenceText(title, body),
		Actor:      "tool",
		Visibility: firstNonEmpty(fm.Visibility, "contextual"),
	}, now)
	if err != nil {
		return report, err
	}

	report = KnowledgePageImportReport{
		SchemaVersion: KnowledgePageSchemaVersion,
		PageID:        pageID,
		Title:         title,
		SourcePath:    path,
		EvidenceID:    evidence.ID,
	}
	for i, claim := range fm.Claims {
		candidate, err := candidateFromKnowledgeClaim(pageID, evidence.ID, fm, claim, i, now)
		if err != nil {
			return report, err
		}
		created, err := s.writeStableCandidate(candidate)
		if err != nil {
			return report, err
		}
		report.CandidateIDs = append(report.CandidateIDs, candidate.ID)
		if created {
			report.Created++
		} else {
			report.Reused++
		}
	}
	return report, nil
}

func candidateFromKnowledgeClaim(pageID, evidenceID string, fm knowledgePageFrontmatter, claim knowledgePageClaim, index int, now time.Time) (Candidate, error) {
	entity := firstNonEmpty(claim.Entity, fm.Entity)
	predicate := strings.TrimSpace(claim.Predicate)
	object := strings.TrimSpace(claim.Object)
	if entity == "" || predicate == "" || object == "" {
		return Candidate{}, fmt.Errorf("knowledge claim %d needs entity, predicate, object", index)
	}
	confidence := claim.Confidence
	if confidence == 0 {
		confidence = 0.8
	}
	if confidence < 0 || confidence > 1 {
		return Candidate{}, fmt.Errorf("knowledge claim %d confidence %.2f outside [0,1]", index, confidence)
	}
	derivation := firstNonEmpty(claim.Derivation, "deterministic")
	if !oneOf(derivation, Derivations) {
		return Candidate{}, fmt.Errorf("knowledge claim %d bad derivation %q", index, derivation)
	}
	if claim.ClaimType != "" && !oneOf(claim.ClaimType, ClaimTypes) {
		return Candidate{}, fmt.Errorf("knowledge claim %d bad claim_type %q", index, claim.ClaimType)
	}
	promotionBasis := firstNonEmpty(claim.PromotionBasis, candidates.BasisCuratedPage)
	if promotionBasis != "" && !candidates.ValidBasis(promotionBasis) {
		return Candidate{}, fmt.Errorf("knowledge claim %d bad promotion_basis %q", index, promotionBasis)
	}
	visibility := firstNonEmpty(claim.Visibility, fm.Visibility, "contextual")
	if !oneOf(visibility, Visibilities) {
		return Candidate{}, fmt.Errorf("knowledge claim %d bad visibility %q", index, visibility)
	}
	// Knowledge-page frontmatter has no expires_at field, so a volatile page
	// claim always refuses here — fail-loud until the frontmatter grows a clock.
	if err := validateVolatileWrite(fmt.Sprintf("knowledge claim %d", index), visibility, "", claim.ClaimType, ""); err != nil {
		return Candidate{}, err
	}
	q := copyStringMap(claim.Qualifiers)
	if fm.Owner != "" {
		q["owner"] = strings.TrimSpace(fm.Owner)
	}
	q["source"] = "knowledge_page"
	q["page_id"] = pageID
	c := Candidate{
		ID:              idFor("cand_", "knowledge-page", pageID, evidenceID, entity, predicate, object),
		Entity:          entity,
		Predicate:       predicate,
		Object:          object,
		ObservationDate: claim.ObservationDate,
		Confidence:      confidence,
		Derivation:      derivation,
		ClaimType:       claim.ClaimType,
		Qualifiers:      q,
		TopicRefs:       normalizeStringRefs(claim.TopicRefs),
		PromotionBasis:  promotionBasis,
		SourceTraceRef:  evidenceID,
		SnippetHash:     digestForPage(firstNonEmpty(claim.EvidenceQuote, object), evidenceID),
		Visibility:      visibility,
		State:           "proposed",
		CreatedAt:       rfc3339(now),
	}
	return c, nil
}

func (s *Store) writeStableCandidate(c Candidate) (bool, error) {
	return candidates.CreateCandidateIfAbsent(s.home, candidates.Memory, c.ID, &c)
}

func splitMarkdownFrontmatter(s string) (fm, body string) {
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

func firstMarkdownHeading(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return ""
}

func evidenceText(title, body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		text = strings.TrimSpace(title)
	}
	const maxEvidenceChars = 4000
	if len(text) > maxEvidenceChars {
		return text[:maxEvidenceChars]
	}
	return text
}

func copyStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func digestForPage(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}
