package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/localmodel"
)

func cmdLocalModels(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound local-models advise [--lm-studio-url U] [--required MODEL]... [--min-context MODEL=TOKENS]... [--host-context-limit N] [--idle-ttl DURATION] [--conserve-memory] [--max-loaded-models N] [--json]")
		os.Exit(2)
	}
	switch args[0] {
	case "advise":
		report := runLocalModelAdvise(args[1:])
		if hasFlag(args[1:], "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# local-models advise %s\n", report.Status)
		for _, finding := range report.Findings {
			blocking := ""
			if finding.Blocking {
				blocking = " blocking=true"
			}
			fmt.Printf("- %s action=%s desired=%d loaded=%d max=%d%s reason=%s\n", finding.ModelID, finding.Action, finding.DesiredContext, finding.LoadedContext, finding.MaxContext, blocking, finding.Reason)
			for _, step := range finding.RecommendedSteps {
				fmt.Printf("  next: %s\n", step)
			}
			if intent := finding.LifecycleIntent; intent != nil {
				fmt.Printf("  intent: owner=%s action=%s mutation_class=%s delegation=%s\n", intent.Owner, intent.Action, intent.MutationClass, intent.Delegation)
			}
		}
		if advice := report.PipelineAdvice; advice != nil {
			fmt.Printf("- pipeline mode=%s max_loaded=%d reason=%s\n", advice.Mode, advice.MaxConcurrentLoaded, advice.Reason)
			for _, step := range advice.Steps {
				fmt.Printf("  step %d: model=%s desired=%d", step.Order, step.ModelID, step.DesiredContext)
				if step.LoadIntent != nil {
					fmt.Printf(" load_intent=%s", step.LoadIntent.Action)
				}
				if step.ReleaseIntent != nil {
					fmt.Printf(" release_intent=%s", step.ReleaseIntent.Action)
				}
				fmt.Println()
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown local-models command %q\n", args[0])
		os.Exit(2)
	}
}

func runLocalModelAdvise(args []string) localmodel.Report {
	baseURL := strings.TrimSpace(os.Getenv("COMPOUND_PANEL_BASE_URL"))
	required := map[string]int{}
	policy := localmodel.Policy{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lm-studio-url":
			baseURL = requireLocalModelValue(args, &i, "--lm-studio-url")
		case "--required":
			model := strings.TrimSpace(requireLocalModelValue(args, &i, "--required"))
			if model != "" {
				if _, ok := required[model]; !ok {
					required[model] = 0
				}
			}
		case "--min-context":
			model, tokens := parseModelContext(requireLocalModelValue(args, &i, "--min-context"))
			required[model] = tokens
		case "--host-context-limit":
			policy.HostContextLimitTokens = parsePositiveInt(requireLocalModelValue(args, &i, "--host-context-limit"), "--host-context-limit")
		case "--idle-ttl":
			ttl, err := time.ParseDuration(requireLocalModelValue(args, &i, "--idle-ttl"))
			if err != nil || ttl < 0 {
				fmt.Fprintf(os.Stderr, "local-models advise: invalid --idle-ttl: %v\n", err)
				os.Exit(2)
			}
			policy.IdleTTL = ttl
			policy.Now = time.Now()
		case "--conserve-memory":
			policy.ConserveMemory = true
		case "--max-loaded-models":
			policy.MaxConcurrentLoaded = parsePositiveInt(requireLocalModelValue(args, &i, "--max-loaded-models"), "--max-loaded-models")
		case "--json":
		default:
			fmt.Fprintf(os.Stderr, "unknown local-models advise arg %q\n", args[i])
			os.Exit(2)
		}
	}

	if baseURL == "" {
		baseURL = localRoutingBaseURL()
	}
	if baseURL == "" {
		fmt.Fprintln(os.Stderr, "local-models advise: local provider base_url unavailable; pass --lm-studio-url or configure the local routing CLI")
		os.Exit(1)
	}

	models, err := (localmodel.LMStudioClient{BaseURL: baseURL}).Models(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "local-models advise:", err)
		os.Exit(1)
	}
	requirements := make([]localmodel.Requirement, 0, len(required))
	for _, model := range sortedStringKeys(required) {
		requirements = append(requirements, localmodel.Requirement{ModelID: model, MinContextTokens: required[model]})
	}
	return localmodel.Evaluate(models, requirements, policy)
}

func localRoutingBaseURL() string {
	cli := strings.TrimSpace(os.Getenv("COMPOUND_LOCAL_ROUTING_CLI"))
	if cli == "" {
		return ""
	}
	out, err := exec.Command(cli, "local", "current", "--json").Output()
	if err != nil {
		return ""
	}
	var payload struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.BaseURL)
}

func parseModelContext(raw string) (string, int) {
	model, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
	if !ok || strings.TrimSpace(model) == "" {
		fmt.Fprintln(os.Stderr, "local-models advise: --min-context must be MODEL=TOKENS")
		os.Exit(2)
	}
	return strings.TrimSpace(model), parsePositiveInt(value, "--min-context")
}

func parsePositiveInt(raw, flag string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "local-models advise: %s must be a non-negative integer\n", flag)
		os.Exit(2)
	}
	return n
}

func requireLocalModelValue(args []string, i *int, flag string) string {
	if *i+1 >= len(args) {
		fmt.Fprintf(os.Stderr, "local-models advise: %s requires value\n", flag)
		os.Exit(2)
	}
	*i = *i + 1
	return args[*i]
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
