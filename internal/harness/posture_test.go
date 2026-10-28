package harness

import "testing"

// TestPostureSeparatesTheOperatorSessionFromEveryLane pins the one distinction
// posture exists to make. Getting it backwards on a single lane is enough to
// freeze that lane on an unanswerable prompt, so the roster is asserted by name
// rather than by spot check.
func TestPostureSeparatesTheOperatorSessionFromEveryLane(t *testing.T) {
	reg := DefaultRegistry()
	interactive := map[string]bool{"claude_code": true, "codex": true}

	for name := range reg.Harnesses {
		want := PostureUnattended
		if interactive[name] {
			want = PostureInteractive
		}
		if got := reg.Posture(name); got != want {
			t.Fatalf("posture(%q)=%q, want %q", name, got, want)
		}
	}

	// Every lane the adapter registry can dispatch into must be classified, not
	// merely absent-and-defaulted: an unclassified lane is one an operator never
	// decided about.
	for _, lane := range []string{
		"claude_minimax", "claude_qwen", "claude_kimi", "claude_zai", "claude_zai_full",
		"claude_neuralwatt", "claude_gpt", "claude_gpt56_sol_wpcom",
		"claude_gpt56_sol_wpcom_headroom", "claude_deepseek_flash_api",
		"claude_deepseek_flash_nvidia", "hermes", "opencode", "prime_agent",
	} {
		h, ok := reg.Get(lane)
		if !ok {
			t.Fatalf("lane %q is not in the default registry", lane)
		}
		if NormalizePosture(h.Posture) != PostureUnattended {
			t.Fatalf("lane %q declares posture %q, want an explicit %q", lane, h.Posture, PostureUnattended)
		}
	}
}

// TestUnknownHarnessDefaultsToUnattended: the fail-safe direction. A name the
// registry has never heard of is far more likely to be a lane than a person,
// and guessing "interactive" is the guess that produces a silent freeze.
func TestUnknownHarnessDefaultsToUnattended(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range []string{"", "claude_something_new", "generic", "not_a_harness"} {
		if got := reg.Posture(name); got != PostureUnattended {
			t.Fatalf("posture(%q)=%q, want %q", name, got, PostureUnattended)
		}
	}
	// A typo must not silently read as interactive either.
	if got := NormalizePosture("Interactve"); got != "" {
		t.Fatalf("NormalizePosture(typo)=%q, want empty so the default decides", got)
	}
	if got := NormalizePosture("  INTERACTIVE "); got != PostureInteractive {
		t.Fatalf("NormalizePosture is not case/space tolerant: %q", got)
	}
}

// TestHyphenatedHarnessNameResolvesPosture: lanes reach this code as
// `claude-minimax` from the CLI and `claude_minimax` from the registry. If only
// one form resolved, the other would fall through to the default -- which is
// the right answer for a lane, and the WRONG one for `claude-code`.
func TestHyphenatedHarnessNameResolvesPosture(t *testing.T) {
	reg := DefaultRegistry()
	if got := reg.Posture("claude-code"); got != PostureInteractive {
		t.Fatalf("posture(claude-code)=%q, want %q", got, PostureInteractive)
	}
	if got := reg.Posture("claude"); got != PostureInteractive {
		t.Fatalf("posture(claude)=%q, want %q", got, PostureInteractive)
	}
	if got := reg.Posture("claude-minimax"); got != PostureUnattended {
		t.Fatalf("posture(claude-minimax)=%q, want %q", got, PostureUnattended)
	}
}

// TestPostureIsPartOfTheRegistryIdentity: two registries that differ only in
// whether a lane's hold projects to ask or deny are not the same registry. If
// the revision hashed them alike, an instruction pack rendered against one
// would validate against the other.
func TestPostureIsPartOfTheRegistryIdentity(t *testing.T) {
	base := DefaultRegistry()
	flipped := DefaultRegistry()
	lane := flipped.Harnesses["claude_minimax"]
	lane.Posture = PostureInteractive
	flipped.Harnesses["claude_minimax"] = lane

	if RegistryRevision(base) == RegistryRevision(flipped) {
		t.Fatalf("registry revision is blind to posture: %s", RegistryRevision(base))
	}
}

// TestManifestPostureResolvesLikeTheBuiltIn: a registry loaded from a manifest
// that says nothing about posture must answer the same as the built-in one for
// the same name, or the two registries disagree about the same lane.
func TestManifestPostureResolvesLikeTheBuiltIn(t *testing.T) {
	manifest := RegistryManifest{
		SchemaVersion: RegistryManifestSchemaVersion,
		Harnesses: []ManifestHarness{
			{Name: "claude_code", InstructionMechanism: "settings + hooks"},
			{Name: "claude_minimax", InstructionMechanism: "profile settings"},
			{Name: "claude_qwen", InstructionMechanism: "profile settings", Posture: "interactive"},
		},
	}
	reg, err := manifest.Registry()
	if err != nil {
		t.Fatalf("manifest registry: %v", err)
	}
	if got := reg.Posture("claude_code"); got != PostureInteractive {
		t.Fatalf("undeclared claude_code posture=%q, want %q", got, PostureInteractive)
	}
	if got := reg.Posture("claude_minimax"); got != PostureUnattended {
		t.Fatalf("undeclared lane posture=%q, want %q", got, PostureUnattended)
	}
	// An explicit declaration is the operator's call and must win over the default.
	if got := reg.Posture("claude_qwen"); got != PostureInteractive {
		t.Fatalf("declared posture=%q was overridden by the default", got)
	}
}
