package capgraph

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
)

const (
	SourceAuthoritySchemaVersion = "compound.capability_source_authority.v0.1"
	CapabilityGraphAuthorityRef  = "compound://registries/capability-graph"
)

// SourceAuthorityRecord records the authority handoff for an imported capability
// source. The source file remains evidence/rollback input; the Compound graph is
// the surface future consumers should treat as authority.
type SourceAuthorityRecord struct {
	SchemaVersion          string   `json:"schema_version"`
	ID                     string   `json:"id"`
	CreatedAt              string   `json:"created_at"`
	SourceObservedAt       string   `json:"source_observed_at,omitempty"`
	SourceAgeHoursAtImport int      `json:"source_age_hours_at_import"`
	SourcePath             string   `json:"source_path"`
	SourceKind             string   `json:"source_kind"`
	SourceOwner            string   `json:"source_owner"`
	SourceClass            string   `json:"source_class"`
	SourceRevision         string   `json:"source_revision"`
	ImportedVia            string   `json:"imported_via"`
	AuthorityRef           string   `json:"authority_ref"`
	AuthorityStatus        string   `json:"authority_status"`
	SourceRole             string   `json:"source_role"`
	Retires                string   `json:"retires"`
	Rollback               string   `json:"rollback"`
	NodeIDs                []string `json:"node_ids"`
	EdgeIDs                []string `json:"edge_ids"`
	SourceObservationID    string   `json:"source_observation_id,omitempty"`
	NoPromoteIDs           []string `json:"no_promote_ids,omitempty"`
	EvidenceRefs           []string `json:"evidence_refs"`
}

type SourceAuthorityInput struct {
	SourcePath          string
	SourceKind          string
	SourceOwner         string
	SourceClass         string
	SourceRevision      string
	SourceObservedAt    string
	ImportedVia         string
	NodeIDs             []string
	EdgeIDs             []string
	SourceObservationID string
	NoPromoteIDs        []string
	EvidenceRefs        []string
}

type SourceAuthoritySummary struct {
	SourceRoleCounts            map[string]int `json:"source_role_counts"`
	RetiredSourceAuthorityCount int            `json:"retired_source_authority_count"`
	RetiredSourceAuthorityRefs  []string       `json:"retired_source_authority_refs,omitempty"`
}

func SummarizeSourceAuthority(records []SourceAuthorityRecord) SourceAuthoritySummary {
	out := SourceAuthoritySummary{SourceRoleCounts: map[string]int{}}
	retired := map[string]bool{}
	for _, rec := range records {
		role := strings.TrimSpace(rec.SourceRole)
		if role == "" {
			role = "unknown"
		}
		out.SourceRoleCounts[role]++
		if ref := strings.TrimSpace(rec.Retires); ref != "" && !retired[ref] {
			retired[ref] = true
			out.RetiredSourceAuthorityRefs = append(out.RetiredSourceAuthorityRefs, ref)
		}
	}
	sort.Strings(out.RetiredSourceAuthorityRefs)
	out.RetiredSourceAuthorityCount = len(out.RetiredSourceAuthorityRefs)
	return out
}

