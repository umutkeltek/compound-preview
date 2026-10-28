package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/work"
)

func TestWorkCreateCLIPropagatesExpectedBaseSHA(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo, base, _ := initLandingCLIRepo(t)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}

	planOut := captureStdout(t, func() {
		cmdWork([]string{
			"create-plan", "--owner", "codex", "--predicate", "land exactly once",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--local-only", "--by", "codex", "--dry-run", "--json",
		})
	})
	var plan work.CreatePlan
	if err := json.Unmarshal([]byte(planOut), &plan); err != nil {
		t.Fatalf("parse create-plan JSON: %v\n%s", err, planOut)
	}
	if plan.Work.GitDelivery == nil || plan.Work.GitDelivery.ExpectedBaseSHA != base || plan.Work.GitDelivery.TargetWorktreeRoot != canonicalRepo || plan.Work.GitDelivery.PublicationMode != "local_only" {
		t.Fatalf("planned git_delivery = %+v", plan.Work.GitDelivery)
	}
	if !landingCommandHasPair(plan.CreateCommand, "--expected-base-sha", base) ||
		!landingCommandHasPair(plan.CreateCommand, "--target-worktree-root", canonicalRepo) ||
		!landingCommandHas(plan.CreateCommand, "--local-only") {
		t.Fatalf("create command dropped git delivery contract: %v", plan.CreateCommand)
	}

	createOut := captureStdout(t, func() {
		cmdWork([]string{
			"create", "--owner", "codex", "--predicate", "persist exact base",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--local-only", "--by", "codex",
		})
	})
	if !strings.Contains(createOut, `"git_delivery": {`) || !strings.Contains(createOut, `"expected_base_sha": "`+base+`"`) {
		t.Fatalf("created WorkEnvelope dropped git delivery contract:\n%s", createOut)
	}

	runLandingCLIGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	publishedOut := captureStdout(t, func() {
		cmdWork([]string{
			"create-plan", "--owner", "codex", "--predicate", "publish exact candidate",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--publication-ref", "refs/remotes/origin/main",
			"--by", "codex", "--dry-run", "--json",
		})
	})
	var published work.CreatePlan
	if err := json.Unmarshal([]byte(publishedOut), &published); err != nil {
		t.Fatalf("parse publication create-plan JSON: %v\n%s", err, publishedOut)
	}
	if published.Work.GitDelivery == nil || published.Work.GitDelivery.PublicationMode != "required" || published.Work.GitDelivery.PublicationRef != "refs/remotes/origin/main" {
		t.Fatalf("publication delivery contract = %+v", published.Work.GitDelivery)
	}
}

func TestWorkLandCheckCLIUsesExplicitRepoAndRecordsProof(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo, base, tip := initLandingCLIRepo(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		Actor:            "codex",
		SuccessPredicate: "candidate is visibly ready to land",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    base,
			PublicationMode:    "local_only",
		},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: w.State, Actor: "codex", ArtifactID: "commit:" + tip}, cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeDetachedSourceBoundProofForCLITest(t, store, homePath, w.ID, "codex", repo, tip, cliTestNow.Add(time.Minute+time.Second))

	dryOut := captureStdout(t, func() {
		cmdWork([]string{
			"land-check", w.ID, "--repo", repo, "--tip", tip, "--target-ref", "refs/heads/main",
			"--dry-run", "--json",
		})
	})
	var dry work.LandCheckReport
	if err := json.Unmarshal([]byte(dryOut), &dry); err != nil {
		t.Fatalf("parse dry land-check JSON: %v\n%s", err, dryOut)
	}
	if dry.Status != "ready_to_land" || dry.Recorded || dry.RepoPath != w.GitDelivery.TargetWorktreeRoot {
		t.Fatalf("dry land-check = %+v", dry)
	}

	runLandingCLIGit(t, repo, "merge", "--ff-only", tip)
	landedOut := captureStdout(t, func() {
		cmdWork([]string{
			"land-check", w.ID, "--tip", tip, "--actor", "codex", "--json",
		})
	})
	var landed work.LandCheckReport
	if err := json.Unmarshal([]byte(landedOut), &landed); err != nil {
		t.Fatalf("parse landed land-check JSON: %v\n%s", err, landedOut)
	}
	if landed.Status != "landed" || !landed.Recorded || !strings.HasPrefix(landed.EvidenceRef, "land-check:") {
		t.Fatalf("recorded land-check = %+v", landed)
	}
	if _, err := os.Stat(landed.ProofPath); err != nil {
		t.Fatalf("recorded proof missing: %v", err)
	}
	attached, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stringSliceContains(attached.EvidenceRefs, landed.EvidenceRef) {
		t.Fatalf("recorded proof not discoverable from WorkEnvelope: %+v", attached.EvidenceRefs)
	}
}

