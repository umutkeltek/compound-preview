package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/authority"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/work"
)

var worktreeRetirementCLILiveness authority.WorktreeRetirementLiveness = authority.LSOFWorktreeRetirementLiveness{}
var worktreeRetirementCLIGit authority.WorktreeRetirementGit = authority.ExecWorktreeRetirementGit{}

const authorityWorktreeRetireUsage = "usage: compound authority worktree-retire <plan --repo ROOT [--worktree PATH] [--ref refs/heads/NAME] --authority-ref refs/heads/main [--publication-ref refs/remotes/origin/main] --owner-work WORK --source-work WORK --recovery-ref REF... [--json] | review REVIEW.yaml --decision authorize_retirement --rationale TEXT --reviewed-by operator:ID [--json] | apply PLAN.json --actor LEASE_HOLDER [--json]>"

func runAuthorityWorktreeRetire(homePath string, args []string, stdout, stderr io.Writer, now time.Time) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, authorityWorktreeRetireUsage)
		return 2
	}
	switch args[0] {
	case "plan":
		values, repeated, jsonOut, err := parseWorktreeRetirementFlags(args[1:], map[string]bool{
			"--repo": true, "--worktree": true, "--ref": true, "--authority-ref": true,
			"--publication-ref": true, "--owner-work": true, "--source-work": true,
		}, "--recovery-ref")
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 2
		}
		for _, required := range []string{"--repo", "--authority-ref", "--owner-work", "--source-work"} {
			if strings.TrimSpace(values[required]) == "" {
				fmt.Fprintf(stderr, "authority worktree-retire plan: %s required\n", required)
				return 2
			}
		}
		if strings.TrimSpace(values["--worktree"]) == "" && strings.TrimSpace(values["--ref"]) == "" {
			fmt.Fprintln(stderr, "authority worktree-retire plan: at least --worktree or --ref required")
			return 2
		}
		store, err := work.Open(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 1
		}
		ownerProof, err := loadWorktreeRetirementOwnerProof(store, values["--owner-work"])
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 1
		}
		sourceProof, err := loadWorktreeRetirementSourceProof(store, homePath, values["--source-work"], now)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 1
		}
		// Compose operator keep-decisions from the decisions reader; authority stays
		// mem-free. Fail closed: an unreadable reader marks the request so the gate
		// blocks (it cannot prove no keep pins the target).
		keepDecisions, keepsUnreadable := loadWorktreeRetirementKeepDecisions(homePath)
		plan, err := authority.BuildWorktreeRetirementPlan(context.Background(), authority.WorktreeRetirementPlanRequest{
			RepoRoot: values["--repo"], WorktreePath: values["--worktree"], CandidateRef: values["--ref"],
			AuthorityRef: values["--authority-ref"], PublicationRef: values["--publication-ref"],
			OwnerProof: ownerProof, SourceProof: sourceProof, RecoveryRefs: repeated,
			KeepDecisions: keepDecisions, KeepsUnreadable: keepsUnreadable,
		}, authority.WorktreeRetirementDependencies{Git: worktreeRetirementCLIGit, Liveness: worktreeRetirementCLILiveness, RecoveryInventoryPreflight: work.PreflightRetirementRecoveryInventory}, now)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 1
		}
		paths, err := authority.RecordWorktreeRetirementPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire plan:", err)
			return 1
		}
		response := struct {
			SchemaVersion string                                `json:"schema_version"`
			Status        string                                `json:"status"`
			AuthorityRef  string                                `json:"authority_ref"`
			Plan          authority.WorktreeRetirementPlan      `json:"plan"`
			Paths         authority.WorktreeRetirementPlanPaths `json:"paths"`
		}{authority.WorktreeRetirementPlanSchemaVersion, plan.Status, "compound://reviews/" + authority.WorktreeRetirementReviewArea, plan, paths}
		if jsonOut {
			writeWorktreeRetirementJSON(stdout, response)
		} else {
			fmt.Fprintf(stdout, "worktree-retire plan %s status=%s eligible=%v relation=%s\n", plan.ID, plan.Status, plan.Eligible, plan.CandidateRelation)
			fmt.Fprintf(stdout, "plan: %s\nreview: %s\n", paths.PlanPath, paths.ReviewYAMLPath)
			for _, reason := range plan.BlockReasons {
				fmt.Fprintf(stdout, "blocked: %s\n", reason)
			}
		}
		if !plan.Eligible {
			return 1
		}
		return 0

	case "review":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(stderr, authorityWorktreeRetireUsage)
			return 2
		}
		values, _, jsonOut, err := parseWorktreeRetirementFlags(args[2:], map[string]bool{
			"--decision": true, "--rationale": true, "--reviewed-by": true,
		}, "")
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire review:", err)
			return 2
		}
		if values["--decision"] != "authorize_retirement" || strings.TrimSpace(values["--rationale"]) == "" || strings.TrimSpace(values["--reviewed-by"]) == "" {
			fmt.Fprintln(stderr, "authority worktree-retire review: --decision authorize_retirement, --rationale, and --reviewed-by required")
			return 2
		}
		review, err := authority.LoadWorktreeRetirementReview(args[1])
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire review:", err)
			return 1
		}
		review.Decision = values["--decision"]
		review.Rationale = values["--rationale"]
		review.ReviewedBy = values["--reviewed-by"]
		report, err := authority.RecordWorktreeRetirementReview(homePath, review, args[1], now)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire review:", err)
			return 1
		}
		if jsonOut {
			writeWorktreeRetirementJSON(stdout, report)
		} else {
			fmt.Fprintf(stdout, "worktree-retire review %s status=%s plan=%s reviewer=%s\n", report.ID, report.Status, report.PlanID, report.ReviewedBy)
			fmt.Fprintf(stdout, "reviewed: %s\n", report.ReviewPath)
		}
		return 0

	case "apply":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(stderr, authorityWorktreeRetireUsage)
			return 2
		}
		values, _, jsonOut, err := parseWorktreeRetirementFlags(args[2:], map[string]bool{"--actor": true}, "")
		if err != nil || strings.TrimSpace(values["--actor"]) == "" {
			fmt.Fprintln(stderr, "authority worktree-retire apply: --actor required")
			return 2
		}
		plan, err := authority.LoadWorktreeRetirementPlan(args[1])
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		reportPath, err := findAuthorizedWorktreeRetirementReport(args[1], plan)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		store, err := work.Open(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		ownerProof, err := loadWorktreeRetirementOwnerProof(store, plan.OwnerProof.WorkID)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		sourceProof, err := loadWorktreeRetirementSourceProof(store, homePath, plan.SourceProof.WorkID, now)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		cfg, _, err := policy.LoadConfig(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", err)
			return 1
		}
		result, applyErr := authority.ApplyWorktreeRetirement(context.Background(), authority.WorktreeRetirementApplyRequest{
			HomePath: homePath, PlanPath: args[1], ReviewReportPath: reportPath, Actor: values["--actor"],
			OwnerProof: ownerProof, SourceProof: sourceProof,
		}, authority.WorktreeRetirementApplyDependencies{
			SourceProofResolver: func(id string) (authority.WorktreeRetirementSourceProof, error) {
				return store.WorktreeRetirementSourceProof(id, now)
			},
			RecoveryInventoryPreflight: work.PreflightRetirementRecoveryInventory,
			Git:                        worktreeRetirementCLIGit, Liveness: worktreeRetirementCLILiveness,
			Evaluate: func(action envelope.ActionIntent, at time.Time) (envelope.PolicyDecision, error) {
				return policy.Evaluate(action, cfg, at), nil
			},
			RecordOutcome: func(outcome authority.WorktreeRetirementActionOutcome, at time.Time) (authority.WorktreeRetirementActionProof, error) {
				recorded, err := store.RecordReceiptedAction(plan.OwnerProof.WorkID, work.ReceiptedActionInput{
					Action: outcome.Action, PreEvaluatedDecision: &outcome.Decision, ExpectedWorkState: "running",
					Outcome: outcome.Outcome, Changed: outcome.Changed, Summary: outcome.Summary,
					MoveType: "control_plane_repair", ProofRefs: outcome.ProofRefs,
					BeforeRefs: outcome.BeforeRefs, AfterRefs: outcome.AfterRefs, FailureCause: outcome.FailureCause,
				}, at)
				if err != nil {
					return authority.WorktreeRetirementActionProof{}, err
				}
				postRecordOwnerProof, err := loadWorktreeRetirementOwnerProof(store, plan.OwnerProof.WorkID)
				if err != nil {
					return authority.WorktreeRetirementActionProof{}, err
				}
				return authority.WorktreeRetirementActionProof{
					ActionIntentID: recorded.Action.ID, PolicyDecisionID: recorded.Decision.ID,
					ReceiptID: recorded.Receipt.ID, TransitionID: recorded.Transition.ID,
					LeaseWorkSHA256: postRecordOwnerProof.LeaseWorkSHA256, WorkDigest: postRecordOwnerProof.WorkDigest,
				}, nil
			},
		}, now)
		if jsonOut {
			writeWorktreeRetirementJSON(stdout, result)
		} else if result.ID != "" {
			fmt.Fprintf(stdout, "worktree-retire apply %s status=%s phase=%s receipt=%s transition=%s\n", result.ID, result.Status, result.Phase, result.ReceiptID, result.TransitionID)
		}
		if applyErr != nil {
			fmt.Fprintln(stderr, "authority worktree-retire apply:", applyErr)
			return 1
		}
		return 0

	default:
		fmt.Fprintln(stderr, authorityWorktreeRetireUsage)
		return 2
	}
}

