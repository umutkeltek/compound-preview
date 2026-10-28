package contract

import "testing"

func TestParseHookRegistrationTSIgnoresCommentSyntax(t *testing.T) {
	tests := map[string]string{
		"line comment":  "// Universal matching keeps authorization in Compound's deterministic monitor [ }.",
		"block comment": "/* Universal matching keeps authorization in Compound's deterministic monitor [ }. */",
	}
	for name, comment := range tests {
		t.Run(name, func(t *testing.T) {
			source := []byte(
				"export const HOOK_REGISTRATIONS: HookReg[] = [\n" +
					"  " + comment + "\n" +
					"  { harness: \"codex\", event: \"PreToolUse\", matcher: \"*\", file: \"compound\", command: \x60'${HOME}/.local/bin/compound' hook pre-tool-use --live --harness codex\x60 },\n" +
					"];\n",
			)

			manifest, err := parseHookRegistrationTS(source)
			if err != nil {
				t.Fatalf("parse hook registration with syntax in %s: %v", name, err)
			}
			if len(manifest.Hooks) != 1 {
				t.Fatalf("hook count = %d, want 1", len(manifest.Hooks))
			}
			hook := manifest.Hooks[0]
			if hook.Harness != "codex" || hook.Name != "PreToolUse" || hook.Matcher != "*" {
				t.Fatalf("parsed hook = %+v, want universal Codex PreToolUse", hook)
			}
		})
	}
}