const workLandCheckStealCLISubEnv = "COMPOUND_WORK_LAND_CHECK_STEAL_CLI_SUB"

func TestWorkLandCheckCLIStealOnlyOverridesProofAttachment(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv(workLandCheckStealCLISubEnv); mode != "" {
		args := []string{
			"land-check", os.Getenv("COMPOUND_WORK_LAND_CHECK_STEAL_WORK_ID"),
			"--tip", os.Getenv("COMPOUND_WORK_LAND_CHECK_STEAL_TIP"),
			"--actor", "operator:test", "--steal", "--reason", "stale executor was replaced",
			"--json",
		}
		if mode == "dry-run" {
			args = append(args, "--dry-run")
		}
		cmdWork(args)
		return
	}

	homePath := initializedTestHome(t)
	repo, base, tip := initLandingCLIRepo(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		Actor:            "codex",
		SuccessPredicate: "stale holder cannot hide current landing proof",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    base,
			PublicationMode:    "local_only",
		},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "actor-a", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{
		To: "running", Actor: "actor-a", ArtifactID: "commit:" + tip,
	}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeDetachedSourceBoundProofForCLITest(t, store, homePath, w.ID, "actor-a", repo, tip, cliTestNow.Add(2*time.Minute+time.Second))

	leasePath := filepath.Join(homePath, "work", "leases", w.ID+".json")
	leaseBefore, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, eventsBefore, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	runChild := func(t *testing.T, mode string) (work.LandCheckReport, []byte) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestWorkLandCheckCLIStealOnlyOverridesProofAttachment$")
		cmd.Env = append(os.Environ(),
			workLandCheckStealCLISubEnv+"="+mode,
			"COMPOUND_HOME="+homePath,
			"COMPOUND_WORK_LAND_CHECK_STEAL_WORK_ID="+w.ID,
			"COMPOUND_WORK_LAND_CHECK_STEAL_TIP="+tip,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("land-check %s failed: %v\n%s", mode, err, out)
		}
		var report work.LandCheckReport
		if err := json.NewDecoder(bytes.NewReader(out)).Decode(&report); err != nil {
			t.Fatalf("parse land-check %s JSON: %v\n%s", mode, err, out)
		}
		return report, out
	}

	ready, _ := runChild(t, "record")
	if ready.Status != "ready_to_land" || !ready.Recorded {
		t.Fatalf("pre-land report = %+v", ready)
	}
	beforeLand, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stringSliceContains(beforeLand.EvidenceRefs, ready.EvidenceRef) {
		t.Fatalf("non-landed report must not attach close evidence: %+v", beforeLand.EvidenceRefs)
	}
	_, eventsAfterReady, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eventsAfterReady, eventsBefore) {
		t.Fatalf("non-landed --steal must not invoke Advance: before=%+v after=%+v", eventsBefore, eventsAfterReady)
	}
	if leaseAfterReady, err := os.ReadFile(leasePath); err != nil || !bytes.Equal(leaseAfterReady, leaseBefore) {
		t.Fatalf("non-landed --steal changed lease: err=%v\nbefore=%s\nafter=%s", err, leaseBefore, leaseAfterReady)
	}

	runLandingCLIGit(t, repo, "merge", "--ff-only", tip)
	proofsBeforeDry, err := os.ReadDir(filepath.Join(homePath, "work", "land-checks"))
	if err != nil {
		t.Fatal(err)
	}
	dry, _ := runChild(t, "dry-run")
	if dry.Status != "landed" || dry.Recorded || dry.EvidenceRef != "" {
		t.Fatalf("dry-run report = %+v", dry)
	}
	proofsAfterDry, err := os.ReadDir(filepath.Join(homePath, "work", "land-checks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(proofsAfterDry) != len(proofsBeforeDry) {
		t.Fatalf("dry-run recorded proof: before=%d after=%d", len(proofsBeforeDry), len(proofsAfterDry))
	}
	_, eventsAfterDry, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eventsAfterDry, eventsBefore) {
		t.Fatalf("dry-run --steal must not invoke Advance: before=%+v after=%+v", eventsBefore, eventsAfterDry)
	}

	landed, _ := runChild(t, "record")
	if landed.Status != "landed" || !landed.Recorded || landed.EvidenceRef == "" {
		t.Fatalf("landed report = %+v", landed)
	}
	after, eventsAfterLand, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || after.Authority.Owner != "actor-a" || !stringSliceContains(after.EvidenceRefs, landed.EvidenceRef) {
		t.Fatalf("same-state proof attachment changed work authority or missed evidence: %+v", after)
	}
	leaseAfterLand, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leaseAfterLand, leaseBefore) {
		t.Fatalf("land-check --steal transferred or rewrote lease:\nbefore=%s\nafter=%s", leaseBefore, leaseAfterLand)
	}
	var sawSteal, sawAttachment bool
	for _, event := range eventsAfterLand[len(eventsBefore):] {
		if event.From != "running" || event.To != "running" || event.By != "operator:test" {
			t.Fatalf("land-check override must stay a same-state operator advance: %+v", event)
		}
		if strings.Contains(event.Note, "lease stolen from \"actor-a\": stale executor was replaced") {
			sawSteal = true
		}
		if stringSliceContains(event.Attached, landed.EvidenceRef) {
			sawAttachment = true
		}
	}
	if !sawSteal || !sawAttachment {
		t.Fatalf("land-check must reuse audited Advance steal then attach proof: steal=%v attachment=%v events=%+v", sawSteal, sawAttachment, eventsAfterLand)
	}
}

