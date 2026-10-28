package retrievalparity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/reviewartifact"
)

const (
	FixtureSchemaVersion    = "compound.parity_fixture.v0.3"
	ReportSchemaVersion     = "compound.parity_report.v0.3"
	CheckpointSchemaVersion = "compound.parity_checkpoint.v0.3"

	// v0.2 remains readable: this method upgrade adds optional orient
	// expectations and content-free orient evidence, both absent-normalizes to
	// "not gated". A frozen v0.2 fixture/checkpoint/report is loaded unchanged.
	FixtureSchemaVersionV02    = "compound.parity_fixture.v0.2"
	ReportSchemaVersionV02     = "compound.parity_report.v0.2"
	CheckpointSchemaVersionV02 = "compound.parity_checkpoint.v0.2"

	ReviewArea = "retrieval-parity"

	// orientCanaryMaxCount / orientCanaryMaxBytes bound the private canary fixture
	// material an orient expectation may carry. Canaries are COMPOUND_HOME-local
	// and never leave the fixture; the bounds keep a fixture from smuggling a large
	// private blob through the expectation surface.
	orientCanaryMaxCount     = 32
	orientCanaryMaxBytes     = 4096
	orientPrivateRefMaxCount = 64

	EngineOutcomeNotAttempted = "not_attempted"
	EngineOutcomeUsed         = "used"
	EngineOutcomeDegraded     = "degraded"
	EngineOutcomeRefused      = "refused"

	EngineErrorUnavailable      = "unavailable"
	EngineErrorUnsupported      = "unsupported"
	EngineErrorEmbedderMismatch = "embedder_mismatch"
	EngineErrorProtocolVersion  = "protocol_version"
	EngineErrorEngineAhead      = "engine_ahead"
	EngineErrorUnownedClaim     = "unowned_claim"
	EngineErrorRetrieval        = "retrieval_error"
)

type Coverage struct {
	WindowStart string   `json:"window_start,omitempty"`
	WindowEnd   string   `json:"window_end,omitempty"`
	KnownGaps   []string `json:"known_gaps"`
}

type NegativeControl struct {
	MaskedRef string `json:"masked_ref"`
}

type FixtureRow struct {
	Consumer        string              `json:"consumer"`
	Query           string              `json:"query"`
	OrientInput     *OrientInput        `json:"orient_input,omitempty"`
	OrientExpect    *OrientExpectations `json:"orient_expectations,omitempty"`
	ExpectedRefs    []string            `json:"expected_refs"`
	NegativeControl *NegativeControl    `json:"negative_control,omitempty"`
}

// OrientExpectations is an optional, orient-only gate on the exact ContextPack a
// replay materializes. Every field is content-free: a body budget, deterministic
// latency/token ceilings, and COMPOUND_HOME-local private fixture material used
// only to prove the wire carries zero private body bytes. Canaries and private
// body refs never leave the fixture — they are matched against the serialized
// pack and only hashes/counts/booleans are reported.
type OrientExpectations struct {
	BodyBudgetTokens int      `json:"body_budget_tokens,omitempty"`
	MaxLatencyMS     float64  `json:"max_latency_ms,omitempty"`
	MaxContextTokens int      `json:"max_context_tokens,omitempty"`
	PrivateBodyRefs  []string `json:"private_body_refs,omitempty"`
	PrivateCanaries  []string `json:"private_canaries,omitempty"`
}

type Fixture struct {
	SchemaVersion     string       `json:"schema_version"`
	ClassifierVersion string       `json:"classifier_version"`
	GeneratedAt       string       `json:"generated_at,omitempty"`
	Coverage          Coverage     `json:"coverage"`
	ControlReportRef  string       `json:"control_report_ref,omitempty"`
	Rows              []FixtureRow `json:"rows"`
}

type PrivacyCheck struct {
	RawQueryExcluded bool `json:"raw_query_excluded"`
	QueryRefsHashed  bool `json:"query_refs_hashed"`
}

type NegativeControlResult struct {
	MaskedRef string `json:"masked_ref"`
	Observed  bool   `json:"observed"`
}

type EngineDependencyEvidence struct {
	Attempted        bool   `json:"attempted"`
	RequestedEngine  string `json:"requested_engine,omitempty"`
	ObservedEngine   string `json:"observed_engine,omitempty"`
	Embedder         string `json:"embedder"`
	CorpusGeneration int64  `json:"corpus_generation"`
	Outcome          string `json:"outcome"`
	ErrorClass       string `json:"error_class,omitempty"`
}

// ReplayRow deliberately contains no raw query field. QuerySHA256 is the only
// query identity that can leave the COMPOUND_HOME-only journal/fixture surface.
type ReplayRow struct {
	Consumer         string                    `json:"consumer"`
	QueryClass       string                    `json:"query_class"`
	QuerySHA256      string                    `json:"query_sha256"`
	InputSHA256      string                    `json:"input_sha256,omitempty"`
	ExpectedRefs     []string                  `json:"expected_refs"`
	ResultRefs       []string                  `json:"result_refs"`
	ResultCount      int                       `json:"result_count"`
	LostHits         []string                  `json:"lost_hits"`
	ExtraHits        []string                  `json:"extra_hits"`
	LatencyMS        float64                   `json:"latency_ms"`
	EngineAvailable  bool                      `json:"engine_available"`
	EngineDependency *EngineDependencyEvidence `json:"engine_dependency,omitempty"`
	DependencyPassed *bool                     `json:"dependency_passed,omitempty"`
	// RuntimeGap records the exact replay-time failure classification for an
	// unavailable row. Checkpoint coverage is derived from these row facts, so
	// resumable state cannot claim an unexplained gap or lose a real one.
	RuntimeGap      string                  `json:"runtime_gap,omitempty"`
	NegativeControl *NegativeControlResult  `json:"negative_control,omitempty"`
	OrientConsumer  *OrientConsumerEvidence `json:"orient_consumer,omitempty"`
	ParityPassed    bool                    `json:"parity_passed"`
	privacySafe     bool
}

// OrientConsumerEvidence is the content-free proof that a replay row executed the
// exact sealed, persisted, body-budget-aware ContextPack materialization path a
// live `compound orient` runs. It carries no raw query, private canary, or body
// text — only the persisted pack ref, its full wire hash, deterministic byte and
// token counts, and pass/fail booleans against the fixture's optional ceilings.
//
// The pass booleans are recomputable from the raw measurements plus the echoed
// ceilings (MaxLatencyMS/MaxContextTokens/BodyBudgetTokens), so report validation
// rejects a hand-edited green verdict. PrivateBytesPassed is always present for an
// orient row and is true only when PrivateBodyBytes is exactly zero.
type OrientConsumerEvidence struct {
	ContextPackRef      string  `json:"context_pack_ref"`
	WireSHA256          string  `json:"wire_sha256"`
	SerializedBytes     int     `json:"serialized_bytes"`
	EstimatedTokens     int     `json:"estimated_tokens"`
	BodyBudgetTokens    int     `json:"body_budget_tokens"`
	BodyUsedTokens      int     `json:"body_used_tokens"`
	PrivateBodyBytes    int     `json:"private_body_bytes"`
	MeasuredLatencyMS   float64 `json:"measured_latency_ms"`
	MaxLatencyMS        float64 `json:"max_latency_ms,omitempty"`
	MaxContextTokens    int     `json:"max_context_tokens,omitempty"`
	LatencyPassed       *bool   `json:"latency_passed,omitempty"`
	TokenPassed         *bool   `json:"token_passed,omitempty"`
	BodyBudgetPassed    *bool   `json:"body_budget_passed,omitempty"`
	PrivateBytesPassed  bool    `json:"private_bytes_passed"`
	DegradationObserved bool    `json:"degradation_observed"`
}

type ClassSummary struct {
	Consumer          string  `json:"consumer"`
	QueryClass        string  `json:"query_class"`
	Rows              int     `json:"rows"`
	Passed            int     `json:"passed"`
	Failed            int     `json:"failed"`
	LostHits          int     `json:"lost_hits"`
	ExtraHits         int     `json:"extra_hits"`
	FalseInclusions   int     `json:"false_inclusions"`
	PrivateByteRows   int     `json:"private_byte_rows"`
	LatencyBudgetFail int     `json:"latency_budget_fail"`
	TokenBudgetFail   int     `json:"token_budget_fail"`
	LatencyMS         float64 `json:"latency_ms"`
}

type Report struct {
	envelope.Header
	ClassifierVersion    string         `json:"classifier_version"`
	FixtureRef           string         `json:"fixture_ref"`
	Consumer             string         `json:"consumer"`
	K                    int            `json:"k,omitempty"`
	Coverage             Coverage       `json:"coverage"`
	Rows                 []ReplayRow    `json:"rows"`
	Classes              []ClassSummary `json:"classes"`
	Privacy              PrivacyCheck   `json:"privacy_check"`
	ExpectEngine         string         `json:"expect_engine,omitempty"`
	ForbidEngine         string         `json:"forbid_engine,omitempty"`
	DependencyPassed     *bool          `json:"dependency_passed,omitempty"`
	ParityPassed         bool           `json:"parity_passed"`
	NegativeControlValid *bool          `json:"negative_control_valid,omitempty"`
}

