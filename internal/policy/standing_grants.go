package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	homepkg "compound/internal/home"
)

const (
	StandingGrantsAuthorityRef          = "compound://registries/standing-grants"
	StandingGrantsRegistrySchema        = "compound.standing_grants.v0.1"
	StandingGrantsSourceAuthoritySchema = "compound.standing_grants_source_authority.v0.1"
	standingGrantsFile                  = "standing-grants.json"
	standingGrantsSourceAuthorityFile   = "standing-grants-source-authority.jsonl"
	standingGrantsImportedVia           = "policy import-standing-grants"
)

type StandingGrantRegistry struct {
	SchemaVersion string          `json:"schema_version" yaml:"schema_version"`
	GeneratedAt   string          `json:"generated_at,omitempty" yaml:"generated_at,omitempty"`
	Grants        []StandingGrant `json:"grants" yaml:"grants"`
}

type StandingGrantSource struct {
	SchemaVersion  string                        `json:"schema_version,omitempty" yaml:"schema_version,omitempty"`
	GeneratedAt    string                        `json:"generated_at,omitempty" yaml:"generated_at,omitempty"`
	Authority      *StandingGrantSourceAuthority `json:"authority,omitempty" yaml:"authority,omitempty"`
	StandingGrants []StandingGrant               `json:"standing_grants,omitempty" yaml:"standing_grants,omitempty"`
	Grants         []OSPipelineGrantSource       `json:"grants,omitempty" yaml:"grants,omitempty"`
}

// StandingGrantSourceAuthority is the operator-provenance decision citation an
// import must carry. The import mints auto-allow authority, so the source file
// must name the decision record that authorized it (decision_ref) and the
// operator identity that reviewed it (reviewed_by) — an executor-authored file
// asserting its own grants is exactly the bypass this door refuses.
type StandingGrantSourceAuthority struct {
	DecisionRef string `json:"decision_ref" yaml:"decision_ref"`
	ReviewedBy  string `json:"reviewed_by" yaml:"reviewed_by"`
}

type OSPipelineGrantSource struct {
	GrantID              string   `json:"grant_id" yaml:"grant_id"`
	Capabilities         []string `json:"capabilities" yaml:"capabilities"`
	TargetScope          string   `json:"target_scope" yaml:"target_scope"`
	MutationClass        string   `json:"mutation_class,omitempty" yaml:"mutation_class,omitempty"`
	ReversibilityCeiling string   `json:"reversibility_ceiling" yaml:"reversibility_ceiling"`
	BlastRadius          string   `json:"blast_radius,omitempty" yaml:"blast_radius,omitempty"`
	ValidUntil           string   `json:"valid_until,omitempty" yaml:"valid_until,omitempty"`
	MaxUSD               *float64 `json:"max_usd,omitempty" yaml:"max_usd,omitempty"`
	MaxTokens            *int     `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

type StandingGrantsImportReport struct {
	AuthorityRef      string                              `json:"authority_ref"`
	AuthorityPath     string                              `json:"authority_path"`
	SourceAuthorityID string                              `json:"source_authority_id"`
	SourceAuthority   StandingGrantsSourceAuthorityRecord `json:"source_authority"`
	GeneratedAt       string                              `json:"generated_at,omitempty"`
	GrantCount        int                                 `json:"grant_count"`
	CapabilityCount   int                                 `json:"capability_count"`
}

type StandingGrantsSourceAuthorityRecord struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	GrantIDs            []string `json:"grant_ids"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func ImportStandingGrants(homePath, sourcePath string, now time.Time) (StandingGrantsImportReport, error) {
	var report StandingGrantsImportReport
	var importErr error
	err := homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		report, importErr = importStandingGrantsLocked(homePath, sourcePath, now)
		return importErr
	})
	return report, err
}

