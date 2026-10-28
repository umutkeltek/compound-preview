package harness

import (
	"time"

	"compound/internal/runattempt"
)

// PrimeAgentAdapter is the prime-agent instantiation of the pi coding-agent
// family adapter (pi_coding_agent_adapter.go). It is an alias rather than a
// wrapper type so the family's method set stays single-owner: one buildCmd,
// one Run, one dry-run formatter, no per-fork copy to drift.
type PrimeAgentAdapter = PiCodingAgentAdapter

// primeAgentSpec pins prime-agent's two environment facts.
//
// PRIME_AGENT_CODING_AGENT_DIR is the per-process home. Upstream computes the
// var name as `${envPrefix}_CODING_AGENT_DIR` from its package config
// (dist/config.js; its own comment reads "e.g., PI_CODING_AGENT_DIR or
// PRIME_AGENT_CODING_AGENT_DIR"), so the prefixed form is the one prime-agent
// reads. The unprefixed PRIME_AGENT_HOME does NOT exist upstream — setting it
// would silently share the real agent home across runs.
//
// PI_SKIP_VERSION_CHECK=1 stops prime-agent self-updating mid-run. A mid-run
// update is a different binary's audit trail and would invalidate the
// RunAttempt chrome. oh-my-pi has no equivalent env, which is why this lives in
// the spec rather than in the shared buildCmd.
var primeAgentSpec = piCodingAgentSpec{
	name:           "prime_agent",
	binary:         "prime-agent",
	homeEnv:        "PRIME_AGENT_CODING_AGENT_DIR",
	homeTempPrefix: "prime-agent-",
	extraEnv:       []string{"PI_SKIP_VERSION_CHECK=1"},
}

// NewPrimeAgentAdapter returns the production adapter. Heartbeats every 5s.
func NewPrimeAgentAdapter(store *runattempt.Store) *PrimeAgentAdapter {
	return newPiCodingAgentAdapter(primeAgentSpec, store, defaultHeartbeatInterval)
}

// NewPrimeAgentAdapterWithInterval returns an adapter with a custom heartbeat
// cadence. Test-only.
func NewPrimeAgentAdapterWithInterval(store *runattempt.Store, interval time.Duration) *PrimeAgentAdapter {
	return newPiCodingAgentAdapter(primeAgentSpec, store, interval)
}

// FormatPrimeAgentDryRunPrompt is the canonical argv-printed summary the
// verifier prints when running a prime-agent `--dry-run` proof.
func FormatPrimeAgentDryRunPrompt(in RunInput) string {
	return formatPiCodingAgentDryRunPrompt(primeAgentSpec.binary, in)
}
