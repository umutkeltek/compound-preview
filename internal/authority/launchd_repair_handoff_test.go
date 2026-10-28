package authority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/work"
)

func completedLaunchdPredecessor(t *testing.T) *adversarialLiveFixture {
	t.Helper()
	f := newAdversarialLiveFixture(t)
	r, err := f.apply()
	if err != nil {
		t.Fatal(err)
	}
	// Real verification may legitimately advance several work generations after
	// the last OS phase. Terminal close must authenticate that newer envelope.
	for _, id := range []string{"ver_example_first", "ver_example_second"} {
		f.now = f.now.Add(time.Second)
		if _, err = f.store.Advance(f.plan.WorkID, work.AdvanceInput{To: "verifying", Actor: f.plan.Actor, VerificationID: id}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(time.Second)
	closed, err := f.store.Close(f.plan.WorkID, work.CloseInput{Outcome: "completed", MoveType: "control_plane_repair", Actor: f.plan.Actor, Changed: true, Summary: "synthetic exact repair verified", ProofRefs: []string{r.Phases[2].ReceiptID}}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.store.ReadTerminalCloseProof(f.plan.WorkID)
	if err != nil || proof.ReceiptID != closed.Receipt.ID || proof.WorkSHA256 == r.Phases[2].WorkSHA256 {
		t.Fatalf("wrong terminal proof: %+v %v", proof, err)
	}
	// Separately authorized quiescence is simulated outside the repair owner.
	f.disabled, f.loaded = true, false
	base := f.f.env.Run
	f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
		if exe == "/usr/bin/plutil" {
			b, err := os.ReadFile(args[len(args)-1])
			return LaunchdCommandResult{Stdout: string(b)}, err
		}
		return base(ctx, exe, args...)
	}
	return f
}

func launchdSuccessorPlan(t *testing.T, f *adversarialLiveFixture, old LaunchdRepairPlan, explicit bool) (LaunchdRepairPlan, error) {
	t.Helper()
	f.now = f.now.Add(time.Second)
	w, err := f.store.Create(work.CreateInput{Owner: old.Actor, Actor: old.Actor, State: "ready", SuccessPredicate: "successor exact synthetic observer"}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Claim(w.ID, old.Actor, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: old.Actor}, f.now); err != nil {
		t.Fatal(err)
	}
	args := append([]string{}, old.CandidateArgs...)
	args = append(args, "--limit", "2")
	f.f.after["ProgramArguments"] = args
	adversarialWriteJSON(t, f.f.candidate, f.f.after)
	if explicit {
		return BuildLaunchdRepairPlanWithHandoff(context.Background(), f.home, f.f.env, w.ID, old.Actor, old.PlistPath, f.f.candidate, f.f.proofs, old.ID)
	}
	return BuildLaunchdRepairPlan(context.Background(), f.f.env, w.ID, old.Actor, old.PlistPath, f.f.candidate, f.f.proofs)
}

func recordLaunchdSuccessor(t *testing.T, f *adversarialLiveFixture, p LaunchdRepairPlan) {
	t.Helper()
	f.plan = p
	for i := range f.cfg.StandingGrants {
		if f.cfg.StandingGrants[i].ID == "grant_example_launchd" {
			f.cfg.StandingGrants[i].Destination = launchdPlanDestination(p)
		}
	}
	f.writeConfig(t)
	if err := RecordLaunchdRepairPlan(f.home, p, f.now); err != nil {
		t.Fatal(err)
	}
	f.review = f.sealReview(t, false)
}

func snapshotLaunchdHistory(t *testing.T, f *adversarialLiveFixture, old LaunchdRepairPlan) map[string]string {
	t.Helper()
	path, err := launchdArtifactPath(f.home, old.ID+".plan.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), old.ID+".") || strings.HasPrefix(e.Name(), "ldrr_") {
			b, err := os.ReadFile(filepath.Join(filepath.Dir(path), e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			result[e.Name()] = string(b)
		}
	}
	return result
}

func TestLaunchdTerminalHandoffApplyAndRollbackPreserveHistory(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	history := snapshotLaunchdHistory(t, f, old)
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateLaunchdPlan(p); err != nil {
		t.Fatal(err)
	}
	recordLaunchdSuccessor(t, f, p)
	r, err := f.apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Phases) != 4 || r.Phases[0].Outcome != "completed" {
		t.Fatalf("unreceipted handoff: %+v", r)
	}
	owner, err := readLaunchdOwner(f.home, p)
	if err != nil || !bytesEqual(owner, launchdOwnerBytes(p.ID)) {
		t.Fatalf("wrong successor owner: %s %v", owner, err)
	}
	if _, err = f.apply(); err != nil {
		t.Fatalf("idempotent successor replay: %v", err)
	}
	rr := f.sealReview(t, true)
	r, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = readLaunchdOwner(f.home, p)
	if err != nil || !bytesEqual(owner, p.Handoff.Owner) {
		t.Fatalf("predecessor ownership not restored: %s %v", owner, err)
	}
	b, err := os.ReadFile(p.PlistPath)
	if err != nil || !bytesEqual(b, old.Candidate) || f.loaded || !f.disabled || len(r.Phases) != 4 {
		t.Fatalf("rollback did not restore original quiescent state: %+v %v", r, err)
	}
	for name, want := range history {
		path, _ := launchdArtifactPath(f.home, name)
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("predecessor history changed: %s %v", name, err)
		}
	}
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err != nil {
		t.Fatalf("rollback receipt replay: %v", err)
	}
	if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap", "enable", "bootstrap", "bootout", "disable"}) {
		t.Fatalf("effect repeated: %v", f.effects)
	}
	if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
		t.Fatalf("authenticated completed rollback must re-admit predecessor ownership: %v", err)
	}
}

