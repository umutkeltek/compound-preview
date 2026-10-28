package policy

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points COMPOUND_HOME at an empty temporary directory, so no test in
// this package reads the operator's live home: a live shell-verb catalog row
// changes built-in classifications (a canonical battery went red that way on
// 2026-10-16). A test that needs a home still sets its own with t.Setenv.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "policy-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "policy tests: temp home:", err)
		os.Exit(1)
	}
	os.Setenv("COMPOUND_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// TestPackageTestsReadNoOperatorCatalog fails when TestMain stops isolating the
// home and the machine running the tests has an operator catalog file.
func TestPackageTestsReadNoOperatorCatalog(t *testing.T) {
	if _, source, err := ActiveShellVerbCatalog(); err != nil || source != "built-in" {
		t.Fatalf("package tests must classify with built-ins only: source=%q err=%v", source, err)
	}
}
