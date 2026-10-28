package authority

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/eventsource"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const (
	// OSAbsorptionMatrixSchema is the CURRENT schema id written by
	// ImportOSAbsorptionMatrix. v0.2 introduces the compound_integration /
	// live_consumers split (work_b28abf9a8718); v0.1 files still load and
	// migrate on read.
	OSAbsorptionMatrixSchema    = "compound.os_absorption_source_matrix.v0.2"
	OSAbsorptionMatrixSchemaV01 = "compound.os_absorption_source_matrix.v0.1"
	OSAbsorptionAuthorityRef    = "compound://registries/os-absorption-source-matrix"

	osAbsorptionMatrixFile = "os-absorption-source-matrix.json"

	// compound_integration enum values for the v0.2 row. They split the
	// old single-axis `status` into two independent facts: how compound
	// itself treats the source (this enum) and whether live services
	// outside compound still reach it (live_consumers). A row can carry
	// compound_integration=retired AND a non-empty live_consumers list;
	// that combination is exactly the "false retirement" the os-absorption
	// meter is supposed to surface (work_b28abf9a8718).
	osAbsorptionIntegrationRetired  = "retired"
	osAbsorptionIntegrationAbsorbed = "absorbed"
	osAbsorptionIntegrationActive   = "active"
	osAbsorptionIntegrationWrapper  = "wrapper"

	// Integration verdict values surfaced by the doctor on every matrix row.
	// "complete" means compound considers the source retired or absorbed
	// AND no live consumer is still reaching it (or every consumer has a
	// retired_at date). The other values explicitly name the failure
	// mode — especially integration_retired_with_live_consumers, which is
	// the os_source_gbrain false-retirement the meter must catch.
	// Exported so the CLI surface can render summary buckets.
	OSAbsorptionVerdictComplete                             = "complete"
	OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers  = "integration_retired_with_live_consumers"
	OSAbsorptionVerdictIntegrationAbsorbedWithLiveConsumers = "integration_absorbed_with_live_consumers"
	OSAbsorptionVerdictIntegrationActiveOrWrapper           = "integration_active_or_wrapper"

	// osAbsorptionDecisionsLedgerRef is the compound_owner ref used by the
	// os_source_decisions_repo absorption row. It is proven by the decisions
	// source-authority ledger (compound/internal/decisions), not asserted.
	osAbsorptionDecisionsLedgerRef = "compound://ledger/decisions"

	// proof_mode values a matrix row can set explicitly to opt into an
	// auto-proof mechanism regardless of its compound_owner string.
	osAbsorptionProofModeCapability    = "capability_source_authority"
	osAbsorptionProofModeDecisions     = "decisions_source_authority"
	osAbsorptionProofModeMemoryLedger  = "memory_ledger_authority"
	osAbsorptionProofModeOperatorState = "operator_state_registry"
	osAbsorptionProofModeWorkPacket    = "work_packet_authority"
	osAbsorptionProofModeOSCapability  = "os_capability_wrapper_authority"
	osAbsorptionProofModeServedOrient  = "served_orientation_authority"
	// osAbsorptionProofModeOSSkills and osAbsorptionProofModeOSStatus prove the
	// os_source_os_skills_cli and os_source_os_status_cli wrapper rows. Unlike
	// os_capability_wrapper_authority (a pure zero-native-logic proxy), `os
	// skills` and `os status` are dashboards that blend genuine live compound
	// authority reads with retained-native operational probes (skill corpus
	// registries; daemon/launchd, switchback, ledger, capacity telemetry).
	// These proof modes verify ONLY the compound-sourced slice is real
	// (digest-anchored entrypoint, no fabrication before the genuine
	// compoundCommand()+execFileSync/spawnSync call, and a live parity probe
	// cross-checked against a direct `compound` invocation); the retained
	// native reads are not required to disappear, only to stay honestly named
	// in the matrix row.
	osAbsorptionProofModeOSSkills = "os_skills_wrapper_authority"
	osAbsorptionProofModeOSStatus = "os_status_wrapper_authority"
	// osAbsorptionProofModeInfraRegistry proves an infra-os operational
	// registry (database.yaml/services.yaml) row: installed registry file
	// exists, its recorded source hash matches the live YAML's current
	// sha256, and the row's evidence_refs resolve to that ledger record. A
	// stale hash (the live source changed since import) proves as "pending"
	// drift, never a silent "ok" or an opaque "failed".
	osAbsorptionProofModeInfraRegistry = "infra_registry_authority"
	// osAbsorptionProofModeRegistryOwnership proves ownership (not health) of
	// an already-live compound_primary registry: the installed registry file
	// exists under $COMPOUND_HOME/registries/ and its corresponding Load*
	// authority function parses it without error. It deliberately does not
	// evaluate the loaded content's own status field (e.g. an
	// operator-approval-frontier row can be proof_status=ok while its
	// projected Status says "operator_approval_frontier_failed" — ownership
	// and health are different axes).
	osAbsorptionProofModeRegistryOwnership = "registry_ownership_authority"
	// osAbsorptionProofModeEventSource proves an events_dir row (an absorbed
	// operating system's events/ directory): every live .ndjson file under
	// source_ref (README.md and any non-.ndjson file excluded) has at least one
	// compound/internal/eventsource source-authority record, the highest
	// historical Imported+Skipped total ever recorded for that file covers its
	// CURRENT non-blank line count (a file that grew since the last `event
	// import-os` run proves "pending" drift naming the file, never a silent
	// "ok"), and a sample of TraceIDs from the most recent record that actually
	// imported new traces resolves to a real TraceEnvelope file under
	// COMPOUND_HOME/events/ (a missing trace file fails the row closed).
	osAbsorptionProofModeEventSource = "event_source_authority"

	osAbsorptionWorkAuthorityRef              = "compound://work"
	osAbsorptionEffectiveContractAuthorityRef = "compound://contracts/effective-contract"
	osAbsorptionPacketEvidenceRef             = "packet_sha256:"
	osAbsorptionServedEntrypointEvidenceRef   = "served_entrypoint:"
	osAbsorptionContextPackEvidenceRef        = "context_pack:"
)

type OSAbsorptionSourceMatrix struct {
	SchemaVersion        string                  `json:"schema_version"`
	GeneratedAt          string                  `json:"generated_at"`
	AuthorityRef         string                  `json:"authority_ref"`
	SourceMatrixPath     string                  `json:"source_matrix_path,omitempty"`
	SourceMatrixRevision string                  `json:"source_matrix_revision,omitempty"`
	Rows                 []OSAbsorptionMatrixRow `json:"rows"`
}

type OSAbsorptionMatrixRow struct {
	ID             string   `json:"id"`
	SourceRef      string   `json:"source_ref"`
	SourceKind     string   `json:"source_kind,omitempty"`
	SourceRevision string   `json:"source_revision,omitempty"`
	OldOwner       string   `json:"old_owner"`
	OldRole        string   `json:"old_role"`
	CurrentRole    string   `json:"current_role"`
	Privacy        string   `json:"privacy"`
	CompoundOwner  string   `json:"compound_owner"`
	Consumer       string   `json:"consumer"`
	Proof          string   `json:"proof"`
	ProofMode      string   `json:"proof_mode,omitempty"`
	RetirementGate string   `json:"retirement_gate"`
	Status         string   `json:"status"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`

	// CompoundIntegration is compound's own engine-profile/caller status
	// for this source — independent of whether live services outside
	// compound still reach it (see LiveConsumers). Introduced in
	// schema v0.2 (work_b28abf9a8718); on a v0.1 row the normalizer
	// migrates this from Status (retired/evidence_only=retired|absorbed,
	// pending=active, wrapper=wrapper) so existing files keep loading
	// without the meter silently swallowing the false-retirement case.
	CompoundIntegration string `json:"compound_integration,omitempty"`

	// LiveConsumers enumerates services that still reach the source, each
	// backed by an evidence date (LastSeenEvidenceDate). A consumer with a
	// non-empty RetiredAt is considered retired; the meter treats the row
	// as complete only when every remaining consumer has one. Empty list
	// means "no live consumers reported" (the default for unmeasured rows
	// — see os_source_brain_solo_builder_doctrine and os_source_umut_events
	// in the live matrix).
	LiveConsumers []OSAbsorptionLiveConsumer `json:"live_consumers,omitempty"`
}

// OSAbsorptionLiveConsumer is one entry in a v0.2 row's live_consumers
// array. Surface and the evidence date are the two load-bearing fields the
// meter uses; RetiredAt turns the entry into "live but retired" once an
// operator has actually shut the consumer down; Notes carries operator
// context that does not fit anywhere else.
type OSAbsorptionLiveConsumer struct {
	Name                 string `json:"name"`
	Surface              string `json:"surface,omitempty"`
	LastSeenEvidenceDate string `json:"last_seen_evidence_date,omitempty"`
	RetiredAt            string `json:"retired_at,omitempty"`
	Notes                string `json:"notes,omitempty"`
}

type OSAbsorptionImportReport struct {
	AuthorityRef         string `json:"authority_ref"`
	AuthorityPath        string `json:"authority_path"`
	SourceMatrixPath     string `json:"source_matrix_path"`
	SourceMatrixRevision string `json:"source_matrix_revision"`
	RowCount             int    `json:"row_count"`
	AbsorbedCount        int    `json:"absorbed_count"`
	PendingCount         int    `json:"pending_count"`
}

type OSAbsorptionDoctorReport struct {
	SchemaVersion        string `json:"schema_version"`
	AuthorityRef         string `json:"authority_ref"`
	AuthorityPath        string `json:"authority_path"`
	Status               string `json:"status"`
	Installed            bool   `json:"installed"`
	RowCount             int    `json:"row_count"`
	AbsorbedCount        int    `json:"absorbed_count"`
	PendingCount         int    `json:"pending_count"`
	AutoProofCount       int    `json:"auto_proof_count"`
	PendingWithGateCount int    `json:"pending_with_gate_count"`
	MissingRequiredCount int    `json:"missing_required_count"`
	FailedAutoProofCount int    `json:"failed_auto_proof_count"`
	// MachineProvenCount/AssertedCount/FailedProofCount split the misleading
	// single AbsorbedCount bucket (status-string based) into what proof_status
	// actually says (evidence based): a row can carry status=absorbed and
	// still be proof_status=manual. PendingCount is unchanged/existing.
	MachineProvenCount int `json:"machine_proven_count"`
	AssertedCount      int `json:"asserted_count"`
	FailedProofCount   int `json:"failed_proof_count"`
	// SkippedProofCount counts rows whose auto-proof declined to run because
	// its declared dependency (e.g. an absorbed-OS root) is not configured on
	// this machine — the public-clone case. It is separate from
	// AutoProofCount/FailedAutoProofCount: a skip is neither a pass nor a
	// failure, so it must never move either of those counters.
	SkippedProofCount int `json:"skipped_proof_count"`

	Checks                 []OSAbsorptionDoctorCheck        `json:"checks"`
	Rows                   []OSAbsorptionDoctorRow          `json:"rows,omitempty"`
	SourceAuthorityRecords []capgraph.SourceAuthorityRecord `json:"-"`
	RootDependencies       OSRootDependencyReport           `json:"root_dependencies"`
}

type OSAbsorptionDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type OSAbsorptionDoctorRow struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	SourceRef     string   `json:"source_ref"`
	CompoundOwner string   `json:"compound_owner"`
	ProofStatus   string   `json:"proof_status"`
	ProofRef      string   `json:"proof_ref,omitempty"`
	ProofDetail   string   `json:"proof_detail,omitempty"`
	MissingFields []string `json:"missing_fields,omitempty"`

	// v0.2 row metrics: surfaced so a doctor run that hits
	// integration_retired_with_live_consumers cannot stay hidden inside a
	// status-string-only report. Both fields mirror the corresponding
	// matrix row fields after normalize-time migration.
	CompoundIntegration string                     `json:"compound_integration,omitempty"`
	LiveConsumers       []OSAbsorptionLiveConsumer `json:"live_consumers,omitempty"`
	// IntegrationVerdict names the meter's per-row conclusion
	// (see the osAbsorptionVerdict* constants). "complete" only when
	// compound_integration is retired/absorbed AND live_consumers is empty
	// or fully retired.
	IntegrationVerdict string `json:"integration_verdict,omitempty"`
}

func ImportOSAbsorptionMatrix(homePath, sourcePath string, now time.Time) (OSAbsorptionImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OSAbsorptionImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return OSAbsorptionImportReport{}, fmt.Errorf("os absorption matrix source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return OSAbsorptionImportReport{}, fmt.Errorf("read os absorption matrix %s: %w", sourcePath, err)
	}
	matrix, err := parseOSAbsorptionMatrix(data)
	if err != nil {
		return OSAbsorptionImportReport{}, fmt.Errorf("parse os absorption matrix %s: %w", sourcePath, err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	matrix.GeneratedAt = firstNonEmpty(matrix.GeneratedAt, now.UTC().Format(time.RFC3339))
	matrix.AuthorityRef = firstNonEmpty(matrix.AuthorityRef, OSAbsorptionAuthorityRef)
	matrix.SourceMatrixPath = sourcePath
	matrix.SourceMatrixRevision = "sha256:" + digestBytes(data)
	normalizeOSAbsorptionRows(&matrix)
	if err := validateOSAbsorptionMatrix(matrix); err != nil {
		return OSAbsorptionImportReport{}, err
	}
	path := OSAbsorptionMatrixPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return OSAbsorptionImportReport{}, err
	}
	body, err := json.MarshalIndent(matrix, "", "  ")
	if err != nil {
		return OSAbsorptionImportReport{}, err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return OSAbsorptionImportReport{}, err
	}
	absorbed, pending := osAbsorptionCounts(matrix.Rows)
	return OSAbsorptionImportReport{
		AuthorityRef:         matrix.AuthorityRef,
		AuthorityPath:        path,
		SourceMatrixPath:     matrix.SourceMatrixPath,
		SourceMatrixRevision: matrix.SourceMatrixRevision,
		RowCount:             len(matrix.Rows),
		AbsorbedCount:        absorbed,
		PendingCount:         pending,
	}, nil
}

func LoadOSAbsorptionMatrix(homePath string) (OSAbsorptionSourceMatrix, error) {
	data, err := os.ReadFile(OSAbsorptionMatrixPath(homePath))
	if err != nil {
		return OSAbsorptionSourceMatrix{}, err
	}
	matrix, err := parseOSAbsorptionMatrix(data)
	if err != nil {
		return OSAbsorptionSourceMatrix{}, err
	}
	normalizeOSAbsorptionRows(&matrix)
	return matrix, nil
}

func OSAbsorptionDoctor(homePath string) (OSAbsorptionDoctorReport, error) {
	report := OSAbsorptionDoctorReport{
		SchemaVersion: OSAbsorptionMatrixSchema,
		AuthorityRef:  OSAbsorptionAuthorityRef,
		AuthorityPath: OSAbsorptionMatrixPath(homePath),
		Status:        "fail",
	}
	matrix, err := LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			report.Checks = append(report.Checks, OSAbsorptionDoctorCheck{Name: "installed", Status: "fail", Detail: "matrix not imported"})
			return report, nil
		}
		return report, err
	}
	report.Installed = true
	report.AuthorityRef = firstNonEmpty(matrix.AuthorityRef, OSAbsorptionAuthorityRef)
	absorbed, pending := osAbsorptionCounts(matrix.Rows)
	report.RowCount = len(matrix.Rows)
	report.AbsorbedCount = absorbed
	report.PendingCount = pending
	capRecords, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		return report, err
	}
	servedSourceRowsCheck, err := osAbsorptionServedOrientationSourceRowsCheck(homePath, matrix.Rows)
	if err != nil {
		return report, err
	}
	rootDependencies, err := inspectOSRootDependencies(homePath)
	if err != nil {
		return report, err
	}
	report.RootDependencies = rootDependencies
	for _, row := range matrix.Rows {
		dr := OSAbsorptionDoctorRow{
			ID:                  row.ID,
			Status:              row.Status,
			SourceRef:           row.SourceRef,
			CompoundOwner:       row.CompoundOwner,
			ProofStatus:         "manual",
			CompoundIntegration: row.CompoundIntegration,
			LiveConsumers:       row.LiveConsumers,
			IntegrationVerdict:  osAbsorptionRowVerdict(row),
		}
		dr.MissingFields = missingOSAbsorptionFields(row)
		if len(dr.MissingFields) > 0 {
			report.MissingRequiredCount++
			dr.ProofStatus = "missing_required"
		}
		if row.Status == "pending" && row.CompoundOwner != "" && row.Consumer != "" && row.RetirementGate != "" {
			report.PendingWithGateCount++
		}
		servedDeclarationErr := validateOSAbsorptionServedOrientationDeclaration(row)
		switch {
		case servedDeclarationErr != nil:
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionProofResult{reason: servedDeclarationErr.Error()})
		case osAbsorptionNeedsWorkPacketProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionWorkPacketProof(homePath, row))
		case osAbsorptionNeedsOSCapabilityProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionOSCapabilityProof(homePath, row))
		case osAbsorptionNeedsOSSkillsProof(row):
			result, drift := osAbsorptionOSSkillsProof(homePath, row)
			if drift {
				dr.ProofStatus = "pending"
				dr.ProofDetail = result.reason
			} else {
				applyOSAbsorptionAutoProof(&report, &dr, result)
			}
		case osAbsorptionNeedsOSStatusProof(row):
			result, drift := osAbsorptionOSStatusProof(homePath, row)
			if drift {
				dr.ProofStatus = "pending"
				dr.ProofDetail = result.reason
			} else {
				applyOSAbsorptionAutoProof(&report, &dr, result)
			}
		case osAbsorptionNeedsCapabilityProof(row):
			result, drift := capabilityAuthorityProof(row, capRecords)
			if drift {
				dr.ProofStatus = "pending"
				dr.ProofDetail = result.reason
			} else {
				applyOSAbsorptionAutoProof(&report, &dr, result)
			}
		case osAbsorptionNeedsDecisionsProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionDecisionsProof(homePath))
		case osAbsorptionNeedsOperatorStateProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionOperatorStateProof(homePath))
		case osAbsorptionNeedsServedOrientationProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, osAbsorptionServedOrientationProof(homePath, row))
		case osAbsorptionNeedsMemoryLedgerProof(row):
			if row.ProofMode == osAbsorptionProofModeMemoryLedger {
				applyOSAbsorptionAutoProof(&report, &dr, memoryLedgerAuthorityProof(homePath, row))
			} else if rec, ok := memoryLedgerProofRecord(homePath, row); ok {
				dr.ProofStatus = "ok"
				dr.ProofRef = rec
			} else {
				dr.ProofStatus = "pending"
			}
		case osAbsorptionNeedsInfraRegistryProof(row):
			result, drift := infraRegistryProof(homePath, row)
			if drift {
				dr.ProofStatus = "pending"
				dr.ProofDetail = result.reason
			} else {
				applyOSAbsorptionAutoProof(&report, &dr, result)
			}
		case osAbsorptionNeedsEventSourceProof(row):
			result, drift := eventSourceAuthorityProof(homePath, row)
			if drift {
				dr.ProofStatus = "pending"
				dr.ProofDetail = result.reason
			} else {
				applyOSAbsorptionAutoProof(&report, &dr, result)
			}
		case osAbsorptionNeedsRegistryOwnershipProof(row):
			applyOSAbsorptionAutoProof(&report, &dr, registryOwnershipProof(homePath, row))
		case row.Status == "pending":
			dr.ProofStatus = "pending"
		}
		switch dr.ProofStatus {
		case "ok":
			report.MachineProvenCount++
		case "manual":
			report.AssertedCount++
		case "failed":
			report.FailedProofCount++
		case "skipped":
			// SkippedProofCount is already incremented by
			// applyOSAbsorptionAutoProof at the moment the skip happens;
			// nothing to add here beyond leaving it out of proven/asserted/
			// failed.
		}
		report.Rows = append(report.Rows, dr)
	}
	report.Checks = append(report.Checks,
		doctorCheck("installed", report.Installed, fmt.Sprintf("%s rows=%d", report.AuthorityPath, report.RowCount)),
		doctorCheck("required_fields", report.MissingRequiredCount == 0, fmt.Sprintf("missing=%d", report.MissingRequiredCount)),
		servedSourceRowsCheck,
		doctorCheck("capability_auto_proofs", report.AutoProofCount >= 3 && report.FailedAutoProofCount == 0, fmt.Sprintf("auto=%d failed=%d", report.AutoProofCount, report.FailedAutoProofCount)),
		doctorCheck(
			"pending_retirement_gates",
			report.PendingWithGateCount == 0,
			fmt.Sprintf("pending_with_gate=%d", report.PendingWithGateCount),
		),
		doctorCheck(
			"active_root_dependencies",
			rootDependencies.Status == "ok" || rootDependencies.Status == "not_configured",
			fmt.Sprintf(
				"status=%s; %s; decisions: %s; custody: %s",
				rootDependencies.Status,
				rootDependencyDetail(rootDependencies),
				decisionCoverageDetail(rootDependencies.DecisionCoverage),
				custodyDetail(rootDependencies.Custody),
			),
		),
		// v0.2 meter check: any row whose compound_integration claims
		// retired or absorbed while live_consumers still names live services
		// (or vice-versa: absorbed but live services still reach the source)
		// fails this check with the row IDs named in detail. This is the
		// false-retirement gate the meter must catch — see
		// work_b28abf9a8718.
		osAbsorptionAbsorptionCompleteCheck(matrix.Rows),
	)
	report.Status = "ok"
	for _, check := range report.Checks {
		if check.Status != "ok" {
			report.Status = "fail"
			break
		}
	}
	return report, nil
}

