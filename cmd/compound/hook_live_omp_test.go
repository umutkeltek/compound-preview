package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

func TestLiveOMPFileToolIngressUsesCanonicalPolicyActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool, canonical, action string
		readOnly                bool
	}{
		{"write", "Write", "write_file", false},
		{"edit", "Edit", "edit_file", false},
		{"read", "Read", "", true},
		{"glob", "Glob", "", true},
		{"grep", "Grep", "", true},
	}
	for _, harness := range []string{"oh-my-pi", "oh_my_pi"} {
		for _, tc := range cases {
			t.Run(harness+"/"+tc.tool, func(t *testing.T) {
				target := filepath.Join(t.TempDir(), "sample.txt")
				input := claudeLiveHookInput{
					SessionID: "native-file-ingress", Cwd: filepath.Dir(target),
					ToolName: tc.tool, ToolInput: map[string]any{"path": target},
					ToolSchemaRevision: "fixture:native-schema-v1",
				}
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				fx, err := liveHookFixture(hookCommandArgs{HookName: "pre-tool-use", Harness: harness, Live: true}, body, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if fx.Tool != tc.canonical {
					t.Errorf("tool = %q, want %q", fx.Tool, tc.canonical)
				}
				if fx.Action == nil {
					t.Fatal("missing real live-ingress ActionIntent")
				}
				if fx.Action.SchemaRevision != input.ToolSchemaRevision {
					t.Error("explicit schema revision was overwritten")
				}
				if tc.readOnly {
					if fx.Action.MutationClass != "read" || fx.Action.Action == "use_tool" {
						t.Errorf("read-only native tool became a mutation: %+v", fx.Action)
					}
					if len(liveToolPaths(fx.Tool, input.ToolInput)) != 0 {
						t.Error("read-only native tool entered mutation target extraction")
					}
				} else if fx.Action.Action != tc.action || fx.Action.Capability != "filesystem.write" || fx.Action.Target.Path != target {
					t.Errorf("native file tool missed canonical file policy: %+v", fx.Action)
				}
				post, err := liveHookFixture(hookCommandArgs{HookName: "post-tool-use", Harness: harness, Live: true}, body, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if post.Tool != tc.canonical {
					t.Errorf("post-tool identity = %q, want %q", post.Tool, tc.canonical)
				}
				if tc.readOnly && post.MutationInputRef != "" {
					t.Error("read-only result recorded a pending mutation identity")
				}
				if !tc.readOnly {
					canonicalInput := input
					canonicalInput.ToolName = tc.canonical
					want := liveMutationInputReference(harness, canonicalInput, []string{target})
					if post.MutationInputRef != want {
						t.Errorf("post mutation identity = %q, want canonical %q", post.MutationInputRef, want)
					}
					found := false
					for _, ref := range fx.Action.EvidenceRefs {
						found = found || ref == want
					}
					if !found {
						t.Errorf("pre action did not share canonical post identity: %v", fx.Action.EvidenceRefs)
					}
				}
			})
		}
	}
}

func TestLiveOMPFileToolIngressPreservesUnknownAndOtherHarnessTools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ harness, tool string }{
		{"oh_my_pi", "WRITE"}, {"oh_my_pi", "read_file"}, {"oh_my_pi", "mcp__synthetic__read"},
		{"oh_my_pi", "custom_write"}, {"claude_code", "write"}, {"codex", "read"}, {"opencode", "edit"},
	} {
		t.Run(tc.harness+"/"+tc.tool, func(t *testing.T) {
			body, err := json.Marshal(claudeLiveHookInput{
				SessionID: "native-unknown", Cwd: "/synthetic-repo", ToolName: tc.tool,
				ToolInput: map[string]any{"path": "/synthetic-repo/sample.txt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			fx, err := liveHookFixture(hookCommandArgs{HookName: "pre-tool-use", Harness: tc.harness, Live: true}, body, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if fx.Tool != tc.tool {
				t.Errorf("unrelated tool changed from %q to %q", tc.tool, fx.Tool)
			}
			if fx.Action == nil || fx.Action.Action != "use_tool" {
				t.Errorf("unrecognized path tool borrowed native file semantics: %+v", fx.Action)
			}
		})
	}
}

func TestLiveOMPFileToolBoundaryDeniesMisplacedWritesWithoutAskingOnReads(t *testing.T) {
	homePath, repo := t.TempDir(), t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_PLACEMENT_GUARD_MODE", "block")
	t.Setenv("PLACEMENT_GUARD_MODE", "block")
	t.Setenv("PLACEMENT_OVERRIDE", "")
	now := time.Now().UTC()
	if err := home.Init(homePath, "native-file-boundary", now); err != nil {
		t.Fatal(err)
	}
	rules := policy.PlacementRules{Owners: []policy.PlacementOwnerRule{{
		ID: "decisions", Owner: "decisions", Status: "active", Confidence: "block",
		CanonicalWriteGlobs: []string{filepath.Join(repo, "decisions", "**")},
		CanonFilenameGlobs:  []string{"DEC-*.md"},
	}}}
	rulesJSON, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	rulesSource := filepath.Join(t.TempDir(), "placement-rules.json")
	if err := os.WriteFile(rulesSource, rulesJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportPlacementRules(homePath, rulesSource, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool   string
		denied bool
	}{
		{"write", true}, {"edit", true}, {"read", false}, {"glob", false}, {"grep", false},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			input, err := json.Marshal(claudeLiveHookInput{
				SessionID: "native-boundary-" + tc.tool, Cwd: repo, ToolName: tc.tool,
				ToolInput: map[string]any{"path": filepath.Join(repo, "DEC-0001.md"), "content": "synthetic fixture"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var exit int
			output := captureStdout(t, func() {
				withStdin(t, string(input), func() {
					exit = runHookCommand([]string{"pre-tool-use", "--live", "--harness", "oh-my-pi", "--enforce"})
				})
			})
			var response claudeLiveHookResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatalf("response: %v\n%s", err, output)
			}
			got := response.HookSpecificOutput
			if got.HookEventName != "PreToolUse" {
				t.Fatalf("wrong native event: %+v", got)
			}
			if tc.denied {
				if exit != 3 || got.PermissionDecision != "deny" || !strings.Contains(strings.ToLower(got.PermissionDecisionReason), "placement") {
					t.Fatalf("native file tool escaped placement denial: exit=%d response=%+v", exit, got)
				}
			} else if exit != 0 || got.PermissionDecision != "" {
				t.Fatalf("native read-only tool asked or denied: exit=%d response=%+v", exit, got)
			}
		})
	}
}

// TestOhMyPiHoldEmitsDenyWhenShimPresent is the R7B fail-closed falsifier
// for oh_my_pi: when the registry's PreToolUseBlock is true (the shim is
// present), a hold outcome MUST project to permissionDecision=deny on the
// live PreToolUse answer. The pre-fix failure shape printed
// "[oh_my_pi/fail_open] ... cannot block natively" to stderr -- a silent
// pass-through that the OMP shim then read as "no decision" and treated
// as an invalid permission decision (block=true, reason="compound policy
// returned an invalid permission decision"). The fix path is two-part:
// the registry row flips PreToolUseBlock=true (already in this packet),
// and hook_live.go emits permissionDecision=deny for oh_my_pi holds the
// same way it does for claude_minimax (the existing livePermissionDecision
// path through hook.NativePermissionDecisionForHarness covers this when
// the registry row reports PreToolUseBlock=true). This test exercises
// the path end-to-end through runHookCommand.
func TestOhMyPiHoldEmitsDenyWhenShimPresent(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Now().UTC()
	if err := home.Init(homePath, "omp-hold-deny", now); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(claudeLiveHookInput{
		SessionID: "omp-hold-deny",
		Cwd:       "/tmp",
		ToolName:  "bash",
		ToolInput: map[string]any{
			"command": "rm -rf /srv/sentinel-r7b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var exit int
	output := captureStdout(t, func() {
		withStdin(t, string(input), func() {
			exit = runHookCommand([]string{"pre-tool-use", "--live", "--harness", "oh-my-pi", "--enforce"})
		})
	})
	var response claudeLiveHookResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("response: %v\n%s", err, output)
	}
	got := response.HookSpecificOutput
	if got.HookEventName != "PreToolUse" {
		t.Fatalf("wrong native event: %+v", got)
	}
	// Post-fix expectation: permissionDecision=deny.
	if got.PermissionDecision != "deny" {
		t.Fatalf("oh_my_pi PreToolUse hold MUST project to permissionDecision=deny, got %q (response=%+v)", got.PermissionDecision, got)
	}
	if got.PermissionDecisionReason == "" {
		t.Fatalf("oh_my_pi deny MUST carry the policy reason, got empty")
	}
	// Exit code 3 = hold (gateExit). The OMP shim reads stdout for
	// permissionDecision=deny regardless of exit code, so 3 is fine.
	if exit != 0 && exit != 3 {
		t.Fatalf("hook exit must be 0 (allow) or 3 (hold); got %d", exit)
	}
}