func TestLaunchdTerminalHandoffRequiresExplicitPredecessorAndConsumedApproval(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, explicit)
			if err != nil {
				t.Fatal(err)
			}
			if !explicit {
				recordLaunchdSuccessor(t, f, p)
				if _, err = f.apply(); err == nil {
					t.Fatal("implicit predecessor accepted")
				}
			} else {
				f.plan = p
				for i := range f.cfg.StandingGrants {
					if f.cfg.StandingGrants[i].ID == "grant_example_launchd" {
						f.cfg.StandingGrants[i].Destination = launchdPlanDestination(p)
					}
				}
				f.writeConfig(t)
				if err = RecordLaunchdRepairPlan(f.home, p, f.now); err != nil {
					t.Fatal(err)
				}
				r, err := ReviewLaunchdRepairPlan(f.home, p.ID, "grant_example_launchd", "operator:fixture", "fixture:handoff", f.now.Add(time.Minute), f.now)
				if err != nil || !r.Pending || r.Review != nil {
					t.Fatalf("unconsumed approval sealed review: %+v %v", r, err)
				}
				if _, err = ApplyLaunchdRepair(context.Background(), f.home, p.ID, r.ActionIntentID, p.Actor, f.f.env); err == nil {
					t.Fatal("unapproved effect accepted")
				}
			}
			owner, _ := readLaunchdOwner(f.home, p)
			if !bytesEqual(owner, launchdOwnerBytes(old.ID)) || len(f.effects) != 2 {
				t.Fatalf("unapproved handoff changed state: %s %v", owner, f.effects)
			}
		})
	}
}

func TestLaunchdTerminalHandoffRejectsNonterminalFailedAndAmbiguousPredecessor(t *testing.T) {
	for _, kind := range []string{"running", "failed", "partial", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdversarialLiveFixture(t)
			if kind == "partial" || kind == "ambiguous" {
				f.f.env.Checkpoint = func(point string) error {
					if point == "replace:after_effect" {
						return errors.New("crash")
					}
					return nil
				}
				if _, err := f.apply(); err == nil {
					t.Fatal("expected injected crash")
				}
				f.f.env.Checkpoint = nil
				if kind == "partial" {
					if _, err := f.apply(); err == nil {
						t.Fatal("ambiguous phase resumed")
					}
				}
			} else {
				if _, err := f.apply(); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "failed" {
				if _, err := f.store.Close(f.plan.WorkID, work.CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: f.plan.Actor, Summary: "fixture failure"}, f.now); err != nil {
					t.Fatal(err)
				}
			}
			f.disabled, f.loaded = true, false
			base := f.f.env.Run
			f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
				if exe == "/usr/bin/plutil" {
					b, err := os.ReadFile(args[len(args)-1])
					return LaunchdCommandResult{Stdout: string(b)}, err
				}
				return base(ctx, exe, args...)
			}
			before := len(f.effects)
			if _, err := launchdSuccessorPlan(t, f, f.plan, true); err == nil {
				t.Fatal("unsafe predecessor accepted")
			}
			if len(f.effects) != before {
				t.Fatal("planning produced effects")
			}
		})
	}
}

func TestLaunchdTerminalHandoffCrashNeverRepeatsOwnerCASOrStartsUnprovedOSRepair(t *testing.T) {
	for _, boundary := range []string{"before_effect", "after_effect", "before_receipt", "after_receipt"} {
		t.Run(boundary, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			f.f.env.Checkpoint = func(point string) error {
				if point == "handoff:"+boundary {
					return errors.New("crash")
				}
				return nil
			}
			if _, err = f.apply(); err == nil {
				t.Fatal("crash did not fire")
			}
			owner, _ := readLaunchdOwner(f.home, p)
			expectedOwner := launchdOwnerBytes(p.ID)
			if boundary == "before_effect" {
				expectedOwner = p.Handoff.Owner
			}
			if !bytesEqual(owner, expectedOwner) || len(f.effects) != 2 {
				t.Fatal("wrong ownership boundary")
			}
			f.f.env.Checkpoint = nil
			r, err := f.apply()
			if boundary == "before_effect" || boundary == "after_effect" {
				if err == nil || len(f.effects) != 2 || len(r.Phases) != 1 || r.Phases[0].Outcome != "failed" {
					t.Fatalf("unproved ownership continued: %+v %v", r, err)
				}
				if boundary == "before_effect" {
					if _, err = launchdSuccessorPlan(t, f, old, true); err == nil {
						t.Fatal("unresolved started handoff admitted a third successor")
					}
				}
				rr := f.sealReview(t, true)
				if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err != nil {
					t.Fatal(err)
				}
				owner, _ = readLaunchdOwner(f.home, p)
				if !bytesEqual(owner, p.Handoff.Owner) {
					t.Fatal("ambiguous owner not restored by fresh rollback")
				}
				if boundary == "before_effect" {
					if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
						t.Fatalf("native no-effect rollback left permanent owner blocker: %v", err)
					}
					var failed launchdPhaseCommit
					if readLaunchdArtifact(f.home, launchdPhaseStem(p, "apply", 3)+".committed.json", &failed) != nil || !reflect.DeepEqual(failed, r.Phases[0]) || failed.Outcome != "failed" {
						t.Fatal("no-effect recovery replaced the actual failed handoff")
					}
					var noop launchdPhaseOutcome
					if readLaunchdArtifact(f.home, launchdPhaseStem(p, "rollback", 0)+".outcome.json", &noop) != nil || noop.Final.Outcome != "completed" || noop.Final.Changed {
						t.Fatal("no-effect rollback invented a changed owner")
					}
				}
			} else if err != nil || len(f.effects) != 4 || len(r.Phases) != 4 {
				t.Fatalf("sealed ownership not recoverable: %+v %v", r, err)
			}
		})
	}
}

