package actuator

import (
	"compound/internal/harness"
	"compound/internal/runattempt"
)

const ompNativeVerifierHarness = "omp-native-verifier"

// prepareOMPNativeVerifierLaunch records an explicit prompt-only posture for
// the production governed-launch consumer. The generic reservation StartGuard
// remains authoritative for the exact work/policy/action generation; this
// marker prevents a native verifier attempt from being mistaken for a generic
// OMP command run in projections and audits.
func prepareOMPNativeVerifierLaunch(in GovernedHarnessLaunchInput, runInput harness.RunInput) (harness.RunInput, error) {
	if in.Adapter == nil || in.Adapter.Name() != ompNativeVerifierHarness {
		return runInput, nil
	}
	if err := harness.ValidateOMPNativeVerifierPreparedInput(runInput); err != nil {
		return harness.RunInput{}, err
	}
	runInput.ExecutionPosture = &runattempt.ExecutionPosture{
		PermissionMode:    "prompt_only",
		PolicyDecisionID:  in.RunningWork.PolicyDecisionID,
		AuthorityEvidence: []runattempt.ExecutionPostureEvidence{{Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"}},
	}
	return runInput, nil
}