func importStandingGrantsLocked(homePath, sourcePath string, now time.Time) (StandingGrantsImportReport, error) {
	source, data, err := loadStandingGrantSource(sourcePath)
	if err != nil {
		return StandingGrantsImportReport{}, err
	}
	if err := validateStandingGrantImportAuthority(source); err != nil {
		return StandingGrantsImportReport{}, err
	}
	registry, err := NormalizeStandingGrantSource(source)
	if err != nil {
		return StandingGrantsImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return StandingGrantsImportReport{}, err
	}
	authorityPath := StandingGrantsPath(homePath)
	body, err := json.MarshalIndent(registry, "", " ")
	if err != nil {
		return StandingGrantsImportReport{}, err
	}
	if err := os.WriteFile(authorityPath, append(body, '\n'), 0o644); err != nil {
		return StandingGrantsImportReport{}, err
	}
	sourceRevision := digestHex(data)
	rec := StandingGrantsSourceAuthorityRecord{
		SchemaVersion:       StandingGrantsSourceAuthoritySchema,
		ID:                  standingGrantsSourceAuthorityID(sourcePath, sourceRevision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         standingGrantsImportedVia,
		AuthorityRef:        StandingGrantsAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "config_only_authority:policy.yaml:standing_grants",
		RetirementCondition: "Policy evaluation reads compound://registries/standing-grants from COMPOUND_HOME; policy.yaml standing_grants remains rollback/evidence input",
		Rollback:            "re-import standing-grants source or remove COMPOUND_HOME registries/standing-grants.json",
		GrantIDs:            standingGrantIDs(registry.Grants),
		EvidenceRefs:        []string{"sha256:" + sourceRevision, StandingGrantsAuthorityRef, strings.TrimSpace(source.Authority.DecisionRef)},
	}
	if err := appendStandingGrantsSourceAuthority(homePath, rec); err != nil {
		return StandingGrantsImportReport{}, err
	}
	return StandingGrantsImportReport{
		AuthorityRef:      StandingGrantsAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		GeneratedAt:       registry.GeneratedAt,
		GrantCount:        len(registry.Grants),
		CapabilityCount:   countStandingGrantCapabilities(registry.Grants),
	}, nil
}

// validateStandingGrantImportAuthority is the provenance door on minting
// auto-allow authority: the import must cite an operator-provenance decision
// record (authority.decision_ref + authority.reviewed_by). Without it an
// executor-authored file could grant itself — including a grant with no
// expires_at, which never expires and so must never proceed uncited.
func validateStandingGrantImportAuthority(source StandingGrantSource) error {
	if source.Authority == nil || strings.TrimSpace(source.Authority.DecisionRef) == "" {
		return fmt.Errorf("standing grants import requires an authority block citing the authorizing decision record (authority.decision_ref); a grant without expires_at never proceeds without one")
	}
	if !HasOperatorProvenance(source.Authority.ReviewedBy) {
		return fmt.Errorf("standing grants import requires operator provenance on authority.reviewed_by, got %q", source.Authority.ReviewedBy)
	}
	return nil
}

func NormalizeStandingGrantSource(source StandingGrantSource) (StandingGrantRegistry, error) {
	registry := StandingGrantRegistry{
		SchemaVersion: StandingGrantsRegistrySchema,
		GeneratedAt:   strings.TrimSpace(source.GeneratedAt),
	}
	for _, grant := range source.StandingGrants {
		normalized, err := normalizeStandingGrant(grant)
		if err != nil {
			return StandingGrantRegistry{}, err
		}
		registry.Grants = append(registry.Grants, normalized)
	}
	for _, grant := range source.Grants {
		normalized, err := normalizeOSPipelineGrant(grant)
		if err != nil {
			return StandingGrantRegistry{}, err
		}
		registry.Grants = append(registry.Grants, normalized...)
	}
	if len(registry.Grants) == 0 {
		return StandingGrantRegistry{}, fmt.Errorf("standing grants source has no grants")
	}
	sort.Slice(registry.Grants, func(i, j int) bool {
		if registry.Grants[i].ID == registry.Grants[j].ID {
			return registry.Grants[i].Capability < registry.Grants[j].Capability
		}
		return registry.Grants[i].ID < registry.Grants[j].ID
	})
	return registry, nil
}

func LoadStandingGrantsFromHome(homePath string) (StandingGrantRegistry, error) {
	data, err := os.ReadFile(StandingGrantsPath(homePath))
	if err != nil {
		return StandingGrantRegistry{}, err
	}
	var registry StandingGrantRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return StandingGrantRegistry{}, err
	}
	return registry, nil
}

