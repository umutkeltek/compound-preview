package authority

// Independent acceptance cases authored by the campaign reviewer, separately
// from the launchd implementation. All processes are synthetic; no user service
// is enabled, loaded, or stopped by this suite.
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

	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
	"gopkg.in/yaml.v3"
)

const adversarialLabel = "org.example.observer"

func adversarialAbsent(uid int) LaunchdCommandResult {
	return LaunchdCommandResult{ExitCode: 113, Stderr: fmt.Sprintf("Bad request.\nCould not find service %q in domain for user gui: %d\n", adversarialLabel, uid)}
}

func TestLaunchdAdversarialProbeRefusesAmbiguousEvidence(t *testing.T) {
	uid := os.Getuid()
	valid := LaunchdCommandResult{Stdout: fmt.Sprintf("disabled services = {\n\t%q => disabled\n\t\"org.example.unrelated\" => true\n}\n", adversarialLabel)}
	cases := []struct {
		name              string
		disabled, service LaunchdCommandResult
		err               error
	}{
		{"permission-denied", LaunchdCommandResult{ExitCode: 1, Stderr: "Operation not permitted"}, adversarialAbsent(uid), nil},
		{"truncated-map", LaunchdCommandResult{Stdout: "disabled services = {\n"}, adversarialAbsent(uid), nil},
		{"duplicate-label", LaunchdCommandResult{Stdout: fmt.Sprintf("disabled services = {\n%q => disabled\n%q => enabled\n}", adversarialLabel, adversarialLabel)}, adversarialAbsent(uid), nil},
		{"malformed-value", LaunchdCommandResult{Stdout: fmt.Sprintf("disabled services = {\n%q => perhaps\n}", adversarialLabel)}, adversarialAbsent(uid), nil},
		{"stderr-on-success", LaunchdCommandResult{Stdout: valid.Stdout, Stderr: "partial read"}, adversarialAbsent(uid), nil},
		{"service-permission", valid, LaunchdCommandResult{ExitCode: 113, Stderr: "Operation not permitted"}, nil},
		{"wrong-uid-absence", valid, adversarialAbsent(uid + 1), nil},
		{"wrong-label-absence", valid, LaunchdCommandResult{ExitCode: 113, Stderr: strings.ReplaceAll(adversarialAbsent(uid).Stderr, adversarialLabel, "org.example.other")}, nil},
		{"exit-zero-absence", valid, LaunchdCommandResult{Stderr: adversarialAbsent(uid).Stderr}, nil},
		{"transport-error", valid, adversarialAbsent(uid), errors.New("deadline exceeded")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := LaunchdEnvironment{UID: uid, Run: func(_ context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
				if exe != "/bin/launchctl" {
					t.Fatalf("unexpected executable %s", exe)
				}
				if args[0] == "print-disabled" {
					return tc.disabled, nil
				}
				if args[0] != "print" {
					t.Fatalf("unexpected effect %v", args)
				}
				return tc.service, tc.err
			}}
			r := ProbeLaunchdRuntime(context.Background(), env, adversarialLabel)
			if r.Error == "" || r.LoadedState != "unknown" {
				t.Fatalf("ambiguous evidence accepted: %+v", r)
			}
		})
	}
	env := LaunchdEnvironment{UID: uid, Run: func(_ context.Context, _ string, args ...string) (LaunchdCommandResult, error) {
		if args[0] == "print-disabled" {
			return valid, nil
		}
		return adversarialAbsent(uid), nil
	}}
	r := ProbeLaunchdRuntime(context.Background(), env, adversarialLabel)
	if r.Error != "" || r.LoadedState != "absent" || r.OverrideState != "disabled" || !r.ForeignOverrides["org.example.unrelated"] {
		t.Fatalf("exact evidence refused or foreign override lost: %+v", r)
	}
}