func OSAbsorptionMatrixPath(homePath string) string {
	return filepath.Join(homePath, "registries", osAbsorptionMatrixFile)
}

func parseOSAbsorptionMatrix(data []byte) (OSAbsorptionSourceMatrix, error) {
	var matrix OSAbsorptionSourceMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		return matrix, err
	}
	if strings.TrimSpace(matrix.SchemaVersion) == "" {
		matrix.SchemaVersion = OSAbsorptionMatrixSchema
	}
	// Accept the current v0.2 schema and the legacy v0.1 schema. v0.1 rows
	// without compound_integration / live_consumers are migrated in
	// normalizeOSAbsorptionRows; any other version is rejected explicitly
	// so a stale or wrong-shape file fails closed instead of being read
	// through with a guessed schema.
	switch matrix.SchemaVersion {
	case OSAbsorptionMatrixSchema, OSAbsorptionMatrixSchemaV01:
	default:
		return matrix, fmt.Errorf("unsupported schema_version %q", matrix.SchemaVersion)
	}
	return matrix, nil
}

func normalizeOSAbsorptionRows(matrix *OSAbsorptionSourceMatrix) {
	matrix.AuthorityRef = firstNonEmpty(matrix.AuthorityRef, OSAbsorptionAuthorityRef)
	for i := range matrix.Rows {
		row := &matrix.Rows[i]
		row.ID = strings.TrimSpace(row.ID)
		row.SourceRef = strings.TrimSpace(row.SourceRef)
		row.SourceKind = strings.TrimSpace(row.SourceKind)
		row.SourceRevision = strings.TrimSpace(row.SourceRevision)
		row.OldOwner = strings.TrimSpace(row.OldOwner)
		row.OldRole = strings.TrimSpace(row.OldRole)
		row.CurrentRole = strings.TrimSpace(row.CurrentRole)
		row.Privacy = firstNonEmpty(row.Privacy, "private")
		row.CompoundOwner = strings.TrimSpace(row.CompoundOwner)
		row.Consumer = strings.TrimSpace(row.Consumer)
		row.Proof = strings.TrimSpace(row.Proof)
		row.ProofMode = strings.TrimSpace(row.ProofMode)
		row.RetirementGate = strings.TrimSpace(row.RetirementGate)
		row.Status = firstNonEmpty(row.Status, "pending")
		row.EvidenceRefs = cleanStrings(row.EvidenceRefs)

		// v0.1 → v0.2 row migration. A row that does not declare
		// compound_integration is migrated from Status; this is the only
		// place that decides the meter's verdict for legacy files. A
		// freshly-declared compound_integration is left as-is — that is
		// the whole point of the split: it overrides a misleading Status.
		row.CompoundIntegration = strings.TrimSpace(row.CompoundIntegration)
		if row.CompoundIntegration == "" {
			row.CompoundIntegration = osAbsorptionIntegrationFromStatus(row.Status)
		}
		row.CompoundIntegration = osAbsorptionNormalizeIntegration(row.CompoundIntegration)

		row.LiveConsumers = normalizeOSAbsorptionLiveConsumers(row.LiveConsumers)
	}
	// Bump legacy v0.1 files to v0.2 in memory so re-import persists the
	// migrated shape — without this the matrix round-trips forever as
	// v0.1 and the meter would never surface in the file header.
	if matrix.SchemaVersion == OSAbsorptionMatrixSchemaV01 {
		matrix.SchemaVersion = OSAbsorptionMatrixSchema
	}
	sort.SliceStable(matrix.Rows, func(i, j int) bool { return matrix.Rows[i].ID < matrix.Rows[j].ID })
}

// osAbsorptionIntegrationFromStatus maps the legacy v0.1 Status enum onto
// the v0.2 compound_integration enum. The mapping is the v0.1
// status-string meaning as documented for the source matrix before the
// split: retired stays retired; absorbed / evidence_only both name a source
// compound has finished absorbing (evidence_only is functionally absorbed
// from compound's POV — see osAbsorptionCounts); wrapper stays wrapper;
// pending becomes active (work is in progress, source is still a live
// candidate). This is a one-way migration; an explicit compound_integration
// on the row is never overwritten.
func osAbsorptionIntegrationFromStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "retired":
		return osAbsorptionIntegrationRetired
	case "absorbed", "evidence_only":
		return osAbsorptionIntegrationAbsorbed
	case "wrapper":
		return osAbsorptionIntegrationWrapper
	case "pending":
		return osAbsorptionIntegrationActive
	default:
		return osAbsorptionIntegrationActive
	}
}

// osAbsorptionNormalizeIntegration trims whitespace and rejects unknown
// integration values so the meter never silently passes on a typo'd enum.
// Unknown values fall back to the conservative reading: active (i.e. still
// a live candidate), so the row cannot be mistaken for retired/absorbed.
func osAbsorptionNormalizeIntegration(value string) string {
	switch strings.TrimSpace(value) {
	case osAbsorptionIntegrationRetired, osAbsorptionIntegrationAbsorbed, osAbsorptionIntegrationActive, osAbsorptionIntegrationWrapper:
		return strings.TrimSpace(value)
	default:
		return osAbsorptionIntegrationActive
	}
}

// normalizeOSAbsorptionLiveConsumers trims each entry, drops entries with
// no Name (an unnamed consumer is unverifiable), and preserves order. A
// non-empty RetiredAt turns the entry into "live-but-retired" for the
// verdict function; absent RetiredAt the consumer is treated as still
// live (work_b28abf9a8718).
func normalizeOSAbsorptionLiveConsumers(consumers []OSAbsorptionLiveConsumer) []OSAbsorptionLiveConsumer {
	if len(consumers) == 0 {
		return nil
	}
	out := make([]OSAbsorptionLiveConsumer, 0, len(consumers))
	for _, c := range consumers {
		c.Name = strings.TrimSpace(c.Name)
		c.Surface = strings.TrimSpace(c.Surface)
		c.LastSeenEvidenceDate = strings.TrimSpace(c.LastSeenEvidenceDate)
		c.RetiredAt = strings.TrimSpace(c.RetiredAt)
		c.Notes = strings.TrimSpace(c.Notes)
		if c.Name == "" {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// osAbsorptionRowVerdict is the meter's per-row conclusion (see the
// osAbsorptionVerdict* constants). The packet's hard requirement
// (work_b28abf9a8718): a row with compound_integration=retired AND a
// non-empty live_consumers list MUST NOT be reported as complete — it is
// exactly the false-retirement case the os_source_gbrain row exposed.
func osAbsorptionRowVerdict(row OSAbsorptionMatrixRow) string {
	hasLiveUnretired := false
	for _, c := range row.LiveConsumers {
		if c.RetiredAt == "" {
			hasLiveUnretired = true
			break
		}
	}
	if !hasLiveUnretired {
		if row.CompoundIntegration == osAbsorptionIntegrationRetired ||
			row.CompoundIntegration == osAbsorptionIntegrationAbsorbed {
			return OSAbsorptionVerdictComplete
		}
		return OSAbsorptionVerdictIntegrationActiveOrWrapper
	}
	switch row.CompoundIntegration {
	case osAbsorptionIntegrationRetired:
		return OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers
	case osAbsorptionIntegrationAbsorbed:
		return OSAbsorptionVerdictIntegrationAbsorbedWithLiveConsumers
	default:
		return OSAbsorptionVerdictIntegrationActiveOrWrapper
	}
}

func validateOSAbsorptionMatrix(matrix OSAbsorptionSourceMatrix) error {
	if len(matrix.Rows) == 0 {
		return fmt.Errorf("os absorption matrix requires rows")
	}
	seen := map[string]bool{}
	for _, row := range matrix.Rows {
		if row.ID == "" {
			return fmt.Errorf("os absorption matrix row missing id")
		}
		if seen[row.ID] {
			return fmt.Errorf("duplicate os absorption row id %q", row.ID)
		}
		seen[row.ID] = true
		if err := validateOSAbsorptionServedOrientationDeclaration(row); err != nil {
			return err
		}
	}
	return nil
}

func osAbsorptionCounts(rows []OSAbsorptionMatrixRow) (absorbed int, pending int) {
	for _, row := range rows {
		switch row.Status {
		case "absorbed", "wrapper", "evidence_only", "retired":
			absorbed++
		case "pending":
			pending++
		}
	}
	return absorbed, pending
}

func capabilityAuthorityProofRecord(row OSAbsorptionMatrixRow, records []capgraph.SourceAuthorityRecord) (capgraph.SourceAuthorityRecord, bool) {
	for _, rec := range records {
		if rec.AuthorityRef != capgraph.CapabilityGraphAuthorityRef {
			continue
		}
		if strings.TrimSpace(rec.SourcePath) != row.SourceRef {
			continue
		}
		if row.SourceRevision != "" && strings.TrimSpace(rec.SourceRevision) != row.SourceRevision {
			continue
		}
		return rec, true
	}
	return capgraph.SourceAuthorityRecord{}, false
}

// capabilityAuthorityProof dispatches capability_source_authority proof for
// one row. A row with no absorb_subset_file evidence_refs takes the legacy,
// unchanged whole-directory path (exact (SourcePath, SourceRevision) string
// equality against a capsrc record) so already-green rows like the recipes
// row (os_source_brain_solo_builder_os) are unaffected. A row that declares
// an enumerated absorb-subset (e.g. os_source_brain_solo_builder_doctrine,
// which absorbed only 2 of 36 files in its source_ref tree) instead takes the
// subset-scoped path: only the named files are ever read or hashed, so
// out-of-row churn elsewhere in the same directory can never flip the row.
// The bool return is true only for subset drift (pending), mirroring
// infraRegistryProof's (result, drift) shape.
func capabilityAuthorityProof(row OSAbsorptionMatrixRow, records []capgraph.SourceAuthorityRecord) (osAbsorptionProofResult, bool) {
	subset := osAbsorptionCapabilitySubsetFiles(row.EvidenceRefs)
	if len(subset) == 0 {
		if rec, ok := capabilityAuthorityProofRecord(row, records); ok {
			return osAbsorptionProofResult{ok: true, proofRef: rec.ID}, false
		}
		return osAbsorptionProofResult{reason: "no matching capability source-authority record for " + row.SourceRef}, false
	}
	return capabilityAuthoritySubsetProof(row, records, subset)
}

// osAbsorptionCapabilitySubsetFiles parses "absorb_subset_file:<relative
// path>:sha256:<hash>" entries out of a ref list into relative-path -> sha256
// hex digest. A row (or capsrc record) with no such entries declares no
// subset, which is exactly the legacy whole-directory signal.
func osAbsorptionCapabilitySubsetFiles(refs []string) map[string]string {
	const prefix = "absorb_subset_file:"
	const hashMarker = ":sha256:"
	subset := map[string]string{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		rest := strings.TrimPrefix(ref, prefix)
		idx := strings.LastIndex(rest, hashMarker)
		if idx < 0 {
			continue
		}
		file := strings.TrimSpace(rest[:idx])
		hash := strings.TrimSpace(rest[idx+len(hashMarker):])
		if file == "" || hash == "" {
			continue
		}
		subset[file] = hash
	}
	return subset
}

// capabilityAuthoritySubsetProof re-hashes, on every doctor run, exactly the
// files the row's absorb-subset names (never the rest of source_ref) and
// requires a capsrc record whose own absorb_subset_file evidence matches that
// exact declared subset — tying the row to the specific import that absorbed
// it. A subset file whose live sha256 no longer matches the absorbed hash is
// reported as pending drift naming the file (never a silent ok); files
// outside the subset are never read, so evidence-only/out-of-row churn (e.g.
// PORTFOLIO-MAP.md next to ETHOS.md) can never affect the row.
func capabilityAuthoritySubsetProof(row OSAbsorptionMatrixRow, records []capgraph.SourceAuthorityRecord, subset map[string]string) (osAbsorptionProofResult, bool) {
	sourceDir := strings.TrimSpace(row.SourceRef)
	if sourceDir == "" {
		return osAbsorptionProofResult{reason: "capability source authority subset: row missing source_ref"}, false
	}

	var rec *capgraph.SourceAuthorityRecord
	for i := range records {
		candidate := records[i]
		if candidate.AuthorityRef != capgraph.CapabilityGraphAuthorityRef {
			continue
		}
		if strings.TrimSpace(candidate.SourcePath) != sourceDir {
			continue
		}
		if capabilitySubsetsEqual(osAbsorptionCapabilitySubsetFiles(candidate.EvidenceRefs), subset) {
			rec = &records[i]
			break
		}
	}
	if rec == nil {
		return osAbsorptionProofResult{reason: "no capability source-authority record corroborates the declared absorb-subset for " + sourceDir}, false
	}

	files := make([]string, 0, len(subset))
	for file := range subset {
		files = append(files, file)
	}
	sort.Strings(files)

	for _, file := range files {
		wantHash := subset[file]
		livePath := filepath.Join(sourceDir, file)
		body, err := os.ReadFile(livePath)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("capability source authority subset file %s: %v", livePath, err)}, false
		}
		liveHash := digestBytes(body)
		if liveHash != wantHash {
			return osAbsorptionProofResult{reason: fmt.Sprintf("capability source authority subset file %s has drifted since import (live sha256:%s, absorbed sha256:%s); re-run capability import-skills to refresh", livePath, liveHash, wantHash)}, true
		}
	}

	return osAbsorptionProofResult{
		ok:       true,
		proofRef: fmt.Sprintf("%s;absorb_subset=%s", rec.ID, strings.Join(files, ",")),
	}, false
}

func capabilitySubsetsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for file, hash := range a {
		if b[file] != hash {
			return false
		}
	}
	return true
}

