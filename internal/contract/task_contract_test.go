package contract

import (
	"reflect"
	"testing"

	"compound/internal/envelope"
)

func TestCompileAndPackCarryOneTaskContract(t *testing.T) {
	in := mkInput("control-plane")
	in.Task = "  adapt task-scoped capabilities  "
	in.Mode = "execute"
	in.TaskContract = envelope.TaskContract{
		DesiredOutcome:   "Task definitions shape capability and model-role selection.",
		SuccessPredicate: "The same typed contract reaches dispatch and panel planning.",
		NonGoals:         []string{"retire the legacy operating system in this slice"},
		ProofMedium:      "cli",
	}

	contract := Compile(in, DefaultPolicy(), fixedNow)
	pack := Pack(in, DefaultPolicy(), fixedNow)
	want := in.TaskContract
	want.Intent = "adapt task-scoped capabilities"
	want.Mode = "execute"

	if !reflect.DeepEqual(contract.TaskContract, want) {
		t.Fatalf("effective contract task_contract = %+v, want %+v", contract.TaskContract, want)
	}
	if !reflect.DeepEqual(pack.TaskContract, want) {
		t.Fatalf("context pack task_contract = %+v, want %+v", pack.TaskContract, want)
	}
	if pack.IDs.ContractID != contract.ID {
		t.Fatalf("context pack contract_id = %q, want %q", pack.IDs.ContractID, contract.ID)
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("effective contract invalid: %v", err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack invalid: %v", err)
	}

	changed := in
	changed.TaskContract.ProofMedium = "api"
	if got := Compile(changed, DefaultPolicy(), fixedNow); got.ID == contract.ID {
		t.Fatalf("materially different task contracts reused effective contract id %q", got.ID)
	}
	if got := Pack(changed, DefaultPolicy(), fixedNow); got.ID == pack.ID {
		t.Fatalf("materially different task contracts reused context pack id %q", got.ID)
	}
}

func TestResolvedTaskContractCannotDisagreeWithLegacyIntentAndMode(t *testing.T) {
	in := mkInput("control-plane")
	in.Task = "canonical task intent"
	in.Mode = "execute"
	in.TaskContract = envelope.TaskContract{
		Intent: "conflicting intent",
		Mode:   "review",
	}

	contract := Compile(in, DefaultPolicy(), fixedNow)
	pack := Pack(in, DefaultPolicy(), fixedNow)
	if contract.TaskContract.Intent != in.Task || pack.TaskContract.Intent != in.Task {
		t.Fatalf("typed intent drifted from input task: contract=%q pack=%q", contract.TaskContract.Intent, pack.TaskContract.Intent)
	}
	if contract.TaskMode != in.Mode || contract.TaskContract.Mode != in.Mode || pack.TaskContract.Mode != in.Mode {
		t.Fatalf("typed mode drifted: task_mode=%q contract=%q pack=%q", contract.TaskMode, contract.TaskContract.Mode, pack.TaskContract.Mode)
	}
}