// ReplayIdentity binds resumable progress to the exact immutable fixture and
// replay configuration. ClassifierVersion participates in both equality and
// the checkpoint path so classifier changes can never reuse stale progress.
type ReplayIdentity struct {
	FixtureRef        string `json:"fixture_ref"`
	Consumer          string `json:"consumer"`
	K                 int    `json:"k,omitempty"`
	ClassifierVersion string `json:"classifier_version"`
	ExpectEngine      string `json:"expect_engine,omitempty"`
	ForbidEngine      string `json:"forbid_engine,omitempty"`
}

// Checkpoint is private resumable replay state. SealSHA256 authenticates every
// other field against accidental or out-of-band mutation.
type Checkpoint struct {
	SchemaVersion string         `json:"schema_version"`
	Identity      ReplayIdentity `json:"identity"`
	Coverage      Coverage       `json:"coverage"`
	Completed     int            `json:"completed"`
	Rows          []ReplayRow    `json:"rows"`
	SealSHA256    string         `json:"seal_sha256"`
}

func LoadFixture(path string) (Fixture, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, "", err
	}
	var fixture Fixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Fixture{}, "", fmt.Errorf("parity fixture must contain exactly one JSON value")
	}
	fixture = normalizeFixture(fixture)
	if err := ValidateFixture(fixture); err != nil {
		return Fixture{}, "", err
	}
	return fixture, hashBytes(data), nil
}

func ValidateFixture(fixture Fixture) error {
	if !isAcceptedFixtureSchema(fixture.SchemaVersion) {
		return fmt.Errorf("unsupported parity fixture schema %q", fixture.SchemaVersion)
	}
	if fixture.ClassifierVersion != ClassifierVersion {
		return fmt.Errorf("unsupported query classifier %q", fixture.ClassifierVersion)
	}
	if len(fixture.Rows) == 0 {
		return fmt.Errorf("parity fixture is empty")
	}
	if fixture.GeneratedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, fixture.GeneratedAt); err != nil {
			return fmt.Errorf("fixture generated_at must be RFC3339Nano")
		}
	}
	if err := validateCoverage(fixture.Coverage); err != nil {
		return err
	}
	if fixture.ControlReportRef != "" && !validSHARef(fixture.ControlReportRef) {
		return fmt.Errorf("parity fixture control_report_ref invalid")
	}
	seen := map[string]bool{}
	for i, row := range fixture.Rows {
		if !validConsumer(row.Consumer) {
			return fmt.Errorf("fixture row %d: unsupported consumer %q", i+1, row.Consumer)
		}
		if strings.TrimSpace(row.Query) == "" && !consumerAllowsBlankQuery(row.Consumer) {
			return fmt.Errorf("fixture row %d: query required", i+1)
		}
		if row.Consumer == ConsumerOrient && row.OrientInput == nil {
			return fmt.Errorf("fixture row %d: orient_input required for orient consumer", i+1)
		}
		if row.OrientInput != nil {
			if row.Consumer != ConsumerOrient {
				return fmt.Errorf("fixture row %d: orient_input is only valid for orient consumer", i+1)
			}
			if err := validateOrientInput(*row.OrientInput); err != nil {
				return fmt.Errorf("fixture row %d: %w", i+1, err)
			}
		}
		for j, ref := range row.ExpectedRefs {
			if !validFixtureRef(row.Consumer, ref) {
				return fmt.Errorf("fixture row %d: expected_ref %d invalid", i+1, j+1)
			}
		}
		key := fixtureRowIdentity(row)
		if seen[key] {
			return fmt.Errorf("fixture row %d: duplicate consumer/query", i+1)
		}
		seen[key] = true
		if row.NegativeControl != nil {
			masked := strings.TrimSpace(row.NegativeControl.MaskedRef)
			if masked == "" {
				return fmt.Errorf("fixture row %d: negative_control.masked_ref required", i+1)
			}
			if !validFixtureRef(row.Consumer, masked) {
				return fmt.Errorf("fixture row %d: negative_control.masked_ref invalid", i+1)
			}
			if !contains(row.ExpectedRefs, masked) {
				return fmt.Errorf("fixture row %d: masked_ref must be an expected_ref", i+1)
			}
		}
	}
	return nil
}

func WriteFixture(homePath, name string, fixture Fixture) (string, error) {
	fixture = normalizeFixture(fixture)
	if err := ValidateFixture(fixture); err != nil {
		return "", err
	}
	return reviewartifact.WriteJSON(homePath, ReviewArea, name, fixture, "  ")
}

func normalizeFixture(fixture Fixture) Fixture {
	if fixture.ClassifierVersion == "" {
		fixture.ClassifierVersion = ClassifierVersion
	}
	fixture.GeneratedAt = strings.TrimSpace(fixture.GeneratedAt)
	fixture.ControlReportRef = strings.TrimSpace(fixture.ControlReportRef)
	fixture.Coverage.WindowStart = strings.TrimSpace(fixture.Coverage.WindowStart)
	fixture.Coverage.WindowEnd = strings.TrimSpace(fixture.Coverage.WindowEnd)
	if fixture.Coverage.KnownGaps == nil {
		fixture.Coverage.KnownGaps = []string{}
	} else {
		fixture.Coverage.KnownGaps = uniqueSorted(fixture.Coverage.KnownGaps)
	}
	for i := range fixture.Rows {
		fixture.Rows[i].Consumer = strings.TrimSpace(fixture.Rows[i].Consumer)
		if fixture.Rows[i].OrientInput != nil {
			normalized := normalizeOrientInput(*fixture.Rows[i].OrientInput)
			fixture.Rows[i].OrientInput = &normalized
		}
		if fixture.Rows[i].OrientExpect != nil {
			normalized := normalizeOrientExpectations(*fixture.Rows[i].OrientExpect)
			fixture.Rows[i].OrientExpect = &normalized
		}
		fixture.Rows[i].ExpectedRefs = uniqueSorted(fixture.Rows[i].ExpectedRefs)
		if fixture.Rows[i].NegativeControl != nil {
			fixture.Rows[i].NegativeControl.MaskedRef = strings.TrimSpace(fixture.Rows[i].NegativeControl.MaskedRef)
		}
	}
	return fixture
}

func fixtureRowIdentity(row FixtureRow) string {
	identity := HashQuery(row.Query)
	if row.Consumer == ConsumerOrient {
		identity = ""
		if row.OrientInput != nil {
			identity = HashOrientInput(*row.OrientInput, row.Query)
		}
	}
	return row.Consumer + "\x00" + identity
}

// FreezeFromJournal records a deduplicated candidate baseline. Repeated rows
// union their result refs so the door does not silently discard intermittent
// pre-intake hits; an operator labels load-bearing refs on this evidence later.
func FreezeFromJournal(homePath string, windowDays int, out, controlReportPath string, now time.Time) (Fixture, string, error) {
	windowStart, windowEnd, err := validatedFreezeWindow(windowDays, now)
	if err != nil {
		return Fixture{}, "", err
	}
	_, controlReportRef, err := loadControlReport(controlReportPath)
	if err != nil {
		return Fixture{}, "", err
	}
	rows, gaps, err := loadJournal(homePath, true)
	if err != nil {
		return Fixture{}, "", err
	}
	breakerCount, err := liveContextBreakerCount(homePath, windowStart, windowEnd)
	if err != nil {
		return Fixture{}, "", fmt.Errorf("read live hook breaker evidence: %w", err)
	}
	if breakerCount > 0 {
		gaps = append(gaps, fmt.Sprintf("live_hook_context_breaker:%d", breakerCount))
	}

	type aggregate struct {
		consumer    string
		query       string
		orientInput *OrientInput
		refs        []string
	}
	groups := map[string]*aggregate{}
	seenConsumers := map[string]bool{}
	observedIdentities := map[string]string{}
	successfulIdentities := map[string]bool{}
	unavailableCounts := map[string]int{}
	var earliest time.Time
	for _, row := range rows {
		ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil {
			gaps = append(gaps, "journal_timestamp_invalid")
			continue
		}
		if ts.Before(windowStart) || ts.After(windowEnd) {
			continue
		}
		if strings.TrimSpace(row.Query) == "" && !consumerAllowsBlankQuery(row.Consumer) {
			gaps = append(gaps, "journal_query_missing:"+row.Consumer)
			continue
		}
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
		seenConsumers[row.Consumer] = true
		identityRef := row.QuerySHA256
		if row.Consumer == ConsumerOrient {
			if row.OrientInput == nil {
				gaps = append(gaps, "orient_input_unavailable")
				continue
			}
			identityRef = HashOrientInput(*row.OrientInput, row.Query)
		}
		key := row.Consumer + "\x00" + identityRef
		observedIdentities[key] = row.Consumer + ":" + identityRef
		if !row.EngineAvailable {
			unavailableCounts[row.Consumer]++
			continue
		}
		successfulIdentities[key] = true
		group := groups[key]
		if group == nil {
			group = &aggregate{consumer: row.Consumer, query: row.Query}
			if row.OrientInput != nil {
				input := *row.OrientInput
				group.orientInput = &input
			}
			groups[key] = group
		} else if group.query != row.Query {
			gaps = append(gaps, "query_hash_collision:"+identityRef)
			continue
		}
		group.refs = append(group.refs, row.ResultRefs...)
	}
	unavailableOnly := []string{}
	for key, identity := range observedIdentities {
		if !successfulIdentities[key] {
			unavailableOnly = append(unavailableOnly, identity)
		}
	}
	if len(unavailableOnly) > 0 {
		sort.Strings(unavailableOnly)
		return Fixture{}, "", fmt.Errorf("retrieval identity %s has no successful observation", unavailableOnly[0])
	}
	if len(groups) == 0 {
		return Fixture{}, "", fmt.Errorf("no retrieval traffic in requested window")
	}
	if earliest.After(windowStart.Add(time.Hour)) {
		gaps = append(gaps, "observed_window_shorter_than_requested")
	}
	for _, consumer := range []string{ConsumerOrient, ConsumerRecall, ConsumerQuery, ConsumerSearch} {
		if !seenConsumers[consumer] {
			gaps = append(gaps, "consumer_no_traffic:"+consumer)
		}
		if unavailableCounts[consumer] > 0 {
			gaps = append(gaps, fmt.Sprintf("engine_unavailable:%s:%d", consumer, unavailableCounts[consumer]))
		}
	}

	fixture := Fixture{
		SchemaVersion:     FixtureSchemaVersion,
		ClassifierVersion: ClassifierVersion,
		GeneratedAt:       windowEnd.Format(time.RFC3339Nano),
		Coverage: Coverage{
			WindowStart: windowStart.Format(time.RFC3339Nano),
			WindowEnd:   windowEnd.Format(time.RFC3339Nano),
			KnownGaps:   uniqueSorted(gaps),
		},
		ControlReportRef: controlReportRef,
		Rows:             []FixtureRow{},
	}
	for _, group := range groups {
		fixture.Rows = append(fixture.Rows, FixtureRow{
			Consumer:     group.consumer,
			Query:        group.query,
			OrientInput:  group.orientInput,
			ExpectedRefs: uniqueSorted(group.refs),
		})
	}
	sort.Slice(fixture.Rows, func(i, j int) bool {
		if fixture.Rows[i].Consumer != fixture.Rows[j].Consumer {
			return fixture.Rows[i].Consumer < fixture.Rows[j].Consumer
		}
		return fixtureRowIdentity(fixture.Rows[i]) < fixtureRowIdentity(fixture.Rows[j])
	})
	name, err := freezeOutputName(out, fixture)
	if err != nil {
		return Fixture{}, "", err
	}
	path, err := WriteFixture(homePath, name, fixture)
	if err != nil {
		return Fixture{}, "", err
	}
	return fixture, path, nil
}