func missingOSAbsorptionFields(row OSAbsorptionMatrixRow) []string {
	var missing []string
	required := map[string]string{
		"id":              row.ID,
		"source_ref":      row.SourceRef,
		"old_owner":       row.OldOwner,
		"old_role":        row.OldRole,
		"current_role":    row.CurrentRole,
		"privacy":         row.Privacy,
		"compound_owner":  row.CompoundOwner,
		"consumer":        row.Consumer,
		"proof":           row.Proof,
		"retirement_gate": row.RetirementGate,
		"status":          row.Status,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	sort.Strings(missing)
	return missing
}

func osAbsorptionNeedsCapabilityProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeCapability {
		return true
	}
	return row.CompoundOwner == capgraph.CapabilityGraphAuthorityRef && row.Status == "absorbed"
}

func osAbsorptionNeedsMemoryLedgerProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode != "" {
		return row.ProofMode == osAbsorptionProofModeMemoryLedger
	}
	return row.CompoundOwner == "compound://ledger/memory"
}

func memoryLedgerProofRecord(homePath string, row OSAbsorptionMatrixRow) (proofRef string, ok bool) {
	store, err := mem.OpenReadOnly(homePath)
	if err != nil {
		return "", false
	}
	claims, err := store.LedgerClaims()
	if err != nil {
		return "", false
	}
	evidence, err := store.Evidence()
	if err != nil {
		return "", false
	}

	rowRefs := make(map[string]bool, len(row.EvidenceRefs))
	for _, ref := range row.EvidenceRefs {
		if ref = memoryLedgerCanonicalRef(ref); ref != "" {
			rowRefs[ref] = true
		}
	}
	evidenceByID := make(map[string]mem.EvidenceItem, len(evidence))
	for _, item := range evidence {
		evidenceByID[item.ID] = item
	}

	for _, claim := range claims {
		if strings.TrimSpace(claim.PromotionBasis) == "" {
			continue
		}
		if evidenceRef, matched := memoryLedgerClaimSourceRef(row, rowRefs, evidenceByID, claim); matched {
			return fmt.Sprintf("%s:claim=%s;evidence=%s", osAbsorptionProofModeMemoryLedger, claim.ID, evidenceRef), true
		}
	}
	return "", false
}

func memoryLedgerClaimSourceRef(row OSAbsorptionMatrixRow, rowRefs map[string]bool, evidenceByID map[string]mem.EvidenceItem, claim mem.Claim) (string, bool) {
	proofEvidence, hasProofEvidence := memoryLedgerClaimEvidenceRef(claim)
	for _, ref := range claim.EvidenceRefs {
		canonical := memoryLedgerCanonicalRef(ref)
		if canonical != "" && rowRefs[canonical] {
			if strings.HasPrefix(canonical, "evidence:") {
				return memoryLedgerProofEvidenceRef(ref), true
			}
			if hasProofEvidence {
				return proofEvidence, true
			}
		}
		if strings.TrimSpace(ref) == row.SourceRef && hasProofEvidence {
			return proofEvidence, true
		}
		item, exists := evidenceByID[strings.TrimPrefix(canonical, "evidence:")]
		if !exists {
			continue
		}
		if rowRefs[memoryLedgerCanonicalRef(item.SourceID)] || strings.TrimSpace(item.SourceID) == row.SourceRef {
			return item.ID, true
		}
	}
	if pageID := strings.TrimSpace(claim.Qualifiers["page_id"]); hasProofEvidence && pageID != "" && rowRefs[memoryLedgerCanonicalRef("knowledge_page:"+pageID)] {
		return proofEvidence, true
	}
	for _, key := range []string{"source_path", "source_ref"} {
		if sourceRef := strings.TrimSpace(claim.Qualifiers[key]); hasProofEvidence && sourceRef != "" && sourceRef == row.SourceRef {
			return proofEvidence, true
		}
	}
	return "", false
}

func memoryLedgerCanonicalRef(ref string) string {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "evidence:"):
		return "evidence:" + strings.TrimSpace(strings.TrimPrefix(ref, "evidence:"))
	case strings.HasPrefix(ref, "knowledge_page:"):
		return "knowledge_page:" + strings.TrimSpace(strings.TrimPrefix(ref, "knowledge_page:"))
	case strings.HasPrefix(ref, "ev_"):
		return "evidence:" + ref
	case strings.HasPrefix(ref, "knowledge_"):
		return "knowledge_page:" + ref
	default:
		return ref
	}
}

func memoryLedgerProofEvidenceRef(ref string) string {
	return strings.TrimPrefix(memoryLedgerCanonicalRef(ref), "evidence:")
}

func memoryLedgerClaimEvidenceRef(claim mem.Claim) (string, bool) {
	for _, ref := range claim.EvidenceRefs {
		if canonical := memoryLedgerCanonicalRef(ref); strings.HasPrefix(canonical, "evidence:") {
			return strings.TrimPrefix(canonical, "evidence:"), true
		}
	}
	return "", false
}

func memoryLedgerAuthorityProof(homePath string, row OSAbsorptionMatrixRow) osAbsorptionProofResult {
	contextPackID, err := memoryLedgerContextPackEvidenceID(row.EvidenceRefs)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	pack, err := readOSAbsorptionContextPack(homePath, contextPackID)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	if pack.Authority.SourceClass != "runtime" || pack.Authority.Status != "current" {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s is not runtime/current authority", pack.ID)}
	}
	if pack.Privacy != row.Privacy {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s privacy %q differs from row privacy %q", pack.ID, pack.Privacy, row.Privacy)}
	}
	if strings.TrimSpace(pack.TaskContract.Intent) == "" {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing task intent", pack.ID)}
	}
	contractID := strings.TrimSpace(pack.IDs.ContractID)
	if !safeOSAbsorptionRuntimeID(contractID, "contract_") {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing or invalid EffectiveContract id %q", pack.ID, contractID)}
	}
	memoryRevision := osAbsorptionContextPackRevision(pack, "memory://relevance-gated")
	if !knownMemoryLedgerRevision(memoryRevision) {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing known source revision for memory://relevance-gated", pack.ID)}
	}
	contract, err := readMemoryLedgerEffectiveContract(homePath, pack, contractID)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	if contract.Authority.SourceClass != "runtime" || contract.Authority.Status != "current" {
		return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s is not runtime/current authority", contract.ID)}
	}
	if contract.Privacy != row.Privacy {
		return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s privacy %q differs from row privacy %q", contract.ID, contract.Privacy, row.Privacy)}
	}
	if !sameMemoryLedgerTaskContract(pack.TaskContract, contract.TaskContract) {
		return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s task contract differs from ContextPack %s", contract.ID, pack.ID)}
	}
	if contractRevision := osAbsorptionEffectiveContractRevision(contract, "memory://relevance-gated"); contractRevision != memoryRevision {
		return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s memory revision %q differs from ContextPack %s revision %q", contract.ID, contractRevision, pack.ID, memoryRevision)}
	}
	packCreatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(pack.CreatedAt))
	if err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s created_at invalid: %v", pack.ID, err)}
	}

	store, err := mem.OpenReadOnly(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "memory ledger: " + err.Error()}
	}
	claims, err := store.LedgerClaims()
	if err != nil {
		return osAbsorptionProofResult{reason: "memory ledger claims: " + err.Error()}
	}
	evidence, err := store.Evidence()
	if err != nil {
		return osAbsorptionProofResult{reason: "memory ledger evidence: " + err.Error()}
	}
	rowRefs := make(map[string]bool, len(row.EvidenceRefs))
	for _, ref := range row.EvidenceRefs {
		if ref = memoryLedgerCanonicalRef(ref); ref != "" {
			rowRefs[ref] = true
		}
	}
	evidenceByID := make(map[string]mem.EvidenceItem, len(evidence))
	for _, item := range evidence {
		evidenceByID[item.ID] = item
	}

	sourceMatch := false
	temporalMismatch := false
	packConsumedMatch := false
	for _, claim := range claims {
		if !safeOSAbsorptionRuntimeID(strings.TrimSpace(claim.ID), "claim_") ||
			!candidates.ValidBasis(strings.TrimSpace(claim.PromotionBasis)) {
			continue
		}
		evidenceRef, matched := memoryLedgerClaimSourceRef(row, rowRefs, evidenceByID, claim)
		if !matched {
			continue
		}
		if !safeOSAbsorptionRuntimeID(evidenceRef, "ev_") {
			continue
		}
		if _, exists := evidenceByID[evidenceRef]; !exists {
			continue
		}
		sourceMatch = true
		claimCreatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.CreatedAt))
		if err != nil || claimCreatedAt.After(packCreatedAt) {
			temporalMismatch = true
			continue
		}
		claimRef := "claim://" + claim.ID
		if !containsOSAbsorptionRef(pack.ActiveMemory, claimRef) ||
			!memoryLedgerPackIncludesCurrentClaim(pack, claimRef) {
			continue
		}
		packConsumedMatch = true
		if !containsOSAbsorptionRef(contract.ActiveMemory, claimRef) {
			continue
		}
		return osAbsorptionProofResult{
			ok: true,
			proofRef: fmt.Sprintf(
				"%s:claim=%s;evidence=%s;context_pack=%s;contract=%s;memory_rev=%s",
				osAbsorptionProofModeMemoryLedger,
				claim.ID,
				evidenceRef,
				pack.ID,
				contractID,
				memoryRevision,
			),
		}
	}
	if packConsumedMatch {
		return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s missing selected claim consumed by ContextPack %s", contract.ID, pack.ID)}
	}
	if temporalMismatch {
		return osAbsorptionProofResult{reason: fmt.Sprintf("source-linked promoted claim postdates ContextPack %s", pack.ID)}
	}
	if sourceMatch {
		return osAbsorptionProofResult{reason: fmt.Sprintf("durably promoted source-linked claim not consumed by ContextPack %s in both ActiveMemory and Included(runtime/current)", pack.ID)}
	}
	return osAbsorptionProofResult{reason: "no durably promoted source-linked claim with persisted evidence"}
}

func memoryLedgerContextPackEvidenceID(refs []string) (string, error) {
	id := ""
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, osAbsorptionContextPackEvidenceRef) {
			continue
		}
		candidate := strings.TrimSpace(strings.TrimPrefix(ref, osAbsorptionContextPackEvidenceRef))
		if !safeOSAbsorptionRuntimeID(candidate, "ctx_") {
			return "", fmt.Errorf("memory ledger authority has invalid ContextPack id %q", candidate)
		}
		if id != "" {
			return "", fmt.Errorf("memory ledger authority has duplicate ContextPack ref")
		}
		id = candidate
	}
	if id == "" {
		return "", fmt.Errorf("memory ledger authority missing ContextPack ref")
	}
	return id, nil
}

func readMemoryLedgerEffectiveContract(homePath string, pack envelope.ContextPack, contractID string) (envelope.EffectiveContract, error) {
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(pack.CreatedAt))
	if err != nil {
		return envelope.EffectiveContract{}, fmt.Errorf("ContextPack %s created_at invalid: %w", pack.ID, err)
	}
	path := filepath.Join(homePath, "contracts", "effective", createdAt.UTC().Format("2006-01-02"), contractID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return envelope.EffectiveContract{}, fmt.Errorf("EffectiveContract %s at %s: %w", contractID, path, err)
	}
	var contract envelope.EffectiveContract
	if err := json.Unmarshal(body, &contract); err != nil {
		return envelope.EffectiveContract{}, fmt.Errorf("EffectiveContract %s parse: %w", contractID, err)
	}
	if contract.ID != contractID {
		return envelope.EffectiveContract{}, fmt.Errorf("EffectiveContract path id %s contains %s", contractID, contract.ID)
	}
	if err := contract.Validate(); err != nil {
		return envelope.EffectiveContract{}, fmt.Errorf("EffectiveContract %s invalid: %w", contractID, err)
	}
	return contract, nil
}

func knownMemoryLedgerRevision(revision string) bool {
	const prefix = "sha256:"
	revision = strings.TrimSpace(revision)
	digest := strings.TrimPrefix(revision, prefix)
	return strings.HasPrefix(revision, prefix) && len(digest) == 64 && isOSAbsorptionHex(digest)
}

func sameMemoryLedgerTaskContract(left, right envelope.TaskContract) bool {
	return left.Intent == right.Intent &&
		left.Mode == right.Mode &&
		left.DesiredOutcome == right.DesiredOutcome &&
		left.SuccessPredicate == right.SuccessPredicate &&
		slices.Equal(left.NonGoals, right.NonGoals) &&
		left.ProofMedium == right.ProofMedium
}

func osAbsorptionEffectiveContractRevision(contract envelope.EffectiveContract, ref string) string {
	for _, revision := range contract.SourceRevisions {
		if revision.Source == ref {
			return strings.TrimSpace(revision.Rev)
		}
	}
	return ""
}

func memoryLedgerPackIncludesCurrentClaim(pack envelope.ContextPack, claimRef string) bool {
	for _, included := range pack.Included {
		if included.Ref == claimRef && included.SourceClass == "runtime" && included.Status == "current" {
			return true
		}
	}
	return false
}

func containsOSAbsorptionRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

// osAbsorptionNeedsWorkPacketProof matches the OS packet directory wrapper.
// An explicit proof_mode wins over tuple inference so a row cannot silently
// receive a different proof mechanism than the source matrix requested.
func osAbsorptionNeedsWorkPacketProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode != "" {
		return row.ProofMode == osAbsorptionProofModeWorkPacket
	}
	return row.CompoundOwner == osAbsorptionWorkAuthorityRef && row.SourceKind == "packet_dir" && row.Status == "wrapper"
}

func osAbsorptionNeedsOSCapabilityProof(row OSAbsorptionMatrixRow) bool {
	tupleMatches := row.ID == "os_source_os_capability_cli" &&
		row.SourceRef == "cli:os capability" &&
		row.SourceKind == "compatibility_cli" &&
		row.CompoundOwner == capgraph.CapabilityGraphAuthorityRef &&
		row.Status == "wrapper"
	if row.ProofMode != "" {
		return row.ProofMode == osAbsorptionProofModeOSCapability && tupleMatches
	}
	return tupleMatches
}

// osAbsorptionNeedsDecisionsProof matches the os_source_decisions_repo row (and
// any row that opts in via proof_mode): it is proven by the decisions
// source-authority ledger having at least one record, not by the row's status
// string (today "evidence_only").
func osAbsorptionNeedsDecisionsProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeDecisions {
		return true
	}
	return row.CompoundOwner == osAbsorptionDecisionsLedgerRef
}

// osAbsorptionNeedsOperatorStateProof matches the os_source_operator_state row
// (and any row that opts in via proof_mode): it is proven by the same
// operator-state registry reader internal/policy uses to apply autonomy state
// and cite compound://registries/operator-state on policy decisions.
func osAbsorptionNeedsOperatorStateProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeOperatorState {
		return true
	}
	return row.CompoundOwner == policy.OperatorStateAuthorityRef
}

// osAbsorptionIsServedOrientationAuthorityRow is the structural owner
// invariant for served orientation. Row IDs, source filenames, current_role,
// and prose are all mutable evidence; the EffectiveContract authority owner
// is the stable classification seam.
func osAbsorptionIsServedOrientationAuthorityRow(row OSAbsorptionMatrixRow) bool {
	return row.CompoundOwner == osAbsorptionEffectiveContractAuthorityRef
}

const (
	osAbsorptionServedManifestFamily     = "os"
	osAbsorptionServedManifestItemPrefix = "compound://sources/manifests/os/items/"
)

type osAbsorptionServedManifest struct {
	SourceFamily string                              `json:"source_family"`
	Lifecycle    osAbsorptionServedManifestLifecycle `json:"lifecycle"`
	Items        []osAbsorptionServedManifestItem    `json:"items"`
}

type osAbsorptionServedManifestLifecycle struct {
	State string `json:"state"`
}

type osAbsorptionServedManifestItem struct {
	ID         string `json:"id"`
	SourceRef  string `json:"source_ref"`
	BodyPolicy string `json:"body_policy"`
	BodyRef    string `json:"body_ref"`
}

func osAbsorptionServedManifestItemRef(itemID string) string {
	return osAbsorptionServedManifestItemPrefix + strings.TrimSpace(itemID)
}

func osAbsorptionServedManifestItemID(ref string) (string, bool) {
	if !strings.HasPrefix(ref, osAbsorptionServedManifestItemPrefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(ref, osAbsorptionServedManifestItemPrefix))
	return id, safeOSAbsorptionRuntimeID(id, "srcitem_")
}

