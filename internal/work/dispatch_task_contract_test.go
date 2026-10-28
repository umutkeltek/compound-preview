package work

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/envelope"
)

func TestDispatchClaimPreservesTaskContractAndContractIdentity(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	task := envelope.TaskContract{
		Intent:           "route the task through scoped capabilities",
		Mode:             "execute",
		DesiredOutcome:   "Task meaning reaches the selected executor.",
		SuccessPredicate: "The dispatch pack retains typed task semantics.",
		NonGoals:         []string{"select a provider inside the contract"},
		ProofMedium:      "cli",
	}
	effective := envelope.EffectiveContract{
		Header:             newHeader(envelope.SVEffectiveContract, "contract_task_dispatch", fixedNow, "", "", "private", "tier0"),
		Harness:            "codex",
		TaskMode:           task.Mode,
		TaskContract:       task,
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		Excluded:           []envelope.ExcludedRef{},
		SourceRevisions:    []envelope.SourceRev{},
		Unknowns:           []string{},
		Conflicts:          []string{},
	}
	pack := envelope.ContextPack{
		Header:             newHeader(envelope.SVContextPack, "ctx_task_dispatch", fixedNow, "", "", "private", "tier0"),
		SummaryMD:          "typed dispatch fixture",
		TaskContract:       task,
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{},
		SourceRevisions:    []envelope.SourceRev{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	pack.IDs.ContractID = effective.ID
	contractPath := filepath.Join(store.home, "contracts", "effective", "2026-07-12", effective.ID+".json")
	packPath := filepath.Join(store.home, "context", "packs", pack.ID+".json")
	for _, path := range []string{contractPath, packPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.writeJSON(contractPath, &effective); err != nil {
		t.Fatalf("record effective contract: %v", err)
	}
	if err := store.writeJSON(packPath, &pack); err != nil {
		t.Fatalf("record context pack: %v", err)
	}

	ready, err := store.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: task.SuccessPredicate,
		DesiredOutcome:   task.DesiredOutcome,
		NonGoals:         task.NonGoals,
		ContextPackID:    pack.ID,
	}, fixedNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	result, err := store.DispatchClaim(fixedNow, DispatchPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("dispatch claim: %v", err)
	}
	if result.ClaimedWorkID != ready.ID {
		t.Fatalf("claimed work = %q, want %q", result.ClaimedWorkID, ready.ID)
	}

	var dispatched envelope.ContextPack
	if err := store.readJSON(filepath.Join(store.home, "context", "packs", result.ContextPackID+".json"), &dispatched); err != nil {
		t.Fatalf("read dispatch pack: %v", err)
	}
	if !reflect.DeepEqual(dispatched.TaskContract, task) {
		t.Fatalf("dispatch task_contract = %+v, want %+v", dispatched.TaskContract, task)
	}
	if dispatched.IDs.ContractID != effective.ID {
		t.Fatalf("dispatch contract_id = %q, want %q", dispatched.IDs.ContractID, effective.ID)
	}
	for _, want := range []string{task.Intent, task.Mode, task.ProofMedium} {
		if !strings.Contains(result.WorkerPromptMD, want) {
			t.Fatalf("worker prompt missing typed task value %q:\n%s", want, result.WorkerPromptMD)
		}
	}
}
