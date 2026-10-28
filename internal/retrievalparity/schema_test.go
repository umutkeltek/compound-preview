package retrievalparity

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/reviewartifact"
)

var parityTestNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

func TestAppendRowPreservesRetrievalTrafficBytes(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", parityTestNow); err != nil {
		t.Fatal(err)
	}
	row := NewRow(
		ConsumerRecall,
		"alpha beta",
		[]string{"claim://b", "claim://a"},
		1500*time.Microsecond,
		true,
		parityTestNow,
	)
	if err := appendRow(homePath, row); err != nil {
		t.Fatal(err)
	}

	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.OpenRetrievalTraffic()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"schema_version\":\"compound.retrieval_traffic.v0.2\",\"classifier_version\":\"consumer-token-count.v0\",\"consumer\":\"recall\",\"query_class\":\"recall/tokens:2-4\",\"query_sha256\":\"sha256:1a989ea86150171c687b0727f218eedbb94c4665a7da9b0add1bf5de607f2bf1\",\"query\":\"alpha beta\",\"result_refs\":[\"claim://a\",\"claim://b\"],\"result_count\":2,\"latency_ms\":1.5,\"engine_available\":true,\"timestamp\":\"2026-07-14T12:00:00Z\"}\n"
	if string(got) != want {
		t.Fatalf("retrieval traffic bytes changed\ngot:  %q\nwant: %q", got, want)
	}
}

func TestOrientInputStaysHomeLocalAndReportCarriesOnlyItsHash(t *testing.T) {
	input := OrientInput{
		Harness:     "claude_code",
		Cwd:         "/workspace/synthetic-library",
		RepoKind:    "library",
		RepoKindVia: "flag",
		Mode:        "review",
		Topic:       "topic:synthetic",
	}
	query := "preserve the exact orient tuple"
	journalRow := NewOrientRow(query, input, []string{"claim://claim_aaaaaaaaaaaa"}, time.Millisecond, true, parityTestNow)
	if journalRow.OrientInput == nil || *journalRow.OrientInput != normalizeOrientInput(input) {
		t.Fatalf("journal orient input = %+v", journalRow.OrientInput)
	}

	replay := NewReplayRow(FixtureRow{
		Consumer:     ConsumerOrient,
		Query:        query,
		OrientInput:  &input,
		ExpectedRefs: journalRow.ResultRefs,
	}, journalRow.ResultRefs, time.Millisecond, true)
	want := HashOrientInput(input, query)
	if replay.InputSHA256 != want {
		t.Fatalf("input hash = %q, want %q", replay.InputSHA256, want)
	}
	persisted, err := json.Marshal(replay)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{input.Cwd, input.Harness, input.Topic, query} {
		if strings.Contains(string(persisted), raw) {
			t.Fatalf("report row leaked raw orient input %q: %s", raw, persisted)
		}
	}
	changed := input
	changed.Topic = "topic:other"
	if HashOrientInput(changed, query) == want {
		t.Fatal("topic did not participate in orient input identity")
	}
}

func TestOrientJournalRequiresExactInputIdentity(t *testing.T) {
	homePath := newParityPackageHome(t)
	row := NewRow(ConsumerOrient, "missing exact input", nil, time.Millisecond, true, parityTestNow)
	if err := appendRow(homePath, row); err == nil || !strings.Contains(err.Error(), "orient_input") {
		t.Fatalf("orient journal without exact input error = %v", err)
	}
}

func TestOrientFixtureRequiresExactInputIdentity(t *testing.T) {
	fixture := Fixture{
		SchemaVersion:     FixtureSchemaVersion,
		ClassifierVersion: ClassifierVersion,
		Coverage:          Coverage{KnownGaps: []string{}},
		Rows: []FixtureRow{{
			Consumer: ConsumerOrient,
			Query:    "missing exact input",
		}},
	}
	if err := ValidateFixture(fixture); err == nil || !strings.Contains(err.Error(), "orient_input") {
		t.Fatalf("orient fixture without exact input error = %v", err)
	}
}

func TestOrientReportRequiresExactInputIdentity(t *testing.T) {
	homePath := newParityPackageHome(t)
	row := NewReplayRow(FixtureRow{
		Consumer: ConsumerOrient,
		Query:    "missing exact input",
	}, nil, time.Millisecond, true)
	report := BuildReport(HashQuery("missing-input-fixture"), ConsumerOrient, 0, Coverage{}, []ReplayRow{row}, parityTestNow)
	if _, err := WriteReport(homePath, report); err == nil || !strings.Contains(err.Error(), "input_sha256") {
		t.Fatalf("orient report without exact input error = %v", err)
	}
}

