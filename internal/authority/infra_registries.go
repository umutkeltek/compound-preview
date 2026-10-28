package authority

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

	"compound/internal/home"
	"gopkg.in/yaml.v3"
)

// infra_registries.go is the import door for the infra-os operational
// registries (database.yaml, services.yaml). It follows the same shape as
// the other single-file import doors in this repo (policy.ImportPlacementRules,
// routeevidence.ImportModelCapabilitiesYAML, authority.ImportJobRoster): read
// the source file, do light structural validation, install a verbatim copy
// under $COMPOUND_HOME/registries/, and append an evidence-bearing record to
// an append-only source-authority ledger. Neither file's schema is rigid
// (infra-os owns arbitrary nested host/service/product shape), so the door
// validates only that the required top-level section exists and is a
// non-empty mapping; it does not attempt to model every field, and it never
// hardcodes any of infra-os's real host/service/product names.
const (
	InfraDatabaseAuthorityRef = "compound://registries/data-sources"
	InfraServicesAuthorityRef = "compound://registries/services"

	InfraRegistrySourceAuthoritySchema = "compound.infra_registry_source_authority.v0.1"

	infraDatabaseRegistryFile        = "data-sources.yaml"
	infraServicesRegistryFile        = "services.yaml"
	infraDatabaseSourceAuthorityFile = "infra-database-source-authority.jsonl"
	infraServicesSourceAuthorityFile = "infra-services-source-authority.jsonl"
	infraDatabaseImportedVia         = "authority import-infra-database"
	infraServicesImportedVia         = "authority import-infra-services"
	infraDatabaseRequiredKey         = "databases"
	infraServicesRequiredKey         = "services"

	// infraRegistryAuthorityStatusNote is the authority-status wording every
	// infra registry source-authority record carries in its retirement
	// condition. AuthorityStatus itself stays the plain "compound_primary"
	// token, matching every other source-authority record in this repo, so
	// nothing that compares it exactly ever breaks.
	infraRegistryAuthorityStatusNote = "compound_primary, source YAML remains evidence/rollback input"
)