func loadOSAbsorptionServedManifest(homePath string) (osAbsorptionServedManifest, error) {
	path := filepath.Join(homePath, "sources", "manifests", osAbsorptionServedManifestFamily+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return osAbsorptionServedManifest{}, err
	}
	var manifest osAbsorptionServedManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return osAbsorptionServedManifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if manifest.SourceFamily != osAbsorptionServedManifestFamily {
		return osAbsorptionServedManifest{}, fmt.Errorf(
			"served source manifest family %q, want %q",
			manifest.SourceFamily,
			osAbsorptionServedManifestFamily,
		)
	}
	if !osAbsorptionServedManifestLifecycleOwned(manifest.Lifecycle.State) {
		return osAbsorptionServedManifest{}, fmt.Errorf(
			"served source manifest lifecycle %q has not reached verified custody",
			manifest.Lifecycle.State,
		)
	}
	return manifest, nil
}

func osAbsorptionServedManifestLifecycleOwned(state string) bool {
	switch state {
	case "verified", "compound_primary_writer", "legacy_frozen", "quiet_window_proven", "archived", "retired":
		return true
	default:
		return false
	}
}

func osAbsorptionServedManifestItemRole(item osAbsorptionServedManifestItem) string {
	sourceRef := filepath.ToSlash(filepath.Clean(strings.TrimSpace(item.SourceRef)))
	if strings.Contains(sourceRef, "/worktrees/") {
		return ""
	}
	switch {
	case strings.HasSuffix(sourceRef, "/"+osAbsorptionServedLiveStart):
		return osAbsorptionServedLiveStart
	case strings.HasSuffix(sourceRef, "/"+osAbsorptionServedWholeSystemMap):
		return osAbsorptionServedWholeSystemMap
	default:
		return ""
	}
}

func osAbsorptionServedManifestSourceRowsCheck(
	manifest osAbsorptionServedManifest,
	rows []OSAbsorptionMatrixRow,
) OSAbsorptionDoctorCheck {
	items := make(map[string]osAbsorptionServedManifestItem, len(manifest.Items))
	for _, item := range manifest.Items {
		items[item.ID] = item
	}
	counts := map[string]int{
		osAbsorptionServedLiveStart:      0,
		osAbsorptionServedWholeSystemMap: 0,
	}
	for _, row := range rows {
		if !osAbsorptionIsServedOrientationAuthorityRow(row) ||
			row.ProofMode != osAbsorptionProofModeServedOrient {
			continue
		}
		itemID, ok := osAbsorptionServedManifestItemID(row.SourceRef)
		if !ok {
			continue
		}
		item, ok := items[itemID]
		if !ok || item.BodyPolicy != "owned_snapshot" || strings.TrimSpace(item.BodyRef) == "" {
			continue
		}
		if role := osAbsorptionServedManifestItemRole(item); role != "" {
			counts[role]++
		}
	}
	ok := counts[osAbsorptionServedLiveStart] == 1 && counts[osAbsorptionServedWholeSystemMap] == 1
	return doctorCheck(
		"served_orientation_source_rows",
		ok,
		fmt.Sprintf(
			"SourceManifest os owned items: %s count=%d, %s count=%d",
			osAbsorptionServedLiveStart,
			counts[osAbsorptionServedLiveStart],
			osAbsorptionServedWholeSystemMap,
			counts[osAbsorptionServedWholeSystemMap],
		),
	)
}

func osAbsorptionExpectedServedSourceRefs(homePath string) ([]string, bool, error) {
	absorbedOSRoot, found, err := ResolveAbsorbedOSRoot(homePath)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	return []string{
		filepath.Clean(filepath.Join(absorbedOSRoot, filepath.FromSlash(osAbsorptionServedLiveStart))),
		filepath.Clean(filepath.Join(absorbedOSRoot, filepath.FromSlash(osAbsorptionServedWholeSystemMap))),
	}, true, nil
}

func osAbsorptionServedOrientationSourceRowsCheck(
	homePath string,
	rows []OSAbsorptionMatrixRow,
) (OSAbsorptionDoctorCheck, error) {
	_, installedHarnessFound, err := ResolveInstalledHarnessHome(homePath)
	if err != nil {
		return OSAbsorptionDoctorCheck{}, fmt.Errorf("resolve installed harness expectation: %w", err)
	}
	expected, found, err := osAbsorptionExpectedServedSourceRefs(homePath)
	if err != nil {
		return OSAbsorptionDoctorCheck{}, fmt.Errorf("resolve served orientation source rows: %w", err)
	}
	if !found {
		if installedHarnessFound {
			manifest, manifestErr := loadOSAbsorptionServedManifest(homePath)
			if manifestErr != nil {
				return doctorCheck(
					"served_orientation_source_rows",
					false,
					fmt.Sprintf(
						"%s is installed but verified compound://sources/manifests/os custody is unavailable: %v",
						InstalledHarnessHomeKey,
						manifestErr,
					),
				), nil
			}
			return osAbsorptionServedManifestSourceRowsCheck(manifest, rows), nil
		}
		return doctorCheck(
			"served_orientation_source_rows",
			true,
			"not applicable: neither installed harness home nor absorbed-OS root declared",
		), nil
	}

	matches := make(map[string][]OSAbsorptionMatrixRow, len(expected))
	for _, row := range rows {
		sourceRef := filepath.Clean(strings.TrimSpace(row.SourceRef))
		for _, expectedRef := range expected {
			if sourceRef == expectedRef {
				matches[expectedRef] = append(matches[expectedRef], row)
			}
		}
	}
	ok := true
	details := make([]string, 0, len(expected))
	for _, expectedRef := range expected {
		matched := matches[expectedRef]
		detail := fmt.Sprintf("%s count=%d", expectedRef, len(matched))
		if len(matched) != 1 {
			ok = false
		} else if matched[0].CompoundOwner != osAbsorptionEffectiveContractAuthorityRef ||
			matched[0].ProofMode != osAbsorptionProofModeServedOrient {
			ok = false
			detail += fmt.Sprintf(
				" owner=%q proof_mode=%q",
				matched[0].CompoundOwner,
				matched[0].ProofMode,
			)
		}
		details = append(details, detail)
	}
	return doctorCheck(
		"served_orientation_source_rows",
		ok,
		"expected exactly one classified row for each derived source: "+strings.Join(details, "; "),
	), nil
}

func validateOSAbsorptionServedOrientationDeclaration(row OSAbsorptionMatrixRow) error {
	if osAbsorptionIsServedOrientationAuthorityRow(row) && row.ProofMode != osAbsorptionProofModeServedOrient {
		return fmt.Errorf(
			"os absorption row %q owned by %s requires proof_mode %q, got %q",
			row.ID,
			osAbsorptionEffectiveContractAuthorityRef,
			osAbsorptionProofModeServedOrient,
			row.ProofMode,
		)
	}
	if row.ProofMode == osAbsorptionProofModeServedOrient && !osAbsorptionIsServedOrientationAuthorityRow(row) {
		return fmt.Errorf(
			"os absorption row %q uses proof_mode %q but compound_owner is %q, want %q",
			row.ID,
			osAbsorptionProofModeServedOrient,
			row.CompoundOwner,
			osAbsorptionEffectiveContractAuthorityRef,
		)
	}
	return nil
}

// osAbsorptionNeedsServedOrientationProof fails closed for both current and
// legacy-installed matrices. Import validation requires the explicit proof
// mode, while this structural fallback still dispatches the proof if someone
// removes proof_mode directly from the installed JSON. The proof itself then
// rejects the missing mode instead of silently degrading to "manual".
func osAbsorptionNeedsServedOrientationProof(row OSAbsorptionMatrixRow) bool {
	return osAbsorptionIsServedOrientationAuthorityRow(row) ||
		row.ProofMode == osAbsorptionProofModeServedOrient
}

// osAbsorptionProofResult is the outcome of one row's auto-proof attempt.
// skipped is distinct from ok/failed: it means the proof declined to run at
// all because a declared dependency (most commonly an absorbed-OS root) is
// not configured on this machine — the public-clone case, not a pass or a
// failure.
type osAbsorptionProofResult struct {
	ok       bool
	skipped  bool
	proofRef string
	reason   string
}

// osAbsorptionDecisionsProof is the auto-proof for os_source_decisions_repo:
// the decisions source-authority ledger (compound/internal/decisions) loads
// and has at least one record. Falsifier: an absent/empty ledger fails, it
// does not silently pass.
func osAbsorptionDecisionsProof(homePath string) osAbsorptionProofResult {
	records, err := decisions.LoadSourceAuthority(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "decisions source authority: " + err.Error()}
	}
	if len(records) == 0 {
		return osAbsorptionProofResult{reason: "decisions source authority ledger has record_count=0"}
	}
	return osAbsorptionProofResult{ok: true, proofRef: osAbsorptionDecisionsLedgerRef}
}

// osAbsorptionOperatorStateProof is the auto-proof for os_source_operator_state:
// the operator-state registry (compound/internal/policy's own reader) loads
// and is non-empty. That reader is what feeds ApplyOperatorState, which is
// what makes policy decisions cite compound://registries/operator-state — so
// "loads + non-empty" is the weakest real check that still proves the live
// consumer path, without inventing new state.
func osAbsorptionOperatorStateProof(homePath string) osAbsorptionProofResult {
	path := policy.OperatorStatePath(homePath)
	state, err := policy.LoadOperatorState(path)
	if err != nil {
		if os.IsNotExist(err) {
			return osAbsorptionProofResult{reason: "operator-state registry not found at " + path}
		}
		return osAbsorptionProofResult{reason: "operator-state registry: " + err.Error()}
	}
	if len(state.Raw) == 0 {
		return osAbsorptionProofResult{reason: "operator-state registry at " + path + " is empty"}
	}
	return osAbsorptionProofResult{ok: true, proofRef: policy.OperatorStateAuthorityRef}
}

type osAbsorptionServedOrientationEvidence struct {
	entrypoints  map[string]string
	contextPacks map[string]osAbsorptionServedContextPackEvidence
}

type osAbsorptionServedContextPackEvidence struct {
	id               string
	entrypointDigest string
}

type osAbsorptionServedOrientationProofInputs struct {
	evidence  osAbsorptionServedOrientationEvidence
	snapshots map[string]installedEntrypointSnapshot
}

func osAbsorptionServedOrientationProof(homePath string, row OSAbsorptionMatrixRow) osAbsorptionProofResult {
	structural, inputs := osAbsorptionServedOrientationStructuralProof(homePath, row)
	if !structural.ok {
		return structural
	}
	return osAbsorptionServedOrientationCurrentProof(homePath, row, inputs, time.Now().UTC())
}

func osAbsorptionServedOrientationStructuralProof(
	homePath string,
	row OSAbsorptionMatrixRow,
) (osAbsorptionProofResult, osAbsorptionServedOrientationProofInputs) {
	var inputs osAbsorptionServedOrientationProofInputs
	if row.ProofMode != osAbsorptionProofModeServedOrient {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"served orientation row %s requires proof_mode %q, got %q",
			row.ID,
			osAbsorptionProofModeServedOrient,
			row.ProofMode,
		)}, inputs
	}
	if !osAbsorptionIsServedOrientationAuthorityRow(row) {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"served orientation row %s must be owned by %s, got %q",
			row.ID,
			osAbsorptionEffectiveContractAuthorityRef,
			row.CompoundOwner,
		)}, inputs
	}
	evidence, err := parseOSAbsorptionServedOrientationEvidence(row.EvidenceRefs)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}, inputs
	}
	if err := validateOSAbsorptionServedOrientationBinding(homePath, row, evidence); err != nil {
		return osAbsorptionProofResult{reason: err.Error()}, inputs
	}
	_, manifestBacked := osAbsorptionServedManifestItemID(row.SourceRef)
	var expectedSourceRefs []string
	if !manifestBacked {
		var found bool
		expectedSourceRefs, found, err = osAbsorptionExpectedServedSourceRefs(homePath)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("resolve served orientation rollback refs: %v", err)}, inputs
		}
		if !found {
			return osAbsorptionProofResult{reason: "served orientation evidence has no declared absorbed-OS rollback refs"}, inputs
		}
	}

	inputs.evidence = evidence
	inputs.snapshots = make(map[string]installedEntrypointSnapshot, 2)
	for _, harness := range []string{"codex", "claude"} {
		snapshot, found, err := readInstalledHarnessEntrypoint(homePath, harness, installedEntrypointReadOps{})
		if err != nil || !found {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s installed entrypoint snapshot: found=%v err=%v",
				harness,
				found,
				err,
			)}, inputs
		}
		if filepath.Clean(evidence.entrypoints[harness]) != snapshot.path {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s served entrypoint %s differs from descriptor snapshot %s",
				harness,
				evidence.entrypoints[harness],
				snapshot.path,
			)}, inputs
		}
		inputs.snapshots[harness] = snapshot
		if manifestBacked {
			if err := validateOSAbsorptionGeneratedInstructionEntrypoint(harness, string(snapshot.body)); err != nil {
				return osAbsorptionProofResult{reason: err.Error()}, inputs
			}
		} else if err := validateOSAbsorptionServedEntrypointSources(harness, expectedSourceRefs, string(snapshot.body)); err != nil {
			return osAbsorptionProofResult{reason: err.Error()}, inputs
		}
	}

	return osAbsorptionProofResult{ok: true}, inputs
}

const (
	osAbsorptionServedContextPackMaxAge     = 24 * time.Hour
	osAbsorptionServedContextPackFutureSkew = 5 * time.Minute
)