func TestFreezeKeepsSameOrientQueryWithDistinctExactTuples(t *testing.T) {
	homePath := newParityPackageHome(t)
	controlPath := writeValidControlReport(t, homePath)
	query := "same query, different context"
	first := OrientInput{Harness: "codex", Cwd: "/workspace/one", RepoKind: "product", RepoKindVia: "detect", Mode: "execute", Topic: "topic:one"}
	second := first
	second.Mode = "review"
	second.Topic = "topic:two"
	Record(homePath, NewOrientRow(query, first, []string{"claim://claim_aaaaaaaaaaaa"}, time.Millisecond, true, parityTestNow))
	Record(homePath, NewOrientRow(query, second, []string{"claim://claim_bbbbbbbbbbbb"}, time.Millisecond, true, parityTestNow))

	fixture, _, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow)
	if err != nil {
		t.Fatalf("freeze exact tuples: %v", err)
	}
	if len(fixture.Rows) != 2 {
		t.Fatalf("frozen rows = %d, want 2: %+v", len(fixture.Rows), fixture.Rows)
	}
	identities := map[string]bool{}
	for _, row := range fixture.Rows {
		if row.OrientInput == nil {
			t.Fatalf("frozen orient row lost tuple: %+v", row)
		}
		identities[HashOrientInput(*row.OrientInput, row.Query)] = true
	}
	if !identities[HashOrientInput(first, query)] || !identities[HashOrientInput(second, query)] {
		t.Fatalf("frozen identities = %v", identities)
	}
}

func TestLegacyClaimRefsAreFixtureSafeButReportHashed(t *testing.T) {
	legacy := "claim://brain-D1"
	fixture := Fixture{
		SchemaVersion:     FixtureSchemaVersion,
		ClassifierVersion: ClassifierVersion,
		Coverage:          Coverage{KnownGaps: []string{}},
		Rows: []FixtureRow{{
			Consumer:     ConsumerQuery,
			Query:        "legacy evidence",
			ExpectedRefs: []string{legacy},
		}},
	}
	if err := ValidateFixture(fixture); err != nil {
		t.Fatalf("safe legacy fixture: %v", err)
	}
	replay := NewReplayRow(fixture.Rows[0], []string{legacy}, time.Millisecond, true)
	want := HashQuery(legacy)
	if len(replay.ExpectedRefs) != 1 || replay.ExpectedRefs[0] != want || len(replay.ResultRefs) != 1 || replay.ResultRefs[0] != want {
		t.Fatalf("persisted legacy refs = %+v, want %q", replay, want)
	}
}

func TestFreezeValidatesWindowEndpointsBeforeReadingOrWriting(t *testing.T) {
	tests := []struct {
		name string
		days int
		now  time.Time
		want string
	}{
		{name: "overflowed start", days: int(^uint(0) >> 1), now: parityTestNow, want: "window_start"},
		{name: "invalid end", days: 1, now: time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC), want: "window_end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), "missing-home")
			_, _, err := FreezeFromJournal(homePath, tt.days, "", "", tt.now)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("freeze error = %v, want endpoint %q", err, tt.want)
			}
			if _, statErr := os.Stat(filepath.Join(homePath, "reviews")); !os.IsNotExist(statErr) {
				t.Fatalf("invalid freeze created artifacts: %v", statErr)
			}
		})
	}
}

func TestFreezeExcludesUnavailableObservationsAndCountsGap(t *testing.T) {
	homePath := newParityPackageHome(t)
	controlPath := writeValidControlReport(t, homePath)
	query := "mixed availability baseline"
	unavailableRef := "claim://claim_aaaaaaaaaaaa"
	availableRef := "claim://claim_bbbbbbbbbbbb"
	Record(homePath, NewRow(ConsumerQuery, query, []string{unavailableRef}, time.Millisecond, false, parityTestNow.Add(-2*time.Hour)))
	Record(homePath, NewRow(ConsumerQuery, query, []string{availableRef}, time.Millisecond, true, parityTestNow.Add(-time.Hour)))

	fixture, _, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow)
	if err != nil {
		t.Fatalf("freeze mixed availability: %v", err)
	}
	if len(fixture.Rows) != 1 || !equalStringSlices(fixture.Rows[0].ExpectedRefs, []string{availableRef}) {
		t.Fatalf("unavailable observation entered baseline: %+v", fixture.Rows)
	}
	if gap := "engine_unavailable:query:1"; !slices.Contains(fixture.Coverage.KnownGaps, gap) {
		t.Fatalf("coverage gaps=%v, want %q", fixture.Coverage.KnownGaps, gap)
	}
}

