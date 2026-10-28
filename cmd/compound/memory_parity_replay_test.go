package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/retrievalparity"
)

const (
	parityCLIHelperEnv       = "COMPOUND_TEST_PARITY_CLI_HELPER"
	parityCLIHelperFixture   = "COMPOUND_TEST_PARITY_CLI_FIXTURE"
	parityCLIHelperConsumer  = "COMPOUND_TEST_PARITY_CLI_CONSUMER"
	parityCLIHelperProfile   = "COMPOUND_TEST_PARITY_CLI_ENGINE_PROFILE"
	parityFailureHelperEnv   = "COMPOUND_TEST_PARITY_FAILURE_HELPER"
	parityFailureHelperQuery = "COMPOUND_TEST_PARITY_FAILURE_QUERY"
	parityFakeSidecarEnv     = "COMPOUND_TEST_PARITY_FAKE_SIDECAR"
	parityFakeSidecarHitID   = "COMPOUND_TEST_PARITY_FAKE_HIT_ID"
	parityFakeSidecarMode    = "COMPOUND_TEST_PARITY_FAKE_MODE"
	parityFakeModeAhead      = "engine_ahead"
	parityFakeModeUnowned    = "unowned_claim"
	parityFakeSidecarTestRun = "^TestParityFakeBreadthSidecar$"
)

func TestParityReplayCLIHelper(t *testing.T) {
	if os.Getenv(parityCLIHelperEnv) != "1" {
		return
	}
	args := []string{
		"parity-replay",
		"--fixture", os.Getenv(parityCLIHelperFixture),
		"--consumer", os.Getenv(parityCLIHelperConsumer),
		"--json",
	}
	if profile := os.Getenv(parityCLIHelperProfile); profile != "" {
		args = append(args, "--engine-profile", profile)
	}
	cmdMemory(args)
	os.Exit(0)
}

func TestParityFailureCLIHelper(t *testing.T) {
	switch os.Getenv(parityFailureHelperEnv) {
	case retrievalparity.ConsumerRecall:
		cmdMemory([]string{"recall", os.Getenv(parityFailureHelperQuery), "--json"})
	case retrievalparity.ConsumerSearch:
		cmdMemory([]string{"search", "--lane", "corpus-fixture", os.Getenv(parityFailureHelperQuery), "--json"})
	default:
		return
	}
	os.Exit(0)
}

func TestParityFakeBreadthSidecar(t *testing.T) {
	if os.Getenv(parityFakeSidecarEnv) != "1" {
		return
	}
	hitID := os.Getenv(parityFakeSidecarHitID)
	mode := os.Getenv(parityFakeSidecarMode)
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	var corpusGen int64
	if mode == parityFakeModeAhead {
		corpusGen = 1 << 30
	}
	for {
		var req mem.Request
		if err := dec.Decode(&req); err != nil {
			os.Exit(0)
		}
		resp := mem.Response{
			OK:            true,
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     req.RequestID,
			Name:          "parity-test-breadth",
			Breadth:       true,
			Ready:         true,
			CorpusGen:     corpusGen,
		}
		switch req.Op {
		case mem.OpCapabilities:
		case mem.OpIndex:
			corpusGen = req.CorpusGen
			resp.CorpusGen = corpusGen
			resp.Indexed = len(req.Claims)
		case mem.OpQuery:
			if mode == parityFakeModeUnowned {
				resp.Hits = []mem.Hit{{ClaimID: "claim_aaaaaaaaaaaa", Score: 1}}
			} else if hitID != "" && slices.Contains(req.Allowlist, hitID) {
				resp.Hits = []mem.Hit{{ClaimID: hitID, Score: 1}}
			}
		default:
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: "unknown operation"}
		}
		if err := enc.Encode(resp); err != nil {
			os.Exit(0)
		}
	}
}

func TestParityReplayNegativeControlMasksData(t *testing.T) {
	now := time.Now().UTC()
	query := "synthetic retrieval answer baseline anchor"
	unmaskedHome := newParityTestHome(t)
	wantRef := seedParityClaim(t, unmaskedHome, "synthetic retrieval answer", now)
	fixturePath := writeParityFixture(t, unmaskedHome, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        query,
			ExpectedRefs: []string{wantRef},
		}},
	})

	unmaskedExit, unmaskedReport := runParityReplayCLI(t, unmaskedHome, fixturePath, retrievalparity.ConsumerQuery)
	if unmaskedExit != 0 || !unmaskedReport.ParityPassed || unmaskedReport.NegativeControlValid != nil || len(unmaskedReport.Rows) != 1 || len(unmaskedReport.Rows[0].LostHits) != 0 {
		t.Fatalf("unmasked replay exit=%d report=%+v, want green with no lost hits", unmaskedExit, unmaskedReport)
	}

	controlFixturePath := writeParityFixtureNamed(t, unmaskedHome, "synthetic-control-fixture.json", retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        query,
			ExpectedRefs: []string{wantRef},
			NegativeControl: &retrievalparity.NegativeControl{
				MaskedRef: wantRef,
			},
		}},
	})
	failedControlExit, failedControlReport := runParityReplayCLI(t, unmaskedHome, controlFixturePath, retrievalparity.ConsumerQuery)
	if failedControlExit == 0 || !failedControlReport.ParityPassed || failedControlReport.NegativeControlValid == nil || *failedControlReport.NegativeControlValid {
		t.Fatalf("failed control replay exit=%d report=%+v, want nonzero with negative_control_valid=false", failedControlExit, failedControlReport)
	}
	maskedHome := cloneParityHomeWithoutClaim(t, unmaskedHome, strings.TrimPrefix(wantRef, "claim://"), now)
	maskedExit, maskedReport := runParityReplayCLI(t, maskedHome, controlFixturePath, retrievalparity.ConsumerQuery)
	if maskedExit == 0 || maskedReport.ParityPassed || maskedReport.NegativeControlValid == nil || !*maskedReport.NegativeControlValid || len(maskedReport.Rows) != 1 {
		t.Fatalf("masked replay exit=%d report=%+v, want nonzero data-level loss", maskedExit, maskedReport)
	}
	if !slices.Contains(maskedReport.Rows[0].LostHits, wantRef) {
		t.Fatalf("masked replay lost_hits=%v, want %s", maskedReport.Rows[0].LostHits, wantRef)
	}
	t.Logf("f1 unmasked_exit=%d masked_exit=%d masked_lost_hits=%v", unmaskedExit, maskedExit, maskedReport.Rows[0].LostHits)
}

func TestParityReplayOrientMatchesProductionContextPack(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	wantRef := seedParityClaim(t, homePath, "token-disjoint expected marker", now)
	query := "unrelated semantic probe"
	t.Setenv(parityFakeSidecarEnv, "1")
	t.Setenv(parityFakeSidecarHitID, strings.TrimPrefix(wantRef, "claim://"))
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", os.Args[0]+" -test.run="+parityFakeSidecarTestRun)

	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FoldPositions(mem.PositionFoldOptions{}, now); err != nil {
		t.Fatalf("fold positions for production-path parity: %v", err)
	}
	wantRefs := append([]string{wantRef}, parityFoldedPositionHashes(t, store)...)
	sort.Strings(wantRefs)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion: retrievalparity.FixtureSchemaVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerOrient,
			Query:        query,
			OrientInput:  parityTestOrientInput(t, homePath),
			ExpectedRefs: wantRefs,
		}},
	})
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
	}, now)
	if err != nil || code != 0 {
		t.Fatalf("replay code=%d err=%v report=%+v", code, err, report)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	productionJSON := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", query})
	})
	var production envelope.ContextPack
	if err := json.Unmarshal([]byte(productionJSON), &production); err != nil {
		t.Fatalf("parse production ContextPack: %v\n%s", err, productionJSON)
	}
	if !slices.Contains(production.ActiveMemory, wantRef) {
		t.Fatalf("production semantic path did not surface fake-sidecar hit %s: %v", wantRef, production.ActiveMemory)
	}
	normalizedProduction, hasPosition := parityPersistedOrientRefs(production.ActiveMemory)
	if !hasPosition {
		t.Fatalf("production ContextPack did not exercise position refs: %v", production.ActiveMemory)
	}
	if len(report.Rows) != 1 || !sameStringSet(report.Rows[0].ResultRefs, normalizedProduction) {
		t.Fatalf("replay refs=%v production refs=%v", report.Rows[0].ResultRefs, production.ActiveMemory)
	}
	t.Logf("f2 replay_refs=%v production_refs_normalized=%v", report.Rows[0].ResultRefs, normalizedProduction)
}