func TestLaunchdTerminalHandoffRejectsTamperedPredecessorProofs(t *testing.T) {
	for _, kind := range []string{"terminal", "phase-receipt", "close-envelope", "active-owner", "consumed-review"} {
		t.Run(kind, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			switch kind {
			case "terminal":
				name := old.ID + ".apply.terminal.json"
				var r LaunchdRepairResult
				if err := readLaunchdArtifact(f.home, name, &r); err != nil {
					t.Fatal(err)
				}
				r.Runtime.Program = "/example/foreign"
				path, _ := launchdArtifactPath(f.home, name)
				adversarialWriteJSON(t, path, r)
			case "phase-receipt":
				name := launchdPhaseStem(old, "apply", 1) + ".committed.json"
				var c launchdPhaseCommit
				if err := readLaunchdArtifact(f.home, name, &c); err != nil {
					t.Fatal(err)
				}
				c.ReceiptID = "rec_unrelated"
				path, _ := launchdArtifactPath(f.home, name)
				adversarialWriteJSON(t, path, c)
			case "close-envelope":
				w, err := f.store.Get(old.WorkID)
				if err != nil {
					t.Fatal(err)
				}
				w.SuccessPredicate = "forged terminal envelope"
				adversarialWriteJSON(t, filepath.Join(f.home, "work", "work", w.ID+".json"), w)
			case "active-owner":
				path, _ := launchdArtifactPath(f.home, launchdActiveName(old))
				adversarialWriteJSON(t, path, map[string]string{"plan_id": "ldrp_unrelated"})
			case "consumed-review":
				r := f.review
				r.Decision.ID = "pol_unrelated"
				path, _ := launchdArtifactPath(f.home, r.ID+".review.json")
				adversarialWriteJSON(t, path, r)
			}
			if _, err := launchdSuccessorPlan(t, f, old, true); err == nil {
				t.Fatal("tampered predecessor accepted")
			}
			if len(f.effects) != 2 {
				t.Fatal("proof validation produced effects")
			}
		})
	}
}