func TestFreezeRecordsLiveContextBreakerAsTypedGap(t *testing.T) {
	homePath := newParityPackageHome(t)
	controlPath := writeValidControlReport(t, homePath)
	Record(homePath, NewRow(ConsumerQuery, "successful baseline traffic", nil, time.Millisecond, true, parityTestNow.Add(-time.Hour)))

	// Seed authoritative hook_ breaker records: liveContextBreakerCount now reads
	// them through the eventstore Scan seam, not the frozen hook-events.ndjson.
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	breakers := []struct {
		id     string
		record map[string]any
	}{
		{"hook_breaker_sessionstart", map[string]any{"id": "hook_breaker_sessionstart", "created_at": parityTestNow.Add(-2 * time.Hour).Format(time.RFC3339Nano), "hook_name": "SessionStart", "breaker_tripped": true}},
		{"hook_breaker_pretooluse", map[string]any{"id": "hook_breaker_pretooluse", "created_at": parityTestNow.Add(-time.Hour).Format(time.RFC3339Nano), "hook_name": "PreToolUse", "breaker_tripped": true}},
		{"hook_breaker_userprompt", map[string]any{"id": "hook_breaker_userprompt", "created_at": parityTestNow.Add(-48 * time.Hour).Format(time.RFC3339Nano), "hook_name": "UserPromptSubmit", "breaker_tripped": true}},
	}
	for _, b := range breakers {
		if _, err := store.PutJSON(b.id, b.record, "  "); err != nil {
			t.Fatal(err)
		}
	}

	fixture, _, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow)
	if err != nil {
		t.Fatalf("freeze with live hook breaker: %v", err)
	}
	if gap := "live_hook_context_breaker:1"; !slices.Contains(fixture.Coverage.KnownGaps, gap) {
		t.Fatalf("coverage gaps=%v, want %q", fixture.Coverage.KnownGaps, gap)
	}
}

func TestFreezeRejectsIdentityWithoutSuccessfulObservation(t *testing.T) {
	homePath := newParityPackageHome(t)
	controlPath := writeValidControlReport(t, homePath)
	Record(homePath, NewRow(
		ConsumerQuery,
		"unavailable-only baseline",
		[]string{"claim://claim_aaaaaaaaaaaa"},
		time.Millisecond,
		false,
		parityTestNow.Add(-time.Hour),
	))

	if _, path, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow); err == nil || path != "" || !strings.Contains(err.Error(), "no successful observation") {
		t.Fatalf("freeze unavailable-only identity path=%q err=%v", path, err)
	}
}

func TestReportSeparatesParityFromNegativeControlValidity(t *testing.T) {
	masked := "claim://claim_aaaaaaaaaaaa"
	control := NewReplayRow(FixtureRow{
		Consumer:        ConsumerQuery,
		Query:           "negative control",
		ExpectedRefs:    []string{masked},
		NegativeControl: &NegativeControl{MaskedRef: masked},
	}, nil, time.Millisecond, true)
	report := BuildReport(HashQuery("fixture"), ConsumerQuery, 10, Coverage{}, []ReplayRow{control}, parityTestNow)
	if report.ParityPassed {
		t.Fatal("negative control loss was incorrectly reported as parity success")
	}
	if report.NegativeControlValid == nil || !*report.NegativeControlValid {
		t.Fatalf("negative control validity = %v, want true", report.NegativeControlValid)
	}

	unmasked := NewReplayRow(FixtureRow{Consumer: ConsumerQuery, Query: "normal"}, nil, time.Millisecond, true)
	normal := BuildReport(HashQuery("fixture-normal"), ConsumerQuery, 10, Coverage{}, []ReplayRow{unmasked}, parityTestNow)
	if !normal.ParityPassed || normal.NegativeControlValid != nil {
		t.Fatalf("normal report parity=%t control=%v", normal.ParityPassed, normal.NegativeControlValid)
	}
}