func RecordSourceAuthority(homePath string, in SourceAuthorityInput, now time.Time) (SourceAuthorityRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(in.SourcePath) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("source path required")
	}
	if strings.TrimSpace(in.SourceRevision) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("source revision required")
	}
	if strings.TrimSpace(in.ImportedVia) == "" {
		return SourceAuthorityRecord{}, fmt.Errorf("imported_via required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	sourceKind := firstNonEmpty(in.SourceKind, "generated_view")
	sourceClass := firstNonEmpty(in.SourceClass, "source")
	sourceObservedAt := strings.TrimSpace(in.SourceObservedAt)
	if sourceObservedAt == "" {
		sourceObservedAt = now.UTC().Format(time.RFC3339)
	}
	sourceObservedTime, err := time.Parse(time.RFC3339, sourceObservedAt)
	if err != nil {
		return SourceAuthorityRecord{}, fmt.Errorf("source_observed_at must be RFC3339: %w", err)
	}
	sourceAgeHours := int(now.UTC().Sub(sourceObservedTime.UTC()).Hours())
	if sourceAgeHours < 0 {
		sourceAgeHours = 0
	}
	rec := SourceAuthorityRecord{
		SchemaVersion:          SourceAuthoritySchemaVersion,
		ID:                     sourceAuthorityID(sourceKind, sourceClass, in.SourceRevision, in.ImportedVia),
		CreatedAt:              now.UTC().Format(time.RFC3339),
		SourceObservedAt:       sourceObservedTime.UTC().Format(time.RFC3339),
		SourceAgeHoursAtImport: sourceAgeHours,
		SourcePath:             strings.TrimSpace(in.SourcePath),
		SourceKind:             sourceKind,
		SourceOwner:            firstNonEmpty(in.SourceOwner, "capability-source"),
		SourceClass:            sourceClass,
		SourceRevision:         strings.TrimSpace(in.SourceRevision),
		ImportedVia:            strings.TrimSpace(in.ImportedVia),
		AuthorityRef:           CapabilityGraphAuthorityRef,
		AuthorityStatus:        "compound_authoritative",
		SourceRole:             "evidence_only_input",
		Retires:                "source_view_authority:" + strings.TrimSpace(in.SourcePath),
		Rollback:               "re-import this source revision or a newer source; source file remains unchanged",
		NodeIDs:                cleanSorted(in.NodeIDs),
		EdgeIDs:                cleanSorted(in.EdgeIDs),
		SourceObservationID:    strings.TrimSpace(in.SourceObservationID),
		NoPromoteIDs:           cleanSorted(in.NoPromoteIDs),
		EvidenceRefs:           cleanSorted(in.EvidenceRefs),
	}
	if len(rec.NodeIDs) == 0 {
		return SourceAuthorityRecord{}, fmt.Errorf("source authority requires at least one imported node")
	}
	path := sourceAuthorityLog(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return SourceAuthorityRecord{}, err
	}
	if err := appendJSONL(path, &rec); err != nil {
		return SourceAuthorityRecord{}, err
	}
	return rec, nil
}

func LoadSourceAuthority(homePath string) ([]SourceAuthorityRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("COMPOUND_HOME required")
	}
	byID := map[string]SourceAuthorityRecord{}
	var order []string
	err := readJSONL(sourceAuthorityLog(homePath), func(line []byte) error {
		var rec SourceAuthorityRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		if strings.TrimSpace(rec.ID) == "" {
			return fmt.Errorf("source authority record missing id")
		}
		backfillSourceAuthorityRecord(&rec)
		if _, seen := byID[rec.ID]; !seen {
			order = append(order, rec.ID)
		}
		byID[rec.ID] = rec
		return nil
	})
	out := make([]SourceAuthorityRecord, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, err
}

func backfillSourceAuthorityRecord(rec *SourceAuthorityRecord) {
	if strings.TrimSpace(rec.SourceObservedAt) == "" {
		rec.SourceObservedAt = rec.CreatedAt
	}
	if rec.SourceAgeHoursAtImport != 0 {
		return
	}
	importedAt, importedErr := time.Parse(time.RFC3339, rec.CreatedAt)
	observedAt, observedErr := time.Parse(time.RFC3339, rec.SourceObservedAt)
	if importedErr != nil || observedErr != nil {
		return
	}
	ageHours := int(importedAt.UTC().Sub(observedAt.UTC()).Hours())
	if ageHours < 0 {
		ageHours = 0
	}
	rec.SourceAgeHoursAtImport = ageHours
}

// AuthorityRevision returns a content digest for the graph authority state that
// determines capability surfacing. It reads append-only graph logs only; source
// authority records remain handoff evidence, not offer state.
func AuthorityRevision(homePath string) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("COMPOUND_HOME required")
	}
	var parts []string
	for _, name := range []string{"capability-nodes.jsonl", "capability-edges.jsonl"} {
		data, err := os.ReadFile(filepath.Join(homePath, "registries", name))
		if err != nil {
			if os.IsNotExist(err) {
				parts = append(parts, name+":missing")
				continue
			}
			return "", err
		}
		sum := sha256.Sum256(data)
		parts = append(parts, name+":"+hex.EncodeToString(sum[:]))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])[:12], nil
}

func sourceAuthorityLog(homePath string) string {
	return filepath.Join(homePath, "registries", "capability-source-authority.jsonl")
}

func sourceAuthorityID(kind, class, revision, via string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{kind, class, revision, via}, "|")))
	return "capsrc_" + hex.EncodeToString(sum[:])[:12]
}

func cleanSorted(values []string) []string {
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
