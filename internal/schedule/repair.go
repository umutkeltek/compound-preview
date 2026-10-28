package schedule

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"compound/internal/authority"
	homepkg "compound/internal/home"
)

const repairUsage = "usage: compound schedule repair plan --work W --actor A --plist P --candidate P --source-proofs P [--handoff-from OLD_PLAN] | review --plan ID --grant ID --reviewed-by operator:NAME --evidence REF --expires RFC3339 [--rollback] | apply|rollback --plan ID --review ID --actor A [--json]"

type repairFlags struct {
	work, actor, plist, candidate, proofs, plan, grant, review, reviewedBy, evidence, expires, handoff string
	rollback, json                                                                                     bool
}

func parseRepairFlags(args []string) (string, repairFlags, error) {
	var f repairFlags
	if len(args) == 0 {
		return "", f, fmt.Errorf("repair subcommand required")
	}
	sub := args[0]
	fs := flag.NewFlagSet("schedule repair "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&f.json, "json", false, "JSON output")
	switch sub {
	case "plan":
		fs.StringVar(&f.work, "work", "", "work")
		fs.StringVar(&f.actor, "actor", "", "actor")
		fs.StringVar(&f.plist, "plist", "", "existing plist")
		fs.StringVar(&f.candidate, "candidate", "", "candidate plist")
		fs.StringVar(&f.proofs, "source-proofs", "", "explicit executable/script proof JSON")
		fs.StringVar(&f.handoff, "handoff-from", "", "exact terminal successful predecessor plan")
	case "review":
		fs.StringVar(&f.plan, "plan", "", "plan ID")
		fs.StringVar(&f.grant, "grant", "", "exact grant ID")
		fs.StringVar(&f.reviewedBy, "reviewed-by", "", "operator")
		fs.StringVar(&f.evidence, "evidence", "", "operator decision ref")
		fs.StringVar(&f.expires, "expires", "", "review expiry")
		fs.BoolVar(&f.rollback, "rollback", false, "review explicit rollback")
	case "apply", "rollback":
		fs.StringVar(&f.plan, "plan", "", "plan ID")
		fs.StringVar(&f.review, "review", "", "review ID")
		fs.StringVar(&f.actor, "actor", "", "lease actor")
	default:
		return sub, f, fmt.Errorf("unknown repair subcommand %q", sub)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return sub, f, err
	}
	if fs.NArg() != 0 {
		return sub, f, fmt.Errorf("unexpected repair positional argument")
	}
	if sub == "plan" && (f.work == "" || f.actor == "" || f.plist == "" || f.candidate == "" || f.proofs == "") {
		return sub, f, fmt.Errorf("plan requires exact work, actor, plist, candidate and source proofs")
	}
	if sub == "review" && (f.plan == "" || f.grant == "" || f.reviewedBy == "" || f.evidence == "" || f.expires == "") {
		return sub, f, fmt.Errorf("review requires exact plan, grant, operator evidence and expiry")
	}
	if (sub == "apply" || sub == "rollback") && (f.plan == "" || f.review == "" || f.actor == "") {
		return sub, f, fmt.Errorf("effect requires exact plan, review and actor")
	}
	return sub, f, nil
}
func repairPrint(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(b))
}
func runScheduleRepair(args []string) int {
	sub, f, err := parseRepairFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, repairUsage)
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "schedule repair requires current-user macOS launchd")
		return 2
	}
	home, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	env := authority.LaunchdEnvironment{UserHome: userHome, UID: os.Getuid()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch sub {
	case "plan":
		var proofs []authority.LaunchdSourceProof
		var body []byte
		body, err = os.ReadFile(f.proofs)
		if err == nil {
			err = json.Unmarshal(body, &proofs)
		}
		if err != nil {
			break
		}
		var p authority.LaunchdRepairPlan
		if f.handoff == "" {
			p, err = authority.BuildLaunchdRepairPlan(ctx, env, f.work, f.actor, f.plist, f.candidate, proofs)
		} else {
			p, err = authority.BuildLaunchdRepairPlanWithHandoff(ctx, home, env, f.work, f.actor, f.plist, f.candidate, proofs, f.handoff)
		}
		if err == nil {
			err = authority.RecordLaunchdRepairPlan(home, p, time.Now().UTC())
		}
		if err == nil {
			repairPrint(p)
		}
	case "review":
		var expiry time.Time
		expiry, err = time.Parse(time.RFC3339, f.expires)
		if err != nil {
			break
		}
		var result authority.LaunchdReviewResult
		if f.rollback {
			result, err = authority.ReviewLaunchdRepairRollbackPlan(home, f.plan, f.grant, f.reviewedBy, f.evidence, expiry, time.Now().UTC())
		} else {
			result, err = authority.ReviewLaunchdRepairPlan(home, f.plan, f.grant, f.reviewedBy, f.evidence, expiry, time.Now().UTC())
		}
		repairPrint(result)
	case "apply", "rollback":
		var result authority.LaunchdRepairResult
		if sub == "apply" {
			result, err = authority.ApplyLaunchdRepair(ctx, home, f.plan, f.review, f.actor, env)
		} else {
			result, err = authority.RollbackLaunchdRepair(ctx, home, f.plan, f.review, f.actor, env)
		}
		repairPrint(result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "schedule repair:", err)
		return 1
	}
	return 0
}