func TestReportRejectsClassSummariesNotDerivedFromRows(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "missing", mutate: func(report *Report) { report.Classes = nil }},
		{name: "counts", mutate: func(report *Report) { report.Classes[0].Rows++ }},
		{name: "latency", mutate: func(report *Report) { report.Classes[0].LatencyMS++ }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityPackageHome(t)
			row := NewReplayRow(FixtureRow{Consumer: ConsumerQuery, Query: "class summary evidence"}, nil, time.Millisecond, true)
			report := BuildReport(HashQuery("class-summary-fixture"), ConsumerQuery, 10, Coverage{}, []ReplayRow{row}, parityTestNow)
			tt.mutate(&report)
			if _, err := WriteReport(homePath, report); err == nil || !strings.Contains(err.Error(), "classes") {
				t.Fatalf("write report with %s class summary err=%v", tt.name, err)
			}
		})
	}
}

func TestUnavailableEngineFailsParityEvenWithNoExpectedRefs(t *testing.T) {
	row := NewReplayRow(FixtureRow{Consumer: ConsumerQuery, Query: "empty expected"}, nil, 0, false)
	if row.ParityPassed {
		t.Fatal("unavailable engine passed an empty-expected parity row")
	}
}

func TestUnavailableEngineCannotValidateNegativeControl(t *testing.T) {
	homePath := newParityPackageHome(t)
	masked := "claim://claim_aaaaaaaaaaaa"
	row := NewReplayRow(FixtureRow{
		Consumer:        ConsumerQuery,
		Query:           "unavailable negative control",
		ExpectedRefs:    []string{masked},
		NegativeControl: &NegativeControl{MaskedRef: masked},
	}, nil, time.Millisecond, false)
	report := BuildReport(HashQuery("unavailable-control-fixture"), ConsumerQuery, 10, Coverage{}, []ReplayRow{row}, parityTestNow)
	if report.NegativeControlValid == nil || *report.NegativeControlValid {
		t.Fatalf("unavailable engine negative-control validity = %v, want false", report.NegativeControlValid)
	}
	controlPath, err := WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write honest unavailable-engine report: %v", err)
	}
	Record(homePath, NewRow(ConsumerQuery, "traffic", nil, time.Millisecond, true, parityTestNow))
	if _, _, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow); err == nil || !strings.Contains(err.Error(), "valid negative control") {
		t.Fatalf("freeze with unavailable-engine control error = %v", err)
	}
}

func TestFreezeRejectsControlReportWithInconsistentSetDeltas(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*ReplayRow)
	}{
		{
			name: "lost hits",
			want: "lost_hits",
			mutate: func(row *ReplayRow) {
				row.ResultRefs = append([]string(nil), row.ExpectedRefs...)
				row.ResultCount = len(row.ResultRefs)
			},
		},
		{
			name: "extra hits",
			want: "extra_hits",
			mutate: func(row *ReplayRow) {
				row.ExtraHits = []string{HashQuery("fabricated extra")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityPackageHome(t)
			controlPath := writeValidControlReport(t, homePath)
			data, err := os.ReadFile(controlPath)
			if err != nil {
				t.Fatal(err)
			}
			var report Report
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&report.Rows[0])
			tampered, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(controlPath, append(tampered, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			Record(homePath, NewRow(ConsumerQuery, "traffic", nil, time.Millisecond, true, parityTestNow))
			if _, _, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("freeze with inconsistent %s error = %v", tt.name, err)
			}
		})
	}
}

