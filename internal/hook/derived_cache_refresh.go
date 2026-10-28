package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/negatives"
)

// DerivedCacheStaleAfter is how old a derived cache may get before PostToolUse
// rebuilds it. 6h matches negatives.StaleAfter and internal/routeevidence's
// account-projection window, so the system has ONE freshness window rather than a
// per-organ collection of them.
const DerivedCacheStaleAfter = 6 * time.Hour

// derivedCacheAttemptRetry throttles retries after a FAILED rebuild. Without it a
// permanently failing build would run on every single tool call.
const derivedCacheAttemptRetry = 30 * time.Minute

// refreshDerivedCaches rebuilds the capability-usage -> memory-temperature chain when
// it has gone stale, and returns the refs it rewrote.
//
// WHY POSTTOOLUSE AND NOT THE READ PATH. The chain terminates on the live orient
// path: contract.loadMemoryTemperatures reads memory-temperature on every prompt and
// feeds a per-claim rank contribution. The obvious fix — refresh on read, the way
// negatives.ReadFresh does — does not fit here, and the numbers are the reason.
// Measured on a real home: a rebuild costs ~0.40s per stage (~0.80s for the chain),
// while a normal user-prompt-submit injection already runs 0.88-1.46s against a
// 1500ms InjectionBudgetMS. Adding a stage would exceed the budget on the very prompt
// where it fired, trip the breaker, and degrade the context it was trying to improve.
// PostToolUse measures ~0.06s and builds no context, so it has the headroom, and it
// runs AFTER the tool result already exists — nothing is waiting on it.
//
// Observed before this existed: both bodies sat at 2026-07-10 for 373 hours with 31
// entries against ~19,090 claim rows, and the builder reported `consumed 0 / ignored 0`
// for every entry because its own input was equally frozen. Nothing rebuilt either
// stage, so the reinforcement half of memory ranking ran on a two-week-old snapshot —
// not absent, which would at least be neutral, but frozen, which biases toward
// whatever was warm two weeks ago.
//
// DEPENDENCY ORDER IS LOAD-BEARING: capability-usage feeds BuildTemperature
// (readInjectedRefUsage), so rebuilding temperature first would derive it from the
// stale usage body and report success while changing nothing that matters.
//
// NON-FATAL on every path. A refresh failure must never fail the tool event; the cost
// of skipping is a stale cache, which is the status quo it is improving on.
// LIVE ONLY. Fixture replay exists to replay a recorded event deterministically, and
// rebuilding ambient caches during a replay is out of scope for it in both directions:
// it makes the replay's cost depend on the temp home's state, and it makes the replay
// mutate state the fixture never described. Measured at ~110ms per fresh home, which a
// replay pays on every single invocation because a new home is always "never built".
// That is a per-replay tax buying nothing — the maintenance this performs is for a real
// session, not for a reconstruction of one.
func refreshDerivedCaches(homePath string, live bool, now time.Time) []string {
	if !live || strings.TrimSpace(homePath) == "" {
		return nil
	}
	// Fail-open: if the running binary cannot be resolved, fall back to window-only
	// staleness rather than refusing to refresh at all.
	codeBuiltAt, codeErr := home.RunningBinaryBuiltAt()
	if codeErr != nil {
		codeBuiltAt = time.Time{}
	}
	if !derivedCachesNeedRefresh(homePath, codeBuiltAt, now) {
		return nil
	}
	// Claim the attempt BEFORE building. Concurrent lanes all fire PostToolUse, and
	// while the writes are atomic (so a duplicate rebuild is harmless to readers),
	// letting every lane rebuild simultaneously would burn ~0.8s of CPU per lane for
	// one useful result.
	if !claimDerivedCacheAttempt(homePath, now) {
		return nil
	}

	refs := make([]string, 0, 2)
	// Stage 1: capability-usage. Its output is stage 2's input.
	usage, err := capgraph.BuildCapabilityUsage(homePath, capgraph.UsageBuildOptions{GeneratedAt: now})
	if err == nil {
		if _, werr := capgraph.WriteCapabilityUsage(homePath, usage); werr == nil {
			refs = append(refs, capgraph.CapabilityUsageProjectionRef)
		}
	}
	// Stage 2: temperature, which reads what stage 1 just wrote. Attempted even if
	// stage 1 failed — a temperature rebuild over an older usage body is still fresher
	// than leaving temperature itself two weeks old.
	temp, err := mem.BuildTemperature(homePath, mem.TemperatureBuildOptions{Now: now})
	if err == nil {
		if _, werr := mem.WriteTemperature(homePath, temp); werr == nil {
			refs = append(refs, mem.TemperatureProjectionRef)
		}
	}
	// negatives, independent of the chain above and measured at <10ms.
	//
	// Its READ path already self-refreshes (negatives.ReadFresh rebuilds in memory past
	// StaleAfter), so its persisted staleness never reaches a reader — but doctor's
	// criterion is "built by the running binary", which an in-memory rebuild cannot
	// satisfy. Left out, it would sit in the doctor report permanently after every
	// install, with no consequence and no automatic remedy: a standing row that teaches
	// the reader to skip the section. Refreshing it here costs nothing and lets the
	// section actually reach zero, which is what makes a non-zero reading mean something.
	if built, buildErr := negatives.Build(homePath, negatives.BuildOptions{Now: now}); buildErr == nil {
		if _, werr := negatives.WriteProjection(homePath, built); werr == nil {
			refs = append(refs, negatives.ProjectionRef)
		}
	}
	return refs
}