func TestLaunchdTerminalHandoffRechecksReviewedProofAndLiveDriftBeforeCAS(t *testing.T) {
	for _, kind := range []string{"source", "bytes", "mode", "enabled", "loaded", "foreign-override", "path", "work", "policy", "predecessor-close"} {
		t.Run(kind, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			switch kind {
			case "source":
				if err = os.WriteFile(p.SourceProofs[1].Path, []byte("changed source"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bytes":
				if err = os.WriteFile(p.PlistPath, []byte("foreign argv bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err = os.Chmod(p.PlistPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "enabled":
				f.disabled = false
			case "loaded":
				f.loaded = true
			case "foreign-override":
				base := f.f.env.Run
				f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
					r, e := base(ctx, exe, args...)
					if exe == "/bin/launchctl" && args[0] == "print-disabled" {
						r.Stdout = strings.ReplaceAll(r.Stdout, "\"org.example.unrelated\" => true", "\"org.example.unrelated\" => false")
					}
					return r, e
				}
			case "path":
				if err = os.Rename(p.PlistPath, p.PlistPath+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(p.PlistPath+".foreign", p.PlistPath); err != nil {
					t.Fatal(err)
				}
			case "work":
				f.now = f.now.Add(time.Second)
				if _, err = f.store.Advance(p.WorkID, work.AdvanceInput{To: "running", Actor: p.Actor, Note: "unrelated generation"}, f.now); err != nil {
					t.Fatal(err)
				}
			case "policy":
				f.cfg.StandingGrants = append(f.cfg.StandingGrants, policy.StandingGrant{ID: "grant_unrelated", Capability: "fixture.inspect", MutationClass: "local_reversible", ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339)})
				f.writeConfig(t)
			case "predecessor-close":
				w, err := f.store.Get(old.WorkID)
				if err != nil {
					t.Fatal(err)
				}
				w.SuccessPredicate = "changed after exact successor review"
				adversarialWriteJSON(t, filepath.Join(f.home, "work", "work", w.ID+".json"), w)
			}
			if _, err = f.apply(); err == nil {
				t.Fatal("drift permitted ownership CAS")
			}
			owner, _ := readLaunchdOwner(f.home, p)
			if !bytesEqual(owner, p.Handoff.Owner) || len(f.effects) != 2 {
				t.Fatalf("drift changed owner or OS: %s %v", owner, f.effects)
			}
		})
	}
}

func TestLaunchdTerminalHandoffConcurrentSuccessorsChooseOneOwner(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	first, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []LaunchdRepairPlan{first, second} {
		f.cfg.StandingGrants = append(f.cfg.StandingGrants, policy.StandingGrant{ID: fmt.Sprintf("grant_successor_%d", i), Capability: LaunchdRepairCapability, MutationClass: "external_side_effect", TargetPath: p.PlistPath, Destination: launchdPlanDestination(p), BlastRadius: "narrow", ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339)})
		if err = RecordLaunchdRepairPlan(f.home, p, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.writeConfig(t)
	// The fake readback follows actual file bytes, independently of which
	// competing plan wins. The label lock serializes the fake OS state too.
	base := f.f.env.Run
	f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
		if exe == "/bin/launchctl" && args[0] == "print" && f.loaded {
			b, err := os.ReadFile(old.PlistPath)
			if err != nil {
				return LaunchdCommandResult{}, err
			}
			var plist struct {
				Args []string `json:"ProgramArguments"`
			}
			if err = json.Unmarshal(b, &plist); err != nil {
				return LaunchdCommandResult{}, err
			}
			var out strings.Builder
			fmt.Fprintf(&out, "%s/%s = {\npath = %s\nprogram = %s\narguments = {\n", old.Domain, old.Label, old.PlistPath, plist.Args[0])
			for _, a := range plist.Args {
				fmt.Fprintln(&out, a)
			}
			fmt.Fprintf(&out, "}\ndomain = %s [123]\n}\n", old.Domain)
			return LaunchdCommandResult{Stdout: out.String()}, nil
		}
		return base(ctx, exe, args...)
	}
	a, b := *f, *f
	a.plan = first
	b.plan = second
	a.review = a.sealReviewGrant(t, false, "grant_successor_0")
	b.review = b.sealReviewGrant(t, false, "grant_successor_1")
	errs := make(chan error, 2)
	go func() { _, err := a.apply(); errs <- err }()
	go func() { _, err := b.apply(); errs <- err }()
	success := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			success++
		}
	}
	owner, err := readLaunchdOwner(f.home, first)
	if success != 1 || err != nil || (!bytesEqual(owner, launchdOwnerBytes(first.ID)) && !bytesEqual(owner, launchdOwnerBytes(second.ID))) || len(f.effects) != 4 {
		t.Fatalf("successors did not serialize: successes=%d owner=%s effects=%v err=%v", success, owner, f.effects, err)
	}
}

func TestLaunchdTerminalHandoffRestoreCrashBlocksThirdSuccessorUntilNativeCompletion(t *testing.T) {
	for _, boundary := range []string{"after_effect", "before_receipt", "after_receipt"} {
		t.Run(boundary, func(t *testing.T) {
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
			f.f.env.Checkpoint = func(point string) error {
				if point == "restore_owner:"+boundary {
					return errors.New("crash after owner restoration")
				}
				return nil
			}
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
				t.Fatal("restoration crash did not fire")
			}
			owner, _ := readLaunchdOwner(f.home, p)
			if !bytesEqual(owner, p.Handoff.Owner) {
				t.Fatal("crash did not reach pointer restoration")
			}
			before := len(f.effects)
			if _, err = launchdSuccessorPlan(t, f, old, true); err == nil {
				t.Fatal("third successor accepted unsealed restored ownership")
			}
			f.f.env.Checkpoint = nil
			_, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env)
			if boundary == "after_effect" {
				if err == nil {
					t.Fatal("ambiguous restoration replay succeeded")
				}
				if _, err = launchdSuccessorPlan(t, f, old, true); err == nil {
					t.Fatal("third successor accepted receipted ambiguity")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
					t.Fatalf("completed native restoration did not reopen ownership: %v", err)
				}
			}
			if len(f.effects) != before {
				t.Fatal("restoration replay or planning repeated OS effects")
			}
		})
	}
}

func TestLaunchdTerminalHandoffUnexpectedOwnerReadbackCannotSucceed(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	recordLaunchdSuccessor(t, f, p)
	f.f.env.Checkpoint = func(point string) error {
		if point == "handoff:after_effect" {
			path, _ := launchdArtifactPath(f.home, launchdActiveName(p))
			adversarialWriteJSON(t, path, map[string]string{"plan_id": "ldrp_foreign"})
		}
		return nil
	}
	r, err := f.apply()
	if err == nil || len(r.Phases) != 1 || r.Phases[0].Outcome != "failed" || len(f.effects) != 2 {
		t.Fatalf("unexpected owner acquired successful receipt: %+v %v", r, err)
	}
}

