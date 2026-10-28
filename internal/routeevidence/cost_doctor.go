package routeevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/writeback"
)

const CostDoctorSchemaVersion = "compound.route_cost_doctor.v0.1"
const RouteCostCandidateSchemaVersion = "compound.route_cost_observation.v0.1"

type CostDoctorReport struct {
	SchemaVersion           string   `json:"schema_version"`
	Path                    string   `json:"path"`
	RowsRead                int      `json:"rows_read"`
	RowsChecked             int      `json:"rows_checked"`
	TokenUsageRows          int      `json:"token_usage_rows"`
	CostKnownRows           int      `json:"cost_known_rows"`
	TokenUsageMissingCost   int      `json:"token_usage_missing_cost"`
	UnknownProjectRows      int      `json:"unknown_project_rows"`
	MissingPurposeRows      int      `json:"missing_purpose_rows"`
	PartialMetadataRows     int      `json:"partial_metadata_rows"`
	Status                  string   `json:"status"`
	EvidenceRefs            []string `json:"evidence_refs"`
	CandidateID             string   `json:"candidate_id,omitempty"`
	NoPromoteID             string   `json:"no_promote_id,omitempty"`
	RecordedCandidatePath   string   `json:"recorded_candidate_path,omitempty"`
	RecordedNoPromote       bool     `json:"recorded_no_promote,omitempty"`
	RecordNoPromoteInserted bool     `json:"record_no_promote_inserted,omitempty"`
}

type RouteCostCandidate struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Subject       string   `json:"subject"`
	Statement     string   `json:"statement"`
	Detail        string   `json:"detail,omitempty"`
	Confidence    float64  `json:"confidence"`
	Derivation    string   `json:"derivation"`
	EvidenceRefs  []string `json:"evidence_refs"`
	PolicyVersion string   `json:"policy_version"`
	Verdict       string   `json:"verdict"`
}

func DoctorProviderLedgerCosts(path string, opts ProviderLedgerImportOptions) (CostDoctorReport, error) {
	lines, rowsRead, err := collectProviderLedgerLines(path, opts)
	if err != nil {
		return CostDoctorReport{}, err
	}
	report := CostDoctorReport{
		SchemaVersion: CostDoctorSchemaVersion,
		Path:          path,
		RowsRead:      rowsRead,
		RowsChecked:   len(lines),
		Status:        "ok",
		EvidenceRefs:  []string{"provider-ledger:" + shortHash(path)},
	}
	for _, line := range lines {
		row, err := decodeProviderLedgerRow(line.Raw, line.Number)
		if err != nil {
			return CostDoctorReport{}, err
		}
		rowRef := "provider-ledger-row:" + shortHash(line.Raw)
		tokens := providerLedgerTotalTokens(row)
		hasCost := row.ActualCostUSD != nil || row.EstimatedCostUSD != nil
		rowHasGap := false
		if tokens > 0 {
			report.TokenUsageRows++
			if !hasCost {
				report.TokenUsageMissingCost++
				rowHasGap = true
			}
		}
		if hasCost {
			report.CostKnownRows++
		}
		if isUnknown(row.Project) {
			report.UnknownProjectRows++
			rowHasGap = true
		}
		if strings.TrimSpace(row.Purpose) == "" {
			report.MissingPurposeRows++
			rowHasGap = true
		}
		if q := strings.TrimSpace(row.MetadataQuality); q != "" && q != "complete" {
			report.PartialMetadataRows++
			rowHasGap = true
		}
		if rowHasGap {
			report.EvidenceRefs = append(report.EvidenceRefs, rowRef)
		}
	}
	if report.TokenUsageMissingCost > 0 || report.UnknownProjectRows > 0 || report.MissingPurposeRows > 0 || report.PartialMetadataRows > 0 {
		report.Status = "review"
	}
	report.EvidenceRefs = dedupeRefs(report.EvidenceRefs)
	return report, nil
}

