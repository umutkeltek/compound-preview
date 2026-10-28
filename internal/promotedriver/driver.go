// Package promotedriver is the governed auto-promote actuator for compound
// memory. It exists because capture loops run (session mining, product
// signals, lane receipts) while adjudication and promotion had no loop at
// all — the ledger stalled 2026-08-09 and stayed stalled sixteen days
// while the inbox grew past 133k files and doctor reported the file count
// as "backlog".
//
// Doctrine the driver enforces, never relaxes:
//
//   - Promotion stays PER-ID through the existing single-id adjudication
//     door. The batch door's refusal of --verdict promote (design decision
//     3) is untouched; the driver never calls it with promote.
//   - A candidate promotes only on a system-derived promotion basis it
//     already carries. The driver mints no basis; "the model extracted it"
//     is still not one.
//   - One lane at a time: a single-flight lock, a per-pass promote limit
//     (default 1), and budget-bounded cursor walks.
//   - Everything is receipted (append-only JSONL) and nothing is deleted:
//     the sweep parks or no_promotes, the dedupe phase no_promotes exact
//     duplicates in favor of a named keeper — every verdict is reversible
//     per-id through the same doors a human uses.
//   - The operator stays on irreversible/expunge decisions only: dry-run
//     is the default; apply is an explicit --confirm at the CLI, admitted
//     as a governed first-party command.
//
// Phase order matters: basis promotion and duplicate collapse run BEFORE
// the disposition sweep, because the sweep closes every remaining proposed
// candidate (park / no_promote by admission class).
package promotedriver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/candidates"
	"compound/internal/lanebeliefs"
	"compound/internal/mem"
	"compound/internal/product"
)

const (
	ReportSchemaVersion         = "compound.promotedriver.report.v0.5"
	frozenReportSchemaVersion   = "compound.promotedriver.report.v0.4"
	previousReportSchemaVersion = "compound.promotedriver.report.v0.3"
	legacyReportSchemaVersion   = "compound.promotedriver.report.v0.2"
	archivedReportSchemaVersion = "compound.promotedriver.report.v0.1"

	// DefaultPromoteLimit is the per-pass promotion ceiling: one lane at a
	// time, literally. Raising it is an explicit operator choice (--promote-limit).
	DefaultPromoteLimit = 1
	// DefaultWalkScanLimit bounds how many inbox entries one pass reads per
	// phase walk. Resumable passes keep each heartbeat cheap; the sweep
	// carries its own cursor across passes.
	DefaultWalkScanLimit = 20_000
	// DefaultSweepScanLimit bounds the disposition sweep's read budget per
	// pass; it resumes via cursor until the inbox is walked to the end.
	DefaultSweepScanLimit = 20_000

	lockFileName          = "lock/promote-driver.lock"
	stateDirName          = "state/promote-driver"
	receiptFileName       = "receipts.jsonl"
	cursorFileName        = "sweep-cursor.json"
	promoteCursorFileName = "promote-cursor.json"
	hashFileName          = "import-hashes.json"

	stagePlanned              = "planned"
	stageOperationRecorded    = "operation_recorded"
	stageFinalizationRecorded = "finalization_recorded"
	stageCommitted            = "committed"
	stagePartial              = "partial"
	stageFailed               = "failed"
	stageRecoveryRequired     = "recovery_required"
	stageAbandoned            = "abandoned"
)

// Options configures one driver pass. The zero value is a bounded PLAN pass
// (dry-run): Apply is the opt-in mutation switch, so a caller that
// constructs Options{} can never mutate — the safe mode is the
// language-level zero value, not just a CLI default.
type Options struct {
	Apply bool // false (zero) = report the plan without mutating; true = the --confirm door

	PromoteLimit   int // max promotions this pass (default DefaultPromoteLimit)
	WalkScanLimit  int // read budget for the basis/dedupe walk (default DefaultWalkScanLimit)
	SweepScanLimit int // read budget for the disposition sweep (default DefaultSweepScanLimit)

	// RegistryPaths are product-registry project-contract files to refresh
	// deterministically: a changed content hash re-runs the trusted
	// deterministic import; an unchanged hash is a no-op. This is the
	// importer whose stall froze the ledger 2026-08-09.
	RegistryPaths []string

	WorkID               string // governance work id stamped on the pass's verdicts
	passID               string
	reservedCandidateIDs map[string]bool

	// Recover routes this pass through the recovery door instead of the
	// normal apply path. The recovery door either completes the frozen
	// pass (every recorded target still matches the live inbox) or
	// abandonments it (a target drifted; record a terminal `abandoned`
	// receipt so the next run plans fresh). The flag is the only mutation
	// door that can clear a frozen pass without re-running it.
	Recover bool
}

// Run executes one driver pass against homePath. Apply passes return a
// non-nil error for partial, failed, and recovery_required outcomes so an
// unattended scheduler cannot mistake a durable non-committed receipt for
// success.
//
// PLAN is read-only. APPLY freezes a reconstructible planned intent before
// mutation, then appends exactly one terminal receipt.
func Run(homePath string, opts Options, now time.Time) (Report, error) {
	now = now.UTC()
	report := Report{
		SchemaVersion: ReportSchemaVersion,
		PassID:        passID(now),
		Home:          homePath,
		DryRun:        !opts.Apply,
		StartedAt:     rfc3339(now),
	}
	if opts.PromoteLimit <= 0 {
		opts.PromoteLimit = DefaultPromoteLimit
	}
	if opts.WalkScanLimit <= 0 {
		opts.WalkScanLimit = DefaultWalkScanLimit
	}
	if opts.SweepScanLimit <= 0 {
		opts.SweepScanLimit = DefaultSweepScanLimit
	}
	if strings.TrimSpace(opts.WorkID) == "" {
		opts.WorkID = "work_promote_driver"
	}
	opts.passID = report.PassID

	if !opts.Apply {
		return runPlan(homePath, opts, now, report)
	}
	if len(opts.RegistryPaths) != 0 {
		return report, fmt.Errorf("registry import mutation is experimental and excluded from supported promote-driver apply; run a read-only plan or use the separately governed importer")
	}
	if opts.Recover {
		return runRecover(homePath, opts, now, report)
	}

	release, locked, err := acquireSingleFlight(homePath)
	if err != nil {
		return report, err
	}
	if locked {
		report.Locked = true
		report.FinishedAt = rfc3339(now)
		return report, nil
	}
	defer release()

	store, err := mem.Open(homePath)
	if err != nil {
		return report, fmt.Errorf("open memory store: %w", err)
	}
	journal, reconciled, err := loadFrozenJournal(homePath, now)
	if err != nil {
		return report, fmt.Errorf("load frozen pass journal: %w", err)
	}
	recordedResults := map[string]PassOperationResult{}
	if journal != nil {
		report = journal.Plan
		report.Stage = ""
		report.Error = ""
		report.FinishedAt = ""
		report.ReceiptPath = ""
		report.OperationResults = orderedOperationResults(*report.Intent, journal.Results)
		report.Finalization = journal.Finalization
		for key, result := range journal.Results {
			recordedResults[key] = result
		}
		report.ReconciledPassIDs = append(reconciled, report.PassID)
		opts.passID = report.PassID
		opts.WorkID = report.Intent.WorkID
	} else {
		report.ReconciledPassIDs = reconciled
		intent, err := buildPassIntent(store, opts, now)
		if err != nil {
			return report, fmt.Errorf("build pass intent: %w", err)
		}
		report.Intent = &intent
		opts.reservedCandidateIDs = map[string]bool{}
		for _, operation := range intent.Operations {
			if operation.Phase != "sweep" {
				opts.reservedCandidateIDs[operation.Target.ID] = true
			}
		}
		report.Stage = stagePlanned
		if err := receiptAppender(homePath, report); err != nil {
			return report, fmt.Errorf("journal planned pass: %w", err)
		}
		report.Stage = ""
	}

	runErr := runFrozenPass(homePath, store, recordedResults, now, &report)
	report.FinishedAt = rfc3339(now)
	report.Stage = stageCommitted
	if runErr != nil {
		var recovery passRecoveryError
		var partial partialPassError
		if errors.As(runErr, &recovery) {
			report.Stage = stageRecoveryRequired
		} else if errors.As(runErr, &partial) {
			report.Stage = stagePartial
		} else {
			report.Stage = stageFailed
		}
		report.Error = runErr.Error()
	}
	// Canonical terminal record: receipt_path identifies the receipt file the
	// appended line lives in, so we set it BEFORE the append. The heartbeat
	// helper reads the receipt line and the producer-rendered stdout, then
	// compares their pass, home, work and stage identity (the stored line's
	// SHA-256 is recorded as evidence). CLI presentation whitespace is
	// allowed to differ; what must match is the canonical record itself.
	receiptPath := filepath.Join(homePath, stateDirName, receiptFileName)
	report.ReceiptPath = receiptPath
	if err := receiptAppender(homePath, report); err != nil {
		report.Stage = stageRecoveryRequired
		report.Error = fmt.Sprintf("terminal receipt unavailable after stage %s: %v", terminalStageForError(runErr), err)
		markerErr := writeStateFile(homePath, "recovery-"+report.PassID+".json", report)
		if markerErr != nil {
			return report, fmt.Errorf("terminal receipt and recovery marker unavailable: receipt=%v marker=%v", err, markerErr)
		}
		return report, fmt.Errorf("terminal receipt unavailable; recovery marker persisted: %w", err)
	}
	if runErr != nil {
		return report, runErr
	}
	return report, nil
}

