package main

import (
	"bytes"
	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/work"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const proofCheckSchemaVersion = work.ProofCheckSchemaVersion

type proofCheckReport = work.ProofCheckReport

type proofCheckStep = work.ProofCheckStep

var proofCheckCanonicalRunner = runProofCheckSealedSnapshotContext
var proofCheckRuntimeArtifactRecorder = recordProofCheckRuntimeArtifact

func cmdProofCheck(args []string) {
	if work.HasProofRecoveryFlag(args, "--reconcile-quiescent") || work.HasProofRecoveryFlag(args, "--resume-retained") {
		os.Exit(work.RunProofRecoveryCLI(args, os.Stdout, os.Stderr, proofCheckRestoreRetainedReport))
	}
	if hasFlag(args, "--remote-reconcile") {
		os.Exit(work.RunRemoteProofRecoveryCLI(args, os.Stdout, os.Stderr))
	}
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("usage: compound proof-check [--json] [--dry-run] [--dev|--presubmit|--ci|--ci-console] [--repo P] [--base REF] [--out FILE] [--work ID --actor ACTOR]")
		fmt.Println(work.RemoteProofHelp)
		fmt.Println(work.ProofRecoveryHelp)
		fmt.Println("  --dev         fast, cache-enabled, non-certifying developer feedback")
		fmt.Println("  --presubmit   non-certifying pre-landing gate; go-test scope derived from --base FULL_SHA...HEAD")
		fmt.Println("  --redact-review ACT_ID exact native public-content review for the complete source snapshot (canonical full proof only)")
		fmt.Println("  --ci          run only the fresh-clone-portable gate subset (bare Go gates; what CI runs on push/PR)")
		fmt.Println("  --ci-console  run console/TypeScript gates (needs bun + console/node_modules; CI console job, not bare-clone --ci)")
		fmt.Println("  --out FILE    write full JSON and print final 'battery: ok|FAILED'; exit code stays truthful (0/1/2)")
		fmt.Println("  --timings     report the recorded per-gate duration history instead of running the battery")
		fmt.Println("                (--dev or --presubmit selects that lane, --window N the sample depth)")
		fmt.Println("  --work ID --actor ACTOR  required for a full local battery; reserves the scarce proof resource")
		fmt.Println("  --ttl DURATION            reservation TTL (minimum 30s, default 45m; renewed by RunAttempt heartbeat)")
		fmt.Println("  --wait DURATION           queue up to DURATION for a busy proof resource instead of refusing")
		fmt.Println("  --execution-timeout DURATION")
		fmt.Println("                bounded active-operation budget for preparation plus the admitted run;")
		fmt.Println("                queue waiting is excluded; canonical full proof only")
		fmt.Println("  --steal --reason TEXT     exceptional audited transfer of a live proof/land resource")
		fmt.Println("  --terminated-run RATT_ID --termination-evidence ARTIFACT_REF")
		fmt.Println("                governed proof that the oldest stopped predecessor was terminated and waited")
		fmt.Println("  --recover-legacy-retained release exact pre-v0.3 journal without verification;")
		fmt.Println("                never runs gates and exits 2 because fresh source-bound v0.3 battery is required")
		return
	}
	routeOptions, err := work.ParseProofCheckRouteOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proof-check:", err)
		os.Exit(2)
	}
	if err := proofCheckLegacyRecoveryModeError(args); err != nil {
		fmt.Fprintln(os.Stderr, "proof-check:", err)
		os.Exit(2)
	}
	if hasFlag(args, "--timings") {
		cmdProofCheckTimings(args)
		return
	}
	if dropped := work.DropInheritedLoopbackProxy(); len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "proof-check: gates run without the inherited loopback proxy (%s)\n", strings.Join(dropped, ", "))
	}
	proofCheckPruneGoCaches(args)
	if proofCheckDevelopmentRequested(args) {
		cmdProofCheckDevelopment(args)
		return
	}
	jsonOut := hasFlag(args, "--json")
	dryRun := hasFlag(args, "--dry-run")
	ciOnly := hasFlag(args, "--ci")
	ciConsole := hasFlag(args, "--ci-console")
	recoverLegacy := hasFlag(args, "--recover-legacy-retained")
	if ciOnly && ciConsole {
		fmt.Fprintln(os.Stderr, "proof-check: --ci and --ci-console are mutually exclusive")
		os.Exit(2)
	}
	repoPath := flagValue(args, "--repo")
	baseRef := flagValue(args, "--base")
	outPath := strings.TrimSpace(flagValue(args, "--out"))
	workID := strings.TrimSpace(flagValue(args, "--work"))
	actor := strings.TrimSpace(flagValue(args, "--actor"))
	steal := hasFlag(args, "--steal")
	stealReason := strings.TrimSpace(flagValue(args, "--reason"))
	terminatedRunAttemptID := strings.TrimSpace(flagValue(args, "--terminated-run"))
	terminationEvidenceRef := strings.TrimSpace(flagValue(args, "--termination-evidence"))
	governedFullProof := proofCheckNeedsAdmission(dryRun, ciOnly, ciConsole)
	var outCustody *proofCheckOutCustody
	if governedFullProof && !recoverLegacy && strings.TrimSpace(baseRef) == "" {
		fmt.Fprintln(os.Stderr, "proof-check: certifying proof-check requires explicit --base")
		os.Exit(2)
	}
	if outPath != "" {
		var outErr error
		outPath, outErr = filepath.Abs(outPath)
		if outErr != nil {
			fmt.Fprintln(os.Stderr, "proof-check: resolve --out path:", outErr)
			os.Exit(2)
		}
		// Fail-closed BEFORE the battery runs, without truncating an existing report: a
		// conflicting request must not damage the active holder's --out bytes before admission.
		if !recoverLegacy {
			custody, err := proofCheckCaptureOutPath(outPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "proof-check: --out path not writable:", err)
				os.Exit(2)
			}
			if !governedFullProof {
				if err := custody.probeWriteCapability(); err != nil {
					_ = custody.close()
					fmt.Fprintln(os.Stderr, "proof-check: --out path not writable:", err)
					os.Exit(2)
				}
			}
			outCustody = &custody
		}
	}
	if recoverLegacy {
		if !governedFullProof {
			fmt.Fprintln(os.Stderr, "proof-check: --recover-legacy-retained is only valid for the full local proof lane")
			os.Exit(2)
		}
		if workID == "" || actor == "" {
			fmt.Fprintln(os.Stderr, "proof-check: legacy retained recovery requires --work ID and --actor ACTOR")
			os.Exit(2)
		}
		resolvedRepo, err := resolveProofCheckRepo(repoPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		commandIdentity, err := work.ProofCheckInvocationIdentity(
			work.ProofCheckArgsWithResolvedOut(
				work.ProofCheckArgsWithoutLegacyRecovery(args),
				outPath,
			),
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		recovered, found, err := proofCheckLegacyRecoverer(
			homePath,
			workID,
			work.ProofResourceLegacyRecoveryInput{
				Actor:     actor,
				Workspace: resolvedRepo,
				Command:   commandIdentity,
			},
			time.Now(),
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check: legacy retained recovery:", err)
			os.Exit(2)
		}
		if !found {
			fmt.Fprintln(os.Stderr, "proof-check: no exact retained legacy proof completion found")
			os.Exit(2)
		}
		if !recovered.ResourceReleased ||
			!recovered.FreshBatteryRequired ||
			recovered.VerificationRef != "" {
			fmt.Fprintln(os.Stderr, "proof-check: legacy retained recovery returned an invalid release-only result")
			os.Exit(2)
		}
		fmt.Println("battery: FAILED recovery-only release complete; fresh source-bound v0.3 battery required")
		os.Exit(2)
	}
	started := time.Now()

	var report proofCheckReport
	if governedFullProof {
		if workID == "" || actor == "" || outPath == "" {
			fmt.Fprintln(os.Stderr, "proof-check: a full local battery requires --work ID, --actor ACTOR, and --out FILE before any gate subprocess")
			os.Exit(2)
		}
		ttl := defaultProofCheckReservationTTL
		if rawTTL := strings.TrimSpace(flagValue(args, "--ttl")); rawTTL != "" {
			parsed, parseErr := time.ParseDuration(rawTTL)
			if parseErr != nil || parsed < minimumProofCheckReservationTTL {
				fmt.Fprintf(os.Stderr, "proof-check: --ttl must be a duration of at least %s\n", minimumProofCheckReservationTTL)
				os.Exit(2)
			}
			ttl = parsed
		}
		var wait time.Duration
		if hasFlag(args, "--wait") {
			parsed, parseErr := time.ParseDuration(strings.TrimSpace(flagValue(args, "--wait")))
			if parseErr != nil || parsed <= 0 || steal {
				fmt.Fprintln(os.Stderr, "proof-check: --wait needs a positive duration such as 45m and cannot be combined with --steal")
				os.Exit(2)
			}
			wait = parsed
		}
		// The bounded execution budget covers the pre-admission preparation and
		// the admitted run. It is charged, never reset: what preparation spends
		// is subtracted from what the admitted run may still use.
		signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stopSignals()
		executionBudget := work.NewProofExecutionBudget(routeOptions.ExecutionTimeout)
		preparationStarted := time.Now()
		profileContext, cancelProfile := executionBudget.PreparationContext(signalContext)
		defer cancelProfile()
		profileContext = gitrepository.ContextWithCommandFactory(profileContext, proofCheckCommandContext)
		resolvedRepo, err := resolveProofCheckRepo(repoPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		sourceSnapshot, err := gitrepository.CaptureCleanSnapshotContext(profileContext, resolvedRepo, baseRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		plan, err := work.ResolveCanonicalProofPlanContext(profileContext, resolvedRepo, sourceSnapshot, proofCheckSteps)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		if routeOptions.PublicRedactionActionID != "" {
			plan, err = work.BindNativeSourceRedactionReview(plan, homePath, workID, actor, routeOptions.PublicRedactionActionID)
			if err != nil {
				fmt.Fprintln(os.Stderr, "proof-check: public redaction review:", err)
				os.Exit(2)
			}
		}
		// The remote profile sealing used to run on an unbounded background
		// context. It is preparation, so it runs inside the bounded budget and
		// stops with it — before the reservation and before any remote launch.
		profile, err := work.ProofResourceProfileForCanonicalPlanWithRemote(profileContext, plan, homePath, routeOptions.RemoteRoute, proofCheckCommandContext)
		if err != nil {
			if budgetErr := work.ProofExecutionBudgetCause(profileContext); budgetErr != nil {
				fmt.Fprintln(os.Stderr, "proof-check:", budgetErr)
				os.Exit(2)
			}
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		publishCompoundRuntimeArtifact, err := work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(plan, profile, proofCheckSteps)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		commandIdentity, err := work.ProofCheckInvocationIdentity(work.ProofCheckArgsWithResolvedOut(args, outPath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		executionBudget.Charge(time.Since(preparationStarted))
		cancelProfile()
		var completed work.ProofResourceCompleteResult
		signalContext = work.ContextWithGateOutputReportPath(signalContext, outPath)
		report, completed, err = runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
			Context:                signalContext,
			HomePath:               homePath,
			WorkID:                 workID,
			Actor:                  actor,
			Workspace:              resolvedRepo,
			Command:                commandIdentity,
			Profile:                profile,
			ReportPath:             outPath,
			ReportCustody:          outCustody,
			TTL:                    ttl,
			Steal:                  steal,
			StealReason:            stealReason,
			TerminatedRunAttemptID: terminatedRunAttemptID,
			TerminationEvidenceRef: terminationEvidenceRef,
			Wait:                   wait,
			WaitProgress:           os.Stderr,
			Now:                    time.Now,
			ExecutionBudget:        executionBudget,
		}, func(ctx context.Context) (proofCheckReport, error) {
			planContext := work.ContextWithCanonicalProofPlan(ctx, plan)
			if routeOptions.PublicRedactionActionID != "" {
				planContext = work.ContextWithPublicRedactionReview(planContext, homePath, workID, actor)
			}
			produced, runErr := proofCheckCanonicalRunner(planContext, resolvedRepo, false, sourceSnapshot, false, false, time.Now())
			if budgetErr := work.ProofExecutionBudgetCause(ctx); budgetErr != nil {
				if produced.Repo == "" {
					produced.Repo = resolvedRepo
					produced.SourceSnapshot = &sourceSnapshot
				}
				produced, deadlineErr := proofCheckPublishDeadlineReport(outPath, produced, outCustody, budgetErr)
				return produced, errors.Join(deadlineErr, runErr)
			}
			if runErr != nil {
				return produced, runErr
			}
			proofCheckAppendTimingWarnings(
				&produced,
				homePath,
				nil,
				time.Since(started),
				time.Now(),
			)
			// The authoritative report file is durable before completion can attach verification.
			if writeErr := proofCheckWriteReportFileWithCustody(outPath, produced, *outCustody); writeErr != nil {
				return produced, fmt.Errorf("writing --out file: %w", writeErr)
			}
			return produced, nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			if verdict, ok := proofCheckPostReportErrorVerdict(report, outPath, err); ok {
				fmt.Println(verdict)
			}
			os.Exit(2)
		}
		if report.Status == "ok" &&
			completed.VerificationRef != "" &&
			completed.ResourceReleased &&
			!completed.RecoveryJournalRetained &&
			publishCompoundRuntimeArtifact {
			if _, recordErr := proofCheckRuntimeArtifactRecorder(homePath, report); recordErr != nil {
				fmt.Fprintln(os.Stderr, "proof-check: installed-runtime proof publication:", recordErr)
				if verdict, ok := proofCheckPostReportErrorVerdict(report, outPath, recordErr); ok {
					fmt.Println(verdict)
				}
				os.Exit(2)
			}
		}
	} else {
		var err error
		runCtx := work.ContextWithGateOutputReportPath(context.Background(), outPath)
		report, err = runProofCheckContext(runCtx, repoPath, dryRun, baseRef, ciOnly, ciConsole, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check:", err)
			os.Exit(2)
		}
		if !dryRun {
			timingHomePath, timingHomeErr := home.Resolve("")
			proofCheckAppendTimingWarnings(
				&report,
				timingHomePath,
				timingHomeErr,
				time.Since(started),
				time.Now(),
			)
		}
		if outPath != "" {
			// A battery whose evidence file failed to write is not green, even if every gate
			// passed: this exit-2 must win over the normal ok/failed (0/1) exit below.
			if err := proofCheckWriteReportFileWithCustody(outPath, report, *outCustody); err != nil {
				fmt.Fprintln(os.Stderr, "proof-check: writing --out file:", err)
				os.Exit(2)
			}
		}
	}
	// Timing history was finalized before report custody above.
	if jsonOut {
		printJSON(report)
	} else {
		printProofCheck(report)
	}
	if outPath != "" {
		// Last stdout line, always — a background notification tail carries this verdict.
		fmt.Println(proofCheckVerdictLine(report, outPath))
	}
	if report.Status != "ok" && !dryRun {
		os.Exit(1)
	}
}

// proofCheckShouldRecordArtifact reports whether a finished run has earned the right to write the
// certifying runtime artifact under COMPOUND_HOME/artifacts/refs/.
//
// The `!ciOnly && !ciConsole` clauses are a CORRECTNESS fix, not tidiness.
// recordProofCheckRuntimeArtifact writes four hardcoded "adjacent Workbench action verified" lines
// regardless of which gates actually ran, and Workbench's adjacent-action consumption then treats
// those claims as satisfied. But `--ci` runs only the 8 fresh-clone-portable gates and
// `--ci-console` only the console subset — neither includes `workbench-projection` or
// `codex-pretooluse-native-gate`. So a green `--ci` was minting an artifact asserting that two
// gates it never executed had passed: a proof that certifies work nobody did, which is worse than
// no proof at all.
//
// Kept as a pure predicate so the rule can be falsified by a table test in microseconds instead of
// by running a multi-minute battery — the decision is separable from the write it authorizes.
func proofCheckLegacyRecoveryModeError(args []string) error {
	if !hasFlag(args, "--recover-legacy-retained") {
		return nil
	}
	if hasFlag(args, "--timings") ||
		proofCheckDevelopmentRequested(args) ||
		hasFlag(args, "--dry-run") ||
		hasFlag(args, "--ci") ||
		hasFlag(args, "--ci-console") {
		return fmt.Errorf("--recover-legacy-retained cannot be combined with any proof runner mode")
	}
	return nil
}

func proofCheckShouldRecordArtifact(dryRun bool, status string, ciOnly, ciConsole bool) bool {
	return !dryRun && status == "ok" && !ciOnly && !ciConsole
}

// proofCheckPublishDeadlineReport ends a bounded canonical proof run truthfully.
// The partial failed report and the exact deadline reason are retained in the
// authoritative --out evidence, the battery is never green, and the returned
// error keeps the deadline as its cause so the reservation closes as failed
// rather than verified.
func proofCheckPublishDeadlineReport(outPath string, report proofCheckReport, custody *proofCheckOutCustody, cause error) (proofCheckReport, error) {
	if report.SchemaVersion == "" {
		report.SchemaVersion = proofCheckSchemaVersion
		report.ProfileKind = work.ProofResourceProfileCanonicalFull
		report.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	report.Status = "failed"
	report.Warnings = append(report.Warnings, cause.Error())
	if outPath == "" || custody == nil {
		return report, cause
	}
	current, err := proofCheckCaptureOutPath(outPath)
	if err != nil {
		return report, errors.Join(cause, err)
	}
	defer current.close()
	expectedInfo, expectedData, expectedMode, existed := custody.targetInfo, custody.targetData, custody.targetMode, custody.existed
	if published := custody.publication; published != nil && published.info != nil {
		expectedInfo, expectedData, expectedMode, existed = published.info, published.data, published.mode, true
	}
	if !os.SameFile(current.parentInfo, custody.parentInfo) || current.existed != existed ||
		(existed && (!os.SameFile(current.targetInfo, expectedInfo) || current.targetMode != expectedMode)) ||
		!bytes.Equal(current.targetData, expectedData) {
		return report, errors.Join(cause, fmt.Errorf("deadline report output changed outside this proof"))
	}
	if err := proofCheckWriteReportFileWithCustody(outPath, report, current); err != nil {
		return report, errors.Join(cause, err)
	}
	return report, cause
}

type proofCheckOutPublication struct {
	info os.FileInfo
	data []byte
	mode os.FileMode
}

type proofCheckOutCustody struct {
	publication *proofCheckOutPublication
	path        string
	dir         string
	leaf        string
	root        *os.Root
	parentInfo  os.FileInfo
	targetInfo  os.FileInfo
	targetData  []byte
	targetMode  os.FileMode
	existed     bool
}

type proofCheckRootedCASPublication interface {
	State() home.RootedFileCASState
	Commit() error
	Rollback() error
}

var proofCheckBeginRootedFileCAS = func(
	root *os.Root,
	canonicalName,
	recoveryName string,
	expectedData []byte,
	expectedInfo os.FileInfo,
	replacementData []byte,
	replacementInfo os.FileInfo,
) (proofCheckRootedCASPublication, error) {
	return home.BeginRootedFileCAS(
		root,
		canonicalName,
		recoveryName,
		expectedData,
		expectedInfo,
		replacementData,
		replacementInfo,
	)
}

var proofCheckBeforeOutPublish = func(string) {}
var proofCheckOutTempCounter atomic.Uint64

// proofCheckCaptureOutPath takes non-mutating custody of the parent directory and
// regular leaf (or its absence). The retained root descriptor is the only leaf
// authority used by later probe, publication, and cleanup.
func proofCheckCaptureOutPath(path string) (proofCheckOutCustody, error) {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	leaf := filepath.Base(path)
	parentInfo, err := os.Stat(dir)
	if err != nil {
		return proofCheckOutCustody{}, err
	}
	if !parentInfo.IsDir() {
		return proofCheckOutCustody{}, fmt.Errorf("--out parent is not a directory: %s", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return proofCheckOutCustody{}, err
	}
	openedParent, err := proofCheckRootDirectoryInfo(root)
	if err != nil {
		_ = root.Close()
		return proofCheckOutCustody{}, err
	}
	if !os.SameFile(parentInfo, openedParent) {
		_ = root.Close()
		return proofCheckOutCustody{}, fmt.Errorf("--out parent changed while taking custody: %s", dir)
	}

	custody := proofCheckOutCustody{
		publication: &proofCheckOutPublication{},
		path:        path,
		dir:         dir,
		leaf:        leaf,
		root:        root,
		parentInfo:  openedParent,
		targetMode:  0o600,
	}
	targetInfo, err := root.Lstat(leaf)
	switch {
	case err == nil:
		if !targetInfo.Mode().IsRegular() {
			_ = root.Close()
			return proofCheckOutCustody{}, fmt.Errorf("--out target is not a regular file: %s", path)
		}
		if special := targetInfo.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
			_ = root.Close()
			return proofCheckOutCustody{}, fmt.Errorf(
				"--out target has unsupported special permission bits %s: %s",
				special,
				path,
			)
		}
		targetData, stableInfo, readErr := proofCheckReadRootPreimage(root, leaf, path, targetInfo)
		if readErr != nil {
			_ = root.Close()
			return proofCheckOutCustody{}, readErr
		}
		custody.targetInfo = stableInfo
		custody.targetData = targetData
		custody.targetMode = targetInfo.Mode().Perm()
		custody.existed = true
	case errors.Is(err, os.ErrNotExist):
		// A new evidence file remains private by default.
	default:
		_ = root.Close()
		return proofCheckOutCustody{}, err
	}
	return custody, nil
}

func proofCheckRootDirectoryInfo(root *os.Root) (os.FileInfo, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--out rooted parent is not a directory")
	}
	return info, nil
}

func proofCheckPrepareOutPath(path string) (proofCheckOutCustody, error) {
	custody, err := proofCheckCaptureOutPath(path)
	if err != nil {
		return proofCheckOutCustody{}, err
	}
	if err := custody.probeWriteCapability(); err != nil {
		_ = custody.close()
		return proofCheckOutCustody{}, err
	}
	return custody, nil
}

func proofCheckReadRootPreimage(
	root *os.Root,
	leaf,
	displayPath string,
	before os.FileInfo,
) ([]byte, os.FileInfo, error) {
	file, err := root.OpenFile(
		leaf,
		os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	after, err := root.Lstat(leaf)
	if err != nil {
		return nil, nil, err
	}
	if !proofCheckSameStableFileState(before, opened) ||
		!proofCheckSameStableFileState(opened, openedAfter) ||
		!proofCheckSameStableFileState(openedAfter, after) ||
		int64(len(data)) != openedAfter.Size() {
		return nil, nil, fmt.Errorf("--out target changed while capturing custody: %s", displayPath)
	}
	return data, openedAfter, nil
}

func proofCheckSameStableFileState(left, right os.FileInfo) bool {
	return left != nil &&
		right != nil &&
		left.Mode().IsRegular() &&
		right.Mode().IsRegular() &&
		os.SameFile(left, right) &&
		left.Mode() == right.Mode() &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func proofCheckReadPublishedReport(path string) ([]byte, error) {
	custody, err := proofCheckCaptureOutPath(path)
	if err != nil {
		return nil, err
	}
	defer custody.close()
	if !custody.existed {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), custody.targetData...), nil
}

// proofCheckValidateOutPath proves --out is a regular file (or absent) and its
// parent supports atomic sibling publication without touching prior evidence.
func proofCheckValidateOutPath(path string) error {
	custody, err := proofCheckPrepareOutPath(path)
	if err != nil {
		return err
	}
	return custody.close()
}

func (custody proofCheckOutCustody) revalidate(path string) error {
	path = filepath.Clean(path)
	if path != custody.path {
		return fmt.Errorf("--out custody path changed from %s to %s", custody.path, path)
	}
	if err := custody.revalidateVisibleParent(); err != nil {
		return err
	}
	openedParent, err := proofCheckRootDirectoryInfo(custody.root)
	if err != nil {
		return err
	}
	if !os.SameFile(custody.parentInfo, openedParent) {
		return fmt.Errorf("--out rooted parent identity changed before publication: %s", custody.dir)
	}
	targetInfo, err := custody.root.Lstat(custody.leaf)
	if !custody.existed {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("--out target appeared before publication: %s", path)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("--out target disappeared before publication: %s", path)
		}
		return err
	}
	return custody.validateExistingTargetInfo(targetInfo, path)
}

func (custody proofCheckOutCustody) validateExistingTargetInfo(info os.FileInfo, path string) error {
	if info == nil {
		return fmt.Errorf("--out target identity unavailable before publication: %s", path)
	}
	if special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
		return fmt.Errorf(
			"--out target has unsupported special permission bits %s: %s",
			special,
			path,
		)
	}
	if !info.Mode().IsRegular() ||
		custody.targetInfo == nil ||
		!os.SameFile(custody.targetInfo, info) ||
		info.Mode().Perm() != custody.targetMode {
		return fmt.Errorf("--out target identity changed before publication: %s", path)
	}
	return nil
}

func (custody proofCheckOutCustody) snapshotCurrentTarget(path string) (proofCheckOutCustody, error) {
	if !custody.existed {
		return proofCheckOutCustody{}, fmt.Errorf("--out target was absent when custody was captured: %s", path)
	}
	if err := custody.revalidate(path); err != nil {
		return proofCheckOutCustody{}, err
	}
	before, err := custody.root.Lstat(custody.leaf)
	if err != nil {
		return proofCheckOutCustody{}, err
	}
	if err := custody.validateExistingTargetInfo(before, path); err != nil {
		return proofCheckOutCustody{}, err
	}
	data, stableInfo, err := proofCheckReadRootPreimage(
		custody.root,
		custody.leaf,
		path,
		before,
	)
	if err != nil {
		return proofCheckOutCustody{}, err
	}
	if err := custody.validateExistingTargetInfo(stableInfo, path); err != nil {
		return proofCheckOutCustody{}, err
	}
	if err := custody.revalidate(path); err != nil {
		return proofCheckOutCustody{}, err
	}
	current := custody
	current.targetInfo = stableInfo
	current.targetData = append([]byte(nil), data...)
	current.targetMode = stableInfo.Mode().Perm()
	return current, nil
}

func (custody proofCheckOutCustody) revalidateVisibleParent() error {
	parentInfo, err := os.Stat(custody.dir)
	if err != nil {
		return err
	}
	if !parentInfo.IsDir() || !os.SameFile(custody.parentInfo, parentInfo) {
		return fmt.Errorf("--out parent identity changed before publication: %s", custody.dir)
	}
	return nil
}

func (custody proofCheckOutCustody) probeWriteCapability() error {
	if err := custody.revalidate(custody.path); err != nil {
		return err
	}
	probe, probeName, err := custody.createRootedTemp("preflight", 0o600)
	if err != nil {
		return err
	}
	if err := probe.Close(); err != nil {
		_ = custody.root.Remove(probeName)
		return err
	}
	if err := custody.root.Remove(probeName); err != nil {
		return err
	}
	return proofCheckSyncRootDirectory(custody.root)
}

func (custody proofCheckOutCustody) createRootedTemp(
	purpose string,
	mode os.FileMode,
) (*os.File, string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		counter := proofCheckOutTempCounter.Add(1)
		name := fmt.Sprintf(
			".%s.%d.%d.%s.tmp",
			custody.leaf,
			os.Getpid(),
			counter,
			purpose,
		)
		file, err := custody.root.OpenFile(
			name,
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			mode,
		)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, name, err
	}
	return nil, "", fmt.Errorf("--out could not allocate unique rooted sibling")
}

func (custody proofCheckOutCustody) close() error {
	if custody.root == nil {
		return nil
	}
	return custody.root.Close()
}

func proofCheckSyncRootDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) {
		return syncErr
	}
	return closeErr
}

// proofCheckWriteReportFile writes the full report as indented JSON to path — the same
// shape as --json stdout, regardless of whether --json was passed. Publication uses a
// synced sibling temp and atomic rename, so a failed preparation never truncates the
// previous complete report.
func proofCheckWriteReportFile(path string, report proofCheckReport) error {
	custody, err := proofCheckPrepareOutPath(path)
	if err != nil {
		return err
	}
	return proofCheckWriteReportFileWithCustody(path, report, custody)
}

func proofCheckWriteReportFileWithCustody(path string, report proofCheckReport, custody proofCheckOutCustody) error {
	defer custody.close()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return proofCheckWriteReportBytesWithCustody(path, data, custody)
}

func proofCheckWriteReportBytes(path string, data []byte) error {
	custody, err := proofCheckPrepareOutPath(path)
	if err != nil {
		return err
	}
	return proofCheckWriteReportBytesWithCustody(path, data, custody)
}

func proofCheckWriteReportBytesWithCustody(path string, data []byte, custody proofCheckOutCustody) error {
	defer custody.close()
	if err := custody.revalidate(path); err != nil {
		return err
	}
	temp, tempName, err := custody.createRootedTemp("publish", custody.targetMode)
	if err != nil {
		return err
	}
	var publication proofCheckRootedCASPublication
	defer func() {
		_ = temp.Close()
		if publication == nil || publication.State() == home.RootedFileCASNotPublished {
			_ = custody.root.Remove(tempName)
		}
	}()
	if err := temp.Chmod(custody.targetMode); err != nil {
		return err
	}
	for remaining := data; len(remaining) > 0; {
		n, writeErr := temp.Write(remaining)
		if writeErr != nil {
			return writeErr
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	replacementInfo, err := custody.root.Lstat(tempName)
	if err != nil {
		return err
	}
	proofCheckBeforeOutPublish(path)
	if err := custody.revalidate(path); err != nil {
		return err
	}
	publication, err = proofCheckBeginRootedFileCAS(
		custody.root,
		custody.leaf,
		tempName,
		custody.targetData,
		custody.targetInfo,
		data,
		replacementInfo,
	)
	if err != nil {
		return err
	}
	if err := publication.Commit(); err != nil {
		return err
	}
	if custody.publication != nil {
		*custody.publication = proofCheckOutPublication{replacementInfo, append([]byte(nil), data...), custody.targetMode}
	}
	return nil
}

// proofCheckVerdictLine renders the single final stdout line printed when --out is
// present, with passed, skipped, and failed gates counted separately.
func proofCheckVerdictLine(report proofCheckReport, path string) string {
	return work.ProofCheckVerdictLine(report, path)
}
func proofCheckReleasePendingVerdictLine(report proofCheckReport, path string) string {
	return work.ProofCheckReleasePendingVerdictLine(report, path)
}

func proofCheckPostReportErrorVerdict(report proofCheckReport, path string, cause error) (string, bool) {
	if report.SchemaVersion == "" || strings.TrimSpace(path) == "" {
		return "", false
	}
	body, err := proofCheckReadPublishedReport(path)
	if err != nil {
		return "", false
	}
	expected, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", false
	}
	expected = append(expected, '\n')
	if !bytes.Equal(body, expected) {
		return "", false
	}
	var releasePending *work.ProofResourceReleasePendingError
	if errors.As(cause, &releasePending) {
		return proofCheckReleasePendingVerdictLine(report, path), true
	}
	return work.ProofCheckPostReportFailureVerdict(report, path), true
}

func runProofCheckContext(ctx context.Context, repoPath string, dryRun bool, baseRef string, ciOnly bool, ciConsole bool, now time.Time) (proofCheckReport, error) {
	return runProofCheckWithRangeContext(ctx, repoPath, dryRun, strings.TrimSpace(baseRef), "", nil, ciOnly, ciConsole, now)
}

func runProofCheckSnapshotContext(
	ctx context.Context,
	repoPath string,
	dryRun bool,
	source gitrepository.Snapshot,
	ciOnly bool,
	ciConsole bool,
	now time.Time,
) (proofCheckReport, error) {
	return runProofCheckWithRangeContext(
		ctx,
		repoPath,
		dryRun,
		source.BaseSHA,
		source.HeadSHA,
		&source,
		ciOnly,
		ciConsole,
		now,
	)
}

func runProofCheckWithRangeContext(
	ctx context.Context,
	repoPath string,
	dryRun bool,
	baseRef string,
	headRef string,
	source *gitrepository.Snapshot,
	ciOnly bool,
	ciConsole bool,
	now time.Time,
) (proofCheckReport, error) {
	repo, err := resolveProofCheckRepo(repoPath)
	if err != nil {
		return proofCheckReport{}, err
	}
	baseRef = strings.TrimSpace(baseRef)
	var steps []proofCheckStep
	if source != nil {
		plan, planErr := work.CanonicalProofPlanForExecution(ctx, repo, *source, proofCheckSteps)
		if planErr != nil {
			return proofCheckReport{}, planErr
		}
		steps = plan.Steps
	} else {
		if err := work.RefuseRustInCompoundOnlyProofMode(repo, "non-certifying proof mode"); err != nil {
			return proofCheckReport{}, err
		}
		steps, err = proofCheckSteps(baseRef, headRef)
		if err != nil {
			return proofCheckReport{}, err
		}
	}
	if ciConsole {
		steps = ciConsoleSteps(steps)
	} else if ciOnly {
		steps = ciPortableSteps(steps)
	}
	if !dryRun {
		if err := work.ValidateConsoleProofPrerequisites(repo, steps); err != nil {
			return proofCheckReport{}, err
		}
		var cleanupRuntimeExecutable func()
		ctx, cleanupRuntimeExecutable, err = work.PinProofRuntimeExecutable(ctx, proofCheckRuntimeExecutable)
		if err != nil {
			return proofCheckReport{}, err
		}
		defer cleanupRuntimeExecutable()
	}
	report := proofCheckReport{
		SchemaVersion:  work.RemoteProofReportSchema(ctx),
		ProfileKind:    work.ProofResourceProfileCanonicalFull,
		Status:         "ok",
		GeneratedAt:    now.UTC().Format(time.RFC3339Nano),
		Repo:           repo,
		BaseRef:        baseRef,
		SourceSnapshot: source,
		DryRun:         dryRun,
		CIOnly:         ciOnly,
		CIConsole:      ciConsole,
		Steps:          make([]proofCheckStep, 0, len(steps)),
	}
	for _, step := range steps {
		if !dryRun && ctx.Err() != nil {
			report.Status = "failed"
			return report, context.Cause(ctx)
		}
		result := step
		if dryRun {
			result.Status = "planned"
		} else {
			result = work.RetainCanonicalConsumerEvidence(report.Steps, runProofCheckStepContext(ctx, repo, step, baseRef, headRef))
			if result.Status == "failed" {
				report.Status = "failed"
			}
		}
		if result.Status == "ok" || result.Status == "planned" {
			report.ProofRefs = append(report.ProofRefs, result.ProofRef)
		}
		report.Steps = append(report.Steps, result)
	}
	return report, work.ValidateRemoteProofSteps(ctx, report.Steps)
}

func recordProofCheckRuntimeArtifact(homePath string, report proofCheckReport) (proofCheckReport, error) {
	if report.ProfileKind == proofCheckProfileDevelopment || report.ProfileKind == work.ProofCheckProfilePresubmit {
		return report, fmt.Errorf("%s profile cannot record canonical installed-runtime proof", report.ProfileKind)
	}
	if strings.TrimSpace(homePath) == "" {
		return report, fmt.Errorf("COMPOUND_HOME is empty")
	}
	generatedAt, err := time.Parse(time.RFC3339, report.GeneratedAt)
	if err != nil {
		generatedAt = time.Now().UTC()
	}
	date := generatedAt.UTC().Format("20060102")
	refsDir := filepath.Join(homePath, "artifacts", "refs")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		return report, err
	}
	name := "proof-check-installed-runtime-" + date + ".md"
	ref := "artifact:artifacts/refs/" + name
	var b strings.Builder
	fmt.Fprintf(&b, "# Proof-check installed runtime proof %s\n\n", generatedAt.UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "`proof-check-installed-runtime` adjacent Workbench action verified installed runtime proof.\n")
	fmt.Fprintf(&b, "`architecture-doctor-strict` adjacent Workbench action verified through the proof-check architecture-doctor step.\n")
	fmt.Fprintf(&b, "`workbench-projection-refresh` adjacent Workbench action verified through proof-check workbench projection step.\n")
	fmt.Fprintf(&b, "`codex-pretooluse-native-gate-proof` adjacent Workbench action verified through proof-check's native allow-and-deny PreToolUse step.\n")
	fmt.Fprintf(&b, "Do not rerun these adjacent Workbench actions as the next move unless proof-check, strict architecture doctor, Workbench projection, the Codex native PreToolUse gate, or source state regresses.\n\n")
	fmt.Fprintf(&b, "- schema: %s\n", report.SchemaVersion)
	fmt.Fprintf(&b, "- status: %s\n", report.Status)
	fmt.Fprintf(&b, "- generated_at: %s\n", report.GeneratedAt)
	fmt.Fprintf(&b, "- repo: %s\n", report.Repo)
	fmt.Fprintf(&b, "- proof_refs: %s\n", strings.Join(report.ProofRefs, ","))
	for _, step := range report.Steps {
		fmt.Fprintf(&b, "- step: %s proof_ref=%s status=%s\n", step.Name, step.ProofRef, step.Status)
	}
	fmt.Fprintln(&b, "- publication_complete: true")
	if err := proofCheckWriteReportBytes(filepath.Join(refsDir, name), []byte(b.String())); err != nil {
		return report, err
	}
	report.ArtifactRef = ref
	return report, nil
}

func resolveProofCheckRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		repoPath = cwd
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--repo must be a directory, got %s", abs)
	}
	return abs, nil
}