func validatedFreezeWindow(windowDays int, now time.Time) (time.Time, time.Time, error) {
	if windowDays <= 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("window-days must be positive")
	}
	windowEnd := now.UTC()
	if err := validateRFC3339Endpoint("window_end", windowEnd); err != nil {
		return time.Time{}, time.Time{}, err
	}
	const day = 24 * time.Hour
	if int64(windowDays) > math.MaxInt64/int64(day) {
		return time.Time{}, time.Time{}, fmt.Errorf("window_start is outside representable duration")
	}
	windowStart := windowEnd.Add(-time.Duration(int64(windowDays) * int64(day)))
	if err := validateRFC3339Endpoint("window_start", windowStart); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if windowStart.After(windowEnd) {
		return time.Time{}, time.Time{}, fmt.Errorf("window_start must not follow window_end")
	}
	return windowStart, windowEnd, nil
}

func validateRFC3339Endpoint(name string, value time.Time) error {
	text := value.Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !parsed.Equal(value) {
		return fmt.Errorf("%s must be representable as RFC3339Nano", name)
	}
	return nil
}

func loadControlReport(path string) (Report, string, error) {
	if strings.TrimSpace(path) == "" {
		return Report{}, "", fmt.Errorf("control-report required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, "", fmt.Errorf("read control report: %w", err)
	}
	var report Report
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Report{}, "", fmt.Errorf("decode control report: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Report{}, "", fmt.Errorf("control report must contain exactly one JSON value")
	}
	if err := validateReport(report); err != nil {
		return Report{}, "", fmt.Errorf("invalid control report: %w", err)
	}
	if report.NegativeControlValid == nil || !*report.NegativeControlValid {
		return Report{}, "", fmt.Errorf("control report must contain a valid negative control")
	}
	return report, hashBytes(data), nil
}

// NewReplayIdentity binds checkpoint state to an immutable fixture and the
// exact replay configuration.
func NewReplayIdentity(fixtureRef, consumer string, k int) ReplayIdentity {
	return ReplayIdentity{
		FixtureRef:        strings.TrimSpace(fixtureRef),
		Consumer:          strings.TrimSpace(consumer),
		K:                 k,
		ClassifierVersion: ClassifierVersion,
	}
}

func NewReplayIdentityWithDependency(fixtureRef, consumer string, k int, expectEngine, forbidEngine string) (ReplayIdentity, error) {
	identity := NewReplayIdentity(fixtureRef, consumer, k)
	identity.ExpectEngine = strings.TrimSpace(expectEngine)
	identity.ForbidEngine = strings.TrimSpace(forbidEngine)
	identity = normalizeReplayIdentity(identity)
	if err := validateEngineDependencyContract(identity.ExpectEngine, identity.ForbidEngine); err != nil {
		return ReplayIdentity{}, err
	}
	return identity, nil
}

// CheckpointPath is deterministic over every replay identity field, including
// the classifier version.
func CheckpointPath(homePath string, identity ReplayIdentity) string {
	identity = normalizeReplayIdentity(identity)
	data, _ := json.Marshal(identity)
	digest := strings.TrimPrefix(hashBytes(data), "sha256:")
	return filepath.Join(homePath, "reviews", ReviewArea, "checkpoints", "checkpoint_"+digest[:16]+".json")
}

func NewCheckpoint(identity ReplayIdentity, coverage Coverage) Checkpoint {
	return Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		Identity:      normalizeReplayIdentity(identity),
		Coverage:      normalizeCoverage(coverage),
		Rows:          []ReplayRow{},
	}
}

func LoadCheckpoint(path string, expected ReplayIdentity) (Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, err
	}
	var checkpoint Checkpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("decode parity checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Checkpoint{}, fmt.Errorf("parity checkpoint must contain exactly one JSON value")
	}
	providedSeal := checkpoint.SealSHA256
	if !validSHARef(providedSeal) || checkpointSeal(checkpoint) != providedSeal {
		return Checkpoint{}, fmt.Errorf("parity checkpoint tampered: content seal mismatch")
	}
	expected = normalizeReplayIdentity(expected)
	if checkpoint.Identity.ExpectEngine != expected.ExpectEngine || checkpoint.Identity.ForbidEngine != expected.ForbidEngine {
		return Checkpoint{}, fmt.Errorf("parity checkpoint engine dependency contract does not match replay")
	}
	if checkpoint.Identity != expected {
		return Checkpoint{}, fmt.Errorf("parity checkpoint identity does not match replay")
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, err
	}
	for i := range checkpoint.Rows {
		checkpoint.Rows[i].privacySafe = true
	}
	return checkpoint, nil
}

// ValidateCheckpointPrefix proves that resumable rows replace exactly the
// selected fixture prefix they claim to have completed. The seal protects the
// checkpoint bytes; this binds those bytes back to the fixture semantics.
func ValidateCheckpointPrefix(checkpoint Checkpoint, fixtureCoverage Coverage, fixtureRows []FixtureRow) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	normalizedCheckpointCoverage := normalizeCoverage(checkpoint.Coverage)
	normalizedFixtureCoverage := normalizeCoverage(fixtureCoverage)
	if normalizedCheckpointCoverage.WindowStart != normalizedFixtureCoverage.WindowStart ||
		normalizedCheckpointCoverage.WindowEnd != normalizedFixtureCoverage.WindowEnd {
		return fmt.Errorf("parity checkpoint coverage window does not match fixture")
	}
	if checkpoint.Completed > len(fixtureRows) {
		return fmt.Errorf("parity checkpoint has more completed rows than the selected fixture")
	}
	expectedCoverage := normalizedFixtureCoverage
	for _, row := range checkpoint.Rows[:checkpoint.Completed] {
		if row.RuntimeGap != "" {
			expectedCoverage.KnownGaps = append(expectedCoverage.KnownGaps, row.RuntimeGap)
		}
	}
	expectedCoverage = normalizeCoverage(expectedCoverage)
	if !equalStringSlices(normalizedCheckpointCoverage.KnownGaps, expectedCoverage.KnownGaps) {
		return fmt.Errorf("parity checkpoint coverage gaps do not match fixture plus completed runtime evidence")
	}
	for i := 0; i < checkpoint.Completed; i++ {
		checkpointRow := checkpoint.Rows[i]
		fixtureRow := fixtureRows[i]
		if checkpointRow.Consumer != fixtureRow.Consumer ||
			checkpointRow.QueryClass != ClassifyQuery(fixtureRow.Consumer, fixtureRow.Query) ||
			checkpointRow.QuerySHA256 != HashQuery(fixtureRow.Query) {
			return fmt.Errorf("parity checkpoint row %d query identity does not match fixture", i+1)
		}
		wantInput := ""
		if fixtureRow.Consumer == ConsumerOrient && fixtureRow.OrientInput != nil {
			wantInput = HashOrientInput(*fixtureRow.OrientInput, fixtureRow.Query)
		}
		if checkpointRow.InputSHA256 != wantInput {
			return fmt.Errorf("parity checkpoint row %d input identity does not match fixture", i+1)
		}
		wantExpected := persistedRefs(fixtureRow.Consumer, uniqueSorted(fixtureRow.ExpectedRefs))
		if !equalStringSlices(checkpointRow.ExpectedRefs, wantExpected) {
			return fmt.Errorf("parity checkpoint row %d expected refs do not match fixture", i+1)
		}
		wantControl := ""
		if fixtureRow.NegativeControl != nil {
			wantControl = persistedRef(fixtureRow.Consumer, fixtureRow.NegativeControl.MaskedRef)
		}
		if (checkpointRow.NegativeControl == nil) != (wantControl == "") ||
			(checkpointRow.NegativeControl != nil && checkpointRow.NegativeControl.MaskedRef != wantControl) {
			return fmt.Errorf("parity checkpoint row %d negative control does not match fixture", i+1)
		}
	}
	return nil
}