func osAbsorptionServedOrientationCurrentProof(
	homePath string,
	row OSAbsorptionMatrixRow,
	inputs osAbsorptionServedOrientationProofInputs,
	now time.Time,
) osAbsorptionProofResult {
	evidence := inputs.evidence
	requiredRefs := []string{
		"control-plane://orientation-map",
		"control-plane://workflow-contract",
		"control-plane://sop-packets",
		"memory://relevance-gated",
	}
	contextPackIDs := map[string]string{}
	contractIDs := map[string]string{}
	entrypointDigests := map[string]string{}
	sharedScope := ""

	for _, harness := range []string{"codex", "claude"} {
		snapshot, found := inputs.snapshots[harness]
		if !found {
			return osAbsorptionProofResult{reason: fmt.Sprintf("%s installed entrypoint snapshot missing", harness)}
		}
		liveEntrypointDigest := snapshot.digest
		if liveEntrypointDigest != evidence.contextPacks[harness].entrypointDigest {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s ContextPack evidence entrypoint digest %s differs from live installed digest %s",
				harness,
				evidence.contextPacks[harness].entrypointDigest,
				liveEntrypointDigest,
			)}
		}

		pack, err := readOSAbsorptionContextPack(homePath, evidence.contextPacks[harness].id)
		if err != nil {
			return osAbsorptionProofResult{reason: err.Error()}
		}
		if pack.Authority.SourceClass != "runtime" || pack.Authority.Status != "current" {
			return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s is not runtime/current authority", pack.ID)}
		}
		if pack.Privacy != row.Privacy {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"ContextPack %s privacy %q differs from row privacy %q",
				pack.ID,
				pack.Privacy,
				row.Privacy,
			)}
		}
		if strings.TrimSpace(pack.TaskContract.Intent) == "" {
			return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing task intent", pack.ID)}
		}
		createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(pack.CreatedAt))
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s created_at invalid: %v", pack.ID, err)}
		}
		if createdAt.After(now.Add(osAbsorptionServedContextPackFutureSkew)) {
			return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s created_at exceeds allowed clock skew", pack.ID)}
		}
		if now.Sub(createdAt) > osAbsorptionServedContextPackMaxAge {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"ContextPack %s is stale: age=%s max=%s",
				pack.ID,
				now.Sub(createdAt).Round(time.Second),
				osAbsorptionServedContextPackMaxAge,
			)}
		}
		if snapshot.info.ModTime().After(createdAt.Add(time.Second)) {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"ContextPack %s predates the installed %s entrypoint digest",
				pack.ID,
				harness,
			)}
		}

		contractID := strings.TrimSpace(pack.IDs.ContractID)
		if !safeOSAbsorptionRuntimeID(contractID, "contract_") {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"ContextPack %s missing or invalid EffectiveContract id %q",
				pack.ID,
				contractID,
			)}
		}
		contract, err := readMemoryLedgerEffectiveContract(homePath, pack, contractID)
		if err != nil {
			return osAbsorptionProofResult{reason: err.Error()}
		}
		if contract.Authority.SourceClass != "runtime" || contract.Authority.Status != "current" {
			return osAbsorptionProofResult{reason: fmt.Sprintf("EffectiveContract %s is not runtime/current authority", contract.ID)}
		}
		contractHarnessFamily, _, supportedHarness := installedHarnessFamily(contract.Harness)
		if !supportedHarness || contractHarnessFamily != harness {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"EffectiveContract %s harness %q does not bind cited %s ContextPack %s",
				contract.ID,
				contract.Harness,
				harness,
				pack.ID,
			)}
		}
		if contract.CreatedAt != pack.CreatedAt {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"EffectiveContract %s created_at differs from ContextPack %s",
				contract.ID,
				pack.ID,
			)}
		}
		if !sameMemoryLedgerTaskContract(pack.TaskContract, contract.TaskContract) {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"EffectiveContract %s task contract differs from ContextPack %s",
				contract.ID,
				pack.ID,
			)}
		}
		entrypointRevisionSource := InstalledHarnessEntrypointRevisionPrefix + harness
		expectedEntrypointRevision := "sha256:" + liveEntrypointDigest
		packEntrypointRevision := osAbsorptionContextPackRevision(pack, entrypointRevisionSource)
		contractEntrypointRevision := osAbsorptionEffectiveContractRevision(contract, entrypointRevisionSource)
		switch {
		case packEntrypointRevision == "" && contractEntrypointRevision == "":
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s ContextPack %s and EffectiveContract %s missing installed entrypoint artifact binding %s",
				harness,
				pack.ID,
				contract.ID,
				entrypointRevisionSource,
			)}
		case packEntrypointRevision == "":
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"ContextPack %s missing installed entrypoint artifact binding %s",
				pack.ID,
				entrypointRevisionSource,
			)}
		case contractEntrypointRevision == "":
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"EffectiveContract %s missing installed entrypoint artifact binding %s",
				contract.ID,
				entrypointRevisionSource,
			)}
		case packEntrypointRevision != expectedEntrypointRevision ||
			contractEntrypointRevision != expectedEntrypointRevision:
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s installed entrypoint artifact binding mismatch: ContextPack=%s EffectiveContract=%s matrix/live=%s",
				harness,
				packEntrypointRevision,
				contractEntrypointRevision,
				expectedEntrypointRevision,
			)}
		}

		scope, err := validateOSAbsorptionServedScope(pack.Scope.Surface)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s scope: %v", pack.ID, err)}
		}
		if contract.Scope.Surface != scope {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"EffectiveContract %s scope %q differs from ContextPack %s scope %q",
				contract.ID,
				contract.Scope.Surface,
				pack.ID,
				scope,
			)}
		}
		if sharedScope == "" {
			sharedScope = scope
		} else if scope != sharedScope {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s ContextPack %s scope %q differs from shared served-orientation scope %q",
				harness,
				pack.ID,
				scope,
				sharedScope,
			)}
		}

		for _, ref := range requiredRefs {
			if !osAbsorptionContextPackIncludes(pack, ref) {
				return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing included ref %s", pack.ID, ref)}
			}
			packRevision := osAbsorptionContextPackRevision(pack, ref)
			if packRevision == "" || packRevision == "unknown" {
				return osAbsorptionProofResult{reason: fmt.Sprintf("ContextPack %s missing source revision for %s", pack.ID, ref)}
			}
			contractRevision := osAbsorptionEffectiveContractRevision(contract, ref)
			if contractRevision != packRevision {
				return osAbsorptionProofResult{reason: fmt.Sprintf(
					"EffectiveContract %s source revision %q differs from ContextPack %s revision %q for %s",
					contract.ID,
					contractRevision,
					pack.ID,
					packRevision,
					ref,
				)}
			}
			if strings.HasPrefix(ref, "control-plane://") {
				currentRevision, err := osAbsorptionCurrentServedSourceRevision(scope, ref)
				if err != nil {
					return osAbsorptionProofResult{reason: fmt.Sprintf("current source revision for %s: %v", ref, err)}
				}
				if currentRevision != packRevision {
					return osAbsorptionProofResult{reason: fmt.Sprintf(
						"ContextPack %s current source revision for %s is %s, recorded %s",
						pack.ID,
						ref,
						currentRevision,
						packRevision,
					)}
				}
			}
		}

		contextPackIDs[harness] = pack.ID
		contractIDs[harness] = contract.ID
		entrypointDigests[harness] = liveEntrypointDigest
	}

	return osAbsorptionProofResult{
		ok: true,
		proofRef: fmt.Sprintf(
			"%s:codex_context_pack=%s;codex_contract=%s;claude_context_pack=%s;claude_contract=%s;scope_sha256=%s;codex_sha256=%s;claude_sha256=%s",
			osAbsorptionProofModeServedOrient,
			contextPackIDs["codex"],
			contractIDs["codex"],
			contextPackIDs["claude"],
			contractIDs["claude"],
			digestBytes([]byte(sharedScope)),
			entrypointDigests["codex"],
			entrypointDigests["claude"],
		),
	}
}

func validateOSAbsorptionServedScope(scope string) (string, error) {
	scope = filepath.Clean(strings.TrimSpace(scope))
	if scope == "." || !filepath.IsAbs(scope) {
		return "", fmt.Errorf("must be an absolute directory, got %q", scope)
	}
	canonicalScope, err := filepath.EvalSymlinks(scope)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", scope, err)
	}
	canonicalScope = filepath.Clean(canonicalScope)
	if canonicalScope != scope {
		return "", fmt.Errorf("must be canonical: claim=%s resolved=%s", scope, canonicalScope)
	}
	info, err := os.Stat(canonicalScope)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("is not a directory: %s", canonicalScope)
	}
	return canonicalScope, nil
}

func osAbsorptionCurrentServedSourceRevision(scope, ref string) (string, error) {
	var rels []string
	switch ref {
	case "control-plane://orientation-map":
		rels = []string{"docs/ARCHITECTURE.md", "docs/LAYERS.md"}
	case "control-plane://workflow-contract":
		rels = []string{"docs/architecture/contracts/README.md"}
	case "control-plane://sop-packets":
		rels = []string{"docs/architecture/runtime.md", "docs/roadmap/phases.md"}
	default:
		return "", fmt.Errorf("unsupported served source %q", ref)
	}

	root := scope
	for {
		info, err := os.Stat(filepath.Join(root, "docs"))
		if err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("no docs root found from scope %s", scope)
		}
		root = parent
	}

	rows := make([]string, 0, len(rels))
	for _, rel := range rels {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("%s: %w", rel, err)
		}
		rows = append(rows, rel+":sha256:"+digestBytes(body)[:12])
	}
	sort.Strings(rows)
	return "sha256:" + digest12(strings.Join(rows, "\n")), nil
}

func parseOSAbsorptionServedOrientationEvidence(refs []string) (osAbsorptionServedOrientationEvidence, error) {
	evidence := osAbsorptionServedOrientationEvidence{
		entrypoints:  map[string]string{},
		contextPacks: map[string]osAbsorptionServedContextPackEvidence{},
	}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		switch {
		case strings.HasPrefix(ref, osAbsorptionServedEntrypointEvidenceRef):
			parts := strings.SplitN(strings.TrimPrefix(ref, osAbsorptionServedEntrypointEvidenceRef), ":", 2)
			if len(parts) != 2 || (parts[0] != "codex" && parts[0] != "claude") || strings.TrimSpace(parts[1]) == "" {
				return evidence, fmt.Errorf("served orientation evidence has invalid entrypoint ref %q", ref)
			}
			if evidence.entrypoints[parts[0]] != "" {
				return evidence, fmt.Errorf("served orientation evidence has duplicate %s entrypoint", parts[0])
			}
			evidence.entrypoints[parts[0]] = strings.TrimSpace(parts[1])
		case strings.HasPrefix(ref, osAbsorptionContextPackEvidenceRef):
			parts := strings.SplitN(strings.TrimPrefix(ref, osAbsorptionContextPackEvidenceRef), ":", 3)
			if len(parts) != 3 || (parts[0] != "codex" && parts[0] != "claude") {
				return evidence, fmt.Errorf("served orientation evidence has invalid harness-scoped ContextPack ref %q", ref)
			}
			harness := parts[0]
			id := strings.TrimSpace(parts[1])
			entrypointDigest := strings.TrimSpace(parts[2])
			if !safeOSAbsorptionRuntimeID(id, "ctx_") {
				return evidence, fmt.Errorf("served orientation evidence has invalid %s ContextPack id %q", harness, id)
			}
			if len(entrypointDigest) != 64 || !isOSAbsorptionHex(entrypointDigest) {
				return evidence, fmt.Errorf("served orientation evidence has invalid %s entrypoint digest %q", harness, entrypointDigest)
			}
			if _, exists := evidence.contextPacks[harness]; exists {
				return evidence, fmt.Errorf("served orientation evidence has duplicate %s ContextPack ref", harness)
			}
			evidence.contextPacks[harness] = osAbsorptionServedContextPackEvidence{
				id:               id,
				entrypointDigest: entrypointDigest,
			}
		}
	}
	for _, harness := range []string{"codex", "claude"} {
		if evidence.entrypoints[harness] == "" {
			return evidence, fmt.Errorf("served orientation evidence missing %s entrypoint ref", harness)
		}
	}
	for _, harness := range []string{"codex", "claude"} {
		if evidence.contextPacks[harness].id == "" {
			return evidence, fmt.Errorf("served orientation evidence missing %s ContextPack ref", harness)
		}
	}
	return evidence, nil
}

const (
	osAbsorptionRequiredOrientHeading    = "## Orient first — Compound (the served contract)"
	osAbsorptionRequiredMeaningAuthority = "Compound owns durable semantic meaning and promotion."
	osAbsorptionRequiredBrainRole        = "brain is source/projection/migration evidence."
	osAbsorptionRequiredGBrainRole       = "gbrain is a bridge/index only."
	osAbsorptionServedBlockBegin         = "<!-- BEGIN COMPOUND SERVED ORIENTATION CONTRACT -->"
	osAbsorptionServedBlockEnd           = "<!-- END COMPOUND SERVED ORIENTATION CONTRACT -->"

	osAbsorptionServedCodexEntrypoint  = ".codex/AGENTS.md"
	osAbsorptionServedClaudeEntrypoint = ".claude/CLAUDE.md"
	osAbsorptionServedLiveStart        = "LIVE_START.md"
	osAbsorptionServedWholeSystemMap   = "universe/WHOLE_SYSTEM_MAP.md"
)

// validateOSAbsorptionServedOrientationBinding prevents copied files from
// masquerading as live evidence. The rollback side is either a promoted legacy
// root (compatibility) or a verified owned SourceManifest item; the served side
// is always the promoted installed harness home.
func validateOSAbsorptionServedOrientationBinding(
	homePath string,
	row OSAbsorptionMatrixRow,
	evidence osAbsorptionServedOrientationEvidence,
) error {
	itemID, manifestBacked := osAbsorptionServedManifestItemID(row.SourceRef)
	if manifestBacked {
		manifest, manifestErr := loadOSAbsorptionServedManifest(homePath)
		if manifestErr != nil {
			return fmt.Errorf("load served SourceManifest custody: %w", manifestErr)
		}
		var item osAbsorptionServedManifestItem
		for _, candidate := range manifest.Items {
			if candidate.ID == itemID {
				item = candidate
				break
			}
		}
		if item.ID == "" || osAbsorptionServedManifestItemRole(item) == "" ||
			item.BodyPolicy != "owned_snapshot" || strings.TrimSpace(item.BodyRef) == "" {
			return fmt.Errorf("served orientation SourceManifest item %q is not an owned canonical entrypoint", itemID)
		}
		store, storeErr := home.NewArtifactStore(homePath)
		if storeErr != nil {
			return fmt.Errorf("open served SourceManifest artifact store: %w", storeErr)
		}
		if _, verifyErr := store.Verify(item.BodyRef); verifyErr != nil {
			return fmt.Errorf("verify served SourceManifest item %q body: %w", itemID, verifyErr)
		}
	} else {
		absorbedOSRoot, found, err := ResolveAbsorbedOSRoot(homePath)
		if err != nil {
			return fmt.Errorf("resolve declared absorbed-OS root: %w", err)
		}
		if !found || !absorbedOSRootPresentOnDisk(absorbedOSRoot) {
			return fmt.Errorf("served orientation evidence has no declared live absorbed-OS root or owned SourceManifest item")
		}
		sourceRef := filepath.Clean(strings.TrimSpace(row.SourceRef))
		allowedSources := map[string]bool{
			filepath.Clean(filepath.Join(absorbedOSRoot, filepath.FromSlash(osAbsorptionServedLiveStart))):      true,
			filepath.Clean(filepath.Join(absorbedOSRoot, filepath.FromSlash(osAbsorptionServedWholeSystemMap))): true,
		}
		if !allowedSources[sourceRef] {
			return fmt.Errorf(
				"served orientation source_ref %s is not a declared absorbed-OS source entrypoint",
				row.SourceRef,
			)
		}
		sourceInfo, statErr := os.Stat(sourceRef)
		if statErr != nil {
			return fmt.Errorf("declared absorbed-OS source entrypoint %s: %w", sourceRef, statErr)
		}
		if !sourceInfo.Mode().IsRegular() {
			return fmt.Errorf("declared absorbed-OS source entrypoint %s is not a regular file", sourceRef)
		}
	}

	installedHome, found, err := ResolveInstalledHarnessHome(homePath)
	if err != nil {
		return fmt.Errorf("resolve declared installed harness home: %w", err)
	}
	if !found || !absorbedOSRootPresentOnDisk(installedHome) {
		return fmt.Errorf("served orientation evidence has no declared live installed harness home")
	}
	expectedEntrypoints := map[string]string{
		"codex":  filepath.Clean(filepath.Join(installedHome, filepath.FromSlash(osAbsorptionServedCodexEntrypoint))),
		"claude": filepath.Clean(filepath.Join(installedHome, filepath.FromSlash(osAbsorptionServedClaudeEntrypoint))),
	}
	for _, harness := range []string{"codex", "claude"} {
		got := filepath.Clean(evidence.entrypoints[harness])
		want := expectedEntrypoints[harness]
		if got != want {
			return fmt.Errorf(
				"%s served entrypoint %s must equal live installed entrypoint %s",
				harness,
				got,
				want,
			)
		}
	}
	return nil
}

func validateOSAbsorptionServedEntrypoint(harness, sourceRef, body string) error {
	return validateOSAbsorptionServedEntrypointSources(harness, []string{sourceRef}, body)
}
func validateOSAbsorptionGeneratedInstructionEntrypoint(harness, body string) error {
	renderHarness := harness
	if harness == "claude" {
		renderHarness = "claude_code"
	}
	begin := "<!-- BEGIN COMPOUND GENERATED INSTRUCTIONS harness=" + renderHarness + " -->"
	end := "<!-- END COMPOUND GENERATED INSTRUCTIONS -->"
	if strings.Count(body, begin) != 1 || strings.Count(body, end) != 1 {
		return fmt.Errorf("%s entrypoint lacks one exact Compound-generated instruction block", harness)
	}
	start := strings.Index(body, begin)
	finish := strings.Index(body[start:], end)
	if finish < 0 {
		return fmt.Errorf("%s entrypoint Compound-generated instruction block is unterminated", harness)
	}
	block := body[start : start+finish+len(end)]
	required := []string{
		"## Compound-generated contract",
		"Generated by `compound contract compile` for **" + renderHarness + "**.",
		"Compound is the authority",
		"`compound orient`",
	}
	for _, want := range required {
		if !strings.Contains(block, want) {
			return fmt.Errorf("%s entrypoint Compound-generated instruction block missing %q", harness, want)
		}
	}
	if strings.Contains(body, osAbsorptionServedBlockBegin) {
		return fmt.Errorf("%s entrypoint retains the retired served-orientation rollback block", harness)
	}
	return nil
}

func validateOSAbsorptionServedEntrypointSources(harness string, sourceRefs []string, body string) error {
	block, err := osAbsorptionServedContractBlock(body)
	if err != nil {
		return fmt.Errorf("%s entrypoint %w", harness, err)
	}
	requiredPrefix := []string{
		osAbsorptionRequiredOrientHeading,
		"Run `compound orient --harness " + harness + " --task \"<task>\"`.",
		osAbsorptionRequiredMeaningAuthority,
		osAbsorptionRequiredBrainRole,
		osAbsorptionRequiredGBrainRole,
		"These files remain source-in-place evidence and rollback:",
	}
	for i, want := range requiredPrefix {
		if i >= len(block) {
			return fmt.Errorf("%s entrypoint managed block missing exact line %q at position %d", harness, want, i+1)
		}
		if block[i] != want {
			return fmt.Errorf("%s entrypoint managed block missing exact line %q at position %d; got %q", harness, want, i+1, block[i])
		}
	}
	sourceSeen := map[string]bool{}
	actualSourceRefs := block[len(requiredPrefix):]
	for _, ref := range actualSourceRefs {
		if strings.TrimSpace(ref) != ref || ref == "" || strings.ContainsAny(ref, "`<>") || !filepath.IsAbs(ref) {
			return fmt.Errorf("%s entrypoint managed block has invalid source ref line %q", harness, ref)
		}
		if sourceSeen[ref] {
			return fmt.Errorf("%s entrypoint managed block has duplicate source ref %q", harness, ref)
		}
		sourceSeen[ref] = true
	}
	if len(actualSourceRefs) != len(sourceRefs) {
		return fmt.Errorf(
			"%s entrypoint managed block must contain exact rollback refs %v, got %v",
			harness,
			sourceRefs,
			actualSourceRefs,
		)
	}
	for i, want := range sourceRefs {
		if actualSourceRefs[i] != want {
			return fmt.Errorf(
				"%s entrypoint managed block must contain exact rollback refs %v in order, got %v",
				harness,
				sourceRefs,
				actualSourceRefs,
			)
		}
	}

	paragraphs := osAbsorptionActiveMarkdownParagraphs(body)
	// Reject ANY "## Orient first — " heading that is not the required
	// Compound heading — genericized so this holds for any retired
	// predecessor orientation entrypoint, not just one hardcoded legacy name.
	for _, paragraph := range paragraphs {
		trimmed := strings.TrimSpace(paragraph)
		if strings.HasPrefix(trimmed, "## Orient first — ") && trimmed != osAbsorptionRequiredOrientHeading {
			return fmt.Errorf("%s entrypoint retains retired orient heading %q", harness, trimmed)
		}
	}
	for _, paragraph := range paragraphs {
		if directive, ok := osAbsorptionRetiredFirstActionRecall(paragraph); ok {
			return fmt.Errorf("%s entrypoint retains retired first-action directive %q", harness, directive)
		}
	}

	for _, paragraph := range paragraphs {
		for _, clause := range osAbsorptionSemanticClauses(paragraph) {
			if contradiction, ok := osAbsorptionMemoryAuthorityContradiction(clause); ok {
				return fmt.Errorf("%s entrypoint retains contradictory memory authority guidance %q", harness, contradiction)
			}
		}
	}
	return nil
}