func TestLaunchdTerminalHandoffMissingStartCannotHideLaterOwnershipJournals(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			if applied {
				if _, err = f.apply(); err != nil {
					t.Fatal(err)
				}
				rr := f.sealReview(t, true)
				f.f.env.Checkpoint = func(point string) error {
					if point == "restore_owner:after_effect" {
						return errors.New("restore CAS crash")
					}
					return nil
				}
				if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
					t.Fatal("crash did not fire")
				}
				path, _ := launchdArtifactPath(f.home, launchdPhaseStem(p, "apply", 3)+".started.json")
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = launchdSuccessorPlan(t, f, old, true)
			if applied && err == nil {
				t.Fatal("missing start hid later ownership and rollback journals")
			}
			if !applied && err != nil {
				t.Fatalf("genuinely planned successor blocked ownership: %v", err)
			}
		})
	}
}

func TestLaunchdTerminalHandoffRestoredOwnerRequiresAuthenticOriginalHandoff(t *testing.T) {
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
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err != nil {
		t.Fatal(err)
	}
	stem := launchdPhaseStem(p, "apply", 3)
	var start launchdPhaseStart
	if err = readLaunchdArtifact(f.home, stem+".started.json", &start); err != nil {
		t.Fatal(err)
	}
	start.BeforeOwner = []byte("forged earlier owner")
	start.Digest = ""
	start.Digest = launchdDigest(start)
	path, _ := launchdArtifactPath(f.home, stem+".started.json")
	adversarialWriteJSON(t, path, start)
	if _, err = launchdSuccessorPlan(t, f, old, true); err == nil {
		t.Fatal("authenticated rollback hid a tampered earlier handoff start")
	}
}

func TestLaunchdTerminalHandoffNoEffectRollbackRefusesMisclassification(t *testing.T) {
	for _, kind := range []string{"changed-owner", "later-os-phase", "tampered-native"} {
		t.Run(kind, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			boundary := "before_effect"
			if kind == "changed-owner" {
				boundary = "after_effect"
			}
			f.f.env.Checkpoint = func(point string) error {
				if point == "handoff:"+boundary {
					return errors.New("handoff crash")
				}
				return nil
			}
			if _, err = f.apply(); err == nil {
				t.Fatal("crash did not fire")
			}
			f.f.env.Checkpoint = nil
			if _, err = f.apply(); err == nil {
				t.Fatal("ambiguous handoff was relabelled success")
			}
			rr := f.sealReview(t, true)
			switch kind {
			case "changed-owner":
				path, _ := launchdArtifactPath(f.home, launchdActiveName(p))
				if err = os.WriteFile(path, p.Handoff.Owner, 0600); err != nil {
					t.Fatal(err)
				}
			case "later-os-phase":
				path, _ := launchdArtifactPath(f.home, launchdPhaseStem(p, "apply", 0)+".started.json")
				adversarialWriteJSON(t, path, map[string]string{"ambiguous": "later replacement"})
			case "tampered-native":
				name := launchdPhaseStem(p, "apply", 3) + ".committed.json"
				var commit launchdPhaseCommit
				if err = readLaunchdArtifact(f.home, name, &commit); err != nil {
					t.Fatal(err)
				}
				commit.ReceiptID = "rec_forged"
				path, _ := launchdArtifactPath(f.home, name)
				adversarialWriteJSON(t, path, commit)
			}
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
				t.Fatal("unsafe phase misclassified as no-effect restoration")
			}
			if len(f.effects) != 2 {
				t.Fatal("misclassification caused OS effects")
			}
		})
	}
}

func TestLaunchdTerminalHandoffNoEffectRollbackObservationCanResume(t *testing.T) {
	for _, boundary := range []string{"before_effect", "after_effect"} {
		t.Run(boundary, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			f.f.env.Checkpoint = func(point string) error {
				if point == "handoff:before_effect" {
					return errors.New("pre-CAS crash")
				}
				return nil
			}
			if _, err = f.apply(); err == nil {
				t.Fatal("handoff crash did not fire")
			}
			f.f.env.Checkpoint = nil
			if _, err = f.apply(); err == nil {
				t.Fatal("failed handoff lost actual failure")
			}
			rr := f.sealReview(t, true)
			f.f.env.Checkpoint = func(point string) error {
				if point == "restore_owner:"+boundary {
					return errors.New("no-effect observation interrupted")
				}
				return nil
			}
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
				t.Fatal("observation crash did not fire")
			}
			f.f.env.Checkpoint = nil
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err != nil {
				t.Fatalf("read-only no-effect observation cannot recover: %v", err)
			}
			if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
				t.Fatalf("observation crash left permanent ownership blocker: %v", err)
			}
			if len(f.effects) != 2 {
				t.Fatal("read-only observation replay caused OS effects")
			}
		})
	}
}

