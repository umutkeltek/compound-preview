// policy_cli.go owns the `compound policy ...` verb dispatcher (cmdPolicy).
// Extracted from cmd/compound/main.go so main.go holds only the cross-cutting
// dispatch table + non-policy verbs. The policy runtime (governance v0:
// ActionIntent -> PolicyDecision, placement rules, standing grants,
// operator state, release goal holds, autonomy halt, editable surfaces)
// lives in compound/internal/policy and is unchanged; this file is only the
// CLI surface that drives it.
//
// Composition with the other CLI verb files:
//   - memory_cli.go                 (cmdMemory)
//   - memory_engine_config.go       (engine config subsystem)
//   - distiller_cli.go              (cmdDistiller)
//   - decisions_cli.go              (cmdDecisions)
//   - this file                     (cmdPolicy — fourth verb extracted)
//
// Subcommand surface (all in cmdPolicy's switch):
//   - evaluate, classify (shell verb catalog: policy.RunClassifyCLI)
//   - action-plan
//   - action-approve
//   - action-check
//   - import-placement-rules
//   - placement-authority
//   - import-standing-grants
//   - standing-grants-authority
//   - import-operator-state
//   - operator-authority
//   - import-operator-prompts
//   - operator-prompts-authority
//   - import-operator-approval-frontier
//   - operator-approval-frontier-authority
//   - import-release-goal-holds
//   - release-goal-holds-authority
//   - autonomy-halt
//   - presence (body in internal/policy.PresenceCLI)
//   - editable-surfaces
//
// Exit code contract (preserved across the move): 0 allow, 3 hold, 4 deny,
// so shell adapters can enforce without parsing JSON.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