func TestWorkAdvanceCLIBindsLegacyGitDelivery(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo, base, _ := initLandingCLIRepo(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "legacy work binds delivery", State: "ready"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "operator:test", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	planOut := captureStdout(t, func() {
		cmdWork([]string{
			"advance-plan", w.ID, "--to", "running", "--actor", "operator:test",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--local-only", "--dry-run", "--json",
		})
	})
	var plan work.AdvancePlan
	if err := json.Unmarshal([]byte(planOut), &plan); err != nil {
		t.Fatalf("parse delivery binding advance-plan: %v\n%s", err, planOut)
	}
	if plan.GitDelivery == nil || plan.GitDelivery.ExpectedBaseSHA != base || !landingCommandHas(plan.AdvanceCommand, "--local-only") {
		t.Fatalf("delivery binding plan = %+v", plan)
	}

	captureStdout(t, func() {
		cmdWork([]string{
			"advance", w.ID, "--to", "running", "--by", "operator:test",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--local-only",
		})
	})
	bound, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.GitDelivery == nil || bound.GitDelivery.ExpectedBaseSHA != base || bound.GitDelivery.PublicationMode != "local_only" {
		t.Fatalf("CLI-bound delivery contract = %+v", bound.GitDelivery)
	}
}

func TestWorkAdvanceHelpServesLegacyDeliveryBindingFlags(t *testing.T) {
	for _, sub := range []string{"advance-plan", "advance"} {
		t.Run(sub, func(t *testing.T) {
			out := captureStdout(t, func() {
				cmdWork([]string{sub, "--help"})
			})
			for _, flag := range []string{"--target-worktree-root", "--target-local-ref", "--expected-base-sha", "--publication-ref", "--local-only"} {
				if !strings.Contains(out, flag) {
					t.Fatalf("%s help missing %s:\n%s", sub, flag, out)
				}
			}
		})
	}
}

const workLandingGuardCLISubEnv = "COMPOUND_WORK_LANDING_GUARD_CLI_SUB"

func TestWorkLandingGuardCLIRejectsMalformedAndUnknownFlags(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv(workLandingGuardCLISubEnv); mode != "" {
		switch mode {
		case "create-unknown":
			cmdWork([]string{"create", "--owner", "codex", "--predicate", "x", "--unknown"})
		case "create-partial-contract":
			cmdWork([]string{"create", "--owner", "codex", "--predicate", "x", "--expected-base-sha", strings.Repeat("a", 40)})
		case "land-unknown":
			cmdWork([]string{"land-check", "work_x", "--tip", strings.Repeat("a", 40), "--expected-main-sha", strings.Repeat("b", 40)})
		case "land-missing-value":
			cmdWork([]string{"land-check", "work_x", "--tip"})
		case "land-steal-without-reason":
			cmdWork([]string{"land-check", "work_x", "--tip", strings.Repeat("a", 40), "--steal"})
		case "land-reason-without-steal":
			cmdWork([]string{"land-check", "work_x", "--tip", strings.Repeat("a", 40), "--reason", "stale holder"})
		case "land-empty-steal-reason":
			cmdWork([]string{"land-check", "work_x", "--tip", strings.Repeat("a", 40), "--steal", "--reason", "   "})
		case "advance-partial-contract":
			cmdWork([]string{"advance", "work_x", "--to", "running", "--expected-base-sha", strings.Repeat("a", 40)})
		}
		return
	}

	homePath := initializedTestHome(t)
	for _, mode := range []string{
		"create-unknown", "create-partial-contract", "land-unknown", "land-missing-value",
		"land-steal-without-reason", "land-reason-without-steal", "land-empty-steal-reason",
		"advance-partial-contract",
	} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkLandingGuardCLIRejectsMalformedAndUnknownFlags$")
			cmd.Env = append(os.Environ(), workLandingGuardCLISubEnv+"="+mode, "COMPOUND_HOME="+homePath)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("%s should exit 2, err=%v output=%s", mode, err, output)
			}
			if !strings.Contains(string(output), "usage: compound work") {
				t.Fatalf("%s must print work usage, got %s", mode, output)
			}
		})
	}
	if !strings.Contains(workUsage(""), "land-check") ||
		!strings.Contains(workUsage("land-check"), "--tip") ||
		!strings.Contains(workUsage("land-check"), "--steal --reason") {
		t.Fatalf("work help must expose land-check: root=%q sub=%q", workUsage(""), workUsage("land-check"))
	}
}