func proofCheckSteps(baseRef string, headRefs ...string) ([]proofCheckStep, error) {
	exe := "compound"
	headRef := "HEAD"
	if len(headRefs) > 0 && strings.TrimSpace(headRefs[0]) != "" {
		headRef = strings.TrimSpace(headRefs[0])
	}
	diffRange := ""
	if strings.TrimSpace(baseRef) != "" {
		diffRange = strings.TrimSpace(baseRef) + "..." + headRef
	}
	redactCommand := []string{"redact", "--repo-visibility", "public", "--from-file", "<changed files>"}
	if diffRange != "" {
		redactCommand = []string{"redact", "--repo-visibility", "public", "<committed blobs changed in " + diffRange + ">"}
	}
	var goSourceArgs []string
	if len(headRefs) > 0 && strings.TrimSpace(headRefs[0]) != "" {
		goSourceArgs = []string{"--source-head", strings.TrimSpace(headRefs[0])}
	}
	return []proofCheckStep{
		// Run every package once with the unchanged -count=1 and -timeout=40m
		// budget. The driver isolates all home tests after other packages finish;
		// it never filters tests or hides an earlier failure behind a later pass.
		{Name: "go-test", Command: append([]string{"go", "run", "./scripts/testbattery", "general-release"}, goSourceArgs...), ProofRef: "go-test-count-1-general-release", CIPortable: true},
		{Name: "go-test-home", Command: append([]string{"go", "run", "./scripts/testbattery", "home"}, goSourceArgs...), ProofRef: "go-test-count-1-home", CIPortable: true},
		{Name: "credential-password-alignment-native", Command: append([]string{"go", "run", "./scripts/testbattery", "credential-password-alignment-native"}, goSourceArgs...), ProofRef: "credential-password-alignment-native-pg17-no-skips"},
		{Name: "go-vet", Command: append([]string{"go", "run", "./scripts/testbattery", "vet"}, goSourceArgs...), ProofRef: "go-vet-all", CIPortable: true},
		{Name: "go-build", Command: append([]string{"go", "run", "./scripts/testbattery", "build"}, goSourceArgs...), ProofRef: "go-build-all", CIPortable: true},
		{Name: "gofmt", Command: []string{"gofmt", "-l", "<tracked *.go>"}, ProofRef: "gofmt-check", CIPortable: true},
		{Name: "diff-check", Command: append([]string{"git", "diff", "--check"}, diffRangeArgs(diffRange)...), ProofRef: "diff-check", CIPortable: true},
		{Name: "console-tsc", Command: []string{"./node_modules/.bin/tsc", "--noEmit"}, Dir: "console", ProofRef: "console-tsc-noemit"},
		{Name: "console-smoke", Command: consoleSmokeCommand("scripts/smoke.mjs"), Dir: "console", ProofRef: "console-smoke"},
		{Name: "console-projection-timeout-smoke", Command: consoleBunSmokeCommand("scripts/projection-timeout-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-projection-timeout-smoke"},
		{Name: "console-mutation-gate-smoke", Command: consoleSmokeCommand("scripts/mutation-guard-smoke.mjs"), Dir: "console", ProofRef: "console-mutation-gate-smoke"},
		{Name: "console-instance-pin-smoke", Command: consoleSmokeCommand("scripts/instance-pin-smoke.mjs"), Dir: "console", ProofRef: "console-instance-pin-smoke"},
		{Name: "console-continue-button-smoke", Command: consoleBunSmokeCommand("scripts/continue-button-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-continue-button-smoke"},
		{Name: "console-work-action-row-smoke", Command: consoleBunSmokeCommand("scripts/work-action-row-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-work-action-row-smoke"},
		{Name: "console-mutation-readiness-consumers-smoke", Command: consoleSmokeCommand("scripts/mutation-readiness-consumers-smoke.mjs"), Dir: "console", ProofRef: "console-mutation-readiness-consumers-smoke"},
		{Name: "console-private-fallback-smoke", Command: consoleSmokeCommand("scripts/private-fallback-smoke.mjs"), Dir: "console", ProofRef: "console-private-fallback-smoke"},
		{Name: "console-private-runtime-guard-smoke", Command: consoleSmokeCommand("scripts/private-runtime-guard-smoke.mjs"), Dir: "console", ProofRef: "console-private-runtime-guard-smoke"},
		{Name: "console-move-type-drift-smoke", Command: consoleSmokeCommand("scripts/move-type-drift-smoke.mjs"), Dir: "console", ProofRef: "console-move-type-drift-smoke"},
		{Name: "console-ai-boundary-smoke", Command: consoleSmokeCommand("scripts/ai-boundary-smoke.mjs"), Dir: "console", ProofRef: "console-ai-boundary-smoke"},
		{Name: "console-client-bundle-guard-smoke", Command: consoleSmokeCommand("scripts/client-bundle-guard-smoke.mjs"), Dir: "console", ProofRef: "console-client-bundle-guard-smoke"},
		{Name: "console-work-trace-smoke", Command: consoleBunSmokeCommand("scripts/work-trace-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-work-trace-smoke"},
		{Name: "console-lineage-smoke", Command: consoleBunSmokeCommand("scripts/lineage-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-lineage-smoke"},
		{Name: "console-generic-actions-smoke", Command: consoleBunSmokeCommand("scripts/generic-actions-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-generic-actions-smoke"},
		{Name: "console-view-registry-smoke", Command: consoleBunSmokeCommand("scripts/view-registry-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-view-registry-smoke"},
		{Name: "console-decide-brief-smoke", Command: consoleBunSmokeCommand("scripts/decide-model-brief-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-decide-brief-smoke"},
		{Name: "console-decision-execute-smoke", Command: consoleBunSmokeCommand("scripts/decision-execute-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-decision-execute-smoke"},
		{Name: "console-standing-law-smoke", Command: consoleBunSmokeCommand("scripts/standing-law-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-standing-law-smoke"},
		{Name: "console-memory-data-smoke", Command: consoleBunSmokeCommand("scripts/memory-data-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-memory-data-smoke"},
		{Name: "console-memory-read-honesty-smoke", Command: consoleBunSmokeCommand("scripts/memory-read-honesty-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-memory-read-honesty-smoke"},
		{Name: "console-evidence-card-smoke", Command: consoleBunSmokeCommand("scripts/evidence-card-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-evidence-card-smoke"},
		// attention-consistency-smoke imports console TSX (DecideList/Headline) that the node
		// strip-types runner cannot resolve, so it runs under bun like its TSX-importing siblings
		// (it re-enters itself via bun when invoked under node — this direct bun invocation is the
		// primary path; a bun-absent host must SKIP the gate, not ENOENT through the re-exec).
		{Name: "console-attention-consistency-smoke", Command: consoleBunSmokeCommand("scripts/attention-consistency-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-attention-consistency-smoke"},
		{Name: "console-work-language-smoke", Command: consoleBunSmokeCommand("scripts/work-language-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-work-language-smoke"},
		{Name: "console-work-legibility-smoke", Command: consoleBunSmokeCommand("scripts/work-legibility-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-work-legibility-smoke"},
		{Name: "console-ask-triage-smoke", Command: consoleBunSmokeCommand("scripts/ask-triage-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-ask-triage-smoke"},
		{Name: "console-representation-falsifiers-smoke", Command: consoleBunSmokeCommand("scripts/representation-falsifiers-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-representation-falsifiers-smoke"},
		{Name: "console-representation-cluster-guard-smoke", Command: consoleSmokeCommand("scripts/representation-cluster-guard-smoke.mjs"), Dir: "console", ProofRef: "console-representation-cluster-guard-smoke"},
		// agents-consistency and projection-doors are pure node (no Bun-only API, no
		// extensionless/TSX import). The home-owning agents gate injects its candidate binary as
		// COMPOUND_BIN, while projection-doors still runs its literal fallback from PATH.
		{Name: "console-agents-consistency-smoke", Command: consoleSmokeCommand("scripts/agents-consistency-smoke.mjs"), Dir: "console", ProofRef: "console-agents-consistency-smoke"},
		{Name: "console-projection-doors-smoke", Command: consoleSmokeCommand("scripts/projection-doors-smoke.mjs"), Dir: "console", SkipIfMissing: "compound", ProofRef: "console-projection-doors-smoke"},
		// bun smokes: these import extensionless TS the node runner cannot resolve, so they run under bun.
		{Name: "console-stream-tail-smoke", Command: consoleBunSmokeCommand("scripts/stream-tail-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-stream-tail-smoke"},
		{Name: "console-decide-decision-feedback-smoke", Command: consoleBunSmokeCommand("scripts/decide-decision-feedback-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-decide-decision-feedback-smoke"},
		{Name: "console-projection-degradation-smoke", Command: consoleBunSmokeCommand("scripts/projection-degradation-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-projection-degradation-smoke"},
		// stream-door-smoke imports the API route via the @/ alias (extensionless TS), so it needs
		// bun; it resolves its own COMPOUND_BIN (env override, else a self-built temp binary with
		// the `stream` verb) and its own throwaway COMPOUND_HOME, so no home/binary setup is needed
		// here beyond the bun runtime gate.
		{Name: "console-stream-door-smoke", Command: consoleBunSmokeCommand("scripts/stream-door-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-stream-door-smoke"},
		// traffic-board-smoke executes the real-time board's pure-TS data-plane + live-fold
		// selftests (traffic / useLiveFleet / run-attempts). It imports extensionless @/ TS, so it
		// runs under bun like the other selftest-runner smokes.
		{Name: "console-traffic-board-smoke", Command: consoleBunSmokeCommand("scripts/traffic-board-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-traffic-board-smoke"},
		{Name: "console-ads-smoke", Command: consoleBunSmokeCommand("scripts/ads-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-ads-smoke"},
		// delegated-sessions-smoke executes the delegated supervised-session board's
		// pure-TS normalize (privacy whitelist) + render falsifiers. Extensionless @/
		// TS imports, so it runs under bun like the other selftest-runner smokes.
		{Name: "console-delegated-sessions-smoke", Command: consoleBunSmokeCommand("scripts/delegated-sessions-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-delegated-sessions-smoke"},
		// coordination-smoke executes the Me / Others / Waiting-on data-plane selftests
		// (the pure builder over `compound coordination --json`). Same bun gate as the
		// other extensionless @/ selftest-runner smokes.
		{Name: "console-coordination-smoke", Command: consoleBunSmokeCommand("scripts/coordination-smoke.mjs"), Dir: "console", SkipIfMissing: "bun", ProofRef: "console-coordination-smoke"},
		{Name: "bun-tools-test", Command: []string{"bun", "test", "<tools/*.test.ts>"}, SkipIfMissing: "bun", ProofRef: "bun-tools-test"},
		{Name: "architecture-doctor", Command: []string{exe, "architecture", "doctor", "--strict"}, ProofRef: "architecture-doctor-strict", CIPortable: true},
		{Name: "profiles-check", Command: []string{exe, "profiles", "check", "--installed-only"}, ProofRef: "profiles-check-installed-only"},
		{Name: "workbench-projection", Command: []string{exe, "workbench", "--json"}, ProofRef: "workbench-projection-refresh"},
		{Name: "codex-pretooluse-native-gate", Command: []string{exe, "hook", "pre-tool-use", "--live", "--harness", "codex", "--enforce"}, ProofRef: "codex-pretooluse-native-gate-proof"},
		// --json because the gate PARSES the report and requires named hops to be "ok";
		// see proof_check_memory_gate.go for why a bare exit code is a vacuous pass here.
		{Name: "memory-doctor", Command: []string{exe, "memory", "doctor", "--json"}, ProofRef: "memory-doctor", CIPortable: true},
		{Name: "redact-public", Command: redactCommand, ProofRef: "redact-public-clean"},
	}, nil
}

// ciPortableSteps returns only the gates that run green in a fresh-clone CI checkout —
// no external non-Go binary (redact), no installed-binary dependency (profiles-check),
// no COMPOUND_HOME body or bun/node console runner. `proof-check --ci` runs exactly these
// so the CI gate set is single-sourced from proofCheckSteps and cannot drift from a
// separately-maintained workflow list.
func diffRangeArgs(diffRange string) []string {
	if strings.TrimSpace(diffRange) == "" {
		return nil
	}
	return []string{diffRange}
}

func ciPortableSteps(steps []proofCheckStep) []proofCheckStep {
	out := make([]proofCheckStep, 0, len(steps))
	for _, s := range steps {
		if s.CIPortable {
			out = append(out, s)
		}
	}
	return out
}

// ciConsoleSteps returns the console/TypeScript gate subset — every step rooted in console/. These
// need bun + console/node_modules (and a compound binary on PATH for the smokes that subprocess
// it), so they cannot run in the bare-clone `--ci` set. `proof-check --ci-console` runs exactly
// these, single-sourced from proofCheckSteps, so the CI console job's gate set cannot drift from a
// hand-maintained workflow list.
//
// bun-tools-test (tools/*.test.ts) is intentionally excluded: those App Store Connect / analytics
// tool tests are env-coupled — asc_preflight validates a macOS launchd plist and scheduler state,
// so it passes on macOS and reds a Linux CI runner. They stay local-battery-only.
func ciConsoleSteps(steps []proofCheckStep) []proofCheckStep {
	out := make([]proofCheckStep, 0, len(steps))
	for _, s := range steps {
		if s.Dir == "console" {
			out = append(out, s)
		}
	}
	return out
}

func consoleSmokeCommand(script string) []string {
	return []string{"node", "--experimental-strip-types", script}
}

func consoleBunSmokeCommand(script string) []string {
	return []string{"bun", script}
}

// proofCheckHomeOwningGates are the gates that CREATE the COMPOUND_HOME they read, instead of
// inheriting the operator's. Declared as a set rather than buried in the dispatch chain
// because membership is the load-bearing fact: a gate that silently leaves this map goes back
// to reading a live home, which makes it slower every day, non-deterministic, and — since an
// empty projection is not a failing one — capable of passing while asserting nothing.
//
// Each runner owns its own fixture and its own assertions; see the per-gate files. The
// dispatch above stays an if-chain for the gates whose signatures differ (redact needs a base
// ref, gofmt and bun-tools take only the repo). Every home-owning runner receives the
// certifying run's context: the process-group cancellation and inherited land-lease barrier
// are part of the gate contract, not optional wiring each fixture may replace.
var proofCheckHomeOwningGates = map[string]func(context.Context, string, proofCheckStep) ([]byte, error){
	"codex-pretooluse-native-gate":        runProofCheckCodexPreToolUseNativeGateContext,
	"memory-doctor":                       runProofCheckMemoryDoctorGateContext,
	"workbench-projection":                runProofCheckWorkbenchGateContext,
	"console-attention-consistency-smoke": runProofCheckConsoleAttentionGateContext,
	"console-agents-consistency-smoke":    runProofCheckConsoleAgentsGateContext,
}

func runProofCheckStep(repo string, step proofCheckStep, baseRef string) proofCheckStep {
	return runProofCheckStepContext(context.Background(), repo, step, baseRef)
}

func runProofCheckStepContext(ctx context.Context, repo string, step proofCheckStep, baseRef string, headRefs ...string) proofCheckStep {
	start := time.Now()
	result := step
	result.Status = "ok"
	var output []byte
	var err error
	if step.SkipIfMissing != "" {
		if _, lookErr := exec.LookPath(step.SkipIfMissing); lookErr != nil {
			result.Status = "skipped"
			result.SkipReason = fmt.Sprintf("required executable %s not found", step.SkipIfMissing)
			result.OutputTail = result.SkipReason + "; skipped " + step.Name + "\n"
			result.DurationMS = time.Since(start).Milliseconds()
			return result
		}
	}
	// Console battery fast-fail: detect (a) absent console/node_modules and (b) an inherited
	// HTTPS_PROXY/https_proxy pointing at a loopback (model-gateway signature) BEFORE invoking
	// bun, so the bun auto-install path never gets a chance to block on a TCP CONNECT that
	// will never complete. Recorded signature on 2026-08-14: a 64-minute hang followed by an
	// identical-signature cascade on the following day. The named reason surface is part of
	// the contract — stable strings the discriminator (and on-call) can match on.
	if reason := work.ConsoleBatteryFastFailReason(repo, step); reason != "" {
		failOutput := fmt.Sprintf("proof-check console battery fast-fail: %s — step=%s; documented in REPORT (2026-08-14): unset HTTPS_PROXY/https_proxy OR populate console/node_modules before re-running\n", reason, step.Name)
		result.Status = "failed"
		result.OutputTail = work.ProofGateOutputTail([]byte(failOutput), 4000)
		result.FailureFacts = work.CanonicalProofFailureFacts(repo, step.Name, []byte(failOutput), errors.New(reason))
		result.ExitCode = 1
		work.AttachFailedGateOutput(&result, ctx, step.Name, []byte(failOutput), false)
		result.DurationMS = time.Since(start).Milliseconds()
		return result
	}
	if step.Name == "go-test" && work.RemoteProofRequired(ctx) {
		output, result.RemoteEvidence, err = work.RunRemoteProofGate(ctx, repo, step)
	} else if strings.HasPrefix(step.Name, "consumer:") {
		output, result.ConsumerEvidence, err = work.RunCanonicalConsumer(ctx, repo, step, headRefs, proofCheckCommandContext, proofCheckRuntimeExecutable)
	} else if step.Name == work.HomeFixtureGateName {
		output, result.HomeFixtureEvidence, err = work.RunHomeFixtureGate(ctx, repo, step, proofCheckCommandEnv(ctx, repo), proofCheckCommandContext)
	} else if step.Name == "redact-public" {
		output, result.RedactionEvidence, result.PrivateSourceEvidence, err = work.RunSourceRedaction(
			ctx, repo, step, baseRef, headRefs, proofCheckRedactInputsAtSnapshotContext,
			proofCheckCommandContext, runProofCheckRedactContext,
		)
	} else if step.Name == "gofmt" {
		output, err = runProofCheckGofmtContext(ctx, repo)
	} else if step.Name == "bun-tools-test" {
		output, err = runProofCheckBunTools(ctx, repo)
	} else if runner, owns := proofCheckHomeOwningGates[step.Name]; owns {
		output, err = runner(ctx, repo, step)
	} else {
		executable, executableErr := work.ProofRuntimeExecutable(ctx, step.Command[0], proofCheckRuntimeExecutable)
		if executableErr != nil {
			result.Status = "failed"
			result.OutputTail = executableErr.Error()
			result.FailureFacts = work.CanonicalProofFailureFacts(repo, step.Name, []byte(executableErr.Error()), executableErr)
			result.ExitCode = 1
			return result
		}
		cmd := proofCheckCommandContext(ctx, executable, step.Command[1:]...)
		cmd.Dir = repo
		if step.Dir != "" {
			cmd.Dir = filepath.Join(repo, step.Dir)
		}
		cmd.Env = os.Environ()
		if proofCheckUsesGo(step) {
			cmd.Env = proofCheckCommandEnv(ctx, repo)
		}
		output, err = cmd.CombinedOutput()
	}
	result.DurationMS = time.Since(start).Milliseconds()
	result.OutputTail = work.ProofGateOutputTail(output, 4000)
	if err != nil {
		result.Status = "failed"
		if strings.TrimSpace(result.OutputTail) == "" {
			result.OutputTail = err.Error()
		}
		result.FailureFacts = work.CanonicalProofFailureFacts(repo, step.Name, output, err)
		result.ExitCode = work.ExitCodeFromRunErr(err)
		work.AttachFailedGateOutput(&result, ctx, step.Name, output, true)
	}
	return result
}

func runProofCheckCodexPreToolUseNativeGate(repo string, step proofCheckStep) ([]byte, error) {
	return runProofCheckCodexPreToolUseNativeGateContext(context.Background(), repo, step)
}

func runProofCheckCodexPreToolUseNativeGateContext(ctx context.Context, repo string, step proofCheckStep) ([]byte, error) {
	tmpHome, err := os.MkdirTemp("", "compound-proofcheck-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpHome)
	tmpRepo, err := os.MkdirTemp("", "compound-proofcheck-repo-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpRepo)
	if err := home.Init(tmpHome, "proof-check", time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("initialize proof-check home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpHome, "config", "policy.yaml"), []byte("placement_guard_mode: block\n"), 0o644); err != nil {
		return nil, fmt.Errorf("enable placement block mode: %w", err)
	}
	// Augment the synthetic fixture with a block-confidence governed root for THIS home so the
	// open-world case can be proven natively (the synthesized default root is warn, which records
	// but does not hold). The allow-set is the home's own typed habitat dirs.
	fixtureData, err := os.ReadFile(filepath.Join(repo, "fixtures", "policy", "placement-rules.json"))
	if err != nil {
		return nil, fmt.Errorf("read placement fixture: %w", err)
	}
	var rules policy.PlacementRules
	if err := json.Unmarshal(fixtureData, &rules); err != nil {
		return nil, fmt.Errorf("parse placement fixture: %w", err)
	}
	placementDirs := home.HabitatPlacementDirs()
	allowGlobs := make([]string, 0, len(placementDirs))
	for _, d := range placementDirs {
		allowGlobs = append(allowGlobs, filepath.Join(tmpHome, d, "**"))
	}
	rules.GovernedRoots = append(rules.GovernedRoots, policy.GovernedRoot{
		ID:         "compound-home",
		RootGlobs:  []string{filepath.Join(tmpHome, "**")},
		AllowGlobs: allowGlobs,
		Confidence: "block",
		Redirect:   "compound placement target --kind operational-finding",
	})
	augmented, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("marshal augmented placement rules: %w", err)
	}
	rulesSrc := filepath.Join(tmpRepo, "proofcheck-placement-rules.json")
	if err := os.WriteFile(rulesSrc, augmented, 0o644); err != nil {
		return nil, fmt.Errorf("write augmented placement rules: %w", err)
	}
	if _, err := policy.ImportPlacementRules(tmpHome, rulesSrc, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("import synthetic placement rules: %w", err)
	}

	runPatch := func(sessionID, patch string) (claudeLiveHookResponse, []byte, int, error) {
		input := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":%q,"cwd":%q,"tool_name":"apply_patch","tool_input":{"command":%q}}`, sessionID, tmpRepo, patch)
		executable, err := work.ProofRuntimeExecutable(ctx, step.Command[0], proofCheckRuntimeExecutable)
		if err != nil {
			return claudeLiveHookResponse{}, nil, 0, err
		}
		cmd := proofCheckCommandContext(ctx, executable, step.Command[1:]...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "COMPOUND_HOME="+tmpHome, "HOME="+tmpRepo, "PLACEMENT_GUARD_MODE=block")
		cmd.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		evidence := bytes.Join([][]byte{stdout, stderr.Bytes()}, nil)
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || (exitErr.ExitCode() != 3 && exitErr.ExitCode() != 4) {
				return claudeLiveHookResponse{}, evidence, 0, fmt.Errorf("Codex native PreToolUse command: %w", err)
			}
			exitCode = exitErr.ExitCode()
		}
		var response claudeLiveHookResponse
		if err := json.Unmarshal(stdout, &response); err != nil {
			return claudeLiveHookResponse{}, evidence, exitCode, fmt.Errorf("parse Codex native PreToolUse response: %w", err)
		}
		return response, evidence, exitCode, nil
	}

	allowedPatch := "*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** End Patch"
	allowed, allowedOut, allowedExit, err := runPatch("sess_codex_native_allow", allowedPatch)
	if err != nil {
		return allowedOut, err
	}
	if allowedExit != 0 {
		return allowedOut, fmt.Errorf("valid Codex apply_patch exited %d, want 0", allowedExit)
	}
	if decision := allowed.HookSpecificOutput.PermissionDecision; decision != "" && decision != "allow" {
		return allowedOut, fmt.Errorf("valid Codex apply_patch decision = %q, want allow", decision)
	}

	blockedPatch := "*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** Add File: DOC-0001.md\n+misplaced canon\n*** End Patch"
	blocked, blockedOut, blockedExit, err := runPatch("sess_codex_native_block", blockedPatch)
	combined := bytes.Join([][]byte{[]byte("allow:\n"), allowedOut, []byte("\nblock:\n"), blockedOut}, nil)
	if err != nil {
		return combined, err
	}
	if blockedExit != 3 {
		return combined, fmt.Errorf("misplaced multi-target Codex apply_patch exited %d, want hold exit 3", blockedExit)
	}
	if blocked.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(strings.ToLower(blocked.HookSpecificOutput.PermissionDecisionReason), "placement") {
		return combined, fmt.Errorf("misplaced multi-target Codex apply_patch did not return native placement deny")
	}

	// Open-world case: a novel filename in an UNCLAIMED location under COMPOUND_HOME. It matches
	// no owner deny/canon rule, so pre-fix it sailed through; the governed-root check must now hold
	// it and hand back the redirect.
	unownedTarget := filepath.Join(tmpHome, "private", "stray-scaffold.md")
	unownedPatch := "*** Begin Patch\n*** Add File: " + unownedTarget + "\n+stray agent scaffolding\n*** End Patch"
	unowned, unownedOut, unownedExit, err := runPatch("sess_codex_native_unowned", unownedPatch)
	combined = bytes.Join([][]byte{combined, []byte("\nunowned:\n"), unownedOut}, nil)
	if err != nil {
		return combined, err
	}
	if unownedExit != 3 {
		return combined, fmt.Errorf("unowned write under COMPOUND_HOME exited %d, want hold exit 3", unownedExit)
	}
	unownedReason := strings.ToLower(unowned.HookSpecificOutput.PermissionDecisionReason)
	if unowned.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(unownedReason, "unowned-under-governed-root") {
		return combined, fmt.Errorf("unowned write under COMPOUND_HOME governed root did not return native placement deny")
	}
	return combined, nil
}

// proofCheckPruneGoCaches bounds the per-worktree proof Go caches once per
// run, here at the entry point: proofCheckCommandEnv runs for every command.
// Pruning never fails the proof.
func proofCheckPruneGoCaches(args []string) {
	if hasFlag(args, "--dry-run") || hasFlag(args, "--recover-legacy-retained") {
		return
	}
	repo, err := resolveProofCheckRepo(flagValue(args, "--repo"))
	if err != nil {
		return
	}
	report, ok, err := work.PruneProofGoCachesForRun(repo, time.Now())
	if !ok {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "proof-gocache: prune skipped:", err)
		return
	}
	fmt.Fprintln(os.Stderr, report.Summary())
	if len(report.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "proof-gocache: %d error(s), first: %s\n", len(report.Errors), report.Errors[0])
	}
}

type proofCheckCacheOriginKey struct{}

// Cache identity follows the caller; source execution stays in the sealed checkout.
func proofCheckCommandEnv(ctx context.Context, repo string) []string {
	if origin, ok := ctx.Value(proofCheckCacheOriginKey{}).(string); ok {
		repo = origin
	}
	env := append(os.Environ(), "GOTOOLCHAIN=local")
	if cache, ok := work.ProofGoCache(repo); ok {
		env = append(env, "GOCACHE="+cache)
	}
	return env
}

func runProofCheckBunTools(ctx context.Context, repo string) ([]byte, error) {
	matches, err := filepath.Glob(filepath.Join(repo, "tools", "*.test.ts"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no tools/*.test.ts files found")
	}
	args := []string{"test"}
	for _, match := range matches {
		rel, err := filepath.Rel(repo, match)
		if err != nil {
			return nil, err
		}
		args = append(args, "."+string(filepath.Separator)+rel)
	}
	cmd := proofCheckCommandContext(ctx, "bun", args...)
	cmd.Dir = repo
	cmd.Env = proofCheckCommandEnv(ctx, repo)
	return cmd.CombinedOutput()
}

func runProofCheckGofmt(repo string) ([]byte, error) {
	return runProofCheckGofmtContext(context.Background(), repo)
}

func runProofCheckGofmtContext(ctx context.Context, repo string) ([]byte, error) {
	files, err := proofCheckGitNULListContext(ctx, repo, "ls-files", "*.go", "-z")
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return []byte("no tracked Go files\n"), nil
	}
	args := append([]string{"-l"}, files...)
	cmd := proofCheckCommandContext(ctx, "gofmt", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, err
	}
	if strings.TrimSpace(string(output)) != "" {
		return output, fmt.Errorf("gofmt reported unformatted tracked Go files")
	}
	return []byte("all tracked Go files gofmt-clean\n"), nil
}

func proofCheckUsesGo(step proofCheckStep) bool {
	return len(step.Command) > 0 && step.Command[0] == "go"
}

var proofCheckRuntimeExecutable = work.ResolveProofRuntimeCommand

func runProofCheckRedact(repo, baseRef string) ([]byte, error) {
	return runProofCheckRedactContext(context.Background(), repo, baseRef)
}

// Canonical source scanning permits complete generated resources up to 8 MiB.
// The scanner still fails closed above this bounded cap; input is never truncated.
func runProofCheckRedactContext(ctx context.Context, repo, baseRef string, headRefs ...string) ([]byte, error) {
	if len(headRefs) > 0 && strings.TrimSpace(headRefs[0]) != "" {
		source := gitrepository.Snapshot{
			BaseSHA: strings.TrimSpace(baseRef),
			HeadSHA: strings.TrimSpace(headRefs[0]),
		}
		inputs, err := proofCheckRedactInputsAtSnapshotContext(ctx, repo, source)
		if err != nil {
			return nil, err
		}
		var combined bytes.Buffer
		for _, input := range inputs {
			cmd := proofCheckCommandContext(ctx, "redact", "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", input.Path)
			cmd.Dir = repo
			cmd.Stdin = bytes.NewReader(input.Body)
			out, err := cmd.CombinedOutput()
			combined.WriteString(input.Path + ":\n")
			combined.Write(out)
			if err != nil {
				return combined.Bytes(), err
			}
		}
		if len(inputs) == 0 {
			combined.WriteString("redact scan skipped: no files changed in " + source.BaseSHA + "..." + source.HeadSHA + "\n")
		}
		return combined.Bytes(), nil
	}
	files, err := proofCheckChangedFilesContext(ctx, repo, baseRef)
	if err != nil {
		return nil, err
	}
	var combined bytes.Buffer
	for _, file := range files {
		cmd := proofCheckCommandContext(ctx, "redact", "--repo-visibility", "public", "--max-bytes", "8388608", "--from-file", file)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		combined.Write(out)
		if err != nil {
			return combined.Bytes(), err
		}
	}
	if len(files) == 0 {
		if strings.TrimSpace(baseRef) != "" {
			headRef := "HEAD"
			if len(headRefs) > 0 && strings.TrimSpace(headRefs[0]) != "" {
				headRef = strings.TrimSpace(headRefs[0])
			}
			combined.WriteString("redact scan skipped: no files changed in " + strings.TrimSpace(baseRef) + "..." + headRef + " and no dirty files\n")
		} else {
			combined.WriteString("redact scan skipped: no changed files\n")
		}
	}
	return combined.Bytes(), nil
}

func proofCheckChangedFiles(repo, baseRef string) ([]string, error) {
	return proofCheckChangedFilesContext(context.Background(), repo, baseRef)
}

func proofCheckChangedFilesContext(ctx context.Context, repo, baseRef string) ([]string, error) {
	// Redact scans only bodies that still exist. With rename detection disabled,
	// a rename is represented as delete+add: ACM retains the new body while the
	// deleted source is excluded.
	paths, err := proofCheckChangedPathsContext(ctx, repo, baseRef, "ACM")
	if err != nil {
		return nil, err
	}
	files := paths[:0]
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(path))); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat changed file %q: %w", path, err)
		}
		files = append(files, path)
	}
	return files, nil
}

func proofCheckChangedFilesAtSnapshot(repo string, source gitrepository.Snapshot) ([]string, error) {
	return gitrepository.CommittedContentPaths(context.Background(), repo, source.BaseSHA, source.HeadSHA)
}

type proofCheckRedactInput = work.PublicRedactionInput

func proofCheckRedactInputsAtSnapshot(repo string, source gitrepository.Snapshot) ([]proofCheckRedactInput, error) {
	return proofCheckRedactInputsAtSnapshotContext(context.Background(), repo, source)
}

func proofCheckRedactInputsAtSnapshotContext(
	ctx context.Context,
	repo string,
	source gitrepository.Snapshot,
) ([]proofCheckRedactInput, error) {
	// The canonical scanner reads exact committed bodies through the shared
	// repository owner, so a mutated caller file cannot change what it scanned.
	paths, err := gitrepository.CommittedContentPaths(ctx, repo, source.BaseSHA, source.HeadSHA)
	if err != nil {
		return nil, err
	}
	inputs := make([]proofCheckRedactInput, 0, len(paths))
	for _, path := range paths {
		body, err := gitrepository.CommittedFileBody(ctx, repo, source.HeadSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read committed redact input %q at %s: %w", path, source.HeadSHA, err)
		}
		inputs = append(inputs, proofCheckRedactInput{Path: path, Body: body})
	}
	return inputs, nil
}

func proofCheckChangedPaths(repo, baseRef, diffFilter string) ([]string, error) {
	return proofCheckChangedPathsContext(context.Background(), repo, baseRef, diffFilter)
}

func proofCheckChangedPathsContext(ctx context.Context, repo, baseRef, diffFilter string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(paths []string) {
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			files = append(files, path)
		}
	}
	if baseRef = strings.TrimSpace(baseRef); baseRef != "" {
		paths, err := proofCheckGitNULListContext(ctx, repo, "diff", "--no-renames", "--name-only", "--diff-filter="+diffFilter, "-z", baseRef+"...HEAD")
		if err != nil {
			return nil, err
		}
		add(paths)
	}
	for _, args := range [][]string{
		{"diff", "--no-renames", "--name-only", "--diff-filter=" + diffFilter, "-z"},
		{"diff", "--cached", "--no-renames", "--name-only", "--diff-filter=" + diffFilter, "-z"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		paths, err := proofCheckGitNULListContext(ctx, repo, args...)
		if err != nil {
			return nil, err
		}
		add(paths)
	}
	return files, nil
}

func proofCheckGitNULList(repo string, args ...string) ([]string, error) {
	return proofCheckGitNULListContext(context.Background(), repo, args...)
}

func proofCheckGitNULListContext(ctx context.Context, repo string, args ...string) ([]string, error) {
	cmd := proofCheckCommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := bytes.Split(out, []byte{0})
	paths := make([]string, 0, len(raw))
	for _, part := range raw {
		if len(part) == 0 {
			continue
		}
		paths = append(paths, string(part))
	}
	return paths, nil
}

func proofCheckCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if barrier, ok := ctx.Value(proofCheckExecutionBarrierContextKey{}).(*landlease.ExecutionBarrier); ok {
		cmd.ExtraFiles = append(cmd.ExtraFiles, barrier.InheritableFiles()...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type proofCheckExecutionBarrierContextKey struct{}

func proofCheckContextWithExecutionBarrier(ctx context.Context, barrier *landlease.ExecutionBarrier) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if barrier == nil {
		return ctx
	}
	return context.WithValue(ctx, proofCheckExecutionBarrierContextKey{}, barrier)
}

func printProofCheck(report proofCheckReport) {
	if report.ProfileKind == proofCheckProfileDevelopment || report.ProfileKind == work.ProofCheckProfilePresubmit {
		fmt.Printf("# proof-check %s (non-certifying)\n", report.ProfileKind)
		fmt.Printf("- profile_kind: %s\n", report.ProfileKind)
	} else {
		fmt.Printf("# proof-check\n")
	}
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- repo: %s\n", report.Repo)
	fmt.Printf("- dry_run: %t\n", report.DryRun)
	for _, step := range report.Steps {
		fmt.Printf("- step: %s status=%s proof=%s command=%s\n", step.Name, step.Status, step.ProofRef, strings.Join(step.Command, " "))
		if step.Status == "failed" && strings.TrimSpace(step.OutputTail) != "" {
			fmt.Println("  output_tail:")
			for _, line := range strings.Split(step.OutputTail, "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		if step.SkipReason != "" {
			fmt.Printf("  skip_reason: %s\n", step.SkipReason)
		}
		// A red CI run prints this human report, not --json. The bounded tail can lose an early
		// failing package, so also print the extracted FAIL/panic/timeout lines.
		if step.Status == "failed" && len(step.FailureLines) > 0 {
			fmt.Printf("  failure_lines:\n    %s\n", strings.Join(step.FailureLines, "\n    "))
		}
	}
	if len(report.ProofRefs) > 0 {
		fmt.Printf("- proof_refs: %s\n", strings.Join(report.ProofRefs, ","))
	}
	if report.ArtifactRef != "" {
		fmt.Printf("- artifact_ref: %s\n", report.ArtifactRef)
	}
	for _, warning := range report.Warnings {
		fmt.Printf("- warning: %s\n", warning)
	}
}