func parseWorktreeRetirementFlags(args []string, allowed map[string]bool, repeatedFlag string) (map[string]string, []string, bool, error) {
	values := map[string]string{}
	var repeated []string
	jsonOut := false
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if flag == "--json" {
			jsonOut = true
			continue
		}
		if flag == repeatedFlag && repeatedFlag != "" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, nil, false, fmt.Errorf("%s value required", flag)
			}
			repeated = append(repeated, args[i+1])
			i++
			continue
		}
		if !allowed[flag] {
			return nil, nil, false, fmt.Errorf("unknown flag %s", flag)
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return nil, nil, false, fmt.Errorf("%s value required", flag)
		}
		if _, duplicate := values[flag]; duplicate {
			return nil, nil, false, fmt.Errorf("duplicate flag %s", flag)
		}
		values[flag] = args[i+1]
		i++
	}
	return values, repeated, jsonOut, nil
}

// loadWorktreeRetirementKeepDecisions projects active operator worktree-keep
// records into the mem-free shape the retirement gate consumes. The second
// return is the fail-closed "keeps unreadable" flag: true when the decisions
// reader could not be read at all (the gate then blocks keep_records_unreadable).
func loadWorktreeRetirementKeepDecisions(homePath string) (map[string]authority.WorktreeKeepRecord, bool) {
	keeps, ok := decisions.ActiveWorktreeKeeps(homePath)
	if !ok {
		return nil, true
	}
	out := make(map[string]authority.WorktreeKeepRecord, len(keeps))
	for ref, k := range keeps {
		out[ref] = authority.WorktreeKeepRecord{DecisionID: k.DecisionID, BranchRef: k.BranchRef, Revisit: k.Revisit}
	}
	return out, false
}