func WriteCheckpoint(homePath string, checkpoint *Checkpoint) error {
	if checkpoint == nil {
		return fmt.Errorf("parity checkpoint required")
	}
	if _, err := home.LoadManifest(homePath); err != nil {
		return fmt.Errorf("invalid COMPOUND_HOME: %w", err)
	}
	if checkpoint.SchemaVersion == "" {
		checkpoint.SchemaVersion = CheckpointSchemaVersion
	}
	checkpoint.Identity = normalizeReplayIdentity(checkpoint.Identity)
	checkpoint.Coverage = normalizeCoverage(checkpoint.Coverage)
	if checkpoint.Rows == nil {
		checkpoint.Rows = []ReplayRow{}
	}
	if err := validateCheckpoint(*checkpoint); err != nil {
		return err
	}
	checkpoint.SealSHA256 = checkpointSeal(*checkpoint)
	path := CheckpointPath(homePath, checkpoint.Identity)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return nil
}

func normalizeReplayIdentity(identity ReplayIdentity) ReplayIdentity {
	identity.FixtureRef = strings.TrimSpace(identity.FixtureRef)
	identity.Consumer = strings.TrimSpace(identity.Consumer)
	identity.ExpectEngine = strings.TrimSpace(identity.ExpectEngine)
	identity.ForbidEngine = strings.TrimSpace(identity.ForbidEngine)
	if identity.ClassifierVersion == "" {
		identity.ClassifierVersion = ClassifierVersion
	}
	return identity
}

func validateReplayIdentity(identity ReplayIdentity) error {
	if !validSHARef(identity.FixtureRef) {
		return fmt.Errorf("parity checkpoint fixture identity invalid")
	}
	if !validConsumer(identity.Consumer) {
		return fmt.Errorf("parity checkpoint consumer identity invalid")
	}
	if identity.ClassifierVersion != ClassifierVersion {
		return fmt.Errorf("parity checkpoint classifier identity invalid")
	}
	if (identity.Consumer == ConsumerQuery || identity.Consumer == ConsumerSearch) && identity.K <= 0 {
		return fmt.Errorf("parity checkpoint k identity required for ranked consumer")
	}
	if (identity.Consumer == ConsumerOrient || identity.Consumer == ConsumerRecall) && identity.K != 0 {
		return fmt.Errorf("parity checkpoint k identity must be omitted for unranked consumer")
	}
	if err := validateEngineDependencyContract(identity.ExpectEngine, identity.ForbidEngine); err != nil {
		return err
	}
	return nil
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if !isAcceptedCheckpointSchema(checkpoint.SchemaVersion) {
		return fmt.Errorf("unsupported parity checkpoint schema %q", checkpoint.SchemaVersion)
	}
	if err := validateReplayIdentity(checkpoint.Identity); err != nil {
		return err
	}
	if err := validateCoverage(checkpoint.Coverage); err != nil {
		return err
	}
	if checkpoint.Completed < 0 || checkpoint.Completed != len(checkpoint.Rows) {
		return fmt.Errorf("parity checkpoint completed must equal len(rows)")
	}
	for i, row := range checkpoint.Rows {
		if err := validateReplayRowWithDependency(row, checkpoint.Identity.Consumer, checkpoint.Identity.ExpectEngine, checkpoint.Identity.ForbidEngine); err != nil {
			return fmt.Errorf("parity checkpoint row %d: %w", i+1, err)
		}
		if !row.EngineAvailable && row.RuntimeGap == "" {
			return fmt.Errorf("parity checkpoint row %d: runtime_gap required when engine is unavailable", i+1)
		}
	}
	return nil
}

func validateEngineDependencyContract(expectEngine, forbidEngine string) error {
	if expectEngine != "" && !validEngineIdentity(expectEngine) {
		return fmt.Errorf("expected engine identity invalid")
	}
	if forbidEngine != "" && !validEngineIdentity(forbidEngine) {
		return fmt.Errorf("forbidden engine identity invalid")
	}
	if expectEngine != "" && expectEngine == forbidEngine {
		return fmt.Errorf("expected and forbidden engine identities contradict")
	}
	return nil
}

func normalizeEngineDependencyEvidence(evidence EngineDependencyEvidence) EngineDependencyEvidence {
	evidence.RequestedEngine = strings.TrimSpace(evidence.RequestedEngine)
	evidence.ObservedEngine = strings.TrimSpace(evidence.ObservedEngine)
	evidence.Embedder = strings.TrimSpace(evidence.Embedder)
	evidence.Outcome = strings.TrimSpace(evidence.Outcome)
	evidence.ErrorClass = strings.TrimSpace(evidence.ErrorClass)
	return evidence
}

func validateEngineDependencyEvidence(evidence EngineDependencyEvidence) error {
	if evidence.RequestedEngine != "" && !validEngineIdentity(evidence.RequestedEngine) {
		return fmt.Errorf("requested engine identity invalid")
	}
	if evidence.ObservedEngine != "" && !validEngineIdentity(evidence.ObservedEngine) {
		return fmt.Errorf("observed engine identity invalid")
	}
	if evidence.Embedder != "" && !validEmbedderIdentity(evidence.Embedder) {
		return fmt.Errorf("engine embedder identity invalid")
	}
	if evidence.CorpusGeneration < 0 {
		return fmt.Errorf("engine corpus generation must be non-negative")
	}
	switch evidence.Outcome {
	case EngineOutcomeNotAttempted:
		if evidence.Attempted || evidence.ObservedEngine != "" || evidence.Embedder != "" || evidence.CorpusGeneration != 0 || evidence.ErrorClass != "" {
			return fmt.Errorf("not_attempted engine evidence is inconsistent")
		}
	case EngineOutcomeUsed:
		if !evidence.Attempted || evidence.ObservedEngine == "" || evidence.ErrorClass != "" {
			return fmt.Errorf("used engine evidence is incomplete")
		}
	case EngineOutcomeDegraded:
		if !validEngineErrorClass(evidence.ErrorClass, false) {
			return fmt.Errorf("degraded engine error class invalid")
		}
	case EngineOutcomeRefused:
		if !validEngineErrorClass(evidence.ErrorClass, true) {
			return fmt.Errorf("refused engine error class invalid")
		}
	default:
		return fmt.Errorf("engine outcome invalid")
	}
	if !evidence.Attempted && evidence.ObservedEngine != "" {
		return fmt.Errorf("unattempted engine cannot be observed")
	}
	return nil
}

func validEngineErrorClass(value string, refused bool) bool {
	if refused {
		switch value {
		case EngineErrorEmbedderMismatch, EngineErrorProtocolVersion, EngineErrorEngineAhead, EngineErrorUnownedClaim:
			return true
		}
		return false
	}
	switch value {
	case EngineErrorUnavailable, EngineErrorUnsupported, EngineErrorRetrieval:
		return true
	}
	return false
}

func validEmbedderIdentity(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func engineDependencyPasses(evidence EngineDependencyEvidence, expectEngine, forbidEngine string) bool {
	if expectEngine != "" && (evidence.Outcome != EngineOutcomeUsed || evidence.ObservedEngine != expectEngine) {
		return false
	}
	if forbidEngine != "" && (evidence.RequestedEngine == forbidEngine || evidence.ObservedEngine == forbidEngine) {
		return false
	}
	return true
}

func validEngineIdentity(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return false
	}
	return true
}

func checkpointSeal(checkpoint Checkpoint) string {
	checkpoint.SealSHA256 = ""
	data, _ := json.Marshal(checkpoint)
	return hashBytes(data)
}

func normalizeCoverage(coverage Coverage) Coverage {
	coverage.WindowStart = strings.TrimSpace(coverage.WindowStart)
	coverage.WindowEnd = strings.TrimSpace(coverage.WindowEnd)
	coverage.KnownGaps = uniqueSorted(coverage.KnownGaps)
	return coverage
}

