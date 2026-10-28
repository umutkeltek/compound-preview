package authority

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// The injected runner owns all launchd effects; no host service is contacted.
func TestLaunchdTerminalApplyRejectsAbsentAfterLastReceipt(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	f.f.env.Checkpoint = func(point string) error {
		if point == "bootstrap:after_receipt" {
			f.loaded = false
		}
		return nil
	}
	result, err := f.apply()
	if err == nil || result.Outcome == "completed" {
		t.Fatalf("terminal drift falsely completed: outcome=%s err=%v", result.Outcome, err)
	}
	terminal := filepath.Join(f.home, "reviews", "launchd-repair", f.plan.ID+".apply.terminal.json")
	if _, err := os.Stat(terminal); !os.IsNotExist(err) {
		t.Fatalf("terminal drift published completion: %v", err)
	}
}

// launchdTerminalDriftAt injects live drift exactly once, immediately after the
// named phase receipt is sealed. mutate owns fixture state; rewrite, when set,
// keeps returning a valid foreign launchd service body for the injected runner.
// The checkpoint runs inside the copy of the environment already handed to the
// executor, so drift must travel through fixture state the runner closes over.
func launchdTerminalDriftAt(t *testing.T, f *adversarialLiveFixture, point string, mutate func(), rewrite func(string) string) {
	t.Helper()
	active := false
	if rewrite != nil {
		base := f.f.env.Run
		f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
			r, err := base(ctx, exe, args...)
			if active && exe == "/bin/launchctl" && len(args) > 0 && args[0] == "print" {
				r.Stdout = rewrite(r.Stdout)
			}
			return r, err
		}
	}
	f.f.env.Checkpoint = func(p string) error {
		if p != point {
			return nil
		}
		mutate()
		active = true
		return nil
	}
}