func loadWorktreeRetirementOwnerProof(store *work.Store, workID string) (authority.WorktreeRetirementOwnerProof, error) {
	w, err := store.Get(strings.TrimSpace(workID))
	if err != nil {
		return authority.WorktreeRetirementOwnerProof{}, err
	}
	if w.State != "running" {
		return authority.WorktreeRetirementOwnerProof{}, fmt.Errorf("owner work %s must be running, got %s", w.ID, w.State)
	}
	leases, err := store.ListLeases()
	if err != nil {
		return authority.WorktreeRetirementOwnerProof{}, err
	}
	for _, lease := range leases {
		if lease.WorkID != w.ID {
			continue
		}
		data, _ := json.Marshal(w)
		digest := sha256.Sum256(data)
		return authority.WorktreeRetirementOwnerProof{
			WorkID: w.ID, State: w.State, LeaseHolder: lease.By, LeaseClaimedAt: lease.ClaimAt,
			LeaseWorkSHA256: lease.WorkSHA256, WorkDigest: "sha256:" + hex.EncodeToString(digest[:]),
		}, nil
	}
	return authority.WorktreeRetirementOwnerProof{}, fmt.Errorf("owner work %s has no current lease", w.ID)
}

func loadWorktreeRetirementSourceProof(store *work.Store, _ string, workID string, now time.Time) (authority.WorktreeRetirementSourceProof, error) {
	return store.WorktreeRetirementSourceProof(workID, now)
}

func findAuthorizedWorktreeRetirementReport(planPath string, plan authority.WorktreeRetirementPlan) (string, error) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(planPath), plan.ID+".*.retirement.reviewed.json"))
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	var valid []string
	for _, path := range matches {
		if _, err := authority.ValidateAuthorizedWorktreeRetirement(planPath, path); err == nil {
			valid = append(valid, path)
		}
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("no exact operator-authorized retirement review for plan %s", plan.ID)
	}
	if len(valid) > 1 {
		return "", fmt.Errorf("multiple exact retirement reviews for plan %s; refuse ambiguous authority", plan.ID)
	}
	return valid[0], nil
}

func writeWorktreeRetirementJSON(w io.Writer, value any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func allLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