func TestLaunchdAdversarialLoadedProbeRejectsMalformedIdentity(t *testing.T) {
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	good := fmt.Sprintf("%s/%s = {\npath = /example/observer.plist\nprogram = /example/python\narguments = {\n/example/python\n/example/observer.py\nrun\n}\ndomain = %s [123]\n}\n", domain, adversarialLabel, domain)
	cases := map[string]string{
		"wrong-domain":          strings.Replace(good, domain+"/", fmt.Sprintf("gui/%d/", uid+1), 1),
		"wrong-label":           strings.Replace(good, adversarialLabel, "org.example.other", 1),
		"wrong-domain-field":    strings.Replace(good, "domain = "+domain, "domain = gui/999999", 1),
		"duplicate-path":        strings.Replace(good, "path = /example/observer.plist", "path = /example/observer.plist\npath = /example/other.plist", 1),
		"duplicate-program":     strings.Replace(good, "program = /example/python", "program = /example/python\nprogram = /example/other", 1),
		"duplicate-arguments":   strings.Replace(good, "arguments = {", "arguments = {\n}\narguments = {", 1),
		"truncated":             strings.TrimSuffix(good, "}\n"),
		"unclosed-quoted-value": strings.Replace(good, "path = /example/observer.plist", "path = \"/example/observer.plist", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			env := LaunchdEnvironment{UID: uid, Run: func(_ context.Context, _ string, args ...string) (LaunchdCommandResult, error) {
				if args[0] == "print-disabled" {
					return LaunchdCommandResult{Stdout: "disabled services = {\n}"}, nil
				}
				return LaunchdCommandResult{Stdout: body}, nil
			}}
			r := ProbeLaunchdRuntime(context.Background(), env, adversarialLabel)
			if r.Error == "" || r.LoadedState != "unknown" {
				t.Fatalf("malformed loaded identity accepted: %+v", r)
			}
		})
	}
}

type adversarialLaunchdFixture struct {
	env              LaunchdEnvironment
	plist, candidate string
	before, after    map[string]any
	proofs           []LaunchdSourceProof
}

func adversarialWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func newAdversarialLaunchdFixture(t *testing.T) *adversarialLaunchdFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f := &adversarialLaunchdFixture{plist: filepath.Join(dir, adversarialLabel+".plist"), candidate: filepath.Join(home, "candidate.plist")}
	exe, script := filepath.Join(home, "python"), filepath.Join(home, "observer.py")
	for _, p := range []string{exe, script} {
		b := []byte("synthetic source\n")
		if err = os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		f.proofs = append(f.proofs, LaunchdSourceProof{Path: p, SHA256: launchdBytesDigest(b)})
	}
	f.before = map[string]any{"Label": adversarialLabel, "ProgramArguments": []any{exe, script, "run", "--hours", "5"}, "StartCalendarInterval": map[string]any{"Hour": 3, "Minute": 15}, "EnvironmentVariables": map[string]any{"EXAMPLE_MODE": "observe"}}
	f.after = map[string]any{"Label": adversarialLabel, "ProgramArguments": []any{exe, script, "run", "--hours", "1", "--only", "example"}, "StartCalendarInterval": map[string]any{"Hour": 3, "Minute": 15}, "EnvironmentVariables": map[string]any{"EXAMPLE_MODE": "observe"}}
	f.env = LaunchdEnvironment{UserHome: home, UID: os.Getuid(), Now: func() time.Time { return time.Now().UTC() }, Run: func(_ context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
		if exe == "/usr/bin/plutil" {
			b, err := os.ReadFile(args[len(args)-1])
			return LaunchdCommandResult{Stdout: string(b)}, err
		}
		if exe != "/bin/launchctl" {
			t.Fatalf("unexpected executable %s", exe)
		}
		switch args[0] {
		case "print-disabled":
			return LaunchdCommandResult{Stdout: fmt.Sprintf("disabled services = {\n%q => true\n\"org.example.unrelated\" => disabled\n}", adversarialLabel)}, nil
		case "print":
			return adversarialAbsent(os.Getuid()), nil
		default:
			t.Fatalf("plan attempted effect %v", args)
			return LaunchdCommandResult{}, nil
		}
	}}
	return f
}

