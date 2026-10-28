// Package retrievalparity owns retrieval-traffic evidence and parity artifact
// schemas. It is a data-layer leaf: production retrieval remains in its existing
// consumers, while this package records and compares their result references.
package retrievalparity

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
)

const (
	JournalSchemaVersion     = "compound.retrieval_traffic.v0.2"
	OrientInputSchemaVersion = "compound.orient_input.v0.1"
	ClassifierVersion        = "consumer-token-count.v0"

	ConsumerOrient = "orient"
	ConsumerRecall = "recall"
	ConsumerQuery  = "query"
	ConsumerSearch = "search"
)

// OrientInput is the exact production input tuple whose retrieval behavior is
// being observed. It is deliberately allowed only on COMPOUND_HOME-local
// journal and fixture surfaces. Reports carry HashOrientInput instead.
type OrientInput struct {
	SchemaVersion string `json:"schema_version"`
	Harness       string `json:"harness"`
	Cwd           string `json:"cwd"`
	RepoKind      string `json:"repo_kind"`
	RepoKindVia   string `json:"repo_kind_via"`
	Mode          string `json:"mode"`
	Topic         string `json:"topic"`
}

type hookBreakerRecord struct {
	CreatedAt      string `json:"created_at"`
	HookName       string `json:"hook_name"`
	BreakerTripped bool   `json:"breaker_tripped"`
}

// Row is one production retrieval observation. Query is intentionally present
// only in this COMPOUND_HOME journal shape; parity reports use QuerySHA256 and
// never embed Row.
type Row struct {
	SchemaVersion     string       `json:"schema_version"`
	ClassifierVersion string       `json:"classifier_version"`
	Consumer          string       `json:"consumer"`
	QueryClass        string       `json:"query_class"`
	QuerySHA256       string       `json:"query_sha256"`
	Query             string       `json:"query"`
	OrientInput       *OrientInput `json:"orient_input,omitempty"`
	ResultRefs        []string     `json:"result_refs"`
	ResultCount       int          `json:"result_count"`
	LatencyMS         float64      `json:"latency_ms"`
	// EngineAvailable is consumer-relative: a successful designed fallback is
	// available even when an optional rented sidecar did not answer.
	EngineAvailable bool   `json:"engine_available"`
	Timestamp       string `json:"timestamp"`
}

// NewRow builds the normalized journal-only representation for one retrieval.
func NewRow(consumer, query string, resultRefs []string, latency time.Duration, engineAvailable bool, now time.Time) Row {
	if latency < 0 {
		latency = 0
	}
	refs := uniqueSorted(resultRefs)
	return Row{
		SchemaVersion:     JournalSchemaVersion,
		ClassifierVersion: ClassifierVersion,
		Consumer:          strings.TrimSpace(consumer),
		QueryClass:        ClassifyQuery(consumer, query),
		QuerySHA256:       HashQuery(query),
		Query:             query,
		ResultRefs:        refs,
		ResultCount:       len(refs),
		LatencyMS:         float64(latency) / float64(time.Millisecond),
		EngineAvailable:   engineAvailable,
		Timestamp:         now.UTC().Format(time.RFC3339Nano),
	}
}

// NewOrientRow records the exact tuple used by the live orient consumer. The
// tuple never crosses into a parity report; NewReplayRow hashes it there.
func NewOrientRow(query string, input OrientInput, resultRefs []string, latency time.Duration, engineAvailable bool, now time.Time) Row {
	row := NewRow(ConsumerOrient, query, resultRefs, latency, engineAvailable, now)
	normalized := normalizeOrientInput(input)
	row.OrientInput = &normalized
	return row
}

// ClassifyQuery is deliberately content-free: consumer plus a coarse token-count
// bucket. Changing the buckets requires a classifier-version change.
func ClassifyQuery(consumer, query string) string {
	bucket := "0"
	switch n := len(strings.Fields(query)); {
	case n == 1:
		bucket = "1"
	case n >= 2 && n <= 4:
		bucket = "2-4"
	case n >= 5 && n <= 12:
		bucket = "5-12"
	case n >= 13:
		bucket = "13+"
	}
	return strings.TrimSpace(consumer) + "/tokens:" + bucket
}

