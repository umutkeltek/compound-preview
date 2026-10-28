package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
)

const DecisionSourceAuthoritySchemaVersion = "compound.decision_source_authority.v0.1"

// SourceAuthorityRecord records the handoff from a legacy decision source to a
// Compound decision ref. The source remains evidence/writeback material; it is
// no longer the active context authority for this decision slice.
type SourceAuthorityRecord struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceKind          string   `json:"source_kind"`
	SourceRole          string   `json:"source_role"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	DecisionID          string   `json:"decision_id"`
	EvidenceID          string   `json:"evidence_id"`
	CandidateID         string   `json:"candidate_id"`
	ClaimID             string   `json:"claim_id,omitempty"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

type SourceAuthorityCoverageReport struct {
	Status         string
	Records        []SourceAuthorityRecord
	Eligible       []string
	Missing        []string
	CountsByKind   map[string]int
	CountsByStatus map[string]int
}

func RecordSourceAuthority(homePath string, report DecisionImportReport, importedVia string, now time.Time) (SourceAuthorityRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(report.SourcePath) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("source path required")
	}
	decisionID := strings.TrimSpace(report.Lifecycle.ID)
	if decisionID == "" {
		decisionID = report.CandidateID
	}
	rec := SourceAuthorityRecord{
		SchemaVersion:       DecisionSourceAuthoritySchemaVersion,
		ID:                  decisionSourceAuthorityID(report.SourcePath, decisionID, importedVia),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          report.SourcePath,
		SourceKind:          firstNonEmpty(report.SourceKind, "decision_source"),
		SourceRole:          "source_in_place_evidence",
		ImportedVia:         firstNonEmpty(importedVia, "decisions import"),
		AuthorityRef:        "decision://" + decisionID,
		AuthorityStatus:     authorityStatusForImport(report),
		DecisionID:          decisionID,
		EvidenceID:          report.EvidenceID,
		CandidateID:         report.CandidateID,
		ClaimID:             report.ClaimID,
		Retires:             "decision_source_authority:" + report.SourcePath,
		RetirementCondition: "future agents consume the decision via Compound ContextPack; source updates go through reviewed writeback/import",
		EvidenceRefs:        cleanRefs([]string{report.EvidenceID, report.CandidateID, report.ClaimID}),
	}
	if err := appendSourceAuthority(homePath, rec); err != nil {
		return SourceAuthorityRecord{}, err
	}
	return rec, nil
}

func LoadSourceAuthority(homePath string) ([]SourceAuthorityRecord, error) {
	path := decisionSourceAuthorityLog(homePath)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []SourceAuthorityRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	byKey := map[string]SourceAuthorityRecord{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec SourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		if rec.ID == "" {
			return nil, fmt.Errorf("decision source authority record missing id")
		}
		key := sourceAuthorityKey(rec)
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = rec
	}
	out := make([]SourceAuthorityRecord, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out, nil
}

func SourceAuthorityCoverage(homePath string, eligiblePaths []string) (SourceAuthorityCoverageReport, error) {
	records, err := LoadSourceAuthority(homePath)
	if err != nil {
		return SourceAuthorityCoverageReport{}, err
	}
	report := SourceAuthorityCoverageReport{
		Status:         "ok",
		Records:        records,
		CountsByKind:   map[string]int{},
		CountsByStatus: map[string]int{},
	}
	covered := map[string]bool{}
	for _, rec := range records {
		covered[cleanPathForCoverage(rec.SourcePath)] = true
		report.CountsByKind[rec.SourceKind]++
		report.CountsByStatus[rec.AuthorityStatus]++
	}
	eligibleSeen := map[string]bool{}
	for _, path := range eligiblePaths {
		clean := cleanPathForCoverage(path)
		if clean == "" || eligibleSeen[clean] {
			continue
		}
		eligibleSeen[clean] = true
		report.Eligible = append(report.Eligible, clean)
		if !covered[clean] {
			report.Missing = append(report.Missing, clean)
		}
	}
	sort.Strings(report.Eligible)
	sort.Strings(report.Missing)
	if len(report.Missing) > 0 {
		report.Status = "missing"
	}
	return report, nil
}

func appendSourceAuthority(homePath string, rec SourceAuthorityRecord) error {
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(decisionSourceAuthorityLog(homePath), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func decisionSourceAuthorityLog(homePath string) string {
	return filepath.Join(homePath, "registries", "decision-source-authority.jsonl")
}

func decisionSourceAuthorityID(path, decisionID, importedVia string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{cleanPathForCoverage(path), decisionID, importedVia}, "|")))
	return "decsrc_" + hex.EncodeToString(sum[:])[:12]
}

func authorityStatusForImport(report DecisionImportReport) string {
	if strings.TrimSpace(report.ClaimID) != "" && report.Verdict == candidates.Promote {
		return "compound_primary"
	}
	switch report.Verdict {
	case candidates.Park:
		return "compound_parked"
	case candidates.Reject:
		return "compound_rejected"
	default:
		return "compound_candidate"
	}
}

func sourceAuthorityKey(rec SourceAuthorityRecord) string {
	if strings.TrimSpace(rec.Retires) != "" {
		return strings.TrimSpace(rec.Retires)
	}
	return strings.Join([]string{cleanPathForCoverage(rec.SourcePath), rec.AuthorityRef}, "|")
}

func cleanPathForCoverage(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func cleanRefs(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