func TestLaunchdTerminalHandoffNoEffectObservationRechecksExpiredAuthority(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	recordLaunchdSuccessor(t, f, p)
	f.f.env.Checkpoint = func(point string) error {
		if point == "handoff:before_effect" {
			return errors.New("pre-CAS crash")
		}
		return nil
	}
	if _, err = f.apply(); err == nil {
		t.Fatal("handoff crash did not fire")
	}
	f.f.env.Checkpoint = nil
	if _, err = f.apply(); err == nil {
		t.Fatal("actual handoff failure lost")
	}
	rr := f.sealReview(t, true)
	f.f.env.Checkpoint = func(point string) error {
		if point == "restore_owner:before_effect" {
			return errors.New("no-effect observation interrupted")
		}
		return nil
	}
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("observation crash did not fire")
	}
	f.f.env.Checkpoint = nil
	f.now = f.now.Add(16 * time.Minute)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("read-only observation bypassed expired exact rollback review")
	}
	owner, _ := readLaunchdOwner(f.home, p)
	if !bytesEqual(owner, p.Handoff.Owner) || len(f.effects) != 2 {
		t.Fatal("expired recovery changed owner or OS")
	}
}

func TestLaunchdTerminalHandoffExpiredNoEffectRollbackAcceptsFreshExactRecovery(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	recordLaunchdSuccessor(t, f, p)
	f.f.env.Checkpoint = func(point string) error {
		if point == "handoff:before_effect" {
			return errors.New("pre-CAS crash")
		}
		return nil
	}
	if _, err = f.apply(); err == nil {
		t.Fatal("handoff crash did not fire")
	}
	f.f.env.Checkpoint = nil
	if _, err = f.apply(); err == nil {
		t.Fatal("failed handoff lost actual failure")
	}
	rr := f.sealReview(t, true)
	f.f.env.Checkpoint = func(point string) error {
		if point == "restore_owner:before_effect" {
			return errors.New("no-effect observation interrupted")
		}
		return nil
	}
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("observation crash did not fire")
	}
	f.f.env.Checkpoint = nil
	f.now = f.now.Add(16 * time.Minute)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("expired exact review was bypassed")
	}
	stem := launchdPhaseStem(p, "rollback", 0)
	var failedStart launchdPhaseStart
	var failedOutcome launchdPhaseOutcome
	var failed launchdPhaseCommit
	for _, item := range []struct {
		name  string
		value any
	}{{stem + ".started.json", &failedStart}, {stem + ".outcome.json", &failedOutcome}, {stem + ".committed.json", &failed}} {
		if err = readLaunchdArtifact(f.home, item.name, item.value); err != nil {
			t.Fatal(err)
		}
	}
	if err = verifyLaunchdCommittedPhase(f.home, p, failedStart, failedOutcome, failed); err != nil || failed.Outcome != "failed" || failedOutcome.Final.Changed {
		t.Fatalf("expired failure was not authentically receipted unchanged: %+v %v", failed, err)
	}
	t.Logf("expired no-effect rollback has immutable failed receipt=%s native_digest=%s cause=%s", failed.ReceiptID, failed.NativeDigest, failedOutcome.Failure)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("old failed rollback unexpectedly resumed")
	} else {
		t.Logf("old review continuation refuses: %v", err)
	}
	if _, err = launchdSuccessorPlan(t, f, old, true); err == nil {
		t.Fatal("third successor bypassed incomplete rollback")
	} else {
		t.Logf("new native successor refuses: %v", err)
	}
	const freshGrant = "grant_fresh_no_effect_recovery"
	// Rotate the exact row; leaving two matching live grants would correctly
	// retain the policy engine's existing first-match grant identity fence.
	var retainedGrants []policy.StandingGrant
	for _, grant := range f.cfg.StandingGrants {
		if grant.ID != "grant_example_launchd" {
			retainedGrants = append(retainedGrants, grant)
		}
	}
	f.cfg.StandingGrants = retainedGrants
	f.cfg.StandingGrants = append(f.cfg.StandingGrants, policy.StandingGrant{ID: freshGrant, Capability: LaunchdRepairCapability, MutationClass: "external_side_effect", TargetPath: p.PlistPath, Destination: launchdPlanDestination(p), BlastRadius: "narrow", ExpiresAt: f.now.Add(40 * time.Minute).Format(time.RFC3339)})
	f.writeConfig(t)
	// This must create and consume a new exact approval through the existing
	// native review door, preserving both failed journals and receipts.
	fresh := f.sealReviewGrant(t, true, freshGrant)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, fresh.ID, p.Actor, f.f.env); err != nil {
		t.Fatalf("fresh exact recovery cannot complete: %v", err)
	}
	var retained launchdPhaseCommit
	if err = readLaunchdArtifact(f.home, stem+".committed.json", &retained); err != nil || !reflect.DeepEqual(retained, failed) {
		t.Fatal("fresh recovery rewrote actual failed rollback")
	}
	if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
		t.Fatalf("fresh recovery left permanent ownership blocker: %v", err)
	}
	if len(f.effects) != 2 {
		t.Fatal("no-effect recovery caused OS effects")
	}
}