func TestFreezeRequiresStrictValidControlReportAndPinsItsHash(t *testing.T) {
	homePath := newParityPackageHome(t)
	controlPath := writeValidControlReport(t, homePath)
	controlBytes, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	Record(homePath, NewRow(ConsumerQuery, "traffic", []string{"claim://claim_aaaaaaaaaaaa"}, time.Millisecond, true, parityTestNow))
	fixture, fixturePath, err := FreezeFromJournal(homePath, 1, "", controlPath, parityTestNow)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if fixturePath == "" || fixture.ControlReportRef != hashBytes(controlBytes) {
		t.Fatalf("control report ref = %q, want %q", fixture.ControlReportRef, hashBytes(controlBytes))
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid-control.json")
	invalid := append([]byte(strings.TrimSpace(string(controlBytes))), []byte(` {"extra":true}`)...)
	if err := os.WriteFile(invalidPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FreezeFromJournal(homePath, 1, "other.json", invalidPath, parityTestNow); err == nil {
		t.Fatal("freeze accepted a control report with trailing JSON")
	}
}

func TestCheckpointSealsIdentityCoverageAndPartialProgress(t *testing.T) {
	homePath := newParityPackageHome(t)
	identity := NewReplayIdentity(HashQuery("fixture"), ConsumerQuery, 10)
	checkpoint := NewCheckpoint(identity, Coverage{KnownGaps: []string{"query_engine_unavailable"}})
	checkpoint.Rows = append(checkpoint.Rows, NewReplayRow(FixtureRow{Consumer: ConsumerQuery, Query: "one"}, nil, 0, false, "query_engine_unavailable"))
	checkpoint.Completed = len(checkpoint.Rows)
	if err := WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	path := CheckpointPath(homePath, identity)
	loaded, err := LoadCheckpoint(path, identity)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if loaded.Completed != 1 || len(loaded.Rows) != 1 || len(loaded.Coverage.KnownGaps) != 1 {
		t.Fatalf("partial checkpoint not preserved: %+v", loaded)
	}
	if temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp-*")); len(temps) != 0 {
		t.Fatalf("atomic write left temporary files: %v", temps)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["completed"] = float64(0)
	tampered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(path, identity); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("tampered checkpoint error = %v", err)
	}

	if err := WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	wrong := NewReplayIdentity(HashQuery("other-fixture"), ConsumerQuery, 10)
	if _, err := LoadCheckpoint(path, wrong); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong identity error = %v", err)
	}
	checkpoint.Completed = 0
	if err := WriteCheckpoint(homePath, &checkpoint); err == nil {
		t.Fatal("checkpoint accepted completed != len(rows)")
	}
}

func TestCheckpointDependencyAssertionRequiresEngineEvidence(t *testing.T) {
	homePath := newParityPackageHome(t)
	identity, err := NewReplayIdentityWithDependency(
		HashQuery("dependency-fixture"),
		ConsumerQuery,
		10,
		"breadth",
		"gbrain",
	)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := NewCheckpoint(identity, Coverage{})
	checkpoint.Rows = append(checkpoint.Rows, NewReplayRow(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "replacement dependency must be attested",
	}, nil, time.Millisecond, true))
	checkpoint.Completed = len(checkpoint.Rows)

	err = WriteCheckpoint(homePath, &checkpoint)
	if err == nil || !strings.Contains(err.Error(), "engine dependency evidence required") {
		t.Fatalf("checkpoint without dependency evidence error = %v", err)
	}
}

func TestExpectedEngineRequiresUsedObservedMatch(t *testing.T) {
	identity, err := NewReplayIdentityWithDependency(
		HashQuery("expected-engine-fixture"),
		ConsumerQuery,
		10,
		"breadth",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	row := NewReplayRowWithEngineEvidence(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "matching refs cannot hide the wrong engine",
	}, nil, time.Millisecond, true, EngineDependencyEvidence{
		Attempted:       true,
		RequestedEngine: "breadth",
		ObservedEngine:  "lexical",
		Outcome:         EngineOutcomeUsed,
	}, identity)
	if row.DependencyPassed == nil || *row.DependencyPassed {
		t.Fatalf("dependency result = %v, want false", row.DependencyPassed)
	}
	if row.ParityPassed {
		t.Fatalf("matching refs with wrong observed engine must fail parity: %+v", row)
	}
}

func TestEngineEvidencePersistsZeroCorpusGeneration(t *testing.T) {
	row := NewReplayRowWithEngineEvidence(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "zero generation is still observed evidence",
	}, nil, time.Millisecond, true, EngineDependencyEvidence{
		Attempted:      true,
		ObservedEngine: "lexical",
		Outcome:        EngineOutcomeUsed,
	}, NewReplayIdentity(HashQuery("zero-generation-fixture"), ConsumerQuery, 10))
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"corpus_generation":0`) {
		t.Fatalf("zero corpus generation omitted from engine evidence: %s", raw)
	}
}

func TestEngineEvidencePersistsEmptyEmbedder(t *testing.T) {
	row := NewReplayRowWithEngineEvidence(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "lexical engine has an explicit empty embedder",
	}, nil, time.Millisecond, true, EngineDependencyEvidence{
		Attempted:      true,
		ObservedEngine: "lexical",
		Outcome:        EngineOutcomeUsed,
	}, NewReplayIdentity(HashQuery("empty-embedder-fixture"), ConsumerQuery, 10))
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"embedder":""`) {
		t.Fatalf("empty embedder omitted from engine evidence: %s", raw)
	}
}

