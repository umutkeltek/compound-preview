package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

func TestLiveOrdinarySystemWorkCannotWritePermissionAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		enforce bool
		deny    bool
	}{
		{"canonical permission authority", "authority", true, true},
		{"non-Git parent symlink", "alias", true, true},
		{"outward leaf symlink", "leaf", true, true},
		{"direct escaped referent is not authority", "referent", true, false},
		{"new file through parent symlink", "new_alias", true, true},
		{"mixed patch with protected target", "batch", true, true},
		{"permission-looking sibling", "sibling", true, false},
		{"missing persisted binding", "unbound", true, true},
		{"corrupt persisted work", "corrupt", true, true},
		{"stored work identity mismatch", "identity", true, true},
		{"invalid persisted envelope", "invalid", true, true},
		{"ordinary README", "readme", true, false},
		{"forbidden environment file", "env", true, true},
		{"observation remains observation", "authority", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, workspace := t.TempDir(), t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			now := time.Now().UTC()
			if err := home.Init(homePath, "synthetic", now); err != nil {
				t.Fatal(err)
			}
			authorityPath := harness.PermissionAuthorityPath(homePath, "codex")
			authority := harness.PermissionAuthority{
				SchemaVersion: harness.PermissionAuthoritySchemaVersion,
				Harness:       "codex",
				AuthorityRef:  harness.PermissionAuthorityRef("codex"),
				TargetPath:    filepath.Join(workspace, "config.toml"),
				Permissions:   harness.PermissionRules{Allow: []string{}, Deny: []string{}, Ask: []string{}},
				AutoMode:      harness.AutoModePolicy{Allow: []string{"$defaults"}},
			}
			if err := authority.Validate(); err != nil {
				t.Fatal(err)
			}
			authorityBytes, err := yaml.Marshal(authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(authorityPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(authorityPath, authorityBytes, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			item, err := store.Create(work.CreateInput{
				Owner: "director", State: "proposed",
				SuccessPredicate: "ordinary synthetic local task",
				ProposalKind:     envelope.ProposalKindOrdinary,
				EvidenceRefs:     []string{"proposed_by:director"},
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			item, err = store.Advance(item.ID, work.AdvanceInput{To: "admitted", Actor: "operator:synthetic"}, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if item.ProposalKind != envelope.ProposalKindOrdinary || item.TargetSurface != "" {
				t.Fatalf("wrong falsifier work: %+v", item)
			}
			workPath := filepath.Join(homePath, "work", "work", item.ID+".json")
			workBefore, err := os.ReadFile(workPath)
			if err != nil {
				t.Fatal(err)
			}
			target := authorityPath
			switch tc.target {
			case "leaf", "referent":
				external := filepath.Join(workspace, "operator-authority.yaml")
				if err := os.Rename(authorityPath, external); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, authorityPath); err != nil {
					t.Fatal(err)
				}
				if tc.target == "referent" {
					target = external
				}
			case "alias", "new_alias":
				alias := filepath.Join(workspace, "authority-alias")
				if err := os.Symlink(filepath.Dir(authorityPath), alias); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(alias, "codex.yaml")
				if tc.target == "new_alias" {
					target = filepath.Join(alias, "synthetic-new.yaml")
				}
			case "sibling":
				target = filepath.Join(homePath, "config", "harness-permissions-other", "codex.yaml")
			case "readme":
				target = filepath.Join(workspace, "README.md")
			case "env":
				target = filepath.Join(workspace, ".env")
			}
			patch := "*** Begin Patch\n*** Update File: " + target + "\n@@\n-old\n+new\n*** End Patch"
			if tc.target == "leaf" || tc.target == "referent" {
				_, _, err := harness.LoadPermissionAuthority(homePath, "codex")
				if err == nil || !strings.Contains(err.Error(), "permission authority file must be a regular file") {
					t.Errorf("valid escaped body must fail specific custody check: %v", err)
				}
			}
			switch tc.target {
			case "corrupt", "identity", "invalid":
				body := []byte("{malformed")
				if tc.target != "corrupt" {
					changed := item
					if tc.target == "identity" {
						changed.ID = "work_synthetic_wrong_identity"
					} else {
						changed.SchemaVersion = "invalid.synthetic.schema"
					}
					body, err = json.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(workPath, body, 0600); err != nil {
					t.Fatal(err)
				}
				workBefore = body
			}
			if tc.target == "batch" {
				patch = "*** Begin Patch\n*** Update File: " + filepath.Join(workspace, "README.md") +
					"\n@@\n-old\n+new\n*** Update File: " + target + "\n@@\n-old\n+new\n*** End Patch"
			}
			hookWorkID := item.ID
			if tc.target == "unbound" {
				hookWorkID = ""
			}
			payload := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":"synthetic-native-target","work_id":%q,"cwd":%q,"tool_name":"apply_patch","tool_input":{"command":%q}}`, hookWorkID, workspace, patch)
			args := []string{"pre-tool-use", "--live", "--harness", "codex"}
			if tc.enforce {
				args = append(args, "--enforce")
			}
			var code int
			out := captureStdout(t, func() {
				withStdin(t, payload, func() { code = runHookCommand(args) })
			})
			var response claudeLiveHookResponse
			if err := json.Unmarshal([]byte(out), &response); err != nil {
				t.Fatalf("native response: %v\n%s", err, out)
			}
			action, decision := actualTargetGateEvidence(t, homePath, hookWorkID, target)
			bindingDiagnostic := map[string]string{
				"corrupt":  "unreadable record",
				"identity": "identity mismatch", "invalid": "invalid envelope",
			}[tc.target]
			if bindingDiagnostic != "" {
				reasons := strings.Join(action.SecurityReasons, "\n")
				if !strings.Contains(reasons, "protected-target work binding: "+bindingDiagnostic) {
					t.Errorf("missing specific binding diagnostic: %s", reasons)
				}
				for _, privateBody := range []string{"{malformed", "work_synthetic_wrong_identity", "invalid.synthetic.schema"} {
					if strings.Contains(reasons, privateBody) {
						t.Errorf("binding diagnostic disclosed record content: %s", reasons)
					}
				}
			}
			if tc.deny == policy.Allowed(decision) {
				t.Errorf("actual target classification: target=%q action=%+v policy=%+v", target, action, decision)
			}
			if tc.enforce && tc.deny {
				if code == 0 || response.HookSpecificOutput.PermissionDecision != "deny" {
					t.Errorf("protected target not natively denied: exit=%d response=%s", code, out)
				}
			} else if code != 0 || response.HookSpecificOutput.PermissionDecision != "" {
				t.Errorf("ordinary/observation control changed: exit=%d response=%s", code, out)
			}
			workAfter, err := os.ReadFile(workPath)
			if err != nil || !bytes.Equal(workBefore, workAfter) {
				t.Errorf("hook rewrote work authority: err=%v", err)
			}
			after, err := os.ReadFile(authorityPath)
			if err != nil || !bytes.Equal(authorityBytes, after) {
				t.Errorf("classification probe mutated permission bytes: err=%v", err)
			}
		})
	}
}

func actualTargetGateEvidence(t *testing.T, homePath, workID, target string) (envelope.ActionIntent, envelope.PolicyDecision) {
	t.Helper()
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"act_", "pol_"}})
	if err != nil {
		t.Fatal(err)
	}
	var action envelope.ActionIntent
	decisions := map[string]envelope.PolicyDecision{}
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			t.Fatal(err)
		}
		var header envelope.Header
		if err := json.Unmarshal(record.Bytes, &header); err != nil {
			t.Fatal(err)
		}
		switch header.SchemaVersion {
		case envelope.SVActionIntent:
			var candidate envelope.ActionIntent
			if err := json.Unmarshal(record.Bytes, &candidate); err != nil {
				t.Fatal(err)
			}
			if candidate.Capability == "filesystem.write" && filepath.Base(candidate.Target.Path) == filepath.Base(target) &&
				(workID == "" || candidate.IDs.WorkID == workID) {
				if action.ID != "" {
					t.Fatalf("multiple native target actions: %s %s", action.ID, candidate.ID)
				}
				action = candidate
			}
		case envelope.SVPolicyDecision:
			var candidate envelope.PolicyDecision
			if err := json.Unmarshal(record.Bytes, &candidate); err != nil {
				t.Fatal(err)
			}
			decisions[candidate.ActionIntentID] = candidate
		}
	}
	decision, ok := decisions[action.ID]
	if action.ID == "" || !ok {
		t.Fatalf("native gate evidence missing: action=%+v policy=%+v", action, decision)
	}
	return action, decision
}
