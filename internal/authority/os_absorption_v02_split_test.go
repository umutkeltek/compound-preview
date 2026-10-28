package authority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRaw writes a literal file body — used to drop a hand-crafted v0.1
// matrix JSON into the temp dir without going through the struct path
// (which would already be v0.2). Kept tiny on purpose: the test owns the
// JSON it wants to assert migration against.
func writeRaw(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
}

// TestOSAbsorptionV02GbrainFalseRetirement is falsifier #1 (work_b28abf9a8718).
// The matrix row for os_source_gbrain must report
// compound_integration=retired, a non-empty live_consumers list, and an
// integration_verdict of integration_retired_with_live_consumers; the
// top-level doctor check absorption_complete must fail with the row ID
// named in its detail. This is the exact failure mode that the v0.2 meter
// exists to catch — without the split, the row's status=retired read as
// "done" and any retirement decision based on it would be wrong.
func TestOSAbsorptionV02GbrainFalseRetirement(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			{
				ID:                  "os_source_gbrain",
				SourceRef:           "gbrain://db.example.invalid",
				SourceKind:          "memory_engine_index",
				OldOwner:            "gbrain",
				OldRole:             "live Postgres retrieval layer and raw corpus/index access",
				CurrentRole:         "compound refuses to call it as an engine; live services still reach the source",
				Privacy:             "private",
				CompoundOwner:       "compound://adapters/memory-engine/gbrain",
				Consumer:            "compound memory recall/query through ledger visibility and source allowlists",
				Proof:               "gbrain memory-engine profile retired 2026-08-08",
				RetirementGate:      "compound memory (ledger + corpus) is the recall authority",
				Status:              "retired",
				CompoundIntegration: osAbsorptionIntegrationRetired,
				LiveConsumers: []OSAbsorptionLiveConsumer{
					{Name: "hermes stdio watchdog", Surface: "gbrain serve", LastSeenEvidenceDate: "2026-08-23"},
					{Name: "codex mcp-on-demand", Surface: "~/.codex/config.toml", LastSeenEvidenceDate: "2026-08-23"},
					{Name: "nonstop-code configs", Surface: "config:nonstop-code", LastSeenEvidenceDate: "2026-08-23"},
					{Name: "hermes cron jobs writing brain paths", Surface: "~/.hermes/cron/jobs.json", LastSeenEvidenceDate: "2026-08-23"},
					{Name: "34-tool MCP server", Surface: "~/.hermes/config.yaml", LastSeenEvidenceDate: "2026-08-23"},
				},
			},
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	var gbrainRow *OSAbsorptionDoctorRow
	for i := range doctor.Rows {
		if doctor.Rows[i].ID == "os_source_gbrain" {
			gbrainRow = &doctor.Rows[i]
			break
		}
	}
	if gbrainRow == nil {
		t.Fatalf("gbrain row missing from doctor: %+v", doctor.Rows)
	}
	if gbrainRow.CompoundIntegration != osAbsorptionIntegrationRetired {
		t.Fatalf("expected compound_integration=retired, got %q", gbrainRow.CompoundIntegration)
	}
	if len(gbrainRow.LiveConsumers) == 0 {
		t.Fatalf("expected non-empty live_consumers, got 0")
	}
	if gbrainRow.IntegrationVerdict != OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers {
		t.Fatalf("expected verdict %q, got %q", OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers, gbrainRow.IntegrationVerdict)
	}
	var completeCheck *OSAbsorptionDoctorCheck
	for i := range doctor.Checks {
		if doctor.Checks[i].Name == "absorption_complete" {
			completeCheck = &doctor.Checks[i]
			break
		}
	}
	if completeCheck == nil {
		t.Fatalf("absorption_complete check missing from doctor: %+v", doctor.Checks)
	}
	if completeCheck.Status != "fail" {
		t.Fatalf("expected absorption_complete=fail, got %+v", completeCheck)
	}
	if !strings.Contains(completeCheck.Detail, "os_source_gbrain") {
		t.Fatalf("expected detail to name os_source_gbrain, got %q", completeCheck.Detail)
	}
	if doctor.Status != "fail" {
		t.Fatalf("expected doctor.Status=fail while false-retirement is open, got %q", doctor.Status)
	}
}

