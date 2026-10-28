package hook

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/work"
)

// TestLiveUserPromptScaleStaysInsideCPUTimeBudget is the always-on sibling of the load-aware
// wall-clock test. While TestLiveUserPromptScaleStaysInsideNativeBudget SKIPS its hard
// assertion under heavy machine load (a contested CI runner can blow the wall-clock budget
// even with a perfect hook), this sibling asserts the SAME property in CPU time and never
// skips. The two together pin the hook's prompt latency: wall-clock below 1.2s p95 under
// quiet conditions, and CPU-time always below a budget — the property is never entirely
// unguarded.
//
// The budget is in reference-host CPU time. A calibration workload runs right before every
// prompt and once after the last; promptScaleNormalizeCPU divides each prompt's CPU time by
// how much slower than the reference host the bracketing calibration samples ran. A slower
// or efficiency core inflates both and cancels out; a more expensive hook inflates only the
// prompt and still trips the budget (see prompt_scale_calibration_test.go for the evidence
// and TestPromptScaleCPUNormalizationKeepsTheBudgetBiting for the falsifier). On the
// reference host the factor is ~1, so the absolute 500ms p95 / 1s max budget is unchanged.
// Under the load signal the legacy 4x contention ceiling still applies to raw CPU time.
//
// Like its wall-clock sibling it uses fixedNow so seeded timestamps stay deterministic.
func TestLiveUserPromptScaleStaysInsideCPUTimeBudget(t *testing.T) {
	homePath := t.TempDir()
	seedPromptScaleMemory(t, homePath)
	seedPromptScaleEntityHistory(t, homePath)
	seedPromptScaleCapabilities(t, homePath)
	assertPromptScaleShape(t, homePath)

	embedder := newPromptScaleEmbedServer(t)
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "prompt-scale-test")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", embedder.server.URL)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")

	if _, err := contract.MaterializeRuntime(contract.Input{
		Harness: "codex", Cwd: "/repo/compound", Task: "warm prompt runtime",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute", ReadOnlyRetrieval: true,
	}, contract.DefaultPolicy(), homePath, fixedNow); err != nil {
		t.Fatal(err)
	}
	embedder.resetTaskCounts()

	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	deps.Store = workStore
	if got := deps.Registry.InjectionBudgetMS("codex"); got != 1500 {
		t.Fatalf("codex injection budget = %dms, want pinned 1500ms", got)
	}

	cpuTimes := make([]time.Duration, 0, promptScaleRuns)
	calibration := make([]time.Duration, 0, promptScaleRuns+1)
	promptScaleCalibrationWork() // warm the calibration path once, unmeasured
	runtime.GC()
	for i := 0; i < promptScaleRuns; i++ {
		calibration = append(calibration, promptScaleCalibrationSample())
		harness := "codex"
		if i%2 == 1 {
			harness = "claude_code"
		}
		raw := fmt.Sprintf("PROMPT_SCALE_CPU_%02d accelerate worldwide shipping", i)
		// promptScalePerPromptCPUTime samples Rusage around the closure so the measurement
		// covers exactly the hook invocation, not the surrounding assertions or seed I/O.
		// The closure collects the prompt's own garbage (~3ms) so its GC debt is billed to
		// the prompt, not to the next calibration sample, which would hide an allocation
		// regression inside the host factor.
		var res Result
		var runErr error
		cpu := promptScalePerPromptCPUTime(func() {
			res, runErr = Run(Fixture{
				Hook: "user-prompt-submit", Harness: harness, Live: true,
				Cwd: "/repo/compound", RepoKind: "control-plane", Task: raw, Prompt: raw,
			}, deps, fixedNow.Add(time.Duration(i)*time.Second))
			runtime.GC()
		})
		cpuTimes = append(cpuTimes, cpu)
		if runErr != nil {
			t.Fatalf("prompt %d (%s): %v", i, harness, runErr)
		}
		if res.BreakerTripped || res.Event.Status == "fail_open" {
			// The breaker exists to fail open when a prompt exceeds its
			// injection budget — under a saturated parallel battery on a
			// 2-core runner that can happen with no regression (run
			// a 2-core CI run (its id is in git history): fail_open context_unavailable mid-loop). Under
			// the same contention signal the budget assertions relax, a
			// breaker trip skips; on a quiet machine it stays fatal —
			// that is the wall-clock regression signal this family owns.
			if load, ok := promptScaleLoadAverageOneMinute(); ok && load > float64(runtime.NumCPU()/2) {
				t.Skipf("prompt %d (%s) tripped breaker under load %.1f (NumCPU=%d): fail_open=%s", i, harness, load, runtime.NumCPU(), res.Event.FailOpenReason)
			}
			t.Fatalf("prompt %d (%s) tripped breaker: %+v", i, harness, res)
		}
	}

	calibration = append(calibration, promptScaleCalibrationSample())

	normalized, factors, err := promptScaleNormalizeCPU(cpuTimes, calibration, promptScaleCalibrationReference)
	if err != nil {
		t.Fatal(err)
	}
	p95CPU, maxCPU := promptScaleP95AndMax(normalized)
	rawP95, _ := promptScaleP95AndMax(cpuTimes)
	budget := promptScaleCPUTimeBudget()
	evidence := fmt.Sprintf("raw_p95=%s raw_cpu_times=%v calibration=%v host_factors=%.2f normalized=%v",
		rawP95, cpuTimes, calibration, factors, normalized)
	t.Logf("prompt scale CPU-time budget=%s p95=%s max=%s (reference-host time); %s", budget, p95CPU, maxCPU, evidence)
	// History, kept because it set these numbers. Before calibration the budget was raw CPU
	// time, and two hosts broke it with no regression:
	//   - a 2-core CI run (id in git history): p95 1.35s on a 2-core runner inside the parallel battery, vs
	//     the 500ms quiet budget the same binary met on an idle box (cache thrash and
	//     scheduler effects inflate cycles for identical work). That added the 4x
	//     contention ceiling below, under the load signal the wall-clock sibling skips on.
	//   - Verifier run 20261024T214850Z-7f8262: p95 875ms on a 10-core host with 6
	//     efficiency cores at load ~4, under the NumCPU/2 signal, so the quiet 500ms
	//     applied. That moved the quiet budget to reference-host CPU time.
	// The contention ceiling stays on RAW time, exactly as before calibration. Its 4x
	// already absorbs contention; dividing by the host factor as well would count the
	// same slowdown twice and loosen it. A real regression (an order of magnitude) still
	// trips it.
	loadSkipThreshold := runtime.NumCPU() / 2
	if loadSkipThreshold < 1 {
		loadSkipThreshold = 1
	}
	if load, ok := promptScaleLoadAverageOneMinute(); ok && load > float64(loadSkipThreshold) {
		if rawP95 > 4*budget {
			t.Fatalf("prompt p95 CPU time = %s under load %.1f (NumCPU=%d), want <= %s contention ceiling; %s", rawP95, load, runtime.NumCPU(), 4*budget, evidence)
		}
		return
	}
	if p95CPU > budget {
		t.Fatalf("prompt p95 reference-host CPU time = %s, want <= %s; %s", p95CPU, budget, evidence)
	}
	// Max budget is 2x p95 to leave headroom for one outlier under jitter.
	if maxCPU > 2*budget {
		t.Fatalf("prompt max reference-host CPU time = %s, want <= 2*%s; %s", maxCPU, budget, evidence)
	}
}