func buildPassIntent(store *mem.Store, opts Options, now time.Time) (PassIntent, error) {
	homePath := store.HomePath()
	promoteCursor, err := readPromoteCursor(homePath)
	if err != nil {
		return PassIntent{}, err
	}
	sweepCursor, err := readSweepCursor(homePath)
	if err != nil {
		return PassIntent{}, err
	}
	intent := PassIntent{
		WorkID:              opts.WorkID,
		ExecutionAt:         rfc3339(now),
		PromoteCursorBefore: promoteCursor.AfterCursor,
		SweepCursorBefore:   sweepCursor.AfterCursor,
		PromotePlan: PromotePhaseReport{
			Limit:       opts.PromoteLimit,
			ResumedFrom: promoteCursor.AfterCursor,
		},
		SweepPlan: SweepPhaseReport{ScanLimit: opts.SweepScanLimit},
	}
	if pending, pendingErr := store.ReadPendingState(); pendingErr == nil {
		intent.PendingGeneration = pending.Generation
	}

	seenCandidate := map[string]bool{}
	addCandidate := func(id string) {
		if id != "" && !seenCandidate[id] {
			seenCandidate[id] = true
			intent.CandidateIDs = append(intent.CandidateIDs, id)
		}
	}
	seenCorrupt := map[candidates.EntryCursor]bool{}
	addCorrupt := func(cursor candidates.EntryCursor) {
		if cursor != "" && !seenCorrupt[cursor] {
			seenCorrupt[cursor] = true
			intent.CorruptEntries = append(intent.CorruptEntries, cursor)
		}
	}
	corpus := map[candidates.EntryCursor]string{}
	type plannedCandidate struct {
		lite   mem.CandidateLite
		cursor candidates.EntryCursor
		digest string
	}
	precondition := func(candidate plannedCandidate) CandidatePrecondition {
		return CandidatePrecondition{
			ID:             candidate.lite.ID,
			EntryCursor:    candidate.cursor,
			State:          candidate.lite.State,
			BodyDigest:     candidate.digest,
			PromotionBasis: candidate.lite.PromotionBasis,
		}
	}
	candidateOperation := map[string]bool{}
	addOperation := func(phase, operation string, target CandidatePrecondition, keeper *CandidatePrecondition, currentClaim *ClaimPrecondition, key string) {
		addCandidate(target.ID)
		if candidateOperation[target.ID] {
			return
		}
		candidateOperation[target.ID] = true
		intent.Operations = append(intent.Operations, PassIntentOperation{
			Phase: phase, Operation: operation, Target: target, Keeper: keeper, CurrentClaim: currentClaim, DedupeKey: key,
		})
	}

	page, err := store.ListCandidatesLite(promoteCursor.AfterCursor, opts.WalkScanLimit)
	if err != nil {
		return PassIntent{}, err
	}
	intent.PromotePlan.Walked = page.Walked
	groups := map[string][]plannedCandidate{}
	safeCursor := promoteCursor.AfterCursor
	blocked := false
	for _, outcome := range page.Entries {
		if outcome.Corruption != nil {
			intent.PromotePlan.Corrupt++
			intent.PromotePlan.CorruptIDs = append(intent.PromotePlan.CorruptIDs, outcome.Cursor)
			addCorrupt(outcome.Cursor)
			corpus[outcome.Cursor] = "corrupt:" + string(outcome.Corruption.Code)
			if !blocked {
				safeCursor = outcome.Cursor
			}
			continue
		}
		entry := plannedCandidate{lite: *outcome.Candidate, cursor: outcome.Cursor, digest: outcome.Record.Digest}
		corpus[outcome.Cursor] = outcome.Record.Digest
		addCandidate(entry.lite.ID)
		intent.PromotePlan.Decoded++
		intent.PromotePlan.Scanned++
		if entry.lite.State != "proposed" {
			if !blocked {
				safeCursor = outcome.Cursor
			}
			continue
		}
		intent.PromotePlan.Considered++
		basis := strings.TrimSpace(entry.lite.PromotionBasis)
		if candidates.ValidBasis(basis) && basis != candidates.BasisExplicitConfirmation {
			if blocked || intent.PromotePlan.Promoted >= opts.PromoteLimit {
				intent.PromotePlan.Deferred = append(intent.PromotePlan.Deferred, entry.lite.ID)
				blocked = true
				continue
			}
			var fullCandidate mem.Candidate
			if err := json.Unmarshal(outcome.Record.Body, &fullCandidate); err != nil {
				return PassIntent{}, fmt.Errorf("decode promotion candidate %s: %w", entry.lite.ID, err)
			}
			claimPrecondition, err := store.FrozenPromotionClaimPrecondition(fullCandidate)
			if err != nil {
				return PassIntent{}, fmt.Errorf("freeze promotion claim precondition for %s: %w", entry.lite.ID, err)
			}
			addOperation("promote", candidates.Promote, precondition(entry), nil, &ClaimPrecondition{
				ID: claimPrecondition.ID, Absent: claimPrecondition.Absent,
			}, "")
			intent.PromotePlan.Promoted++
			intent.PromotePlan.Promotions = append(intent.PromotePlan.Promotions, PromotionReceipt{
				CandidateID: entry.lite.ID,
				Basis:       basis,
				Action:      "would_promote",
			})
			safeCursor = outcome.Cursor
			continue
		}
		if blocked {
			continue
		}
		groups[dedupeKey(entry.lite)] = append(groups[dedupeKey(entry.lite)], entry)
		safeCursor = outcome.Cursor
	}

	walkedAll := promoteCursor.AfterCursor == "" && page.AtEnd && !blocked
	if blocked || !page.AtEnd {
		intent.PromotePlan.NextCursor = safeCursor
	}
	if !walkedAll {
		switch {
		case blocked:
			intent.DedupePlan.Skipped = "promotion write cap or failure left an actionable candidate before the safe cursor frontier"
		case promoteCursor.AfterCursor != "":
			intent.DedupePlan.Skipped = "resume walk: duplicate groups may split across passes; collapse runs on a full walk from the start"
		default:
			intent.DedupePlan.Skipped = "walk budget exhausted before the inbox end; collapse runs on a full walk (raise --walk-scan-limit)"
		}
	} else {
		var keys []string
		for key, group := range groups {
			if len(group) > 1 {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			group := groups[key]
			sort.Slice(group, func(i, j int) bool {
				if group[i].lite.CreatedAt != group[j].lite.CreatedAt {
					return group[i].lite.CreatedAt < group[j].lite.CreatedAt
				}
				return group[i].lite.ID < group[j].lite.ID
			})
			keeper := precondition(group[0])
			action := DedupeAction{Key: key, Kept: keeper.ID, Action: "would_collapse"}
			for _, duplicate := range group[1:] {
				target := precondition(duplicate)
				addOperation("dedupe", candidates.NoPromote, target, &keeper, nil, key)
				action.NoPromoted = append(action.NoPromoted, target.ID)
			}
			intent.DedupePlan.Groups++
			intent.DedupePlan.Collapsed += len(action.NoPromoted)
			intent.DedupePlan.Actions = append(intent.DedupePlan.Actions, action)
		}
	}

	sweepPlan := store.AdjudicatePending(mem.AdjudicatePendingOptions{
		WorkID:      opts.WorkID,
		DryRun:      true,
		ScanLimit:   opts.SweepScanLimit,
		AfterCursor: sweepCursor.AfterCursor,
		ReservedIDs: candidateOperation,
	}, time.Time{})
	intent.SweepPlan.Walked = sweepPlan.Walked
	intent.SweepPlan.Decoded = sweepPlan.Decoded
	intent.SweepPlan.Corrupt = sweepPlan.Corrupt
	intent.SweepPlan.Scanned = sweepPlan.Scanned
	intent.SweepPlan.Skipped = sweepPlan.Skipped
	intent.SweepPlan.Deferred = sweepPlan.Deferred
	intent.SweepPlan.Failed = sweepPlan.Failed
	intent.SweepPlan.BasisDeferred = sweepPlan.BasisDeferred
	intent.SweepPlan.CorruptIDs = append([]candidates.EntryCursor(nil), sweepPlan.CorruptIDs...)
	intent.SweepPlan.NextCursor = sweepPlan.NextCursor
	intent.SweepPlan.WalkedAll = sweepPlan.AtEnd && !sweepPlan.LimitReached && sweepPlan.NextCursor == ""
	for _, cursor := range sweepPlan.CorruptIDs {
		addCorrupt(cursor)
		if _, ok := corpus[cursor]; !ok {
			corpus[cursor] = "corrupt"
		}
	}
	for _, pendingErr := range sweepPlan.Errors {
		identity := pendingErr.CandidateID
		if identity == "" {
			identity = cursorDisplay(pendingErr.EntryCursor)
		}
		intent.SweepPlan.Errors = append(intent.SweepPlan.Errors, fmt.Sprintf("%s: %s", identity, pendingErr.Err))
	}
	for _, action := range sweepPlan.Actions {
		target := CandidatePrecondition{
			ID:          action.CandidateID,
			EntryCursor: action.EntryCursor,
			State:       action.ExpectedState,
			BodyDigest:  action.ExpectedDigest,
		}
		corpus[target.EntryCursor] = target.BodyDigest
		addOperation("sweep", action.Verdict, target, nil, nil, "")
		intent.SweepPlan.Actions = append(intent.SweepPlan.Actions, SweepAction{CandidateID: action.CandidateID, Verdict: action.Verdict})
	}

	sort.Strings(intent.CandidateIDs)
	sort.Slice(intent.CorruptEntries, func(i, j int) bool { return intent.CorruptEntries[i] < intent.CorruptEntries[j] })
	var corpusLines []string
	for cursor, digest := range corpus {
		corpusLines = append(corpusLines, string(cursor)+"\x00"+digest)
	}
	sort.Strings(corpusLines)
	corpusSum := sha256.Sum256([]byte(strings.Join(corpusLines, "\x00")))
	intent.CandidateCorpusGeneration = "sha256:" + hex.EncodeToString(corpusSum[:])
	if err := sealPassIntent(opts.passID, &intent); err != nil {
		return PassIntent{}, err
	}
	return intent, nil
}

func sealPassIntent(passID string, intent *PassIntent) error {
	for index := range intent.Operations {
		intent.Operations[index].Ordinal = index
		intent.Operations[index].Key = ""
		body, err := json.Marshal(intent.Operations[index])
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(passID + "\x00" + string(body)))
		intent.Operations[index].Key = "op_" + hex.EncodeToString(sum[:16])
	}
	intent.PlanDigest = ""
	body, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	intent.PlanDigest = "sha256:" + hex.EncodeToString(sum[:])
	return nil
}

func validatePassIntent(passID string, intent PassIntent) error {
	if strings.TrimSpace(intent.WorkID) == "" {
		return fmt.Errorf("plan work id required")
	}
	if strings.TrimSpace(intent.CandidateCorpusGeneration) == "" {
		return fmt.Errorf("candidate corpus generation required")
	}
	if !validSHA256Digest(intent.CandidateCorpusGeneration) {
		return fmt.Errorf("candidate corpus generation %q is not a sha256 digest", intent.CandidateCorpusGeneration)
	}
	if intent.PendingGeneration != "" && !validSHA256Digest(intent.PendingGeneration) {
		return fmt.Errorf("pending generation %q is not a sha256 digest", intent.PendingGeneration)
	}
	if _, err := time.Parse(time.RFC3339, intent.ExecutionAt); err != nil {
		return fmt.Errorf("plan execution_at invalid: %w", err)
	}
	want := intent.PlanDigest
	if strings.TrimSpace(want) == "" {
		return fmt.Errorf("plan digest required")
	}
	copyOfIntent := intent
	copyOfIntent.Operations = append([]PassIntentOperation(nil), intent.Operations...)
	if err := sealPassIntent(passID, &copyOfIntent); err != nil {
		return err
	}
	if copyOfIntent.PlanDigest != want {
		return fmt.Errorf("plan digest mismatch: got %s want %s", copyOfIntent.PlanDigest, want)
	}
	if len(copyOfIntent.Operations) != len(intent.Operations) {
		return fmt.Errorf("plan operation count changed")
	}
	for index := range intent.Operations {
		operation := intent.Operations[index]
		if copyOfIntent.Operations[index].Key != operation.Key || operation.Ordinal != index {
			return fmt.Errorf("plan operation %d key/ordinal mismatch", index)
		}
		if strings.TrimSpace(operation.Target.ID) == "" || operation.Target.EntryCursor == "" || strings.TrimSpace(operation.Target.BodyDigest) == "" || strings.TrimSpace(operation.Target.State) == "" {
			return fmt.Errorf("plan operation %d target id, cursor, digest, and state required", index)
		}
		if !validSHA256Digest(operation.Target.BodyDigest) {
			return fmt.Errorf("plan operation %d target digest %q is invalid", index, operation.Target.BodyDigest)
		}
		switch operation.Phase {
		case "promote":
			if operation.Operation != candidates.Promote || operation.Keeper != nil {
				return fmt.Errorf("plan operation %d promotion shape invalid", index)
			}
			if operation.CurrentClaim == nil || operation.CurrentClaim.Absent == (strings.TrimSpace(operation.CurrentClaim.ID) != "") {
				return fmt.Errorf("plan operation %d promotion current-claim precondition invalid", index)
			}
		case "dedupe":
			if operation.Operation != candidates.NoPromote || operation.Keeper == nil ||
				strings.TrimSpace(operation.Keeper.ID) == "" ||
				operation.Keeper.EntryCursor == "" ||
				strings.TrimSpace(operation.Keeper.BodyDigest) == "" ||
				strings.TrimSpace(operation.Keeper.State) == "" {
				return fmt.Errorf("plan operation %d dedupe keeper id, cursor, digest, and state required", index)
			}
			if operation.CurrentClaim != nil {
				return fmt.Errorf("plan operation %d dedupe cannot carry a current-claim precondition", index)
			}
			if !validSHA256Digest(operation.Keeper.BodyDigest) {
				return fmt.Errorf("plan operation %d keeper digest %q is invalid", index, operation.Keeper.BodyDigest)
			}
		case "sweep":
			if operation.Operation != candidates.Park && operation.Operation != candidates.Reject && operation.Operation != candidates.NoPromote {
				return fmt.Errorf("plan operation %d sweep verdict %q is invalid", index, operation.Operation)
			}
			if operation.CurrentClaim != nil || operation.Keeper != nil {
				return fmt.Errorf("plan operation %d sweep cannot carry claim or keeper preconditions", index)
			}
		default:
			return fmt.Errorf("plan operation %d phase %q is invalid", index, operation.Phase)
		}
	}
	return nil
}

type frozenJournal struct {
	Plan         Report
	Results      map[string]PassOperationResult
	Finalization *PassFinalizationResult
}

type frozenJournalPass struct {
	plan         *Report
	results      map[string]PassOperationResult
	finalization *PassFinalizationResult
	terminal     bool
	replayable   bool
}