func launchdForeignOwnerPointer(t *testing.T, f *adversarialLiveFixture) {
	t.Helper()
	path, err := launchdArtifactPath(f.home, launchdActiveName(f.plan))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, launchdOwnerBytes("ldrp_terminal_foreign"), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertNoLaunchdTerminal(t *testing.T, f *adversarialLiveFixture, direction string) {
	t.Helper()
	path, err := launchdArtifactPath(f.home, f.plan.ID+"."+direction+".terminal.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("terminal drift published completion: %v", err)
	}
}

// Successful native phase receipts are history, not a terminal claim: drift
// after the last one must leave every prior receipt authentic and untouched.
func assertLaunchdNativePhases(t *testing.T, f *adversarialLiveFixture, direction string, indices []int) {
	t.Helper()
	for _, i := range indices {
		stem := launchdPhaseStem(f.plan, direction, i)
		var start launchdPhaseStart
		var outcome launchdPhaseOutcome
		var commit launchdPhaseCommit
		for _, item := range []struct {
			name  string
			value any
		}{{stem + ".started.json", &start}, {stem + ".outcome.json", &outcome}, {stem + ".committed.json", &commit}} {
			if err := readLaunchdArtifact(f.home, item.name, item.value); err != nil {
				t.Fatal(err)
			}
		}
		if start.Direction != direction || start.Index != i || commit.Outcome != "completed" || outcome.Final.Outcome != "completed" {
			t.Fatalf("phase %d/%s lost its successful receipt: %+v", i, direction, commit)
		}
		if err := verifyLaunchdCommittedPhase(f.home, f.plan, start, outcome, commit); err != nil {
			t.Fatalf("phase %d/%s native evidence rewritten: %v", i, direction, err)
		}
	}
}

func TestLaunchdTerminalApplyRejectsFinalStateDriftAfterLastReceipt(t *testing.T) {
	for _, kind := range []string{"loaded-absent", "override-disabled", "foreign-service-path", "foreign-service-program", "foreign-service-arguments", "foreign-plist-bytes", "foreign-owner", "work-generation"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdversarialLiveFixture(t)
			var rewrite func(string) string
			var mutate func()
			switch kind {
			case "loaded-absent":
				mutate = func() { f.loaded = false }
			case "override-disabled":
				mutate = func() { f.disabled = true }
			case "foreign-service-path":
				mutate = func() { f.loaded = true }
				rewrite = func(body string) string {
					return strings.Replace(body, "path = "+f.plan.PlistPath, "path = "+f.plan.PlistPath+".foreign", 1)
				}
			case "foreign-service-program":
				mutate = func() { f.loaded = true }
				rewrite = func(body string) string {
					return strings.Replace(body, "program = "+f.plan.CandidateArgs[0], "program = /example/foreign/program", 1)
				}
			case "foreign-service-arguments":
				mutate = func() { f.loaded = true }
				rewrite = func(body string) string { return strings.Replace(body, "\nrun\n", "\ndelete\n", 1) }
			case "foreign-plist-bytes":
				mutate = func() {
					if err := os.WriteFile(f.plan.PlistPath, []byte(`{"Label":"foreign"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-owner":
				mutate = func() { launchdForeignOwnerPointer(t, f) }
			case "work-generation":
				mutate = func() {
					f.now = f.now.Add(time.Millisecond)
					if _, err := f.store.Advance(f.plan.WorkID, work.AdvanceInput{To: "running", Actor: f.plan.Actor, Note: "unrelated generation"}, f.now); err != nil {
						t.Fatal(err)
					}
				}
			default:
				t.Fatalf("unknown drift kind %s", kind)
			}
			launchdTerminalDriftAt(t, f, "bootstrap:after_receipt", mutate, rewrite)
			result, err := f.apply()
			if err == nil || result.Outcome == "completed" {
				t.Fatalf("terminal drift falsely completed: outcome=%s err=%v", result.Outcome, err)
			}
			if result.Failure == "" && kind != "work-generation" {
				t.Fatal("terminal refusal did not state its cause")
			}
			assertNoLaunchdTerminal(t, f, "apply")
			assertLaunchdNativePhases(t, f, "apply", []int{0, 1, 2})
			effects := append([]string{}, f.effects...)
			if _, err = f.apply(); err == nil {
				t.Fatal("retry under terminal drift completed")
			}
			if !reflect.DeepEqual(effects, f.effects) {
				t.Fatalf("retry repeated OS effects: %v -> %v", effects, f.effects)
			}
			assertNoLaunchdTerminal(t, f, "apply")
			assertLaunchdNativePhases(t, f, "apply", []int{0, 1, 2})
		})
	}
}

func TestLaunchdTerminalRollbackRejectsFinalStateDriftAfterLastReceipt(t *testing.T) {
	for _, kind := range []string{"loaded-after-restore", "enabled-after-restore", "foreign-original-bytes", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdversarialLiveFixture(t)
			if _, err := f.apply(); err != nil {
				t.Fatal(err)
			}
			rr := f.sealReview(t, true)
			var mutate func()
			switch kind {
			case "loaded-after-restore":
				mutate = func() { f.loaded = true }
			case "enabled-after-restore":
				mutate = func() { f.disabled = false }
			case "foreign-original-bytes":
				mutate = func() {
					if err := os.WriteFile(f.plan.PlistPath, []byte(`{"Label":"foreign"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-owner":
				mutate = func() { launchdForeignOwnerPointer(t, f) }
			default:
				t.Fatalf("unknown drift kind %s", kind)
			}
			launchdTerminalDriftAt(t, f, "restore:after_receipt", mutate, nil)
			rollback := func() (LaunchdRepairResult, error) {
				return RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rr.ID, f.plan.Actor, f.f.env)
			}
			result, err := rollback()
			if err == nil || result.Outcome == "completed" {
				t.Fatalf("terminal rollback drift falsely completed: outcome=%s err=%v", result.Outcome, err)
			}
			assertNoLaunchdTerminal(t, f, "rollback")
			assertLaunchdNativePhases(t, f, "rollback", []int{0, 1, 2})
			effects := append([]string{}, f.effects...)
			if _, err = rollback(); err == nil {
				t.Fatal("retry under terminal rollback drift completed")
			}
			if !reflect.DeepEqual(effects, f.effects) {
				t.Fatalf("retry repeated OS effects: %v -> %v", effects, f.effects)
			}
			assertNoLaunchdTerminal(t, f, "rollback")
		})
	}
}

// The positive side of the same gate: an authentic predecessor restoration and
// a read-only no-effect continuation still seal their terminal on the exact
// predecessor pointer, and a completed apply still does on its own.
func TestLaunchdTerminalHandoffAndNoEffectSealExactFinalState(t *testing.T) {
	t.Run("handoff-apply-and-rollback", func(t *testing.T) {
		f := completedLaunchdPredecessor(t)
		old := f.plan
		p, err := launchdSuccessorPlan(t, f, old, true)
		if err != nil {
			t.Fatal(err)
		}
		recordLaunchdSuccessor(t, f, p)
		r, err := f.apply()
		if err != nil || r.Outcome != "completed" {
			t.Fatalf("exact successor apply refused: %+v %v", r, err)
		}
		var applied LaunchdRepairResult
		if err = readLaunchdArtifact(f.home, p.ID+".apply.terminal.json", &applied); err != nil {
			t.Fatal(err)
		}
		b, _, err := launchdOwnedFile(p.PlistPath, p.UID)
		if err != nil || !bytesEqual(b, p.Candidate) || launchdStateMatches(p, b, applied.Runtime, p.Candidate, "enabled", "loaded") != nil {
			t.Fatalf("sealed apply terminal does not bind the exact live final state: %v", err)
		}
		owner, err := readLaunchdOwner(f.home, p)
		if err != nil || !bytesEqual(owner, launchdOwnerBytes(p.ID)) {
			t.Fatalf("sealed apply terminal lost exact plan ownership: %s %v", owner, err)
		}
		rr := f.sealReview(t, true)
		r, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env)
		if err != nil || r.Outcome != "completed" {
			t.Fatalf("exact restoration refused: %+v %v", r, err)
		}
		var restored LaunchdRepairResult
		if err = readLaunchdArtifact(f.home, p.ID+".rollback.terminal.json", &restored); err != nil {
			t.Fatal(err)
		}
		b, _, err = launchdOwnedFile(p.PlistPath, p.UID)
		if err != nil || !bytesEqual(b, p.Original) || launchdStateMatches(p, b, restored.Runtime, p.Original, "disabled", "absent") != nil {
			t.Fatalf("sealed rollback terminal does not bind the exact restored state: %v", err)
		}
		if owner, err = readLaunchdOwner(f.home, p); err != nil || !bytesEqual(owner, p.Handoff.Owner) {
			t.Fatalf("sealed rollback terminal lost the predecessor pointer: %s %v", owner, err)
		}
	})
	t.Run("no-effect-continuation", func(t *testing.T) {
		f, _ := expiredNoEffectContinuationFixture(t)
		p := f.plan
		rotateNoEffectContinuationGrant(t, f, "grant_terminal_no_effect")
		review := f.sealReviewGrant(t, true, "grant_terminal_no_effect")
		r, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env)
		if err != nil || r.Outcome != "completed" {
			t.Fatalf("no-effect continuation refused: %+v %v", r, err)
		}
		var terminal LaunchdRepairResult
		if err = readLaunchdArtifact(f.home, p.ID+".rollback.terminal.json", &terminal); err != nil {
			t.Fatal(err)
		}
		b, _, err := launchdOwnedFile(p.PlistPath, p.UID)
		if err != nil || !bytesEqual(b, p.Original) || launchdStateMatches(p, b, terminal.Runtime, p.Original, "disabled", "absent") != nil {
			t.Fatalf("no-effect terminal does not bind the exact unchanged state: %v", err)
		}
		if owner, err := readLaunchdOwner(f.home, p); err != nil || !bytesEqual(owner, p.Handoff.Owner) {
			t.Fatalf("no-effect terminal lost the predecessor pointer: %s %v", owner, err)
		}
		if len(f.effects) != 2 {
			t.Fatalf("no-effect observation caused OS effects: %v", f.effects)
		}
	})
}