func TestBuildReportWithoutDependencyKeepsLegacyID(t *testing.T) {
	fixtureRef := HashQuery("legacy report identity fixture")
	row := NewReplayRow(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "legacy report identity",
	}, nil, time.Millisecond, true)
	report := BuildReport(fixtureRef, ConsumerQuery, 10, Coverage{}, []ReplayRow{row}, parityTestNow)
	want := reviewartifact.ID("parity_", fixtureRef, ConsumerQuery, parityTestNow.UTC().Format(time.RFC3339Nano))
	if report.ID != want {
		t.Fatalf("report id = %q, want backward-compatible %q", report.ID, want)
	}
}

func TestReportValidationRecomputesDependencyFromRowEvidence(t *testing.T) {
	homePath := newParityPackageHome(t)
	identity, err := NewReplayIdentityWithDependency(
		HashQuery("report-dependency-fixture"),
		ConsumerQuery,
		10,
		"breadth",
		"gbrain",
	)
	if err != nil {
		t.Fatal(err)
	}
	row := NewReplayRowWithEngineEvidence(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "report validation must distrust booleans",
	}, nil, time.Millisecond, true, EngineDependencyEvidence{
		Attempted:       true,
		RequestedEngine: "breadth",
		ObservedEngine:  "breadth",
		Outcome:         EngineOutcomeUsed,
	}, identity)
	report := BuildReportWithIdentity(identity, Coverage{}, []ReplayRow{row}, parityTestNow)
	report.Rows[0].EngineDependency.ObservedEngine = "lexical"

	_, err = WriteReport(homePath, report)
	if err == nil || !strings.Contains(err.Error(), "dependency_passed does not match engine evidence") {
		t.Fatalf("tampered report dependency error = %v", err)
	}
}

func TestCheckpointRejectsChangedEngineDependencyContract(t *testing.T) {
	homePath := newParityPackageHome(t)
	fixtureRef := HashQuery("checkpoint-dependency-fixture")
	identity, err := NewReplayIdentityWithDependency(fixtureRef, ConsumerQuery, 10, "breadth", "gbrain")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := NewCheckpoint(identity, Coverage{})
	checkpoint.Rows = append(checkpoint.Rows, NewReplayRowWithEngineEvidence(FixtureRow{
		Consumer: ConsumerQuery,
		Query:    "resume binds the engine contract",
	}, nil, time.Millisecond, true, EngineDependencyEvidence{
		Attempted:       true,
		RequestedEngine: "breadth",
		ObservedEngine:  "breadth",
		Outcome:         EngineOutcomeUsed,
	}, identity))
	checkpoint.Completed = len(checkpoint.Rows)
	if err := WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatal(err)
	}
	path := CheckpointPath(homePath, identity)
	changed, err := NewReplayIdentityWithDependency(fixtureRef, ConsumerQuery, 10, "lexical", "gbrain")
	if err != nil {
		t.Fatal(err)
	}
	if CheckpointPath(homePath, changed) == path {
		t.Fatal("dependency contract did not participate in checkpoint path")
	}
	_, err = LoadCheckpoint(path, changed)
	if err == nil || !strings.Contains(err.Error(), "dependency contract") {
		t.Fatalf("changed dependency contract error = %v", err)
	}
}

func writeValidControlReport(t *testing.T, homePath string) string {
	t.Helper()
	masked := "claim://claim_aaaaaaaaaaaa"
	row := NewReplayRow(FixtureRow{
		Consumer:        ConsumerQuery,
		Query:           "control",
		ExpectedRefs:    []string{masked},
		NegativeControl: &NegativeControl{MaskedRef: masked},
	}, nil, time.Millisecond, true)
	report := BuildReport(HashQuery("control-fixture"), ConsumerQuery, 10, Coverage{}, []ReplayRow{row}, parityTestNow)
	path, err := WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write control report: %v", err)
	}
	return path
}

func newParityPackageHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := home.Init(homePath, "test", parityTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	return homePath
}