func osAbsorptionRetiredFirstActionRecall(paragraph string) (string, bool) {
	trimmed := strings.TrimSpace(paragraph)
	if trimmed == "" || osAbsorptionRetiredQuotation(trimmed) {
		return "", false
	}
	normalized := osAbsorptionSemanticWords(trimmed)
	words := strings.Fields(normalized)
	candidate := strings.Contains(normalized, "first action is recall") ||
		strings.Contains(normalized, "recall first") ||
		strings.Contains(normalized, "before any work") &&
			(osAbsorptionHasWord(words, "brain") || osAbsorptionHasWord(words, "gbrain"))
	if candidate {
		for i, word := range words {
			if word == "recall" && !osAbsorptionWriteActionNegated(words, i) {
				return trimmed, true
			}
		}
	}
	return "", false
}

// osAbsorptionServedContractBlock extracts one exact managed active-prose
// block. The delimiters themselves are comments, but must appear as unquoted,
// unindented physical lines outside any CommonMark fenced block. This makes
// positive authority proof structural: examples, inline-code quotations, and
// longer outer fences cannot satisfy it.
func osAbsorptionServedContractBlock(body string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var block []string
	inBlock := false
	foundBlock := false
	inComment := false
	var fence osAbsorptionMarkdownFence

	for _, raw := range lines {
		if fence.marker != 0 {
			if fence.closes(raw) {
				fence = osAbsorptionMarkdownFence{}
			}
			continue
		}
		if inComment {
			_ = osAbsorptionStripHTMLComments(raw, &inComment)
			continue
		}
		if osAbsorptionMarkdownNonProseLine(raw) {
			continue
		}
		switch raw {
		case osAbsorptionServedBlockBegin:
			if foundBlock || inBlock {
				return nil, fmt.Errorf("has duplicate managed block begin marker")
			}
			inBlock = true
			foundBlock = true
			continue
		case osAbsorptionServedBlockEnd:
			if !inBlock {
				return nil, fmt.Errorf("has managed block end without active begin")
			}
			inBlock = false
			continue
		}
		if opened, ok := osAbsorptionOpenMarkdownFence(raw); ok {
			fence = opened
			continue
		}
		line := osAbsorptionStripHTMLComments(raw, &inComment)
		if inBlock {
			if inComment || strings.TrimSpace(line) != raw || raw == "" {
				return nil, fmt.Errorf("managed block contains non-active or non-exact line %q", raw)
			}
			block = append(block, raw)
		}
	}
	if inBlock {
		return nil, fmt.Errorf("managed block is missing end marker")
	}
	if !foundBlock {
		return nil, fmt.Errorf("missing managed block begin marker %q", osAbsorptionServedBlockBegin)
	}
	return block, nil
}

// osAbsorptionActiveMarkdownParagraphs renders the limited text surface this
// guard needs: comments, CommonMark fenced/indented code, and blockquotes are
// excluded; consecutive prose lines are joined into paragraphs so semantic
// checks cannot be bypassed with a physical line break.
func osAbsorptionActiveMarkdownParagraphs(body string) []string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var paragraphs []string
	var paragraph []string
	inComment := false
	var fence osAbsorptionMarkdownFence
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		paragraphs = append(paragraphs, strings.Join(paragraph, " "))
		paragraph = nil
	}

	for _, raw := range lines {
		if fence.marker != 0 {
			if fence.closes(raw) {
				fence = osAbsorptionMarkdownFence{}
			}
			continue
		}
		if osAbsorptionMarkdownNonProseLine(raw) {
			flush()
			continue
		}
		line := osAbsorptionStripHTMLComments(raw, &inComment)
		if opened, ok := osAbsorptionOpenMarkdownFence(line); ok {
			flush()
			fence = opened
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			flush()
			paragraphs = append(paragraphs, trimmed)
			continue
		}
		if osAbsorptionMarkdownListItem(trimmed) {
			flush()
		}
		paragraph = append(paragraph, trimmed)
	}
	flush()
	return paragraphs
}

// osAbsorptionSemanticClauses splits rendered prose at sentence terminators
// before applying retired-quotation and authority checks. Without this seam, a
// paragraph that begins with a retired quotation can hide a live instruction
// later in the same paragraph. Semicolons deliberately stay inside a clause:
// "brain is not merely evidence; it is the authority" is one assertion.
func osAbsorptionSemanticClauses(paragraph string) []string {
	var clauses []string
	start := 0
	for i, r := range paragraph {
		switch r {
		case '.', '!', '?':
			clause := strings.TrimSpace(paragraph[start : i+1])
			if clause != "" {
				clauses = append(clauses, clause)
			}
			start = i + 1
		}
	}
	if clause := strings.TrimSpace(paragraph[start:]); clause != "" {
		clauses = append(clauses, clause)
	}
	return clauses
}

type osAbsorptionMarkdownFence struct {
	marker byte
	length int
}

func osAbsorptionOpenMarkdownFence(line string) (osAbsorptionMarkdownFence, bool) {
	content, ok := osAbsorptionMarkdownFenceContent(line)
	if !ok || len(content) < 3 || (content[0] != '`' && content[0] != '~') {
		return osAbsorptionMarkdownFence{}, false
	}
	marker := content[0]
	length := 0
	for length < len(content) && content[length] == marker {
		length++
	}
	if length < 3 {
		return osAbsorptionMarkdownFence{}, false
	}
	if marker == '`' && strings.ContainsRune(content[length:], '`') {
		return osAbsorptionMarkdownFence{}, false
	}
	return osAbsorptionMarkdownFence{marker: marker, length: length}, true
}

func (f osAbsorptionMarkdownFence) closes(line string) bool {
	content, ok := osAbsorptionMarkdownFenceContent(line)
	if !ok || len(content) < f.length || content[0] != f.marker {
		return false
	}
	length := 0
	for length < len(content) && content[length] == f.marker {
		length++
	}
	return length >= f.length && strings.TrimSpace(content[length:]) == ""
}

func osAbsorptionMarkdownFenceContent(line string) (string, bool) {
	if strings.HasPrefix(line, "\t") {
		return "", false
	}
	leading := 0
	for leading < len(line) && line[leading] == ' ' {
		leading++
	}
	if leading > 3 {
		return "", false
	}
	return line[leading:], true
}

func osAbsorptionMarkdownNonProseLine(line string) bool {
	if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
		return true
	}
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, ">")
}

func osAbsorptionMarkdownListItem(line string) bool {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return true
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' '
}

func osAbsorptionStripHTMLComments(line string, inComment *bool) string {
	var out strings.Builder
	for line != "" {
		if *inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				return out.String()
			}
			line = line[end+3:]
			*inComment = false
			continue
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			out.WriteString(line)
			break
		}
		out.WriteString(line[:start])
		line = line[start+4:]
		*inComment = true
	}
	return out.String()
}

func osAbsorptionMemoryAuthorityContradiction(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || osAbsorptionRetiredQuotation(trimmed) {
		return "", false
	}
	normalized := osAbsorptionSemanticWords(trimmed)
	if normalized == "" {
		return "", false
	}
	words := strings.Fields(normalized)

	hasMemorySubject := osAbsorptionHasWord(words, "brain") || osAbsorptionHasWord(words, "gbrain")
	if !hasMemorySubject {
		return "", false
	}
	for _, authorityClause := range strings.Split(trimmed, ";") {
		authorityWords := strings.Fields(osAbsorptionSemanticWords(authorityClause))
		if osAbsorptionMemoryAuthorityRoleBeforeSubject(authorityWords) {
			return strings.TrimSpace(authorityClause), true
		}
	}
	if osAbsorptionHasActiveMemoryAuthorityAssertion(words) {
		return trimmed, true
	}

	// A semicolon can connect two independent instructions. Keep it intact for
	// authority assertions ("not merely evidence; it is the authority"), but
	// do not let a write verb in one side acquire brain/gbrain as the target
	// from the other side.
	for _, writeClause := range strings.Split(trimmed, ";") {
		writeWords := strings.Fields(osAbsorptionSemanticWords(writeClause))
		if (osAbsorptionHasWord(writeWords, "brain") || osAbsorptionHasWord(writeWords, "gbrain")) &&
			osAbsorptionDirectBrainWrite(writeWords) {
			return strings.TrimSpace(writeClause), true
		}
	}
	return "", false
}

func osAbsorptionRetiredQuotation(line string) bool {
	normalized := osAbsorptionSemanticWords(line)
	for _, prefix := range []string{
		"retired quotation ",
		"retired guidance ",
		"retired instruction ",
		"obsolete wording ",
		"obsolete guidance ",
		"historical quotation ",
		"superseded wording ",
		"superseded guidance ",
		"former instruction ",
	} {
		if strings.HasPrefix(normalized+" ", prefix) {
			return true
		}
	}
	words := strings.Fields(normalized)
	if osAbsorptionHasAnyWord(words, "retired", "obsolete", "superseded", "historical", "former") &&
		osAbsorptionHasAnyWord(words, "quotation", "quote", "statement", "claim", "guidance", "instruction", "wording") &&
		!osAbsorptionHasAnyWord(words, "but", "however", "remains", "still", "wins") {
		return true
	}
	return false
}

func osAbsorptionSemanticWords(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	lastSpace := true
	for _, r := range value {
		isWord := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isWord {
			out.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			out.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(out.String())
}

func osAbsorptionHasActiveMemoryAuthorityAssertion(words []string) bool {
	for subjectIndex, word := range words {
		if word != "brain" && word != "gbrain" {
			continue
		}
		if subjectIndex > 0 && words[subjectIndex-1] == "not" {
			continue
		}
		roleIndex, ok := osAbsorptionMemoryAuthorityRoleAfter(words, subjectIndex)
		if !ok {
			continue
		}
		if !osAbsorptionMemoryAuthorityAssertionNegated(words, subjectIndex, roleIndex) {
			return true
		}
	}
	return false
}

func osAbsorptionMemoryAuthorityRoleBeforeSubject(words []string) bool {
	for subjectIndex, word := range words {
		if word != "brain" && word != "gbrain" {
			continue
		}
		start := 0
		for i := subjectIndex - 1; i >= 0; i-- {
			if words[i] == "compound" || words[i] == "brain" || words[i] == "gbrain" {
				start = i + 1
				break
			}
		}
		before := words[start:subjectIndex]
		if !osAbsorptionHasAnyWord(before, "is", "remains", "becomes") {
			continue
		}
		if osAbsorptionHasAnyWord(before, "authority", "authoritative", "canon", "canonical") ||
			osAbsorptionWordsContain(before, "system", "record") ||
			(osAbsorptionWordsContain(before, "system", "of") && osAbsorptionHasWord(before, "record")) {
			return true
		}
	}
	return false
}

func osAbsorptionMemoryAuthorityRoleAfter(words []string, subjectIndex int) (int, bool) {
	end := len(words)
	for i := subjectIndex + 1; i < end; i++ {
		if words[i] == "compound" || words[i] == "brain" || words[i] == "gbrain" {
			break
		}
		switch words[i] {
		case "authority", "authoritative", "canon", "outranks", "overrides", "precedence", "prevails", "supersedes", "wins":
			return i, true
		case "canonical":
			windowEnd := i + 4
			if windowEnd > end {
				windowEnd = end
			}
			if osAbsorptionHasAnyWord(words[i+1:windowEnd], "authority", "memory", "meaning", "truth") ||
				osAbsorptionHasAnyWord(words[subjectIndex+1:i], "is", "remains") {
				return i, true
			}
		case "source":
			if i+2 < end && words[i+1] == "of" && words[i+2] == "truth" {
				return i, true
			}
		case "system":
			if i+2 < end && words[i+1] == "of" && words[i+2] == "record" {
				return i, true
			}
		case "owner", "owns":
			windowEnd := i + 4
			if windowEnd > end {
				windowEnd = end
			}
			if osAbsorptionHasAnyWord(words[i+1:windowEnd], "meaning", "memory", "promotion", "truth") ||
				osAbsorptionHasAnyWord(words[subjectIndex+1:i], "meaning", "memory", "promotion", "truth") {
				return i, true
			}
		case "memory":
			windowEnd := i + 3
			if windowEnd > end {
				windowEnd = end
			}
			if osAbsorptionHasAnyWord(words[i+1:windowEnd], "authority", "owner") &&
				osAbsorptionHasAnyWord(words[subjectIndex+1:i], "durable", "canonical") {
				return i, true
			}
		}
	}
	return 0, false
}

func osAbsorptionMemoryAuthorityAssertionNegated(words []string, subjectIndex, roleIndex int) bool {
	segment := words[subjectIndex+1 : roleIndex+1]
	for i, word := range segment {
		if word != "not" && word != "never" {
			continue
		}
		afterNegative := segment[i+1:]
		if osAbsorptionHasAnyWord(afterNegative, "remains", "wins") ||
			(osAbsorptionHasAnyWord(afterNegative, "merely", "just", "only") &&
				osAbsorptionHasAnyWord(afterNegative, "is", "becomes", "authority", "authoritative", "canon", "canonical", "precedence")) {
			return false
		}
		return true
	}

	if subjectIndex > 0 && words[subjectIndex-1] == "not" {
		return true
	}
	if subjectIndex > 1 && words[subjectIndex-2] == "not" {
		switch words[subjectIndex-1] {
		case "call", "consider", "declare", "make", "regard", "treat", "use":
			return true
		case "forget":
			return false
		}
	}
	from := subjectIndex - 3
	if from < 0 {
		from = 0
	}
	if osAbsorptionHasAnyWord(words[from:subjectIndex], "reject", "rejects", "rejected", "prevent", "prevents") {
		return true
	}
	end := roleIndex + 4
	if end > len(words) {
		end = len(words)
	}
	return osAbsorptionHasAnyWord(words[roleIndex+1:end], "forbidden", "prohibited", "rejected", "retired")
}

func osAbsorptionDirectBrainWrite(words []string) bool {
	actions := map[string]bool{
		"append": true, "appends": true,
		"curate": true, "curates": true,
		"keep": true, "keeps": true,
		"land": true, "lands": true,
		"persist": true, "persists": true,
		"promote": true, "promotes": true,
		"record": true, "records": true,
		"save": true, "saves": true,
		"store": true, "stores": true,
		"write": true, "writes": true,
	}
	durableMarkers := map[string]bool{
		"canon": true, "canonical": true,
		"claim": true, "claims": true,
		"decision": true, "decisions": true,
		"durable":  true,
		"evidence": true,
		"fact":     true, "facts": true,
		"memory":  true,
		"outcome": true, "outcomes": true,
		"recipe": true, "recipes": true,
		"review": true, "reviews": true,
		"truth": true,
	}

	for actionIndex, word := range words {
		if !actions[word] {
			continue
		}
		if osAbsorptionWriteActionNegated(words, actionIndex) {
			continue
		}
		for targetIndex := actionIndex + 1; targetIndex < len(words); targetIndex++ {
			if words[targetIndex] != "brain" && words[targetIndex] != "gbrain" {
				continue
			}
			if targetIndex > 0 && words[targetIndex-1] == "from" {
				continue
			}
			if osAbsorptionWordsContain(words[targetIndex+1:], "to", "compound") {
				continue
			}
			hasTargetDirection := targetIndex == actionIndex+1 ||
				osAbsorptionHasAnyWord(words[actionIndex+1:targetIndex], "to", "into", "in", "under", "at")
			if !hasTargetDirection {
				continue
			}
			if osAbsorptionHasAnyMappedWord(words, durableMarkers) {
				return true
			}
		}
	}
	for targetIndex, word := range words {
		if word != "brain" && word != "gbrain" {
			continue
		}
		if targetIndex == 0 ||
			!osAbsorptionHasAnyWord(words[targetIndex-1:targetIndex], "to", "into", "in", "under", "at") {
			continue
		}
		for actionIndex := targetIndex + 1; actionIndex < len(words); actionIndex++ {
			if words[actionIndex] == "brain" || words[actionIndex] == "gbrain" || words[actionIndex] == "compound" {
				break
			}
			if actions[words[actionIndex]] &&
				!osAbsorptionWriteActionNegated(words, actionIndex) &&
				osAbsorptionHasAnyMappedWord(words[actionIndex+1:], durableMarkers) {
				return true
			}
		}
	}
	return false
}

func osAbsorptionWriteActionNegated(words []string, actionIndex int) bool {
	from := actionIndex - 3
	if from < 0 {
		from = 0
	}
	window := words[from:actionIndex]
	if osAbsorptionHasWord(window, "forget") {
		return false
	}
	return osAbsorptionHasAnyWord(window, "not", "never")
}

func osAbsorptionHasWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}

func osAbsorptionHasAnyWord(words []string, wants ...string) bool {
	for _, want := range wants {
		if osAbsorptionHasWord(words, want) {
			return true
		}
	}
	return false
}

func osAbsorptionHasAnyMappedWord(words []string, wants map[string]bool) bool {
	for _, word := range words {
		if wants[word] {
			return true
		}
	}
	return false
}

func osAbsorptionWordsContain(words []string, first, second string) bool {
	for i := 0; i+1 < len(words); i++ {
		if words[i] == first && words[i+1] == second {
			return true
		}
	}
	return false
}

func readOSAbsorptionContextPack(homePath, id string) (envelope.ContextPack, error) {
	path := filepath.Join(homePath, "context", "packs", id+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return envelope.ContextPack{}, fmt.Errorf("ContextPack %s at %s: %w", id, path, err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(body, &pack); err != nil {
		return envelope.ContextPack{}, fmt.Errorf("ContextPack %s parse: %w", id, err)
	}
	if pack.ID != id {
		return envelope.ContextPack{}, fmt.Errorf("ContextPack path id %s contains %s", id, pack.ID)
	}
	if err := pack.Validate(); err != nil {
		return envelope.ContextPack{}, fmt.Errorf("ContextPack %s invalid: %w", id, err)
	}
	return pack, nil
}

func osAbsorptionContextPackIncludes(pack envelope.ContextPack, ref string) bool {
	for _, included := range pack.Included {
		if included.Ref == ref {
			return true
		}
	}
	return false
}

func osAbsorptionContextPackRevision(pack envelope.ContextPack, ref string) string {
	for _, revision := range pack.SourceRevisions {
		if revision.Source == ref {
			return strings.TrimSpace(revision.Rev)
		}
	}
	return ""
}

// osAbsorptionWorkPacketProof proves both halves of packet absorption:
// every explicitly cited WorkEnvelope is a completed packet-backed chain, and
// at least one current packet beneath source_ref is content-bound to the full
// WorkEnvelope -> ContextPack -> Receipt -> Transition consumer path. The
// second condition prevents unrelated packet imports from proving a directory.
func osAbsorptionWorkPacketProof(homePath string, row OSAbsorptionMatrixRow) osAbsorptionProofResult {
	workIDs, err := osAbsorptionWorkEvidenceIDs(row.EvidenceRefs)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	if len(workIDs) == 0 {
		return osAbsorptionProofResult{reason: "work packet authority requires at least one work_* evidence_ref"}
	}
	sourceDir := strings.TrimSpace(row.SourceRef)
	if _, statErr := os.Stat(sourceDir); os.IsNotExist(statErr) {
		custody := inspectLegacySourceFamilyCustody(homePath, "os", filepath.Dir(sourceDir))
		if !custody.CustodyComplete || !legacySourceLifecycleFrozen(custody.LifecycleState) {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"work packet authority source directory unavailable without frozen complete OS custody: status=%s lifecycle=%s error=%s",
				custody.Status,
				custody.LifecycleState,
				custody.Error,
			)}
		}
		var result osAbsorptionProofResult
		lockErr := home.WithAutonomyAdmissionLock(homePath, func() error {
			result = osAbsorptionFrozenWorkPacketProofLocked(homePath, workIDs)
			return nil
		})
		if lockErr != nil {
			return osAbsorptionProofResult{reason: "work packet authority snapshot: " + lockErr.Error()}
		}
		return result
	}
	candidates, packetFiles, parseFailures, err := osAbsorptionSourcePacketCandidates(row)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	var result osAbsorptionProofResult
	err = home.WithAutonomyAdmissionLock(homePath, func() error {
		result = osAbsorptionWorkPacketProofLocked(homePath, workIDs, candidates, packetFiles, parseFailures)
		return nil
	})
	if err != nil {
		return osAbsorptionProofResult{reason: "work packet authority snapshot: " + err.Error()}
	}
	return result
}

func osAbsorptionWorkPacketProofLocked(homePath string, workIDs []string, candidates []osAbsorptionSourceCandidate, packetFiles, parseFailures int) osAbsorptionProofResult {
	store, err := work.Open(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "work packet authority: " + err.Error()}
	}
	for _, workID := range workIDs {
		w, err := store.Get(workID)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s: %v", workID, err)}
		}
		item, err := store.ProofChainForWork(workID)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s proof chain: %v", workID, err)}
		}
		if err := validateOSAbsorptionPacketWork(homePath, w, workID, item); err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s: %v", workID, err)}
		}
	}

	sourceWorkID, err := osAbsorptionSourcePacketWork(homePath, store, candidates, packetFiles, parseFailures)
	if err != nil {
		return osAbsorptionProofResult{reason: err.Error()}
	}
	prerequisiteIDs := append([]string(nil), workIDs...)
	sort.Strings(prerequisiteIDs)
	proofRef := osAbsorptionProofModeWorkPacket + ":prerequisite=" + strings.Join(prerequisiteIDs, ",") + ";source_consumer=" + sourceWorkID
	return osAbsorptionProofResult{ok: true, proofRef: proofRef}
}