func (f *adversarialLaunchdFixture) plan(t *testing.T) (LaunchdRepairPlan, error) {
	t.Helper()
	adversarialWriteJSON(t, f.plist, f.before)
	adversarialWriteJSON(t, f.candidate, f.after)
	return BuildLaunchdRepairPlan(context.Background(), f.env, "work_example_launchd", "codex:example", f.plist, f.candidate, f.proofs)
}

func TestLaunchdAdversarialPlanPreservesExecutionBoundary(t *testing.T) {
	t.Run("valid-calendar-only", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		p, err := f.plan(t)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateLaunchdPlan(p); err != nil {
			t.Fatal(err)
		}
	})
	for _, key := range []string{"KeepAlive", "RunAtLoad", "StartInterval", "WatchPaths", "QueueDirectories", "Sockets", "MachServices", "LaunchEvents", "StartOnMount"} {
		t.Run("trigger-"+key, func(t *testing.T) {
			f := newAdversarialLaunchdFixture(t)
			f.before[key] = true
			f.after[key] = true
			if _, err := f.plan(t); err == nil {
				t.Fatalf("bootstrap-affecting %s accepted", key)
			}
		})
	}
	t.Run("keepalive-dictionary", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		f.before["KeepAlive"] = map[string]any{"SuccessfulExit": false}
		f.after["KeepAlive"] = f.before["KeepAlive"]
		if _, err := f.plan(t); err == nil {
			t.Fatal("KeepAlive dictionary accepted")
		}
	})
	for _, key := range []string{"Label", "EnvironmentVariables", "StandardOutPath", "StartCalendarInterval"} {
		t.Run("changed-"+key, func(t *testing.T) {
			f := newAdversarialLaunchdFixture(t)
			f.after[key] = "different"
			if _, err := f.plan(t); err == nil {
				t.Fatalf("non-argv change accepted: %s", key)
			}
		})
	}
	t.Run("subcommand-change", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		f.after["ProgramArguments"].([]any)[2] = "delete"
		if _, err := f.plan(t); err == nil {
			t.Fatal("different script subcommand accepted")
		}
	})
	t.Run("source-content-drift", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		if err := os.WriteFile(f.proofs[1].Path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.plan(t); err == nil {
			t.Fatal("stale source proof accepted")
		}
	})
	t.Run("missing-proof", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		f.proofs = f.proofs[:1]
		if _, err := f.plan(t); err == nil {
			t.Fatal("missing executable/script proof accepted")
		}
	})
	t.Run("symlink-target", func(t *testing.T) {
		f := newAdversarialLaunchdFixture(t)
		if _, err := f.plan(t); err != nil {
			t.Fatal(err)
		}
		real := f.plist + ".real"
		if err := os.Rename(f.plist, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, f.plist); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildLaunchdRepairPlan(context.Background(), f.env, "work_example_launchd", "codex:example", f.plist, f.candidate, f.proofs); err == nil {
			t.Fatal("symlink plist accepted")
		}
	})
}