func equalCoverage(left, right Coverage) bool {
	left = normalizeCoverage(left)
	right = normalizeCoverage(right)
	return left.WindowStart == right.WindowStart &&
		left.WindowEnd == right.WindowEnd &&
		equalStringSlices(left.KnownGaps, right.KnownGaps)
}

func freezeOutputName(out string, fixture Fixture) (string, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		data, _ := json.Marshal(fixture)
		return "fixture_" + strings.TrimPrefix(hashBytes(data), "sha256:")[:12] + ".json", nil
	}
	if filepath.IsAbs(out) {
		return "", fmt.Errorf("out must be home-relative")
	}
	clean := filepath.Clean(out)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("out must stay under reviews/%s", ReviewArea)
	}
	if filepath.Dir(clean) != "." {
		wantDir := filepath.Join("reviews", ReviewArea)
		if filepath.Dir(clean) != wantDir {
			return "", fmt.Errorf("out must stay under %s", wantDir)
		}
	}
	name := filepath.Base(clean)
	if filepath.Ext(name) != ".json" {
		return "", fmt.Errorf("out must name a .json fixture")
	}
	return name, nil
}

func NewReplayRow(row FixtureRow, resultRefs []string, latency time.Duration, engineAvailable bool, runtimeGaps ...string) ReplayRow {
	return newReplayRow(row, resultRefs, latency, engineAvailable, nil, ReplayIdentity{}, runtimeGaps...)
}

func NewReplayRowWithEngineEvidence(row FixtureRow, resultRefs []string, latency time.Duration, engineAvailable bool, evidence EngineDependencyEvidence, identity ReplayIdentity, runtimeGaps ...string) ReplayRow {
	evidence = normalizeEngineDependencyEvidence(evidence)
	return newReplayRow(row, resultRefs, latency, engineAvailable, &evidence, identity, runtimeGaps...)
}

func newReplayRow(row FixtureRow, resultRefs []string, latency time.Duration, engineAvailable bool, evidence *EngineDependencyEvidence, identity ReplayIdentity, runtimeGaps ...string) ReplayRow {
	expected := uniqueSorted(row.ExpectedRefs)
	actual := uniqueSorted(resultRefs)
	lost := difference(expected, actual)
	extra := difference(actual, expected)
	reportExpected := persistedRefs(row.Consumer, expected)
	reportActual := persistedRefs(row.Consumer, actual)
	reportLost := persistedRefs(row.Consumer, lost)
	reportExtra := persistedRefs(row.Consumer, extra)
	privacySafe := validFixtureRefs(row.Consumer, expected) && validFixtureRefs(row.Consumer, actual) &&
		validPersistedRefs(row.Consumer, reportExpected) && validPersistedRefs(row.Consumer, reportActual) &&
		validPersistedRefs(row.Consumer, reportLost) && validPersistedRefs(row.Consumer, reportExtra)
	runtimeGap := ""
	if len(runtimeGaps) == 1 {
		runtimeGap = strings.TrimSpace(runtimeGaps[0])
	} else if len(runtimeGaps) > 1 {
		runtimeGap = "invalid_multiple_runtime_gaps"
	}
	reportRow := ReplayRow{
		Consumer:        row.Consumer,
		QueryClass:      ClassifyQuery(row.Consumer, row.Query),
		QuerySHA256:     HashQuery(row.Query),
		ExpectedRefs:    reportExpected,
		ResultRefs:      reportActual,
		ResultCount:     len(reportActual),
		LostHits:        reportLost,
		ExtraHits:       reportExtra,
		LatencyMS:       float64(max(latency, 0)) / float64(time.Millisecond),
		EngineAvailable: engineAvailable,
		RuntimeGap:      runtimeGap,
		ParityPassed:    engineAvailable && len(lost) == 0 && len(extra) == 0,
		privacySafe:     privacySafe,
	}
	if evidence != nil {
		dependencyPassed := engineDependencyPasses(*evidence, identity.ExpectEngine, identity.ForbidEngine)
		reportRow.EngineDependency = evidence
		reportRow.DependencyPassed = &dependencyPassed
		reportRow.ParityPassed = reportRow.ParityPassed && dependencyPassed
		reportRow.privacySafe = reportRow.privacySafe && validateEngineDependencyEvidence(*evidence) == nil
	}
	if row.Consumer == ConsumerOrient && row.OrientInput != nil {
		reportRow.InputSHA256 = HashOrientInput(*row.OrientInput, row.Query)
		reportRow.privacySafe = reportRow.privacySafe && validSHARef(reportRow.InputSHA256)
	}
	if row.NegativeControl != nil {
		masked := strings.TrimSpace(row.NegativeControl.MaskedRef)
		persistedMasked := persistedRef(row.Consumer, masked)
		reportRow.privacySafe = reportRow.privacySafe && validFixtureRef(row.Consumer, masked) && validPersistedRef(row.Consumer, persistedMasked)
		reportRow.NegativeControl = &NegativeControlResult{
			MaskedRef: persistedMasked,
			Observed:  engineAvailable && contains(reportLost, persistedMasked),
		}
	}
	return reportRow
}

func BuildReport(fixtureRef, consumer string, k int, coverage Coverage, rows []ReplayRow, now time.Time) Report {
	return BuildReportWithIdentity(NewReplayIdentity(fixtureRef, consumer, k), coverage, rows, now)
}

