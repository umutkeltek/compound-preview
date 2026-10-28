package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

func runPolicyActionApprovalCommand(
	args []string,
	stdout, stderr io.Writer,
	now time.Time,
) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: compound policy <action-plan|action-approve|action-check> <id> [options]")
		return 2
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	store := policy.NewExactActionApprovalStore(homePath)
	jsonOutput := hasExactArgument(args[2:], "--json")

	switch args[0] {
	case "action-plan":
		ttl := 5 * time.Minute
		if raw, ok := exactFlagValue(args[2:], "--ttl"); ok {
			ttl, err = time.ParseDuration(raw)
			if err != nil {
				fmt.Fprintln(stderr, "action-plan: invalid --ttl:", err)
				return 2
			}
		}
		plan, ref, err := store.Plan(args[1], cfg, ttl, now)
		if err != nil {
			fmt.Fprintln(stderr, "action-plan:", err)
			return 1
		}
		if jsonOutput {
			printJSONTo(stdout, plan)
			return 0
		}
		fmt.Fprintf(stdout, "exact action approval plan: %s\nbinding: %s\nexpires: %s\nref: %s\n",
			plan.ID, plan.BindingDigest, plan.ExpiresAt, ref)
	case "action-approve":
		grantedBy, ok := exactFlagValue(args[2:], "--granted-by")
		if !ok || strings.TrimSpace(grantedBy) == "" {
			fmt.Fprintln(stderr, "usage: compound policy action-approve <plan-id> --granted-by operator:<id> [--json]")
			return 2
		}
		approval, ref, err := store.Approve(args[1], grantedBy, cfg, now)
		if err != nil {
			fmt.Fprintln(stderr, "action-approve:", err)
			return 1
		}
		if jsonOutput {
			printJSONTo(stdout, approval)
			return 0
		}
		fmt.Fprintf(stdout, "exact one-use approval: %s\nexpires: %s\nref: %s\n",
			approval.ID, approval.ExpiresAt, ref)
	case "action-check":
		status, err := store.Check(args[1], now)
		if err != nil {
			fmt.Fprintln(stderr, "action-check:", err)
			return 1
		}
		if jsonOutput {
			printJSONTo(stdout, status)
			return 0
		}
		fmt.Fprintf(stdout, "exact action approval: %s\nstatus: %s\nexpires: %s\n",
			status.ID, status.Status, status.ExpiresAt)
	default:
		fmt.Fprintln(stderr, "usage: compound policy <action-plan|action-approve|action-check> <id> [options]")
		return 2
	}
	return 0
}

func exactFlagValue(args []string, name string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasExactArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func printJSONTo(writer io.Writer, value any) {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