func loadFrozenJournal(homePath string, now time.Time) (*frozenJournal, []string, error) {
	path := filepath.Join(stateDir(homePath), receiptFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	passes := map[string]*frozenJournalPass{}
	for lineNo, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		receipt, err := decodeReceiptLine([]byte(line))
		if err != nil {
			return nil, nil, fmt.Errorf("receipt line %d is corrupt: %w", lineNo+1, err)
		}
		if strings.TrimSpace(receipt.PassID) == "" {
			return nil, nil, fmt.Errorf("receipt line %d has blank pass id", lineNo+1)
		}
		pass := passes[receipt.PassID]
		if pass == nil {
			pass = &frozenJournalPass{results: map[string]PassOperationResult{}}
			passes[receipt.PassID] = pass
		}
		if pass.terminal {
			return nil, nil, fmt.Errorf("pass %s receipt stage %q follows a terminal frame", receipt.PassID, receipt.Stage)
		}
		// Pre-frozen-journal eras appended one COMPLETE Report JSON per pass
		// with no stage field (schema v0.1-v0.3). Such a report is terminal
		// history: any mutations it describes went through the per-id
		// adjudicate doors at the time, never through this journal, so there
		// is nothing to replay. Fail closed only when the legacy report is
		// truncated (no finished_at) — that is genuine corruption, not history.
		if strings.TrimSpace(receipt.Stage) == "" && !replayableFrozenSchema(receipt.SchemaVersion) {
			if strings.TrimSpace(receipt.FinishedAt) == "" {
				return nil, nil, fmt.Errorf("receipt line %d: legacy %s report for pass %s has no finished_at (truncated)", lineNo+1, receipt.SchemaVersion, receipt.PassID)
			}
			pass.terminal = true
			continue
		}
		switch receipt.Stage {
		case stagePlanned:
			if pass.plan != nil {
				return nil, nil, fmt.Errorf("pass %s has duplicate planned receipts", receipt.PassID)
			}
			plan := receipt
			pass.plan = &plan
			if replayableFrozenSchema(receipt.SchemaVersion) && receipt.Intent != nil && strings.TrimSpace(receipt.Intent.PlanDigest) != "" {
				if err := validatePassIntent(receipt.PassID, *receipt.Intent); err != nil {
					return nil, nil, fmt.Errorf("pass %s planned intent invalid: %w", receipt.PassID, err)
				}
				if err := validatePlannedFrame(receipt); err != nil {
					return nil, nil, fmt.Errorf("pass %s planned frame invalid: %w", receipt.PassID, err)
				}
				pass.replayable = true
			}
		case stageOperationRecorded:
			if pass.plan == nil || !pass.replayable {
				return nil, nil, fmt.Errorf("pass %s operation result precedes a replayable plan", receipt.PassID)
			}
			if pass.finalization != nil {
				return nil, nil, fmt.Errorf("pass %s operation result follows finalization", receipt.PassID)
			}
			if receipt.SchemaVersion != pass.plan.SchemaVersion || receipt.Intent == nil {
				return nil, nil, fmt.Errorf("pass %s operation frame intent is missing or has schema %q", receipt.PassID, receipt.SchemaVersion)
			}
			if err := validateFrozenFrameProvenance(*pass.plan, receipt); err != nil {
				return nil, nil, fmt.Errorf("pass %s operation frame provenance invalid: %w", receipt.PassID, err)
			}
			if err := validatePassIntent(receipt.PassID, *receipt.Intent); err != nil {
				return nil, nil, fmt.Errorf("pass %s operation frame intent invalid: %w", receipt.PassID, err)
			}
			if !samePassIntent(*pass.plan.Intent, *receipt.Intent) {
				return nil, nil, fmt.Errorf("pass %s operation frame intent does not match the planned intent", receipt.PassID)
			}
			if len(receipt.OperationResults) != 1 {
				return nil, nil, fmt.Errorf("pass %s operation frame carries %d results, want 1", receipt.PassID, len(receipt.OperationResults))
			}
			result := receipt.OperationResults[0]
			if err := validateOperationResult(*pass.plan.Intent, result); err != nil {
				return nil, nil, fmt.Errorf("pass %s operation result invalid: %w", receipt.PassID, err)
			}
			if prior, ok := pass.results[result.Key]; ok {
				if !sameOperationResult(prior, result) {
					return nil, nil, fmt.Errorf("pass %s operation %s has conflicting results", receipt.PassID, result.Key)
				}
				continue
			}
			pass.results[result.Key] = result
		case stageFinalizationRecorded:
			if pass.plan == nil || !pass.replayable || pass.plan.Intent == nil {
				return nil, nil, fmt.Errorf("pass %s finalization precedes a replayable plan", receipt.PassID)
			}
			if pass.plan.SchemaVersion != ReportSchemaVersion || receipt.SchemaVersion != pass.plan.SchemaVersion {
				return nil, nil, fmt.Errorf("pass %s finalization is unsupported for schema %q", receipt.PassID, pass.plan.SchemaVersion)
			}
			if pass.finalization != nil {
				return nil, nil, fmt.Errorf("pass %s has duplicate finalization frames", receipt.PassID)
			}
			if receipt.Intent == nil || !samePassIntent(*pass.plan.Intent, *receipt.Intent) {
				return nil, nil, fmt.Errorf("pass %s finalization intent does not match the planned intent", receipt.PassID)
			}
			if err := validateFrozenFrameProvenance(*pass.plan, receipt); err != nil {
				return nil, nil, fmt.Errorf("pass %s finalization frame provenance invalid: %w", receipt.PassID, err)
			}
			if len(pass.results) != len(pass.plan.Intent.Operations) {
				return nil, nil, fmt.Errorf("pass %s finalization follows %d operation results, want %d", receipt.PassID, len(pass.results), len(pass.plan.Intent.Operations))
			}
			if receipt.Finalization == nil {
				return nil, nil, fmt.Errorf("pass %s finalization frame has no result", receipt.PassID)
			}
			if err := validateFinalizationResult(*pass.plan.Intent, *receipt.Finalization); err != nil {
				return nil, nil, fmt.Errorf("pass %s finalization result invalid: %w", receipt.PassID, err)
			}
			finalization := *receipt.Finalization
			if receipt.Finalization.Pending != nil {
				pending := *receipt.Finalization.Pending
				finalization.Pending = &pending
			}
			pass.finalization = &finalization
		case stageRecoveryRequired:
			if pass.plan == nil {
				return nil, nil, fmt.Errorf("pass %s recovery frame precedes a plan", receipt.PassID)
			}
			if !pass.replayable {
				pass.terminal = true
				continue
			}
			if receipt.SchemaVersion != pass.plan.SchemaVersion || receipt.Intent == nil || !samePassIntent(*pass.plan.Intent, *receipt.Intent) {
				return nil, nil, fmt.Errorf("pass %s recovery frame does not match the replayable plan", receipt.PassID)
			}
			if err := validateFrozenFrameProvenance(*pass.plan, receipt); err != nil {
				return nil, nil, fmt.Errorf("pass %s recovery frame provenance invalid: %w", receipt.PassID, err)
			}
		case stageAbandoned:
			if err := validateAbandonedReceipt(receipt.PassID, pass, receipt); err != nil {
				return nil, nil, fmt.Errorf("pass %s abandonment receipt invalid: %w", receipt.PassID, err)
			}
			pass.terminal = true
		case stageCommitted, stagePartial, stageFailed:
			if receipt.SchemaVersion == ReportSchemaVersion || pass.replayable {
				if err := validateTerminalReceipt(receipt.PassID, pass, receipt); err != nil {
					return nil, nil, fmt.Errorf("pass %s terminal receipt invalid: %w", receipt.PassID, err)
				}
			}
			pass.terminal = true
		default:
			return nil, nil, fmt.Errorf("pass %s has unknown receipt stage %q", receipt.PassID, receipt.Stage)
		}
	}

	var active []*frozenJournalPass
	var legacy []string
	for passID, pass := range passes {
		if pass.plan == nil || pass.terminal {
			continue
		}
		if pass.replayable {
			active = append(active, pass)
		} else {
			legacy = append(legacy, passID)
		}
	}
	sort.Strings(legacy)
	for _, passID := range legacy {
		recovery := *passes[passID].plan
		recovery.SchemaVersion = ReportSchemaVersion
		recovery.ReceiptPath = filepath.Join(homePath, stateDirName, receiptFileName)
		recovery.Stage = stageRecoveryRequired
		recovery.FinishedAt = rfc3339(now)
		recovery.Error = "legacy_unreplayable: planned receipt lacks a validated v0.4 frozen plan; no mutation was replayed"
		if err := receiptAppender(homePath, recovery); err != nil {
			return nil, nil, fmt.Errorf("terminalize legacy planned-only pass %s: %w", passID, err)
		}
	}
	if len(active) > 1 {
		var ids []string
		for _, pass := range active {
			ids = append(ids, pass.plan.PassID)
		}
		sort.Strings(ids)
		return nil, nil, fmt.Errorf("multiple replayable passes require recovery: %s", strings.Join(ids, ", "))
	}
	if len(active) == 0 {
		return nil, legacy, nil
	}
	return &frozenJournal{Plan: *active[0].plan, Results: active[0].results, Finalization: active[0].finalization}, legacy, nil
}

func replayableFrozenSchema(schema string) bool {
	return schema == ReportSchemaVersion || schema == frozenReportSchemaVersion
}

func validateFinalizationResult(intent PassIntent, result PassFinalizationResult) error {
	if result.PromoteCursorAfter != intent.PromotePlan.NextCursor {
		return fmt.Errorf("promote cursor %q does not match frozen result %q", result.PromoteCursorAfter, intent.PromotePlan.NextCursor)
	}
	if result.SweepCursorAfter != intent.SweepPlan.NextCursor {
		return fmt.Errorf("sweep cursor %q does not match frozen result %q", result.SweepCursorAfter, intent.SweepPlan.NextCursor)
	}
	if intent.SweepPlan.WalkedAll != (result.Pending != nil) {
		return fmt.Errorf("pending result presence %v does not match walked-all %v", result.Pending != nil, intent.SweepPlan.WalkedAll)
	}
	if result.Pending == nil {
		return nil
	}
	pending := result.Pending
	if pending.Pending < 0 || pending.Files < 0 || pending.Corrupt < 0 || pending.Pending > pending.Files || pending.Corrupt > pending.Files || pending.Pending+pending.Corrupt > pending.Files {
		return fmt.Errorf("pending census is invalid: pending=%d files=%d corrupt=%d", pending.Pending, pending.Files, pending.Corrupt)
	}
	if !validSHA256Digest(pending.Generation) {
		return fmt.Errorf("pending generation %q is not a sha256 digest", pending.Generation)
	}
	return nil
}

func validatePlannedFrame(plan Report) error {
	if plan.DryRun {
		return fmt.Errorf("frozen apply plan cannot be dry-run")
	}
	if strings.TrimSpace(plan.Home) == "" {
		return fmt.Errorf("home required")
	}
	started, err := time.Parse(time.RFC3339, plan.StartedAt)
	if err != nil {
		return fmt.Errorf("started_at invalid: %w", err)
	}
	if plan.Intent == nil || plan.Intent.ExecutionAt != rfc3339(started) {
		return fmt.Errorf("started_at %q does not match frozen execution_at", plan.StartedAt)
	}
	return nil
}

func validateFrozenFrameProvenance(plan, frame Report) error {
	if frame.DryRun != plan.DryRun {
		return fmt.Errorf("dry_run=%v does not match planned %v", frame.DryRun, plan.DryRun)
	}
	if frame.Home != plan.Home {
		return fmt.Errorf("home %q does not match planned %q", frame.Home, plan.Home)
	}
	if frame.StartedAt != plan.StartedAt {
		return fmt.Errorf("started_at %q does not match planned %q", frame.StartedAt, plan.StartedAt)
	}
	started, err := time.Parse(time.RFC3339, plan.StartedAt)
	if err != nil {
		return fmt.Errorf("planned started_at invalid: %w", err)
	}
	finished, err := time.Parse(time.RFC3339, frame.FinishedAt)
	if err != nil {
		return fmt.Errorf("finished_at invalid: %w", err)
	}
	if finished.Before(started) {
		return fmt.Errorf("finished_at %q precedes started_at %q", frame.FinishedAt, frame.StartedAt)
	}
	return nil
}

