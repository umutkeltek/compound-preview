package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContract drops a project contract into a temp dir and returns its path.
func writeContract(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	return path
}

const deployedContract = `schema_version: 1
project:
  slug: harbor-fixture
  name: Harbor Fixture
  class: product
  status: active
  owner: operator
source:
  authority:
    host: workstation
    path: /workspace/harbor
    repo: https://example.invalid/harbor.git
runtime:
  authority: build-host
  service_names:
    - control-service
  urls:
    - https://harbor.example.invalid
  healthchecks:
    - http://control.example.invalid:8080/status
  deployment:
    method: launchd_in_place_repo
    command: make deploy
    rollback: manual git revert on build-host, then verify
data:
  authority: mixed
  classification: mixed
`

// TestProjectContractCarriesDeployment is the depth test for the import seam:
// the contract's deployment block must survive into the bundle. Before this
// slice the struct had no deployment field at all, so the block parsed and was
// silently discarded — a green import that taught an agent nothing.
func TestProjectContractCarriesDeployment(t *testing.T) {
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", deployedContract), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	got := bundle.Product.Deployment
	if got.Place != "place:build-host" {
		t.Errorf("deployment place = %q, want %q (runtime.authority is the place)", got.Place, "place:build-host")
	}
	if got.Method != "launchd_in_place_repo" {
		t.Errorf("deployment method = %q, want launchd_in_place_repo", got.Method)
	}
	if got.Command != "make deploy" {
		t.Errorf("deployment command = %q, want %q", got.Command, "make deploy")
	}
	if !strings.HasPrefix(got.Rollback, "manual git revert on build-host") {
		t.Errorf("deployment rollback = %q, want the contract's rollback text", got.Rollback)
	}
	if len(got.Services) != 1 || got.Services[0] != "control-service" {
		t.Errorf("deployment services = %v, want [control-service]", got.Services)
	}
	if len(got.Healthchecks) != 1 {
		t.Errorf("deployment healthchecks = %v, want the contract's one healthcheck", got.Healthchecks)
	}
}

// TestDeploymentClaimsKeyOnProductEntity is the namespace-join test. Deployment
// facts used to land on a bare-slug entity written by internal/authority while
// product facts landed on product:<slug>, so no reader could join them. Every
// deployment claim must key on the product entity.
func TestDeploymentClaimsKeyOnProductEntity(t *testing.T) {
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", deployedContract), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	claims := productClaims(bundle, "ev_test", "project.yaml")

	want := map[string]string{
		"deploys_to":      "place:build-host",
		"deploy_method":   "launchd_in_place_repo",
		"deploy_command":  "make deploy",
		"deploy_rollback": "manual git revert on build-host, then verify",
	}
	seen := map[string]string{}
	for _, c := range claims {
		if _, tracked := want[c.Predicate]; !tracked {
			continue
		}
		if c.Entity != bundle.Product.ID {
			t.Errorf("claim %q keyed on %q, want the product entity %q", c.Predicate, c.Entity, bundle.Product.ID)
		}
		seen[c.Predicate] = c.Object
	}
	for pred, obj := range want {
		if seen[pred] != obj {
			t.Errorf("claim %q = %q, want %q", pred, seen[pred], obj)
		}
	}
	if !strings.HasPrefix(bundle.Product.ID, "product:") {
		t.Fatalf("product id = %q, want a product: entity", bundle.Product.ID)
	}
}

const undeployedContract = `schema_version: 1
project:
  slug: no-place-fixture
  name: No Place Fixture
  class: product
  status: active
source:
  authority:
    repo: https://example.invalid/no-place.git
runtime:
  authority: unknown
  urls:
    - https://no-place.example.invalid
data:
  classification: local_first
`

// TestUnknownDeploymentStaysUnknown is the honesty test. A product with no
// resolvable place must report nothing rather than inherit a plausible host: a
// fabricated place hands an agent a deploy command aimed at the wrong machine,
// which is strictly worse than an absent answer.
func TestUnknownDeploymentStaysUnknown(t *testing.T) {
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", undeployedContract), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	if got := bundle.Product.Deployment.Place; got != "" {
		t.Errorf("deployment place = %q, want empty for an unresolvable runtime authority", got)
	}
	for _, c := range productClaims(bundle, "ev_test", "project.yaml") {
		if strings.HasPrefix(c.Predicate, "deploy") {
			t.Errorf("emitted %q = %q for a product with no known place", c.Predicate, c.Object)
		}
	}
}

// TestDeploymentPlaceFallsBackToSourceHost covers the common contract shape
// where runtime authority is absent but the source host is the machine that
// actually runs the thing — the same fallback internal/runtimebinding uses.
func TestDeploymentPlaceFallsBackToSourceHost(t *testing.T) {
	body := strings.Replace(deployedContract, "  authority: build-host\n", "  authority: none\n", 1)
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", body), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	if got := bundle.Product.Deployment.Place; got != "place:workstation" {
		t.Errorf("deployment place = %q, want place:workstation from source.authority.host", got)
	}
}

// TestPlaceholderDeploymentFieldsAreDropped covers contracts that fill the
// deployment block with "unknown". The place is real, so the product is still
// reported as deployed, but a placeholder must never reach an agent as though
// it were a method or a command to run.
func TestPlaceholderDeploymentFieldsAreDropped(t *testing.T) {
	body := strings.Replace(deployedContract,
		"    method: launchd_in_place_repo\n    command: make deploy\n",
		"    method: unknown\n    command: TBD\n", 1)
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", body), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	got := bundle.Product.Deployment
	if got.Place != "place:build-host" {
		t.Errorf("deployment place = %q, want place:build-host — the place is still real", got.Place)
	}
	if got.Method != "" || got.Command != "" {
		t.Errorf("placeholder method/command survived: method=%q command=%q", got.Method, got.Command)
	}
	for _, c := range productClaims(bundle, "ev_test", "project.yaml") {
		if c.Predicate == "deploy_method" || c.Predicate == "deploy_command" {
			t.Errorf("emitted %q = %q from a placeholder", c.Predicate, c.Object)
		}
	}
}

// TestDeploymentPlacePreservesHostSyntax guards the join key with
// the services registry (compound://registries/services), which addresses
// hosts by dotted name such as `edge.example`. Running
// the place through the product-id normalizer would yield `edge_example` and
// silently break that join.
func TestDeploymentPlacePreservesHostSyntax(t *testing.T) {
	body := strings.Replace(deployedContract, "  authority: build-host\n", "  authority: edge.example\n", 1)
	bundle, err := LoadProjectContractBundle(writeContract(t, "project.yaml", body), "")
	if err != nil {
		t.Fatalf("load project contract: %v", err)
	}
	if got := bundle.Product.Deployment.Place; got != "place:edge.example" {
		t.Errorf("deployment place = %q, want place:edge.example", got)
	}
}
