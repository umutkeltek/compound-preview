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

type credentialPasswordAlignmentOwner interface {
	Plan(policy.CredentialPasswordAlignmentRequest, string, string) (act.CredentialPasswordAlignmentPlanResult, error)
	Execute(context.Context, string, string) (act.CredentialPasswordAlignmentResult, error)
}

// Test-only replacement seam; no command option can select another owner.
var newCredentialPasswordAlignmentOwner = func(h string) (credentialPasswordAlignmentOwner, error) { return act.NewCredentialPasswordAligner(h) }

func CredentialPasswordAlignmentUsage() string {
	return "compound act credential-password-align (plan --request-file P.json --work W | execute --plan-ref REF) --actor A [--home P] [--json]"
}

func parseCredentialPasswordAlignmentOptions(args []string) (string, map[string]string, error) {
	bad := errors.New("credential_password_align:usage")
	if len(args) == 0 || (args[0] != "plan" && args[0] != "execute") {
		return "", nil, bad
	}
	operation := args[0]
	opts := map[string]string{}
	allowed := map[string]bool{"--actor": true, "--home": true, "--json": true}
	if operation == "plan" {
		allowed["--request-file"] = true
		allowed["--work"] = true
	} else {
		allowed["--plan-ref"] = true
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
	// The process entrypoint consumes --home and binds COMPOUND_HOME before
	// dispatch. Resolve through the same owner when no direct option remains.
	if _, explicit := opts["--home"]; !explicit {
		selected, err := home.Resolve("")
		if err != nil {
			return "", nil, bad
		}
		opts["--home"] = selected
	}
	if opts["--actor"] == "" || strings.TrimSpace(opts["--actor"]) != opts["--actor"] || !filepath.IsAbs(opts["--home"]) || filepath.Clean(opts["--home"]) != opts["--home"] {
		return "", nil, bad
	}
	if operation == "plan" && (opts["--work"] == "" || !filepath.IsAbs(opts["--request-file"]) || filepath.Clean(opts["--request-file"]) != opts["--request-file"] || filepath.Ext(opts["--request-file"]) != ".json") {
		return "", nil, bad
	}
	if operation == "execute" && opts["--plan-ref"] == "" {
		return "", nil, bad
	}
	return operation, opts, nil
}

func credentialPasswordAlignmentRequestFile(path string) (policy.CredentialPasswordAlignmentRequest, error) {
	var zero policy.CredentialPasswordAlignmentRequest
	fd, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return zero, errors.New("credential_password_align:request_unavailable")
	}
	defer fd.Close()
	info, e := fd.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 32768 {
		return zero, errors.New("credential_password_align:request_unavailable")
	}
	raw, e := io.ReadAll(io.LimitReader(fd, 32769))
	if e != nil {
		return zero, errors.New("credential_password_align:request_unavailable")
	}
	return policy.ParseCredentialPasswordAlignmentRequest(raw)
}

func RunCredentialPasswordAlignment(args []string, stdout, stderr io.Writer) int {
	operation, opts, e := parseCredentialPasswordAlignmentOptions(args)
	if e != nil {
		fmt.Fprintln(stderr, CredentialPasswordAlignmentUsage())
		return 2
	}
	var request policy.CredentialPasswordAlignmentRequest
	if operation == "plan" {
		request, e = credentialPasswordAlignmentRequestFile(opts["--request-file"])
		if e != nil {
			fmt.Fprintln(stderr, "error: credential_password_align:invalid_request_file")
			return 1
		}
	}
	owner, e := newCredentialPasswordAlignmentOwner(opts["--home"])
	if e != nil {
		fmt.Fprintln(stderr, "error: credential_password_align:owner_unavailable")
		return 1
	}
	var result any
	if operation == "plan" {
		result, e = owner.Plan(request, opts["--work"], opts["--actor"])
	} else {
		result, e = owner.Execute(context.Background(), opts["--plan-ref"], opts["--actor"])
	}
	// Even failed post-admission results expose only typed sanitized evidence.
	raw, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		fmt.Fprintln(stderr, "error: credential_password_align:rendering_failed")
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	if e != nil {
		fmt.Fprintln(stderr, "error: credential_password_align:operation_refused")
		return 1
	}
	return 0
}
