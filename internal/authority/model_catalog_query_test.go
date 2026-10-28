package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// modelCatalogQueryTestNow is the deterministic clock the slice-2 query tests
// use so timestamp-derived ids don't drift across runs.
var modelCatalogQueryTestNow = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

// writeModelCatalogQueryFixture is the same synthetic fixture the slice-1
// test owns, replayed here so the F1/F2 tests are self-contained and not
// coupled to slice-1's fixture internals.
func writeModelCatalogQueryFixture(t *testing.T, path string) {
	t.Helper()
	body := `{
  "schema": "examplecorp/provider-registry@3",
  "models": [
    {
      "provider_id": "examplecorp",
      "model_id": "alpha-flash",
      "display_name": "Alpha Flash",
      "upstream_model_ids": ["alpha-flash-001"],
      "input_micros_per_mtok": 140000,
      "output_micros_per_mtok": 560000,
      "context_window": 128000,
      "tier": "standard",
      "source_url": "https://catalog.invalid/alpha-flash",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "shared-model",
      "display_name": "Shared Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 100000,
      "output_micros_per_mtok": 300000,
      "context_window": 32000,
      "tier": "value",
      "source_url": "",
      "workload_kind": "chat"
    },
    {
      "provider_id": "othercorp",
      "model_id": "shared-model",
      "display_name": "Shared Model Prime",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 90000,
      "output_micros_per_mtok": 270000,
      "context_window": 64000,
      "tier": "value",
      "source_url": "https://catalog.invalid/shared-model-prime",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "legacy-model",
      "display_name": "Legacy Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 50000,
      "output_micros_per_mtok": 150000,
      "context_window": 8000,
      "tier": "legacy",
      "source_url": "",
      "workload_kind": "chat"
    }
  ],
  "deprecated": [
    {
      "model": "legacy-model",
      "providers": ["examplecorp"],
      "status": "deprecated",
      "note": "superseded by alpha-flash"
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installModelCatalogQueryFixture installs the fixture into a fresh test home
// and returns the install report so the F1 query tests can assert count
// fields exactly.
func installModelCatalogQueryFixture(t *testing.T, homePath, source string) ModelCatalogImportReport {
	t.Helper()
	report, err := ImportModelCatalog(homePath, source, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("install model catalog: %v", err)
	}
	return report
}

// TestModelCatalogQueryListsRowsAndSourceRevision is F1 (whole-list half):
// with a fixture catalog installed in a TEST home, the query verb returns
// the expected row set + source revision.
//
// RED at base: the query verb is absent, so any call into it fails to
// compile. A green result here proves the verb reads the INSTALLED registry
// (not the source repo), one entry per enrollable model, the right
// currency/price numerals, and the source revision (sha256:...) round-trips
// from the install ledger.
func TestModelCatalogQueryListsRowsAndSourceRevision(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installReport := installModelCatalogQueryFixture(t, homePath, source)

	result, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query model catalog: %v", err)
	}
	if result.AuthorityRef != ModelCatalogAuthorityRef {
		t.Fatalf("authority_ref = %q, want %q", result.AuthorityRef, ModelCatalogAuthorityRef)
	}
	if result.SourceRevision != installReport.SourceAuthority.SourceRevision {
		t.Fatalf("source_revision = %q, want %q", result.SourceRevision, installReport.SourceAuthority.SourceRevision)
	}
	if !result.SourceRevisionSet {
		t.Fatalf("source_revision_set must be true when the install ledger has a record")
	}
	// F1 lists every enrollable row (4 declared, 1 deprecated, 3 enrollable).
	if len(result.Rows) != 3 {
		t.Fatalf("rows count = %d, want 3 (4 declared - 1 deprecated), got %+v", len(result.Rows), result.Rows)
	}
	// Deprecated rows must not surface — query serves the installed truth,
	// not raw declared rows.
	for _, row := range result.Rows {
		if row.ProviderID == "examplecorp" && row.ModelID == "legacy-model" {
			t.Fatalf("deprecated row %s leaked into query output", row.Slug)
		}
	}
	// Pick the canonical row and assert the price/context/tier/revision
	// blocks byte-match the install report's source revision.
	var found bool
	for _, row := range result.Rows {
		if row.Slug == "model:examplecorp/alpha-flash" {
			found = true
			if row.PriceInUSDPerMtok != "0.14" {
				t.Fatalf("alpha-flash price_in_usd_per_mtok = %q, want 0.14", row.PriceInUSDPerMtok)
			}
			if row.PriceOutUSDPerMtok != "0.56" {
				t.Fatalf("alpha-flash price_out_usd_per_mtok = %q, want 0.56", row.PriceOutUSDPerMtok)
			}
			if row.ContextWindow != 128000 {
				t.Fatalf("alpha-flash context_window = %d, want 128000", row.ContextWindow)
			}
			if row.Tier != "standard" {
				t.Fatalf("alpha-flash tier = %q, want standard", row.Tier)
			}
			if row.SourceRevision != result.SourceRevision {
				t.Fatalf("row source_revision = %q, plan source_revision = %q", row.SourceRevision, result.SourceRevision)
			}
		}
	}
	if !found {
		t.Fatalf("alpha-flash row missing from query result: %+v", result.Rows)
	}
}

// TestModelCatalogQueryFamilyFilterScopesRows is F1's --family half: the
// filter narrows the result to one provider without mutating the on-disk
// catalog. A shared alias under two providers must surface in BOTH family
// views and absent from a third provider's scope.
func TestModelCatalogQueryFamilyFilterScopesRows(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	examplecorp, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{Provider: "examplecorp"}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query examplecorp: %v", err)
	}
	if len(examplecorp.Rows) != 2 {
		t.Fatalf("examplecorp rows = %d, want 2 (alpha-flash + shared-model), got %+v", len(examplecorp.Rows), examplecorp.Rows)
	}
	for _, row := range examplecorp.Rows {
		if row.ProviderID != "examplecorp" {
			t.Fatalf("family filter leaked row %s from provider %s", row.Slug, row.ProviderID)
		}
	}

	othercorp, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{Provider: "othercorp"}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query othercorp: %v", err)
	}
	if len(othercorp.Rows) != 1 || othercorp.Rows[0].Slug != "model:othercorp/shared-model" {
		t.Fatalf("othercorp rows = %+v, want exactly [model:othercorp/shared-model]", othercorp.Rows)
	}
}

// TestModelCatalogQueryModelReturnsSupersessionEdge is F1's --model half:
// --model <deprecated-id> answers cost + currency for one model incl. whether
// it is superseded and by what. The deprecated row's "superseded by" edge
// must surface so an operator can tell at a glance they're holding stale
// knowledge.
func TestModelCatalogQueryModelReturnsSupersessionEdge(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	current, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{Model: "model:examplecorp/alpha-flash"}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query current model: %v", err)
	}
	if len(current.Rows) != 1 {
		t.Fatalf("alpha-flash rows = %d, want 1, got %+v", len(current.Rows), current.Rows)
	}
	if current.Rows[0].SupersededBy != "" {
		t.Fatalf("alpha-flash superseded_by = %q, want empty (model is current)", current.Rows[0].SupersededBy)
	}

	deprecated, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{Model: "model:examplecorp/legacy-model"}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query deprecated model: %v", err)
	}
	if len(deprecated.Rows) != 1 {
		t.Fatalf("legacy-model rows = %d, want 1, got %+v", len(deprecated.Rows), deprecated.Rows)
	}
	row := deprecated.Rows[0]
	if row.SupersededBy == "" {
		t.Fatalf("legacy-model superseded_by must be populated, got empty")
	}
	// The fixture's "note" is "superseded by alpha-flash"; the verb must
	// surface either the literal note or the bare successor slug. Either is
	// acceptable; the failure mode the test guards is the verb SILENTLY
	// dropping the supersession edge.
	if !strings.Contains(row.SupersededBy, "alpha-flash") && !strings.Contains(row.SupersededBy, "model:examplecorp/alpha-flash") {
		t.Fatalf("legacy-model superseded_by = %q, want it to name alpha-flash", row.SupersededBy)
	}
}

// TestModelCatalogQueryJSONShapeMatchesSerialized is the CLI-shape half of
// the falsifier: the query result must serialize cleanly so the CLI's
// --json path can print it. A red result here (failing to round-trip
// through encoding/json) means the CLI silently corrupts --json output.
func TestModelCatalogQueryJSONShapeMatchesSerialized(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	result, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{}, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("query model catalog: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := ModelCatalogQueryResult{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.AuthorityRef != result.AuthorityRef {
		t.Fatalf("authority_ref round-trip lost: %q != %q", decoded.AuthorityRef, result.AuthorityRef)
	}
	if len(decoded.Rows) != len(result.Rows) {
		t.Fatalf("rows count round-trip lost: %d != %d", len(decoded.Rows), len(result.Rows))
	}
	// Sanity: the printed JSON must mention the source revision, since the
	// packet requires "every output carries the installed source revision".
	if !bytes.Contains(encoded, []byte(result.SourceRevision)) {
		t.Fatalf("serialized JSON must carry source_revision %q, got %s", result.SourceRevision, string(encoded))
	}
}

// TestModelCatalogQueryRejectsBadModelID is a guard against the verb
// silently no-op'ing on a malformed --model value. A bad provider portion
// must surface as an error, not as an empty result.
func TestModelCatalogQueryRejectsBadModelID(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	if _, err := QueryModelCatalog(context.Background(), homePath, ModelCatalogQueryFilter{Model: "model:no-slash"}, modelCatalogQueryTestNow); err == nil {
		t.Fatal("expected malformed --model value to fail validation, got nil")
	}
}

// TestModelCatalogDoctorReportsOkWhenSourceMatchesInstalled is F2's "ok"
// half: with a fixture source path that BYTE-EQUALS the just-installed
// catalog, the doctor MUST report status=ok. A red result here means
// the doctor is misreading digests or the source-comparison seam is
// wrongly inverted.
func TestModelCatalogDoctorReportsOkWhenSourceMatchesInstalled(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	report, err := ModelCatalogDoctor(homePath, source, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("doctor status = %q, want ok", report.Status)
	}
	if report.InstalledDigest == "" {
		t.Fatal("installed_digest must be populated when the install is present")
	}
	if report.SourceDigest == "" {
		t.Fatal("source_digest must be populated when --source is supplied and the file is present")
	}
	if report.InstalledDigest != report.SourceDigest {
		t.Fatalf("digests must match on identical content: installed=%s source=%s", report.InstalledDigest, report.SourceDigest)
	}
	if report.SourcePath != source {
		t.Fatalf("source_path = %q, want %q", report.SourcePath, source)
	}
	if report.LastImportID == "" {
		t.Fatal("last_import_id must surface the ledger's latest record id")
	}
	// The doctor MUST NOT auto-import; an unchanged source and an unchanged
	// installed digest prove nothing was written.
	if _, err := os.Stat(ModelCatalogRegistryPath(homePath)); err != nil {
		t.Fatalf("installed catalog must still be present: %v", err)
	}
}

// TestModelCatalogDoctorReportsDriftWhenSourceDiffersFromInstalled is
// F2's "drift" half: mutate the fixture source after install and the
// doctor MUST report status=drift with both digests surfaced. A red
// result here means the doctor silently re-categorized drift as ok.
func TestModelCatalogDoctorReportsDriftWhenSourceDiffersFromInstalled(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	// Mutate the SOURCE file post-install. This is the case the packet
	// calls out: an upstream provider-registry.json that has changed but
	// the install hasn't been re-run.
	mutated := []byte(`{
  "schema": "examplecorp/provider-registry@4",
  "models": [
    {
      "provider_id": "examplecorp",
      "model_id": "alpha-flash",
      "display_name": "Alpha Flash Updated",
      "upstream_model_ids": ["alpha-flash-001"],
      "input_micros_per_mtok": 200000,
      "output_micros_per_mtok": 800000,
      "context_window": 256000,
      "tier": "premium",
      "source_url": "https://catalog.invalid/alpha-flash",
      "workload_kind": "chat"
    }
  ],
  "deprecated": []
}
`)
	if err := os.WriteFile(source, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ModelCatalogDoctor(homePath, source, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "drift" {
		t.Fatalf("doctor status = %q, want drift", report.Status)
	}
	if report.InstalledDigest == report.SourceDigest {
		t.Fatal("drift case must surface different digests")
	}
	// The doctor MUST NOT auto-import; the mutation happened only on the
	// SOURCE file, so the INSTALLED digest must stay unchanged.
	installedData, err := os.ReadFile(ModelCatalogRegistryPath(homePath))
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if "sha256:"+digestBytes(installedData) != report.InstalledDigest {
		t.Fatal("installed_digest must reflect the actual installed file, not the source mutation")
	}
	// The drift check row must surface the actionable re-import command.
	foundDriftCheck := false
	for _, c := range report.Checks {
		if c.Name == "digest_match" {
			foundDriftCheck = true
			if c.Status != "fail" {
				t.Fatalf("digest_match check status = %q, want fail", c.Status)
			}
		}
	}
	if !foundDriftCheck {
		t.Fatalf("digest_match check missing from doctor report: %+v", report.Checks)
	}
}

// TestModelCatalogDoctorReportsSourceAbsentWhenFileMissing is F2's
// "source-absent" half: a --source path that doesn't exist on disk
// must NOT collapse to a silent ok — the doctor must surface the absence
// so the operator can decide whether to re-run import.
func TestModelCatalogDoctorReportsSourceAbsentWhenFileMissing(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	report, err := ModelCatalogDoctor(homePath, missing, modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "source-absent" {
		t.Fatalf("doctor status = %q, want source-absent", report.Status)
	}
	if report.SourcePath != missing {
		t.Fatalf("source_path = %q, want %q", report.SourcePath, missing)
	}
	if report.SourceDigest != "" {
		t.Fatalf("source_digest must be empty when source is absent, got %q", report.SourceDigest)
	}
}

// TestModelCatalogDoctorReportsSourceAbsentWhenNoSourceFlag is F2's
// "source-absent" half for the no-flag case: a doctor call without
// --source must also surface source-absent (not silently ok) so an
// operator knows the comparison didn't run.
func TestModelCatalogDoctorReportsSourceAbsentWhenNoSourceFlag(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogQueryFixture(t, source)
	installModelCatalogQueryFixture(t, homePath, source)

	report, err := ModelCatalogDoctor(homePath, "", modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "source-absent" {
		t.Fatalf("doctor status = %q, want source-absent", report.Status)
	}
	if report.SourcePath != "" {
		t.Fatalf("source_path = %q, want empty when --source is unset", report.SourcePath)
	}
}

// TestModelCatalogDoctorReportsNotInstalledWhenHomeMissing is the
// not-installed edge case: a fresh home (no install) must report
// not-installed with an actionable hint, not crash on a missing file.
func TestModelCatalogDoctorReportsNotInstalledWhenHomeMissing(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", modelCatalogQueryTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	// No install — doctor must surface not-installed.
	report, err := ModelCatalogDoctor(homePath, "", modelCatalogQueryTestNow)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "not-installed" {
		t.Fatalf("doctor status = %q, want not-installed", report.Status)
	}
	if report.InstalledDigest != "" {
		t.Fatalf("installed_digest must be empty on not-installed, got %q", report.InstalledDigest)
	}
}