func TestLaunchdTerminalHandoffPathsRefuseFinalDrift(t *testing.T) {
	t.Run("apply-owner", func(t *testing.T) {
		f := completedLaunchdPredecessor(t)
		old := f.plan
		p, err := launchdSuccessorPlan(t, f, old, true)
		if err != nil {
			t.Fatal(err)
		}
		recordLaunchdSuccessor(t, f, p)
		launchdTerminalDriftAt(t, f, "bootstrap:after_receipt", func() { launchdForeignOwnerPointer(t, f) }, nil)
		r, err := f.apply()
		if err == nil || r.Outcome == "completed" {
			t.Fatalf("foreign ownership after handoff falsely completed: %+v %v", r, err)
		}
		assertNoLaunchdTerminal(t, f, "apply")
		assertLaunchdNativePhases(t, f, "apply", []int{3, 0, 1, 2})
		if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap", "enable", "bootstrap"}) {
			t.Fatalf("terminal refusal repeated OS effects: %v", f.effects)
		}
	})
	t.Run("rollback-owner", func(t *testing.T) {
		f := completedLaunchdPredecessor(t)
		old := f.plan
		p, err := launchdSuccessorPlan(t, f, old, true)
		if err != nil {
			t.Fatal(err)
		}
		recordLaunchdSuccessor(t, f, p)
		if _, err = f.apply(); err != nil {
			t.Fatal(err)
		}
		rr := f.sealReview(t, true)
		launchdTerminalDriftAt(t, f, "restore_owner:after_receipt", func() { launchdForeignOwnerPointer(t, f) }, nil)
		r, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env)
		if err == nil || r.Outcome == "completed" {
			t.Fatalf("restored ownership drift falsely completed: %+v %v", r, err)
		}
		assertNoLaunchdTerminal(t, f, "rollback")
		if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap", "enable", "bootstrap", "bootout", "disable"}) {
			t.Fatalf("terminal refusal repeated OS effects: %v", f.effects)
		}
	})
	t.Run("no-effect-owner", func(t *testing.T) {
		f, _ := expiredNoEffectContinuationFixture(t)
		p := f.plan
		rotateNoEffectContinuationGrant(t, f, "grant_terminal_no_effect_drift")
		review := f.sealReviewGrant(t, true, "grant_terminal_no_effect_drift")
		launchdTerminalDriftAt(t, f, "restore_owner:after_receipt", func() { launchdForeignOwnerPointer(t, f) }, nil)
		r, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env)
		if err == nil || r.Outcome == "completed" {
			t.Fatalf("no-effect ownership drift falsely completed: %+v %v", r, err)
		}
		assertNoLaunchdTerminal(t, f, "rollback")
		if len(f.effects) != 2 {
			t.Fatalf("no-effect terminal refusal repeated OS effects: %v", f.effects)
		}
	})
	t.Run("applied-then-tampered-terminal-state", func(t *testing.T) {
		// A completed apply replay revalidates live final state: post-terminal
		// drift must be refused without inventing a phase or losing the receipt.
		f := newAdversarialLiveFixture(t)
		if _, err := f.apply(); err != nil {
			t.Fatal(err)
		}
		var terminal LaunchdRepairResult
		if err := readLaunchdArtifact(f.home, f.plan.ID+".apply.terminal.json", &terminal); err != nil {
			t.Fatal(err)
		}
		f.loaded = false
		result, err := f.apply()
		if err == nil || result.Outcome == "completed" {
			t.Fatalf("replay ignored live drift after a completed apply: %+v %v", result, err)
		}
		if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
			t.Fatalf("replay repeated OS effects: %v", f.effects)
		}
		var sealed LaunchdRepairResult
		if err = readLaunchdArtifact(f.home, f.plan.ID+".apply.terminal.json", &sealed); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sealed, terminal) {
			t.Fatal("completed apply terminal record was rewritten by a refused replay")
		}
	})
}

// A drifted terminal never authorizes a different repair through the same plan.
func TestLaunchdTerminalRefusalKeepsReviewedPlanClosedToReapply(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	launchdTerminalDriftAt(t, f, "bootstrap:after_receipt", func() { f.loaded = false }, nil)
	if _, err := f.apply(); err == nil {
		t.Fatal("terminal drift completed")
	}
	// Restoring the exact final state lets an idempotent replay of the same
	// reviewed plan finish without repeating any OS effect.
	f.loaded = true
	result, err := f.apply()
	if err != nil || result.Outcome != "completed" {
		t.Fatalf("exact replay after drift refused: %+v %v", result, err)
	}
	if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
		t.Fatalf("recovery replay repeated OS effects: %v", f.effects)
	}
	var terminal LaunchdRepairResult
	if err = readLaunchdArtifact(f.home, f.plan.ID+".apply.terminal.json", &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != "completed" || len(terminal.Phases) != 3 {
		t.Fatalf("recovery terminal does not bind the exact phase chain: %+v", terminal)
	}
}