// Abandonment closes replay without claiming that every operation completed.
// runRecover copies the planned summaries and carries all durably recorded
// operation and finalization results; accepting that frame must preserve them.
func validateAbandonedReceipt(passID string, pass *frozenJournalPass, abandoned Report) error {
	if pass.plan == nil || !pass.replayable || pass.plan.Intent == nil {
		return fmt.Errorf("abandonment requires replayable planned intent")
	}
	if abandoned.SchemaVersion != pass.plan.SchemaVersion {
		return fmt.Errorf("abandonment schema %q does not match planned schema %q", abandoned.SchemaVersion, pass.plan.SchemaVersion)
	}
	if err := validateFrozenFrameProvenance(*pass.plan, abandoned); err != nil {
		return fmt.Errorf("abandonment provenance invalid: %w", err)
	}
	if abandoned.Intent == nil {
		return fmt.Errorf("abandonment has no frozen intent")
	}
	if err := validatePassIntent(passID, *abandoned.Intent); err != nil {
		return fmt.Errorf("abandonment intent invalid: %w", err)
	}
	if !samePassIntent(*pass.plan.Intent, *abandoned.Intent) {
		return fmt.Errorf("abandonment intent does not match planned intent")
	}
	if abandoned.Locked || strings.TrimSpace(abandoned.Error) == "" {
		return fmt.Errorf("abandonment requires an observed drift reason and an acquired lock")
	}
	if pass.finalization == nil {
		if abandoned.Finalization != nil {
			return fmt.Errorf("abandonment carries finalization without prior frame")
		}
	} else if abandoned.Finalization == nil || !sameFinalizationResult(*pass.finalization, *abandoned.Finalization) {
		return fmt.Errorf("abandonment finalization does not match its recorded frame")
	}
	expected := orderedOperationResults(*pass.plan.Intent, pass.results)
	if len(abandoned.OperationResults) != len(expected) {
		return fmt.Errorf("abandonment carries %d operation results, want %d", len(abandoned.OperationResults), len(expected))
	}
	for index := range expected {
		if err := validateOperationResult(*pass.plan.Intent, abandoned.OperationResults[index]); err != nil {
			return fmt.Errorf("abandonment operation result %d invalid: %w", index, err)
		}
		if !sameOperationResult(expected[index], abandoned.OperationResults[index]) {
			return fmt.Errorf("abandonment operation result %d does not match its recorded frame", index)
		}
	}
	for _, summary := range []struct {
		name        string
		got, wanted any
	}{
		{"promote", abandoned.Promote, pass.plan.Promote},
		{"dedupe", abandoned.Dedupe, pass.plan.Dedupe},
		{"sweep", abandoned.Sweep, pass.plan.Sweep},
		{"imports", abandoned.Imports, pass.plan.Imports},
		{"lane_beliefs", abandoned.LaneBeliefs, pass.plan.LaneBeliefs},
		{"pending_after", abandoned.PendingAfter, pass.plan.PendingAfter},
	} {
		if !sameJSONValue(summary.got, summary.wanted) {
			return fmt.Errorf("abandonment %s summary does not match planned summary", summary.name)
		}
	}
	return nil
}

func validateTerminalReceipt(passID string, pass *frozenJournalPass, terminal Report) error {
	if pass.plan == nil || !pass.replayable || pass.plan.Intent == nil {
		return fmt.Errorf("current terminal frame requires a replayable planned intent")
	}
	if terminal.SchemaVersion != pass.plan.SchemaVersion {
		return fmt.Errorf("terminal schema %q does not match planned schema %q", terminal.SchemaVersion, pass.plan.SchemaVersion)
	}
	if terminal.Intent == nil {
		return fmt.Errorf("terminal frame has no frozen intent")
	}
	if err := validateFrozenFrameProvenance(*pass.plan, terminal); err != nil {
		return fmt.Errorf("terminal provenance invalid: %w", err)
	}
	if err := validatePassIntent(passID, *terminal.Intent); err != nil {
		return fmt.Errorf("terminal intent invalid: %w", err)
	}
	if !samePassIntent(*pass.plan.Intent, *terminal.Intent) {
		return fmt.Errorf("terminal intent does not match the planned intent")
	}
	if len(pass.results) != len(pass.plan.Intent.Operations) {
		return fmt.Errorf("terminal follows %d recorded operation results, want %d", len(pass.results), len(pass.plan.Intent.Operations))
	}
	expected := orderedOperationResults(*pass.plan.Intent, pass.results)
	if len(terminal.OperationResults) != len(expected) {
		return fmt.Errorf("terminal carries %d operation results, want %d", len(terminal.OperationResults), len(expected))
	}
	for index := range expected {
		if err := validateOperationResult(*pass.plan.Intent, terminal.OperationResults[index]); err != nil {
			return fmt.Errorf("terminal operation result %d invalid: %w", index, err)
		}
		if !sameOperationResult(expected[index], terminal.OperationResults[index]) {
			return fmt.Errorf("terminal operation result %d does not match its recorded frame", index)
		}
	}
	if terminal.SchemaVersion == ReportSchemaVersion {
		finalizationRequired := terminal.Stage == stageCommitted || terminal.Stage == stagePartial
		if finalizationRequired && pass.finalization == nil {
			return fmt.Errorf("terminal stage %q has no prior finalization frame", terminal.Stage)
		}
		if pass.finalization == nil {
			if terminal.Finalization != nil {
				return fmt.Errorf("terminal carries finalization without a prior frame")
			}
		} else {
			if terminal.Finalization == nil || !sameFinalizationResult(*pass.finalization, *terminal.Finalization) {
				return fmt.Errorf("terminal finalization does not match its recorded frame")
			}
		}
	} else if terminal.Finalization != nil || pass.finalization != nil {
		return fmt.Errorf("terminal schema %q cannot carry finalization", terminal.SchemaVersion)
	}

	projectionIntent, err := clonePassIntent(*pass.plan.Intent)
	if err != nil {
		return fmt.Errorf("clone planned intent for terminal projection: %w", err)
	}
	derived := Report{}
	summarizeFrozenResults(projectionIntent, expected, &derived)
	if pass.finalization != nil {
		applyFinalizationResult(*pass.finalization, &derived)
	}
	terminalSweep := terminal.Sweep
	derivedSweep := derived.Sweep
	if terminal.SchemaVersion == frozenReportSchemaVersion {
		if pass.plan.Intent.SweepPlan.WalkedAll {
			if terminalSweep.ReconciledPending == nil || terminal.PendingAfter == nil || *terminalSweep.ReconciledPending != *terminal.PendingAfter || *terminal.PendingAfter < 0 {
				return fmt.Errorf("historical terminal pending summary is inconsistent")
			}
		} else if terminalSweep.ReconciledPending != nil || terminal.PendingAfter != nil {
			return fmt.Errorf("historical terminal carries pending summary without a full sweep")
		}
		terminalSweep.ReconciledPending = nil
	}
	summaries := []struct {
		name        string
		got, wanted any
	}{
		{name: "promote", got: terminal.Promote, wanted: derived.Promote},
		{name: "dedupe", got: terminal.Dedupe, wanted: derived.Dedupe},
		{name: "sweep", got: terminalSweep, wanted: derivedSweep},
		{name: "imports", got: terminal.Imports, wanted: derived.Imports},
		{name: "lane_beliefs", got: terminal.LaneBeliefs, wanted: derived.LaneBeliefs},
	}
	if terminal.SchemaVersion == ReportSchemaVersion {
		summaries = append(summaries, struct {
			name        string
			got, wanted any
		}{name: "pending_after", got: terminal.PendingAfter, wanted: derived.PendingAfter})
	}
	for _, summary := range summaries {
		if !sameJSONValue(summary.got, summary.wanted) {
			return fmt.Errorf("terminal %s summary does not match frozen results", summary.name)
		}
	}
	wantStage, wantError := frozenTerminalOutcome(derived, expected)
	if terminal.Stage != wantStage || terminal.Error != wantError {
		return fmt.Errorf("terminal outcome stage=%q error=%q, want stage=%q error=%q", terminal.Stage, terminal.Error, wantStage, wantError)
	}
	return nil
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == sha256.Size
}

func sameFinalizationResult(left, right PassFinalizationResult) bool {
	return sameJSONValue(left, right)
}

func sameJSONValue(left, right any) bool {
	leftBody, leftErr := json.Marshal(left)
	rightBody, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftBody) == string(rightBody)
}

func frozenTerminalOutcome(report Report, results []PassOperationResult) (string, string) {
	for _, result := range results {
		if result.Outcome == mem.FrozenAdjudicationStalePrecondition {
			return stageFailed, fmt.Sprintf("stale_precondition: operation %s candidate %s drifted at %s", result.Key, result.CandidateID, operationMismatch(result))
		}
	}
	if failures := partialFailures(report); len(failures) > 0 {
		return stagePartial, partialPassError{failures: failures}.Error()
	}
	return stageCommitted, ""
}

func samePassIntent(left, right PassIntent) bool {
	leftBody, _ := json.Marshal(left)
	rightBody, _ := json.Marshal(right)
	return string(leftBody) == string(rightBody)
}

func clonePassIntent(intent PassIntent) (PassIntent, error) {
	body, err := json.Marshal(intent)
	if err != nil {
		return PassIntent{}, err
	}
	var cloned PassIntent
	if err := json.Unmarshal(body, &cloned); err != nil {
		return PassIntent{}, err
	}
	return cloned, nil
}

func validateOperationResult(intent PassIntent, result PassOperationResult) error {
	if result.Ordinal < 0 || result.Ordinal >= len(intent.Operations) {
		return fmt.Errorf("operation ordinal %d outside plan", result.Ordinal)
	}
	operation := intent.Operations[result.Ordinal]
	if result.Key != operation.Key || result.Phase != operation.Phase || result.Operation != operation.Operation || result.CandidateID != operation.Target.ID {
		return fmt.Errorf("operation %d identity does not match frozen plan", result.Ordinal)
	}
	if result.BeforeDigest != operation.Target.BodyDigest {
		return fmt.Errorf("operation %s before digest %q does not match %q", result.Key, result.BeforeDigest, operation.Target.BodyDigest)
	}
	if result.Error != "" {
		return fmt.Errorf("operation %s result carries an unexpected error", result.Key)
	}
	switch result.Outcome {
	case mem.FrozenAdjudicationApplied, mem.FrozenAdjudicationAlreadyApplied:
		if !validSHA256Digest(result.AfterDigest) {
			return fmt.Errorf("operation %s after digest %q is invalid", result.Key, result.AfterDigest)
		}
		if result.MismatchCandidateID != "" || result.PreconditionMismatch != "" {
			return fmt.Errorf("operation %s successful outcome carries a precondition mismatch", result.Key)
		}
		if err := validateSuccessfulOperationResult(operation, result); err != nil {
			return err
		}
	case mem.FrozenAdjudicationStalePrecondition:
		hasCandidateMismatch := strings.TrimSpace(result.MismatchCandidateID) != ""
		hasClaimMismatch := strings.TrimSpace(result.PreconditionMismatch) != ""
		if hasCandidateMismatch == hasClaimMismatch {
			return fmt.Errorf("operation %s stale outcome lacks precondition mismatch", result.Key)
		}
		if hasCandidateMismatch && result.MismatchCandidateID != operation.Target.ID && (operation.Keeper == nil || result.MismatchCandidateID != operation.Keeper.ID) {
			return fmt.Errorf("operation %s stale mismatch candidate %q is outside the frozen plan", result.Key, result.MismatchCandidateID)
		}
		if result.AfterDigest != "" && !validSHA256Digest(result.AfterDigest) {
			return fmt.Errorf("operation %s stale after digest %q is invalid", result.Key, result.AfterDigest)
		}
		if result.Verdict != operation.Operation || result.ClaimID != "" || result.TransitionID != "" || result.SupersededClaimID != "" || result.Note != "" {
			return fmt.Errorf("operation %s stale outcome carries successful mutation fields", result.Key)
		}
	default:
		return fmt.Errorf("operation %s has unknown outcome %q", result.Key, result.Outcome)
	}
	return nil
}

func validateSuccessfulOperationResult(operation PassIntentOperation, result PassOperationResult) error {
	if operation.Phase != "promote" {
		wantState := map[string]string{
			candidates.Park:      "parked",
			candidates.Reject:    "rejected",
			candidates.NoPromote: "no_promote",
		}[operation.Operation]
		if wantState == "" || result.Verdict != operation.Operation || result.CandidateState != wantState {
			return fmt.Errorf("operation %s result verdict/state %q/%q does not match frozen %q/%q", result.Key, result.Verdict, result.CandidateState, operation.Operation, wantState)
		}
		if result.ClaimID != "" || result.TransitionID != "" || result.SupersededClaimID != "" || result.Note != "" {
			return fmt.Errorf("operation %s non-promotion result carries promotion fields", result.Key)
		}
		return nil
	}

	if operation.CurrentClaim == nil {
		return fmt.Errorf("operation %s promotion lacks current-claim precondition", result.Key)
	}
	switch result.CandidateState {
	case "promoted":
		if result.Verdict != candidates.Promote || !strings.HasPrefix(result.ClaimID, "claim_") || !strings.HasPrefix(result.TransitionID, "tr_") || result.Note != "" {
			return fmt.Errorf("operation %s promoted result lacks exact claim/transition linkage", result.Key)
		}
		if operation.CurrentClaim.Absent {
			if result.SupersededClaimID != "" {
				return fmt.Errorf("operation %s promoted absent-claim result names superseded claim %q", result.Key, result.SupersededClaimID)
			}
		} else if result.SupersededClaimID != operation.CurrentClaim.ID {
			return fmt.Errorf("operation %s superseded claim %q does not match frozen current claim %q", result.Key, result.SupersededClaimID, operation.CurrentClaim.ID)
		}
		if result.ClaimID == operation.CurrentClaim.ID {
			return fmt.Errorf("operation %s reused the superseded claim id", result.Key)
		}
	case "rejected":
		if operation.CurrentClaim.Absent || result.Verdict != candidates.Reject || result.ClaimID != "" || result.TransitionID != "" || result.SupersededClaimID != "" || result.Note != "already current: "+operation.CurrentClaim.ID {
			return fmt.Errorf("operation %s duplicate-rejection result does not match frozen current claim", result.Key)
		}
	default:
		return fmt.Errorf("operation %s promotion result has state %q", result.Key, result.CandidateState)
	}
	return nil
}

