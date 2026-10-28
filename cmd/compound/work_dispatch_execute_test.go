package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkDispatchExecuteConfirmSelectsClaimsAndRunsGovernedAdapter(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", now)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	binDir := t.TempDir()
	launchLog := filepath.Join(t.TempDir(), "codex-launch.txt")
	fakeCodex := filepath.Join(binDir, "fixture-codex-wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$COMPOUND_TEST_LAUNCH_LOG\"\nprintf 'fixture executor consumed dispatch prompt\\n'\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(compoundTestBinary(t), "work", "dispatch-execute", w.ID, "--workspace", workspace, "--confirm", "--json")
	cmd.Env = []string{
		"COMPOUND_HOME=" + homePath,
		"HOME=" + userHome,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"COMPOUND_TEST_LAUNCH_LOG=" + launchLog,
		"TZ=UTC",
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("confirmed dispatch-execute failed: %v\n%s", runErr, out)
	}
	var result workDispatchExecutionResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse confirmed result: %v\n%s", err, out)
	}
	if result.Status != "executed" || result.WorkID != w.ID || result.FinalState != "verifying" {
		t.Fatalf("unexpected execution result: %+v", result)
	}
	if result.Selected.ModelID != "fixture-codex" || result.Adapter != "codex" {
		t.Fatalf("execution did not preserve selected lane: %+v", result)
	}
	if result.RunAttemptID == "" || result.RoutePlanID == "" || result.RouteSourceRevision == "" {
		t.Fatalf("execution evidence identities missing: %+v", result)
	}
	if strings.Contains(strings.Join(result.Warnings, "\n"), "does not execute work") {
		t.Fatalf("executed result retained the claim stage's contradictory warning: %v", result.Warnings)
	}

	final, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "verifying" || final.Authority.Owner != "codex" {
		t.Fatalf("selected adapter did not own and terminalize work: %+v", final)
	}
	if len(final.RunAttemptIDs) != 2 || final.RunAttemptIDs[1] != result.RunAttemptID {
		t.Fatalf("work attempt chain = %v, want claim reservation + selected execution", final.RunAttemptIDs)
	}
	attempt, err := runattempt.Open(homePath).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusSucceeded || attempt.Harness != "codex" || attempt.LaneID != "codex" {
		t.Fatalf("terminal execution identity = %+v", attempt)
	}
	if attempt.RequestedModelID != "fixture-codex" || attempt.RequestedProviderRoute != "fixture-provider" {
		t.Fatalf("selected tuple was not stamped on RunAttempt: %+v", attempt)
	}
	if filepath.Base(attempt.Command) != "fixture-codex-wrapper" {
		t.Fatalf("selected tuple did not control the launched executable: command=%q", attempt.Command)
	}
	joinedProofs := strings.Join(attempt.ProofRefs, "\n")
	if !strings.Contains(joinedProofs, result.RoutePlanID) || !strings.Contains(joinedProofs, result.RouteSourceRevision) {
		t.Fatalf("route authority missing from RunAttempt proof refs: %v", attempt.ProofRefs)
	}
	launchBody, err := os.ReadFile(launchLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launchBody), "exec") || !strings.Contains(string(launchBody), "extract a bounded record from an export") {
		t.Fatalf("existing dispatch prompt was not consumed by selected adapter:\n%s", launchBody)
	}
}

func TestWorkDispatchExecuteRefusesMissingAdapterBeforeClaim(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixtureForHarness(t, homePath, "switchback")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCapabilityResolveHome(t, homePath)

	_, err = buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err == nil || !strings.Contains(err.Error(), "no registered adapter") {
		t.Fatalf("missing adapter error = %v", err)
	}
	assertDispatchExecuteReadyAndUnchanged(t, store, w.ID, homePath, before)
}

func TestWorkDispatchExecuteRefusesMissingWorkspaceBeforeClaim(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCapabilityResolveHome(t, homePath)
	missing := filepath.Join(t.TempDir(), "absent-workspace")

	_, err = buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: missing}, cliTestNow)
	if err == nil || !strings.Contains(err.Error(), "workspace unavailable") {
		t.Fatalf("missing workspace error = %v", err)
	}
	assertDispatchExecuteReadyAndUnchanged(t, store, w.ID, homePath, before)
}

func TestWorkDispatchExecuteRefusesAdapterPreflightBeforeClaim(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "claude-code", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixtureForHarness(t, homePath, "claude-code")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Adapter != "claude_code" {
		t.Fatalf("selected adapter = %q, want normalized claude_code", plan.Adapter)
	}
	before := snapshotCapabilityResolveHome(t, homePath)

	_, err = executeWorkDispatchExecution(homePath, store, plan, workDispatchExecuteOptions{
		PermissionMode: "not-a-permission-mode",
	}, cliTestNow)
	if err == nil || !strings.Contains(err.Error(), "unknown --permission-mode") {
		t.Fatalf("adapter preflight error = %v", err)
	}
	assertDispatchExecuteReadyAndUnchanged(t, store, w.ID, homePath, before)
}