// TestOSAbsorptionV02RetiredEmptyConsumersComplete is falsifier #2
// (work_b28abf9a8718). A row whose compound_integration=retired and whose
// live_consumers is empty must report verdict=complete and the
// absorption_complete check must pass for that row.
func TestOSAbsorptionV02RetiredEmptyConsumersComplete(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			{
				ID:                  "os_source_quietly_retired",
				SourceRef:           "/synthetic/quietly-retired",
				OldOwner:            "legacy",
				OldRole:             "legacy role",
				CurrentRole:         "retired no live consumers",
				Privacy:             "private",
				CompoundOwner:       "compound://registries/test",
				Consumer:            "none (retired, no live consumer)",
				Proof:               "fully retired, no live consumer",
				RetirementGate:      "no live consumer remains",
				Status:              "retired",
				CompoundIntegration: osAbsorptionIntegrationRetired,
			},
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	var row *OSAbsorptionDoctorRow
	for i := range doctor.Rows {
		if doctor.Rows[i].ID == "os_source_quietly_retired" {
			row = &doctor.Rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("row missing from doctor: %+v", doctor.Rows)
	}
	if row.IntegrationVerdict != OSAbsorptionVerdictComplete {
		t.Fatalf("expected verdict=complete, got %q", row.IntegrationVerdict)
	}
	for _, c := range doctor.Checks {
		if c.Name == "absorption_complete" && c.Status != "ok" {
			t.Fatalf("absorption_complete check should pass for retired+empty; got %+v", c)
		}
	}
}

// TestOSAbsorptionV01SchemaMigratesOnLoad is falsifier #3 (work_b28abf9a8718).
// A v0.1 matrix file written without compound_integration or live_consumers
// must still load (no silent breakage) and the normalizer must migrate the
// row so compound_integration is populated from Status. After migration, a
// v0.1 row with status=retired and empty live_consumers reports verdict
// complete (so the v0.1 fixture stays green end-to-end).
func TestOSAbsorptionV01SchemaMigratesOnLoad(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	raw := `{
  "schema_version": "compound.os_absorption_source_matrix.v0.1",
  "generated_at": "2026-08-08T15:06:42Z",
  "authority_ref": "compound://registries/os-absorption-source-matrix",
  "rows": [
    {
      "id": "os_source_legacy_retired",
      "source_ref": "/synthetic/legacy-source",
      "source_kind": "memory_engine_index",
      "old_owner": "legacy",
      "old_role": "legacy role",
      "current_role": "retired",
      "privacy": "private",
      "compound_owner": "compound://registries/test",
      "consumer": "none",
      "proof": "fully retired",
      "retirement_gate": "no consumer remains",
      "status": "retired"
    }
  ]
}`
	if err := writeRaw(matrixPath, []byte(raw)); err != nil {
		t.Fatalf("write v0.1 matrix: %v", err)
	}
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import v0.1 matrix: %v", err)
	}
	loaded, err := LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	if loaded.SchemaVersion != OSAbsorptionMatrixSchema {
		t.Fatalf("expected post-migration schema %q, got %q", OSAbsorptionMatrixSchema, loaded.SchemaVersion)
	}
	if len(loaded.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(loaded.Rows))
	}
	row := loaded.Rows[0]
	if row.CompoundIntegration != osAbsorptionIntegrationRetired {
		t.Fatalf("expected migrated compound_integration=retired from status=retired, got %q", row.CompoundIntegration)
	}
	if row.Status != "retired" {
		t.Fatalf("legacy status field must be preserved on migration: %+v", row)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	var dr *OSAbsorptionDoctorRow
	for i := range doctor.Rows {
		if doctor.Rows[i].ID == "os_source_legacy_retired" {
			dr = &doctor.Rows[i]
			break
		}
	}
	if dr == nil {
		t.Fatalf("migrated row missing from doctor")
	}
	if dr.IntegrationVerdict != OSAbsorptionVerdictComplete {
		t.Fatalf("expected migrated row verdict=complete, got %q", dr.IntegrationVerdict)
	}
}