func BuildReportWithIdentity(identity ReplayIdentity, coverage Coverage, rows []ReplayRow, now time.Time) Report {
	identity = normalizeReplayIdentity(identity)
	reportID := reviewartifact.ID("parity_", identity.FixtureRef, identity.Consumer, now.UTC().Format(time.RFC3339Nano))
	if identity.ExpectEngine != "" || identity.ForbidEngine != "" {
		reportID = reviewartifact.ID("parity_", identity.FixtureRef, identity.Consumer, identity.ExpectEngine, identity.ForbidEngine, now.UTC().Format(time.RFC3339Nano))
	}
	coverage.KnownGaps = uniqueSorted(coverage.KnownGaps)
	if coverage.WindowStart == "" || coverage.WindowEnd == "" {
		coverage.KnownGaps = uniqueSorted(append(coverage.KnownGaps, "fixture_window_unrecorded"))
	}
	privacySafe := allRowsPrivacySafe(rows) && validateCoverage(coverage) == nil
	report := Report{
		Header: envelope.Header{
			SchemaVersion: ReportSchemaVersion,
			ID:            reportID,
			CreatedAt:     now.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Scope:         envelope.Scope{Surface: ReviewArea},
			Authority:     envelope.Authority{SourceClass: "review", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{identity.FixtureRef},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ClassifierVersion: ClassifierVersion,
		FixtureRef:        identity.FixtureRef,
		Consumer:          identity.Consumer,
		K:                 identity.K,
		Coverage:          coverage,
		Rows:              append([]ReplayRow(nil), rows...),
		Classes:           summarizeClasses(rows),
		Privacy:           PrivacyCheck{RawQueryExcluded: privacySafe, QueryRefsHashed: allRowsHashed(rows)},
		ExpectEngine:      identity.ExpectEngine,
		ForbidEngine:      identity.ForbidEngine,
		ParityPassed:      true,
	}
	for _, row := range rows {
		if !row.ParityPassed {
			report.ParityPassed = false
			break
		}
	}
	report.DependencyPassed = rowsDependencyPassed(rows, identity.ExpectEngine, identity.ForbidEngine)
	report.NegativeControlValid = negativeControlValidity(rows)
	return report
}

func WriteReport(homePath string, report Report) (string, error) {
	if !isAcceptedReportSchema(report.SchemaVersion) {
		return "", fmt.Errorf("unsupported parity report schema %q", report.SchemaVersion)
	}
	if report.ID == "" {
		return "", fmt.Errorf("parity report id required")
	}
	if err := validateReport(report); err != nil {
		return "", err
	}
	return reviewartifact.WriteJSON(homePath, ReviewArea, report.ID+".json", report, "  ")
}

func validateReport(report Report) error {
	if !isAcceptedReportSchema(report.SchemaVersion) {
		return fmt.Errorf("unsupported parity report schema %q", report.SchemaVersion)
	}
	if strings.TrimSpace(report.ID) == "" {
		return fmt.Errorf("parity report id required")
	}
	if _, err := time.Parse(time.RFC3339Nano, report.CreatedAt); err != nil {
		return fmt.Errorf("parity report created_at must be RFC3339Nano")
	}
	if report.ClassifierVersion != ClassifierVersion {
		return fmt.Errorf("unsupported query classifier %q", report.ClassifierVersion)
	}
	if !validSHARef(report.FixtureRef) {
		return fmt.Errorf("parity report fixture_ref invalid")
	}
	if !validConsumer(report.Consumer) {
		return fmt.Errorf("parity report consumer invalid")
	}
	if err := validateEngineDependencyContract(report.ExpectEngine, report.ForbidEngine); err != nil {
		return fmt.Errorf("parity report dependency contract: %w", err)
	}
	if len(report.Rows) == 0 {
		return fmt.Errorf("parity report rows required")
	}
	if (report.Consumer == ConsumerQuery || report.Consumer == ConsumerSearch) && report.K <= 0 {
		return fmt.Errorf("parity report k required for ranked consumer")
	}
	if (report.Consumer == ConsumerOrient || report.Consumer == ConsumerRecall) && report.K != 0 {
		return fmt.Errorf("parity report k must be omitted for unranked consumer")
	}
	if err := validateCoverage(report.Coverage); err != nil {
		return err
	}
	for i, row := range report.Rows {
		if err := validateReplayRowWithDependency(row, report.Consumer, report.ExpectEngine, report.ForbidEngine); err != nil {
			return fmt.Errorf("parity report row %d: %w", i+1, err)
		}
	}
	for i, class := range report.Classes {
		if class.Consumer != report.Consumer || !validQueryClass(class.Consumer, class.QueryClass) {
			return fmt.Errorf("parity report class %d classification invalid", i+1)
		}
	}
	if !equalClassSummaries(report.Classes, summarizeClasses(report.Rows)) {
		return fmt.Errorf("parity report classes do not match rows")
	}
	if !report.Privacy.RawQueryExcluded || !report.Privacy.QueryRefsHashed || !allRowsHashed(report.Rows) {
		return fmt.Errorf("parity report privacy check failed")
	}
	if report.ParityPassed != rowsParityPassed(report.Rows) {
		return fmt.Errorf("parity report parity_passed does not match rows")
	}
	wantDependency := rowsDependencyPassed(report.Rows, report.ExpectEngine, report.ForbidEngine)
	if !equalOptionalBool(report.DependencyPassed, wantDependency) {
		return fmt.Errorf("parity report dependency_passed does not match rows")
	}
	wantControl := negativeControlValidity(report.Rows)
	if !equalOptionalBool(report.NegativeControlValid, wantControl) {
		return fmt.Errorf("parity report negative_control_valid does not match rows")
	}
	return nil
}

func rowsDependencyPassed(rows []ReplayRow, expectEngine, forbidEngine string) *bool {
	if expectEngine == "" && forbidEngine == "" {
		return nil
	}
	passed := true
	for _, row := range rows {
		if row.EngineDependency == nil || !engineDependencyPasses(*row.EngineDependency, expectEngine, forbidEngine) {
			passed = false
			break
		}
	}
	return &passed
}

func validateReplayRow(row ReplayRow, consumer string) error {
	return validateReplayRowWithDependency(row, consumer, "", "")
}

func validateReplayRowWithDependency(row ReplayRow, consumer, expectEngine, forbidEngine string) error {
	if row.Consumer != consumer || !validQueryClass(row.Consumer, row.QueryClass) {
		return fmt.Errorf("classification invalid")
	}
	if !validSHARef(row.QuerySHA256) {
		return fmt.Errorf("query_sha256 invalid")
	}
	if row.Consumer == ConsumerOrient {
		if !validSHARef(row.InputSHA256) {
			return fmt.Errorf("input_sha256 required for orient consumer")
		}
	} else if row.InputSHA256 != "" {
		return fmt.Errorf("input_sha256 is only valid for orient consumer")
	}
	for _, refs := range [][]string{row.ExpectedRefs, row.ResultRefs, row.LostHits, row.ExtraHits} {
		if !validPersistedRefs(row.Consumer, refs) {
			return fmt.Errorf("reference invalid")
		}
	}
	if !equalStringSlices(row.LostHits, difference(row.ExpectedRefs, row.ResultRefs)) {
		return fmt.Errorf("lost_hits must equal expected_refs minus result_refs")
	}
	if !equalStringSlices(row.ExtraHits, difference(row.ResultRefs, row.ExpectedRefs)) {
		return fmt.Errorf("extra_hits must equal result_refs minus expected_refs")
	}
	if row.ResultCount != len(row.ResultRefs) {
		return fmt.Errorf("result_count does not match result_refs")
	}
	if row.LatencyMS < 0 {
		return fmt.Errorf("latency_ms must be non-negative")
	}
	if row.RuntimeGap != "" {
		if row.EngineAvailable {
			return fmt.Errorf("runtime_gap is invalid when engine is available")
		}
		if !validReplayRuntimeGap(row.Consumer, row.RuntimeGap) {
			return fmt.Errorf("runtime_gap invalid for %s consumer", row.Consumer)
		}
	}
	dependencyPassed := true
	if row.EngineDependency != nil {
		if err := validateEngineDependencyEvidence(*row.EngineDependency); err != nil {
			return err
		}
		dependencyPassed = engineDependencyPasses(*row.EngineDependency, expectEngine, forbidEngine)
		if row.DependencyPassed == nil || *row.DependencyPassed != dependencyPassed {
			return fmt.Errorf("dependency_passed does not match engine evidence")
		}
	} else {
		if expectEngine != "" || forbidEngine != "" {
			return fmt.Errorf("engine dependency evidence required")
		}
		if row.DependencyPassed != nil {
			return fmt.Errorf("dependency_passed requires engine evidence")
		}
	}
	orientPassed := true
	if row.OrientConsumer != nil {
		if row.Consumer != ConsumerOrient {
			return fmt.Errorf("orient_consumer is only valid for orient consumer")
		}
		evidence, recompute, err := validateOrientConsumerEvidence(*row.OrientConsumer)
		if err != nil {
			return err
		}
		orientPassed = evidence
		_ = recompute // recompute verifies row fields echo the recomputable form
	}
	if row.ParityPassed != (row.EngineAvailable && len(row.LostHits) == 0 && len(row.ExtraHits) == 0 && dependencyPassed && orientPassed) {
		return fmt.Errorf("parity_passed does not match evidence")
	}
	if row.NegativeControl != nil {
		if !validPersistedRef(row.Consumer, row.NegativeControl.MaskedRef) ||
			row.NegativeControl.Observed != (row.EngineAvailable && contains(row.LostHits, row.NegativeControl.MaskedRef)) {
			return fmt.Errorf("negative_control invalid")
		}
	}
	return nil
}

// validateOrientConsumerEvidence re-derives every pass boolean from the raw
// measurements plus the echoed ceilings, then checks the row's stored booleans
// are consistent. The recomputation makes a hand-edited green verdict visible:
// the row cannot claim LatencyPassed=true while MeasuredLatencyMS exceeds
// MaxLatencyMS. Privacy: PrivateBodyBytes is opaque byte count; no canary text
// is loaded.
func validateOrientConsumerEvidence(evidence OrientConsumerEvidence) (passed bool, ok bool, err error) {
	if !validSHARef(evidence.WireSHA256) {
		return false, false, fmt.Errorf("orient_consumer.wire_sha256 invalid")
	}
	if !strings.HasPrefix(evidence.ContextPackRef, "ctx_") || len(evidence.ContextPackRef) > 64 {
		return false, false, fmt.Errorf("orient_consumer.context_pack_ref invalid")
	}
	if evidence.SerializedBytes <= 0 {
		return false, false, fmt.Errorf("orient_consumer.serialized_bytes must be positive")
	}
	if evidence.EstimatedTokens != (evidence.SerializedBytes+3)/4 {
		return false, false, fmt.Errorf("orient_consumer.estimated_tokens does not match serialized_bytes")
	}
	if evidence.BodyUsedTokens > evidence.BodyBudgetTokens {
		return false, false, fmt.Errorf("orient_consumer.body_used_tokens exceeds body_budget_tokens")
	}
	if evidence.MeasuredLatencyMS < 0 {
		return false, false, fmt.Errorf("orient_consumer.measured_latency_ms must be non-negative")
	}
	if !evidence.PrivateBytesPassed {
		return false, false, fmt.Errorf("orient_consumer.private_bytes_passed must be true")
	}
	if evidence.PrivateBodyBytes != 0 {
		return false, false, fmt.Errorf("orient_consumer.private_body_bytes must be zero")
	}

	wantLatencyPassed := evidence.MaxLatencyMS == 0 || evidence.MeasuredLatencyMS <= evidence.MaxLatencyMS
	wantTokenPassed := evidence.MaxContextTokens == 0 || evidence.EstimatedTokens <= evidence.MaxContextTokens
	wantBudgetPassed := evidence.BodyBudgetTokens == 0 || evidence.BodyUsedTokens <= evidence.BodyBudgetTokens

	if evidence.LatencyPassed != nil && *evidence.LatencyPassed != wantLatencyPassed {
		return false, false, fmt.Errorf("orient_consumer.latency_passed does not match measurement")
	}
	if evidence.TokenPassed != nil && *evidence.TokenPassed != wantTokenPassed {
		return false, false, fmt.Errorf("orient_consumer.token_passed does not match measurement")
	}
	if evidence.BodyBudgetPassed != nil && *evidence.BodyBudgetPassed != wantBudgetPassed {
		return false, false, fmt.Errorf("orient_consumer.body_budget_passed does not match measurement")
	}

	overall := evidence.PrivateBytesPassed && wantLatencyPassed && wantTokenPassed && wantBudgetPassed
	return overall, true, nil
}

func validReplayRuntimeGap(consumer, gap string) bool {
	switch consumer {
	case ConsumerOrient:
		return gap == "orient_retrieval_error"
	case ConsumerQuery:
		switch gap {
		case "query_engine_unavailable", "query_engine_refused", "query_retrieval_error":
			return true
		}
	}
	return false
}

func rowsParityPassed(rows []ReplayRow) bool {
	for _, row := range rows {
		if !row.ParityPassed {
			return false
		}
	}
	return true
}

func negativeControlValidity(rows []ReplayRow) *bool {
	hasControl := false
	valid := true
	for _, row := range rows {
		if row.NegativeControl == nil {
			continue
		}
		hasControl = true
		valid = valid && row.EngineAvailable && row.NegativeControl.Observed
	}
	if !hasControl {
		return nil
	}
	return &valid
}

func equalOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func summarizeClasses(rows []ReplayRow) []ClassSummary {
	byClass := map[string]*ClassSummary{}
	for _, row := range rows {
		key := row.Consumer + "\x00" + row.QueryClass
		summary := byClass[key]
		if summary == nil {
			summary = &ClassSummary{Consumer: row.Consumer, QueryClass: row.QueryClass}
			byClass[key] = summary
		}
		summary.Rows++
		if row.ParityPassed {
			summary.Passed++
		} else {
			summary.Failed++
		}
		summary.LostHits += len(row.LostHits)
		summary.ExtraHits += len(row.ExtraHits)
		if len(row.ExtraHits) > 0 {
			summary.FalseInclusions++
		}
		if row.OrientConsumer != nil {
			if row.OrientConsumer.PrivateBodyBytes > 0 {
				summary.PrivateByteRows++
			}
			if row.OrientConsumer.LatencyPassed != nil && !*row.OrientConsumer.LatencyPassed {
				summary.LatencyBudgetFail++
			}
			if row.OrientConsumer.TokenPassed != nil && !*row.OrientConsumer.TokenPassed {
				summary.TokenBudgetFail++
			}
		}
		summary.LatencyMS += row.LatencyMS
	}
	out := make([]ClassSummary, 0, len(byClass))
	for _, summary := range byClass {
		if summary.Rows > 0 {
			summary.LatencyMS /= float64(summary.Rows)
		}
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Consumer != out[j].Consumer {
			return out[i].Consumer < out[j].Consumer
		}
		return out[i].QueryClass < out[j].QueryClass
	})
	return out
}

func equalClassSummaries(left, right []ClassSummary) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func allRowsHashed(rows []ReplayRow) bool {
	for _, row := range rows {
		if !validSHARef(row.QuerySHA256) {
			return false
		}
		if row.InputSHA256 != "" && !validSHARef(row.InputSHA256) {
			return false
		}
	}
	return true
}

func allRowsPrivacySafe(rows []ReplayRow) bool {
	for _, row := range rows {
		if !row.privacySafe {
			return false
		}
	}
	return true
}

func validFixtureRefs(consumer string, refs []string) bool {
	for _, ref := range refs {
		if !validFixtureRef(consumer, ref) {
			return false
		}
	}
	return true
}

func consumerAllowsBlankQuery(consumer string) bool {
	return consumer == ConsumerOrient || consumer == ConsumerRecall
}

func validFixtureRef(consumer, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 512 || containsControl(ref) {
		return false
	}
	claimRef := strings.HasPrefix(ref, "claim://") && validClaimFixtureID(strings.TrimPrefix(ref, "claim://"))
	switch consumer {
	case ConsumerQuery:
		return claimRef
	case ConsumerOrient:
		return claimRef || validPositionRef(ref)
	case ConsumerRecall:
		return claimRef || validOpaqueID(ref)
	case ConsumerSearch:
		return validSearchChunkRef(ref)
	default:
		return false
	}
}

func persistedRefs(consumer string, refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, persistedRef(consumer, ref))
	}
	return uniqueSorted(out)
}