// HashQuery returns the privacy-safe query reference used outside the journal.
func HashQuery(query string) string {
	sum := sha256.Sum256([]byte(query))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashOrientInput returns a deterministic identity over the full orient tuple
// and query. JSON over a fixed struct is canonical for this schema: field order
// is declaration order and strings have one unambiguous encoding.
func HashOrientInput(input OrientInput, query string) string {
	payload := struct {
		SchemaVersion string `json:"schema_version"`
		Harness       string `json:"harness"`
		Cwd           string `json:"cwd"`
		RepoKind      string `json:"repo_kind"`
		RepoKindVia   string `json:"repo_kind_via"`
		Mode          string `json:"mode"`
		Topic         string `json:"topic"`
		Query         string `json:"query"`
	}{
		SchemaVersion: normalizeOrientInput(input).SchemaVersion,
		Harness:       input.Harness,
		Cwd:           input.Cwd,
		RepoKind:      input.RepoKind,
		RepoKindVia:   input.RepoKindVia,
		Mode:          input.Mode,
		Topic:         input.Topic,
		Query:         query,
	}
	data, _ := json.Marshal(payload)
	return HashQuery(string(data))
}

// Record is a best-effort tap. A journal failure emits exactly one warning and
// cannot change the retrieval consumer's return value, exit code, or stdout.
func Record(homePath string, row Row) {
	if err := appendRow(homePath, row); err != nil {
		fmt.Fprintf(os.Stderr, "retrieval journal warning: %v\n", err)
	}
}

func appendRow(homePath string, row Row) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	if _, err := home.LoadManifest(homePath); err != nil {
		return fmt.Errorf("invalid COMPOUND_HOME: %w", err)
	}
	row = normalizeRow(row)
	if err := validateRow(row); err != nil {
		return err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	return store.AppendRetrievalTraffic(row)
}

func normalizeRow(row Row) Row {
	row.Consumer = strings.TrimSpace(row.Consumer)
	if row.SchemaVersion == "" {
		row.SchemaVersion = JournalSchemaVersion
	}
	if row.ClassifierVersion == "" {
		row.ClassifierVersion = ClassifierVersion
	}
	if row.QueryClass == "" {
		row.QueryClass = ClassifyQuery(row.Consumer, row.Query)
	}
	if row.QuerySHA256 == "" {
		row.QuerySHA256 = HashQuery(row.Query)
	}
	if row.OrientInput != nil {
		normalized := normalizeOrientInput(*row.OrientInput)
		row.OrientInput = &normalized
	}
	row.ResultRefs = uniqueSorted(row.ResultRefs)
	row.ResultCount = len(row.ResultRefs)
	return row
}

func normalizeOrientInput(input OrientInput) OrientInput {
	if input.SchemaVersion == "" {
		input.SchemaVersion = OrientInputSchemaVersion
	}
	return input
}

func validateOrientInput(input OrientInput) error {
	if input.SchemaVersion != OrientInputSchemaVersion {
		return fmt.Errorf("unsupported orient input schema %q", input.SchemaVersion)
	}
	return nil
}

func validateRow(row Row) error {
	if row.SchemaVersion != JournalSchemaVersion {
		return fmt.Errorf("unsupported retrieval journal schema %q", row.SchemaVersion)
	}
	if row.ClassifierVersion != ClassifierVersion {
		return fmt.Errorf("unsupported query classifier %q", row.ClassifierVersion)
	}
	if !validConsumer(row.Consumer) {
		return fmt.Errorf("unsupported retrieval consumer %q", row.Consumer)
	}
	if row.Consumer == ConsumerOrient && row.OrientInput == nil {
		return fmt.Errorf("orient_input required for orient consumer")
	}
	if row.OrientInput != nil {
		if row.Consumer != ConsumerOrient {
			return fmt.Errorf("orient_input is only valid for orient consumer")
		}
		if err := validateOrientInput(*row.OrientInput); err != nil {
			return err
		}
	}
	if row.QueryClass != ClassifyQuery(row.Consumer, row.Query) {
		return fmt.Errorf("query_class does not match classifier")
	}
	if row.QuerySHA256 != HashQuery(row.Query) {
		return fmt.Errorf("query_sha256 does not match query")
	}
	if row.LatencyMS < 0 {
		return fmt.Errorf("latency_ms must be non-negative")
	}
	if _, err := time.Parse(time.RFC3339Nano, row.Timestamp); err != nil {
		return fmt.Errorf("timestamp must be RFC3339Nano")
	}
	return nil
}

// LoadJournal reads the strict journal surface used by tests and consumers.
func LoadJournal(homePath string) ([]Row, error) {
	rows, _, err := loadJournal(homePath, false)
	return rows, err
}

func loadJournal(homePath string, tolerateMalformed bool) ([]Row, []string, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return nil, nil, err
	}
	file, err := store.OpenRetrievalTraffic()
	if os.IsNotExist(err) {
		return []Row{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	rows := []Row{}
	gaps := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var row Row
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			if tolerateMalformed {
				gaps = append(gaps, fmt.Sprintf("malformed_journal_row:%d", lineNo))
				continue
			}
			return nil, nil, fmt.Errorf("retrieval journal row %d: %w", lineNo, err)
		}
		if err := validateRow(row); err != nil {
			if tolerateMalformed {
				gaps = append(gaps, fmt.Sprintf("invalid_journal_row:%d", lineNo))
				continue
			}
			return nil, nil, fmt.Errorf("retrieval journal row %d: %w", lineNo, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return rows, uniqueSorted(gaps), nil
}

// liveContextBreakerCount reads the hook spine's existing typed breaker
// evidence. A timed-out or failed live context build has no exact retrieval
// duration/result tuple, so parity coverage counts the gap without fabricating
// an orient observation.
func liveContextBreakerCount(homePath string, windowStart, windowEnd time.Time) (int, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return 0, err
	}
	// Read the authoritative hook_ records through the Scan seam instead of the
	// frozen hook-events.ndjson compatibility stream. This is a count, so record
	// order is irrelevant; Scan also dedups (the ndjson could double-count a
	// re-imported id), which only tightens correctness for the breaker tally.
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"hook_"}, RegularOnly: true})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			return 0, err
		}
		var record hookBreakerRecord
		if err := json.Unmarshal(rec.Bytes, &record); err != nil || !record.BreakerTripped {
			continue
		}
		if record.HookName != "SessionStart" && record.HookName != "UserPromptSubmit" {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		if err != nil || createdAt.Before(windowStart) || createdAt.After(windowEnd) {
			continue
		}
		count++
	}
	return count, nil
}

func validConsumer(consumer string) bool {
	switch consumer {
	case ConsumerOrient, ConsumerRecall, ConsumerQuery, ConsumerSearch:
		return true
	default:
		return false
	}
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
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