func ApplyStandingGrantsFromHome(cfg *Config, homePath string) error {
	if cfg == nil || strings.TrimSpace(homePath) == "" {
		return nil
	}
	registry, err := LoadStandingGrantsFromHome(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg.StandingGrants = append(cfg.StandingGrants, registry.Grants...)
	cfg.PolicyEvidenceRefs = mergeStrings(cfg.PolicyEvidenceRefs, StandingGrantsAuthorityRef)
	return nil
}

func StandingGrantsPath(homePath string) string {
	return filepath.Join(homePath, "registries", standingGrantsFile)
}

func StandingGrantsSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", standingGrantsSourceAuthorityFile)
}

func LoadStandingGrantsSourceAuthority(homePath string) ([]StandingGrantsSourceAuthorityRecord, error) {
	data, err := os.ReadFile(StandingGrantsSourceAuthorityPath(homePath))
	if err != nil {
		return nil, err
	}
	var records []StandingGrantsSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec StandingGrantsSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse standing grants source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func loadStandingGrantSource(path string) (StandingGrantSource, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StandingGrantSource{}, nil, err
	}
	var source StandingGrantSource
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&source); err != nil {
		return StandingGrantSource{}, nil, err
	}
	return source, data, nil
}

func normalizeOSPipelineGrant(source OSPipelineGrantSource) ([]StandingGrant, error) {
	id := strings.TrimSpace(source.GrantID)
	if id == "" {
		return nil, fmt.Errorf("standing grant source grant_id required")
	}
	target := normalizeOSTargetScope(source.TargetScope)
	if target == "" {
		return nil, fmt.Errorf("standing grant %q target_scope required", id)
	}
	if len(source.Capabilities) == 0 {
		return nil, fmt.Errorf("standing grant %q capabilities required", id)
	}
	ceiling, err := normalizeReversibility(source.ReversibilityCeiling)
	if err != nil {
		return nil, fmt.Errorf("standing grant %q invalid reversibility_ceiling: %w", id, err)
	}
	mutationClass := normalizeGrantMutationClass(source.MutationClass)
	var out []StandingGrant
	for _, capability := range source.Capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			continue
		}
		grantID := id
		if len(source.Capabilities) > 1 {
			grantID = id + ":" + normalizeGrantIDPart(capability)
		}
		out = append(out, StandingGrant{
			ID:                   grantID,
			Capability:           capability,
			MutationClass:        mutationClass,
			ReversibilityCeiling: ceiling,
			BlastRadius:          strings.TrimSpace(source.BlastRadius),
			TargetPath:           target,
			MaxUSD:               source.MaxUSD,
			MaxTokens:            source.MaxTokens,
			ExpiresAt:            strings.TrimSpace(source.ValidUntil),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("standing grant %q capabilities required", id)
	}
	for _, grant := range out {
		if _, err := normalizeStandingGrant(grant); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeStandingGrant(grant StandingGrant) (StandingGrant, error) {
	grant.ID = strings.TrimSpace(grant.ID)
	grant.Capability = strings.TrimSpace(grant.Capability)
	grant.MutationClass = normalizeGrantMutationClass(grant.MutationClass)
	grant.Reversibility = strings.TrimSpace(grant.Reversibility)
	grant.ReversibilityCeiling = strings.TrimSpace(grant.ReversibilityCeiling)
	grant.BlastRadius = strings.TrimSpace(grant.BlastRadius)
	grant.TargetPath = strings.TrimSpace(grant.TargetPath)
	grant.ExpiresAt = strings.TrimSpace(grant.ExpiresAt)
	grant.NotBefore = strings.TrimSpace(grant.NotBefore)
	grant.RequiresPresence = strings.ToLower(strings.TrimSpace(grant.RequiresPresence))
	if grant.ID == "" || grant.Capability == "" {
		return StandingGrant{}, fmt.Errorf("standing grant id and capability required")
	}
	if grant.Reversibility != "" && grant.ReversibilityCeiling != "" {
		return StandingGrant{}, fmt.Errorf("standing grant %q cannot set both reversibility and reversibility_ceiling", grant.ID)
	}
	if grant.Reversibility != "" {
		normalized, err := normalizeReversibility(grant.Reversibility)
		if err != nil {
			return StandingGrant{}, fmt.Errorf("standing grant %q invalid reversibility: %w", grant.ID, err)
		}
		grant.Reversibility = normalized
	}
	if grant.ReversibilityCeiling != "" {
		normalized, err := normalizeReversibility(grant.ReversibilityCeiling)
		if err != nil {
			return StandingGrant{}, fmt.Errorf("standing grant %q invalid reversibility_ceiling: %w", grant.ID, err)
		}
		grant.ReversibilityCeiling = normalized
	}
	var expiresAt time.Time
	if grant.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, grant.ExpiresAt)
		if err != nil {
			return StandingGrant{}, fmt.Errorf("standing grant %q invalid expires_at: %w", grant.ID, err)
		}
		expiresAt = t
	}
	if grant.NotBefore != "" {
		notBefore, err := time.Parse(time.RFC3339, grant.NotBefore)
		if err != nil {
			return StandingGrant{}, fmt.Errorf("standing grant %q invalid not_before: %w", grant.ID, err)
		}
		if !expiresAt.IsZero() && notBefore.After(expiresAt) {
			return StandingGrant{}, fmt.Errorf("standing grant %q not_before %s is after expires_at %s; the grant could never be eligible", grant.ID, grant.NotBefore, grant.ExpiresAt)
		}
	}
	switch grant.RequiresPresence {
	case "", PresenceAny, PresencePresent, PresenceAway:
	default:
		return StandingGrant{}, fmt.Errorf("standing grant %q invalid requires_presence %q: must be any|present|away", grant.ID, grant.RequiresPresence)
	}
	return grant, nil
}

