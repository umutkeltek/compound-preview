package harness

import (
	"strings"
	"testing"

	"compound/internal/runattempt"
)

// The registry's stated rule is that a capability flag describes an INSTALLED
// binding, not a product's brochure. These tests are the falsifiers for the
// three external harnesses enrolled at a conservative floor: each one fails if
// a future edit raises a flag without the binding that justifies it.
//
// The failure they exist to prevent is the claude_minimax one: routing believes
// the intended end state, work goes to an executor that cannot honour it, and
// the failure looks like silence rather than an error.

// TestExternalHarnessFloorsClaimNoHookSurface pins the single most expensive
// over-claim. Neither of these two has an installed hook binding, so every hook
// flag must be false — including PreToolUseBlock, where assuming a block that
// does not exist is a silent fail-open rather than a missing feature.
func TestExternalHarnessFloorsClaimNoHookSurface(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range []string{"qwen_code", "deepseek_harness"} {
		h, ok := reg.Get(name)
		if !ok {
			t.Fatalf("DefaultRegistry() missing %q", name)
		}
		c := h.Capabilities
		for _, flag := range []struct {
			hook string
			got  bool
		}{
			{"SessionStart", c.SessionStartContext},
			{"UserPromptSubmit", c.UserPromptContext},
			{"PreToolUse", c.PreToolUseBlock},
			{"PostToolUse", c.PostToolUseEvent},
			{"Stop", c.StopFeedback},
			{"SessionEnd", c.SessionEndReceipt},
		} {
			if flag.got {
				t.Errorf("%s declares %s=true, but no hook binding is installed for it — "+
					"raise a hook flag only after `compound hook install` writes the binding AND an event is observed", name, flag.hook)
			}
		}
		if len(h.Limitations) == 0 {
			t.Errorf("%s has no limitations; a floor with no stated reason is indistinguishable from an unreviewed row", name)
		}
		if strings.TrimSpace(h.ExpectedVersion) == "" {
			t.Errorf("%s has no ExpectedVersion pin; an unpinned external harness self-updates into a different audit trail", name)
		}
	}
}

// TestOhMyPiShimRaisesOnlyObservedFlags pins the evidence split from the
// 2026-08-25 shim landing, revised 2026-08-26 by packet
// 2026-08-25-skills-graph-truth (slice 3): the compound-owned TS shim
// (oh-my-pi-hook-shim@2 at ~/.omp/agent/hooks/pre/compound.ts, installed by
// `compound hook install --harness oh-my-pi`) was observed end-to-end
// delivering UserPromptSubmit context injection (a live model named the
// injected ctx_ id), PostToolUse telemetry, and a SessionEnd receipt that
// now carries skills_fired evidence because the shim derives transcript_path
// from the omp session directory (F6). SessionStart has no context-return
// surface, and a PreToolUse deny was never observed blocking — those stay
// false. A future edit must argue with this table, not with vibes; the
// SessionEndReceipt=true claim in particular now has live proof in the
// 2026-08-25-skills-graph-truth.REPORT.md (live proof 3).
func TestOhMyPiShimRaisesOnlyObservedFlags(t *testing.T) {
	h, ok := DefaultRegistry().Get("oh_my_pi")
	if !ok {
		t.Fatal("DefaultRegistry() missing oh_my_pi")
	}
	c := h.Capabilities
	for _, flag := range []struct {
		hook string
		got  bool
		want bool
	}{
		{"SessionStartContext", c.SessionStartContext, false},
		{"UserPromptContext", c.UserPromptContext, true},
		{"PreToolUseBlock", c.PreToolUseBlock, true},
		{"PostToolUseEvent", c.PostToolUseEvent, true},
		{"StopFeedback", c.StopFeedback, false},
		{"SessionEndReceipt", c.SessionEndReceipt, true},
	} {
		if flag.got != flag.want {
			t.Errorf("oh_my_pi %s = %v, want %v — raise/lower only with a receipt the floor comment names", flag.hook, flag.got, flag.want)
		}
	}
	if h.Capabilities.NativeSkills != true {
		t.Error("oh_my_pi NativeSkills = false, want true (populated skills root discovered with zero wiring)")
	}
}

