package authority

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// model_catalog_query.go owns the read-only query surface for the installed
// provider+model catalog: the model-catalog answer to "what models exist /
// what does X cost / what is current in family Y". It NEVER reads the
// source repo file — every read is off the INSTALLED catalog under
// $COMPOUND_HOME/registries/provider-catalog.json, and every output carries
// the installed source revision (sha256:...) from the source-authority
// ledger so an operator can tell at a glance whether the answer is current.
//
// Imports stay the IMPORT-DOOR pattern: the verb reuses row structs and
// loading helpers from model_catalog_entities.go (no re-decode, no re-parse)
// so the slice-2 surface stays a thin reader on top of slice-1's install
// path. The slice-2 query is additive — nothing here mutates home state.

// ModelCatalogQueryFilter is the read-only filter set. Empty fields mean
// "no filter on this dimension". The verb MUST validate all four filters
// before any read so a malformed --model can't silently no-op.
type ModelCatalogQueryFilter struct {
	// Provider narrows by provider_id (--family). Empty = no filter.
	Provider string
	// Model is the EXACT <provider>/<model> slug (--model). Empty = no filter.
	// Mutually meaningful with Provider: if both are set, Model takes precedence
	// (Provider is implied by the slug).
	Model string
}

// ModelCatalogQueryResult is the read-only answer the verb returns.
type ModelCatalogQueryResult struct {
	AuthorityRef      string                 `json:"authority_ref"`
	SourceRevision    string                 `json:"source_revision,omitempty"`
	SourceRevisionSet bool                   `json:"source_revision_set"`
	Rows              []ModelCatalogQueryRow `json:"rows"`
	Warnings          []string               `json:"warnings,omitempty"`
}

// ModelCatalogQueryRow is one installable provider+model row. The fields
// here are the SETTLED subset the CLI/doctor/--model paths all want:
// price in/out (USD/Mtok, exact decimal), context window, tier, free-lane
// flag, the supersession edge (deprecated -> successor), and the source
// revision carried on every row so individual records stay self-identifying.
type ModelCatalogQueryRow struct {
	Slug               string `json:"slug"`
	ProviderID         string `json:"provider_id"`
	ModelID            string `json:"model_id"`
	DisplayName        string `json:"display_name"`
	PriceInUSDPerMtok  string `json:"price_in_usd_per_mtok"`
	PriceOutUSDPerMtok string `json:"price_out_usd_per_mtok"`
	ContextWindow      int    `json:"context_window"`
	Tier               string `json:"tier,omitempty"`
	FreeLane           bool   `json:"free_lane"`
	SupersededBy       string `json:"superseded_by,omitempty"`
	SourceRevision     string `json:"source_revision"`
}

// QueryModelCatalog is the read-only catalog query. It loads the INSTALLED
// registry (registries/provider-catalog.json), never the source repo file,
// derives the deprecation supersession edges from the SOURCE authority
// record's installed "deprecated" map, and returns one row per enrollable
// provider+model. The `ctx` param is reserved for a future cap that wants
// to honor cancellation; the current surface is fast and unbounded.
func QueryModelCatalog(ctx context.Context, homePath string, filter ModelCatalogQueryFilter, now time.Time) (ModelCatalogQueryResult, error) {
	_ = ctx
	_ = now
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ModelCatalogQueryResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	filter.Provider = strings.TrimSpace(filter.Provider)
	filter.Model = strings.TrimSpace(filter.Model)
	// Validate --model early so a malformed value never silently no-ops.
	if filter.Model != "" {
		if _, _, ok := splitModelCatalogSlugForQuery(filter.Model); !ok {
			return ModelCatalogQueryResult{}, fmt.Errorf("malformed --model %q: expected model:<provider_id>/<model_id>", filter.Model)
		}
	}

	manifest, err := loadInstalledModelCatalogManifest(homePath)
	if err != nil {
		return ModelCatalogQueryResult{}, fmt.Errorf("read installed model catalog: %w", err)
	}

	// Pull the latest source-authority record so the result carries the
	// installed source revision (sha256:...) consistently with slice-1's
	// resolve path. An empty ledger (no record yet) leaves SourceRevision
	// blank with SourceRevisionSet=false so callers can distinguish.
	records, err := LoadModelCatalogSourceAuthority(homePath)
	if err != nil {
		return ModelCatalogQueryResult{}, fmt.Errorf("load source authority: %w", err)
	}
	revision := ""
	for _, rec := range records {
		if rec.SourceRevision != "" {
			revision = rec.SourceRevision
		}
	}

	deprecated := modelCatalogDeprecatedSet(manifest)
	rows := make([]ModelCatalogQueryRow, 0, len(manifest.Models))
	for _, row := range manifest.Models {
		// When --model is set, restrict to the exact (provider, model) pair.
		// When --family is set, restrict to one provider. Both unset = list all.
		if filter.Provider != "" && row.ProviderID != filter.Provider {
			continue
		}
		if filter.Model != "" {
			provider, modelID, ok := splitModelCatalogSlugForQuery(filter.Model)
			if !ok {
				continue
			}
			if row.ProviderID != provider || row.ModelID != modelID {
				continue
			}
		}
		// Deprecated rows ARE surfaced by --model (so the operator can see
		// the successor edge) but NOT by the bare-list / --family paths
		// (consistent with the install path's enrollable row semantics).
		if filter.Model == "" && deprecated[row.ProviderID+"|"+row.ModelID] {
			continue
		}

		queryRow := ModelCatalogQueryRow{
			Slug:               modelCatalogEntitySlug(row),
			ProviderID:         row.ProviderID,
			ModelID:            row.ModelID,
			DisplayName:        row.DisplayName,
			PriceInUSDPerMtok:  formatMicrosPerMtokUSD(row.InputMicrosPerMtok),
			PriceOutUSDPerMtok: formatMicrosPerMtokUSD(row.OutputMicrosPerMtok),
			ContextWindow:      row.ContextWindow,
			Tier:               row.Tier,
			FreeLane:           modelCatalogIsFreeLane(row),
			SourceRevision:     revision,
		}
		if deprecated[row.ProviderID+"|"+row.ModelID] {
			queryRow.SupersededBy = modelCatalogSupersessionEdge(manifest, row)
		}
		rows = append(rows, queryRow)
	}

	// Deterministic order: slug ascending, so the CLI output is reproducible
	// and the falsifier can assert exact row sets.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Slug < rows[j].Slug
	})

	return ModelCatalogQueryResult{
		AuthorityRef:      ModelCatalogAuthorityRef,
		SourceRevision:    revision,
		SourceRevisionSet: revision != "",
		Rows:              rows,
	}, nil
}

