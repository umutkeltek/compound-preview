package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/contract"
	"compound/internal/corpus"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/retrievalparity"
)

func cmdMemoryParityReplay(homePath string, store *mem.Store, args []string) {
	report, reportPath, code, err := runMemoryParityReplay(homePath, store, args, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory parity-replay:", err)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	}
	if hasFlag(args, "--json") {
		printJSON(report)
	} else {
		fmt.Printf("# memory parity-replay %s\n", report.ID)
		fmt.Printf("- consumer: %s\n", report.Consumer)
		fmt.Printf("- rows: %d\n", len(report.Rows))
		fmt.Printf("- parity_passed: %t\n", report.ParityPassed)
		if report.NegativeControlValid != nil {
			fmt.Printf("- negative_control_valid: %t\n", *report.NegativeControlValid)
		}
		fmt.Printf("- report: %s\n", reportPath)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func runMemoryParityReplay(homePath string, store *mem.Store, args []string, now time.Time) (retrievalparity.Report, string, int, error) {
	parsed, err := parseMemoryParityReplayArgs(args)
	if err != nil {
		return retrievalparity.Report{}, "", 2, err
	}
	fixturePath := parsed.Fixture
	consumer := strings.TrimSpace(parsed.Consumer)
	if fixturePath == "" || consumer == "" {
		return retrievalparity.Report{}, "", 2, fmt.Errorf("usage: compound memory parity-replay --fixture <file.json> --consumer orient|query [--k N (query only)] [--expect-engine ID] [--forbid-engine ID] [--resume-from <checkpoint.json>] [--json]")
	}
	expectEngine := strings.TrimSpace(parsed.ExpectEngine)
	forbidEngine := strings.TrimSpace(parsed.ForbidEngine)
	if consumer != retrievalparity.ConsumerOrient && consumer != retrievalparity.ConsumerQuery {
		return retrievalparity.Report{}, "", 2, fmt.Errorf("consumer must be orient or query")
	}
	rawK := parsed.K
	k := 0
	if consumer == retrievalparity.ConsumerOrient && rawK != "" {
		return retrievalparity.Report{}, "", 2, fmt.Errorf("--k is supported only for the query consumer")
	}
	if consumer == retrievalparity.ConsumerQuery {
		k = 10
	}
	if rawK != "" {
		parsed, err := strconv.Atoi(rawK)
		if err != nil || parsed <= 0 {
			return retrievalparity.Report{}, "", 2, fmt.Errorf("k must be a positive integer")
		}
		k = parsed
	}
	fixture, fixtureRef, err := retrievalparity.LoadFixture(fixturePath)
	if err != nil {
		return retrievalparity.Report{}, "", 1, err
	}
	selected := make([]retrievalparity.FixtureRow, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		if row.Consumer == consumer {
			selected = append(selected, row)
		}
	}
	if len(selected) == 0 {
		return retrievalparity.Report{}, "", 1, fmt.Errorf("fixture has no %s rows", consumer)
	}

	identity, err := retrievalparity.NewReplayIdentityWithDependency(
		fixtureRef,
		consumer,
		k,
		expectEngine,
		forbidEngine,
	)
	if err != nil {
		return retrievalparity.Report{}, "", 2, err
	}
	checkpoint := retrievalparity.NewCheckpoint(identity, fixture.Coverage)
	if resumePath := strings.TrimSpace(parsed.ResumeFrom); resumePath != "" {
		checkpoint, err = retrievalparity.LoadCheckpoint(resumePath, identity)
		if err != nil {
			return retrievalparity.Report{}, "", 1, err
		}
		if err := retrievalparity.ValidateCheckpointPrefix(checkpoint, fixture.Coverage, selected); err != nil {
			return retrievalparity.Report{}, "", 1, err
		}
	}
	coverage := checkpoint.Coverage
	for _, fixtureRow := range selected[checkpoint.Completed:] {
		resultRefs := []string{}
		engineAvailable := false
		runtimeGap := ""
		latency := time.Duration(0)
		var engineEvidence *retrievalparity.EngineDependencyEvidence
		var orientEvidence *retrievalparity.OrientConsumerEvidence
		switch consumer {
		case retrievalparity.ConsumerOrient:
			if fixtureRow.OrientInput == nil {
				return retrievalparity.Report{}, "", 1, fmt.Errorf("orient fixture row requires exact orient_input")
			}
			in := contract.Input{
				Harness:     fixtureRow.OrientInput.Harness,
				Cwd:         fixtureRow.OrientInput.Cwd,
				Task:        fixtureRow.Query,
				RepoKind:    fixtureRow.OrientInput.RepoKind,
				RepoKindVia: fixtureRow.OrientInput.RepoKindVia,
				Mode:        fixtureRow.OrientInput.Mode,
				Topic:       fixtureRow.OrientInput.Topic,
			}
			budget := 0
			if fixtureRow.OrientExpect != nil {
				budget = fixtureRow.OrientExpect.BodyBudgetTokens
			}
			latency, orientResult, orientErr := parityReplayOrientMaterialize(homePath, in, budget, now)
			evidence := parityOrientEngineEvidence(orientResult.EngineDependency)
			engineEvidence = &evidence
			if orientErr != nil {
				runtimeGap = "orient_retrieval_error"
				break
			}
			resultRefs = append(resultRefs, orientResult.Pack.ActiveMemory...)
			engineAvailable = true
			wire, err := parityOrientWireBytes(orientResult.Pack)
			if err != nil {
				return retrievalparity.Report{}, "", 1, fmt.Errorf("orient replay wire marshal: %w", err)
			}
			canaries, refs, admittedText := parityOrientFixtureCanaryMaterial(fixtureRow, orientResult.Pack)
			evidenceOri := retrievalparity.BuildOrientConsumerEvidence(
				wire,
				orientResult.Pack.ID,
				budget,
				orientResult.BodyPackUsedTokens,
				latency,
				refs,
				admittedText,
				canaries,
				orientResult.Degraded,
			)
			if fixtureRow.OrientExpect != nil {
				retrievalparity.ApplyOrientExpectations(&evidenceOri, *fixtureRow.OrientExpect)
			}
			orientEvidence = &evidenceOri
		case retrievalparity.ConsumerQuery:
			started := time.Now()
			claims, evidence, queryErr := parityQueryBreadthWithEvidence(store, fixtureRow.Query, k)
			latency = time.Since(started)
			engineEvidence = &evidence
			engineAvailable = queryErr == nil
			if queryErr == nil {
				resultRefs = parityClaimRefs(claims)
			} else {
				runtimeGap = parityQueryRuntimeGap(evidence)
			}
		}
		if runtimeGap != "" {
			coverage.KnownGaps = append(coverage.KnownGaps, runtimeGap)
		}
		switch {
		case orientEvidence != nil && engineEvidence != nil:
			row := retrievalparity.NewReplayRowWithEngineEvidence(fixtureRow, resultRefs, latency, engineAvailable, *engineEvidence, identity, runtimeGap)
			row.OrientConsumer = orientEvidence
			checkpoint.Rows = append(checkpoint.Rows, row)
		case engineEvidence != nil:
			checkpoint.Rows = append(checkpoint.Rows, retrievalparity.NewReplayRowWithEngineEvidence(fixtureRow, resultRefs, latency, engineAvailable, *engineEvidence, identity, runtimeGap))
		default:
			row := retrievalparity.NewReplayRow(fixtureRow, resultRefs, latency, engineAvailable, runtimeGap)
			row.OrientConsumer = orientEvidence
			checkpoint.Rows = append(checkpoint.Rows, row)
		}
		checkpoint.Completed = len(checkpoint.Rows)
		checkpoint.Coverage = coverage
		if err := retrievalparity.WriteCheckpoint(homePath, &checkpoint); err != nil {
			return retrievalparity.Report{}, "", 1, fmt.Errorf("write parity checkpoint: %w", err)
		}
	}
	report := retrievalparity.BuildReportWithIdentity(identity, checkpoint.Coverage, checkpoint.Rows, now)
	reportPath, err := retrievalparity.WriteReport(homePath, report)
	if err != nil {
		return report, "", 1, err
	}
	if !report.ParityPassed || (report.NegativeControlValid != nil && !*report.NegativeControlValid) {
		return report, reportPath, 1, nil
	}
	return report, reportPath, 0, nil
}

type memoryParityReplayArgs struct {
	Fixture      string
	Consumer     string
	K            string
	ExpectEngine string
	ForbidEngine string
	ResumeFrom   string
	// EngineProfile is applied by cmdMemory before dispatch; replay only validates the passthrough argv.
	EngineProfile string
	JSON          bool
}

func parseMemoryParityReplayArgs(args []string) (memoryParityReplayArgs, error) {
	var parsed memoryParityReplayArgs
	seen := make(map[string]bool)
	for i := 0; i < len(args); {
		name := args[i]
		if strings.Contains(name, "=") {
			return memoryParityReplayArgs{}, fmt.Errorf("unsupported parity-replay argument syntax %q", name)
		}
		if name == "--json" {
			if seen[name] {
				return memoryParityReplayArgs{}, fmt.Errorf("%s may be provided only once", name)
			}
			seen[name] = true
			parsed.JSON = true
			i++
			continue
		}
		switch name {
		case "--fixture", "--consumer", "--k", "--expect-engine", "--forbid-engine", "--resume-from", "--engine-profile":
		default:
			return memoryParityReplayArgs{}, fmt.Errorf("unsupported parity-replay argument %q", name)
		}
		if seen[name] {
			return memoryParityReplayArgs{}, fmt.Errorf("%s may be provided only once", name)
		}
		seen[name] = true
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || strings.TrimSpace(args[i+1]) == "" {
			return memoryParityReplayArgs{}, fmt.Errorf("%s requires a value", name)
		}
		value := args[i+1]
		switch name {
		case "--fixture":
			parsed.Fixture = value
		case "--consumer":
			parsed.Consumer = value
		case "--k":
			parsed.K = value
		case "--expect-engine":
			parsed.ExpectEngine = value
		case "--forbid-engine":
			parsed.ForbidEngine = value
		case "--resume-from":
			parsed.ResumeFrom = value
		case "--engine-profile":
			parsed.EngineProfile = value
		}
		i += 2
	}
	return parsed, nil
}

func parityOrientEngineEvidence(trace contract.EngineDependencyTrace) retrievalparity.EngineDependencyEvidence {
	return retrievalparity.EngineDependencyEvidence{
		Attempted:        trace.Attempted,
		RequestedEngine:  trace.RequestedEngine,
		ObservedEngine:   trace.ObservedEngine,
		Embedder:         trace.Embedder,
		CorpusGeneration: trace.CorpusGeneration,
		Outcome:          trace.Outcome,
		ErrorClass:       trace.ErrorClass,
	}
}

func parityQueryBreadthWithEvidence(store *mem.Store, query string, k int) ([]mem.Claim, retrievalparity.EngineDependencyEvidence, error) {
	eng, closeEng := memBreadthEngine()
	defer closeEng()
	evidence := retrievalparity.EngineDependencyEvidence{
		Attempted:       true,
		RequestedEngine: parityRequestedEngineIdentity(),
	}
	parityObserveEngineCapabilities(&evidence, eng.Capabilities(), false)
	claims, err := store.QueryBreadth(query, k, eng)
	if err != nil {
		evidence.Outcome, evidence.ErrorClass = parityEngineErrorEvidence(err)
		return nil, evidence, err
	}
	parityObserveEngineCapabilities(&evidence, eng.Capabilities(), true)
	if evidence.ObservedEngine == "" {
		evidence.Outcome = retrievalparity.EngineOutcomeRefused
		evidence.ErrorClass = retrievalparity.EngineErrorProtocolVersion
	} else {
		evidence.Outcome = retrievalparity.EngineOutcomeUsed
	}
	return claims, evidence, nil
}

func parityObserveEngineCapabilities(evidence *retrievalparity.EngineDependencyEvidence, caps mem.EngineCapabilities, successful bool) {
	if evidence == nil {
		return
	}
	if name := paritySafeEngineIdentity(caps.Name); name != "protocol" || successful {
		evidence.ObservedEngine = name
	}
	evidence.Embedder = paritySafeEmbedderIdentity(caps.Embedder)
	if caps.CorpusGen >= 0 {
		evidence.CorpusGeneration = caps.CorpusGen
	}
}

func parityRequestedEngineIdentity() string {
	if profile := paritySafeEngineIdentity(os.Getenv("COMPOUND_MEMORY_CLI_PROFILE")); profile != "" {
		return profile
	}
	if strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) != "" {
		return "protocol"
	}
	return "lexical"
}

func paritySafeEngineIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return ""
	}
	return value
}

func paritySafeEmbedderIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
			continue
		}
		return ""
	}
	return value
}

func parityEngineErrorEvidence(err error) (string, string) {
	switch {
	case errors.Is(err, mem.ErrEmbedderMismatch):
		return retrievalparity.EngineOutcomeRefused, retrievalparity.EngineErrorEmbedderMismatch
	case errors.Is(err, mem.ErrProtocolVersion):
		return retrievalparity.EngineOutcomeRefused, retrievalparity.EngineErrorProtocolVersion
	case errors.Is(err, mem.ErrEngineAhead):
		return retrievalparity.EngineOutcomeRefused, retrievalparity.EngineErrorEngineAhead
	case errors.Is(err, mem.ErrEngineServedUnownedClaim):
		return retrievalparity.EngineOutcomeRefused, retrievalparity.EngineErrorUnownedClaim
	case errors.Is(err, mem.ErrUnsupported):
		return retrievalparity.EngineOutcomeDegraded, retrievalparity.EngineErrorUnsupported
	case errors.Is(err, mem.ErrEngineUnavailable):
		return retrievalparity.EngineOutcomeDegraded, retrievalparity.EngineErrorUnavailable
	default:
		return retrievalparity.EngineOutcomeDegraded, retrievalparity.EngineErrorRetrieval
	}
}