func TestLaunchdAdversarialExactGrant(t *testing.T) {
	f := newAdversarialLaunchdFixture(t)
	p, err := f.plan(t)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := policy.StandingGrant{ID: "grant_example_launchd", Capability: LaunchdRepairCapability, MutationClass: "external_side_effect", TargetPath: p.PlistPath, Destination: launchdPlanDestination(p), BlastRadius: "narrow", ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339)}
	if _, err = launchdExactGrant(p, policy.Config{StandingGrants: []policy.StandingGrant{base}}, base.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*policy.StandingGrant)
	}{
		{"expired", func(g *policy.StandingGrant) { g.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339) }},
		{"not-active", func(g *policy.StandingGrant) { g.NotBefore = now.Add(time.Hour).Format(time.RFC3339) }},
		{"broad-target", func(g *policy.StandingGrant) { g.TargetPath = filepath.Dir(p.PlistPath) }},
		{"broad-destination", func(g *policy.StandingGrant) { g.Destination = "launchd:*" }},
		{"other-work", func(g *policy.StandingGrant) {
			g.Destination = strings.ReplaceAll(g.Destination, p.WorkID, "work_other")
		}},
		{"wrong-capability", func(g *policy.StandingGrant) { g.Capability = "system.launchd.*" }},
		{"broad-radius", func(g *policy.StandingGrant) { g.BlastRadius = "system" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := base
			tc.change(&g)
			if _, err := launchdExactGrant(p, policy.Config{StandingGrants: []policy.StandingGrant{g}}, g.ID, now); err == nil {
				t.Fatal("inexact authority accepted")
			}
		})
	}
	if _, err = launchdExactGrant(p, policy.Config{}, base.ID, now); err == nil {
		t.Fatal("revoked grant accepted")
	}
	if _, err = launchdExactGrant(p, policy.Config{StandingGrants: []policy.StandingGrant{base, base}}, base.ID, now); err == nil {
		t.Fatal("duplicate grant accepted")
	}
}

func TestLaunchdAdversarialForeignServiceCannotBeOwned(t *testing.T) {
	f := newAdversarialLaunchdFixture(t)
	p, err := f.plan(t)
	if err != nil {
		t.Fatal(err)
	}
	r := LaunchdRuntime{Domain: p.Domain, Label: p.Label, LoadedState: "loaded", OverrideState: "enabled", ServicePath: p.PlistPath, Program: p.CandidateArgs[0], Arguments: append([]string{}, p.CandidateArgs...)}
	if !launchdServiceOwned(p, r) {
		t.Fatal("exact service rejected")
	}
	for _, which := range []string{"path", "program", "args", "domain", "label"} {
		t.Run(which, func(t *testing.T) {
			bad := r
			switch which {
			case "path":
				bad.ServicePath += ".foreign"
			case "program":
				bad.Program += ".foreign"
			case "args":
				bad.Arguments = append([]string{}, r.Arguments...)
				bad.Arguments[2] = "delete"
			case "domain":
				bad.Domain = "gui/999999"
			case "label":
				bad.Label = "org.example.other"
			}
			if launchdServiceOwned(p, bad) {
				t.Fatalf("foreign %s accepted for destructive rollback", which)
			}
		})
	}
}

