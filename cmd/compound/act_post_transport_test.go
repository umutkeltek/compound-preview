package main

// Falsifiers for work_d8c134492a6e: compound stays transport-agnostic in code
// while authorization stays concrete per adapter. act post must not carry an
// implicit transport (no vendor default), must not let the caller select the
// capability (policy laundering), must declare honest reversibility for a
// transport the generic exec adapter cannot un-post, and must preview exactly
// which adapter authority would fire.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/product"
)

// assertDirStaysEmpty fails if the directory has any entries — the proof that
// a refused invocation never reached policy evaluation, adapter resolution, or
// any persistence (no PolicyDecision, no event, no artifact was minted).
func assertDirStaysEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read home dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("refused invocation touched the home (entries %v); refusal must happen before policy eval / adapter resolution", names)
	}
}

// Falsifier (a): no --transport = usage error (exit 2) BEFORE policy
// evaluation. The home stays byte-empty, proving no PolicyDecision was minted
// and no adapter was resolved. An implicit vendor default would instead
// proceed to a preview (exit 0) — that is the red state this test refuses.
func TestActPostMissingTransportIsUsageErrorBeforePolicyEval(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--content", "-", "--work", "work_no_transport", "--home", homePath,
	}, strings.NewReader("card body"), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing --transport exit = %d, want 2 (usage); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage:") || !strings.Contains(stderr.String(), "--transport NAME") {
		t.Fatalf("missing --transport must print usage naming --transport NAME as required, got %q", stderr.String())
	}
	assertDirStaysEmpty(t, homePath)
}

// Falsifier (b): --capability is not caller-selectable for act post — the
// capability is fixed to product.ContentPostCapability so a caller cannot
// launder policy through a different capability's standing grants. Passing the
// flag at all is a refusal (exit 2) before policy evaluation.
func TestActPostCapabilityFlagIsRejected(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--content", "-", "--work", "work_cap_launder",
		"--transport", "fixture", "--capability", "anything.else",
		"--home", homePath,
	}, strings.NewReader("card body"), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("--capability exit = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not caller-selectable") || !strings.Contains(stderr.String(), product.ContentPostCapability) {
		t.Fatalf("--capability refusal must say the capability is not caller-selectable and name the fixed capability, got %q", stderr.String())
	}
	assertDirStaysEmpty(t, homePath)
}

// Falsifier (c): the composed intent tells the truth about reversibility. The
// generic exec transport adapter has no delete verb, so nothing guarantees a
// posted body can be recalled: Reversibility is "irreversible" and the
// rollback plan says so honestly instead of naming a delete verb that does
// not exist.
func TestGovernedPostIntentDeclaresIrreversibleWithHonestRollback(t *testing.T) {
	t.Parallel()
	target := envelope.ActionTarget{
		Kind: "transport",
		ID:   "fixture",
		Path: "actuator/transports/fixture.json",
	}
	ai := governedPostIntent("work_truth", product.ContentPostCapability, target, "card body", cliTestNow)
	if ai.Reversibility != "irreversible" {
		t.Fatalf("Reversibility = %q, want %q (exec adapter has no delete verb)", ai.Reversibility, "irreversible")
	}
	if ai.RollbackPlan != "none guaranteed by the transport adapter" {
		t.Fatalf("RollbackPlan = %q, want the honest %q", ai.RollbackPlan, "none guaranteed by the transport adapter")
	}
	if err := ai.Validate(); err != nil {
		t.Fatalf("irreversible post intent must stay schema-valid: %v", err)
	}
}

// Falsifier (d): the no---confirm preview names the exact resolved transport
// AND the resolved adapter config path, so the operator sees precisely which
// authority would fire before confirming.
func TestActPostPreviewPrintsTransportNameAndConfigPath(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--content", "-", "--work", "work_preview",
		"--transport", "fixture", "--home", homePath,
	}, strings.NewReader("card body"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("preview exit = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "transport: fixture") {
		t.Fatalf("preview must name the resolved transport, got:\n%s", out)
	}
	if !strings.Contains(out, "actuator/transports/fixture.json") {
		t.Fatalf("preview must name the resolved adapter config path actuator/transports/fixture.json, got:\n%s", out)
	}
}

// Falsifier (d, JSON shape): the JSON preview carries the same authority
// pointer machine-readably.
func TestActPostJSONPreviewCarriesTransportConfigPath(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--content", "-", "--work", "work_preview_json",
		"--transport", "fixture", "--home", homePath, "--json",
	}, strings.NewReader("card body"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("json preview exit = %d, stderr=%q", code, stderr.String())
	}
	var preview struct {
		Transport       string `json:"transport"`
		TransportConfig string `json:"transport_config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatalf("decode json preview: %v\n%s", err, stdout.String())
	}
	if preview.Transport != "fixture" {
		t.Fatalf("json preview transport = %q, want %q", preview.Transport, "fixture")
	}
	if preview.TransportConfig != "actuator/transports/fixture.json" {
		t.Fatalf("json preview transport_config = %q, want %q", preview.TransportConfig, "actuator/transports/fixture.json")
	}
}

// Falsifier (e): no transport vendor literal survives in production source.
// compound's code must not name or assume any concrete transport — a vendor
// is one swappable adapter definition under actuator/transports/, authorized
// by its own concrete grant. Test files may carry the literal as a fixture;
// production .go files under cmd/ and internal/act/ may not.
func TestNoVendorTransportLiteralInProductionSource(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"../../cmd", "../../internal/act"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(data), "buzz") {
				t.Errorf("production source %s carries a vendor transport literal; transports are adapter definitions, not code", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