func persistedRef(consumer, ref string) string {
	ref = strings.TrimSpace(ref)
	if (consumer == ConsumerOrient && validPositionRef(ref)) || (consumer == ConsumerSearch && validSearchChunkRef(ref)) {
		return hashBytes([]byte(ref))
	}
	if strings.HasPrefix(ref, "claim://") {
		claimID := strings.TrimPrefix(ref, "claim://")
		if validClaimFixtureID(claimID) && !validCanonicalID(claimID, "claim_") {
			return hashBytes([]byte(ref))
		}
	}
	if consumer == ConsumerRecall && validOpaqueID(ref) && !validCanonicalID(ref, "cand_") {
		return hashBytes([]byte(ref))
	}
	return ref
}

func validClaimFixtureID(value string) bool {
	return len(value) <= 500 && validSafeID(value)
}

func validPersistedRefs(consumer string, refs []string) bool {
	for _, ref := range refs {
		if !validPersistedRef(consumer, ref) {
			return false
		}
	}
	return true
}

func validPersistedRef(consumer, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 512 || containsControl(ref) {
		return false
	}
	claimRef := strings.HasPrefix(ref, "claim://") && validCanonicalID(strings.TrimPrefix(ref, "claim://"), "claim_")
	switch consumer {
	case ConsumerQuery:
		return claimRef || validSHARef(ref)
	case ConsumerOrient:
		return claimRef || validSHARef(ref)
	case ConsumerRecall:
		return claimRef || validCanonicalID(ref, "cand_") || validSHARef(ref)
	case ConsumerSearch:
		return validSHARef(ref)
	default:
		return false
	}
}

func validCanonicalID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && validHex(strings.TrimPrefix(value, prefix), 12)
}

func validPositionRef(ref string) bool {
	const prefix = "position://"
	if !strings.HasPrefix(ref, prefix) {
		return false
	}
	payload := strings.TrimPrefix(ref, prefix)
	marker := strings.LastIndex(payload, "::slot-")
	if marker <= 0 {
		return false
	}
	return validHex(payload[marker+len("::slot-"):], 12)
}

func validOpaqueID(ref string) bool {
	if ref == "" || len(ref) > 512 || containsControl(ref) || strings.IndexFunc(ref, func(r rune) bool { return r == ' ' || r == '\t' }) >= 0 {
		return false
	}
	base := ref
	if chunk := strings.LastIndex(base, "#"); chunk >= 0 {
		if chunk == 0 || !validDecimal(base[chunk+1:], true) {
			return false
		}
		base = base[:chunk]
	}
	separator := strings.LastIndex(base, "_")
	if separator <= 0 || !validHex(base[separator+1:], 12) {
		return false
	}
	return validSafeID(base[:separator])
}

func validSearchChunkRef(ref string) bool {
	separator := strings.LastIndex(ref, "#")
	if separator <= 0 || !validDecimal(ref[separator+1:], true) {
		return false
	}
	return validSafeID(ref[:separator])
}

func validSafeID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validQueryClass(consumer, queryClass string) bool {
	prefix := consumer + "/tokens:"
	if !strings.HasPrefix(queryClass, prefix) {
		return false
	}
	switch strings.TrimPrefix(queryClass, prefix) {
	case "0", "1", "2-4", "5-12", "13+":
		return true
	default:
		return false
	}
}

func validateCoverage(coverage Coverage) error {
	startText := strings.TrimSpace(coverage.WindowStart)
	endText := strings.TrimSpace(coverage.WindowEnd)
	if (startText == "") != (endText == "") {
		return fmt.Errorf("parity coverage window_start and window_end must be paired")
	}
	if startText != "" {
		start, startErr := time.Parse(time.RFC3339Nano, startText)
		end, endErr := time.Parse(time.RFC3339Nano, endText)
		if startErr != nil || endErr != nil {
			return fmt.Errorf("parity coverage window must be RFC3339Nano")
		}
		if start.After(end) {
			return fmt.Errorf("parity coverage window_start must not follow window_end")
		}
	}
	for i, gap := range coverage.KnownGaps {
		if !validCoverageGap(gap) {
			return fmt.Errorf("parity coverage known_gap %d invalid", i+1)
		}
	}
	return nil
}

func validCoverageGap(gap string) bool {
	switch gap {
	case "observed_window_shorter_than_requested",
		"journal_timestamp_invalid",
		"fixture_window_unrecorded",
		"orient_cwd_unavailable",
		"orient_input_unavailable",
		"orient_retrieval_error",
		"query_engine_unavailable",
		"query_engine_refused",
		"query_retrieval_error":
		return true
	}
	for _, prefix := range []string{"malformed_journal_row:", "invalid_journal_row:"} {
		if strings.HasPrefix(gap, prefix) {
			return validDecimal(strings.TrimPrefix(gap, prefix), false)
		}
	}
	for _, prefix := range []string{"journal_query_missing:", "consumer_no_traffic:"} {
		if strings.HasPrefix(gap, prefix) {
			return validConsumer(strings.TrimPrefix(gap, prefix))
		}
	}
	if strings.HasPrefix(gap, "engine_unavailable:") {
		parts := strings.Split(strings.TrimPrefix(gap, "engine_unavailable:"), ":")
		return len(parts) == 2 && validConsumer(parts[0]) && validDecimal(parts[1], false)
	}
	if strings.HasPrefix(gap, "live_hook_context_breaker:") {
		return validDecimal(strings.TrimPrefix(gap, "live_hook_context_breaker:"), false)
	}
	if strings.HasPrefix(gap, "query_hash_collision:") {
		return validSHARef(strings.TrimPrefix(gap, "query_hash_collision:"))
	}
	return false
}