func operationMismatch(result PassOperationResult) string {
	if strings.TrimSpace(result.PreconditionMismatch) != "" {
		return result.PreconditionMismatch
	}
	return result.MismatchCandidateID
}

func sameOperationResult(left, right PassOperationResult) bool {
	leftBody, _ := json.Marshal(left)
	rightBody, _ := json.Marshal(right)
	return string(leftBody) == string(rightBody)
}

func orderedOperationResults(intent PassIntent, results map[string]PassOperationResult) []PassOperationResult {
	ordered := make([]PassOperationResult, 0, len(intent.Operations))
	for _, operation := range intent.Operations {
		if result, ok := results[operation.Key]; ok {
			ordered = append(ordered, result)
		}
	}
	return ordered
}

func decodeReceiptLine(line []byte) (Report, error) {
	var receipt Report
	primaryErr := json.Unmarshal(line, &receipt)
	if primaryErr == nil {
		return receipt, nil
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return Report{}, primaryErr
	}
	if isLegacyCursorMigrationSchema(header.SchemaVersion) {
		migrated, changed, migrationErr := migrateLegacyReceiptCursors(line)
		if migrationErr == nil && changed {
			if err := json.Unmarshal(migrated, &receipt); err == nil {
				return receipt, nil
			}
		}
	}
	// A stageless legacy report (pre-frozen eras appended one complete Report
	// per pass, no stage key) decodes minimally: identity + finished_at only.
	// The journal classifies it as terminal history (loadFrozenJournal) — the
	// nested cursors it may carry are historical facts, never replay inputs.
	// Any receipt that DECLARES a stage keeps failing closed; staged frames
	// must decode fully or the journal is corrupt.
	var probe struct {
		SchemaVersion string  `json:"schema_version"`
		PassID        string  `json:"pass_id"`
		Stage         *string `json:"stage"`
		FinishedAt    string  `json:"finished_at"`
	}
	if err := json.Unmarshal(line, &probe); err == nil && probe.Stage == nil &&
		strings.TrimSpace(probe.PassID) != "" && isLegacyReceiptSchema(probe.SchemaVersion) {
		return Report{SchemaVersion: probe.SchemaVersion, PassID: probe.PassID, FinishedAt: probe.FinishedAt}, nil
	}
	return Report{}, primaryErr
}

// isLegacyCursorMigrationSchema reports whether s predates the unversioned
// raw-cursor prohibition (v0.3). v0.1/v0.2 receipts legitimately carry raw
// pre-token cursors and are migrated; a v0.3+ receipt carrying them is
// corruption and must fail closed (falsified by
// TestCurrentReceiptSchemaRejectsRawCursorJSON).
func isLegacyCursorMigrationSchema(s string) bool {
	return s == legacyReportSchemaVersion || s == archivedReportSchemaVersion
}

// isLegacyReceiptSchema reports whether s predates the frozen journal eras
// (v0.4+). Legacy receipts never replay; the journal treats stageless ones as
// terminal history and refuses staged ones that fail full decode.
func isLegacyReceiptSchema(s string) bool {
	switch s {
	case legacyReportSchemaVersion, previousReportSchemaVersion, archivedReportSchemaVersion:
		return true
	}
	return false
}

func migrateLegacyReceiptCursors(line []byte) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(line, &root); err != nil {
		return nil, false, err
	}
	type cursorField struct {
		section string
		name    string
		list    bool
	}
	fields := []cursorField{
		{section: "intent", name: "promote_cursor_before"},
		{section: "intent", name: "sweep_cursor_before"},
		{section: "intent", name: "corrupt_entries", list: true},
		{section: "promote", name: "resumed_from"},
		{section: "promote", name: "next_cursor"},
		{section: "promote", name: "corrupt_ids", list: true},
		{section: "sweep", name: "next_cursor"},
		{section: "sweep", name: "corrupt_ids", list: true},
	}

	changed := false
	for _, field := range fields {
		sectionRaw, ok := root[field.section]
		if !ok {
			continue
		}
		var section map[string]json.RawMessage
		if err := json.Unmarshal(sectionRaw, &section); err != nil {
			return nil, false, fmt.Errorf("decode legacy receipt %s section: %w", field.section, err)
		}
		value, ok := section[field.name]
		if !ok {
			continue
		}
		var (
			normalized   json.RawMessage
			fieldChanged bool
			err          error
		)
		if field.list {
			normalized, fieldChanged, err = normalizeLegacyReceiptCursorList(value)
		} else {
			normalized, fieldChanged, err = normalizeLegacyReceiptCursor(value)
		}
		if err != nil {
			return nil, false, fmt.Errorf("decode legacy receipt %s.%s: %w", field.section, field.name, err)
		}
		if !fieldChanged {
			continue
		}
		section[field.name] = normalized
		encodedSection, err := json.Marshal(section)
		if err != nil {
			return nil, false, err
		}
		root[field.section] = encodedSection
		changed = true
	}
	if !changed {
		return line, false, nil
	}
	encoded, err := json.Marshal(root)
	return encoded, true, err
}

func normalizeLegacyReceiptCursor(raw json.RawMessage) (json.RawMessage, bool, error) {
	var current candidates.EntryCursor
	if err := json.Unmarshal(raw, &current); err == nil {
		return raw, false, nil
	}
	var legacyID string
	if err := json.Unmarshal(raw, &legacyID); err != nil {
		return nil, false, err
	}
	cursor, err := candidates.EntryCursorFromFilename(legacyID + ".json")
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(cursor)
	return encoded, true, err
}

func normalizeLegacyReceiptCursorList(raw json.RawMessage) (json.RawMessage, bool, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, err
	}
	changed := false
	for index, value := range values {
		normalized, itemChanged, err := normalizeLegacyReceiptCursor(value)
		if err != nil {
			return nil, false, fmt.Errorf("item %d: %w", index, err)
		}
		if itemChanged {
			values[index] = normalized
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(values)
	return encoded, true, err
}

// runPass executes every independent bounded phase. Phase/integrity failures
// fail immediately; candidate-local failures are collected and returned as a
// typed partial outcome only after the remaining phases have had a chance to
// make useful progress.
func runPass(homePath string, store *mem.Store, opts Options, now time.Time, report *Report) error {
	promote, dedupe, err := runBasisAndDedupe(store, opts, now)
	if err != nil {
		return fmt.Errorf("basis/dedupe phase: %w", err)
	}
	report.Promote, report.Dedupe = promote, dedupe
	imports, err := runImports(homePath, store, opts, now)
	report.Imports = imports
	if err != nil {
		return fmt.Errorf("imports phase: %w", err)
	}
	report.LaneBeliefs = runLaneBeliefs(homePath, opts, now)
	if report.LaneBeliefs.Error != "" {
		return fmt.Errorf("lane-beliefs phase: %s", report.LaneBeliefs.Error)
	}
	sweep, err := runSweep(store, opts, now)
	if err != nil {
		report.Sweep = sweep
		return fmt.Errorf("sweep phase: %w", err)
	}
	report.Sweep = sweep
	report.PendingAfter = sweep.ReconciledPending
	if failures := partialFailures(*report); len(failures) > 0 {
		return partialPassError{failures: failures}
	}
	return nil
}

type passRecoveryError struct{ err error }

func (e passRecoveryError) Error() string { return "recovery_required: " + e.err.Error() }
func (e passRecoveryError) Unwrap() error { return e.err }

// runRecover handles the --recover door: it loads any active frozen pass
// and either completes it (every recorded target still matches the live
// inbox) or abandons it (a target drifted, the next pass plans fresh).
// The door is fail-closed: a target mid-replacement (a candidate publication
// staging file present, or the live file's digest/state changed without
// the planner's signal) refuses to abandon and surfaces an explicit error
// the operator can investigate.
//
// The implementation shares `runFrozenPass` for the success branch (so
// the recovered pass produces the same per-operation receipts as an
// uninterrupted replay would). For the abandonment branch it writes a
// single `abandoned` terminal frame under the same pass id and returns
// without applying any of the recorded operations.
func runRecover(homePath string, opts Options, now time.Time, report Report) (Report, error) {
	release, locked, err := acquireSingleFlight(homePath)
	if err != nil {
		return report, err
	}
	if locked {
		report.Locked = true
		report.Stage = stageAbandoned
		report.FinishedAt = rfc3339(now)
		return report, nil
	}
	defer release()

	store, err := mem.Open(homePath)
	if err != nil {
		return report, fmt.Errorf("open memory store: %w", err)
	}
	journal, reconciled, err := loadFrozenJournal(homePath, now)
	if err != nil {
		return report, fmt.Errorf("load frozen pass journal: %w", err)
	}
	if journal == nil {
		report.Stage = stageCommitted
		report.FinishedAt = rfc3339(now)
		report.ReconciledPassIDs = reconciled
		report.Error = "no active frozen pass: --recover is a no-op when there is no replayable pass"
		return report, nil
	}
	report = journal.Plan
	report.Stage = ""
	report.Error = ""
	report.FinishedAt = ""
	report.ReconciledPassIDs = append(reconciled, report.PassID)
	recordedResults := journal.Results
	report.OperationResults = orderedOperationResults(*report.Intent, recordedResults)
	report.Finalization = journal.Finalization
	opts.passID = report.PassID
	opts.WorkID = report.Intent.WorkID

	// Fail-closed gate: every target's live state must still match what the
	// frozen plan recorded. Any drift means the planner's view diverged
	// from reality (sometimes legitimately, e.g. an operator adjudicated
	// the candidate by hand) — abandonment is the safer default than a
	// partial replay that would either revive an already-decided
	// candidate or apply a verdict the operator never approved.
	intent, err := clonePassIntent(*report.Intent)
	if err != nil {
		return report, fmt.Errorf("clone frozen pass intent: %w", err)
	}
	drift, driftErr := detectFrozenTargetDrift(homePath, store, intent)
	if driftErr != nil {
		return report, fmt.Errorf("detect frozen target drift: %w", driftErr)
	}
	if drift != nil {
		abortReport := report
		abortReport.Stage = stageAbandoned
		abortReport.FinishedAt = rfc3339(now)
		abortReport.Error = drift.Reason
		if err := receiptAppender(homePath, abortReport); err != nil {
			return abortReport, fmt.Errorf("journal abandoned frame: %w", err)
		}
		abortReport.ReceiptPath = filepath.Join(homePath, stateDirName, receiptFileName)
		return abortReport, nil
	}

	// Live targets match the frozen plan exactly. Replay through the same
	// code path an uninterrupted pass would have used.
	replayErr := runFrozenPass(homePath, store, recordedResults, now, &report)
	report.FinishedAt = rfc3339(now)
	report.Stage = stageCommitted
	if replayErr != nil {
		var partial partialPassError
		if errors.As(replayErr, &partial) {
			report.Stage = stagePartial
		} else {
			// runFrozenPass returns passRecoveryError only when the inner
			// MutateCandidateIfDigests rejects an operation. Post-fix the
			// content-based compare eliminates that path; if it ever
			// fires the recovery door cannot rescue us, so we surface the
			// error and let the operator investigate.
			report.Stage = stageRecoveryRequired
		}
		report.Error = replayErr.Error()
	}
	if err := receiptAppender(homePath, report); err != nil {
		report.Stage = stageRecoveryRequired
		report.Error = fmt.Sprintf("terminal receipt unavailable after recovery stage %s: %v", terminalStageForError(replayErr), err)
		markerErr := writeStateFile(homePath, "recovery-"+report.PassID+".json", report)
		if markerErr != nil {
			return report, fmt.Errorf("terminal receipt and recovery marker unavailable: receipt=%v marker=%v", err, markerErr)
		}
		return report, fmt.Errorf("terminal receipt unavailable; recovery marker persisted: %w", err)
	}
	report.ReceiptPath = filepath.Join(homePath, stateDirName, receiptFileName)
	if replayErr != nil {
		return report, replayErr
	}
	return report, nil
}

// frozenDriftReason describes a single target that no longer matches the
// frozen plan. The string is human-readable so the abandoned receipt's
// `Error` field names the offending operation and what was observed.
type frozenDriftReason struct {
	Reason string
}

// detectFrozenTargetDrift re-validates every Operation in intent against
// the live inbox and mem store, returning the first drift it observes.
// Empty return (`nil, nil`) means the frozen plan can be replayed
// verbatim; a non-nil Reason names the offending operation and the
// observed state. The check mirrors the per-operation preconditions
// AdjudicateFrozen would face on each apply: target BodyDigest+State and,
// for promotions, the current-claim signature.
//
// Mid-replacement detection: if any target has a candidate publication
// staging file present (`<id>.json.<tmp>` in the inbox dir), the inbox
// reader treats that as an in-flight publish and reports drift rather
// than letting --recover silently usurp it.
func detectFrozenTargetDrift(homePath string, store *mem.Store, intent PassIntent) (*frozenDriftReason, error) {
	for _, op := range intent.Operations {
		path := filepath.Join(homePath, candidates.InboxDir(candidates.Memory), op.Target.ID+".json")
		if entries, err := os.ReadDir(filepath.Dir(path)); err == nil {
			for _, entry := range entries {
				name := entry.Name()
				// The candidate publication writer stages replacement bytes
				// in a sibling file. Two shapes can be present at recovery
				// time:
				//
				//  • `<id>.json.<suffix>` — older / ad-hoc writers that
				//    swap the data out under a derived name. Worth flagging
				//    because the locked takeover would clobber it.
				//  • `.candidate-publish-<hex>` — the canonical temp name
				//    used by internal/candidates createTempFile. The
				//    presence of any file in the inbox directory that is
				//    not the recorded `<id>.json` indicates another publish
				//    is in flight.
				if strings.HasPrefix(name, op.Target.ID+".json.") || strings.HasPrefix(name, ".candidate-publish-") {
					return &frozenDriftReason{Reason: fmt.Sprintf("operation %s drift: candidate %s has staging file %s in flight; refusing to abandon mid-replacement", op.Key, op.Target.ID, name)}, nil
				}
			}
		}
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return &frozenDriftReason{Reason: fmt.Sprintf("operation %s drift: candidate %s no longer exists on disk", op.Key, op.Target.ID)}, nil
			}
			return nil, fmt.Errorf("read candidate %s: %w", op.Target.ID, err)
		}
		sum := sha256.Sum256(body)
		liveDigest := "sha256:" + hex.EncodeToString(sum[:])
		if liveDigest != op.Target.BodyDigest {
			return &frozenDriftReason{Reason: fmt.Sprintf("operation %s drift: candidate %s body changed (recorded digest %s, live digest %s)", op.Key, op.Target.ID, op.Target.BodyDigest, liveDigest)}, nil
		}
		var lite struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &lite); err != nil {
			return nil, fmt.Errorf("decode candidate %s: %w", op.Target.ID, err)
		}
		if lite.State != op.Target.State {
			return &frozenDriftReason{Reason: fmt.Sprintf("operation %s drift: candidate %s state changed (recorded %s, live %s)", op.Key, op.Target.ID, op.Target.State, lite.State)}, nil
		}
	}
	return nil, nil
}