func TestWorkDispatchExecuteRefusesMissingCodexExecutableBeforeClaim(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCapabilityResolveHome(t, homePath)
	t.Setenv("PATH", t.TempDir())

	_, err = executeWorkDispatchExecution(homePath, store, plan, workDispatchExecuteOptions{}, cliTestNow)
	if err == nil || !strings.Contains(err.Error(), "no executable tuple alias") {
		t.Fatalf("missing Codex executable preflight error = %v", err)
	}
	assertDispatchExecuteReadyAndUnchanged(t, store, w.ID, homePath, before)
}

func TestWorkDispatchExecuteRefusesRouterRevisionDriftBeforeClaim(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	writeDispatchExecuteModelRouterFixtureForHarnessAlias(t, homePath, "codex", "fixture-codex-wrapper-v2")
	before := snapshotCapabilityResolveHome(t, homePath)

	_, err = executeWorkDispatchExecution(homePath, store, plan, workDispatchExecuteOptions{}, cliTestNow)
	if err == nil || !strings.Contains(err.Error(), "model-router revision drifted") {
		t.Fatalf("router revision drift error = %v", err)
	}
	assertDispatchExecuteReadyAndUnchanged(t, store, w.ID, homePath, before)
}

func TestWorkDispatchExecuteFailedSubprocessTerminalizesWorkAndAttempts(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	now := time.Now().UTC()
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", now)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	binDir := t.TempDir()
	fakeCodex := filepath.Join(binDir, "fixture-codex-wrapper")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nprintf 'fixture executor failed after launch\\n'\nexit 23\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(compoundTestBinary(t), "work", "dispatch-execute", w.ID, "--workspace", workspace, "--confirm", "--json")
	cmd.Env = []string{
		"COMPOUND_HOME=" + homePath,
		"HOME=" + userHome,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TZ=UTC",
	}
	out, runErr := cmd.CombinedOutput()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("failed dispatch-execute exit = %v, want 1\n%s", runErr, out)
	}
	var result workDispatchExecutionResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse failed result: %v\n%s", err, out)
	}
	if result.FinalState != "failed" || result.ExitCode != 23 || result.RunAttemptID == "" {
		t.Fatalf("unexpected failed execution result: %+v", result)
	}
	final, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "failed" {
		t.Fatalf("failed selected execution left work in %q", final.State)
	}
	attempts := runattempt.Open(homePath)
	attempt, err := attempts.Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusFailed {
		t.Fatalf("failed subprocess attempt status = %q", attempt.Status)
	}
	joinedProofs := strings.Join(attempt.ProofRefs, "\n")
	if !strings.Contains(joinedProofs, result.RoutePlanID) || !strings.Contains(joinedProofs, result.RouteSourceRevision) {
		t.Fatalf("failed selected execution lost route authority: %v", attempt.ProofRefs)
	}
	stuck, err := attempts.ListNonTerminal()
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 0 {
		t.Fatalf("failed selected execution left nonterminal attempts: %+v", stuck)
	}
}

func assertDispatchExecuteReadyAndUnchanged(t *testing.T, store *work.Store, workID, homePath, before string) {
	t.Helper()
	after := snapshotCapabilityResolveHome(t, homePath)
	if before != after {
		t.Fatalf("failed preflight mutated COMPOUND_HOME\nbefore=%s\nafter=%s", before, after)
	}
	w, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "ready" || w.Authority.Owner != "" {
		t.Fatalf("failed preflight claimed work: %+v", w)
	}
}

func TestWorkDispatchExecuteCLIUnconfirmedPreviewsWithoutMutation(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", time.Now().UTC())
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	before := snapshotCapabilityResolveHome(t, homePath)

	cmd := exec.Command(compoundTestBinary(t), "work", "dispatch-execute", w.ID, "--workspace", workspace, "--json")
	cmd.Env = []string{
		"COMPOUND_HOME=" + homePath,
		"HOME=" + userHome,
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
	}
	out, runErr := cmd.CombinedOutput()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("unconfirmed dispatch-execute exit = %v, want 1\n%s", runErr, out)
	}
	var preview workDispatchExecutionPlan
	if err := json.Unmarshal(out, &preview); err != nil {
		t.Fatalf("parse preview: %v\n%s", err, out)
	}
	if preview.Status != "preview" || preview.Selected.ModelID != "fixture-codex" || preview.Adapter != "codex" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if after := snapshotCapabilityResolveHome(t, homePath); before != after {
		t.Fatalf("unconfirmed CLI mutated COMPOUND_HOME\nbefore=%s\nafter=%s", before, after)
	}
}