func normalizeReversibility(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "reversible", "irreversible":
		return normalized, nil
	default:
		return "", fmt.Errorf("%q must be reversible|irreversible", value)
	}
}

func reversibilityWithinCeiling(actual, ceiling string) bool {
	actualRank, actualOK := reversibilityRank(actual)
	ceilingRank, ceilingOK := reversibilityRank(ceiling)
	return actualOK && ceilingOK && actualRank <= ceilingRank
}

func reversibilityRank(value string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "reversible", "easy":
		return 0, true
	case "irreversible", "manual", "hard", "none":
		return 1, true
	default:
		return 2, false
	}
}

func normalizeGrantMutationClass(value string) string {
	switch strings.TrimSpace(value) {
	case "reversible_local":
		return "local_reversible"
	default:
		return strings.TrimSpace(value)
	}
}

func normalizeOSTargetScope(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "**") {
		return value
	}
	return strings.ReplaceAll(value, "*", "**")
}

func normalizeGrantIDPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func appendStandingGrantsSourceAuthority(homePath string, rec StandingGrantsSourceAuthorityRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := StandingGrantsSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func standingGrantIDs(grants []StandingGrant) []string {
	ids := make([]string, 0, len(grants))
	for _, grant := range grants {
		if strings.TrimSpace(grant.ID) != "" {
			ids = append(ids, strings.TrimSpace(grant.ID))
		}
	}
	sort.Strings(ids)
	return ids
}

func countStandingGrantCapabilities(grants []StandingGrant) int {
	seen := map[string]bool{}
	for _, grant := range grants {
		if strings.TrimSpace(grant.Capability) != "" {
			seen[grant.Capability] = true
		}
	}
	return len(seen)
}

func standingGrantsSourceAuthorityID(path, revision string) string {
	return "sgsrc_" + digestHex([]byte(strings.Join([]string{path, revision, standingGrantsImportedVia}, "|")))[:12]
}