func runFrozenPass(homePath string, store *mem.Store, recorded map[string]PassOperationResult, now time.Time, report *Report) error {
	if report.Intent == nil {
		return fmt.Errorf("frozen pass intent required")
	}
	intent, err := clonePassIntent(*report.Intent)
	if err != nil {
		return fmt.Errorf("clone frozen pass intent: %w", err)
	}
	if err := validatePassIntent(report.PassID, intent); err != nil {
		return fmt.Errorf("validate frozen pass: %w", err)
	}
	executionAt, err := time.Parse(time.RFC3339, intent.ExecutionAt)
	if err != nil {
		return fmt.Errorf("parse frozen execution time: %w", err)
	}
	results := make(map[string]PassOperationResult, len(recorded))
	for key, result := range recorded {
		if err := validateOperationResult(intent, result); err != nil {
			return fmt.Errorf("validate recorded operation %s: %w", key, err)
		}
		results[key] = result
	}
	for _, operation := range intent.Operations {
		if _, ok := results[operation.Key]; ok {
			continue
		}
		frozen := mem.FrozenAdjudication{
			OperationKey: operation.Key,
			Target: mem.FrozenCandidatePrecondition{
				CandidateID: operation.Target.ID,
				BodyDigest:  operation.Target.BodyDigest,
				State:       operation.Target.State,
			},
		}
		if operation.CurrentClaim != nil {
			frozen.CurrentClaim = &mem.FrozenClaimPrecondition{
				ID: operation.CurrentClaim.ID, Absent: operation.CurrentClaim.Absent,
			}
		}
		if operation.Keeper != nil {
			frozen.Companion = &mem.FrozenCandidatePrecondition{
				CandidateID: operation.Keeper.ID,
				BodyDigest:  operation.Keeper.BodyDigest,
				State:       operation.Keeper.State,
			}
		}
		applied, applyErr := store.AdjudicateFrozen(frozen, operation.Operation, intent.WorkID, executionAt)
		if applyErr != nil {
			return passRecoveryError{err: fmt.Errorf("operation %s: %w", operation.Key, applyErr)}
		}
		result := PassOperationResult{
			Key:                  operation.Key,
			Ordinal:              operation.Ordinal,
			Phase:                operation.Phase,
			Operation:            operation.Operation,
			CandidateID:          operation.Target.ID,
			Outcome:              applied.Outcome,
			Verdict:              applied.Adjudication.Verdict,
			CandidateState:       applied.Adjudication.Candidate.State,
			BeforeDigest:         applied.BeforeDigest,
			AfterDigest:          applied.AfterDigest,
			MismatchCandidateID:  applied.MismatchCandidateID,
			PreconditionMismatch: applied.PreconditionMismatch,
			SupersededClaimID:    applied.Adjudication.Superseded,
			Note:                 applied.Adjudication.Note,
		}
		if applied.Adjudication.Claim != nil {
			result.ClaimID = applied.Adjudication.Claim.ID
		}
		if applied.Adjudication.Transition != nil {
			result.TransitionID = applied.Adjudication.Transition.ID
		}
		if err := validateOperationResult(intent, result); err != nil {
			return passRecoveryError{err: fmt.Errorf("operation %s returned invalid result: %w", operation.Key, err)}
		}
		frame := Report{
			SchemaVersion:    report.SchemaVersion,
			PassID:           report.PassID,
			Home:             homePath,
			Stage:            stageOperationRecorded,
			StartedAt:        report.StartedAt,
			FinishedAt:       rfc3339(now),
			Intent:           report.Intent,
			OperationResults: []PassOperationResult{result},
		}
		if err := receiptAppender(homePath, frame); err != nil {
			return passRecoveryError{err: fmt.Errorf("journal operation %s result: %w", operation.Key, err)}
		}
		results[operation.Key] = result
	}

	report.OperationResults = orderedOperationResults(intent, results)
	summarizeFrozenResults(intent, report.OperationResults, report)
	for _, result := range report.OperationResults {
		if result.Outcome == mem.FrozenAdjudicationStalePrecondition {
			return fmt.Errorf("stale_precondition: operation %s candidate %s drifted at %s", result.Key, result.CandidateID, operationMismatch(result))
		}
	}
	var finalization PassFinalizationResult
	if report.Finalization != nil {
		finalization = *report.Finalization
		if report.Finalization.Pending != nil {
			pending := *report.Finalization.Pending
			finalization.Pending = &pending
		}
		if err := validateFinalizationResult(intent, finalization); err != nil {
			return fmt.Errorf("validate recorded finalization: %w", err)
		}
	} else {
		finalization, err = finalizeFrozenPass(homePath, store, intent, executionAt)
		if err != nil {
			return passRecoveryError{err: err}
		}
		if report.SchemaVersion == ReportSchemaVersion {
			frameFinalization := finalization
			frame := Report{
				SchemaVersion: report.SchemaVersion,
				PassID:        report.PassID,
				Home:          homePath,
				Stage:         stageFinalizationRecorded,
				StartedAt:     report.StartedAt,
				FinishedAt:    rfc3339(now),
				Intent:        report.Intent,
				Finalization:  &frameFinalization,
			}
			if err := receiptAppender(homePath, frame); err != nil {
				return passRecoveryError{err: fmt.Errorf("journal finalization result: %w", err)}
			}
			report.Finalization = &frameFinalization
		}
	}
	applyFinalizationResult(finalization, report)
	if failures := partialFailures(*report); len(failures) > 0 {
		return partialPassError{failures: failures}
	}
	return nil
}

func summarizeFrozenResults(intent PassIntent, results []PassOperationResult, report *Report) {
	report.Promote = intent.PromotePlan
	report.Promote.Promoted = 0
	for index := range report.Promote.Promotions {
		report.Promote.Promotions[index].Action = "planned"
		report.Promote.Promotions[index].ClaimID = ""
	}
	report.Dedupe = intent.DedupePlan
	report.Dedupe.Collapsed = 0
	for index := range report.Dedupe.Actions {
		report.Dedupe.Actions[index].Action = "planned"
	}
	report.Sweep = intent.SweepPlan
	report.Sweep.Adjudicated = 0
	report.Sweep.Failed = 0
	report.Sweep.ChangedIDs = nil
	report.Sweep.VerdictCounts = map[string]int{}
	report.Sweep.ReconciledPending = nil
	report.PendingAfter = nil
	report.Imports = ImportsPhaseReport{Skipped: "registry import mutation is experimental and excluded from supported candidate-only apply"}
	report.LaneBeliefs = LaneBeliefsPhaseReport{Skipped: "lane-belief mutation is experimental and excluded from supported candidate-only apply"}

	byKey := make(map[string]PassOperationResult, len(results))
	for _, result := range results {
		byKey[result.Key] = result
		if result.Outcome != mem.FrozenAdjudicationStalePrecondition {
			continue
		}
		message := fmt.Sprintf("%s: stale_precondition at %s", result.CandidateID, operationMismatch(result))
		switch result.Phase {
		case "promote":
			report.Promote.Errors = append(report.Promote.Errors, message)
		case "dedupe":
			report.Dedupe.Errors = append(report.Dedupe.Errors, message)
		case "sweep":
			report.Sweep.Errors = append(report.Sweep.Errors, message)
			report.Sweep.Failed++
		}
	}
	for _, operation := range intent.Operations {
		result, ok := byKey[operation.Key]
		if !ok || (result.Outcome != mem.FrozenAdjudicationApplied && result.Outcome != mem.FrozenAdjudicationAlreadyApplied) {
			continue
		}
		switch operation.Phase {
		case "promote":
			for index := range report.Promote.Promotions {
				if report.Promote.Promotions[index].CandidateID != operation.Target.ID {
					continue
				}
				report.Promote.Promotions[index].ClaimID = result.ClaimID
				if result.CandidateState == "promoted" {
					report.Promote.Promotions[index].Action = "promoted"
					report.Promote.Promoted++
				} else {
					report.Promote.Promotions[index].Action = "rejected_duplicate"
				}
			}
		case "dedupe":
			report.Dedupe.Collapsed++
		case "sweep":
			report.Sweep.Adjudicated++
			report.Sweep.VerdictCounts[operation.Operation]++
			report.Sweep.ChangedIDs = append(report.Sweep.ChangedIDs, operation.Target.ID)
		}
	}
	for index := range report.Dedupe.Actions {
		action := &report.Dedupe.Actions[index]
		complete := true
		for _, candidateID := range action.NoPromoted {
			found := false
			for _, operation := range intent.Operations {
				if operation.Phase != "dedupe" || operation.Target.ID != candidateID {
					continue
				}
				result, ok := byKey[operation.Key]
				found = ok && (result.Outcome == mem.FrozenAdjudicationApplied || result.Outcome == mem.FrozenAdjudicationAlreadyApplied)
			}
			complete = complete && found
		}
		if complete {
			action.Action = "collapsed"
		}
	}
}

