package distiller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"compound/internal/mem"
)

// ConcurrenceRunner is the rented-edge implementation of mem.ConcurrenceRunner:
// it asks the SAME content of one model lane and returns that lane's verdict. It
// reuses ChatCompletionJSONWithMeta (the existing OpenAI-compatible client that
// already understands Switchback route headers, retries, and JSON sanitization) —
// it opens NO parallel HTTP path. Mem owns the gate; this only rents model
// traffic, per "own spine; rent executors."
//
// Lane endpoints are resolved per-lane from the environment (matching the
// COMPOUND_MEM_ENGINE_BIN idiom used elsewhere in the CLI), keeping credentials
// out of tracked files:
//
//	COMPOUND_CONCUR_<LANE>_BASE_URL   (required; absence => lane not configured)
//	COMPOUND_CONCUR_<LANE>_MODEL      (required)
//	COMPOUND_CONCUR_<LANE>_API_KEY    (optional; empty is fine for a local tap)
//
// <LANE> is the lane id uppercased with non-alphanumerics folded to underscores
// (zai-direct => ZAI_DIRECT). An unconfigured or failing lane returns an error;
// mem.RunConcurrence records that as an abstention, so a missing/rate-limited free
// lane can never masquerade as agreement.

const (
	concurrenceEnvPrefix     = "COMPOUND_CONCUR_"
	concurrenceCallTimeout   = 60 * time.Second
	concurrenceVerdictSchema = "compound.distiller.concurrence_verdict.v0.1"
)

// ConcurrenceLane is one lane's resolved endpoint.
type ConcurrenceLane struct {
	BaseURL string
	Model   string
	APIKey  string
}

// concurrenceVerdictResponse is the strict JSON a witness lane must return.
type concurrenceVerdictResponse struct {
	SchemaVersion string `json:"schema_version,omitempty"`
	Verdict       string `json:"verdict"`
	Summary       string `json:"summary"`
}