// InfraRegistrySourceAuthorityRecord is the append-only evidence ledger entry
// written on every import-infra-database / import-infra-services run.
type InfraRegistrySourceAuthorityRecord struct {
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
	TopLevelKeys        []string `json:"top_level_keys"`
	EntryCount          int      `json:"entry_count"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

type InfraRegistryImportReport struct {
	AuthorityRef      string                             `json:"authority_ref"`
	AuthorityPath     string                             `json:"authority_path"`
	SourceAuthorityID string                             `json:"source_authority_id"`
	SourceAuthority   InfraRegistrySourceAuthorityRecord `json:"source_authority"`
	TopLevelKeys      []string                           `json:"top_level_keys"`
	EntryCount        int                                `json:"entry_count"`
}

type infraRegistryImportSpec struct {
	authorityRef   string
	registryFile   string
	sourceAuthFile string
	importedVia    string
	requiredKey    string
	retirementNote string
}

// ImportInfraDatabaseRegistry installs infra-os's database.yaml as
// compound://registries/data-sources.
func ImportInfraDatabaseRegistry(homePath, sourcePath string, now time.Time) (InfraRegistryImportReport, error) {
	return importInfraRegistry(homePath, sourcePath, now, infraRegistryImportSpec{
		authorityRef:   InfraDatabaseAuthorityRef,
		registryFile:   infraDatabaseRegistryFile,
		sourceAuthFile: infraDatabaseSourceAuthorityFile,
		importedVia:    infraDatabaseImportedVia,
		requiredKey:    infraDatabaseRequiredKey,
		retirementNote: "runtime data-source reads read compound://registries/data-sources from COMPOUND_HOME; infra-os database.yaml remains rollback/evidence input",
	})
}

// ImportInfraServicesRegistry installs infra-os's services.yaml as
// compound://registries/services.
func ImportInfraServicesRegistry(homePath, sourcePath string, now time.Time) (InfraRegistryImportReport, error) {
	return importInfraRegistry(homePath, sourcePath, now, infraRegistryImportSpec{
		authorityRef:   InfraServicesAuthorityRef,
		registryFile:   infraServicesRegistryFile,
		sourceAuthFile: infraServicesSourceAuthorityFile,
		importedVia:    infraServicesImportedVia,
		requiredKey:    infraServicesRequiredKey,
		retirementNote: "runtime service-topology reads read compound://registries/services from COMPOUND_HOME; infra-os services.yaml remains rollback/evidence input",
	})
}

func importInfraRegistry(homePath, sourcePath string, now time.Time, spec infraRegistryImportSpec) (InfraRegistryImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return InfraRegistryImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return InfraRegistryImportReport{}, fmt.Errorf("infra registry source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return InfraRegistryImportReport{}, fmt.Errorf("read infra registry %s: %w", sourcePath, err)
	}
	entryCount, topKeys, err := decodeInfraRegistryTopLevel(data, spec.requiredKey)
	if err != nil {
		return InfraRegistryImportReport{}, fmt.Errorf("parse infra registry %s: %w", sourcePath, err)
	}
	if now.IsZero() {
		now = time.Now()
	}

	var report InfraRegistryImportReport
	err = home.WithAutonomyAdmissionLock(homePath, func() error {
		var installErr error
		report, installErr = installInfraRegistry(homePath, sourcePath, data, entryCount, topKeys, now, spec)
		return installErr
	})
	return report, err
}

func installInfraRegistry(homePath, sourcePath string, data []byte, entryCount int, topKeys []string, now time.Time, spec infraRegistryImportSpec) (InfraRegistryImportReport, error) {
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return InfraRegistryImportReport{}, err
	}
	authorityPath := filepath.Join(homePath, "registries", spec.registryFile)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return InfraRegistryImportReport{}, err
	}
	revision := "sha256:" + digestBytes(data)
	rec := InfraRegistrySourceAuthorityRecord{
		SchemaVersion:       InfraRegistrySourceAuthoritySchema,
		ID:                  infraRegistrySourceAuthorityID(sourcePath, revision, spec.importedVia),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      revision,
		ImportedVia:         spec.importedVia,
		AuthorityRef:        spec.authorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: spec.retirementNote + " (" + infraRegistryAuthorityStatusNote + ")",
		Rollback:            fmt.Sprintf("re-import source %s or remove COMPOUND_HOME registries/%s", filepath.Base(sourcePath), spec.registryFile),
		TopLevelKeys:        topKeys,
		EntryCount:          entryCount,
		EvidenceRefs:        []string{revision, spec.authorityRef},
	}
	if err := appendInfraRegistrySourceAuthority(homePath, spec.sourceAuthFile, rec); err != nil {
		return InfraRegistryImportReport{}, err
	}
	return InfraRegistryImportReport{
		AuthorityRef:      spec.authorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		TopLevelKeys:      topKeys,
		EntryCount:        entryCount,
	}, nil
}

// decodeInfraRegistryTopLevel validates that data is a YAML mapping and that
// requiredKey is present as a non-empty mapping. It returns the size of that
// mapping (a coarse "entry_count") and the sorted list of top-level keys, so
// the import report/evidence carries a real structural signal without the
// door having to model infra-os's full (and privacy-laden) schema.
func decodeInfraRegistryTopLevel(data []byte, requiredKey string) (int, []string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return 0, nil, fmt.Errorf("parse yaml: %w", err)
	}
	if len(doc) == 0 {
		return 0, nil, fmt.Errorf("registry yaml has no top-level keys")
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	raw, ok := doc[requiredKey]
	if !ok {
		return 0, keys, fmt.Errorf("registry yaml missing required top-level key %q", requiredKey)
	}
	section, ok := raw.(map[string]any)
	if !ok || len(section) == 0 {
		return 0, keys, fmt.Errorf("registry yaml key %q must be a non-empty mapping", requiredKey)
	}
	return len(section), keys, nil
}

func InfraDatabaseRegistryPath(homePath string) string {
	return filepath.Join(homePath, "registries", infraDatabaseRegistryFile)
}

func InfraServicesRegistryPath(homePath string) string {
	return filepath.Join(homePath, "registries", infraServicesRegistryFile)
}

func InfraDatabaseSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", infraDatabaseSourceAuthorityFile)
}

func InfraServicesSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", infraServicesSourceAuthorityFile)
}

func LoadInfraDatabaseSourceAuthority(homePath string) ([]InfraRegistrySourceAuthorityRecord, error) {
	return loadInfraRegistrySourceAuthority(InfraDatabaseSourceAuthorityPath(homePath))
}

func LoadInfraServicesSourceAuthority(homePath string) ([]InfraRegistrySourceAuthorityRecord, error) {
	return loadInfraRegistrySourceAuthority(InfraServicesSourceAuthorityPath(homePath))
}

func loadInfraRegistrySourceAuthority(path string) ([]InfraRegistrySourceAuthorityRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []InfraRegistrySourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec InfraRegistrySourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse infra registry source authority %s line %d: %w", path, i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func appendInfraRegistrySourceAuthority(homePath, file string, rec InfraRegistrySourceAuthorityRecord) error {
	path := filepath.Join(homePath, "registries", file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func infraRegistrySourceAuthorityID(path, revision, importedVia string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{path, revision, importedVia}, "|")))
	return "infrasrc_" + hex.EncodeToString(sum[:])[:12]
}