func TestParityReplayEngineAvailabilityFollowsConsumerFallback(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic available engine marker"
	wantRef := seedParityClaim(t, homePath, query, now)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FoldPositions(mem.PositionFoldOptions{}, now); err != nil {
		t.Fatalf("fold positions for fallback test: %v", err)
	}
	wantRefs := append([]string{wantRef}, parityFoldedPositionHashes(t, store)...)
	sort.Strings(wantRefs)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{
			{Consumer: retrievalparity.ConsumerOrient, Query: query, OrientInput: parityTestOrientInput(t, homePath), ExpectedRefs: wantRefs},
			{Consumer: retrievalparity.ConsumerQuery, Query: query, ExpectedRefs: []string{wantRef}},
		},
	})

	queryAvailable, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code != 0 || len(queryAvailable.Rows) != 1 || !queryAvailable.Rows[0].EngineAvailable {
		t.Fatalf("lexical query replay code=%d err=%v report=%+v", code, err, queryAvailable)
	}

	t.Setenv("COMPOUND_MEM_ENGINE_BIN", filepath.Join(t.TempDir(), "missing-engine"))
	orientFallback, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
	}, now.Add(time.Second))
	if err != nil || code != 0 || len(orientFallback.Rows) != 1 || !orientFallback.Rows[0].EngineAvailable {
		t.Fatalf("orient fallback replay code=%d err=%v report=%+v", code, err, orientFallback)
	}
	if !slices.Contains(orientFallback.Rows[0].ResultRefs, wantRef) {
		t.Fatalf("orient fallback refs=%v, want %s", orientFallback.Rows[0].ResultRefs, wantRef)
	}

	queryUnavailable, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now.Add(2*time.Second))
	if err != nil || code == 0 || len(queryUnavailable.Rows) != 1 || queryUnavailable.Rows[0].EngineAvailable {
		t.Fatalf("unavailable query replay code=%d err=%v report=%+v", code, err, queryUnavailable)
	}
	if !slices.Contains(queryUnavailable.Rows[0].LostHits, wantRef) || !slices.Contains(queryUnavailable.Coverage.KnownGaps, "query_engine_unavailable") {
		t.Fatalf("unavailable query loss/gaps=%v/%v, want %s and query_engine_unavailable", queryUnavailable.Rows[0].LostHits, queryUnavailable.Coverage.KnownGaps, wantRef)
	}
}

func TestParityReplayCmdMemoryEngineProfileBindsConfiguredEngine(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	wantRef := seedParityClaim(t, homePath, "synthetic configured profile hit", now)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        "token-disjoint configured profile query",
			ExpectedRefs: []string{wantRef},
		}},
	})
	exitCode, out := runParityProfileReplayCLI(t, homePath, fixturePath, "memory-cli", strings.TrimPrefix(wantRef, "claim://"))
	if exitCode != 0 {
		t.Fatalf("cmdMemory parity replay with engine profile exit=%d, want 0\n%s", exitCode, out)
	}
	var report retrievalparity.Report
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse profiled parity replay report: %v\n%s", err, out)
	}
	if !report.ParityPassed || len(report.Rows) != 1 {
		t.Fatalf("profiled parity replay report=%+v", report)
	}
	evidence := report.Rows[0].EngineDependency
	if evidence == nil || evidence.RequestedEngine != "memory-cli" || evidence.ObservedEngine != "parity-test-breadth" || evidence.Outcome != retrievalparity.EngineOutcomeUsed {
		t.Fatalf("profiled parity replay evidence=%+v", evidence)
	}
}

func TestParityReplayRefusedEngineErrorsUseRefusedRuntimeGap(t *testing.T) {
	now := time.Now().UTC()
	for _, mode := range []string{parityFakeModeAhead, parityFakeModeUnowned} {
		t.Run(mode, func(t *testing.T) {
			homePath := newParityTestHome(t)
			fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
				SchemaVersion:     retrievalparity.FixtureSchemaVersion,
				ClassifierVersion: retrievalparity.ClassifierVersion,
				Rows: []retrievalparity.FixtureRow{{
					Consumer: retrievalparity.ConsumerQuery,
					Query:    "synthetic refused engine",
				}},
			})
			t.Setenv(parityFakeSidecarEnv, "1")
			t.Setenv(parityFakeSidecarMode, mode)
			t.Setenv("COMPOUND_MEM_ENGINE_BIN", os.Args[0]+" -test.run="+parityFakeSidecarTestRun)
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			report, _, code, err := runMemoryParityReplay(homePath, store, []string{
				"--fixture", fixturePath,
				"--consumer", retrievalparity.ConsumerQuery,
			}, now)
			if err != nil || code == 0 || len(report.Rows) != 1 {
				t.Fatalf("refused replay mode=%s code=%d err=%v report=%+v", mode, code, err, report)
			}
			row := report.Rows[0]
			if row.EngineDependency == nil || row.EngineDependency.Outcome != retrievalparity.EngineOutcomeRefused {
				t.Fatalf("refused replay mode=%s engine evidence=%+v", mode, row.EngineDependency)
			}
			if row.RuntimeGap != "query_engine_refused" || !slices.Contains(report.Coverage.KnownGaps, "query_engine_refused") {
				t.Fatalf("refused replay mode=%s runtime_gap=%q known_gaps=%v", mode, row.RuntimeGap, report.Coverage.KnownGaps)
			}
		})
	}
}

func TestParityReplayExpectedEngineFailsWhenLexicalMatchesRefs(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic expected replacement engine marker"
	wantRef := seedParityClaim(t, homePath, query, now)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        query,
			ExpectedRefs: []string{wantRef},
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--expect-engine", "breadth",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || report.ParityPassed || report.ExpectEngine != "breadth" || len(report.Rows) != 1 {
		t.Fatalf("expected-engine replay code=%d report=%+v", code, report)
	}
	row := report.Rows[0]
	if len(row.LostHits) != 0 || !slices.Contains(row.ResultRefs, wantRef) {
		t.Fatalf("data parity must stay green while dependency fails: %+v", row)
	}
	if row.EngineDependency == nil || row.EngineDependency.Outcome != retrievalparity.EngineOutcomeUsed || row.EngineDependency.ObservedEngine != "lexical" || row.DependencyPassed == nil || *row.DependencyPassed {
		t.Fatalf("lexical dependency evidence = %+v", row)
	}
}

func TestParityReplayForbidsRequestedGBrainWhenOrientRefsMatch(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	wantRef := seedParityClaim(t, homePath, "synthetic forbidden gbrain result", now)
	query := "token disjoint forbidden dependency probe"
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerOrient,
			Query:        query,
			OrientInput:  parityTestOrientInput(t, homePath),
			ExpectedRefs: []string{wantRef},
		}},
	})
	t.Setenv(parityFakeSidecarEnv, "1")
	t.Setenv(parityFakeSidecarHitID, strings.TrimPrefix(wantRef, "claim://"))
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", os.Args[0]+" -test.run="+parityFakeSidecarTestRun)
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "gbrain")
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FoldPositions(mem.PositionFoldOptions{}, now); err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
		"--forbid-engine", "gbrain",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || report.ParityPassed || report.ForbidEngine != "gbrain" || len(report.Rows) != 1 {
		t.Fatalf("forbidden-engine replay code=%d report=%+v", code, report)
	}
	row := report.Rows[0]
	if len(row.LostHits) != 0 || !slices.Contains(row.ResultRefs, wantRef) {
		t.Fatalf("matching orient refs must remain visible in the failed dependency row: %+v", row)
	}
	if row.EngineDependency == nil || row.EngineDependency.RequestedEngine != "gbrain" || row.EngineDependency.ObservedEngine != "parity-test-breadth" || row.EngineDependency.Outcome != retrievalparity.EngineOutcomeUsed || row.DependencyPassed == nil || *row.DependencyPassed {
		t.Fatalf("forbidden gbrain dependency evidence = %+v", row)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{query, os.Args[0], os.Getenv("COMPOUND_MEM_ENGINE_BIN")} {
		if privateValue != "" && strings.Contains(string(raw), privateValue) {
			t.Fatalf("parity report leaked query/command evidence %q: %s", privateValue, raw)
		}
	}
}