func landingCommandHasPair(command []string, flag, value string) bool {
	for i := 0; i+1 < len(command); i++ {
		if command[i] == flag && command[i+1] == value {
			return true
		}
	}
	return false
}

func landingCommandHas(command []string, flag string) bool {
	for _, item := range command {
		if item == flag {
			return true
		}
	}
	return false
}

func initLandingCLIRepo(t *testing.T) (repo, base, tip string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runLandingCLIGit(t, repo, "init", "-b", "main")
	runLandingCLIGit(t, repo, "config", "user.name", "Compound Test")
	runLandingCLIGit(t, repo, "config", "user.email", "compound@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runLandingCLIGit(t, repo, "add", "base.txt")
	runLandingCLIGit(t, repo, "commit", "-m", "base")
	base = landingCLIOutput(t, repo, "rev-parse", "HEAD")
	runLandingCLIGit(t, repo, "switch", "-c", "candidate")
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runLandingCLIGit(t, repo, "add", "candidate.txt")
	runLandingCLIGit(t, repo, "commit", "-m", "candidate")
	tip = landingCLIOutput(t, repo, "rev-parse", "HEAD")
	runLandingCLIGit(t, repo, "switch", "main")
	return repo, base, tip
}

func runLandingCLIGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func landingCLIOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