func expiredNoEffectContinuationFixture(t *testing.T) (*adversarialLiveFixture, LaunchdRepairPlan) {
	t.Helper()
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	recordLaunchdSuccessor(t, f, p)
	f.f.env.Checkpoint = func(point string) error {
		if point == "handoff:before_effect" {
			return errors.New("pre-CAS crash")
		}
		return nil
	}
	if _, err = f.apply(); err == nil {
		t.Fatal("handoff crash did not fire")
	}
	f.f.env.Checkpoint = nil
	if _, err = f.apply(); err == nil {
		t.Fatal("failed handoff lost actual failure")
	}
	rr := f.sealReview(t, true)
	f.f.env.Checkpoint = func(point string) error {
		if point == "restore_owner:before_effect" {
			return errors.New("observation interrupted")
		}
		return nil
	}
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("observation crash did not fire")
	}
	f.f.env.Checkpoint = nil
	f.now = f.now.Add(16 * time.Minute)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, rr.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("expired original review was bypassed")
	}
	return f, old
}

func rotateNoEffectContinuationGrant(t *testing.T, f *adversarialLiveFixture, id string) {
	t.Helper()
	var grants []policy.StandingGrant
	for _, g := range f.cfg.StandingGrants {
		if g.Capability != LaunchdRepairCapability || g.Destination != launchdPlanDestination(f.plan) {
			grants = append(grants, g)
		}
	}
	f.cfg.StandingGrants = append(grants, policy.StandingGrant{ID: id, Capability: LaunchdRepairCapability, MutationClass: "external_side_effect", TargetPath: f.plan.PlistPath, Destination: launchdPlanDestination(f.plan), BlastRadius: "narrow", ExpiresAt: f.now.Add(40 * time.Minute).Format(time.RFC3339)})
	f.writeConfig(t)
}

func TestLaunchdNoEffectContinuationRefusesDriftAndMissingApproval(t *testing.T) {
	for _, kind := range []string{"owner", "file", "loaded", "enabled", "foreign-override", "tampered-prefix", "missing-prefix", "unknown-suffix", "missing-consumed-approval"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := expiredNoEffectContinuationFixture(t)
			p := f.plan
			rotateNoEffectContinuationGrant(t, f, "grant_fresh_no_effect")
			var review LaunchdRepairReview
			if kind == "missing-consumed-approval" {
				r, err := ReviewLaunchdRepairRollbackPlan(f.home, p.ID, "grant_fresh_no_effect", "operator:fixture", "fixture:independent-launchd-review", f.now.Add(15*time.Minute), f.now)
				if err != nil || !r.Pending || r.Review != nil {
					t.Fatalf("unconsumed continuation sealed: %+v %v", r, err)
				}
				prefix := strings.TrimSuffix(r.IntentPath, ".review-intent.json")
				var req LaunchdReviewRequest
				var proposal launchdReviewProposal
				if err = readLaunchdArtifact(f.home, filepath.Base(prefix)+".review-request.json", &req); err != nil {
					t.Fatal(err)
				}
				if err = readLaunchdArtifact(f.home, filepath.Base(prefix)+".review-proposal.json", &proposal); err != nil {
					t.Fatal(err)
				}
				w, err := f.store.Get(p.WorkID)
				if err != nil {
					t.Fatal(err)
				}
				review = LaunchdRepairReview{Request: req, Decision: proposal.Decision, WorkSHA256: launchdWorkDigest(w)}
				review.ID = "ldrr_" + launchdDigest(review)[:24]
				if err = writeLaunchdArtifact(f.home, review.ID+".review.json", review); err != nil {
					t.Fatal(err)
				}
			} else {
				review = f.sealReviewGrant(t, true, "grant_fresh_no_effect")
			}
			ownerPath, _ := launchdArtifactPath(f.home, launchdActiveName(p))
			switch kind {
			case "owner":
				if err := os.WriteFile(ownerPath, launchdOwnerBytes(p.ID), 0600); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(p.PlistPath, []byte("foreign exact file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "loaded":
				f.loaded = true
			case "enabled":
				f.disabled = false
			case "foreign-override":
				base := f.f.env.Run
				f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
					r, e := base(ctx, exe, args...)
					if exe == "/bin/launchctl" && args[0] == "print-disabled" {
						r.Stdout = strings.ReplaceAll(r.Stdout, "\"org.example.unrelated\" => true", "\"org.example.unrelated\" => false")
					}
					return r, e
				}
			case "tampered-prefix":
				name := launchdPhaseStem(p, "rollback", 0) + ".committed.json"
				var c launchdPhaseCommit
				if err := readLaunchdArtifact(f.home, name, &c); err != nil {
					t.Fatal(err)
				}
				c.NativeDigest = "forged"
				path, _ := launchdArtifactPath(f.home, name)
				adversarialWriteJSON(t, path, c)
			case "missing-prefix":
				path, _ := launchdArtifactPath(f.home, launchdPhaseStem(p, "rollback", 0)+".outcome.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "unknown-suffix":
				path, _ := launchdArtifactPath(f.home, p.ID+".rollback.0.unknown.json")
				adversarialWriteJSON(t, path, map[string]string{"unknown": "phase"})
			}
			ownerBefore, err := os.Stat(ownerPath)
			if err != nil {
				t.Fatal(err)
			}
			ownerBytes, err := os.ReadFile(ownerPath)
			if err != nil {
				t.Fatal(err)
			}
			fileBefore, err := os.Stat(p.PlistPath)
			if err != nil {
				t.Fatal(err)
			}
			fileBytes, err := os.ReadFile(p.PlistPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env); err == nil {
				t.Fatal("unsafe continuation succeeded")
			}
			ownerAfter, _ := os.Stat(ownerPath)
			gotOwner, _ := os.ReadFile(ownerPath)
			fileAfter, _ := os.Stat(p.PlistPath)
			gotFile, _ := os.ReadFile(p.PlistPath)
			if !os.SameFile(ownerBefore, ownerAfter) || !bytesEqual(ownerBytes, gotOwner) || !os.SameFile(fileBefore, fileAfter) || !bytesEqual(fileBytes, gotFile) || len(f.effects) != 2 {
				t.Fatal("refused continuation performed CAS/file/OS effects")
			}
		})
	}
}

