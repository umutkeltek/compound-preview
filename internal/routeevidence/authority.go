package routeevidence

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	RouteEvidenceAuthorityRef          = "compound://ledger/execution-route-evidence"
	RouteEvidenceAuthorityReportSchema = "compound.route_evidence_authority_report.v0.1"
)

type AuthorityReport struct {
	SchemaVersion               string            `json:"schema_version"`
	Status                      string            `json:"status"`
	Installed                   bool              `json:"installed"`
	AuthorityRef                string            `json:"authority_ref"`
	AuthorityPath               string            `json:"authority_path"`
	RouteEvidenceCount          int               `json:"route_evidence_count"`
	ProviderLedgerEvidenceCount int               `json:"provider_ledger_evidence_count"`
	TokenUsageRows              int               `json:"token_usage_rows"`
	CostKnownRows               int               `json:"cost_known_rows"`
	TotalTokens                 int               `json:"total_tokens"`
	TotalCostMicros             int64             `json:"total_cost_micros"`
	StatusCounts                map[string]int    `json:"status_counts"`
	ProviderCounts              map[string]int    `json:"provider_counts"`
	ProtocolCounts              map[string]int    `json:"protocol_counts"`
	Recent                      []EvidenceSummary `json:"recent"`
	SourceRole                  string            `json:"source_role"`
	FallbackRole                string            `json:"fallback_role"`
}

type EvidenceSummary struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	RequestID     string `json:"request_id"`
	WorkID        string `json:"work_id,omitempty"`
	TransitionID  string `json:"transition_id,omitempty"`
	ClientProfile string `json:"client_profile"`
	Protocol      string `json:"protocol"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Status        string `json:"status"`
	TotalTokens   int    `json:"total_tokens,omitempty"`
	CostMicros    int64  `json:"cost_micros,omitempty"`
}

func ExecutionRouteEvidenceLedgerPath(homePath string) string {
	return filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
}

func LoadAuthority(homePath string, recentLimit int) (AuthorityReport, error) {
	if recentLimit < 0 {
		recentLimit = 0
	}
	path := ExecutionRouteEvidenceLedgerPath(homePath)
	report := AuthorityReport{
		SchemaVersion:  RouteEvidenceAuthorityReportSchema,
		Status:         "missing",
		Installed:      false,
		AuthorityRef:   RouteEvidenceAuthorityRef,
		AuthorityPath:  path,
		StatusCounts:   map[string]int{},
		ProviderCounts: map[string]int{},
		ProtocolCounts: map[string]int{},
		SourceRole:     "metadata_only_route_evidence",
		FallbackRole:   "provider ledgers remain source evidence until imported; route evidence is not task success",
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return AuthorityReport{}, fmt.Errorf("read route evidence authority: %w", err)
	}
	defer f.Close()

	recent := []EvidenceSummary{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		ev, err := Decode([]byte(raw))
		if err != nil {
			return AuthorityReport{}, fmt.Errorf("decode route evidence authority line %d: %w", line, err)
		}
		report.RouteEvidenceCount++
		if isProviderLedgerEvidence(ev) {
			report.ProviderLedgerEvidenceCount++
		}
		if ev.Usage.TotalTokens > 0 {
			report.TokenUsageRows++
			report.TotalTokens += ev.Usage.TotalTokens
		}
		if ev.CostMicros > 0 {
			report.CostKnownRows++
			report.TotalCostMicros += ev.CostMicros
		}
		increment(report.StatusCounts, ev.Status)
		increment(report.ProviderCounts, ev.SelectedTarget.Provider)
		increment(report.ProtocolCounts, ev.Protocol)
		if recentLimit > 0 {
			recent = append(recent, summarizeEvidence(ev))
			if len(recent) > recentLimit {
				copy(recent, recent[1:])
				recent = recent[:recentLimit]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return AuthorityReport{}, fmt.Errorf("scan route evidence authority: %w", err)
	}
	report.Recent = recent
	report.Installed = report.RouteEvidenceCount > 0
	if report.Installed {
		report.Status = "ok"
	}
	sortStringIntMap(report.StatusCounts)
	sortStringIntMap(report.ProviderCounts)
	sortStringIntMap(report.ProtocolCounts)
	return report, nil
}

func summarizeEvidence(ev Evidence) EvidenceSummary {
	return EvidenceSummary{
		ID:            ev.ID,
		CreatedAt:     ev.CreatedAt,
		RequestID:     ev.RequestID,
		WorkID:        ev.WorkID,
		TransitionID:  ev.TransitionID,
		ClientProfile: ev.ClientProfile,
		Protocol:      ev.Protocol,
		Provider:      ev.SelectedTarget.Provider,
		Model:         ev.SelectedTarget.Model,
		Status:        ev.Status,
		TotalTokens:   ev.Usage.TotalTokens,
		CostMicros:    ev.CostMicros,
	}
}

func isProviderLedgerEvidence(ev Evidence) bool {
	if strings.HasPrefix(ev.ClientProfile, "provider-ledger") || ev.Protocol == "provider-call-ledger" || ev.Protocol == "switchback_usage_log" {
		return true
	}
	for _, ref := range ev.EvidenceRefs {
		if strings.HasPrefix(ref, "provider-ledger-") {
			return true
		}
	}
	return false
}

func increment(m map[string]int, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "unknown"
	}
	m[key]++
}

func sortStringIntMap(m map[string]int) {
	if len(m) == 0 {
		return
	}
	type row struct {
		Key   string
		Value int
	}
	rows := make([]row, 0, len(m))
	for k, v := range m {
		rows = append(rows, row{Key: k, Value: v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Value == rows[j].Value {
			return rows[i].Key < rows[j].Key
		}
		return rows[i].Value > rows[j].Value
	})
	clear(m)
	for _, row := range rows {
		m[row.Key] = row.Value
	}
}

func MarshalAuthorityReport(report AuthorityReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