func TestParityReplayForbidsGBrainWhenCapabilitiesProbeFails(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic broken gbrain fallback marker"
	wantRef := seedParityClaim(t, homePath, query, now)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerOrient,
			Query:        query,
			OrientInput:  parityTestOrientInput(t, homePath),
			ExpectedRefs: []string{wantRef},
		}},
	})
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", filepath.Join(t.TempDir(), "missing-gbrain"))
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "gbrain")
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
		"--forbid-engine", "gbrain",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || len(report.Rows) != 1 || len(report.Rows[0].LostHits) != 0 {
		t.Fatalf("broken forbidden gbrain replay code=%d report=%+v", code, report)
	}
	evidence := report.Rows[0].EngineDependency
	if evidence == nil || evidence.RequestedEngine != "gbrain" || evidence.ObservedEngine != "" || !evidence.Attempted || evidence.Outcome != retrievalparity.EngineOutcomeDegraded || evidence.ErrorClass != retrievalparity.EngineErrorUnavailable {
		t.Fatalf("probe-failure gbrain evidence = %+v", evidence)
	}
}

func TestParityReplayRejectsMissingEngineExpectationValue(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer: retrievalparity.ConsumerQuery,
			Query:    "missing dependency flag value",
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--expect-engine",
	}, now)
	if err == nil || code != 2 || reportPath != "" || !strings.Contains(err.Error(), "--expect-engine requires") {
		t.Fatalf("missing expect-engine value code=%d report=%q err=%v", code, reportPath, err)
	}
}

func TestParityReplayRejectsInvalidArgvBeforeWritingArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Now().UTC()
	tests := []struct {
		name  string
		extra []string
	}{
		{name: "misspelled flag", extra: []string{"--expect-engne", "gbrain"}},
		{name: "unknown flag", extra: []string{"--unknown", "value"}},
		{name: "equals syntax", extra: []string{"--expect-engine=gbrain"}},
		{name: "stray positional", extra: []string{"stray"}},
		{name: "missing value", extra: []string{"--expect-engine"}},
		{name: "value replaced by flag", extra: []string{"--expect-engine", "--json"}},
		{name: "duplicate value flag", extra: []string{"--expect-engine", "gbrain", "--expect-engine", "gbrain"}},
		{name: "missing engine profile", extra: []string{"--engine-profile"}},
		{name: "duplicate engine profile", extra: []string{"--engine-profile", "gbrain", "--engine-profile", "gbrain"}},
		{name: "duplicate json", extra: []string{"--json", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityTestHome(t)
			fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
				SchemaVersion:     retrievalparity.FixtureSchemaVersion,
				ClassifierVersion: retrievalparity.ClassifierVersion,
				Rows: []retrievalparity.FixtureRow{{
					Consumer: retrievalparity.ConsumerQuery,
					Query:    "invalid argv must not execute",
				}},
			})
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			before := parityReplayArtifactPaths(t, homePath)
			args := []string{
				"--fixture", fixturePath,
				"--consumer", retrievalparity.ConsumerQuery,
			}
			args = append(args, tt.extra...)
			_, reportPath, code, replayErr := runMemoryParityReplay(homePath, store, args, now)
			if replayErr == nil || code != 2 || reportPath != "" {
				t.Fatalf("invalid argv code=%d report=%q err=%v, want usage rejection", code, reportPath, replayErr)
			}
			after := parityReplayArtifactPaths(t, homePath)
			if !slices.Equal(after, before) {
				t.Fatalf("invalid argv wrote artifacts: before=%v after=%v", before, after)
			}
		})
	}
}

func TestParityReplayRejectsMalformedOrContradictoryEngineContract(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer: retrievalparity.ConsumerQuery,
			Query:    "invalid dependency contract",
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "malformed", args: []string{"--expect-engine", "../gbrain"}, want: "identity invalid"},
		{name: "contradictory", args: []string{"--expect-engine", "gbrain", "--forbid-engine", "gbrain"}, want: "contradict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--fixture", fixturePath, "--consumer", retrievalparity.ConsumerQuery}
			args = append(args, tt.args...)
			_, reportPath, code, err := runMemoryParityReplay(homePath, store, args, now)
			if err == nil || code != 2 || reportPath != "" || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("invalid engine contract code=%d report=%q err=%v", code, reportPath, err)
			}
		})
	}
}

func TestParityReplayWithoutEngineFlagsPreservesSuccessSemantics(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic compatibility engine marker"
	wantRef := seedParityClaim(t, homePath, query, now)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        query,
			ExpectedRefs: []string{wantRef},
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code != 0 || !report.ParityPassed || report.ExpectEngine != "" || report.ForbidEngine != "" || report.DependencyPassed != nil {
		t.Fatalf("compatibility replay code=%d err=%v report=%+v", code, err, report)
	}
	row := report.Rows[0]
	if row.EngineDependency == nil || row.EngineDependency.ObservedEngine != "lexical" || row.EngineDependency.Outcome != retrievalparity.EngineOutcomeUsed || row.DependencyPassed == nil || !*row.DependencyPassed {
		t.Fatalf("compatibility row evidence = %+v", row)
	}
}

func TestRetrievalJournalCLIAndBestEffortFailure(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic journal retrieval marker"
	seedParityClaim(t, homePath, query, now)

	captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--task", query})
	})
	healthyQueryOut := captureStdout(t, func() {
		cmdMemory([]string{"query", query})
	})

	rows, err := retrievalparity.LoadJournal(homePath)
	if err != nil {
		t.Fatalf("load retrieval journal: %v", err)
	}
	byConsumer := map[string]retrievalparity.Row{}
	for _, row := range rows {
		if row.Consumer == retrievalparity.ConsumerOrient || row.Consumer == retrievalparity.ConsumerQuery {
			byConsumer[row.Consumer] = row
		}
	}
	for _, consumer := range []string{retrievalparity.ConsumerOrient, retrievalparity.ConsumerQuery} {
		row, ok := byConsumer[consumer]
		if !ok {
			t.Fatalf("journal consumers=%v, missing %s", byConsumer, consumer)
		}
		if row.QueryClass != retrievalparity.ClassifyQuery(consumer, query) || row.ClassifierVersion != retrievalparity.ClassifierVersion {
			t.Fatalf("journal row classification=%+v", row)
		}
		if !validParitySHA(row.QuerySHA256) || row.LatencyMS < 0 || row.Timestamp == "" || row.ResultCount != len(row.ResultRefs) || !row.EngineAvailable {
			t.Fatalf("journal row incomplete=%+v", row)
		}
		if _, err := time.Parse(time.RFC3339Nano, row.Timestamp); err != nil {
			t.Fatalf("journal timestamp %q: %v", row.Timestamp, err)
		}
	}

	journalPath := filepath.Join(homePath, "events", "retrieval-traffic.ndjson")
	if err := os.Rename(journalPath, journalPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	brokenQueryOut, brokenErr := captureParityOutput(t, func() {
		cmdMemory([]string{"query", query})
	})
	if brokenQueryOut != healthyQueryOut {
		t.Fatalf("journal failure changed consumer stdout\nhealthy=%q\nbroken=%q", healthyQueryOut, brokenQueryOut)
	}
	if got := strings.Count(brokenErr, "retrieval journal warning:"); got != 1 {
		t.Fatalf("journal failure warnings=%d, want 1; stderr=%q", got, brokenErr)
	}
	if strings.Contains(brokenErr, query) {
		t.Fatalf("journal warning leaked raw query: %q", brokenErr)
	}
	t.Logf("f3 journal_rows=%d consumers=%v best_effort_stdout_equal=%t warnings=1", len(rows), []string{retrievalparity.ConsumerOrient, retrievalparity.ConsumerQuery}, brokenQueryOut == healthyQueryOut)
}