func TestLaunchdNoEffectContinuationRepeatedExpiryAndStaleReviews(t *testing.T) {
	f, old := expiredNoEffectContinuationFixture(t)
	p := f.plan
	ownerPath, _ := launchdArtifactPath(f.home, launchdActiveName(p))
	ownerBefore, err := os.Stat(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	fileBefore, err := os.Stat(p.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	var failures []launchdPhaseCommit
	var stale []LaunchdRepairReview
	var initial launchdPhaseCommit
	if err = readLaunchdArtifact(f.home, launchdPhaseStem(p, "rollback", 0)+".committed.json", &initial); err != nil {
		t.Fatal(err)
	}
	failures = append(failures, initial)
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("grant_no_effect_epoch_%d", i)
		rotateNoEffectContinuationGrant(t, f, id)
		review := f.sealReviewGrant(t, true, id)
		stale = append(stale, review)
		if review.Request.NoEffectContinuation == nil || review.Request.NoEffectContinuation.Index != i {
			t.Fatal("continuation skipped native failed prefix")
		}
		f.f.env.Checkpoint = func(point string) error {
			if point == "restore_owner:before_effect" {
				return errors.New("continuation interrupted")
			}
			return nil
		}
		if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env); err == nil {
			t.Fatal("continuation crash did not fire")
		}
		f.f.env.Checkpoint = nil
		f.now = f.now.Add(16 * time.Minute)
		if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env); err == nil {
			t.Fatal("expired continuation approval was bypassed")
		}
		var failed launchdPhaseCommit
		if err = readLaunchdArtifact(f.home, launchdPhaseStem(p, "rollback", i)+".committed.json", &failed); err != nil || failed.Outcome != "failed" {
			t.Fatalf("expired continuation did not retain actual failure: %v", err)
		}
		failures = append(failures, failed)
	}
	rotateNoEffectContinuationGrant(t, f, "grant_no_effect_final")
	fresh := f.sealReviewGrant(t, true, "grant_no_effect_final")
	for _, review := range stale {
		if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, review.ID, p.Actor, f.f.env); err == nil {
			t.Fatal("stale prefix review resumed")
		}
	}
	r, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, fresh.ID, p.Actor, f.f.env)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Phases) != 4 || !reflect.DeepEqual(r.Phases[:3], failures) || r.Phases[3].Outcome != "completed" {
		t.Fatalf("fresh continuation relabelled or lost failed prefix: %+v", r)
	}
	for i, want := range failures {
		var actual launchdPhaseCommit
		if readLaunchdArtifact(f.home, launchdPhaseStem(p, "rollback", i)+".committed.json", &actual) != nil || !reflect.DeepEqual(actual, want) {
			t.Fatal("actual failed native tuple changed")
		}
	}
	ownerAfter, _ := os.Stat(ownerPath)
	fileAfter, _ := os.Stat(p.PlistPath)
	if !os.SameFile(ownerBefore, ownerAfter) || !os.SameFile(fileBefore, fileAfter) || len(f.effects) != 2 {
		t.Fatal("observation continuations performed CAS/file/OS effects")
	}
	if _, err = launchdSuccessorPlan(t, f, old, true); err != nil {
		t.Fatalf("authenticated failed prefix plus successful tail did not admit successor: %v", err)
	}
}

func TestLaunchdNoEffectContinuationCanRenewUnstartedExpiredApproval(t *testing.T) {
	f, old := expiredNoEffectContinuationFixture(t)
	p := f.plan
	rotateNoEffectContinuationGrant(t, f, "grant_expiring_fresh")
	expired := f.sealReviewGrant(t, true, "grant_expiring_fresh")
	f.now = f.now.Add(16 * time.Minute)
	if _, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, expired.ID, p.Actor, f.f.env); err == nil {
		t.Fatal("expired fresh approval was bypassed")
	}
	rotateNoEffectContinuationGrant(t, f, "grant_renewed_fresh")
	fresh := f.sealReviewGrant(t, true, "grant_renewed_fresh")
	if _, err := RollbackLaunchdRepair(context.Background(), f.home, p.ID, fresh.ID, p.Actor, f.f.env); err != nil {
		t.Fatal(err)
	}
	if _, err := launchdSuccessorPlan(t, f, old, true); err != nil {
		t.Fatal(err)
	}
	if len(f.effects) != 2 {
		t.Fatal("fresh review renewal caused OS effects")
	}
}