func finalizeFrozenPass(homePath string, store *mem.Store, intent PassIntent, executionAt time.Time) (PassFinalizationResult, error) {
	result := PassFinalizationResult{
		PromoteCursorAfter: intent.PromotePlan.NextCursor,
		SweepCursorAfter:   intent.SweepPlan.NextCursor,
	}
	if err := finalizePromoteCursor(homePath, intent.PromoteCursorBefore, intent.PromotePlan.NextCursor, executionAt); err != nil {
		return PassFinalizationResult{}, fmt.Errorf("finalize promote cursor: %w", err)
	}
	if err := finalizeSweepCursor(homePath, intent.SweepCursorBefore, intent.SweepPlan.NextCursor, executionAt); err != nil {
		return PassFinalizationResult{}, fmt.Errorf("finalize sweep cursor: %w", err)
	}
	if intent.SweepPlan.WalkedAll {
		state, err := store.ReconcilePendingState("promote-driver frozen sweep", executionAt)
		if err != nil {
			return PassFinalizationResult{}, fmt.Errorf("reconcile pending state: %w", err)
		}
		result.Pending = &PendingFinalizationResult{
			Pending: state.Pending, Files: state.Files, Corrupt: state.Corrupt, Generation: state.Generation,
		}
	}
	if err := validateFinalizationResult(intent, result); err != nil {
		return PassFinalizationResult{}, err
	}
	return result, nil
}

func applyFinalizationResult(result PassFinalizationResult, report *Report) {
	if result.Pending == nil {
		return
	}
	pending := result.Pending.Pending
	report.Sweep.ReconciledPending = &pending
	report.PendingAfter = &pending
}

func finalizePromoteCursor(homePath string, before, after candidates.EntryCursor, now time.Time) error {
	current, err := readPromoteCursor(homePath)
	if err != nil {
		return err
	}
	if current.AfterCursor == after {
		return nil
	}
	if current.AfterCursor != before {
		return fmt.Errorf("cursor precondition changed from %q to %q (planned %q)", before, current.AfterCursor, after)
	}
	if after == "" {
		return clearPromoteCursor(homePath)
	}
	return promoteCursorPersist(homePath, promoteCursor{AfterCursor: after, UpdatedAt: rfc3339(now)})
}

func finalizeSweepCursor(homePath string, before, after candidates.EntryCursor, now time.Time) error {
	current, err := readSweepCursor(homePath)
	if err != nil {
		return err
	}
	if current.AfterCursor == after {
		return nil
	}
	if current.AfterCursor != before {
		return fmt.Errorf("cursor precondition changed from %q to %q (planned %q)", before, current.AfterCursor, after)
	}
	if after == "" {
		return clearSweepCursor(homePath)
	}
	return writeSweepCursor(homePath, sweepCursor{AfterCursor: after, UpdatedAt: rfc3339(now)})
}

type partialPassError struct{ failures []string }

func (e partialPassError) Error() string {
	return "partial pass: " + strings.Join(e.failures, "; ")
}

func partialFailures(report Report) []string {
	var failures []string
	for _, value := range report.Promote.Errors {
		failures = append(failures, "promote: "+value)
	}
	for _, value := range report.Dedupe.Errors {
		failures = append(failures, "dedupe: "+value)
	}
	for _, source := range report.Imports.Sources {
		if source.Error != "" {
			failures = append(failures, "import "+source.Path+": "+source.Error)
		}
	}
	for _, value := range report.Sweep.Errors {
		failures = append(failures, "sweep: "+value)
	}
	return failures
}

func terminalStageForError(err error) string {
	if err == nil {
		return stageCommitted
	}
	var partial partialPassError
	if errors.As(err, &partial) {
		return stagePartial
	}
	return stageFailed
}

// runPlan is the read-only pass. It opens the ledger read-only and touches
// nothing: no lock, no cursors, no receipts, no pending-state writes. A
// plan pass must be runnable from any read-classified scheduler on any home
// without leaving footprints. A home with no memory ledger yet (never
// captured) is a legitimate plan target: the pass reports empty phases and
// still exits 0, because "nothing to digest" is a true plan, not an error.
func runPlan(homePath string, opts Options, now time.Time, report Report) (Report, error) {
	store, err := mem.OpenScan(homePath)
	if err != nil {
		return report, fmt.Errorf("open memory store for scan: %w", err)
	}
	intent, err := buildPassIntent(store, opts, now)
	if err != nil {
		return report, fmt.Errorf("build pass intent: %w", err)
	}
	report.Intent = &intent
	report.Promote, report.Dedupe, err = runBasisAndDedupe(store, opts, now)
	if err != nil {
		return report, fmt.Errorf("basis/dedupe phase: %w", err)
	}
	imports, err := runImports(homePath, store, opts, now)
	report.Imports = imports
	if err != nil {
		return report, fmt.Errorf("imports phase: %w", err)
	}
	report.LaneBeliefs = runLaneBeliefs(homePath, Options{}, now)
	if report.LaneBeliefs.Error != "" {
		return report, fmt.Errorf("lane-beliefs phase: %s", report.LaneBeliefs.Error)
	}
	sweep, err := runSweep(store, opts, now)
	if err != nil {
		report.Sweep = sweep
		return report, fmt.Errorf("sweep phase: %w", err)
	}
	report.Sweep = sweep
	report.FinishedAt = rfc3339(time.Now().UTC())
	if failures := partialFailures(report); len(failures) > 0 {
		return report, partialPassError{failures: failures}
	}
	return report, nil
}

// runBasisAndDedupe performs one budget-bounded walk over the inbox and
// applies the two pre-sweep phases to proposed candidates: basis-gated
// promotion (per-id, limited) and exact duplicate collapse.
//
// The walk carries its OWN resume cursor, separate from the sweep's. This
// is what keeps the promotion chance alive on a saturated inbox: closed
// candidates stay on disk forever (adjudication never deletes), so a
// walk-from-start with a fixed budget would re-read the same leading
// window every pass and never reach proposed candidates sorting past it.
// With the cursor, successive passes tile the whole inbox; the worst-case
// discovery latency for a new basis-carrying candidate is one full cycle
// (total / budget passes).
//
// Duplicate collapse is stricter: it runs ONLY when a walk started at the
// beginning and reached the end within the pass, so no group ever splits
// across passes. On an inbox larger than the walk budget this phase
// reports skipped — the operator can force it with a larger
// --walk-scan-limit during maintenance.
func runBasisAndDedupe(store *mem.Store, opts Options, now time.Time) (PromotePhaseReport, DedupePhaseReport, error) {
	dryRun := !opts.Apply
	promote := PromotePhaseReport{Limit: opts.PromoteLimit}
	dedupe := DedupePhaseReport{}

	homePath := store.HomePath()
	cursor, err := readPromoteCursor(homePath)
	if err != nil {
		return promote, dedupe, err
	}
	promote.ResumedFrom = cursor.AfterCursor

	page, err := store.ListCandidatesLite(cursor.AfterCursor, opts.WalkScanLimit)
	if err != nil {
		return promote, dedupe, err
	}
	promote.Walked = page.Walked

	groups := map[string][]mem.CandidateLite{}
	safeCursor := cursor.AfterCursor
	blocked := false
	for _, outcome := range page.Entries {
		if outcome.Corruption != nil {
			promote.Corrupt++
			promote.CorruptIDs = append(promote.CorruptIDs, outcome.Cursor)
			if !blocked {
				safeCursor = outcome.Cursor
			}
			continue
		}

		promote.Decoded++
		promote.Scanned++ // compatibility: scanned historically meant decoded candidates.
		entry := *outcome.Candidate
		if entry.State != "proposed" {
			if !blocked {
				safeCursor = outcome.Cursor
			}
			continue
		}
		promote.Considered++
		basis := strings.TrimSpace(entry.PromotionBasis)
		if candidates.ValidBasis(basis) && basis != candidates.BasisExplicitConfirmation {
			if blocked || promote.Promoted >= opts.PromoteLimit {
				promote.Deferred = append(promote.Deferred, entry.ID)
				blocked = true
				continue
			}
			receipt := PromotionReceipt{CandidateID: entry.ID, Basis: basis}
			if dryRun {
				receipt.Action = "would_promote"
			} else {
				res, err := store.Adjudicate(entry.ID, candidates.Promote, opts.WorkID, now)
				if err != nil {
					promote.Errors = append(promote.Errors, fmt.Sprintf("%s: %v", entry.ID, err))
					blocked = true
					continue
				}
				receipt.Action = "promoted"
				if res.Claim != nil {
					receipt.ClaimID = res.Claim.ID
				}
			}
			promote.Promotions = append(promote.Promotions, receipt)
			promote.Promoted++
			safeCursor = outcome.Cursor
			continue
		}
		if blocked {
			continue
		}
		key := dedupeKey(entry)
		groups[key] = append(groups[key], entry)
		safeCursor = outcome.Cursor
	}

	walkedAll := cursor.AfterCursor == "" && page.AtEnd && !blocked
	if blocked || !page.AtEnd {
		promote.NextCursor = safeCursor
		if opts.Apply && safeCursor != "" {
			if err := promoteCursorPersist(homePath, promoteCursor{AfterCursor: safeCursor, UpdatedAt: rfc3339(now)}); err != nil {
				return promote, dedupe, fmt.Errorf("write promote cursor: %w", err)
			}
		}
	} else if opts.Apply {
		if err := clearPromoteCursor(homePath); err != nil {
			return promote, dedupe, fmt.Errorf("clear promote cursor: %w", err)
		}
	}
	if !walkedAll {
		if blocked {
			dedupe.Skipped = "promotion write cap or failure left an actionable candidate before the safe cursor frontier"
		} else if cursor.AfterCursor != "" {
			dedupe.Skipped = "resume walk: duplicate groups may split across passes; collapse runs on a full walk from the start"
		} else {
			dedupe.Skipped = "walk budget exhausted before the inbox end; collapse runs on a full walk (raise --walk-scan-limit)"
		}
		return promote, dedupe, nil
	}
	var keys []string
	for key, group := range groups {
		if len(group) > 1 {
			keys = append(keys, key)
			dedupe.Groups++
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool {
			if group[i].CreatedAt != group[j].CreatedAt {
				return group[i].CreatedAt < group[j].CreatedAt
			}
			return group[i].ID < group[j].ID
		})
		keeper := group[0]
		action := DedupeAction{Key: key, Kept: keeper.ID}
		for _, dup := range group[1:] {
			if dryRun {
				action.NoPromoted = append(action.NoPromoted, dup.ID)
				continue
			}
			if _, err := store.Adjudicate(dup.ID, candidates.NoPromote, opts.WorkID, now); err != nil {
				dedupe.Errors = append(dedupe.Errors, fmt.Sprintf("%s: %v", dup.ID, err))
				continue
			}
			action.NoPromoted = append(action.NoPromoted, dup.ID)
		}
		if dryRun {
			action.Action = "would_collapse"
		} else {
			action.Action = "collapsed"
		}
		dedupe.Actions = append(dedupe.Actions, action)
		dedupe.Collapsed += len(action.NoPromoted)
	}
	return promote, dedupe, nil
}

// dedupeKey identifies EXACT duplicates: same entity, predicate, and
// object text. Near-duplicates (a re-mined preference whose object text
// carries a changed evidence count) are deliberately NOT collapsed — they
// park with the sweep and remain operator material.
func dedupeKey(c mem.CandidateLite) string {
	return c.Entity + "\x1f" + c.Predicate + "\x1f" + c.Object
}