func TestRecallFailureJournalsUnavailableObservationBeforeExit(t *testing.T) {
	homePath := newParityTestHome(t)
	query := "synthetic recall failure marker"
	// A torn FINAL line is a crash-truncated append the ledger law tolerates as
	// never-written; genuine corruption must be mid-file to fail loud. Two
	// garbage lines put the first one mid-file.
	claimsPath := filepath.Join(homePath, "ledger", "claims.jsonl")
	if err := os.WriteFile(claimsPath, []byte("not-json\nnot-json-either\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exitCode, output := runParityFailureCLI(t, homePath, retrievalparity.ConsumerRecall, query)
	if exitCode != 1 || !strings.Contains(output, "error:") {
		t.Fatalf("recall failure exit=%d output=%q, want existing exit 1 and error output", exitCode, output)
	}
	assertUnavailableParityRow(t, homePath, retrievalparity.ConsumerRecall, query)
}

func TestSearchFailureJournalsUnavailableObservationBeforeExit(t *testing.T) {
	homePath := newParityTestHome(t)
	query := "synthetic corpus failure marker"
	configPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `schema_version: compound.memory_engine_config.v0.1
corpus_lanes:
  - lane: corpus-fixture
    source_family: synthetic_import
    privacy: private
    embedder:
      provider: ollama
      model: fixture-embed
      dims: 3
      bits: f32
    chunk:
      unit: runes
      size: 64
      overlap: 8
      boundary: structure
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	exitCode, output := runParityFailureCLI(t, homePath, retrievalparity.ConsumerSearch, query)
	if exitCode != 1 || !strings.Contains(output, "memory search:") {
		t.Fatalf("search failure exit=%d output=%q, want existing exit 1 and error output", exitCode, output)
	}
	assertUnavailableParityRow(t, homePath, retrievalparity.ConsumerSearch, query)
}

func TestParityReportPrivacy(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic journal-only privacy sentinel"
	wantRef := seedParityClaim(t, homePath, query, now)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        query,
			ExpectedRefs: []string{wantRef},
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code != 0 {
		t.Fatalf("replay code=%d err=%v report=%+v", code, err, report)
	}
	persisted, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), query) {
		t.Fatalf("persisted report leaked raw query: %s", persisted)
	}
	if report.FixtureRef == "" || !validParitySHA(report.FixtureRef) || !report.Privacy.RawQueryExcluded || !report.Privacy.QueryRefsHashed {
		t.Fatalf("report privacy metadata=%+v fixture_ref=%q", report.Privacy, report.FixtureRef)
	}
	for _, row := range report.Rows {
		if !validParitySHA(row.QuerySHA256) {
			t.Fatalf("report query ref=%q, want sha256", row.QuerySHA256)
		}
		if row.Consumer == retrievalparity.ConsumerOrient && !validParitySHA(row.InputSHA256) {
			t.Fatalf("orient report input ref=%q, want sha256", row.InputSHA256)
		}
		if row.Consumer != retrievalparity.ConsumerOrient && row.InputSHA256 != "" {
			t.Fatalf("%s report carried orient-only input ref=%q", row.Consumer, row.InputSHA256)
		}
	}
	t.Logf("f4 report=%s raw_query_present=%t query_ref=%s", filepath.Base(reportPath), strings.Contains(string(persisted), query), report.Rows[0].QuerySHA256)
}

func TestParityReportHashesSemanticOrientRefs(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "fixture:retrieval-parity"
	semanticRef := "position://" + query + "::baseline_anchor::slot-aaaaaaaaaaaa"
	fixtureRow := retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerOrient,
		Query:        query,
		OrientInput:  parityTestOrientInput(t, homePath),
		ExpectedRefs: []string{semanticRef},
	}
	replayRow := retrievalparity.NewReplayRow(fixtureRow, []string{semanticRef}, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic orient fixture"),
		retrievalparity.ConsumerOrient,
		0,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{replayRow},
		now,
	)
	reportPath, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write orient report: %v", err)
	}
	persisted, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), query) {
		t.Fatalf("persisted orient report leaked raw task: %s", persisted)
	}
	if !slices.Contains(report.Rows[0].ResultRefs, retrievalparity.HashQuery(semanticRef)) {
		t.Fatalf("semantic orient ref was not hashed: %+v", report.Rows[0])
	}
}

func TestParityReportHashesSemanticSearchRefs(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "item_a"
	chunkRef := query + "#0"
	fixtureRow := retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerSearch,
		Query:        query,
		ExpectedRefs: []string{chunkRef},
	}
	replayRow := retrievalparity.NewReplayRow(fixtureRow, []string{chunkRef}, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic search fixture"),
		retrievalparity.ConsumerSearch,
		10,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{replayRow},
		now,
	)
	reportPath, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write search report: %v", err)
	}
	persisted, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), query) {
		t.Fatalf("persisted search report leaked raw query: %s", persisted)
	}
	if !slices.Contains(report.Rows[0].ResultRefs, retrievalparity.HashQuery(chunkRef)) {
		t.Fatalf("semantic search ref was not hashed: %+v", report.Rows[0])
	}
}

func TestParityReportHashesNonCanonicalOpaqueRefs(t *testing.T) {
	now := time.Now().UTC()
	query := "syntheticsentinel"
	tests := []struct {
		name     string
		consumer string
		k        int
		ref      string
	}{
		{name: "claim", consumer: retrievalparity.ConsumerQuery, k: 10, ref: "claim://" + query + "_aaaaaaaaaaaa"},
		{name: "candidate", consumer: retrievalparity.ConsumerRecall, ref: query + "_aaaaaaaaaaaa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityTestHome(t)
			fixtureRow := retrievalparity.FixtureRow{
				Consumer:     tt.consumer,
				Query:        query,
				ExpectedRefs: []string{tt.ref},
			}
			replayRow := retrievalparity.NewReplayRow(fixtureRow, []string{tt.ref}, time.Millisecond, true)
			report := retrievalparity.BuildReport(
				retrievalparity.HashQuery("synthetic opaque fixture"),
				tt.consumer,
				tt.k,
				retrievalparity.Coverage{},
				[]retrievalparity.ReplayRow{replayRow},
				now,
			)
			reportPath, err := retrievalparity.WriteReport(homePath, report)
			if err != nil {
				t.Fatalf("write opaque report: %v", err)
			}
			persisted, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(persisted), query) {
				t.Fatalf("persisted opaque report leaked raw query: %s", persisted)
			}
			if !slices.Contains(report.Rows[0].ResultRefs, retrievalparity.HashQuery(tt.ref)) {
				t.Fatalf("noncanonical opaque ref was not hashed: %+v", report.Rows[0])
			}
		})
	}
}

func TestParityFixtureRejectsUnknownFields(t *testing.T) {
	homePath := newParityTestHome(t)
	fixturePath := filepath.Join(homePath, "unknown-field-fixture.json")
	raw := `{"schema_version":"` + retrievalparity.FixtureSchemaVersion + `","classifier_version":"` + retrievalparity.ClassifierVersion + `","coverage":{"known_gaps":[]},"rows":[{"consumer":"query","query":"synthetic strict fixture","expected_ref":"claim://claim_aaaaaaaaaaaa"}]}`
	if err := os.WriteFile(fixturePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := retrievalparity.LoadFixture(fixturePath); err == nil {
		t.Fatal("fixture with misspelled expected_refs field loaded successfully")
	}
}

func TestParityReportRejectsWrongClassifierVersion(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixtureRow := retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerQuery,
		Query:        "synthetic classifier report",
		ExpectedRefs: []string{"claim://claim_aaaaaaaaaaaa"},
	}
	replayRow := retrievalparity.NewReplayRow(fixtureRow, fixtureRow.ExpectedRefs, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic classifier fixture"),
		retrievalparity.ConsumerQuery,
		10,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{replayRow},
		now,
	)
	report.ClassifierVersion = ""
	if _, err := retrievalparity.WriteReport(homePath, report); err == nil {
		t.Fatal("report with empty classifier_version persisted")
	}
}

func TestParitySchemaSeparatesHomeRefsFromReportRefs(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	positionRef := "position://fixture:parity::baseline_anchor::slot-aaaaaaaaaaaa"
	searchRef := "item_a#0"
	fixture := retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{
			{Consumer: retrievalparity.ConsumerOrient, Query: "synthetic orient", OrientInput: parityTestOrientInput(t, homePath), ExpectedRefs: []string{positionRef}},
			{Consumer: retrievalparity.ConsumerSearch, Query: "synthetic search", ExpectedRefs: []string{searchRef}},
		},
	}
	if _, err := retrievalparity.WriteFixture(homePath, "typed-raw-refs.json", fixture); err != nil {
		t.Fatalf("raw production refs should be valid in home-local fixture: %v", err)
	}
	fixture.Rows[0].ExpectedRefs = []string{retrievalparity.HashQuery(positionRef)}
	if _, err := retrievalparity.WriteFixture(homePath, "prehashed-fixture.json", fixture); err == nil {
		t.Fatal("pre-hashed orient ref should not be valid in fixture")
	}

	row := retrievalparity.NewReplayRow(retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerOrient,
		Query:        "synthetic orient",
		OrientInput:  parityTestOrientInput(t, homePath),
		ExpectedRefs: []string{positionRef},
	}, []string{positionRef}, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic validator fixture"),
		retrievalparity.ConsumerOrient,
		0,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{row},
		now,
	)
	report.Rows[0].ExpectedRefs = []string{positionRef}
	report.Rows[0].ResultRefs = []string{positionRef}
	if _, err := retrievalparity.WriteReport(homePath, report); err == nil {
		t.Fatal("raw semantic refs should not be valid in persisted report")
	}
}

func TestParityReplayRejectsUntypedReportFields(t *testing.T) {
	now := time.Now().UTC()
	query := "synthetic adversarial privacy sentinel"
	tests := []struct {
		name    string
		fixture retrievalparity.Fixture
	}{
		{
			name: "expected ref",
			fixture: retrievalparity.Fixture{
				SchemaVersion:     retrievalparity.FixtureSchemaVersion,
				ClassifierVersion: retrievalparity.ClassifierVersion,
				Rows: []retrievalparity.FixtureRow{{
					Consumer:     retrievalparity.ConsumerQuery,
					Query:        query,
					ExpectedRefs: []string{query},
				}},
			},
		},
		{
			name: "coverage gap",
			fixture: retrievalparity.Fixture{
				SchemaVersion:     retrievalparity.FixtureSchemaVersion,
				ClassifierVersion: retrievalparity.ClassifierVersion,
				Coverage: retrievalparity.Coverage{
					KnownGaps: []string{query},
				},
				Rows: []retrievalparity.FixtureRow{{
					Consumer:     retrievalparity.ConsumerQuery,
					Query:        query,
					ExpectedRefs: []string{"claim://claim_000000000000"},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityTestHome(t)
			fixturePath := filepath.Join(homePath, "adversarial-fixture.json")
			data, err := json.Marshal(tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixturePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			_, reportPath, code, replayErr := runMemoryParityReplay(homePath, store, []string{
				"--fixture", fixturePath,
				"--consumer", retrievalparity.ConsumerQuery,
			}, now)
			if replayErr == nil || code != 1 || reportPath != "" {
				t.Fatalf("replay code=%d report=%q err=%v, want rejected fixture with no report", code, reportPath, replayErr)
			}
			if strings.Contains(replayErr.Error(), query) {
				t.Fatalf("fixture rejection leaked raw query: %q", replayErr)
			}
			matches, err := filepath.Glob(filepath.Join(homePath, "reviews", retrievalparity.ReviewArea, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("rejected fixture persisted report artifacts: %v", matches)
			}
		})
	}
}

func TestParityFreezeBuildsFixtureFromJournal(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic repeated freeze query"
	claimA := "claim://claim_aaaaaaaaaaaa"
	claimB := "claim://claim_bbbbbbbbbbbb"
	searchChunk := "item_a#0"
	orientInput := parityTestOrientInput(t, homePath)
	retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerQuery, query, []string{claimA}, 2*time.Millisecond, true, now.Add(-2*time.Hour)))
	retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerQuery, query, []string{claimB}, 3*time.Millisecond, true, now.Add(-time.Hour)))
	retrievalparity.Record(homePath, retrievalparity.NewOrientRow(query, *orientInput, []string{claimA}, time.Millisecond, true, now.Add(-30*time.Minute)))
	retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerSearch, query, []string{searchChunk}, time.Millisecond, true, now.Add(-15*time.Minute)))
	controlReportPath := writeParityControlReport(t, homePath, now)

	fixture, fixturePath, code, err := runMemoryParityFreeze(homePath, []string{
		"--from-journal",
		"--window-days", "1",
		"--control-report", controlReportPath,
		"--out", "reviews/retrieval-parity/frozen-synthetic.json",
	}, now)
	if err != nil || code != 0 {
		t.Fatalf("freeze code=%d err=%v fixture=%+v", code, err, fixture)
	}
	if fixture.SchemaVersion != retrievalparity.FixtureSchemaVersion || fixture.ControlReportRef == "" || len(fixture.Rows) != 3 {
		t.Fatalf("frozen fixture=%+v", fixture)
	}
	if filepath.Dir(fixturePath) != filepath.Join(homePath, "reviews", retrievalparity.ReviewArea) {
		t.Fatalf("fixture path escaped review area: %s", fixturePath)
	}
	for _, row := range fixture.Rows {
		if row.Consumer == retrievalparity.ConsumerQuery && !sameStringSet(row.ExpectedRefs, []string{claimA, claimB}) {
			t.Fatalf("freeze did not union repeated refs: %+v", row)
		}
		if row.Consumer == retrievalparity.ConsumerSearch && !sameStringSet(row.ExpectedRefs, []string{searchChunk}) {
			t.Fatalf("freeze did not preserve corpus chunk refs: %+v", row)
		}
	}
	loaded, _, err := retrievalparity.LoadFixture(fixturePath)
	if err != nil || len(loaded.Rows) != len(fixture.Rows) {
		t.Fatalf("load frozen fixture: rows=%d err=%v", len(loaded.Rows), err)
	}
}

func TestParityFreezePreservesZeroTokenTraffic(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	retrievalparity.Record(homePath, retrievalparity.NewOrientRow(
		"",
		*parityTestOrientInput(t, homePath),
		nil,
		time.Millisecond,
		true,
		now.Add(-time.Hour),
	))
	controlReportPath := writeParityControlReport(t, homePath, now)

	fixture, fixturePath, code, err := runMemoryParityFreeze(homePath, []string{
		"--from-journal",
		"--window-days", "1",
		"--control-report", controlReportPath,
	}, now)
	if err != nil || code != 0 {
		t.Fatalf("freeze zero-token traffic code=%d err=%v", code, err)
	}
	if len(fixture.Rows) != 1 {
		t.Fatalf("frozen zero-token rows=%d, want 1", len(fixture.Rows))
	}
	row := fixture.Rows[0]
	if row.Consumer != retrievalparity.ConsumerOrient || row.Query != "" {
		t.Fatalf("frozen zero-token row=%+v", row)
	}
	if got := retrievalparity.ClassifyQuery(row.Consumer, row.Query); got != "orient/tokens:0" {
		t.Fatalf("zero-token class=%q", got)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
	}, now)
	if err != nil || code != 0 || report.K != 0 || len(report.Rows) != 1 || report.Rows[0].QueryClass != "orient/tokens:0" {
		t.Fatalf("replay zero-token traffic code=%d err=%v report=%+v", code, err, report)
	}
}

func TestParityReplayRejectsOrientK(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:    retrievalparity.ConsumerOrient,
			Query:       "",
			OrientInput: parityTestOrientInput(t, homePath),
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, reportPath, code, replayErr := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
		"--k", "1",
	}, now)
	if replayErr == nil || code != 2 || reportPath != "" {
		t.Fatalf("orient --k code=%d report=%q err=%v, want usage rejection", code, reportPath, replayErr)
	}
}

func TestParityReplayRejectsOrientFixtureWithoutExactInput(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := filepath.Join(homePath, "missing-orient-input.json")
	fixture := retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Coverage:          retrievalparity.Coverage{KnownGaps: []string{}},
		Rows: []retrievalparity.FixtureRow{{
			Consumer: retrievalparity.ConsumerOrient,
			Query:    "synthetic missing exact orient input",
		}},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
	}, now); err == nil || code == 0 || reportPath != "" || !strings.Contains(err.Error(), "orient_input") {
		t.Fatalf("orient replay without exact input code=%d report=%q err=%v", code, reportPath, err)
	}
}

func TestParityReplayPreservesExactOrientTuple(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	query := "synthetic exact orient tuple"
	tuple := retrievalparity.OrientInput{
		Harness:     "claude_code",
		Cwd:         filepath.Join(t.TempDir(), "library"),
		RepoKind:    "library",
		RepoKindVia: "flag",
		Mode:        "review",
		Topic:       "topic:synthetic",
	}
	production, err := contract.PackWithCapabilities(contract.Input{
		Harness:     tuple.Harness,
		Cwd:         tuple.Cwd,
		Task:        query,
		RepoKind:    tuple.RepoKind,
		RepoKindVia: tuple.RepoKindVia,
		Mode:        tuple.Mode,
		Topic:       tuple.Topic,
	}, orientPolicy(), homePath, now)
	if err != nil {
		t.Fatalf("build production tuple pack: %v", err)
	}
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerOrient,
			Query:        query,
			OrientInput:  &tuple,
			ExpectedRefs: production.ActiveMemory,
		}},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerOrient,
	}, now)
	if err != nil || code != 0 || len(report.Rows) != 1 {
		t.Fatalf("exact-tuple replay code=%d err=%v report=%+v", code, err, report)
	}
	wantInputRef := retrievalparity.HashOrientInput(tuple, query)
	if report.Rows[0].InputSHA256 != wantInputRef {
		t.Fatalf("orient input identity=%q, want %q", report.Rows[0].InputSHA256, wantInputRef)
	}
	wantRefs, _ := parityPersistedOrientRefs(production.ActiveMemory)
	if !sameStringSet(report.Rows[0].ResultRefs, wantRefs) {
		t.Fatalf("exact-tuple replay refs=%v production=%v", report.Rows[0].ResultRefs, production.ActiveMemory)
	}

	changed := tuple
	changed.Topic = "topic:other"
	if retrievalparity.HashOrientInput(changed, query) == wantInputRef {
		t.Fatal("topic did not participate in orient input identity")
	}
}

func TestParityFixtureAcceptsSafeLegacyClaimRefsAndHashesReports(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	legacyRef := "claim://brain-D1"
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer:     retrievalparity.ConsumerQuery,
			Query:        "synthetic legacy claim ref",
			ExpectedRefs: []string{legacyRef},
		}},
	})
	if fixturePath == "" {
		t.Fatal("safe legacy fixture was not written")
	}
	row := retrievalparity.NewReplayRow(retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerQuery,
		Query:        "synthetic legacy claim ref",
		ExpectedRefs: []string{legacyRef},
	}, []string{legacyRef}, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic legacy fixture"),
		retrievalparity.ConsumerQuery,
		10,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{row},
		now,
	)
	reportPath, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write legacy-ref report: %v", err)
	}
	persisted, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), legacyRef) {
		t.Fatalf("persisted report leaked legacy ref: %s", persisted)
	}
	want := retrievalparity.HashQuery(legacyRef)
	if !slices.Contains(report.Rows[0].ExpectedRefs, want) || !slices.Contains(report.Rows[0].ResultRefs, want) {
		t.Fatalf("legacy refs were not hashed: %+v", report.Rows[0])
	}
}

func TestParityFreezeRejectsInvalidWindowEndpointsBeforeArtifact(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	retrievalparity.Record(homePath, retrievalparity.NewRow(
		retrievalparity.ConsumerQuery,
		"synthetic endpoint validation",
		nil,
		time.Millisecond,
		true,
		now,
	))
	tests := []struct {
		name       string
		windowDays int
		at         time.Time
		want       string
	}{
		{name: "overflowed start", windowDays: int(^uint(0) >> 1), at: now, want: "window_start"},
		{name: "invalid end", windowDays: 1, at: time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC), want: "window_end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := retrievalparity.FreezeFromJournal(homePath, tt.windowDays, "", "", tt.at)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("freeze err=%v, want endpoint error containing %q", err, tt.want)
			}
		})
	}
	matches, err := filepath.Glob(filepath.Join(homePath, "reviews", retrievalparity.ReviewArea, "fixture_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("invalid freeze wrote artifacts: %v", matches)
	}
}

func TestParityReportSeparatesControlValidityAndFreezeRequiresControlReport(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	maskedRef := "claim://claim_aaaaaaaaaaaa"
	row := retrievalparity.NewReplayRow(retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerQuery,
		Query:        "synthetic negative control",
		ExpectedRefs: []string{maskedRef},
		NegativeControl: &retrievalparity.NegativeControl{
			MaskedRef: maskedRef,
		},
	}, nil, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic negative control fixture"),
		retrievalparity.ConsumerQuery,
		10,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{row},
		now,
	)
	if report.ParityPassed {
		t.Fatal("masked negative control must remain a parity failure")
	}
	if report.NegativeControlValid == nil || !*report.NegativeControlValid {
		t.Fatalf("negative control validity=%v, want typed true", report.NegativeControlValid)
	}
	controlReportPath, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write control report: %v", err)
	}
	retrievalparity.Record(homePath, retrievalparity.NewRow(
		retrievalparity.ConsumerQuery,
		"synthetic traffic to freeze",
		[]string{maskedRef},
		time.Millisecond,
		true,
		now,
	))
	if _, path, code, err := runMemoryParityFreeze(homePath, []string{
		"--from-journal",
		"--window-days", "1",
	}, now); err == nil || code != 2 || path != "" {
		t.Fatalf("freeze without control code=%d path=%q err=%v, want usage rejection", code, path, err)
	}
	fixture, path, code, err := runMemoryParityFreeze(homePath, []string{
		"--from-journal",
		"--window-days", "1",
		"--control-report", controlReportPath,
	}, now)
	if err != nil || code != 0 || path == "" || fixture.ControlReportRef == "" {
		t.Fatalf("freeze with control code=%d path=%q err=%v fixture=%+v", code, path, err, fixture)
	}
}

func TestParityReplayFailsUnavailableEngineWithNoExpectedRefs(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer: retrievalparity.ConsumerQuery,
			Query:    "synthetic empty expected unavailable engine",
		}},
	})
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", filepath.Join(t.TempDir(), "missing-engine"))
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code == 0 || report.ParityPassed || len(report.Rows) != 1 || report.Rows[0].EngineAvailable {
		t.Fatalf("unavailable empty-expected replay code=%d err=%v report=%+v", code, err, report)
	}
}

func TestParityReplayCheckpointResumeRejectsTamperAndWrongIdentity(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{
			{Consumer: retrievalparity.ConsumerQuery, Query: "synthetic checkpoint one"},
			{Consumer: retrievalparity.ConsumerQuery, Query: "synthetic checkpoint two"},
		},
	})
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code != 0 || len(report.Rows) != 2 {
		t.Fatalf("initial checkpoint replay code=%d err=%v report=%+v", code, err, report)
	}
	_, fixtureRef, err := retrievalparity.LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	identity := retrievalparity.NewReplayIdentity(fixtureRef, retrievalparity.ConsumerQuery, 10)
	checkpointPath := retrievalparity.CheckpointPath(homePath, identity)
	checkpoint, err := retrievalparity.LoadCheckpoint(checkpointPath, identity)
	if err != nil || checkpoint.Completed != 2 || len(checkpoint.Rows) != 2 {
		t.Fatalf("load complete checkpoint=%+v err=%v", checkpoint, err)
	}
	checkpoint.Completed = 1
	checkpoint.Rows = checkpoint.Rows[:1]
	if err := retrievalparity.WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatalf("write partial checkpoint: %v", err)
	}
	resumed, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--resume-from", checkpointPath,
	}, now.Add(time.Second))
	if err != nil || code != 0 || len(resumed.Rows) != 2 {
		t.Fatalf("resume code=%d err=%v report=%+v", code, err, resumed)
	}
	if temps, _ := filepath.Glob(filepath.Join(filepath.Dir(checkpointPath), ".*.tmp-*")); len(temps) != 0 {
		t.Fatalf("atomic checkpoint left temp files: %v", temps)
	}

	raw, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(raw, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["completed"] = float64(1)
	tamperedRaw, err := json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, append(tamperedRaw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--resume-from", checkpointPath,
	}, now.Add(2*time.Second)); err == nil || code == 0 || reportPath != "" || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("tampered resume code=%d report=%q err=%v", code, reportPath, err)
	}

	if err := retrievalparity.WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	otherFixturePath := writeParityFixtureNamed(t, homePath, "other-fixture.json", retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Rows: []retrievalparity.FixtureRow{{
			Consumer: retrievalparity.ConsumerQuery,
			Query:    "synthetic different identity",
		}},
	})
	if _, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", otherFixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--resume-from", checkpointPath,
	}, now.Add(3*time.Second)); err == nil || code == 0 || reportPath != "" || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong-identity resume code=%d report=%q err=%v", code, reportPath, err)
	}
}

func TestParityReplayCheckpointResumesAfterRecordedRuntimeGap(t *testing.T) {
	now := time.Now().UTC()
	homePath := newParityTestHome(t)
	fixturePath := writeParityFixture(t, homePath, retrievalparity.Fixture{
		SchemaVersion:     retrievalparity.FixtureSchemaVersion,
		ClassifierVersion: retrievalparity.ClassifierVersion,
		Coverage: retrievalparity.Coverage{
			KnownGaps: []string{"fixture_window_unrecorded"},
		},
		Rows: []retrievalparity.FixtureRow{
			{Consumer: retrievalparity.ConsumerQuery, Query: "synthetic unavailable checkpoint one"},
			{Consumer: retrievalparity.ConsumerQuery, Query: "synthetic unavailable checkpoint two"},
		},
	})
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", filepath.Join(t.TempDir(), "missing-engine"))
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	initial, _, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
	}, now)
	if err != nil || code == 0 || len(initial.Rows) != 2 ||
		!slices.Contains(initial.Coverage.KnownGaps, "fixture_window_unrecorded") ||
		!slices.Contains(initial.Coverage.KnownGaps, "query_engine_unavailable") {
		t.Fatalf("initial unavailable replay code=%d err=%v report=%+v", code, err, initial)
	}

	_, fixtureRef, err := retrievalparity.LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	identity := retrievalparity.NewReplayIdentity(fixtureRef, retrievalparity.ConsumerQuery, 10)
	checkpointPath := retrievalparity.CheckpointPath(homePath, identity)
	checkpoint, err := retrievalparity.LoadCheckpoint(checkpointPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Completed = 1
	checkpoint.Rows = checkpoint.Rows[:1]
	if err := retrievalparity.WriteCheckpoint(homePath, &checkpoint); err != nil {
		t.Fatalf("write interrupted unavailable checkpoint: %v", err)
	}

	resumed, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
		"--fixture", fixturePath,
		"--consumer", retrievalparity.ConsumerQuery,
		"--resume-from", checkpointPath,
	}, now.Add(time.Second))
	if err != nil || code == 0 || reportPath == "" || len(resumed.Rows) != 2 {
		t.Fatalf("resume unavailable checkpoint code=%d report=%q err=%v rows=%d", code, reportPath, err, len(resumed.Rows))
	}
	if !slices.Contains(resumed.Coverage.KnownGaps, "fixture_window_unrecorded") ||
		!slices.Contains(resumed.Coverage.KnownGaps, "query_engine_unavailable") {
		t.Fatalf("resumed coverage lost baseline/runtime gaps: %+v", resumed.Coverage)
	}
}

func TestParityReplayCheckpointBindsExactFixturePrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Now().UTC()
	claimA := "claim://claim_aaaaaaaaaaaa"
	claimB := "claim://claim_bbbbbbbbbbbb"
	tests := []struct {
		name     string
		consumer string
		mutate   func(*retrievalparity.Checkpoint, retrievalparity.FixtureRow)
	}{
		{
			name:     "coverage",
			consumer: retrievalparity.ConsumerQuery,
			mutate: func(checkpoint *retrievalparity.Checkpoint, _ retrievalparity.FixtureRow) {
				checkpoint.Coverage.KnownGaps = []string{"query_engine_unavailable"}
			},
		},
		{
			name:     "query identity",
			consumer: retrievalparity.ConsumerQuery,
			mutate: func(checkpoint *retrievalparity.Checkpoint, _ retrievalparity.FixtureRow) {
				checkpoint.Rows[0].QuerySHA256 = retrievalparity.HashQuery("synthetic checkpoint impostor")
			},
		},
		{
			name:     "expected refs",
			consumer: retrievalparity.ConsumerQuery,
			mutate: func(checkpoint *retrievalparity.Checkpoint, fixtureRow retrievalparity.FixtureRow) {
				fixtureRow.ExpectedRefs = []string{claimA}
				checkpoint.Rows[0] = retrievalparity.NewReplayRow(fixtureRow, nil, time.Millisecond, true)
			},
		},
		{
			name:     "negative control",
			consumer: retrievalparity.ConsumerQuery,
			mutate: func(checkpoint *retrievalparity.Checkpoint, fixtureRow retrievalparity.FixtureRow) {
				fixtureRow.NegativeControl = &retrievalparity.NegativeControl{MaskedRef: claimB}
				checkpoint.Rows[0] = retrievalparity.NewReplayRow(fixtureRow, nil, time.Millisecond, true)
			},
		},
		{
			name:     "orient input",
			consumer: retrievalparity.ConsumerOrient,
			mutate: func(checkpoint *retrievalparity.Checkpoint, _ retrievalparity.FixtureRow) {
				checkpoint.Rows[0].InputSHA256 = retrievalparity.HashQuery("synthetic checkpoint wrong orient input")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := newParityTestHome(t)
			fixtureRow := retrievalparity.FixtureRow{
				Consumer:        tt.consumer,
				Query:           "synthetic checkpoint exact prefix",
				ExpectedRefs:    []string{claimA, claimB},
				NegativeControl: &retrievalparity.NegativeControl{MaskedRef: claimA},
			}
			if tt.consumer == retrievalparity.ConsumerOrient {
				fixtureRow.OrientInput = parityTestOrientInput(t, homePath)
				fixtureRow.ExpectedRefs = nil
				fixtureRow.NegativeControl = nil
			}
			fixture := retrievalparity.Fixture{
				SchemaVersion:     retrievalparity.FixtureSchemaVersion,
				ClassifierVersion: retrievalparity.ClassifierVersion,
				Coverage:          retrievalparity.Coverage{KnownGaps: []string{}},
				Rows: []retrievalparity.FixtureRow{
					fixtureRow,
					fixtureRow,
				},
			}
			fixture.Rows[1].Query = "synthetic checkpoint remaining row"
			fixturePath := writeParityFixture(t, homePath, fixture)
			_, fixtureRef, err := retrievalparity.LoadFixture(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			k := 0
			if tt.consumer == retrievalparity.ConsumerQuery {
				k = 10
			}
			identity := retrievalparity.NewReplayIdentity(fixtureRef, tt.consumer, k)
			checkpoint := retrievalparity.NewCheckpoint(identity, fixture.Coverage)
			checkpoint.Rows = []retrievalparity.ReplayRow{
				retrievalparity.NewReplayRow(fixtureRow, nil, time.Millisecond, true),
			}
			checkpoint.Completed = 1
			tt.mutate(&checkpoint, fixtureRow)
			if err := retrievalparity.WriteCheckpoint(homePath, &checkpoint); err != nil {
				t.Fatalf("write validly sealed mismatched checkpoint: %v", err)
			}
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			checkpointPath := retrievalparity.CheckpointPath(homePath, identity)
			_, reportPath, code, err := runMemoryParityReplay(homePath, store, []string{
				"--fixture", fixturePath,
				"--consumer", tt.consumer,
				"--resume-from", checkpointPath,
			}, now)
			if err == nil || code == 0 || reportPath != "" || !strings.Contains(err.Error(), "checkpoint") {
				t.Fatalf("mismatched %s checkpoint code=%d report=%q err=%v", tt.name, code, reportPath, err)
			}
		})
	}
}

func newParityTestHome(t *testing.T) string {
	t.Helper()
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	return homePath
}

func parityReplayArtifactPaths(t *testing.T, homePath string) []string {
	t.Helper()
	root := filepath.Join(homePath, "reviews", retrievalparity.ReviewArea)
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("list parity replay artifacts: %v", err)
	}
	slices.Sort(paths)
	return paths
}

func seedParityClaim(t *testing.T, homePath, object string, now time.Time) string {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(object))
	captured, err := store.Capture([]mem.Candidate{{
		Entity:         "fixture:retrieval-parity",
		Predicate:      "baseline_anchor",
		Object:         object,
		Confidence:     0.99,
		Derivation:     "deterministic",
		ClaimType:      "architecture",
		PromotionBasis: candidates.BasisExplicitConfirmation,
		SourceTraceRef: "trace_parity_" + hex.EncodeToString(sum[:])[:12],
		SnippetHash:    "sha256:" + hex.EncodeToString(sum[:]),
	}}, now)
	if err != nil {
		t.Fatalf("capture parity claim: %v", err)
	}
	if _, err := store.Adjudicate(captured[0].ID, candidates.Park, "work_parity_fixture", now); err != nil {
		t.Fatalf("park parity claim: %v", err)
	}
	confirmation, err := store.RecordOperatorConfirmation(
		captured[0].ID,
		"test fixture confirms "+captured[0].ID,
		"operator:test",
		now,
	)
	if err != nil {
		t.Fatalf("record parity confirmation: %v", err)
	}
	result, err := store.AdjudicateExplicitConfirmation(captured[0].ID, confirmation.ID, "work_parity_fixture", now)
	if err != nil {
		t.Fatalf("promote parity claim: %v", err)
	}
	if result.Claim == nil {
		t.Fatalf("promotion returned no claim: %+v", result)
	}
	return "claim://" + result.Claim.ID
}

func writeParityFixture(t *testing.T, homePath string, fixture retrievalparity.Fixture) string {
	t.Helper()
	return writeParityFixtureNamed(t, homePath, "synthetic-fixture.json", fixture)
}

func writeParityFixtureNamed(t *testing.T, homePath, name string, fixture retrievalparity.Fixture) string {
	t.Helper()
	path, err := retrievalparity.WriteFixture(homePath, name, fixture)
	if err != nil {
		t.Fatalf("write parity fixture: %v", err)
	}
	return path
}

func parityTestOrientInput(t *testing.T, homePath string) *retrievalparity.OrientInput {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get parity fixture cwd: %v", err)
	}
	in, err := parseOrientArgsWithHome([]string{"--harness", "codex", "--cwd", cwd}, homePath)
	if err != nil {
		t.Fatalf("resolve parity fixture orient input: %v", err)
	}
	tuple := parityOrientInput(in)
	return &tuple
}

func writeParityControlReport(t *testing.T, homePath string, now time.Time) string {
	t.Helper()
	maskedRef := "claim://claim_cccccccccccc"
	row := retrievalparity.NewReplayRow(retrievalparity.FixtureRow{
		Consumer:     retrievalparity.ConsumerQuery,
		Query:        "synthetic freeze control",
		ExpectedRefs: []string{maskedRef},
		NegativeControl: &retrievalparity.NegativeControl{
			MaskedRef: maskedRef,
		},
	}, nil, time.Millisecond, true)
	report := retrievalparity.BuildReport(
		retrievalparity.HashQuery("synthetic freeze control fixture"),
		retrievalparity.ConsumerQuery,
		10,
		retrievalparity.Coverage{},
		[]retrievalparity.ReplayRow{row},
		now,
	)
	path, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		t.Fatalf("write parity control report: %v", err)
	}
	return path
}

func cloneParityHomeWithoutClaim(t *testing.T, sourceHome, claimID string, now time.Time) string {
	t.Helper()
	maskedHome := filepath.Join(t.TempDir(), "home")
	if err := os.CopyFS(maskedHome, os.DirFS(sourceHome)); err != nil {
		t.Fatalf("clone scratch home: %v", err)
	}
	if err := os.Chmod(maskedHome, 0o700); err != nil {
		t.Fatalf("secure cloned scratch home: %v", err)
	}
	if err := home.Init(maskedHome, "test", now); err != nil {
		t.Fatalf("init cloned scratch home: %v", err)
	}
	claimsPath := filepath.Join(maskedHome, "ledger", "claims.jsonl")
	input, err := os.Open(claimsPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []byte
	found := false
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var claim mem.Claim
		if err := json.Unmarshal(line, &claim); err != nil {
			input.Close()
			t.Fatalf("parse cloned claim: %v", err)
		}
		if claim.ID == claimID {
			found = true
			continue
		}
		kept = append(kept, line...)
		kept = append(kept, '\n')
	}
	if err := scanner.Err(); err != nil {
		input.Close()
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("claim %s not present in cloned ledger", claimID)
	}
	if err := os.WriteFile(claimsPath, kept, 0o644); err != nil {
		t.Fatal(err)
	}
	return maskedHome
}

func runParityReplayCLI(t *testing.T, homePath, fixturePath, consumer string) (int, retrievalparity.Report) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestParityReplayCLIHelper$")
	cmd.Env = parityTestEnv(os.Environ(), map[string]string{
		parityCLIHelperEnv:        "1",
		parityCLIHelperFixture:    fixturePath,
		parityCLIHelperConsumer:   consumer,
		"COMPOUND_HOME":           homePath,
		"COMPOUND_MEM_ENGINE_BIN": "",
	})
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !strings.Contains(err.Error(), "exit status") || !asExitError(err, &exitErr) {
			t.Fatalf("run parity CLI helper: %v\n%s", err, out)
		}
		exitCode = exitErr.ExitCode()
	}
	var report retrievalparity.Report
	if jsonErr := json.Unmarshal(out, &report); jsonErr != nil {
		t.Fatalf("parse parity CLI report (exit=%d): %v\n%s", exitCode, jsonErr, out)
	}
	return exitCode, report
}

func runParityProfileReplayCLI(t *testing.T, homePath, fixturePath, profile, hitID string) (int, []byte) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestParityReplayCLIHelper$")
	cmd.Env = parityTestEnv(os.Environ(), map[string]string{
		parityCLIHelperEnv:             "1",
		parityCLIHelperFixture:         fixturePath,
		parityCLIHelperConsumer:        retrievalparity.ConsumerQuery,
		parityCLIHelperProfile:         profile,
		parityFakeSidecarEnv:           "1",
		parityFakeSidecarHitID:         hitID,
		parityFakeSidecarMode:          "",
		"COMPOUND_HOME":                homePath,
		"COMPOUND_MEM_ENGINE_BIN":      os.Args[0] + " -test.run=" + parityFakeSidecarTestRun,
		"COMPOUND_MEM_ENGINE_EMBEDDER": "",
		"COMPOUND_MEMORY_CLI_PROFILE":  "",
	})
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, out
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run profiled parity CLI helper: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), out
}

func runParityFailureCLI(t *testing.T, homePath, consumer, query string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestParityFailureCLIHelper$")
	cmd.Env = parityTestEnv(os.Environ(), map[string]string{
		parityFailureHelperEnv:    consumer,
		parityFailureHelperQuery:  query,
		"COMPOUND_HOME":           homePath,
		"COMPOUND_MEM_ENGINE_BIN": "",
	})
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run parity failure helper: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func assertUnavailableParityRow(t *testing.T, homePath, consumer, query string) {
	t.Helper()
	rows, err := retrievalparity.LoadJournal(homePath)
	if err != nil {
		t.Fatalf("load failure journal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("failure journal rows=%d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Consumer != consumer || row.Query != query || row.EngineAvailable || len(row.ResultRefs) != 0 || row.ResultCount != 0 {
		t.Fatalf("failure journal row=%+v, want unavailable %s observation with zero refs", row, consumer)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

func parityTestEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func captureParityOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	fn()
	if err := outW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errW.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	errBytes, err := io.ReadAll(errR)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(errBytes)
}

func sameStringSet(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func parityPersistedOrientRefs(refs []string) ([]string, bool) {
	out := make([]string, 0, len(refs))
	hasPosition := false
	for _, ref := range refs {
		if strings.HasPrefix(ref, "position://") {
			hasPosition = true
			ref = retrievalparity.HashQuery(ref)
		}
		out = append(out, ref)
	}
	return out, hasPosition
}

// parityFoldedPositionHashes returns the persisted (sha256-hashed) refs of every
// position the store has folded. Used by tests to keep ExpectedRefs in sync
// with the production orient path, which includes folded positions in
// ActiveMemory alongside the underlying claim. The fixture accepts the raw
// position://<id>::slot-... form; persistedRefs converts it to the
// sha256-hashed form for the row.
func parityFoldedPositionHashes(t *testing.T, store *mem.Store) []string {
	t.Helper()
	positions, err := store.LoadFoldedPositions()
	if err != nil {
		t.Fatalf("load folded positions: %v", err)
	}
	out := make([]string, 0, len(positions))
	for _, p := range positions {
		out = append(out, "position://"+p.ID)
	}
	return out
}

func validParitySHA(ref string) bool {
	if !strings.HasPrefix(ref, "sha256:") || len(ref) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(ref, "sha256:"))
	return err == nil
}
