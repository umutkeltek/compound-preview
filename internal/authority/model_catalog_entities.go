package authority

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/mem"
)

// model_catalog_entities.go is the import door for a provider+model catalog
// (the switchback-shaped provider-registry.json schema: a flat "models" array
// of {provider_id, model_id, display_name, upstream_model_ids[], pricing,
// context_window, tier, source_url, workload_kind, ...} rows plus a top-level
// "deprecated" list). It follows the same shape as the other single-file
// import doors in this repo (infra_registries.go, routeevidence's
// ImportModelCapabilitiesYAML): read the source file, do light structural
// validation, install a verbatim copy under $COMPOUND_HOME/registries/, and
// append an evidence-bearing record to an append-only source-authority
// ledger — reusing the EXISTING model-router authority ref
// (routeevidence.ModelCapabilitiesAuthorityRef's value; not re-imported here
// to avoid a cross-package dependency for one string literal, grandfathered
// in internal/arch/mint.go's grandfatheredRegistries as "model-router").
// Unlike infra_registries.go (a generic two-file split: importer +
// entities/capabilities), this one file also registers each row as a
// resolvable mem.Entity and enrolls it as a capability-graph node, mirroring
// infra_service_entities.go's attach-point pattern end to end (settled for
// work_07ea9df822a7, slice 1 of work_ab70ae305a91).
const (
	// ModelCatalogAuthorityRef reuses the existing model-router authority ref
	// (never mint a new compound://registries/* name for this door).
	ModelCatalogAuthorityRef          = "compound://registries/model-router"
	ModelCatalogSourceAuthoritySchema = "compound.model_catalog_source_authority.v0.1"

	// ModelCatalogEntityKind is the mem.Entity kind used for every model:*
	// entity registered from the catalog. mem.EntityKinds has no dedicated
	// "model" value and extending it is out of this packet's scope (the
	// TOUCH ONLY boundary excludes internal/mem/entity.go), so this reuses
	// the closest existing kind — the same reuse-don't-extend call
	// infra-services made with "server" for InfraServiceEntityKind.
	ModelCatalogEntityKind = "tool"

	// ModelCatalogCapabilityKind is the capgraph.NodeKinds value minted for
	// model nodes. "model_profile" already exists in NodeKinds and is
	// currently unused by any minting path (verified in the packet's scout);
	// this is its first consumer, zero capgraph.go edits required.
	ModelCatalogCapabilityKind = "model_profile"

	modelCatalogRegistryFile        = "provider-catalog.json"
	modelCatalogSourceAuthorityFile = "provider-catalog-source-authority.jsonl"
	modelCatalogImportedVia         = "authority import-model-catalog"
)

// ModelCatalogModelRow is one decoded "models[]" entry from the source file.
type ModelCatalogModelRow struct {
	ProviderID               string   `json:"provider_id"`
	ModelID                  string   `json:"model_id"`
	DisplayName              string   `json:"display_name"`
	UpstreamModelIDs         []string `json:"upstream_model_ids"`
	InputMicrosPerMtok       int64    `json:"input_micros_per_mtok"`
	OutputMicrosPerMtok      int64    `json:"output_micros_per_mtok"`
	CachedInputMicrosPerMtok int64    `json:"cached_input_micros_per_mtok"`
	ContextWindow            int      `json:"context_window"`
	Tier                     string   `json:"tier"`
	SourceURL                string   `json:"source_url"`
	WorkloadKind             string   `json:"workload_kind"`
}

// ModelCatalogDeprecatedEntry is one decoded top-level "deprecated[]" entry —
// a model retired for a specific set of providers.
type ModelCatalogDeprecatedEntry struct {
	Model     string   `json:"model"`
	Providers []string `json:"providers"`
	Status    string   `json:"status"`
	Note      string   `json:"note"`
}

