package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/work"
)

func TestContinuePublicCLIRefusesDriftAndUnavailablePreflight(t *testing.T) {
	for _, scenario := range []string{"queue_target", "same_state_generation", "adapter_missing", "workspace_unresolvable"} {
		t.Run(scenario, func(t *testing.T) {
			homePath := initializedExecutionHome(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestContinueExactnessCLIHelper$")
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "COMPOUND_CONTINUE_TEST_SCENARIO="+scenario)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("CLI interleaving: %v\n%s", err, out)
			}
			var result work.ContinueResult
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("decode: %v\n%s", err, out)
			}
			if scenario == "queue_target" || scenario == "same_state_generation" {
				if result.Status != "target_changed" {
					t.Fatalf("status=%s, want target_changed", result.Status)
				}
			} else if result.Status != "refused" || result.Skipped != scenario || result.Execute == nil || result.Execute.WouldExecute != "" {
				t.Fatalf("preflight refusal = %+v", result)
			}
		})
	}
}

// Exercise the real public parser/composition root in a fresh process. Route,
// capacity and policy observations are controlled to deterministically pause at
// the existing policy seam; preflight, lifecycle adapters and persistence are real.
func TestContinueExactnessCLIHelper(t *testing.T) {
	scenario := os.Getenv("COMPOUND_CONTINUE_TEST_SCENARIO")
	if scenario == "" {
		return
	}
	homePath := os.Getenv("COMPOUND_HOME")
	s, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	create := func(id string) {
		if _, err := s.Create(work.CreateInput{WorkID: id, Owner: "orchestrator:fixture", Actor: "claude_code:orchestrator", State: "ready", SuccessPredicate: "public continuation preserves selected generation"}, now); err != nil {
			t.Fatal(err)
		}
	}
	create("work_public_exact_a")
	grant := map[string]any{"schema_version": "compound.standing_grants.v0.1", "grants": []map[string]string{{
		"id": "grant_unattended_work_continue_v1", "capability": "work.continue.unattended", "mutation_class": "local_reversible",
		"reversibility_ceiling": "reversible", "blast_radius": "local", "target_path": filepath.Join(homePath, "**"), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}}
	body, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "registries", "standing-grants.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	lane := "claude-minimax"
	if scenario == "adapter_missing" {
		lane = "claude-unavailable"
	}
	work.SwapExecuteStageRoutePlan(func(string, string) (string, bool) { return lane, true })
	work.SwapExecuteStageCapacity(func(string, string, time.Time) (float64, bool, error) { return 30, true, nil })
	before := continueLifecycleSnapshot(t, homePath)
	probed := false
	work.SwapExecuteStagePolicyEval(func(work.PolicyActionIntentShim, work.PolicyConfigShim, time.Time) work.PolicyDecisionShim {
		probed = true
		if scenario == "queue_target" {
			create("work_public_exact_b")
			if _, err := s.Claim("work_public_exact_b", "codex", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		} else if scenario == "same_state_generation" {
			if _, err := s.Advance("work_public_exact_a", work.AdvanceInput{To: "ready", Actor: "codex", Note: "other session changed generation"}, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		} else {
			return work.PolicyDecisionShim{ID: "pol_public_probe", Outcome: "allow"}
		}
		before = continueLifecycleSnapshot(t, homePath)
		return work.PolicyDecisionShim{ID: "pol_public_probe", Outcome: "hold"}
	})
	os.Args = []string{"compound", "work", "continue", "--actor", "codex", "--expect-work", "work_public_exact_a", "--json"}
	if scenario == "adapter_missing" || scenario == "workspace_unresolvable" {
		os.Args = append(os.Args, "--dry-run")
	}
	main()
	if !probed {
		t.Fatal("policy interleaving never reached")
	}
	after := continueLifecycleSnapshot(t, homePath)
	if !reflect.DeepEqual(before, after) {
		for path, digest := range after {
			if before[path] != digest {
				t.Errorf("public command mutated %s after frozen selection", path)
			}
		}
		for path := range before {
			if _, ok := after[path]; !ok {
				t.Errorf("public command removed %s", path)
			}
		}
	}
	if t.Failed() {
		os.Exit(1)
	}
	os.Exit(0)
}