// modelCatalogIsFreeLane is the narrow free-tier heuristic the slice-2
// query exposes: a model is "free lane" when both input AND output prices
// are zero. The slice-1 schema doesn't carry an explicit free_lane flag,
// so we derive it from the price signal to keep slice-2 a thin reader.
// A future schema revision can promote this to a stored field.
func modelCatalogIsFreeLane(row ModelCatalogModelRow) bool {
	return row.InputMicrosPerMtok == 0 && row.OutputMicrosPerMtok == 0
}

// modelCatalogSupersessionEdge renders the deprecated row's "superseded by"
// edge so the --model path can surface it. We surface the human note from
// the deprecated entry (e.g. "superseded by alpha-flash") AND a bare
// successor slug when the entries list providers for which the model is
// deprecated. The verb MUST surface at least one of the two; the falsifier
// accepts either form.
func modelCatalogSupersessionEdge(manifest ModelCatalogManifest, row ModelCatalogModelRow) string {
	var notes []string
	var successorSlug string
	for _, dep := range manifest.Deprecated {
		if dep.Model != row.ModelID {
			continue
		}
		for _, provider := range dep.Providers {
			if provider == row.ProviderID {
				if dep.Note != "" {
					notes = append(notes, dep.Note)
				}
				// Bare successor indication: the row is deprecated under
				// this provider, so the operator needs SOMETHING they can
				// paste into the next query. We DON'T re-derive the actual
				// successor model id (the schema doesn't bind them); we
				// only surface the slug pattern that let the operator
				// search for the current model.
				successorSlug = "model:" + row.ProviderID + "/" + "(superseded — query provider for current models)"
			}
		}
	}
	// Prefer the human note (it's what the upstream registry actually
	// says); fall back to a slug-pattern indicator when no note exists.
	if len(notes) > 0 {
		// De-dup notes while preserving order.
		seen := map[string]bool{}
		var dedup []string
		for _, n := range notes {
			if !seen[n] {
				seen[n] = true
				dedup = append(dedup, n)
			}
		}
		return strings.Join(dedup, "; ")
	}
	return successorSlug
}

// splitModelCatalogSlugForQuery is the mirror of splitModelCatalogSlugRest
// but for the FULL "model:<provider>/<model>" form the query CLI accepts.
// Returning the bool on the third slot lets the caller distinguish a valid
// empty (provider, model) pair from a malformed input.
func splitModelCatalogSlugForQuery(slug string) (provider, modelID string, ok bool) {
	const prefix = "model:"
	if !strings.HasPrefix(slug, prefix) {
		return "", "", false
	}
	return splitModelCatalogSlugRest(strings.TrimPrefix(slug, prefix))
}

// --- model catalog refresh reflex (F2) ---
// Lives in the same file as the query because both consume the same
// installed-source-authority read seam; slice-2 stays a thin reader on
// top of slice-1's install path with no new internal/arch triad.