func TestWorkDispatchExecutePreviewSelectsEligibleRouteWithoutMutation(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteModelRouterFixture(t, homePath)

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if routed, routeErr := routeevidence.PlanCapabilityRoute(homePath, "classify-extract", routeevidence.CapabilityRoutePlanOptions{SuppressPendingObservations: true}, cliTestNow); routeErr != nil {
		t.Fatalf("route fixture: %v", routeErr)
	} else if routed.Status != "planned" {
		t.Fatalf("route fixture not eligible: status=%s omitted=%+v warnings=%v", routed.Status, routed.Omitted, routed.Warnings)
	}

	before := snapshotCapabilityResolveHome(t, homePath)
	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{
		Workspace: workspace,
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotCapabilityResolveHome(t, homePath)
	if before != after {
		t.Fatalf("dispatch-execute preview mutated COMPOUND_HOME\nbefore=%s\nafter=%s", before, after)
	}

	if plan.Status != "preview" || plan.WorkID != w.ID {
		t.Fatalf("unexpected preview header: %+v", plan)
	}
	if plan.Selected.ModelID != "fixture-codex" || plan.Selected.Harness != "codex" {
		t.Fatalf("selector did not choose the first eligible route lane: %+v", plan.Selected)
	}
	if plan.Adapter != "codex" {
		t.Fatalf("resolved adapter = %q, want codex", plan.Adapter)
	}
	if plan.Workspace != workspace {
		t.Fatalf("workspace = %q, want %q", plan.Workspace, workspace)
	}
	if !strings.Contains(plan.Task, "extract a bounded record from an export") || !strings.Contains(plan.Task, "extraction returns one bounded record") {
		t.Fatalf("task was not derived from the WorkEnvelope contract: %q", plan.Task)
	}
	if plan.Resolution.RoutePlan.SourceAuthorityID == "" || plan.Resolution.RoutePlan.SourceRevision == "" {
		t.Fatalf("route authority missing from preview: %+v", plan.Resolution.RoutePlan)
	}

	unchanged, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != "ready" || unchanged.Authority.Owner != "" {
		t.Fatalf("preview changed work authority: %+v", unchanged)
	}
}

func TestWorkDispatchExecutePreviewSuppressesWalledLaneEvidence(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityRows(t, cliTestNow,
		dispatchExecuteCapacityRow{Provider: "qwencloud", UsedPercent: 100},
		dispatchExecuteCapacityRow{Provider: "codex", UsedPercent: 10},
	)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteWalledFallbackFixture(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCapabilityResolveHome(t, homePath)

	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selected.ModelID != "fixture-codex" {
		t.Fatalf("walled first lane did not fall through to live adapter-backed lane: %+v", plan.Selected)
	}
	if after := snapshotCapabilityResolveHome(t, homePath); before != after {
		t.Fatalf("walled-lane preview wrote refusal evidence\nbefore=%s\nafter=%s", before, after)
	}
}

func TestWorkDispatchExecuteSelectsAdapterBeyondPresentationLimit(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	userHome := writeDispatchExecuteCapacityFixture(t, "codex", cliTestNow)
	t.Setenv("HOME", userHome)
	addCapabilityGraphFixture(t, homePath)
	writeDispatchExecuteLongRouteFixture(t, homePath, 17)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "execution-consumer",
		DesiredOutcome:   "extract a bounded record from an export",
		SuccessPredicate: "the extraction returns one bounded record",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := buildWorkDispatchExecutionPlan(homePath, store, w.ID, workDispatchExecuteOptions{Workspace: t.TempDir()}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Resolution.RoutePlan.Chain) != capabilityResolutionRouteLaneLimit || plan.Resolution.RoutePlan.OmittedRouteLaneCount != 2 {
		t.Fatalf("presentation bound changed: shown=%d omitted=%d", len(plan.Resolution.RoutePlan.Chain), plan.Resolution.RoutePlan.OmittedRouteLaneCount)
	}
	if plan.Selected.ModelID != "fixture-codex" || plan.Adapter != "codex" {
		t.Fatalf("selector searched only the presentation slice: selected=%+v adapter=%q", plan.Selected, plan.Adapter)
	}
}

type dispatchExecuteCapacityRow struct {
	Provider    string
	UsedPercent float64
}

func writeDispatchExecuteCapacityFixture(t *testing.T, provider string, now time.Time) string {
	t.Helper()
	return writeDispatchExecuteCapacityRows(t, now, dispatchExecuteCapacityRow{Provider: provider, UsedPercent: 10})
}

func writeDispatchExecuteCapacityRows(t *testing.T, now time.Time, rows ...dispatchExecuteCapacityRow) string {
	t.Helper()
	userHome := t.TempDir()
	usagePath := filepath.Join(userHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&body, "{\"provider\":%q,\"windowKind\":\"weekly\",\"windowMinutes\":10080,\"usedPercent\":%g,\"resetsAt\":%q,\"sampledAt\":%q}\n",
			row.Provider, row.UsedPercent, now.Add(24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if err := os.WriteFile(usagePath, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return userHome
}

func writeDispatchExecuteModelRouterFixture(t *testing.T, homePath string) {
	t.Helper()
	writeDispatchExecuteModelRouterFixtureForHarness(t, homePath, "codex")
}

func writeDispatchExecuteModelRouterFixtureForHarness(t *testing.T, homePath, harnessName string) {
	t.Helper()
	writeDispatchExecuteModelRouterFixtureForHarnessAlias(t, homePath, harnessName, "fixture-codex-wrapper")
}

func writeDispatchExecuteModelRouterFixtureForHarnessAlias(t *testing.T, homePath, harnessName, alias string) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	body := fmt.Sprintf(`version: 1
purpose: "Synthetic selector-to-executor fixture."
rules:
  - "Route extraction to the cheapest sufficient registered executor."
capability_classes:
  - classify-extract
models:
  fixture-codex:
    lane: "fixture codex lane"
    harness: %q
    provider_route: "fixture-provider"
    resolution:
      mode: direct
      targets:
        - provider: fixture-provider
          model: fixture-codex
    effort_default: low
    cost_class: free
    tier: free
    packet_ceiling: single-file
    aliases: [%s]
    capabilities:
      classify-extract:
        level: primary
        evidence: "synthetic fixture proves selector composition"
routing_defaults:
  classify-extract:
    - fixture-codex
`, harnessName, alias)
	if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, sourcePath, cliTestNow); err != nil {
		t.Fatal(err)
	}
}

func writeDispatchExecuteWalledFallbackFixture(t *testing.T, homePath string) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	body := `version: 1
purpose: "Synthetic walled fallback fixture."
rules: ["Walled lanes are omitted before adapter selection."]
capability_classes: [classify-extract]
models:
  fixture-walled:
    lane: "fixture walled lane"
    harness: switchback
    provider_route: qwen
    resolution: {mode: direct, targets: [{provider: qwen, model: fixture-walled}]}
    cost_class: free
    tier: free
    packet_ceiling: single-file
    capacity_source: {kind: codexbar, freshness_minutes: 10, ref: "codexbar://provider/qwencloud"}
    capabilities: {classify-extract: {level: primary, evidence: "synthetic walled fixture"}}
  fixture-codex:
    lane: "fixture codex lane"
    harness: codex
    provider_route: fixture-provider
    resolution: {mode: direct, targets: [{provider: fixture-provider, model: fixture-codex}]}
    cost_class: free
    tier: free
    packet_ceiling: single-file
    aliases: [fixture-codex-wrapper]
    capacity_source: {kind: codexbar, freshness_minutes: 10, ref: "codexbar://provider/codex"}
    capabilities: {classify-extract: {level: primary, evidence: "synthetic live fixture"}}
routing_defaults:
  classify-extract: [fixture-walled, fixture-codex]
`
	if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, sourcePath, cliTestNow); err != nil {
		t.Fatal(err)
	}
}

func writeDispatchExecuteLongRouteFixture(t *testing.T, homePath string, missingAdapters int) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	var body strings.Builder
	body.WriteString("version: 1\npurpose: synthetic long route\nrules: [search the full eligible route]\ncapability_classes: [classify-extract]\nmodels:\n")
	defaults := make([]string, 0, missingAdapters+1)
	for i := 0; i < missingAdapters; i++ {
		modelID := fmt.Sprintf("fixture-missing-%02d", i)
		defaults = append(defaults, modelID)
		fmt.Fprintf(&body, "  %s:\n    lane: %s lane\n    harness: missing_harness_%02d\n    provider_route: fixture-provider-%02d\n    tier: free\n    packet_ceiling: single-file\n    capabilities: {classify-extract: {level: primary, evidence: synthetic fixture}}\n", modelID, modelID, i, i)
	}
	defaults = append(defaults, "fixture-codex")
	body.WriteString("  fixture-codex:\n    lane: fixture codex lane\n    harness: codex\n    provider_route: fixture-provider\n    tier: free\n    packet_ceiling: single-file\n    aliases: [fixture-codex-wrapper]\n    capabilities: {classify-extract: {level: primary, evidence: synthetic fixture}}\n")
	fmt.Fprintf(&body, "routing_defaults:\n  classify-extract: [%s]\n", strings.Join(defaults, ", "))
	if err := os.WriteFile(sourcePath, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, sourcePath, cliTestNow); err != nil {
		t.Fatal(err)
	}
}