func RecordCostDoctorObservation(homePath string, report CostDoctorReport, now time.Time) (CostDoctorReport, error) {
	if report.Status == "ok" {
		return report, fmt.Errorf("provider ledger cost coverage ok; no route observation needed")
	}
	cand := BuildRouteCostCandidate(report, now)
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return report, err
	}
	path := filepath.Join(dir, cand.ID+".json")
	b, err := json.MarshalIndent(&cand, "", " ")
	if err != nil {
		return report, err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return report, err
	}
	store, err := writeback.Open(homePath)
	if err != nil {
		return report, err
	}
	nop, inserted, err := store.RecordNoPromote(RouteCostNoPromote(cand, report, now), now)
	if err != nil {
		return report, err
	}
	report.CandidateID = cand.ID
	report.NoPromoteID = nop.ID
	report.RecordedCandidatePath = path
	report.RecordedNoPromote = true
	report.RecordNoPromoteInserted = inserted
	return report, nil
}

func BuildRouteCostCandidate(report CostDoctorReport, _ time.Time) RouteCostCandidate {
	subject := "provider-ledger:cost-coverage"
	statement := fmt.Sprintf(
		"provider ledger has %d token-usage row(s) without cost metadata",
		report.TokenUsageMissingCost,
	)
	if report.TokenUsageMissingCost == 0 {
		statement = "provider ledger metadata quality needs route-cost review"
	}
	detail := fmt.Sprintf(
		"rows_checked=%d token_usage_rows=%d cost_known_rows=%d unknown_project_rows=%d missing_purpose_rows=%d partial_metadata_rows=%d",
		report.RowsChecked,
		report.TokenUsageRows,
		report.CostKnownRows,
		report.UnknownProjectRows,
		report.MissingPurposeRows,
		report.PartialMetadataRows,
	)
	id := idFor("route_cost_", subject, statement, detail, strings.Join(report.EvidenceRefs, "|"))
	return RouteCostCandidate{
		SchemaVersion: RouteCostCandidateSchemaVersion,
		ID:            id,
		Kind:          string(candidates.RouteObservation),
		Subject:       subject,
		Statement:     statement,
		Detail:        detail,
		Confidence:    1,
		Derivation:    "provider_call_ledger_cost_doctor",
		EvidenceRefs:  append([]string(nil), report.EvidenceRefs...),
		PolicyVersion: "provider-ledger-cost-doctor.v0.1",
		Verdict:       candidates.NoPromote,
	}
}

func RouteCostNoPromote(cand RouteCostCandidate, report CostDoctorReport, now time.Time) writeback.NoPromoteDecision {
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CandidateRef:  cand.ID,
		OwnerTarget:   writeback.OwnerTarget{Kind: "route_cost_coverage", Ref: "provider-ledger"},
		Basis:         "provider ledger cost coverage requires review before route policy or cost allocation changes",
		EvidenceRefs:  append([]string(nil), report.EvidenceRefs...),
		Status:        writeback.NoPromoteActive,
		CreatedAt:     rfc3339(now),
	}
}

func decodeProviderLedgerRow(raw string, lineNumber int) (providerLedgerRow, error) {
	var row providerLedgerRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return providerLedgerRow{}, fmt.Errorf("decode provider ledger line %d: %w", lineNumber, err)
	}
	if strings.TrimSpace(row.RequestID) == "" {
		return providerLedgerRow{}, fmt.Errorf("provider ledger line %d: request_id required", lineNumber)
	}
	if strings.TrimSpace(row.TSStart) == "" {
		return providerLedgerRow{}, fmt.Errorf("provider ledger line %d: ts_start required", lineNumber)
	}
	if _, err := time.Parse(time.RFC3339, row.TSStart); err != nil {
		return providerLedgerRow{}, fmt.Errorf("provider ledger line %d: ts_start invalid: %w", lineNumber, err)
	}
	return row, nil
}

func isUnknown(value string) bool {
	v := strings.TrimSpace(strings.ToLower(value))
	return v == "" || v == "unknown" || v == "unknown_project"
}