func TestLaunchdAdversarialEffectsAreExact(t *testing.T) {
	p := LaunchdRepairPlan{Domain: fmt.Sprintf("gui/%d", os.Getuid()), Label: adversarialLabel, PlistPath: "/example/Library/LaunchAgents/org.example.observer.plist"}
	for _, phase := range []string{"enable", "bootstrap", "bootout", "disable"} {
		t.Run(phase, func(t *testing.T) {
			calls := 0
			env := LaunchdEnvironment{Run: func(_ context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
				calls++
				want := []string{phase, p.Domain + "/" + p.Label}
				if phase == "bootstrap" {
					want = []string{phase, p.Domain, p.PlistPath}
				}
				if exe != "/bin/launchctl" || !reflect.DeepEqual(args, want) {
					t.Fatalf("unexpected effect %s %v", exe, args)
				}
				return LaunchdCommandResult{}, nil
			}}
			if err := launchdEffect(context.Background(), env, p, phase); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

type adversarialLiveFixture struct {
	f                *adversarialLaunchdFixture
	home             string
	plan             LaunchdRepairPlan
	review           LaunchdRepairReview
	now              time.Time
	cfg              policy.Config
	store            *work.Store
	disabled, loaded bool
	effects          []string
	failAfter        string
}

func (f *adversarialLiveFixture) writeConfig(t *testing.T) {
	t.Helper()
	b, err := yaml.Marshal(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.home, "config", "policy.yaml"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func newAdversarialLiveFixture(t *testing.T) *adversarialLiveFixture {
	t.Helper()
	f := &adversarialLiveFixture{f: newAdversarialLaunchdFixture(t), home: t.TempDir(), now: time.Now().UTC(), disabled: true}
	if err := homepkg.Init(f.home, "test", f.now); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = work.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.store.Create(work.CreateInput{Owner: "operator:fixture", Actor: "operator:fixture", State: "ready", SuccessPredicate: "repair only exact synthetic observer"}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Claim(w.ID, "operator:fixture", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:fixture"}, f.now); err != nil {
		t.Fatal(err)
	}
	adversarialWriteJSON(t, f.f.plist, f.f.before)
	adversarialWriteJSON(t, f.f.candidate, f.f.after)
	f.f.env.Now = func() time.Time { return f.now }
	f.plan, err = BuildLaunchdRepairPlan(context.Background(), f.f.env, w.ID, "operator:fixture", f.f.plist, f.f.candidate, f.f.proofs)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = policy.DefaultConfig()
	f.cfg.StandingGrants = append(f.cfg.StandingGrants, policy.StandingGrant{ID: "grant_example_launchd", Capability: LaunchdRepairCapability, MutationClass: "external_side_effect", TargetPath: f.plan.PlistPath, Destination: launchdPlanDestination(f.plan), BlastRadius: "narrow", ExpiresAt: f.now.Add(40 * time.Minute).Format(time.RFC3339)})
	f.writeConfig(t)
	if err = RecordLaunchdRepairPlan(f.home, f.plan, f.now); err != nil {
		t.Fatal(err)
	}
	f.review = f.sealReview(t, false)
	f.f.env.Run = func(_ context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
		if exe != "/bin/launchctl" {
			t.Fatalf("unexpected executable %s", exe)
		}
		switch args[0] {
		case "print-disabled":
			return LaunchdCommandResult{Stdout: fmt.Sprintf("disabled services = {\n%q => %t\n\"org.example.unrelated\" => true\n}", adversarialLabel, f.disabled)}, nil
		case "print":
			if !f.loaded {
				return adversarialAbsent(os.Getuid()), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s/%s = {\npath = %s\nprogram = %s\narguments = {\n", f.plan.Domain, f.plan.Label, f.plan.PlistPath, f.plan.CandidateArgs[0])
			for _, a := range f.plan.CandidateArgs {
				fmt.Fprintln(&b, a)
			}
			fmt.Fprintf(&b, "}\ndomain = %s [123]\n}\n", f.plan.Domain)
			return LaunchdCommandResult{Stdout: b.String()}, nil
		case "enable":
			f.disabled = false
		case "bootstrap":
			f.loaded = true
		case "bootout":
			f.loaded = false
		case "disable":
			f.disabled = true
		default:
			t.Fatalf("unexpected effect %v", args)
		}
		f.effects = append(f.effects, args[0])
		if f.failAfter == args[0] {
			return LaunchdCommandResult{ExitCode: 5, Stderr: "synthetic error after actual effect"}, nil
		}
		return LaunchdCommandResult{}, nil
	}
	return f
}

func (f *adversarialLiveFixture) sealReview(t *testing.T, rollback bool) LaunchdRepairReview {
	t.Helper()
	return f.sealReviewGrant(t, rollback, "grant_example_launchd")
}

func (f *adversarialLiveFixture) sealReviewGrant(t *testing.T, rollback bool, grantID string) LaunchdRepairReview {
	t.Helper()
	reviewer := "operator:fixture"
	expiry := f.now.Add(15 * time.Minute)
	evidence := "fixture:independent-launchd-review"
	fn := ReviewLaunchdRepairPlan
	if rollback {
		fn = ReviewLaunchdRepairRollbackPlan
	}
	r, err := fn(f.home, f.plan.ID, grantID, reviewer, evidence, expiry, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Pending {
		t.Fatal("review accepted before exact operator approval")
	}
	b, err := os.ReadFile(r.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	var intent envelope.ActionIntent
	if err = json.Unmarshal(b, &intent); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := policy.LoadConfig(f.home)
	if err != nil {
		t.Fatal(err)
	}
	ap := policy.NewExactActionApprovalStore(f.home)
	plan, _, _, err := ap.EnsurePlanForIntent(intent, cfg, 15*time.Minute, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ap.Approve(plan.ID, reviewer, cfg, f.now); err != nil {
		t.Fatal(err)
	}
	recorded, err := f.store.EvaluateAndRecordAction(f.plan.WorkID, intent, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Allowed(recorded.Decision) {
		t.Fatalf("exact fixture approval refused: %+v", recorded.Decision)
	}
	r, err = fn(f.home, f.plan.ID, grantID, reviewer, evidence, expiry, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pending || r.Review == nil {
		t.Fatal("authentic approval did not seal review")
	}
	return *r.Review
}

func (f *adversarialLiveFixture) apply() (LaunchdRepairResult, error) {
	return ApplyLaunchdRepair(context.Background(), f.home, f.plan.ID, f.review.ID, f.plan.Actor, f.f.env)
}

func TestLaunchdAdversarialPhasesAndRollbackAreReceiptedOnce(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	r, err := f.apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Phases) != 3 || !f.loaded || f.disabled {
		t.Fatalf("wrong final state: %+v", r)
	}
	for _, p := range r.Phases {
		if p.ReceiptID == "" || p.Outcome != "completed" {
			t.Fatalf("unreceipted phase %+v", p)
		}
	}
	if _, err = f.apply(); err != nil {
		t.Fatalf("completed replay must read receipts: %v", err)
	}
	if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
		t.Fatalf("forward replay repeated effects: %v", f.effects)
	}
	rr := f.sealReview(t, true)
	r, err = RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rr.ID, f.plan.Actor, f.f.env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.plan.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.loaded || !f.disabled || !bytesEqual(b, f.plan.Original) {
		t.Fatalf("rollback not exact %+v", r)
	}
	// A completed rollback releases label ownership. A second call must refuse
	// the retired owner without issuing another OS effect.
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rr.ID, f.plan.Actor, f.f.env); err == nil || !strings.Contains(err.Error(), "no active owned plan") {
		t.Fatalf("completed rollback did not refuse retired ownership: %v", err)
	}
	if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap", "bootout", "disable"}) {
		t.Fatalf("rollback replay repeated effects: %v", f.effects)
	}
}

func TestLaunchdAdversarialDriftBeforeEveryNextPhase(t *testing.T) {
	for _, phase := range []string{"replace", "enable"} {
		for _, kind := range []string{"expiry", "revocation", "grant-revision", "work-generation", "source", "plist"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				f := newAdversarialLiveFixture(t)
				fired := false
				f.f.env.Checkpoint = func(point string) error {
					if point != phase+":after_receipt" || fired {
						return nil
					}
					fired = true
					switch kind {
					case "expiry":
						f.now = f.now.Add(time.Hour)
					case "revocation":
						f.cfg.StandingGrants = nil
						f.writeConfig(t)
					case "grant-revision":
						f.cfg.StandingGrants[0].ExpiresAt = f.now.Add(50 * time.Minute).Format(time.RFC3339)
						f.writeConfig(t)
					case "work-generation":
						f.now = f.now.Add(time.Millisecond)
						if _, err := f.store.Advance(f.plan.WorkID, work.AdvanceInput{To: "running", Actor: f.plan.Actor}, f.now); err != nil {
							t.Fatal(err)
						}
					case "source":
						if err := os.WriteFile(f.plan.SourceProofs[1].Path, []byte("changed after receipt"), 0600); err != nil {
							t.Fatal(err)
						}
					case "plist":
						if err := os.WriteFile(f.plan.PlistPath, []byte("foreign replacement"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return nil
				}
				if _, err := f.apply(); err == nil {
					t.Fatal("drift did not stop next effect")
				}
				if !fired {
					t.Fatal("test never reached drift seam")
				}
				want := []string{}
				if phase == "enable" {
					want = []string{"enable"}
				}
				if len(f.effects) != len(want) || len(want) > 0 && !reflect.DeepEqual(f.effects, want) {
					t.Fatalf("effect after drift: %v", f.effects)
				}
			})
		}
	}
}

func TestLaunchdAdversarialCrashNeverRepeatsAnEffect(t *testing.T) {
	for _, phase := range []string{"replace", "enable", "bootstrap"} {
		for _, boundary := range []string{"after_effect", "before_receipt", "after_receipt"} {
			t.Run(phase+"/"+boundary, func(t *testing.T) {
				f := newAdversarialLiveFixture(t)
				fired := false
				f.f.env.Checkpoint = func(point string) error {
					if point == phase+":"+boundary && !fired {
						fired = true
						return errors.New("simulated executor crash")
					}
					return nil
				}
				if _, err := f.apply(); err == nil {
					t.Fatal("crash was not surfaced")
				}
				if !fired {
					t.Fatal("crash seam not reached")
				}
				before := append([]string{}, f.effects...)
				f.f.env.Checkpoint = nil
				r, err := f.apply()
				if boundary == "after_effect" {
					if err == nil {
						t.Fatal("unrecorded outcome must reconcile as failed, not authorize continuation")
					}
					if len(before) != len(f.effects) || len(before) > 0 && !reflect.DeepEqual(before, f.effects) {
						t.Fatalf("crash replay repeated effect: %v -> %v", before, f.effects)
					}
					if len(r.Phases) == 0 || r.Phases[len(r.Phases)-1].Outcome == "completed" {
						t.Fatalf("ambiguous outcome falsely completed: %+v", r)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
						t.Fatalf("receipt recovery repeated effect: %v", f.effects)
					}
				}
			})
		}
	}
}

func TestLaunchdAdversarialErrorAfterEffectCannotAdvance(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	f.failAfter = "enable"
	if _, err := f.apply(); err == nil {
		t.Fatal("nonzero effect exit accepted")
	}
	if f.disabled || f.loaded || !reflect.DeepEqual(f.effects, []string{"enable"}) {
		t.Fatal("partial actual state not retained")
	}
	if _, err := f.apply(); err == nil {
		t.Fatal("failed effect blindly resumed")
	}
	if len(f.effects) != 1 {
		t.Fatalf("repeated failed effect: %v", f.effects)
	}
	rr := f.sealReview(t, true)
	if _, err := RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rr.ID, f.plan.Actor, f.f.env); err != nil {
		t.Fatal(err)
	}
	if !f.disabled || f.loaded {
		t.Fatal("partial rollback failed")
	}
}

func TestLaunchdAdversarialRollbackRefusesForeignDrift(t *testing.T) {
	for _, kind := range []string{"service-path", "service-arguments", "plist-bytes", "foreign-override"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdversarialLiveFixture(t)
			if _, err := f.apply(); err != nil {
				t.Fatal(err)
			}
			rr := f.sealReview(t, true)
			runner := f.f.env.Run
			f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
				r, err := runner(ctx, exe, args...)
				if args[0] == "print" {
					switch kind {
					case "service-path":
						r.Stdout = strings.Replace(r.Stdout, "path = "+f.plan.PlistPath, "path = "+f.plan.PlistPath+".foreign", 1)
					case "service-arguments":
						r.Stdout = strings.Replace(r.Stdout, "\nrun\n", "\ndelete\n", 1)
					}
				}
				if kind == "foreign-override" && args[0] == "print-disabled" {
					r.Stdout = strings.Replace(r.Stdout, "\"org.example.unrelated\" => true", "\"org.example.unrelated\" => false", 1)
				}
				return r, err
			}
			if kind == "plist-bytes" {
				if err := os.WriteFile(f.plan.PlistPath, []byte("foreign bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rr.ID, f.plan.Actor, f.f.env); err == nil {
				t.Fatal("rollback accepted foreign state")
			}
			if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
				t.Fatalf("foreign service or file touched: %v", f.effects)
			}
		})
	}
}

func TestLaunchdAdversarialResealedJournalCannotReplaceNativeEvidence(t *testing.T) {
	for _, kind := range []string{"outcome", "start"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdversarialLiveFixture(t)
			if _, err := f.apply(); err != nil {
				t.Fatal(err)
			}
			stem := launchdPhaseStem(f.plan, "apply", 0)
			var start launchdPhaseStart
			var outcome launchdPhaseOutcome
			var commit launchdPhaseCommit
			for _, item := range []struct {
				name string
				v    any
			}{{stem + ".started.json", &start}, {stem + ".outcome.json", &outcome}, {stem + ".committed.json", &commit}} {
				if err := readLaunchdArtifact(f.home, item.name, item.v); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "start" {
				start.BeforeRuntime.Error = "forged preimage"
				start.Digest = ""
				start.Digest = launchdDigest(start)
				outcome.StartDigest = start.Digest
				commit.StartDigest = start.Digest
			} else {
				outcome.Final.Summary = "forged successful outcome"
			}
			outcome.Digest = ""
			outcome.Digest = launchdDigest(outcome)
			commit.OutcomeDigest = outcome.Digest
			for _, item := range []struct {
				name string
				v    any
			}{{stem + ".started.json", start}, {stem + ".outcome.json", outcome}, {stem + ".committed.json", commit}} {
				p, err := launchdArtifactPath(f.home, item.name)
				if err != nil {
					t.Fatal(err)
				}
				adversarialWriteJSON(t, p, item.v)
			}
			if _, err := f.apply(); err == nil {
				t.Fatal("self-resealed journal replaced native receipt authority")
			}
			if !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
				t.Fatalf("tampered replay produced effects: %v", f.effects)
			}
		})
	}
}

func TestLaunchdAdversarialUnknownReadbackIsNotObservedChange(t *testing.T) {
	before := []byte("known original bytes")
	start := launchdPhaseStart{Name: "enable", BeforeBytes: before, BeforeRuntime: LaunchdRuntime{OverrideState: "disabled", LoadedState: "absent"}}
	for _, tc := range []struct {
		name    string
		b       []byte
		r       LaunchdRuntime
		changed bool
	}{
		{"unknown-runtime", before, LaunchdRuntime{OverrideState: "unknown", LoadedState: "unknown", Error: "permission denied"}, false},
		{"unknown-file-and-runtime", nil, LaunchdRuntime{OverrideState: "unknown", LoadedState: "unknown", Error: "unreadable"}, false},
		{"known-file-delta-unknown-runtime", []byte("observed new bytes"), LaunchdRuntime{OverrideState: "unknown", LoadedState: "unknown"}, true},
		{"known-override-delta", before, LaunchdRuntime{OverrideState: "enabled", LoadedState: "absent"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := makeLaunchdOutcome(start, tc.b, tc.r, errors.New("readback failed"))
			if out.Final.Outcome != "failed" || out.Final.Changed != tc.changed {
				t.Fatalf("uncertainty misstated as observed change: %+v", out.Final)
			}
			if tc.b == nil {
				if out.FileSHA256 != "" {
					t.Fatal("unreadable file acquired a fabricated content digest")
				}
				for _, ref := range out.Final.AfterRefs {
					if strings.HasPrefix(ref, "sha256:") {
						t.Fatal("unreadable file represented as observed empty content")
					}
				}
			}
		})
	}
}
