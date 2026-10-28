package runtimebinding

import (
	"path/filepath"
	"testing"
)

func TestProjectContractBundle(t *testing.T) {
	b, err := LoadProjectContract(filepath.Join("..", "..", "fixtures", "runtime", "project-contract.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if b.RuntimeBinding.ServiceID != "service:synthetic-api" {
		t.Fatalf("service id = %q", b.RuntimeBinding.ServiceID)
	}
	if b.RuntimeBinding.OwnerEntity != "project:synthetic-product" {
		t.Fatalf("owner = %q", b.RuntimeBinding.OwnerEntity)
	}
	if b.RuntimeBinding.HostBinding != "apps.example" {
		t.Fatalf("host binding = %q", b.RuntimeBinding.HostBinding)
	}
	if b.RuntimeBinding.RiskTier != "tier2" {
		t.Fatalf("risk = %q", b.RuntimeBinding.RiskTier)
	}
	if got := contains(b.RuntimeBinding.DataBindings, "authority:db.example"); !got {
		t.Fatalf("data bindings missing authority: %+v", b.RuntimeBinding.DataBindings)
	}
	if got := contains(b.ServiceContract.InterfaceRefs, "health:https://synthetic.example.invalid/health"); !got {
		t.Fatalf("interfaces missing healthcheck: %+v", b.ServiceContract.InterfaceRefs)
	}
	if got := contains(b.ServiceContract.Dependencies, "service:synthetic-worker"); !got {
		t.Fatalf("dependencies missing worker: %+v", b.ServiceContract.Dependencies)
	}
	if report := Doctor(b); report.Status != "ok" {
		t.Fatalf("doctor failed adapted project contract: %+v", report)
	}
}

func TestProjectContractRequiresSlug(t *testing.T) {
	_, err := ProjectContractBundle(projectContract{}, "empty.yaml")
	if err == nil {
		t.Fatal("expected missing slug error")
	}
}

func TestDependencyRefsEmptyServices(t *testing.T) {
	if got := dependencyRefs(nil, []string{"db"}); len(got) != 1 || got[0] != "database:db" {
		t.Fatalf("dependencyRefs = %v", got)
	}
}

func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
