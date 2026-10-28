package routeevidence

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// ProviderLedgerImportOptions controls bounded imports from append-only provider
// call ledgers. Limit reads the first N non-empty rows; Tail reads the last N.
type ProviderLedgerImportOptions struct {
	Limit int
	Tail  int
}

type ProviderLedgerImportSummary struct {
	Path       string         `json:"path"`
	RowsRead   int            `json:"rows_read"`
	Imported   int            `json:"imported"`
	FirstID    string         `json:"first_id,omitempty"`
	LastID     string         `json:"last_id,omitempty"`
	Results    []ImportResult `json:"results,omitempty"`
	EvidenceID []string       `json:"evidence_ids,omitempty"`
}

type providerLedgerLine struct {
	Number int
	Raw    string
}

type providerLedgerRow struct {
	RequestID        string   `json:"request_id"`
	RunID            string   `json:"run_id"`
	TSStart          string   `json:"ts_start"`
	TSEnd            string   `json:"ts_end"`
	Caller           string   `json:"caller"`
	Project          string   `json:"project"`
	Purpose          string   `json:"purpose"`
	Host             string   `json:"host"`
	RouteAlias       string   `json:"route_alias"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	AccountAlias     string   `json:"account_alias"`
	Transport        string   `json:"transport"`
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	ReasoningTokens  int      `json:"reasoning_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd"`
	ActualCostUSD    *float64 `json:"actual_cost_usd"`
	LatencyMS        int64    `json:"latency_ms"`
	Status           string   `json:"status"`
	ErrorClass       string   `json:"error_class"`
	MetadataQuality  string   `json:"metadata_quality"`
	ArtifactHashes   []string `json:"artifact_hashes"`
}

// ImportProviderLedgerFile adapts generic provider-call NDJSON rows into
// metadata-only ExecutionRouteEvidence. It intentionally drops source-specific
// row payloads and never mutates gateway/provider/model-router config.
func ImportProviderLedgerFile(homePath, path string, opts ProviderLedgerImportOptions) (ProviderLedgerImportSummary, error) {
	lines, rowsRead, err := collectProviderLedgerLines(path, opts)
	if err != nil {
		return ProviderLedgerImportSummary{}, err
	}
	summary := ProviderLedgerImportSummary{Path: path, RowsRead: rowsRead}
	for _, line := range lines {
		ev, err := ProviderLedgerRowToEvidence(line.Raw, line.Number)
		if err != nil {
			return ProviderLedgerImportSummary{}, err
		}
		result, err := RecordJoined(homePath, ev)
		if err != nil {
			return ProviderLedgerImportSummary{}, err
		}
		summary.Results = append(summary.Results, result)
		summary.EvidenceID = append(summary.EvidenceID, result.Evidence.ID)
		summary.Imported++
		if summary.FirstID == "" {
			summary.FirstID = result.Evidence.ID
		}
		summary.LastID = result.Evidence.ID
	}
	return summary, nil
}

func ProviderLedgerRowToEvidence(raw string, lineNumber int) (Evidence, error) {
	row, err := decodeProviderLedgerRow(raw, lineNumber)
	if err != nil {
		return Evidence{}, err
	}
	startedAt, err := time.Parse(time.RFC3339, row.TSStart)
	if err != nil {
		return Evidence{}, fmt.Errorf("provider ledger line %d: ts_start invalid: %w", lineNumber, err)
	}
	return Build(BuildInput{
		CreatedAt:     startedAt,
		RequestID:     strings.TrimSpace(row.RequestID),
		ClientProfile: providerLedgerClientProfile(row),
		Protocol:      firstNonEmpty(row.Transport, "provider-call-ledger"),
		Provider:      firstNonEmpty(row.Provider, "unknown-provider"),
		Model:         firstNonEmpty(row.Model, "unknown-model"),
		Usage: Usage{
			PromptTokens:     nonNegative(row.InputTokens) + nonNegative(row.CacheReadTokens) + nonNegative(row.CacheWriteTokens),
			CompletionTokens: nonNegative(row.OutputTokens) + nonNegative(row.ReasoningTokens),
			TotalTokens:      providerLedgerTotalTokens(row),
		},
		CostMicros:   providerLedgerCostMicros(row),
		LatencyMS:    row.LatencyMS,
		Status:       firstNonEmpty(row.Status, "unknown"),
		EvidenceRefs: providerLedgerEvidenceRefs(row, raw),
	})
}

func collectProviderLedgerLines(path string, opts ProviderLedgerImportOptions) ([]providerLedgerLine, int, error) {
	if opts.Limit < 0 || opts.Tail < 0 {
		return nil, 0, fmt.Errorf("limit and tail must be non-negative")
	}
	if opts.Limit > 0 && opts.Tail > 0 {
		return nil, 0, fmt.Errorf("use --limit or --tail, not both")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read provider ledger %s: %w", path, err)
	}
	defer f.Close()

	var selected []providerLedgerLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNumber := 0
	nonEmpty := 0
	for scanner.Scan() {
		lineNumber++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		nonEmpty++
		line := providerLedgerLine{Number: lineNumber, Raw: raw}
		switch {
		case opts.Tail > 0:
			if len(selected) < opts.Tail {
				selected = append(selected, line)
			} else {
				copy(selected, selected[1:])
				selected[len(selected)-1] = line
			}
		default:
			selected = append(selected, line)
			if opts.Limit > 0 && len(selected) >= opts.Limit {
				return selected, nonEmpty, scanner.Err()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nonEmpty, fmt.Errorf("scan provider ledger %s: %w", path, err)
	}
	return selected, nonEmpty, nil
}

func providerLedgerClientProfile(row providerLedgerRow) string {
	parts := []string{"provider-ledger"}
	for _, part := range []string{row.Caller, row.Project, row.RouteAlias, row.Purpose} {
		if p := strings.TrimSpace(part); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ":")
}

func providerLedgerEvidenceRefs(row providerLedgerRow, raw string) []string {
	refs := []string{
		"provider-ledger-request:" + strings.TrimSpace(row.RequestID),
		"provider-ledger-row:" + shortHash(raw),
	}
	if row.RunID != "" {
		refs = append(refs, "provider-ledger-run:"+strings.TrimSpace(row.RunID))
	}
	if row.RouteAlias != "" {
		refs = append(refs, "provider-ledger-route:"+strings.TrimSpace(row.RouteAlias))
	}
	for _, hash := range row.ArtifactHashes {
		if h := strings.TrimSpace(hash); h != "" {
			refs = append(refs, "artifact:"+h)
		}
	}
	return refs
}

func providerLedgerTotalTokens(row providerLedgerRow) int {
	return nonNegative(row.InputTokens) +
		nonNegative(row.OutputTokens) +
		nonNegative(row.ReasoningTokens) +
		nonNegative(row.CacheReadTokens) +
		nonNegative(row.CacheWriteTokens)
}

func providerLedgerCostMicros(row providerLedgerRow) int64 {
	if row.ActualCostUSD != nil {
		return int64(math.Round(*row.ActualCostUSD * 1_000_000))
	}
	if row.EstimatedCostUSD != nil {
		return int64(math.Round(*row.EstimatedCostUSD * 1_000_000))
	}
	return 0
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