func parityQueryRuntimeGap(evidence retrievalparity.EngineDependencyEvidence) string {
	if evidence.Outcome == retrievalparity.EngineOutcomeRefused {
		return "query_engine_refused"
	}
	if evidence.ErrorClass == retrievalparity.EngineErrorUnsupported || evidence.ErrorClass == retrievalparity.EngineErrorUnavailable {
		return "query_engine_unavailable"
	}
	return "query_retrieval_error"
}

func cmdMemoryParityFreeze(homePath string, args []string) {
	fixture, fixturePath, code, err := runMemoryParityFreeze(homePath, args, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory parity-freeze:", err)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	}
	fmt.Println("# memory parity-freeze")
	fmt.Printf("- rows: %d\n", len(fixture.Rows))
	fmt.Printf("- window_start: %s\n", fixture.Coverage.WindowStart)
	fmt.Printf("- window_end: %s\n", fixture.Coverage.WindowEnd)
	fmt.Printf("- known_gaps: %d\n", len(fixture.Coverage.KnownGaps))
	fmt.Printf("- fixture: %s\n", fixturePath)
}

func runMemoryParityFreeze(homePath string, args []string, now time.Time) (retrievalparity.Fixture, string, int, error) {
	if !hasFlag(args, "--from-journal") {
		return retrievalparity.Fixture{}, "", 2, fmt.Errorf("usage: compound memory parity-freeze --from-journal --window-days N --control-report <report.json> [--out <home-relative path>]")
	}
	rawDays := valueFlag(args, "--window-days")
	windowDays, err := strconv.Atoi(rawDays)
	if err != nil || windowDays <= 0 {
		return retrievalparity.Fixture{}, "", 2, fmt.Errorf("window-days must be a positive integer")
	}
	controlReportPath := strings.TrimSpace(valueFlag(args, "--control-report"))
	if controlReportPath == "" {
		return retrievalparity.Fixture{}, "", 2, fmt.Errorf("control-report is required")
	}
	fixture, path, err := retrievalparity.FreezeFromJournal(homePath, windowDays, valueFlag(args, "--out"), controlReportPath, now)
	if err != nil {
		return retrievalparity.Fixture{}, "", 1, err
	}
	return fixture, path, 0, nil
}

func parityClaimRefs(claims []mem.Claim) []string {
	refs := make([]string, 0, len(claims))
	for _, claim := range claims {
		refs = append(refs, "claim://"+claim.ID)
	}
	return refs
}

func parityRecallRefs(result mem.RecallResult) []string {
	refs := parityClaimRefs(result.Claims)
	for _, candidate := range result.EvidenceCandidates {
		refs = append(refs, candidate.ID)
	}
	return refs
}

func parityCorpusRefs(report corpus.QueryReport) []string {
	refs := make([]string, 0, len(report.Hits))
	for _, hit := range report.Hits {
		ref := strings.TrimSpace(hit.ChunkID)
		if ref == "" {
			ref = strings.TrimSpace(hit.ItemID)
		}
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func parityOrientInput(in contract.Input) retrievalparity.OrientInput {
	return retrievalparity.OrientInput{
		Harness:     in.Harness,
		Cwd:         in.Cwd,
		RepoKind:    in.RepoKind,
		RepoKindVia: in.RepoKindVia,
		Mode:        in.Mode,
		Topic:       in.Topic,
	}
}

// parityReplayOrientMaterialize drives the one cmd-layer materialization path
// the parity replay must use: shared runtime materialization, optional body
// materialization, sealing, and atomic persistence. The latency wrapper
// allows tests to inject a deterministic observed duration via
// parityReplayLatencyOverride; in production the override is the real
// time.Since(time.Now()).
func parityReplayOrientMaterialize(homePath string, in contract.Input, bodyBudget int, now time.Time) (time.Duration, orientMaterializationResult, error) {
	started := now
	if parityReplayLatencyOverride != nil {
		started = parityReplayLatencyOverride()
	}
	result, err := materializeOrientContextPackDetailed(homePath, in, bodyBudget, now)
	elapsed := time.Since(started)
	return elapsed, result, err
}

// parityReplayLatencyOverride is an injectable latency anchor for the orient
// materializer. nil = use real time.Now. Tests must guard the override with
// t.Cleanup and must not call t.Parallel (the override is process-wide).
var parityReplayLatencyOverride func() time.Time

// parityOrientWireBytes marshals the exact returned pack with the same
// indentation + trailing newline that contract.RecordRuntimeMaterialization
// uses to write it to disk. The wire hash matches the persisted file hash.
func parityOrientWireBytes(pack envelope.ContextPack) ([]byte, error) {
	body, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+1)
	out = append(out, body...)
	out = append(out, '\n')
	return out, nil
}

// parityOrientFixtureCanaryMaterial returns the fixture's canary strings,
// configured private body refs, and a map of admitted body text for each
// ref. The canaries and refs come from the row's OrientExpectations; the
// admitted text is read from the returned pack's body_pack.bodies. Reports
// carry only counts/booleans — this function never returns the canary or
// private text outside of the local variable.
func parityOrientFixtureCanaryMaterial(row retrievalparity.FixtureRow, pack envelope.ContextPack) (canaries []string, refs []string, admittedText map[string]string) {
	if row.OrientExpect != nil {
		canaries = append(canaries, row.OrientExpect.PrivateCanaries...)
		refs = append(refs, row.OrientExpect.PrivateBodyRefs...)
	}
	admittedText = map[string]string{}
	if pack.BodyPack == nil {
		return
	}
	for _, body := range pack.BodyPack.Bodies {
		if body.Ref == "" {
			continue
		}
		admittedText[body.Ref] = body.Text
	}
	return
}

// isBytesEqual is used by orient replay tests to verify the wire hash matches
// the persisted file's bytes.
func isBytesEqual(a, b []byte) bool { return bytes.Equal(a, b) }