func osAbsorptionFrozenWorkPacketProofLocked(homePath string, workIDs []string) osAbsorptionProofResult {
	store, err := work.Open(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "work packet authority: " + err.Error()}
	}
	for _, workID := range workIDs {
		w, getErr := store.Get(workID)
		if getErr != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s: %v", workID, getErr)}
		}
		item, proofErr := store.ProofChainForWork(workID)
		if proofErr != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s proof chain: %v", workID, proofErr)}
		}
		if validateErr := validateOSAbsorptionPacketWork(homePath, w, workID, item); validateErr != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("work packet authority %s: %v", workID, validateErr)}
		}
	}
	prerequisiteIDs := append([]string(nil), workIDs...)
	sort.Strings(prerequisiteIDs)
	return osAbsorptionProofResult{
		ok:       true,
		proofRef: osAbsorptionProofModeWorkPacket + ":frozen-custody=" + strings.Join(prerequisiteIDs, ","),
	}
}

func osAbsorptionWorkEvidenceIDs(refs []string) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		workID := ""
		switch {
		case strings.HasPrefix(ref, "work_"):
			workID = ref
		case strings.HasPrefix(ref, "work:work_"):
			workID = strings.TrimPrefix(ref, "work:")
		default:
			continue
		}
		if !safeOSAbsorptionRuntimeID(workID, "work_") {
			return nil, fmt.Errorf("invalid work evidence_ref %q", ref)
		}
		if !seen[workID] {
			seen[workID] = true
			ids = append(ids, workID)
		}
	}
	return ids, nil
}

func validateOSAbsorptionPacketWork(homePath string, w envelope.WorkEnvelope, workID string, item work.ProofChainDoctorItem) error {
	if w.ID != workID {
		return fmt.Errorf("resolved work id %q does not match requested id", w.ID)
	}
	if err := w.Validate(); err != nil {
		return fmt.Errorf("invalid WorkEnvelope: %w", err)
	}
	if w.Authority.SourceClass != "source" {
		return fmt.Errorf("authority.source_class=%q, want source", w.Authority.SourceClass)
	}
	if !strings.HasPrefix(w.CanonicalSurface, "packet:") || strings.TrimSpace(strings.TrimPrefix(w.CanonicalSurface, "packet:")) == "" {
		return fmt.Errorf("canonical_surface=%q, want non-empty packet:*", w.CanonicalSurface)
	}
	if _, err := osAbsorptionPacketEvidence(w); err != nil {
		return err
	}
	if w.State != "completed" {
		return fmt.Errorf("state=%q, want completed", w.State)
	}
	if item.WorkID != workID {
		return fmt.Errorf("canonical proof chain has no terminal item")
	}
	if !item.ProofComplete {
		return fmt.Errorf("canonical proof chain incomplete: %v", item.Problems)
	}
	if !safeOSAbsorptionRuntimeID(item.ReceiptID, "rec_") {
		return fmt.Errorf("canonical proof chain receipt_id=%q is missing or invalid", item.ReceiptID)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	record, err := store.Read(item.ReceiptID)
	if err != nil {
		return fmt.Errorf("read canonical receipt %s: %w", item.ReceiptID, err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &receipt); err != nil {
		return fmt.Errorf("decode canonical receipt %s: %w", item.ReceiptID, err)
	}
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("canonical receipt %s invalid: %w", item.ReceiptID, err)
	}
	if receipt.ID != item.ReceiptID || receipt.IDs.WorkID != workID {
		return fmt.Errorf("canonical receipt %s does not match work %s", item.ReceiptID, workID)
	}
	if receipt.Outcome != "completed" {
		return fmt.Errorf("canonical receipt %s outcome=%q, want completed", receipt.ID, receipt.Outcome)
	}
	return nil
}

func osAbsorptionPacketEvidence(w envelope.WorkEnvelope) (string, error) {
	rawRevision := strings.TrimSpace(w.Authority.SourceRevision)
	if !strings.HasPrefix(rawRevision, "sha256:") {
		return "", fmt.Errorf("authority.source_revision=%q, want sha256 packet digest", w.Authority.SourceRevision)
	}
	revision := strings.TrimPrefix(rawRevision, "sha256:")
	if len(revision) != 64 || !isOSAbsorptionHex(revision) {
		return "", fmt.Errorf("authority.source_revision=%q, want sha256 plus 64 lowercase hex characters", w.Authority.SourceRevision)
	}
	for _, ref := range w.EvidenceRefs {
		if !strings.HasPrefix(ref, osAbsorptionPacketEvidenceRef) {
			continue
		}
		digest := strings.TrimPrefix(ref, osAbsorptionPacketEvidenceRef)
		if len(digest) == 16 && isOSAbsorptionHex(digest) && strings.HasPrefix(revision, digest) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("evidence_refs lack canonical 16-character packet_sha256 digest matching authority.source_revision")
}

type osAbsorptionSourceCandidate struct {
	path  string
	proof work.PacketSourceProof
}

func osAbsorptionSourcePacketCandidates(row OSAbsorptionMatrixRow) ([]osAbsorptionSourceCandidate, int, int, error) {
	if row.SourceKind != "packet_dir" {
		return nil, 0, 0, fmt.Errorf("work packet authority source_kind=%q, want packet_dir", row.SourceKind)
	}
	rawSourceDir := strings.TrimSpace(row.SourceRef)
	if rawSourceDir == "" || !filepath.IsAbs(rawSourceDir) {
		return nil, 0, 0, fmt.Errorf("work packet authority source directory %q must be absolute", row.SourceRef)
	}
	sourceDir := filepath.Clean(rawSourceDir)
	info, err := os.Stat(sourceDir)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("work packet authority source directory %s: %v", sourceDir, err)
	}
	if !info.IsDir() {
		return nil, 0, 0, fmt.Errorf("work packet authority source directory %s is not a directory", sourceDir)
	}
	var candidates []osAbsorptionSourceCandidate
	packetFiles := 0
	parseFailures := 0
	err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			return nil
		}
		packetFiles++
		proof, err := work.PacketSourceProofForPath(path)
		if err != nil {
			parseFailures++
			return nil
		}
		candidates = append(candidates, osAbsorptionSourceCandidate{path: path, proof: proof})
		return nil
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("work packet authority source directory %s: %w", sourceDir, err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })
	return candidates, packetFiles, parseFailures, nil
}

func osAbsorptionSourcePacketWork(homePath string, store *work.Store, candidates []osAbsorptionSourceCandidate, packetFiles, parseFailures int) (string, error) {
	works, err := store.ListWithArchived()
	if err != nil {
		return "", fmt.Errorf("work packet authority list: %w", err)
	}
	knownSurfaces := map[string]bool{}
	for _, w := range works {
		if strings.HasPrefix(w.CanonicalSurface, "packet:") {
			knownSurfaces[w.CanonicalSurface] = true
		}
	}
	matchedSurfaces := 0
	for _, candidate := range candidates {
		if !knownSurfaces[candidate.proof.CanonicalSurface] {
			continue
		}
		matchedSurfaces++
		report, err := store.ActivePacketProof(time.Now(), candidate.proof.CanonicalSurface, candidate.proof.EvidenceRef)
		if err != nil {
			return "", fmt.Errorf("work packet authority source directory proof %s: %w", candidate.path, err)
		}
		if report.Status != "ok" {
			continue
		}
		w, err := store.Get(report.WorkID)
		if err != nil {
			return "", fmt.Errorf("work packet authority source work %s: %w", report.WorkID, err)
		}
		item, err := store.ProofChainForWork(report.WorkID)
		if err != nil {
			return "", fmt.Errorf("work packet authority source work %s proof chain: %w", report.WorkID, err)
		}
		if err := validateOSAbsorptionPacketWork(homePath, w, report.WorkID, item); err != nil {
			return "", fmt.Errorf("work packet authority source work %s: %w", report.WorkID, err)
		}
		return report.WorkID, nil
	}
	return "", fmt.Errorf("work packet authority source directory has no content-bound completed packet proof (packet_files=%d candidate_surfaces=%d parse_failures=%d)", packetFiles, matchedSurfaces, parseFailures)
}

func safeOSAbsorptionRuntimeID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func isOSAbsorptionHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

// applyOSAbsorptionAutoProof records one row's auto-proof outcome onto the
// doctor report/row in place, mirroring the capability-proof bookkeeping:
// proof_status=ok + proof_ref on success, proof_status=failed on failure
// (never manual, never a status-string change).
func applyOSAbsorptionAutoProof(report *OSAbsorptionDoctorReport, dr *OSAbsorptionDoctorRow, result osAbsorptionProofResult) {
	if result.skipped {
		report.SkippedProofCount++
		dr.ProofStatus = "skipped"
		dr.ProofDetail = result.reason
		return
	}
	if result.ok {
		report.AutoProofCount++
		dr.ProofStatus = "ok"
		dr.ProofRef = result.proofRef
		return
	}
	report.FailedAutoProofCount++
	dr.ProofStatus = "failed"
	dr.ProofDetail = result.reason
}

func doctorCheck(name string, ok bool, detail string) OSAbsorptionDoctorCheck {
	status := "ok"
	if !ok {
		status = "fail"
	}
	return OSAbsorptionDoctorCheck{Name: name, Status: status, Detail: detail}
}

// osAbsorptionAbsorptionCompleteCheck is the v0.2 meter's top-level gate.
// A row that reports compound_integration=retired or absorbed but still
// has live, un-retired consumers is a false-retirement claim — exactly
// the case work_b28abf9a8718 was opened for. Active/wrapper rows are
// intentionally not gated here: those are honest "still working on it"
// rows and are already covered by the existing pending_retirement_gates
// and capability_auto_proofs checks.
func osAbsorptionAbsorptionCompleteCheck(rows []OSAbsorptionMatrixRow) OSAbsorptionDoctorCheck {
	var bad []string
	for _, row := range rows {
		switch osAbsorptionRowVerdict(row) {
		case OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers,
			OSAbsorptionVerdictIntegrationAbsorbedWithLiveConsumers:
			bad = append(bad, row.ID)
		}
	}
	if len(bad) == 0 {
		return OSAbsorptionDoctorCheck{Name: "absorption_complete", Status: "ok", Detail: "no false-retirement rows"}
	}
	sort.Strings(bad)
	return OSAbsorptionDoctorCheck{
		Name:   "absorption_complete",
		Status: "fail",
		Detail: "false_retirement=" + strings.Join(bad, ","),
	}
}

// osAbsorptionNeedsInfraRegistryProof matches the os_source_infra_database
// and os_source_infra_services rows (and any row that opts in via
// proof_mode): both own a real live infra-os YAML source that can drift
// independently of the installed $COMPOUND_HOME copy, so their proof must
// re-hash the live source on every doctor run, not just check installation.
func osAbsorptionNeedsInfraRegistryProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeInfraRegistry {
		return true
	}
	return row.CompoundOwner == InfraDatabaseAuthorityRef || row.CompoundOwner == InfraServicesAuthorityRef
}

// infraRegistrySourceAuthorityRefID extracts the single required
// "source_authority:<id>" evidence_ref that pins a matrix row to one exact
// ledger record. Requiring this (rather than "any record for this
// authority_ref") means a matrix row cannot silently ride along on a later,
// unrelated re-import; the row's own evidence has to name the record.
func infraRegistrySourceAuthorityRefID(refs []string) (string, error) {
	const prefix = "source_authority:"
	id := ""
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		candidate := strings.TrimSpace(strings.TrimPrefix(ref, prefix))
		if candidate == "" {
			return "", fmt.Errorf("infra registry evidence has empty source_authority ref")
		}
		if id != "" {
			return "", fmt.Errorf("infra registry evidence has duplicate source_authority ref")
		}
		id = candidate
	}
	if id == "" {
		return "", fmt.Errorf("infra registry evidence missing source_authority ref")
	}
	return id, nil
}