// ModelCatalogManifest is the decoded top-level source document. Only the
// fields this door needs are modeled; unrecognized top-level keys (sources,
// money, pricing_units, provider_catalogs, ...) are ignored by
// encoding/json, never re-modeled here.
type ModelCatalogManifest struct {
	Schema     string                        `json:"schema"`
	Models     []ModelCatalogModelRow        `json:"models"`
	Deprecated []ModelCatalogDeprecatedEntry `json:"deprecated"`
}

// ModelCatalogSourceAuthorityRecord is the append-only evidence ledger entry
// written on every import-model-catalog run.
type ModelCatalogSourceAuthorityRecord struct {
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
	ModelCount          int      `json:"model_count"`
	ProviderCount       int      `json:"provider_count"`
	DeprecatedCount     int      `json:"deprecated_count"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

// ModelCatalogImportReport summarizes an ImportModelCatalog run.
type ModelCatalogImportReport struct {
	AuthorityRef      string                            `json:"authority_ref"`
	AuthorityPath     string                            `json:"authority_path"`
	SourceAuthorityID string                            `json:"source_authority_id"`
	SourceAuthority   ModelCatalogSourceAuthorityRecord `json:"source_authority"`
	ModelCount        int                               `json:"model_count"`
	ProviderCount     int                               `json:"provider_count"`
	DeprecatedCount   int                               `json:"deprecated_count"`
}

// ModelCatalogEntityReport summarizes a RegisterModelCatalogEntities run.
type ModelCatalogEntityReport struct {
	RegisteredSlugs []string `json:"registered_slugs,omitempty"`
	SkippedSlugs    []string `json:"skipped_slugs,omitempty"`
}

// ModelCatalogCapabilityReport summarizes an EnrollModelCatalogCapabilities run.
type ModelCatalogCapabilityReport struct {
	NodeIDs           []string `json:"node_ids,omitempty"`
	SourceAuthorityID string   `json:"source_authority_id,omitempty"`
}

// ModelCatalogProvenance is the additive per-model detail `authority resolve`
// prints when a resolved entity is a model:* row.
type ModelCatalogProvenance struct {
	Provider           string
	PriceInUSDPerMtok  string
	PriceOutUSDPerMtok string
	ContextWindow      int
	Tier               string
	SourceRevision     string
}

// ImportModelCatalog installs a verbatim copy of a provider+model catalog
// source file under COMPOUND_HOME/registries/provider-catalog.json and
// records source-authority evidence citing the EXISTING
// compound://registries/model-router authority ref. It does not register
// entities or capability nodes — call RegisterModelCatalogEntities and
// EnrollModelCatalogCapabilities after (same additive-effects shape as
// import-infra-services).
func ImportModelCatalog(homePath, sourcePath string, now time.Time) (ModelCatalogImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return ModelCatalogImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return ModelCatalogImportReport{}, fmt.Errorf("model catalog source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return ModelCatalogImportReport{}, fmt.Errorf("read model catalog %s: %w", sourcePath, err)
	}
	manifest, err := decodeModelCatalogManifest(data)
	if err != nil {
		return ModelCatalogImportReport{}, fmt.Errorf("parse model catalog %s: %w", sourcePath, err)
	}
	if now.IsZero() {
		now = time.Now()
	}

	var report ModelCatalogImportReport
	err = home.WithAutonomyAdmissionLock(homePath, func() error {
		var installErr error
		report, installErr = installModelCatalog(homePath, sourcePath, data, manifest, now)
		return installErr
	})
	return report, err
}

func decodeModelCatalogManifest(data []byte) (ModelCatalogManifest, error) {
	var manifest ModelCatalogManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ModelCatalogManifest{}, fmt.Errorf("parse json: %w", err)
	}
	if len(manifest.Models) == 0 {
		return ModelCatalogManifest{}, fmt.Errorf("model catalog has no models")
	}
	return manifest, nil
}

func installModelCatalog(homePath, sourcePath string, data []byte, manifest ModelCatalogManifest, now time.Time) (ModelCatalogImportReport, error) {
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return ModelCatalogImportReport{}, err
	}
	authorityPath := ModelCatalogRegistryPath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return ModelCatalogImportReport{}, err
	}
	revision := "sha256:" + digestBytes(data)
	deprecatedCount := 0
	providers := map[string]bool{}
	for _, row := range manifest.Models {
		providers[row.ProviderID] = true
	}
	deprecated := modelCatalogDeprecatedSet(manifest)
	for _, row := range manifest.Models {
		if deprecated[row.ProviderID+"|"+row.ModelID] {
			deprecatedCount++
		}
	}
	rec := ModelCatalogSourceAuthorityRecord{
		SchemaVersion:       ModelCatalogSourceAuthoritySchema,
		ID:                  "mdlcatsrc_" + digest12(strings.Join([]string{sourcePath, revision, modelCatalogImportedVia}, "|")),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      revision,
		ImportedVia:         modelCatalogImportedVia,
		AuthorityRef:        ModelCatalogAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "compound route/model-router reads compound://registries/model-router from COMPOUND_HOME; the provider-registry source file remains rollback/evidence input",
		Rollback:            "re-import source provider-registry.json or remove COMPOUND_HOME registries/" + modelCatalogRegistryFile,
		ModelCount:          len(manifest.Models),
		ProviderCount:       len(providers),
		DeprecatedCount:     deprecatedCount,
		EvidenceRefs:        []string{revision, ModelCatalogAuthorityRef},
	}
	if err := appendModelCatalogSourceAuthority(homePath, rec); err != nil {
		return ModelCatalogImportReport{}, err
	}
	return ModelCatalogImportReport{
		AuthorityRef:      ModelCatalogAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		ModelCount:        rec.ModelCount,
		ProviderCount:     rec.ProviderCount,
		DeprecatedCount:   rec.DeprecatedCount,
	}, nil
}

// modelCatalogDeprecatedSet indexes the top-level "deprecated" list as
// "<provider_id>|<model_id>" -> true, so a model row can be checked for
// retirement in O(1) regardless of how many providers one deprecated entry
// spans.
func modelCatalogDeprecatedSet(manifest ModelCatalogManifest) map[string]bool {
	set := map[string]bool{}
	for _, dep := range manifest.Deprecated {
		model := strings.TrimSpace(dep.Model)
		if model == "" {
			continue
		}
		for _, provider := range dep.Providers {
			provider = strings.TrimSpace(provider)
			if provider == "" {
				continue
			}
			set[provider+"|"+model] = true
		}
	}
	return set
}

// modelCatalogEnrollableRows returns manifest.Models minus any row matching
// the top-level deprecated list — the rows RegisterModelCatalogEntities and
// EnrollModelCatalogCapabilities actually enroll.
func modelCatalogEnrollableRows(manifest ModelCatalogManifest) []ModelCatalogModelRow {
	deprecated := modelCatalogDeprecatedSet(manifest)
	out := make([]ModelCatalogModelRow, 0, len(manifest.Models))
	for _, row := range manifest.Models {
		if deprecated[row.ProviderID+"|"+row.ModelID] {
			continue
		}
		out = append(out, row)
	}
	return out
}

func modelCatalogEntitySlug(row ModelCatalogModelRow) string {
	return "model:" + row.ProviderID + "/" + row.ModelID
}

// modelCatalogAliases builds the model_id + display_name + upstream_model_ids
// alias set, lowercased and deduped (settled decision: lowercased consistent
// with existing entity conventions — resolveEntity's alias tier already
// compares case-insensitively, so this keeps the stored form canonical).
func modelCatalogAliases(row ModelCatalogModelRow) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(row.ModelID)
	add(row.DisplayName)
	for _, upstream := range row.UpstreamModelIDs {
		add(upstream)
	}
	return out
}

// modelCatalogIdentifiers is source_url only, per the settled entity shape —
// deliberately narrower than infra-services' full-key identifier.
func modelCatalogIdentifiers(row ModelCatalogModelRow) []string {
	url := strings.TrimSpace(row.SourceURL)
	if url == "" {
		return nil
	}
	return []string{url}
}

// modelCatalogTriggers is the settled, deliberately narrow trigger set:
// model_id, provider_id, display_name, and workload_kind — no generic tokens
// like "model" or "cheap" (122 nodes with generic triggers would spam every
// surface). Reuses infra_service_entities.go's generic dedupe helper.
func modelCatalogTriggers(row ModelCatalogModelRow) []string {
	values := []string{row.ModelID, row.ProviderID, row.DisplayName}
	if row.WorkloadKind != "" {
		values = append(values, row.WorkloadKind)
	}
	return dedupeInfraServiceTriggers(values)
}

// RegisterModelCatalogEntities reads the installed provider-catalog.json
// (ImportModelCatalog must already have run) and registers one mem.Entity
// per enrollable model row via mergeEntity, skipping deprecated rows.
// Re-running against an unchanged catalog is a no-op per slug (reuses
// infra_service_entities.go's infraServiceEntityRegistered guard, which is
// fully generic despite its name), so re-import never appends duplicate rows
// to ledger/entities.jsonl.
func RegisterModelCatalogEntities(store *mem.Store, homePath string, now time.Time) (ModelCatalogEntityReport, error) {
	manifest, err := loadInstalledModelCatalogManifest(homePath)
	if err != nil {
		return ModelCatalogEntityReport{}, err
	}
	existing, err := store.Entities()
	if err != nil {
		return ModelCatalogEntityReport{}, err
	}
	var report ModelCatalogEntityReport
	for _, row := range modelCatalogEnrollableRows(manifest) {
		slug := modelCatalogEntitySlug(row)
		merged := mergeEntity(existing, slug, ModelCatalogEntityKind, modelCatalogAliases(row), modelCatalogIdentifiers(row))
		if infraServiceEntityRegistered(existing, merged) {
			report.SkippedSlugs = append(report.SkippedSlugs, slug)
			continue
		}
		registered, err := store.RegisterEntity(merged, now)
		if err != nil {
			return report, err
		}
		existing = append(existing, registered)
		report.RegisteredSlugs = append(report.RegisteredSlugs, slug)
	}
	return report, nil
}

// EnrollModelCatalogCapabilities reads the installed provider-catalog.json
// and mints one capgraph "model_profile" node per enrollable row (idempotent:
// capgraph.Store.Add already no-ops an unchanged node by ID), then records
// source-authority provenance mirroring
// infra_service_entities.go's EnrollInfraServiceCapabilities.
func EnrollModelCatalogCapabilities(homePath string, now time.Time) (ModelCatalogCapabilityReport, error) {
	manifest, err := loadInstalledModelCatalogManifest(homePath)
	if err != nil {
		return ModelCatalogCapabilityReport{}, err
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		return ModelCatalogCapabilityReport{}, err
	}
	var nodeIDs []string
	for _, row := range modelCatalogEnrollableRows(manifest) {
		node := capgraph.Node{
			ID:       modelCatalogEntitySlug(row),
			Kind:     ModelCatalogCapabilityKind,
			Name:     firstNonEmpty(row.DisplayName, row.ModelID),
			Triggers: modelCatalogTriggers(row),
		}
		if err := store.Add(node); err != nil {
			return ModelCatalogCapabilityReport{}, err
		}
		nodeIDs = append(nodeIDs, node.ID)
	}
	installedPath := ModelCatalogRegistryPath(homePath)
	data, err := os.ReadFile(installedPath)
	if err != nil {
		return ModelCatalogCapabilityReport{}, err
	}
	revision := "sha256:" + digestBytes(data)
	auth, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:       installedPath,
		SourceKind:       "model_catalog_registry",
		SourceOwner:      "model-catalog-import",
		SourceClass:      "operational_registry",
		SourceRevision:   revision,
		SourceObservedAt: now.UTC().Format(time.RFC3339),
		ImportedVia:      modelCatalogImportedVia,
		NodeIDs:          nodeIDs,
		EvidenceRefs:     []string{ModelCatalogAuthorityRef, revision},
	}, now)
	if err != nil {
		return ModelCatalogCapabilityReport{}, err
	}
	return ModelCatalogCapabilityReport{NodeIDs: nodeIDs, SourceAuthorityID: auth.ID}, nil
}

// ResolveModelCatalogProvenance looks up the provider/price/context/tier/
// source_revision detail for a resolved model:<provider>/<model> entity
// slug, feeding the CLI's additive `authority resolve` output. Returns
// found=false (no error) when slug is not a model:* entity, is malformed, or
// has no matching installed provider-catalog.json row (e.g. a stale/renamed
// entry).
func ResolveModelCatalogProvenance(homePath, slug string) (ModelCatalogProvenance, bool, error) {
	const prefix = "model:"
	if !strings.HasPrefix(slug, prefix) {
		return ModelCatalogProvenance{}, false, nil
	}
	providerID, modelID, ok := splitModelCatalogSlugRest(strings.TrimPrefix(slug, prefix))
	if !ok {
		return ModelCatalogProvenance{}, false, nil
	}
	manifest, err := loadInstalledModelCatalogManifest(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return ModelCatalogProvenance{}, false, nil
		}
		return ModelCatalogProvenance{}, false, err
	}
	var match *ModelCatalogModelRow
	for i := range manifest.Models {
		if manifest.Models[i].ProviderID == providerID && manifest.Models[i].ModelID == modelID {
			match = &manifest.Models[i]
			break
		}
	}
	if match == nil {
		return ModelCatalogProvenance{}, false, nil
	}
	records, err := LoadModelCatalogSourceAuthority(homePath)
	if err != nil {
		return ModelCatalogProvenance{}, false, err
	}
	revision := ""
	if len(records) > 0 {
		revision = records[len(records)-1].SourceRevision
	}
	return ModelCatalogProvenance{
		Provider:           match.ProviderID,
		PriceInUSDPerMtok:  formatMicrosPerMtokUSD(match.InputMicrosPerMtok),
		PriceOutUSDPerMtok: formatMicrosPerMtokUSD(match.OutputMicrosPerMtok),
		ContextWindow:      match.ContextWindow,
		Tier:               match.Tier,
		SourceRevision:     revision,
	}, true, nil
}

// splitModelCatalogSlugRest splits "<provider_id>/<model_id>" on the FIRST
// slash only (a model_id may itself contain slashes; provider_id never
// does), rejecting a rest with no slash or an empty half.
func splitModelCatalogSlugRest(rest string) (providerID, modelID string, ok bool) {
	idx := strings.Index(rest, "/")
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// formatMicrosPerMtokUSD converts a micros-per-Mtok integer price (1 USD =
// 1,000,000 micros) to an exact decimal USD string using integer arithmetic
// only — no float64 division, so there is no rounding drift between the
// stored integer and the printed price (e.g. 140000 -> "0.14").
func formatMicrosPerMtokUSD(micros int64) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	whole := micros / 1_000_000
	frac := micros % 1_000_000
	fracStr := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	out := fmt.Sprintf("%d", whole)
	if fracStr != "" {
		out = fmt.Sprintf("%d.%s", whole, fracStr)
	}
	if neg {
		out = "-" + out
	}
	return out
}

func ModelCatalogRegistryPath(homePath string) string {
	return filepath.Join(homePath, "registries", modelCatalogRegistryFile)
}

func ModelCatalogSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", modelCatalogSourceAuthorityFile)
}

func LoadModelCatalogSourceAuthority(homePath string) ([]ModelCatalogSourceAuthorityRecord, error) {
	data, err := os.ReadFile(ModelCatalogSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []ModelCatalogSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec ModelCatalogSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse model catalog source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func appendModelCatalogSourceAuthority(homePath string, rec ModelCatalogSourceAuthorityRecord) error {
	path := ModelCatalogSourceAuthorityPath(homePath)
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

func loadInstalledModelCatalogManifest(homePath string) (ModelCatalogManifest, error) {
	data, err := os.ReadFile(ModelCatalogRegistryPath(homePath))
	if err != nil {
		return ModelCatalogManifest{}, err
	}
	return decodeModelCatalogManifest(data)
}
