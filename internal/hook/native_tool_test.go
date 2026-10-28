package hook

import "testing"

func TestNormalizeNativeTool(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"oh_my_pi", "oh-my-pi", "claude_code", "codex", "opencode", "custom"} {
		for _, tc := range []struct{ native, canonical string }{
			{"read", "Read"}, {"write", "Write"}, {"edit", "Edit"},
			{"glob", "Glob"}, {"grep", "Grep"},
			{"Read", "Read"}, {"Write", "Write"}, {"Edit", "Edit"},
			{"Glob", "Glob"}, {"Grep", "Grep"},
			{"bash", "bash"}, {"Bash", "Bash"}, {"WRITE", "WRITE"},
			{"apply_patch", "apply_patch"},
			{"mcp__synthetic__read", "mcp__synthetic__read"},
			{"custom_write", "custom_write"}, {"read_file", "read_file"}, {"", ""},
		} {
			t.Run(harness+"/"+tc.native, func(t *testing.T) {
				want := tc.native
				if harness == "oh_my_pi" || harness == "oh-my-pi" {
					want = tc.canonical
				}
				got := NormalizeNativeTool(harness, tc.native)
				if got != want {
					t.Errorf("NormalizeNativeTool(%q, %q) = %q, want %q", harness, tc.native, got, want)
				}
				if twice := NormalizeNativeTool(harness, got); twice != got {
					t.Errorf("normalization is not idempotent: %q -> %q", got, twice)
				}
			})
		}
	}
}