// TestExternalHarnessSkillAndMCPFloorsMatchInstalledBindings pins the exact
// asymmetry the intake found, so a future "make them consistent" edit has to
// argue with a test rather than with a comment.
//
//   - oh_my_pi: skills TRUE (it discovers the populated .claude/skills root
//     with zero wiring), MCP FALSE (none of its mcp.json paths exist).
//   - qwen_code: MCP TRUE (its user settings carry a live mcpServers entry),
//     skills FALSE (the loader is real but the directory is empty).
//   - deepseek_harness: both FALSE, and both are verified absences.
func TestExternalHarnessSkillAndMCPFloorsMatchInstalledBindings(t *testing.T) {
	reg := DefaultRegistry()
	for _, tc := range []struct {
		name             string
		wantNativeSkills bool
		wantMCP          bool
	}{
		{"oh_my_pi", true, false},
		{"qwen_code", false, true},
		{"deepseek_harness", false, false},
	} {
		h, ok := reg.Get(tc.name)
		if !ok {
			t.Fatalf("DefaultRegistry() missing %q", tc.name)
		}
		if got := h.Capabilities.NativeSkills; got != tc.wantNativeSkills {
			t.Errorf("%s NativeSkills = %v, want %v", tc.name, got, tc.wantNativeSkills)
		}
		if got := h.Capabilities.MCP; got != tc.wantMCP {
			t.Errorf("%s MCP = %v, want %v", tc.name, got, tc.wantMCP)
		}
	}
}

// TestPrimeAgentFloorDropsSupportedNotInstalledClaims is the repair falsifier.
// prime_agent declared NativeSkills+MCP while the user skills directory did not
// exist and settings.json carried no mcpServers key, and its doc-comment cited a
// `--mcp` daemon flag the CLI does not have. Both are corrected; this test
// fails if either regresses.
func TestPrimeAgentFloorDropsSupportedNotInstalledClaims(t *testing.T) {
	h, ok := DefaultRegistry().Get("prime_agent")
	if !ok {
		t.Fatal("DefaultRegistry() missing prime_agent")
	}
	if h.Capabilities != (Capabilities{}) {
		t.Fatalf("prime_agent capabilities = %+v, want the zero floor: no hook protocol exists, "+
			"and skills/MCP are supported-not-installed", h.Capabilities)
	}
	joined := strings.Join(h.Limitations, "\n")
	if !strings.Contains(joined, "mcpServers") {
		t.Error("prime_agent limitations must name the real MCP mechanism (the mcpServers settings key)")
	}
	if !strings.Contains(joined, "SUPPORTED-not-installed") {
		t.Error("prime_agent limitations must say skills/MCP are supported-not-installed, " +
			"so nobody routes skill-bearing work here expecting a body to load")
	}
}

// TestExpectedVersionPinsAreComparableByTheVersionProbe is the anti-noise
// guard. The doctor's harness_version_drift signal compares ExpectedVersion
// against parseSemverToken(<binary> --version). parseSemverToken understands
// X.Y.Z and nothing else, so a pin carrying a pre-release suffix could never
// equal its own observation: the signal would report drift forever on a machine
// running exactly the pinned build, and an always-on signal is one the operator
// learns to skip.
//
// The rule is therefore conditional, not absolute: a pin must round-trip
// through the parser only when the harness is actually probed. That is why
// deepseek_harness can honestly pin "0.1.0-rc.7" while staying out of
// binaryForHarness.
func TestExpectedVersionPinsAreComparableByTheVersionProbe(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range reg.Names() {
		h, _ := reg.Get(name)
		pin := strings.TrimSpace(h.ExpectedVersion)
		if pin == "" {
			continue
		}
		binary, probed := binaryForHarness[name]
		if !probed {
			continue
		}
		if got := parseSemverToken(pin); got != pin {
			t.Errorf("%s pins ExpectedVersion=%q and is probed via %q, but parseSemverToken recovers %q — "+
				"the pin can never equal its own observation, so harness_version_drift would fire forever",
				name, pin, binary, got)
		}
	}
}

