package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"compound/internal/act"
	"compound/internal/policy"
)

func BackupCredentialStageUsage() string {
	return "compound act backup-credential-stage (plan --request-file P.json --work W | recovery-plan --plan-ref ORIGINAL --operation reconcile --work W | execute --plan-ref REF) --actor A [--home P] [--json]"
}

// The CLI supplies metadata and immutable plan references only. Credential
// values, commands, caller-created attempts and policy decisions have no flag.
func RunBackupCredentialStage(args []string, stdout, stderr io.Writer) int {
	operation, opts, e := parseRuntimeConfigInstallOptions(args)
	if e != nil || (operation != "plan" && operation != "recovery-plan" && operation != "execute") || (operation == "recovery-plan" && opts["--operation"] != "reconcile") {
		fmt.Fprintln(stderr, BackupCredentialStageUsage())
		return 2
	}
	var request policy.BackupCredentialStageRequest
	if operation == "plan" {
		raw, err := runtimeConfigRequestBytes(opts["--request-file"])
		if err == nil {
			request, err = policy.DecodeBackupCredentialStageRequest(raw)
		}
		if err != nil {
			fmt.Fprintln(stderr, "error: backup_credential_stage:request")
			return 1
		}
	}
	owner, e := act.NewBackupCredentialStager(opts["--home"])
	if e != nil {
		fmt.Fprintln(stderr, "error: backup_credential_stage:owner")
		return 1
	}
	var result any
	switch operation {
	case "plan":
		result, e = owner.Plan(request, opts["--work"], opts["--actor"])
	case "recovery-plan":
		result, e = owner.PlanRecovery(opts["--plan-ref"], "reconcile", opts["--work"], opts["--actor"])
	case "execute":
		result, e = owner.Execute(context.Background(), opts["--plan-ref"], opts["--actor"])
	}
	if result != nil {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return 1
		}
	}
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	return 0
}