func validDecimal(value string, allowZero bool) bool {
	n, err := strconv.Atoi(value)
	if err != nil {
		return false
	}
	if allowZero {
		return n >= 0 && strconv.Itoa(n) == value
	}
	return n > 0 && strconv.Itoa(n) == value
}

func validSHARef(ref string) bool {
	if !strings.HasPrefix(ref, "sha256:") || len(ref) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(ref, "sha256:"))
	return err == nil
}

func difference(left, right []string) []string {
	rightSet := map[string]bool{}
	for _, value := range right {
		rightSet[value] = true
	}
	out := []string{}
	for _, value := range left {
		if !rightSet[value] {
			out = append(out, value)
		}
	}
	return uniqueSorted(out)
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// isAcceptedFixtureSchema / isAcceptedReportSchema / isAcceptedCheckpointSchema
// gate which frozen versions a load or write accepts. v0.2 is read-only: a writer
// always emits the v0.3 schema; a v0.2 frozen artifact is loaded and round-tripped
// without re-tagging the version, so a downstream tool can still inspect it.
func isAcceptedFixtureSchema(version string) bool {
	return version == FixtureSchemaVersion || version == FixtureSchemaVersionV02
}

func isAcceptedReportSchema(version string) bool {
	return version == ReportSchemaVersion || version == ReportSchemaVersionV02
}

func isAcceptedCheckpointSchema(version string) bool {
	return version == CheckpointSchemaVersion || version == CheckpointSchemaVersionV02
}

// validateOrientExpectations rejects fixture material that would smuggle raw
// queries, large canaries, or invalid private refs through the orient expectation
// surface. The values are content-free for reports: only counts, lengths, and
// hashes leave the fixture, so a fixture must not be a covert channel.
func validateOrientExpectations(expect OrientExpectations) error {
	if expect.BodyBudgetTokens < 0 {
		return fmt.Errorf("orient_expectations.body_budget_tokens must be non-negative")
	}
	if expect.MaxLatencyMS < 0 {
		return fmt.Errorf("orient_expectations.max_latency_ms must be non-negative")
	}
	if expect.MaxContextTokens < 0 {
		return fmt.Errorf("orient_expectations.max_context_tokens must be non-negative")
	}
	if len(expect.PrivateBodyRefs) > orientPrivateRefMaxCount {
		return fmt.Errorf("orient_expectations.private_body_refs exceeds %d", orientPrivateRefMaxCount)
	}
	for i, ref := range expect.PrivateBodyRefs {
		trimmed := strings.TrimSpace(ref)
		if trimmed == "" {
			return fmt.Errorf("orient_expectations.private_body_refs[%d] blank", i)
		}
		if len(trimmed) > 512 || containsControl(trimmed) {
			return fmt.Errorf("orient_expectations.private_body_refs[%d] invalid", i)
		}
	}
	if len(expect.PrivateCanaries) > orientCanaryMaxCount {
		return fmt.Errorf("orient_expectations.private_canaries exceeds %d", orientCanaryMaxCount)
	}
	for i, canary := range expect.PrivateCanaries {
		trimmed := strings.TrimSpace(canary)
		if trimmed == "" {
			return fmt.Errorf("orient_expectations.private_canaries[%d] blank", i)
		}
		if len(trimmed) > orientCanaryMaxBytes {
			return fmt.Errorf("orient_expectations.private_canaries[%d] exceeds %d bytes", i, orientCanaryMaxBytes)
		}
		if containsControl(trimmed) {
			return fmt.Errorf("orient_expectations.private_canaries[%d] contains control character", i)
		}
	}
	return nil
}

// normalizeOrientExpectations sorts and dedupes expectation lists so a
// canary/ref match in a fixture and its round-tripped form compare equal.
func normalizeOrientExpectations(expect OrientExpectations) OrientExpectations {
	expect.PrivateBodyRefs = uniqueSorted(expect.PrivateBodyRefs)
	expect.PrivateCanaries = uniqueSorted(expect.PrivateCanaries)
	return expect
}

// BuildOrientConsumerEvidence scans the exact serialized ContextPack returned
// from a live materialization and emits the content-free evidence a replay row
// needs. It is the single helper that JSON-marshals the pack once; the
// helper scans that byte slice for private canary content and configured
// private body refs. The boolean passed flags are recomputable from the raw
// measurements plus the echoed ceilings — validation rejects a hand-edited
// green verdict.
//
// Inputs:
//   - wire: the persisted-pack bytes (MarshalIndent + '\n', matching what
//     contract.RecordRuntimeMaterialization writes).
//   - contextPackID: the ctx_... id of the sealed pack.
//   - bodyBudgetTokens: the request budget; 0 if body materialization was off.
//   - bodyUsedTokens: pack.BodyPack.EstimatedUsedTokens; 0 if no body pack.
//   - latency: the measured materialization duration.
//   - privateBodyRefs: the optional configured private ref set whose full body
//     text is also a needle. Each ref's admitted body text is added to the
//     needle set only if the ref is admitted with status ok. Empty text bodies
//     contribute nothing (a needle of zero length would match everything).
//   - privateCanaries: opaque COMPOUND_HOME-local strings that must never
//     appear on the wire. Each becomes a needle.
//   - degraded: a flag the resolver sets when it swallowed an optional-source
//     failure (malformed config, lane embedder/query error). Always false for a
//     healthy configured body source.
//
// Reports/checkpoints/stdout must carry the returned struct directly — never
// the raw pack bytes.
func BuildOrientConsumerEvidence(
	wire []byte,
	contextPackID string,
	bodyBudgetTokens int,
	bodyUsedTokens int,
	latency time.Duration,
	privateBodyRefs []string,
	admittedBodyTexts map[string]string,
	privateCanaries []string,
	degraded bool,
) OrientConsumerEvidence {
	serialized := append([]byte(nil), wire...)
	hash := hashBytes(serialized)
	bytes := len(serialized)
	estimated := (bytes + 3) / 4
	measured := float64(max(latency, 0)) / float64(time.Millisecond)

	needles := make([][]byte, 0, len(privateCanaries))
	for _, canary := range privateCanaries {
		trimmed := strings.TrimSpace(canary)
		if trimmed == "" {
			continue
		}
		needles = append(needles, []byte(trimmed))
	}
	for _, ref := range uniqueSorted(privateBodyRefs) {
		text, ok := admittedBodyTexts[ref]
		if !ok {
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		needles = append(needles, []byte(text))
	}
	privateBytes := countUniqueMatchedRanges(serialized, needles)

	evidence := OrientConsumerEvidence{
		ContextPackRef:      strings.TrimSpace(contextPackID),
		WireSHA256:          hash,
		SerializedBytes:     bytes,
		EstimatedTokens:     estimated,
		BodyBudgetTokens:    bodyBudgetTokens,
		BodyUsedTokens:      bodyUsedTokens,
		PrivateBodyBytes:    privateBytes,
		MeasuredLatencyMS:   measured,
		PrivateBytesPassed:  privateBytes == 0,
		DegradationObserved: degraded,
	}
	return evidence
}

// ApplyOrientExpectations re-derives the optional latency/token/budget pass
// booleans against a fixture's expectations and stamps the echoed ceilings
// onto the evidence so it remains self-validating on a round trip.
func ApplyOrientExpectations(evidence *OrientConsumerEvidence, expect OrientExpectations) {
	if evidence == nil {
		return
	}
	evidence.MaxLatencyMS = expect.MaxLatencyMS
	evidence.MaxContextTokens = expect.MaxContextTokens
	latency := true
	token := true
	budget := true
	if expect.MaxLatencyMS > 0 {
		latency = evidence.MeasuredLatencyMS <= expect.MaxLatencyMS
		evidence.LatencyPassed = &latency
	} else {
		evidence.LatencyPassed = nil
	}
	if expect.MaxContextTokens > 0 {
		token = evidence.EstimatedTokens <= expect.MaxContextTokens
		evidence.TokenPassed = &token
	} else {
		evidence.TokenPassed = nil
	}
	if expect.BodyBudgetTokens > 0 {
		budget = evidence.BodyUsedTokens <= expect.BodyBudgetTokens
		evidence.BodyBudgetPassed = &budget
	} else {
		evidence.BodyBudgetPassed = nil
	}
}

// countUniqueMatchedRanges returns the byte length of the union of needle
// occurrences in data, with overlapping ranges counted once. An empty needle
// contributes nothing. The result is a content-free upper bound on the number
// of private bytes the wire carries — no needle text leaves this package.
func countUniqueMatchedRanges(data []byte, needles [][]byte) int {
	if len(needles) == 0 || len(data) == 0 {
		return 0
	}
	type span struct{ start, end int }
	spans := make([]span, 0, len(needles))
	for _, needle := range needles {
		n := len(needle)
		if n == 0 || n > len(data) {
			continue
		}
		for i := 0; i+n <= len(data); i++ {
			if bytes.Equal(data[i:i+n], needle) {
				spans = append(spans, span{i, i + n})
			}
		}
	}
	if len(spans) == 0 {
		return 0
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	merged := []span{spans[0]}
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
		} else {
			merged = append(merged, s)
		}
	}
	total := 0
	for _, s := range merged {
		total += s.end - s.start
	}
	return total
}