// TestDeepseekHarnessStaysOutOfTheVersionProbe is the other half of the pair:
// it proves the exemption above is deliberate and still necessary, rather than
// an oversight that a later "complete the table" edit should close. If
// deepseek-harness ever ships a plain X.Y.Z, this test is the reminder to add
// the probe row.
func TestDeepseekHarnessStaysOutOfTheVersionProbe(t *testing.T) {
	h, ok := DefaultRegistry().Get("deepseek_harness")
	if !ok {
		t.Fatal("DefaultRegistry() missing deepseek_harness")
	}
	pin := strings.TrimSpace(h.ExpectedVersion)
	if parseSemverToken(pin) == pin {
		t.Fatalf("deepseek_harness pin %q now round-trips through parseSemverToken — "+
			"the pre-release exemption is obsolete; add it to binaryForHarness", pin)
	}
	if binary, probed := binaryForHarness["deepseek_harness"]; probed {
		t.Fatalf("deepseek_harness is mapped to binary %q while its pin %q cannot round-trip — "+
			"every probe would report false drift", binary, pin)
	}
}

// TestIdentityOnlyHarnessesHaveNoAdapter pins the boundary between "known" and
// "dispatchable". oh_my_pi shares the pi-family adapter and IS dispatchable;
// qwen_code and deepseek_harness are identity rows only. Registering an adapter
// for either without a proven headless contract would turn a legible refusal
// into a routing hole.
func TestIdentityOnlyHarnessesHaveNoAdapter(t *testing.T) {
	adapters := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	if _, ok := adapters.AdapterByName("oh_my_pi"); !ok {
		t.Error("oh_my_pi has a registry row and a proven family argv but no adapter — it cannot be dispatched")
	}
	for _, name := range []string{"qwen_code", "deepseek_harness"} {
		if _, ok := adapters.AdapterByName(name); ok {
			t.Errorf("%s has an adapter but its headless contract is not proven here; "+
				"see the DefaultRegistry comment before enrolling one", name)
		}
	}
}

// TestPiFamilyAdaptersShareOneImplementation is the anti-fork guard. The whole
// argument for adopting oh-my-pi cheaply is that it parameterises an adapter
// that already exists. If someone later copies buildCmd into a second type, the
// two will drift on the next dispatch-contract change and only one will be
// fixed.
func TestPiFamilyAdaptersShareOneImplementation(t *testing.T) {
	store := runattempt.Open(t.TempDir())
	prime := NewPrimeAgentAdapter(store)
	omp := NewOhMyPiAdapter(store)

	if prime.spec.name == omp.spec.name {
		t.Fatalf("both family members report name %q — the AdapterRegistry would hold one row", prime.spec.name)
	}
	if prime.spec.binary == omp.spec.binary {
		t.Fatalf("both family members default to binary %q", prime.spec.binary)
	}
	if prime.spec.homeEnv == omp.spec.homeEnv {
		t.Fatalf("both family members isolate through %q; one fork would read the other's home var and fall back to the shared agent dir", prime.spec.homeEnv)
	}
	// oh-my-pi has no version-check suppression env. Carrying prime-agent's
	// would be a claim nothing enforces.
	if len(omp.spec.extraEnv) != 0 {
		t.Fatalf("oh_my_pi spec carries extraEnv %v; no such env is read by that fork", omp.spec.extraEnv)
	}
	if len(prime.spec.extraEnv) == 0 {
		t.Fatal("prime_agent spec lost PI_SKIP_VERSION_CHECK; the binary would self-update mid-run and invalidate the RunAttempt chrome")
	}
}