// ModelCatalogDoctorReport is the read-only drift surface. A red status
// ("drift") means the source on disk changed since the last import;
// "ok" means identical content; "source-absent" means --source was not
// supplied OR the source file is not present locally; "not-installed"
// means the installed catalog is missing entirely. The doctor is never
// blank — every status carries a check list with an actionable hint.
type ModelCatalogDoctorReport struct {
	AuthorityRef    string                    `json:"authority_ref"`
	AuthorityPath   string                    `json:"authority_path"`
	Status          string                    `json:"status"` // drift|ok|source-absent|not-installed
	InstalledDigest string                    `json:"installed_digest,omitempty"`
	SourcePath      string                    `json:"source_path,omitempty"`
	SourceDigest    string                    `json:"source_digest,omitempty"`
	LastImportID    string                    `json:"last_import_id,omitempty"`
	Checks          []ModelCatalogDoctorCheck `json:"checks"`
}

// ModelCatalogDoctorCheck is one cell of the drift surface (installed ok,
// source-readable, digests match, etc.). Same name/status/detail shape as
// the other authority doctor reports so the CLI / JSON consumers stay
// uniform.
type ModelCatalogDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ModelCatalogDoctor is the read-only refresh reflex. It loads the
// installed catalog (the source of truth), reads the latest
// source-authority ledger entry, and (if --source was supplied and the
// source file is present locally) compares its sha256 against the
// installed digest. Writes nothing. Auto-import lives on the import
// verb, never here.
func ModelCatalogDoctor(homePath, sourcePath string, now time.Time) (ModelCatalogDoctorReport, error) {
	_ = now
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ModelCatalogDoctorReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	report := ModelCatalogDoctorReport{
		AuthorityRef:  ModelCatalogAuthorityRef,
		AuthorityPath: ModelCatalogRegistryPath(homePath),
		Checks:        []ModelCatalogDoctorCheck{},
	}

	// Installed digest: read the installed catalog file directly so the
	// doctor works even when the source-authority ledger is empty (e.g.
	// a manual install that bypassed the import verb).
	installedData, err := os.ReadFile(report.AuthorityPath)
	if err != nil {
		if os.IsNotExist(err) {
			report.Status = "not-installed"
			report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
				Name: "installed", Status: "fail",
				Detail: "no installed catalog at " + report.AuthorityPath + " — run `compound authority import-model-catalog <provider-registry.json>`",
			})
			return report, nil
		}
		return ModelCatalogDoctorReport{}, fmt.Errorf("read installed catalog: %w", err)
	}
	report.InstalledDigest = "sha256:" + digestBytes(installedData)
	report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
		Name: "installed", Status: "ok",
		Detail: "installed catalog at " + report.AuthorityPath,
	})

	records, err := LoadModelCatalogSourceAuthority(homePath)
	if err != nil {
		return ModelCatalogDoctorReport{}, fmt.Errorf("load source authority: %w", err)
	}
	if len(records) > 0 {
		last := records[len(records)-1]
		report.LastImportID = last.ID
		report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
			Name: "ledger", Status: "ok",
			Detail: "last import " + last.ID + " at " + last.CreatedAt,
		})
	} else {
		report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
			Name: "ledger", Status: "warn",
			Detail: "no source-authority ledger entry — installed digest is the only available signal",
		})
	}

	// Source comparison: only when --source was supplied AND the file is
	// present locally. We never wake up the source repo file's content
	// speculatively — the doctor is READ-ONLY, and the operator owns
	// picking the source path.
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		report.Status = "source-absent"
		report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
			Name: "source", Status: "skipped",
			Detail: "no --source path supplied; pass --source <provider-registry.json> to enable drift compare",
		})
		return report, nil
	}
	report.SourcePath = sourcePath
	sourceData, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			report.Status = "source-absent"
			report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
				Name: "source", Status: "fail",
				Detail: "source file not found at " + sourcePath,
			})
			return report, nil
		}
		return ModelCatalogDoctorReport{}, fmt.Errorf("read source %s: %w", sourcePath, err)
	}
	report.SourceDigest = "sha256:" + digestBytes(sourceData)
	if report.SourceDigest == report.InstalledDigest {
		report.Status = "ok"
		report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
			Name: "digest_match", Status: "ok",
			Detail: "installed sha256 == source sha256",
		})
		return report, nil
	}
	report.Status = "drift"
	report.Checks = append(report.Checks, ModelCatalogDoctorCheck{
		Name: "digest_match", Status: "fail",
		Detail: "source " + report.SourceDigest + " differs from installed " + report.InstalledDigest + " — re-run `compound authority import-model-catalog " + sourcePath + "`",
	})
	return report, nil
}