// runImports refreshes the deterministic product-registry imports: content
// hash per source file, re-Apply only when the content changed. Idempotent
// by construction — an unchanged registry costs one hash read.
func runImports(homePath string, store *mem.Store, opts Options, now time.Time) (ImportsPhaseReport, error) {
	report := ImportsPhaseReport{}
	if len(opts.RegistryPaths) == 0 {
		report.Skipped = "no registry paths configured"
		return report, nil
	}
	hashes, hashErr := readImportHashes(homePath)
	if hashErr != nil {
		report.Error = hashErr.Error()
		return report, hashErr
	}
	// Registry entries may be glob patterns (the heartbeat passes
	// ~/Projects/products/*/project.yaml): expand before the per-source loop
	// so one pattern covers every contract in the registry. A pattern that
	// matches nothing is reported, never silently dropped.
	var paths []string
	for _, pattern := range opts.RegistryPaths {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			report.Sources = append(report.Sources, ImportSourceReport{Path: pattern, Action: "error", Error: "bad glob: " + err.Error()})
			continue
		}
		if len(matches) == 0 {
			report.Sources = append(report.Sources, ImportSourceReport{Path: pattern, Action: "error", Error: "glob matched no files"})
			continue
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	for _, path := range paths {
		source := ImportSourceReport{Path: path}
		data, err := os.ReadFile(path)
		if err != nil {
			source.Error = err.Error()
			report.Sources = append(report.Sources, source)
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		source.HashBefore = hashes[path]
		source.HashAfter = hash
		if source.HashBefore == hash {
			source.Action = "up_to_date"
			report.Sources = append(report.Sources, source)
			continue
		}
		if !opts.Apply {
			source.Action = "would_import"
			report.Sources = append(report.Sources, source)
			continue
		}
		bundle, err := product.LoadProjectContractBundle(path, "")
		if err != nil {
			source.Error = "load project contract: " + err.Error()
			report.Sources = append(report.Sources, source)
			continue
		}
		applyReport, err := product.Apply(homePath, store, bundle, path, now)
		if err != nil {
			source.Error = "apply: " + err.Error()
			report.Sources = append(report.Sources, source)
			continue
		}
		source.Action = "imported"
		source.ClaimIDs = applyReport.ClaimIDs
		hashes[path] = hash
		report.Sources = append(report.Sources, source)
	}
	if opts.Apply {
		if err := writeImportHashes(homePath, hashes); err != nil {
			report.Error = err.Error()
			// The hash ledger is what makes re-import idempotent: losing
			// this write after applying imports means the NEXT pass
			// re-imports everything while this one stamps stage=committed.
			// State persistence failures fail the pass (same posture as the
			// promote/sweep cursors).
			return report, fmt.Errorf("write import hashes: %w", err)
		}
	}
	return report, nil
}

// runLaneBeliefs runs the B2/B3/B4 lane-belief pipeline in order: fold
// (propose), promote (cross-lane gate), cool. Each phase is the existing
// organ with its own gates; the driver only sequences them. Apply passes
// run the real pipeline; the plan pass sequences the same organs in
// dry-run so a plan never writes through them.
var runLaneBeliefs = runLaneBeliefsOwned

func runLaneBeliefsOwned(homePath string, opts Options, now time.Time) LaneBeliefsPhaseReport {
	dryRun := !opts.Apply
	report := LaneBeliefsPhaseReport{}
	fold, err := lanebeliefs.Fold(homePath, lanebeliefs.FoldOptions{DryRun: dryRun, Now: now})
	if err != nil {
		report.Error = "fold: " + err.Error()
		return report
	}
	report.Fold = fold
	promote, err := lanebeliefs.Promote(homePath, lanebeliefs.PromoteOptions{DryRun: dryRun, Now: now})
	if err != nil {
		report.Error = "promote: " + err.Error()
		return report
	}
	report.Promote = promote
	cool, err := lanebeliefs.Cool(homePath, lanebeliefs.CoolOptions{DryRun: dryRun, Now: now})
	if err != nil {
		report.Error = "cool: " + err.Error()
		return report
	}
	report.Cool = cool
	return report
}

// runSweep runs the budget-bounded disposition pass (auto no_promote/park;
// promotion is refused by that door and the driver never asks it to). The
// cursor persists between APPLY passes; a pass that walks the whole inbox
// reconciles the pending count — the honest backlog number doctor reads.
// Cursor persistence failures are reported, never swallowed: a sweep that
// mutated candidates but failed to persist its continuation point must say
// so, because the next pass will repeat or resume inconsistently.
func runSweep(store *mem.Store, opts Options, now time.Time) (SweepPhaseReport, error) {
	report := SweepPhaseReport{ScanLimit: opts.SweepScanLimit}
	homePath := store.HomePath()
	cursor, err := readSweepCursor(homePath)
	if err != nil {
		return report, err
	}
	pendingReport := store.AdjudicatePending(mem.AdjudicatePendingOptions{
		WorkID:      opts.WorkID,
		DryRun:      !opts.Apply,
		ScanLimit:   opts.SweepScanLimit,
		AfterCursor: cursor.AfterCursor,
		ReservedIDs: opts.reservedCandidateIDs,
	}, now)
	report.Walked = pendingReport.Walked
	report.Decoded = pendingReport.Decoded
	report.Corrupt = pendingReport.Corrupt
	report.Scanned = pendingReport.Scanned
	report.Adjudicated = pendingReport.Adjudicated
	report.Skipped = pendingReport.Skipped
	report.Deferred = pendingReport.Deferred
	report.Failed = pendingReport.Failed

	report.BasisDeferred = pendingReport.BasisDeferred
	report.VerdictCounts = pendingReport.VerdictCounts
	report.CorruptIDs = pendingReport.CorruptIDs
	report.ChangedIDs = append([]string(nil), pendingReport.ChangedIDs...)
	for _, action := range pendingReport.Actions {
		report.Actions = append(report.Actions, SweepAction{CandidateID: action.CandidateID, Verdict: action.Verdict})
	}
	for _, e := range pendingReport.Errors {
		identity := e.CandidateID
		if identity == "" {
			identity = cursorDisplay(e.EntryCursor)
		}
		report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", identity, e.Err))
	}
	report.NextCursor = pendingReport.NextCursor
	if report.NextCursor != "" && opts.Apply {
		if err := writeSweepCursor(homePath, sweepCursor{AfterCursor: report.NextCursor, UpdatedAt: rfc3339(now)}); err != nil {
			return report, fmt.Errorf("write sweep cursor: %w", err)
		}
	}
	if pendingReport.Failed > 0 {
		return report, fmt.Errorf("pending sweep stopped at %d failed actionable candidate(s)", pendingReport.Failed)
	}
	report.WalkedAll = pendingReport.AtEnd && !pendingReport.LimitReached && pendingReport.NextCursor == ""
	if report.WalkedAll {
		if opts.Apply {
			if err := clearSweepCursor(homePath); err != nil {
				return report, fmt.Errorf("clear sweep cursor: %w", err)
			}
			st, err := store.ReconcilePendingState("promote-driver sweep", now)
			if err != nil {
				return report, fmt.Errorf("reconcile pending state: %w", err)
			}
			report.ReconciledPending = &st.Pending
		}
		return report, nil
	}
	return report, nil
}

func cursorDisplay(cursor candidates.EntryCursor) string {
	if cursor == "" {
		return "*"
	}
	token, err := cursor.Token()
	if err != nil {
		return fmt.Sprintf("entry[%x]", []byte(cursor))
	}
	return token
}

// --- single flight ---

// acquireSingleFlight takes the driver's lock without blocking. A held
// lock means another pass is running; the doctrine is keep waiting, never
// contend — the caller gets Locked=true and does nothing.
func acquireSingleFlight(homePath string) (release func(), locked bool, err error) {
	lockPath := filepath.Join(homePath, lockFileName)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open driver lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, true, nil
		}
		_ = file.Close()
		return nil, false, fmt.Errorf("lock driver pass: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, false, nil
}

// --- state + receipts ---

type sweepCursor struct {
	AfterCursor candidates.EntryCursor `json:"after_cursor,omitempty"`
	AfterID     string                 `json:"after_id,omitempty"` // legacy read-only field
	UpdatedAt   string                 `json:"updated_at"`
}

type promoteCursor struct {
	AfterCursor candidates.EntryCursor `json:"after_cursor,omitempty"`
	AfterID     string                 `json:"after_id,omitempty"` // legacy read-only field
	UpdatedAt   string                 `json:"updated_at"`
}

func readPromoteCursor(homePath string) (promoteCursor, error) {
	path := filepath.Join(stateDir(homePath), promoteCursorFileName)
	var c promoteCursor
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return promoteCursor{}, fmt.Errorf("promote cursor %s is corrupt: %w", path, err)
	}
	resolved, err := resolveStoredCursor(c.AfterCursor, c.AfterID)
	if err != nil {
		return promoteCursor{}, fmt.Errorf("promote cursor %s is corrupt: %w", path, err)
	}
	c.AfterCursor = resolved
	c.AfterID = ""
	return c, nil
}

func writePromoteCursor(homePath string, c promoteCursor) error {
	resolved, err := resolveStoredCursor(c.AfterCursor, c.AfterID)
	if err != nil {
		return err
	}
	c.AfterCursor = resolved
	c.AfterID = ""
	return writeStateFile(homePath, promoteCursorFileName, c)
}

func clearPromoteCursor(homePath string) error {
	if err := os.Remove(filepath.Join(stateDir(homePath), promoteCursorFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func stateDir(homePath string) string { return filepath.Join(homePath, stateDirName) }

func readSweepCursor(homePath string) (sweepCursor, error) {
	path := filepath.Join(stateDir(homePath), cursorFileName)
	var c sweepCursor
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return sweepCursor{}, fmt.Errorf("sweep cursor %s is corrupt: %w", path, err)
	}
	resolved, err := resolveStoredCursor(c.AfterCursor, c.AfterID)
	if err != nil {
		return sweepCursor{}, fmt.Errorf("sweep cursor %s is corrupt: %w", path, err)
	}
	c.AfterCursor = resolved
	c.AfterID = ""
	return c, nil
}

func resolveStoredCursor(afterCursor candidates.EntryCursor, legacyAfterID string) (candidates.EntryCursor, error) {
	if afterCursor != "" {
		if _, err := afterCursor.Token(); err != nil {
			return "", err
		}
		if legacyAfterID != "" {
			legacy, err := candidates.EntryCursorFromFilename(legacyAfterID + ".json")
			if err != nil {
				return "", fmt.Errorf("legacy after_id %q: %w", legacyAfterID, err)
			}
			if legacy != afterCursor {
				return "", fmt.Errorf("after_cursor %q conflicts with legacy after_id %q", afterCursor, legacyAfterID)
			}
		}
		return afterCursor, nil
	}
	if legacyAfterID == "" {
		return "", nil
	}
	if strings.TrimSpace(legacyAfterID) != legacyAfterID {
		return "", fmt.Errorf("legacy after_id %q has surrounding whitespace", legacyAfterID)
	}
	resolved, err := candidates.EntryCursorFromFilename(legacyAfterID + ".json")
	if err != nil {
		return "", fmt.Errorf("legacy after_id %q: %w", legacyAfterID, err)
	}
	return resolved, nil
}

// promoteCursorWriter is a test seam for deterministic promote-cursor
// persistence failure, mirroring sweepCursorWriter.
var promoteCursorWriter func(homePath string, c promoteCursor) error

func promoteCursorPersist(homePath string, c promoteCursor) error {
	if promoteCursorWriter != nil {
		return promoteCursorWriter(homePath, c)
	}
	return writePromoteCursor(homePath, c)
}

// sweepCursorWriter is a test seam for deterministic cursor-persistence
// failure at the exact point an apply pass needs its continuation point.
// Production passes leave it nil and persist through the owned state file.
var sweepCursorWriter func(homePath string, c sweepCursor) error

func writeSweepCursor(homePath string, c sweepCursor) error {
	if sweepCursorWriter != nil {
		return sweepCursorWriter(homePath, c)
	}
	resolved, err := resolveStoredCursor(c.AfterCursor, c.AfterID)
	if err != nil {
		return err
	}
	c.AfterCursor = resolved
	c.AfterID = ""
	return writeStateFile(homePath, cursorFileName, c)
}

func clearSweepCursor(homePath string) error {
	if err := os.Remove(filepath.Join(stateDir(homePath), cursorFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func readImportHashes(homePath string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Join(stateDir(homePath), hashFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		// A corrupt hash ledger is NOT "no hashes": silently treating it as
		// empty re-imports everything, and the next successful write
		// overwrites the evidence. Surface it and let the caller fail the
		// pass; repair is an explicit operator action, not a side effect of
		// a scheduled pump.
		return nil, fmt.Errorf("import hash state %s is corrupt: %w (repair deliberately, e.g. remove the file to force a full re-import)", filepath.Join(stateDir(homePath), hashFileName), err)
	}
	return out, nil
}

func writeImportHashes(homePath string, hashes map[string]string) error {
	return writeStateFile(homePath, hashFileName, hashes)
}

func writeStateFile(homePath, name string, v any) error {
	dir := stateDir(homePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

var receiptAppender = appendReceipt

// appendReceipt adds the pass report to the append-only receipt log.
// Receipts are never rewritten or deleted: the log is the audit trail that
// makes every autonomous verdict reversible-by-inspection.
func appendReceipt(homePath string, report Report) error {
	dir := stateDir(homePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, receiptFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// passID mints a collision-proof pass identifier: nanosecond-resolution time
// plus cryptographic randomness. Second-resolution time hashed with the home
// path collided on two sequential passes within one second; a journal that
// keys receipts on the pass id must never be able to mint duplicates.
func passID(now time.Time) string {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// crypto/rand failing is a host anomaly; fall back to the clock's
		// own nanosecond bits rather than aborting the pass.
		binary.BigEndian.PutUint32(nonce[:], uint32(now.UnixNano()))
	}
	return fmt.Sprintf("pass_%s_%s", now.Format("20060102T150405.000000000Z"), hex.EncodeToString(nonce[:]))
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