// refreshPostToolCaches runs only after the HookEvent is durable, so a Skill use
// can reach capability-usage before the same hook invocation returns. Ordinary
// tool calls keep the six-hour maintenance cadence.
func refreshPostToolCaches(homePath string, live, forceUsage bool, now time.Time) {
	refs := refreshDerivedCaches(homePath, live, now)
	if !live || !forceUsage || strings.TrimSpace(homePath) == "" {
		return
	}
	for _, ref := range refs {
		if ref == capgraph.CapabilityUsageProjectionRef {
			return
		}
	}
	usage, err := capgraph.BuildCapabilityUsage(homePath, capgraph.UsageBuildOptions{GeneratedAt: now})
	if err != nil {
		return
	}
	_, _ = capgraph.WriteCapabilityUsage(homePath, usage)
}

// derivedCachesNeedRefresh reports whether either stage is older than the window, OR
// predates the binary now running.
//
// The second criterion is what makes this agree with `compound doctor`, which asks
// "was this body built by the code I am about to trust?" (home.ProjectionFreshnessReport).
// Window-only staleness cannot answer that: right after installing a fix the bodies are
// minutes old — comfortably inside 6h — while being the pre-fix output the fix was
// supposed to replace. That is find_b27a3de99682 exactly, and it is the case where the
// operator most needs the rebuild to be automatic. Without this, doctor reports a gap
// that the refresh structurally cannot close, and the two guards contradict each other.
//
// Cost is bounded by how often a binary is installed, not by how often a tool runs: one
// ~0.8s tool call per install, then the bodies are newer than the binary and both guards
// go quiet.
//
// An UNREADABLE or undated body counts as needing a refresh, matching
// negatives.projectionStale: a body that cannot prove its own age must never be
// trusted as fresh.
// codeBuiltAt is INJECTED rather than read here, following the same rule as the clock:
// a function that resolves its own os.Executable() cannot be tested against a fixture,
// because the test binary is always newer than any fixture timestamp the test writes.
// A zero codeBuiltAt disables the binary comparison and leaves window-only staleness.
func derivedCachesNeedRefresh(homePath string, codeBuiltAt, now time.Time) bool {
	for _, rel := range []string{
		"projections/capability-usage.json",
		"projections/memory-temperature.json",
		"projections/negatives.json",
	} {
		stamp, ok := derivedCacheComputedAt(filepath.Join(homePath, filepath.FromSlash(rel)))
		if !ok || now.UTC().Sub(stamp.UTC()) > DerivedCacheStaleAfter {
			return true
		}
		if !codeBuiltAt.IsZero() && stamp.UTC().Before(codeBuiltAt.UTC()) {
			return true
		}
	}
	return false
}

func derivedCacheComputedAt(path string) (time.Time, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	var doc struct {
		ComputedAt  string `json:"computed_at"`
		GeneratedAt string `json:"generated_at"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return time.Time{}, false
	}
	for _, raw := range []string{doc.ComputedAt, doc.GeneratedAt} {
		if stamp, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
			return stamp, true
		}
	}
	return time.Time{}, false
}

func derivedCacheAttemptPath(homePath string) string {
	return filepath.Join(homePath, "state", "runs", "derived-cache-refresh", "last-attempt.json")
}

// claimDerivedCacheAttempt records that this process is about to rebuild, and reports
// whether the claim was won.
//
// Coarse on purpose. It is not a lock: two lanes can still race between the read and
// the write, and that is acceptable because the writes are atomic and a duplicate
// rebuild produces an identical valid body. What it prevents is the pathological case
// — every lane on the machine rebuilding on every tool call — and the retry storm from
// a build that fails every time.
func claimDerivedCacheAttempt(homePath string, now time.Time) bool {
	path := derivedCacheAttemptPath(homePath)
	if body, err := os.ReadFile(path); err == nil {
		var doc struct {
			AttemptedAt string `json:"attempted_at"`
		}
		if json.Unmarshal(body, &doc) == nil {
			if stamp, perr := time.Parse(time.RFC3339, strings.TrimSpace(doc.AttemptedAt)); perr == nil {
				if now.UTC().Sub(stamp.UTC()) < derivedCacheAttemptRetry {
					return false
				}
			}
		}
	}
	body, err := json.MarshalIndent(map[string]string{
		"schema_version": "compound.derived_cache_refresh_attempt.v0.1",
		"attempted_at":   now.UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	// Best effort: if the marker cannot be written, still rebuild. A missing marker
	// costs a possible duplicate rebuild; refusing to rebuild costs the staleness this
	// exists to remove.
	_ = os.WriteFile(path, body, 0o644)
	return true
}