// ConcurrenceLaneFromEnv resolves a lane's endpoint from the environment. ok is
// false when the lane is not configured (no base url / model).
func ConcurrenceLaneFromEnv(lane string) (ConcurrenceLane, bool) {
	key := concurrenceEnvKey(lane)
	cfg := ConcurrenceLane{
		BaseURL: strings.TrimSpace(os.Getenv(concurrenceEnvPrefix + key + "_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv(concurrenceEnvPrefix + key + "_MODEL")),
		APIKey:  strings.TrimSpace(os.Getenv(concurrenceEnvPrefix + key + "_API_KEY")),
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return cfg, false
	}
	return cfg, true
}

func concurrenceEnvKey(lane string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(lane) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ConcurrenceRunner rents model lanes to produce concurrence verdicts.
type ConcurrenceRunner struct {
	HTTPClient *http.Client
	// LookupLane resolves a lane id to its endpoint; defaults to
	// ConcurrenceLaneFromEnv. Injectable for tests.
	LookupLane func(lane string) (ConcurrenceLane, bool)
}

// NewConcurrenceRunner returns a runner that resolves lanes from the environment.
func NewConcurrenceRunner() *ConcurrenceRunner {
	return &ConcurrenceRunner{HTTPClient: http.DefaultClient, LookupLane: ConcurrenceLaneFromEnv}
}

func (r *ConcurrenceRunner) lookup(lane string) (ConcurrenceLane, bool) {
	if r.LookupLane != nil {
		return r.LookupLane(lane)
	}
	return ConcurrenceLaneFromEnv(lane)
}

func (r *ConcurrenceRunner) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// RunConcurrence asks one lane for its verdict on the shared probe content. Any
// failure (unconfigured, network, malformed verdict) yields an abstention-shaped
// answer plus an error; the caller (mem.RunConcurrence) treats that as abstain.
// Critically, a malformed or unrecognized verdict can NEVER be reported as
// "agree" — the safety invariant lives here as well as in the reducer.
func (r *ConcurrenceRunner) RunConcurrence(probe mem.ConcurrenceProbe, lane string) (mem.ConcurrenceAnswer, error) {
	cfg, ok := r.lookup(lane)
	if !ok {
		return mem.ConcurrenceAnswer{Lane: lane, Verdict: mem.ConcurrenceVerdictAbstain},
			fmt.Errorf("concurrence: lane %q not configured (set %s%s_BASE_URL and _MODEL)", lane, concurrenceEnvPrefix, concurrenceEnvKey(lane))
	}
	ctx, cancel := context.WithTimeout(context.Background(), concurrenceCallTimeout)
	defer cancel()

	content, _, _, meta, err := ChatCompletionJSONWithMeta(
		ctx, r.client(), cfg.BaseURL, cfg.APIKey, cfg.Model,
		concurrenceSystemPrompt(), concurrenceUserPrompt(probe),
	)
	if err != nil {
		return mem.ConcurrenceAnswer{Lane: lane, Verdict: mem.ConcurrenceVerdictAbstain,
				ModelProvider: meta.ResolvedProvider, ModelName: meta.ResolvedModel},
			fmt.Errorf("concurrence: lane %q call failed: %w", lane, err)
	}

	verdict, summary, err := parseConcurrenceVerdict(content)
	if err != nil {
		return mem.ConcurrenceAnswer{Lane: lane, Verdict: mem.ConcurrenceVerdictAbstain,
				ModelProvider: meta.ResolvedProvider, ModelName: meta.ResolvedModel,
				Summary: "unparseable verdict; treated as abstain"},
			fmt.Errorf("concurrence: lane %q returned an unparseable verdict: %w", lane, err)
	}
	return mem.ConcurrenceAnswer{
		Lane:          lane,
		ModelProvider: meta.ResolvedProvider,
		ModelName:     meta.ResolvedModel,
		Verdict:       verdict,
		Summary:       summary,
	}, nil
}

// parseConcurrenceVerdict strict-decodes a witness reply. An unrecognized verdict
// is mapped to abstain (never agree) so a chatty or confused model cannot inflate
// the agreement count.
func parseConcurrenceVerdict(content []byte) (verdict, summary string, err error) {
	var resp concurrenceVerdictResponse
	if err := json.Unmarshal(content, &resp); err != nil {
		return "", "", fmt.Errorf("decode verdict: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(resp.Verdict)) {
	case mem.ConcurrenceVerdictAgree:
		return mem.ConcurrenceVerdictAgree, strings.TrimSpace(resp.Summary), nil
	case mem.ConcurrenceVerdictDisagree:
		return mem.ConcurrenceVerdictDisagree, strings.TrimSpace(resp.Summary), nil
	case mem.ConcurrenceVerdictAbstain:
		return mem.ConcurrenceVerdictAbstain, strings.TrimSpace(resp.Summary), nil
	default:
		// Unrecognized => abstain, not agreement.
		return mem.ConcurrenceVerdictAbstain, "unrecognized verdict " + strconvQuote(resp.Verdict) + "; treated as abstain", nil
	}
}

func strconvQuote(s string) string { return "\"" + strings.TrimSpace(s) + "\"" }

func concurrenceSystemPrompt() string {
	return `You are one INDEPENDENT witness in a multi-model concurrence check.
You will be shown a claim/assertion under test. Decide strictly whether it holds.
Reply with ONLY a JSON object — no prose, no markdown fence — of the form:
{"schema_version":"` + concurrenceVerdictSchema + `","verdict":"agree|disagree|abstain","summary":"<one short reason>"}
Rules:
- "agree" means the claim holds.
- "disagree" means the claim does NOT hold; put the concrete blocker in "summary".
- "abstain" means you cannot determine it from the given content.
Never guess. If you are not sure, abstain.`
}

func concurrenceUserPrompt(probe mem.ConcurrenceProbe) string {
	return "Claim under test (id=" + strings.TrimSpace(probe.ClaimID) + "):\n" + probe.Content
}