// infraRegistryProof proves an infra-os operational registry row. The bool
// return is true only for the drift case: the installed registry and its
// cited ledger record agree with each other, but the live infra-os source
// file has since changed. That is reported as proof_status=pending (not
// failed, not ok) so a source-side edit surfaces as review debt instead of
// silently flipping the row to unproven with no explanation, or staying
// falsely "ok".
func infraRegistryProof(homePath string, row OSAbsorptionMatrixRow) (osAbsorptionProofResult, bool) {
	var (
		installedPath string
		loadRecords   func(string) ([]InfraRegistrySourceAuthorityRecord, error)
		label         string
	)
	switch row.CompoundOwner {
	case InfraDatabaseAuthorityRef:
		installedPath, loadRecords, label = InfraDatabaseRegistryPath(homePath), LoadInfraDatabaseSourceAuthority, "infra-database"
	case InfraServicesAuthorityRef:
		installedPath, loadRecords, label = InfraServicesRegistryPath(homePath), LoadInfraServicesSourceAuthority, "infra-services"
	default:
		return osAbsorptionProofResult{reason: fmt.Sprintf("infra registry authority: unrecognized compound_owner %q", row.CompoundOwner)}, false
	}

	evidenceID, err := infraRegistrySourceAuthorityRefID(row.EvidenceRefs)
	if err != nil {
		return osAbsorptionProofResult{reason: label + " " + err.Error()}, false
	}
	records, err := loadRecords(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("%s source authority ledger: %v", label, err)}, false
	}
	var rec *InfraRegistrySourceAuthorityRecord
	for i := range records {
		if records[i].ID == evidenceID {
			rec = &records[i]
			break
		}
	}
	if rec == nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("%s evidence ref source_authority:%s does not resolve to a ledger record", label, evidenceID)}, false
	}
	if rec.AuthorityRef != row.CompoundOwner {
		return osAbsorptionProofResult{reason: fmt.Sprintf("%s ledger record %s authority_ref %q differs from row compound_owner %q", label, rec.ID, rec.AuthorityRef, row.CompoundOwner)}, false
	}

	installedBody, err := os.ReadFile(installedPath)
	if err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("installed %s registry: %v", label, err)}, false
	}
	installedRevision := "sha256:" + digestBytes(installedBody)
	if installedRevision != rec.SourceRevision {
		return osAbsorptionProofResult{reason: fmt.Sprintf("%s installed registry sha256 %s does not match ledger record %s revision %s (re-import)", label, installedRevision, rec.ID, rec.SourceRevision)}, false
	}

	liveSourcePath := strings.TrimSpace(row.SourceRef)
	if liveSourcePath == "" {
		return osAbsorptionProofResult{reason: label + " row missing source_ref"}, false
	}
	liveBody, err := os.ReadFile(liveSourcePath)
	frozenCustody := false
	if os.IsNotExist(err) {
		custody := inspectLegacySourceFamilyCustody(homePath, "infra", filepath.Dir(liveSourcePath))
		if !custody.CustodyComplete || !legacySourceLifecycleFrozen(custody.LifecycleState) {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"%s live source unavailable without frozen complete infra custody: status=%s lifecycle=%s error=%s",
				label,
				custody.Status,
				custody.LifecycleState,
				custody.Error,
			)}, false
		}
		frozenCustody = true
	} else if err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("%s live source %s: %v", label, liveSourcePath, err)}, false
	} else {
		liveRevision := "sha256:" + digestBytes(liveBody)
		if liveRevision != rec.SourceRevision {
			return osAbsorptionProofResult{reason: fmt.Sprintf("%s live source %s sha256 %s has drifted from imported record %s revision %s; re-run authority import-%s to refresh", label, liveSourcePath, liveRevision, rec.ID, rec.SourceRevision, label)}, true
		}
	}
	sourceProof := "live-source"
	if frozenCustody {
		sourceProof = "frozen-custody"
	}

	return osAbsorptionProofResult{
		ok: true,
		proofRef: fmt.Sprintf(
			"%s:source_authority=%s;revision=%s;installed=%s;source=%s",
			osAbsorptionProofModeInfraRegistry,
			rec.ID,
			rec.SourceRevision,
			installedPath,
			sourceProof,
		),
	}, false
}

// osAbsorptionNeedsEventSourceProof matches the os_source_example_events row
// (and any row that opts in via proof_mode): its compound_owner is the real
// trace-envelope authority ref eventsource.TraceEventsAuthorityRef, and its
// source_kind marks it as an events directory rather than a single file.
func osAbsorptionNeedsEventSourceProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeEventSource {
		return true
	}
	return row.CompoundOwner == eventsource.TraceEventsAuthorityRef && row.SourceKind == "events_dir"
}

// osAbsorptionEventSourceTraceSpotCheckSample bounds how many TraceIDs from
// the most recent import-bearing record are spot-checked per file, so a
// stream with hundreds of trace ids doesn't turn every doctor run into a
// full filesystem walk.
const osAbsorptionEventSourceTraceSpotCheckSample = 5

// eventSourceAuthorityProof proves the os_source_example_events row directly
// against live eventsource.LoadSourceAuthority evidence, with zero new
// imports: every live *.ndjson file under row.SourceRef (README.md and any
// non-.ndjson file excluded) must have at least one matching source-authority
// record, and the highest historical Imported+Skipped total ever recorded for
// that file must cover the file's CURRENT non-blank line count. Because
// compound event import-os always rescans a source file from line 1 on every
// run, one run's Imported+Skipped already equals the non-blank lines it saw
// at that time; taking the max across a file's records is therefore a
// monotonically safe "best known coverage" for an append-only source (any
// lines appended after the last full pass, without a re-import, make the
// live count exceed that max and correctly report pending drift). The bool
// return is true only for that drift case, mirroring infraRegistryProof's
// (result, drift) shape; a resolvable trace-id mismatch is reported as a hard
// failure (drift=false, ok=false), never a silent pending or ok.
func eventSourceAuthorityProof(homePath string, row OSAbsorptionMatrixRow) (osAbsorptionProofResult, bool) {
	sourceDir := strings.TrimSpace(row.SourceRef)
	if sourceDir == "" || !filepath.IsAbs(sourceDir) {
		return osAbsorptionProofResult{reason: "event source authority: source_ref must be an absolute directory"}, false
	}
	entries, err := os.ReadDir(sourceDir)
	if os.IsNotExist(err) {
		return frozenEventSourceAuthorityProof(homePath, sourceDir)
	}
	if err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: source directory %s: %v", sourceDir, err)}, false
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "README.md" {
			continue
		}
		if strings.ToLower(filepath.Ext(name)) != ".ndjson" {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: no .ndjson files found under %s", sourceDir)}, false
	}

	records, err := eventsource.LoadSourceAuthority(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "event source authority: " + err.Error()}, false
	}
	byFile := map[string][]eventsource.SourceAuthorityRecord{}
	for _, rec := range records {
		recPath := strings.TrimSpace(rec.SourcePath)
		if filepath.Dir(recPath) != sourceDir {
			continue
		}
		base := filepath.Base(recPath)
		byFile[base] = append(byFile[base], rec)
	}

	eventStore, err := eventstore.Open(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "event source authority: " + err.Error()}, false
	}
	var proofRefs []string
	for _, name := range files {
		fileRecords := byFile[name]
		if len(fileRecords) == 0 {
			return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: %s has no eventsource.LoadSourceAuthority record under %s", name, sourceDir)}, true
		}

		maxCoverage := 0
		var latestWithTraces eventsource.SourceAuthorityRecord
		haveLatestWithTraces := false
		var bestCoverageRecord eventsource.SourceAuthorityRecord
		for _, rec := range fileRecords {
			coverage := rec.Imported + rec.Skipped
			if coverage >= maxCoverage {
				maxCoverage = coverage
				bestCoverageRecord = rec
			}
			if len(rec.TraceIDs) == 0 {
				continue
			}
			if !haveLatestWithTraces || rec.CreatedAt > latestWithTraces.CreatedAt {
				latestWithTraces = rec
				haveLatestWithTraces = true
			}
		}

		currentLines, err := osAbsorptionNonBlankLineCount(filepath.Join(sourceDir, name))
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: read %s: %v", name, err)}, false
		}
		if maxCoverage < currentLines {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"event source authority: %s has %d line(s) appended since import (best recorded imported+skipped=%d, current non-blank lines=%d); re-run `compound event import-os` to refresh",
				name, currentLines-maxCoverage, maxCoverage, currentLines,
			)}, true
		}

		if haveLatestWithTraces {
			sample := latestWithTraces.TraceIDs
			if len(sample) > osAbsorptionEventSourceTraceSpotCheckSample {
				sample = sample[:osAbsorptionEventSourceTraceSpotCheckSample]
			}
			for _, traceID := range sample {
				exists, err := eventStore.Exists(traceID)
				if err != nil {
					return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: trace %s from record %s does not resolve under event store: %v", traceID, latestWithTraces.ID, err)}, false
				}
				if !exists {
					return osAbsorptionProofResult{reason: fmt.Sprintf("event source authority: trace %s from record %s does not resolve under event store: not found", traceID, latestWithTraces.ID)}, false
				}
			}
			proofRefs = append(proofRefs, latestWithTraces.ID)
		} else {
			proofRefs = append(proofRefs, bestCoverageRecord.ID)
		}
	}

	proofRefs = mergeStrings(proofRefs)
	sort.Strings(proofRefs)
	return osAbsorptionProofResult{ok: true, proofRef: strings.Join(proofRefs, ",")}, false
}

func frozenEventSourceAuthorityProof(homePath, sourceDir string) (osAbsorptionProofResult, bool) {
	custody := inspectLegacySourceFamilyCustody(homePath, "os", filepath.Dir(sourceDir))
	if !custody.CustodyComplete || !legacySourceLifecycleFrozen(custody.LifecycleState) {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"event source authority: source directory unavailable without frozen complete OS custody: status=%s lifecycle=%s error=%s",
			custody.Status,
			custody.LifecycleState,
			custody.Error,
		)}, false
	}
	records, err := eventsource.LoadSourceAuthority(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "event source authority: " + err.Error()}, false
	}
	best := map[string]eventsource.SourceAuthorityRecord{}
	for _, record := range records {
		name := filepath.Base(strings.TrimSpace(record.SourcePath))
		if strings.ToLower(filepath.Ext(name)) != ".ndjson" ||
			record.AuthorityRef != eventsource.TraceEventsAuthorityRef ||
			record.AuthorityStatus != "compound_primary" {
			continue
		}
		current := best[name]
		if current.ID == "" ||
			record.Imported+record.Skipped > current.Imported+current.Skipped ||
			record.Imported+record.Skipped == current.Imported+current.Skipped && record.CreatedAt > current.CreatedAt {
			best[name] = record
		}
	}
	if len(best) == 0 {
		return osAbsorptionProofResult{reason: "event source authority: frozen custody has no Compound-primary event imports"}, false
	}
	eventStore, err := eventstore.Open(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "event source authority: " + err.Error()}, false
	}
	var proofRefs []string
	for _, record := range best {
		sample := record.TraceIDs
		if len(sample) > osAbsorptionEventSourceTraceSpotCheckSample {
			sample = sample[:osAbsorptionEventSourceTraceSpotCheckSample]
		}
		for _, traceID := range sample {
			exists, existsErr := eventStore.Exists(traceID)
			if existsErr != nil || !exists {
				return osAbsorptionProofResult{reason: fmt.Sprintf(
					"event source authority: frozen trace %s from record %s does not resolve: exists=%v err=%v",
					traceID,
					record.ID,
					exists,
					existsErr,
				)}, false
			}
		}
		proofRefs = append(proofRefs, record.ID)
	}
	proofRefs = mergeStrings(proofRefs)
	sort.Strings(proofRefs)
	return osAbsorptionProofResult{
		ok:       true,
		proofRef: "frozen-custody:" + strings.Join(proofRefs, ","),
	}, false
}

// osAbsorptionNonBlankLineCount counts non-blank lines in path the same way
// eventsource.ImportOSEvents does (raw := strings.TrimSpace(line); blank
// lines are skipped and never counted), so the comparison against a record's
// Imported+Skipped total is apples-to-apples.
func osAbsorptionNonBlankLineCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// osAbsorptionNeedsRegistryOwnershipProof matches already-live
// compound_primary registries whose ownership proof is "installed file
// exists + its own authority command loads it without error" rather than a
// source-hash drift check (unlike the infra registries, these rows do not
// each carry one stable, independently-editable upstream source file this
// doctor can re-hash on every run).
func osAbsorptionNeedsRegistryOwnershipProof(row OSAbsorptionMatrixRow) bool {
	if row.ProofMode == osAbsorptionProofModeRegistryOwnership {
		return true
	}
	_, ok := registryOwnershipSpecs("")[row.ID]
	return ok
}

// registryOwnershipSpec pairs one matrix row id with the installed-file path
// and Load* authority call that proves compound owns that registry. load
// returning a non-nil error (including "not installed") fails the row;
// load succeeding proves ownership regardless of what the loaded content's
// own status/health field says (see osAbsorptionProofModeRegistryOwnership).
type registryOwnershipSpec struct {
	path string
	load func() error
}

func registryOwnershipSpecs(homePath string) map[string]registryOwnershipSpec {
	return map[string]registryOwnershipSpec{
		"os_source_standing_grants": {
			path: policy.StandingGrantsPath(homePath),
			load: func() error { _, err := policy.LoadStandingGrantsFromHome(homePath); return err },
		},
		"os_source_operator_approval_frontier": {
			path: policy.OperatorApprovalFrontierPath(homePath),
			load: func() error { _, err := policy.LoadOperatorApprovalFrontierFromHome(homePath); return err },
		},
		"os_source_release_goal_holds": {
			path: policy.ReleaseGoalHoldsPath(homePath),
			load: func() error { _, err := policy.LoadReleaseGoalHoldsFromHome(homePath); return err },
		},
		"os_source_placement_rules": {
			path: policy.PlacementRulesPath(homePath),
			load: func() error { _, err := policy.LoadPlacementRulesFromHome(homePath); return err },
		},
		"os_source_promotion_criteria": {
			path: routeevidence.PromotionCriteriaPath(homePath),
			load: func() error { _, _, err := routeevidence.LoadPromotionCriteria(homePath); return err },
		},
		"os_source_model_capabilities": {
			path: routeevidence.ModelCapabilitiesPath(homePath),
			load: func() error { _, _, err := routeevidence.LoadModelCapabilitiesFromHome(homePath); return err },
		},
		"os_source_job_roster": {
			path: JobRosterAuthorityPath(homePath),
			load: func() error { _, err := LoadJobRosterAuthority(homePath); return err },
		},
	}
}

func registryOwnershipProof(homePath string, row OSAbsorptionMatrixRow) osAbsorptionProofResult {
	spec, ok := registryOwnershipSpecs(homePath)[row.ID]
	if !ok {
		return osAbsorptionProofResult{reason: fmt.Sprintf("registry ownership authority: no proof spec for row id %q", row.ID)}
	}
	if _, err := os.Stat(spec.path); err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("installed registry file: %v", err)}
	}
	if err := spec.load(); err != nil {
		return osAbsorptionProofResult{reason: fmt.Sprintf("authority command: %v", err)}
	}
	return osAbsorptionProofResult{
		ok:       true,
		proofRef: fmt.Sprintf("%s:file=%s", osAbsorptionProofModeRegistryOwnership, spec.path),
	}
}

func cleanStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
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

// OSAbsorptionDoctorSummaryCounts tallies the v0.2 meter verdict buckets
// from one doctor report. The summary line in os-absorption-doctor prints
// these alongside the legacy status-string counts so an operator can read
// the difference at a glance: complete + active_or_wrapper is what the
// legacy absorbed_count + pending_count roughly meant, while
// retired_with_live_consumers / absorbed_with_live_consumers are the
// new failure modes that the legacy single-axis meter swallowed
// (work_b28abf9a8718).
func OSAbsorptionDoctorSummaryCounts(rows []OSAbsorptionDoctorRow) (complete, retiredWithLive, absorbedWithLive, activeOrWrapper int) {
	for _, r := range rows {
		switch r.IntegrationVerdict {
		case OSAbsorptionVerdictComplete:
			complete++
		case OSAbsorptionVerdictIntegrationRetiredWithLiveConsumers:
			retiredWithLive++
		case OSAbsorptionVerdictIntegrationAbsorbedWithLiveConsumers:
			absorbedWithLive++
		case OSAbsorptionVerdictIntegrationActiveOrWrapper:
			activeOrWrapper++
		}
	}
	return complete, retiredWithLive, absorbedWithLive, activeOrWrapper
}

// OSAbsorptionDoctorMeterLine renders the summary counts as the single
// operator-facing v0.2 meter line.
func OSAbsorptionDoctorMeterLine(rows []OSAbsorptionDoctorRow) string {
	complete, retiredWithLive, absorbedWithLive, activeOrWrapper := OSAbsorptionDoctorSummaryCounts(rows)
	return fmt.Sprintf("v0.2 meter: complete=%d retired_with_live_consumers=%d absorbed_with_live_consumers=%d active_or_wrapper=%d", complete, retiredWithLive, absorbedWithLive, activeOrWrapper)
}
