package routeevidencecli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"compound/internal/routeevidence"
)

// ExecutionProfileUsage is the operator-facing usage line for
// `compound route execution-profile`. It lives HERE (not in cmd/compound)
// because this subcommand's CLI body lives in this organ: the cmd layer stays
// a thin dispatch, and the cmd/compound LOC budget ratchet
// (internal/arch.checkCmdCompoundBudget) only lets cmd/compound shrink.
func ExecutionProfileUsage() string {
	return "usage: compound route execution-profile (--list|--profile ID [--role R] [--capability-class C] [--harness claude-code|codex|worker] [--transport native|switchback|local] [--route-alias A] [--model provider/model] [--semantic-effort economy|balanced|deep|max] [--effort E] [--orchestration direct|standard|ultracode]) [--json]"
}

// TryExecutionProfileCommand handles `compound route execution-profile ...`
// and reports whether it consumed the invocation. It exits the process with
// the subcommand's exit code on a non-zero result, mirroring the other route
// subcommand doors in this organ.
func TryExecutionProfileCommand(args []string) bool {
	if len(args) == 0 || args[0] != "execution-profile" {
		return false
	}
	code := RunExecutionProfileCommand(args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
	return true
}

// RunExecutionProfileCommand runs the execution-profile subcommand body over
// args, writing to stdout/stderr, and returns the process exit code (2 on
// flag/usage error, 0 otherwise). It resolves a built-in execution profile
// into an intent-addressed plan; it never executes anything.
func RunExecutionProfileCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("route execution-profile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profileID := flags.String("profile", "", "built-in execution profile id")
	role := flags.String("role", "", "execution role")
	capabilityClass := flags.String("capability-class", "", "capability class")
	harnessID := flags.String("harness", "", "client harness id")
	transportID := flags.String("transport", "", "transport id")
	routeAlias := flags.String("route-alias", "", "owner-qualified transport route alias")
	modelConstraint := flags.String("model", "", "fully qualified provider/model constraint")
	semanticEffort := flags.String("semantic-effort", "", "economy|balanced|deep|max")
	nativeEffort := flags.String("effort", "", "requested native effort")
	orchestration := flags.String("orchestration", "", "direct|standard|ultracode")
	privacyCeiling := flags.String("privacy-ceiling", "", "public|private|sensitive|secret")
	list := flags.Bool("list", false, "list built-in profiles")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "route execution-profile: unexpected arguments: %v\n", flags.Args())
		return 2
	}
	if *list {
		profiles := routeevidence.ListExecutionProfiles()
		if *jsonOutput {
			return encodeExecutionProfileJSON(stdout, stderr, profiles)
		}
		fmt.Fprintln(stdout, "# route execution-profiles")
		for _, profile := range profiles {
			fmt.Fprintf(stdout, "- profile: %s semantic=%s orchestration=%s default=%s/%s\n",
				profile.ID,
				profile.SemanticEffort,
				profile.OrchestrationPolicy,
				profile.DefaultClientHarness,
				profile.DefaultTransport,
			)
		}
		fmt.Fprintln(stdout, "- verdict: read_only_profile_catalog")
		return 0
	}
	if *profileID == "" {
		fmt.Fprintln(stderr, ExecutionProfileUsage())
		return 2
	}

	plan, err := routeevidence.ResolveExecutionProfile(routeevidence.ExecutionProfileIntent{
		ProfileID:           *profileID,
		Role:                *role,
		CapabilityClass:     *capabilityClass,
		ClientHarnessID:     *harnessID,
		TransportID:         *transportID,
		RouteAlias:          *routeAlias,
		ModelConstraint:     *modelConstraint,
		SemanticEffort:      *semanticEffort,
		NativeEffort:        *nativeEffort,
		OrchestrationPolicy: *orchestration,
		PrivacyCeiling:      *privacyCeiling,
	})
	if err != nil {
		fmt.Fprintln(stderr, "route execution-profile:", err)
		return 2
	}
	if *jsonOutput {
		return encodeExecutionProfileJSON(stdout, stderr, plan)
	}

	fmt.Fprintf(stdout, "# route execution-profile %s\n", plan.ID)
	fmt.Fprintf(stdout, "- profile: %s\n", plan.ProfileID)
	fmt.Fprintf(stdout, "- role: %s\n", plan.Role)
	fmt.Fprintf(stdout, "- capability_class: %s\n", plan.CapabilityClass)
	fmt.Fprintf(stdout, "- client_harness: %s\n", plan.ClientHarnessID)
	fmt.Fprintf(stdout, "- transport: %s\n", plan.TransportID)
	if plan.RouteAlias != "" {
		fmt.Fprintf(stdout, "- route_alias: %s\n", plan.RouteAlias)
	}
	if plan.ModelConstraint != "" {
		fmt.Fprintf(stdout, "- requested_model: %s\n", plan.ModelConstraint)
	}
	fmt.Fprintf(stdout, "- semantic_effort: %s\n", plan.SemanticEffort)
	fmt.Fprintf(stdout, "- native_effort: %s (%s)\n", plan.NativeEffort.Requested, plan.NativeEffort.Disposition)
	fmt.Fprintf(stdout, "- orchestration: %s\n", plan.OrchestrationPolicy)
	fmt.Fprintf(stdout, "- privacy_ceiling: %s\n", plan.PrivacyCeiling)
	fmt.Fprintf(stdout, "- effective_provider: %s\n", plan.EffectiveProvider)
	fmt.Fprintf(stdout, "- effective_model: %s\n", plan.EffectiveModel)
	fmt.Fprintf(stdout, "- effective_native_effort: %s\n", plan.EffectiveNativeEffort)
	maxUSD := "inherit"
	if plan.Budget.MaxUSD != nil {
		maxUSD = fmt.Sprintf("%.2f", *plan.Budget.MaxUSD)
	}
	fmt.Fprintf(stdout, "- budget: requests=%d input_tokens=%d output_tokens=%d duration_seconds=%d max_usd=%s parallelism=%d\n",
		plan.Budget.MaxRequests,
		plan.Budget.MaxInputTokens,
		plan.Budget.MaxOutputTokens,
		plan.Budget.MaxDurationSeconds,
		maxUSD,
		plan.Budget.MaxParallelism,
	)
	if plan.Checkpoint.Required {
		fmt.Fprintf(stdout, "- checkpoint: every_requests=%d persist_partial=%t\n", plan.Checkpoint.EveryRequests, plan.Checkpoint.PersistPartial)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(stdout, "- warning: %s\n", warning)
	}
	fmt.Fprintln(stdout, "- verdict: resolved_intent_not_execution_receipt")
	return 0
}

func encodeExecutionProfileJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, "route execution-profile: encode JSON:", err)
		return 1
	}
	return 0
}