// TestOSAbsorptionV02StatusPendingMigratesToActive covers the second
// branch of the v0.1 → v0.2 migration: status=pending becomes
// compound_integration=active. Without this the verdict would report
// "complete" for unmeasured pending rows, which is the false-positive
// shape of the bug the split is closing.
func TestOSAbsorptionV02StatusPendingMigratesToActive(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("os_source_unmeasured_pending", "/synthetic/unmeasured", "pending", "compound://registries/test", "compound memory recall", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	loaded, err := LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	if len(loaded.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(loaded.Rows))
	}
	if loaded.Rows[0].CompoundIntegration != osAbsorptionIntegrationActive {
		t.Fatalf("expected migrated compound_integration=active from status=pending, got %q", loaded.Rows[0].CompoundIntegration)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	for _, r := range doctor.Rows {
		if r.ID != "os_source_unmeasured_pending" {
			continue
		}
		if r.IntegrationVerdict != OSAbsorptionVerdictIntegrationActiveOrWrapper {
			t.Fatalf("expected pending row verdict=integration_active_or_wrapper, got %q", r.IntegrationVerdict)
		}
	}
}

// TestOSAbsorptionV02ConsumersAllRetiredIsComplete covers the "every
// consumer has a retired_at" branch of the verdict: a retired integration
// whose only remaining consumer is itself marked retired is complete. This
// is the path an operator follows when they actually shut a live service
// down — the meter must stop flagging the row the moment the consumer's
// retired_at is recorded.
func TestOSAbsorptionV02ConsumersAllRetiredIsComplete(t *testing.T) {
	row := OSAbsorptionMatrixRow{
		ID:                  "os_source_retired_consumer",
		SourceRef:           "/synthetic/retired-consumer",
		CompoundIntegration: osAbsorptionIntegrationRetired,
		LiveConsumers: []OSAbsorptionLiveConsumer{
			{Name: "hermes stdio watchdog", Surface: "gbrain serve", LastSeenEvidenceDate: "2026-08-22", RetiredAt: "2026-08-23"},
		},
	}
	if got := osAbsorptionRowVerdict(row); got != OSAbsorptionVerdictComplete {
		t.Fatalf("expected verdict=complete when every consumer is retired, got %q", got)
	}
}

// TestOSAbsorptionV02VerdictUnitTable is the table-driven micro-falsifier
// for osAbsorptionRowVerdict: every (compound_integration, hasLiveConsumer)
// pair returns exactly the verdict the meter promises. A regression here
// is the exact regression work_b28abf9a8718 was opened for.
func TestOSAbsorptionV02VerdictUnitTable(t *testing.T) {
	cases := []struct {
		name        string
		integration string
		live        bool
		want        string
	}{
		{"retired_no_live_is_complete", "retired", false, OSAbsorptionVerdictComplete},
		{"absorbed_no_live_is_complete", "absorbed", false, OSAbsorptionVerdictComplete},
		{"active_no_live_is_active_or_wrapper", "active", false, OSAbsorptionVerdictIntegrationActiveOrWrapper},
		{"wrapper_no_live_is_active_or_wrapper", "wrapper", false, OSAbsorptionVerdictIntegrationActiveOrWrapper},
		{"retired_live_is_false_retirement", "retired", true, OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers},
		{"absorbed_live_is_false_retirement", "absorbed", true, OSAbsorptionVerdictIntegrationAbsorbedWithLiveConsumers},
		{"active_live_is_active_or_wrapper", "active", true, OSAbsorptionVerdictIntegrationActiveOrWrapper},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := OSAbsorptionMatrixRow{CompoundIntegration: tc.integration}
			if tc.live {
				row.LiveConsumers = []OSAbsorptionLiveConsumer{{Name: "live", LastSeenEvidenceDate: "2026-08-23"}}
			}
			if got := osAbsorptionRowVerdict(row); got != tc.want {
				t.Fatalf("integration=%q live=%v: got %q, want %q", tc.integration, tc.live, got, tc.want)
			}
		})
	}
}

// TestOSAbsorptionV02AbsorptionCompleteCheckFailsOnFalseRetirement is the
// direct unit test for the doctor-level check: when a row carries
// retired+live consumers, the check returns status=fail with the row id
// named in the detail; when no such row exists, the check returns ok.
func TestOSAbsorptionV02AbsorptionCompleteCheckFailsOnFalseRetirement(t *testing.T) {
	bad := []OSAbsorptionMatrixRow{
		{ID: "os_source_a", CompoundIntegration: "retired", LiveConsumers: []OSAbsorptionLiveConsumer{{Name: "live"}}},
		{ID: "os_source_b", CompoundIntegration: "absorbed", LiveConsumers: []OSAbsorptionLiveConsumer{{Name: "live"}}},
		{ID: "os_source_c", CompoundIntegration: "retired"},
	}
	check := osAbsorptionAbsorptionCompleteCheck(bad)
	if check.Status != "fail" {
		t.Fatalf("expected fail with two false-retirement rows, got %+v", check)
	}
	if !strings.Contains(check.Detail, "os_source_a") || !strings.Contains(check.Detail, "os_source_b") {
		t.Fatalf("expected detail to name both bad rows, got %q", check.Detail)
	}
	if strings.Contains(check.Detail, "os_source_c") {
		t.Fatalf("expected detail to omit the clean retired row, got %q", check.Detail)
	}
	good := []OSAbsorptionMatrixRow{
		{ID: "os_source_d", CompoundIntegration: "retired"},
		{ID: "os_source_e", CompoundIntegration: "active", LiveConsumers: []OSAbsorptionLiveConsumer{{Name: "live"}}},
	}
	check = osAbsorptionAbsorptionCompleteCheck(good)
	if check.Status != "ok" {
		t.Fatalf("expected ok on no false-retirement rows, got %+v", check)
	}
}
