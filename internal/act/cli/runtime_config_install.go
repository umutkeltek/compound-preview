package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"compound/internal/act"
	"compound/internal/home"
	"compound/internal/policy"
)

type runtimeConfigInstallOwner interface {
	Plan(policy.RuntimeConfigInstallRequest, string, string) (act.RuntimeConfigInstallPlanResult, error)
	PlanRecovery(string, string, string, string) (act.RuntimeConfigInstallPlanResult, error)
	Execute(context.Context, string, string) (act.RuntimeConfigInstallResult, error)
	PlanCaptureQualification(act.RuntimeCaptureQualificationRequest, string, string) (act.RuntimeCaptureQualificationPlanResult, error)
	QualifyCapture(string, string) (act.RuntimeCaptureQualificationResult, error)
}

// Test-only replacement seam; no command option can select another owner.
var newRuntimeConfigInstallOwner = func(h string) (runtimeConfigInstallOwner, error) { return act.NewRuntimeConfigInstaller(h) }

func RuntimeConfigInstallUsage() string {
	return "compound act runtime-config-install (plan --request-file P.json --work W | recovery-plan --plan-ref ORIGINAL --operation reconcile|rollback --work W | execute --plan-ref REF | qualify-capture-plan --request-file Q.json --work W | qualify-capture --plan-ref REF) --actor A [--home P] [--json]"
}

func parseRuntimeConfigInstallOptions(args []string) (string, map[string]string, error) {
	bad := errors.New("runtime_config_install:usage")
	if len(args) == 0 || (args[0] != "plan" && args[0] != "recovery-plan" && args[0] != "execute" && args[0] != "qualify-capture-plan" && args[0] != "qualify-capture") {
		return "", nil, bad
	}
	operation := args[0]
	opts := map[string]string{}
	allowed := map[string]bool{"--actor": true, "--home": true, "--json": true}
	if operation == "plan" || operation == "qualify-capture-plan" {
		allowed["--request-file"] = true
		allowed["--work"] = true
	} else {
		allowed["--plan-ref"] = true
		if operation == "recovery-plan" {
			allowed["--operation"] = true
			allowed["--work"] = true
		}
	}
	for i := 1; i < len(args); i++ {
		key := args[i]
		if !allowed[key] {
			return "", nil, bad
		}
		if _, exists := opts[key]; exists {
			return "", nil, bad
		}
		if key == "--json" {
			opts[key] = "true"
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
			return "", nil, bad
		}
		i++
		opts[key] = args[i]
	}
	if opts["--home"] == "" {
		resolved, err := home.Resolve("")
		if err != nil {
			return "", nil, bad
		}
		opts["--home"] = resolved
	}
	if opts["--actor"] == "" || strings.TrimSpace(opts["--actor"]) != opts["--actor"] || !filepath.IsAbs(opts["--home"]) || filepath.Clean(opts["--home"]) != opts["--home"] {
		return "", nil, bad
	}
	if (operation == "plan" || operation == "qualify-capture-plan") && (opts["--work"] == "" || !filepath.IsAbs(opts["--request-file"]) || filepath.Clean(opts["--request-file"]) != opts["--request-file"] || filepath.Ext(opts["--request-file"]) != ".json") {
		return "", nil, bad
	}
	if (operation == "execute" || operation == "qualify-capture") && opts["--plan-ref"] == "" {
		return "", nil, bad
	}
	if operation == "recovery-plan" && (opts["--plan-ref"] == "" || opts["--work"] == "" || (opts["--operation"] != "reconcile" && opts["--operation"] != "rollback")) {
		return "", nil, bad
	}
	return operation, opts, nil
}

func runtimeConfigInstallRequestFile(path string) (policy.RuntimeConfigInstallRequest, error) {
	raw, err := runtimeConfigRequestBytes(path)
	if err != nil {
		return policy.RuntimeConfigInstallRequest{}, err
	}
	return policy.DecodeRuntimeConfigInstallRequest(raw)
}

func runtimeConfigRequestBytes(path string) ([]byte, error) {
	fd, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, errors.New("runtime_config_install:request_unavailable")
	}
	defer fd.Close()
	info, e := fd.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 32768 {
		return nil, errors.New("runtime_config_install:request_unavailable")
	}
	raw, e := io.ReadAll(io.LimitReader(fd, 32769))
	if e != nil {
		return nil, errors.New("runtime_config_install:request_unavailable")
	}
	return raw, nil
}

func RunRuntimeConfigInstall(args []string, stdout, stderr io.Writer) int {
	operation, opts, e := parseRuntimeConfigInstallOptions(args)
	if e != nil {
		fmt.Fprintln(stderr, RuntimeConfigInstallUsage())
		return 2
	}
	var request policy.RuntimeConfigInstallRequest
	var qualification act.RuntimeCaptureQualificationRequest
	if operation == "plan" {
		request, e = runtimeConfigInstallRequestFile(opts["--request-file"])
		if e != nil {
			fmt.Fprintln(stderr, "error: runtime_config_install:invalid_request_file")
			return 1
		}
	}
	if operation == "qualify-capture-plan" {
		var raw []byte
		raw, e = runtimeConfigRequestBytes(opts["--request-file"])
		if e == nil {
			qualification, e = act.DecodeRuntimeCaptureQualificationRequest(raw)
		}
		if e != nil {
			fmt.Fprintln(stderr, "error: runtime_config_install:invalid_qualification_request")
			return 1
		}
	}
	owner, e := newRuntimeConfigInstallOwner(opts["--home"])
	if e != nil {
		fmt.Fprintln(stderr, "error: runtime_config_install:owner_unavailable")
		return 1
	}
	var result any
	if operation == "plan" {
		result, e = owner.Plan(request, opts["--work"], opts["--actor"])
	} else if operation == "recovery-plan" {
		result, e = owner.PlanRecovery(opts["--plan-ref"], opts["--operation"], opts["--work"], opts["--actor"])
	} else if operation == "qualify-capture-plan" {
		result, e = owner.PlanCaptureQualification(qualification, opts["--work"], opts["--actor"])
	} else if operation == "qualify-capture" {
		result, e = owner.QualifyCapture(opts["--plan-ref"], opts["--actor"])
	} else {
		result, e = owner.Execute(context.Background(), opts["--plan-ref"], opts["--actor"])
	}
	raw, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		fmt.Fprintln(stderr, "error: runtime_config_install:rendering_failed")
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	if e != nil {
		fmt.Fprintln(stderr, "error: runtime_config_install:operation_refused")
		return 1
	}
	return 0
}
