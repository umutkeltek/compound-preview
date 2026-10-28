package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const RemoteProofHelp = "  --remote-go-test FILE  sealed canonical general-release route; no local fallback\n  --remote-reconcile ID --work ID --actor ACTOR [--cancel]  reconcile exact persisted request"

type ProofCheckRouteOptions struct {
	RemoteRoute             string
	PublicRedactionActionID string
	// ExecutionTimeout is the optional bounded active-operation budget of the
	// canonical full proof run. Zero means the option was omitted.
	ExecutionTimeout time.Duration
}

// proofCheckOptionScan records every argv token exactly once and the value that
// follows the first occurrence of each token. It is the shared reading of the
// proof-check command line: option compatibility must not be re-derived by a
// second parser that can drift from this one.
func proofCheckOptionScan(args []string) (seen map[string]bool, values map[string]string, development bool) {
	seen = map[string]bool{}
	values = map[string]string{}
	for i, arg := range args {
		development = development || ProofCheckNonCertifyingMode([]string{arg}) != ""
		if !seen[arg] && i+1 < len(args) {
			values[arg] = args[i+1]
		}
		seen[arg] = true
	}
	return seen, values, development
}

// ProofCheckLightweightMode reports whether argv selects a lightweight,
// recovery, or timing mode. Those modes never run the canonical full battery,
// so a canonical-only obligation must be refused in them.
func ProofCheckLightweightMode(args []string) bool {
	seen, _, development := proofCheckOptionScan(args)
	return development || seen["--dry-run"] || seen["--ci"] || seen["--ci-console"] ||
		seen["--recover-legacy-retained"] || seen["--timings"]
}

// ParseProofCheckRouteOptions owns compatibility between proof obligations and
// lightweight/recovery modes. Other CLI options remain with their existing owners.
func ParseProofCheckRouteOptions(args []string) (ProofCheckRouteOptions, error) {
	seen, values, development := proofCheckOptionScan(args)
	options := ProofCheckRouteOptions{
		RemoteRoute:             strings.TrimSpace(values["--remote-go-test"]),
		PublicRedactionActionID: strings.TrimSpace(values["--redact-review"]),
	}
	lightweight := development || seen["--dry-run"] || seen["--ci"] || seen["--ci-console"] ||
		seen["--recover-legacy-retained"] || seen["--timings"]
	if seen["--remote-go-test"] && (options.RemoteRoute == "" || strings.HasPrefix(options.RemoteRoute, "--") || lightweight) {
		return options, fmt.Errorf("--remote-go-test requires exact route file and canonical full proof")
	}
	if seen["--redact-review"] && (options.PublicRedactionActionID == "" || strings.HasPrefix(options.PublicRedactionActionID, "--")) {
		return options, fmt.Errorf("--redact-review requires an exact recorded ActionIntent ID")
	}
	if options.PublicRedactionActionID != "" && lightweight {
		return options, fmt.Errorf("reviewed redaction requires canonical full proof, not a lightweight or recovery mode")
	}
	timeout, err := ParseProofCheckExecutionTimeout(args)
	if err != nil {
		return options, err
	}
	options.ExecutionTimeout = timeout
	return options, nil
}

func ProofResourceProfileForCanonicalPlanWithRemote(ctx context.Context, plan CanonicalProofPlan, homePath, routePath string, command RemoteProofCommand) (ProofResourceProfile, error) {
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil || routePath == "" {
		return profile, err
	}
	if err := ValidateRemoteProofDomain(homePath); err != nil {
		return ProofResourceProfile{}, err
	}
	return SealRemoteProofProfile(ctx, profile, routePath, command)
}

func ValidateRemoteProofSteps(ctx context.Context, steps []ProofCheckStep) error {
	if !RemoteProofRequired(ctx) {
		return nil
	}
	for _, step := range steps {
		if step.Name == "go-test" && step.RemoteEvidence != nil {
			return nil
		}
	}
	return fmt.Errorf("remote canonical gate has no terminal receipt; reconcile original reservation before fresh proof")
}

// ProofCheckInvocationIdentity preserves exact native argv as proof authority.
func ProofCheckInvocationIdentity(args []string) (string, error) {
	argv := append([]string{"compound", "proof-check"}, args...)
	body, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	return "argv-json:" + string(body), nil
}