func cmdPolicy(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound policy <evaluate|action-plan|action-approve|action-check|import-placement-rules|placement-authority|import-standing-grants|standing-grants-authority|import-operator-state|operator-authority|import-operator-prompts|operator-prompts-authority|import-operator-approval-frontier|operator-approval-frontier-authority|import-release-goal-holds|release-goal-holds-authority|autonomy-halt|presence|editable-surfaces|classify> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "action-plan", "action-approve", "action-check":
		if code := runPolicyActionApprovalCommand(args, os.Stdout, os.Stderr, time.Now()); code != 0 {
			os.Exit(code)
		}
	case "classify":
		if code := policy.RunClassifyCLI(args[1:], firstPartyCommandCatalog(), os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
	case "evaluate":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy evaluate <action-intent.json>")
			os.Exit(2)
		}
		ai := loadActionIntent(args[1])
		if err := ai.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "error: invalid action intent:", err)
			os.Exit(1)
		}
		pd := policy.Evaluate(ai, policyConfig(), time.Now())
		printJSON(pd)
		os.Exit(gateExit(pd))
	case "import-placement-rules":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-placement-rules <placement-rules.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportPlacementRules(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "placement-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy placement-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type placementAuthorityReport struct {
			PlacementMode    string                                  `json:"placement_guard_mode,omitempty"`
			PolicySource     string                                  `json:"policy_source,omitempty"`
			AuthorityRef     string                                  `json:"authority_ref"`
			AuthorityPath    string                                  `json:"authority_path"`
			Installed        bool                                    `json:"installed"`
			OwnerCount       int                                     `json:"owner_count,omitempty"`
			BlockOwnerCount  int                                     `json:"block_owner_count,omitempty"`
			BlockOwnerIDs    []string                                `json:"block_owner_ids,omitempty"`
			OpenWriteCount   int                                     `json:"open_write_count,omitempty"`
			SourceAuthority  []policy.PlacementSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath string                                  `json:"source_record_path"`
		}
		cfg, policySource, err := policy.LoadConfig(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report := placementAuthorityReport{
			PlacementMode:    cfg.PlacementGuardMode,
			PolicySource:     policySource,
			AuthorityRef:     policy.PlacementRulesAuthorityRef,
			AuthorityPath:    policy.PlacementRulesPath(homePath),
			SourceRecordPath: policy.PlacementSourceAuthorityPath(homePath),
		}
		rules, err := policy.LoadPlacementRulesFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.OwnerCount = len(rules.Owners)
			report.BlockOwnerIDs = rules.BlockOwnerIDs()
			report.BlockOwnerCount = len(report.BlockOwnerIDs)
			report.OpenWriteCount = len(rules.OpenWriteGlobs)
		}
		records, err := policy.LoadPlacementSourceAuthority(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report.SourceAuthority = records
		printJSON(report)
	case "import-standing-grants":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-standing-grants <standing-grants.yaml|json>")
			fmt.Fprintln(os.Stderr, "  the source must carry an authority block citing the authorizing decision record (authority.decision_ref + authority.reviewed_by operator:<id>)")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportStandingGrants(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "standing-grants-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy standing-grants-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type standingGrantsAuthorityReport struct {
			AuthorityRef     string                                       `json:"authority_ref"`
			AuthorityPath    string                                       `json:"authority_path"`
			Installed        bool                                         `json:"installed"`
			GeneratedAt      string                                       `json:"generated_at,omitempty"`
			GrantCount       int                                          `json:"grant_count,omitempty"`
			CapabilityCount  int                                          `json:"capability_count,omitempty"`
			Grants           []policy.StandingGrant                       `json:"grants,omitempty"`
			SourceAuthority  []policy.StandingGrantsSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath string                                       `json:"source_record_path"`
		}
		report := standingGrantsAuthorityReport{
			AuthorityRef:     policy.StandingGrantsAuthorityRef,
			AuthorityPath:    policy.StandingGrantsPath(homePath),
			SourceRecordPath: policy.StandingGrantsSourceAuthorityPath(homePath),
		}
		registry, err := policy.LoadStandingGrantsFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.GeneratedAt = registry.GeneratedAt
			report.GrantCount = len(registry.Grants)
			report.CapabilityCount = len(registry.Grants)
			report.Grants = registry.Grants
		}
		records, err := policy.LoadStandingGrantsSourceAuthority(homePath)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report.SourceAuthority = records
		if len(args) == 2 && args[1] == "--json" {
			printJSON(report)
			return
		}
		fmt.Printf("standing grants authority: %s\n", report.AuthorityRef)
		fmt.Printf("installed: %t\n", report.Installed)
		fmt.Printf("grants: %d capabilities=%d\n", report.GrantCount, report.CapabilityCount)
	case "import-operator-state":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-operator-state <operator-state.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportOperatorState(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "import-operator-prompts":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-operator-prompts <operator-decision-prompts.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportOperatorPrompts(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "operator-prompts-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy operator-prompts-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type operatorPromptsAuthorityReport struct {
			AuthorityRef      string                                        `json:"authority_ref"`
			AuthorityPath     string                                        `json:"authority_path"`
			Installed         bool                                          `json:"installed"`
			GeneratedAt       string                                        `json:"generated_at,omitempty"`
			PromptCount       int                                           `json:"prompt_count,omitempty"`
			HighSeverityCount int                                           `json:"high_severity_count,omitempty"`
			Prompts           []policy.OperatorPrompt                       `json:"prompts,omitempty"`
			SourceAuthority   []policy.OperatorPromptsSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath  string                                        `json:"source_record_path"`
		}
		report := operatorPromptsAuthorityReport{
			AuthorityRef:     policy.OperatorPromptsAuthorityRef,
			AuthorityPath:    policy.OperatorPromptsPath(homePath),
			SourceRecordPath: policy.OperatorPromptsSourceAuthorityPath(homePath),
		}
		registry, err := policy.LoadOperatorPromptsFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.GeneratedAt = registry.GeneratedAt
			report.PromptCount = len(registry.Prompts)
			report.HighSeverityCount = 0
			for _, prompt := range registry.Prompts {
				if strings.EqualFold(strings.TrimSpace(prompt.Severity), "high") {
					report.HighSeverityCount++
				}
			}
			report.Prompts = registry.Prompts
		}
		records, err := policy.LoadOperatorPromptsSourceAuthority(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.SourceAuthority = records
		}
		printJSON(report)
	case "import-operator-approval-frontier":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-operator-approval-frontier <operator-approval-frontier-summary.json>")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportOperatorApprovalFrontier(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "operator-approval-frontier-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy operator-approval-frontier-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type operatorApprovalFrontierAuthorityReport struct {
			AuthorityRef              string                                                 `json:"authority_ref"`
			AuthorityPath             string                                                 `json:"authority_path"`
			Installed                 bool                                                   `json:"installed"`
			GeneratedAt               string                                                 `json:"generated_at,omitempty"`
			RunID                     string                                                 `json:"run_id,omitempty"`
			Status                    string                                                 `json:"status,omitempty"`
			FamilyCount               int                                                    `json:"family_count,omitempty"`
			DeferredCount             int                                                    `json:"deferred_count,omitempty"`
			ApprovalRequiredOpenCount int                                                    `json:"approval_required_open_count,omitempty"`
			Families                  []policy.OperatorApprovalFamily                        `json:"families,omitempty"`
			CurrentState              map[string]interface{}                                 `json:"current_state,omitempty"`
			SourceAuthority           []policy.OperatorApprovalFrontierSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath          string                                                 `json:"source_record_path"`
		}
		report := operatorApprovalFrontierAuthorityReport{
			AuthorityRef:     policy.OperatorApprovalFrontierAuthorityRef,
			AuthorityPath:    policy.OperatorApprovalFrontierPath(homePath),
			SourceRecordPath: policy.OperatorApprovalFrontierSourceAuthorityPath(homePath),
		}
		frontier, err := policy.LoadOperatorApprovalFrontierFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.GeneratedAt = frontier.GeneratedAt
			report.RunID = frontier.RunID
			report.Status = frontier.Status
			report.FamilyCount = len(frontier.Families)
			report.DeferredCount = 0
			for _, family := range frontier.Families {
				if strings.EqualFold(strings.TrimSpace(family.CurrentDecision), "defer") {
					report.DeferredCount++
				}
			}
			report.ApprovalRequiredOpenCount = frontier.ApprovalRequiredOpenCount
			report.Families = frontier.Families
			report.CurrentState = frontier.CurrentState
		}
		records, err := policy.LoadOperatorApprovalFrontierSourceAuthority(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.SourceAuthority = records
		}
		printJSON(report)
	case "import-release-goal-holds":
		sourcePath := ""
		gateLive := policy.DefaultReleaseGoalHoldsGateLive
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--gate-live":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "usage: compound policy import-release-goal-holds <release-orchestrator-state.sqlite> [--gate-live TS]")
					os.Exit(2)
				}
				gateLive = args[i+1]
				i++
			default:
				if sourcePath != "" {
					fmt.Fprintln(os.Stderr, "usage: compound policy import-release-goal-holds <release-orchestrator-state.sqlite> [--gate-live TS]")
					os.Exit(2)
				}
				sourcePath = args[i]
			}
		}
		if sourcePath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound policy import-release-goal-holds <release-orchestrator-state.sqlite> [--gate-live TS]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report, err := policy.ImportReleaseGoalHolds(homePath, sourcePath, gateLive, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printJSON(report)
	case "release-goal-holds-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy release-goal-holds-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type releaseGoalHoldsAuthorityReport struct {
			AuthorityRef     string                                         `json:"authority_ref"`
			AuthorityPath    string                                         `json:"authority_path"`
			Installed        bool                                           `json:"installed"`
			GeneratedAt      string                                         `json:"generated_at,omitempty"`
			SourceDB         string                                         `json:"source_db,omitempty"`
			GateLive         string                                         `json:"gate_live,omitempty"`
			HoldCount        int                                            `json:"hold_count,omitempty"`
			Holds            []policy.ReleaseGoalHold                       `json:"holds,omitempty"`
			SourceAuthority  []policy.ReleaseGoalHoldsSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath string                                         `json:"source_record_path"`
		}
		report := releaseGoalHoldsAuthorityReport{
			AuthorityRef:     policy.ReleaseGoalHoldsAuthorityRef,
			AuthorityPath:    policy.ReleaseGoalHoldsPath(homePath),
			SourceRecordPath: policy.ReleaseGoalHoldsSourceAuthorityPath(homePath),
		}
		authority, err := policy.LoadReleaseGoalHoldsFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.GeneratedAt = authority.GeneratedAt
			report.SourceDB = authority.SourceDB
			report.GateLive = authority.GateLive
			report.HoldCount = authority.HoldCount
			report.Holds = authority.Holds
		}
		records, err := policy.LoadReleaseGoalHoldsSourceAuthority(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.SourceAuthority = records
		}
		printJSON(report)
	case "operator-authority":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy operator-authority [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		type operatorAuthorityReport struct {
			AuthorityRef     string                                      `json:"authority_ref"`
			GeneratedAt      string                                      `json:"generated_at"`
			AuthorityPath    string                                      `json:"authority_path"`
			Installed        bool                                        `json:"installed"`
			AutonomyLevel    string                                      `json:"autonomy_level,omitempty"`
			Mode             string                                      `json:"mode,omitempty"`
			NeedsMeCount     int                                         `json:"needs_me_count,omitempty"`
			BlockedCount     int                                         `json:"blocked_count,omitempty"`
			SourceAuthority  []policy.OperatorStateSourceAuthorityRecord `json:"source_authority,omitempty"`
			SourceRecordPath string                                      `json:"source_record_path"`
		}
		report := operatorAuthorityReport{
			AuthorityRef:     policy.OperatorStateAuthorityRef,
			GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
			AuthorityPath:    policy.OperatorStatePath(homePath),
			SourceRecordPath: policy.OperatorStateSourceAuthorityPath(homePath),
		}
		state, err := policy.LoadOperatorStateFromHome(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.Installed = true
			report.AutonomyLevel = policy.OperatorStateLevel(state)
			report.Mode = state.Mode
			report.NeedsMeCount = len(state.NeedsMe)
			report.BlockedCount = len(state.Blocked)
		}
		records, err := policy.LoadOperatorStateSourceAuthority(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			report.SourceAuthority = records
		}
		printJSON(report)
	case "autonomy-halt":
		const autonomyHaltUsage = "usage: compound policy autonomy-halt [--json | --global --reason <text> | --clear-global | --disable-class <class> --reason <text> | --enable-class <class>]"
		jsonOut := false
		setGlobal := false
		clearGlobal := false
		disableClass := ""
		enableClass := ""
		reason := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--json":
				jsonOut = true
			case "--global":
				setGlobal = true
			case "--clear-global":
				clearGlobal = true
			case "--disable-class":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, autonomyHaltUsage)
					os.Exit(2)
				}
				i++
				disableClass = strings.TrimSpace(args[i])
			case "--enable-class":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, autonomyHaltUsage)
					os.Exit(2)
				}
				i++
				enableClass = strings.TrimSpace(args[i])
			case "--reason":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, autonomyHaltUsage)
					os.Exit(2)
				}
				i++
				reason = strings.TrimSpace(args[i])
			default:
				fmt.Fprintln(os.Stderr, autonomyHaltUsage)
				os.Exit(2)
			}
		}
		mutationCount := 0
		for _, active := range []bool{setGlobal, clearGlobal, disableClass != "", enableClass != ""} {
			if active {
				mutationCount++
			}
		}
		if mutationCount > 1 || ((setGlobal || disableClass != "") && reason == "") || ((!setGlobal && disableClass == "") && reason != "") {
			fmt.Fprintln(os.Stderr, autonomyHaltUsage)
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if mutationCount == 1 {
			err := policy.UpdateAutonomyHalt(homePath, func(halt *policy.AutonomyHalt) error {
				switch {
				case setGlobal:
					halt.Global = true
					halt.Reason = reason
				case clearGlobal:
					halt.Global = false
					halt.Reason = ""
				case disableClass != "":
					halt.DisabledClasses[disableClass] = reason
				case enableClass != "":
					delete(halt.DisabledClasses, enableClass)
				}
				return nil
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "policy autonomy-halt:", err)
				os.Exit(1)
			}
		}
		type autonomyHaltClass struct {
			Class  string `json:"class"`
			Reason string `json:"reason,omitempty"`
		}
		type autonomyHaltReport struct {
			AuthorityRef       string              `json:"authority_ref"`
			AuthorityPath      string              `json:"authority_path"`
			Source             string              `json:"source"`
			Installed          bool                `json:"installed"`
			GlobalHalt         bool                `json:"global_halt"`
			Reason             string              `json:"reason,omitempty"`
			DisabledClasses    []autonomyHaltClass `json:"disabled_classes,omitempty"`
			DisabledClassCount int                 `json:"disabled_class_count"`
		}
		halt, source, err := policy.LoadAutonomyHalt(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report := autonomyHaltReport{
			AuthorityRef:  policy.AutonomyHaltAuthorityRef,
			AuthorityPath: policy.AutonomyHaltPath(homePath),
			Source:        source,
			Installed:     source != "unset",
			GlobalHalt:    halt.Global,
			Reason:        halt.Reason,
		}
		for class, reason := range halt.DisabledClasses {
			report.DisabledClasses = append(report.DisabledClasses, autonomyHaltClass{Class: class, Reason: reason})
		}
		sort.Slice(report.DisabledClasses, func(i, j int) bool { return report.DisabledClasses[i].Class < report.DisabledClasses[j].Class })
		report.DisabledClassCount = len(report.DisabledClasses)
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("autonomy halt authority: %s\n", report.AuthorityRef)
		fmt.Printf("source: %s\n", report.Source)
		fmt.Printf("global_halt: %t\n", report.GlobalHalt)
		if report.Reason != "" {
			fmt.Printf("reason: %s\n", report.Reason)
		}
		fmt.Printf("disabled_classes: %d\n", report.DisabledClassCount)
		for _, dc := range report.DisabledClasses {
			if dc.Reason != "" {
				fmt.Printf("  - %s: %s\n", dc.Class, dc.Reason)
			} else {
				fmt.Printf("  - %s\n", dc.Class)
			}
		}
	case "presence":
		os.Exit(policy.PresenceCLI(args[1:], os.Stdout, os.Stderr, time.Now()))
	case "editable-surfaces":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound policy editable-surfaces [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		reg, source, err := policy.LoadEditableSurfaces(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		report := buildEditableSurfacesReport(reg, source, policy.EditableSurfacesPath(homePath))
		if len(args) == 2 && args[1] == "--json" {
			printJSON(report)
			return
		}
		fmt.Printf("editable surfaces authority: %s\n", report.AuthorityRef)
		fmt.Printf("source: %s\n", report.Source)
		fmt.Printf("installed: %t\n", report.Installed)
		fmt.Printf("valid: %t\n", report.Valid)
		fmt.Printf("nothing_editable: %t\n", report.NothingEditable)
		fmt.Printf("hard_deny_core: %s\n", strings.Join(report.HardDenyCore, ", "))
		fmt.Printf("entries: %d (active: %d)\n", report.EntryCount, report.ActiveCount)
		for _, e := range report.Entries {
			fmt.Printf("  - %s [%s] class=%s", e.Surface, e.Status, e.ProposalClass)
			if e.Description != "" {
				fmt.Printf(" — %s", e.Description)
			}
			fmt.Println()
		}
		for _, ve := range report.ValidationErrors {
			fmt.Printf("  ! validation_error: %s\n", ve)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: compound policy <evaluate|action-plan|action-approve|action-check|import-placement-rules|placement-authority|import-operator-state|operator-authority|import-operator-prompts|operator-prompts-authority|import-operator-approval-frontier|operator-approval-frontier-authority|import-release-goal-holds|release-goal-holds-authority|autonomy-halt|presence|editable-surfaces|classify> ...")
		os.Exit(2)
	}
}
