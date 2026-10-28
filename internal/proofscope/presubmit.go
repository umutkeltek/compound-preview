package proofscope

import "sort"

// Presubmit is the minutes-scale, non-certifying pre-landing slice of a
// scope. It never replaces the canonical battery: a certifying run still uses
// Scope.Packages, which keeps every root reader.
//
// Rule (skills/compound-land/SKILL.md §1):
//   - Full scope (go.mod/go.sum, scripts/testbattery, internal/proofscope, a
//     root file, a removed package, ...) runs every package of the Git
//     inventory at head, never a `./...` walk that would reach gitignored
//     directories.
//   - Otherwise it runs every package reached from a changed package by
//     import or test edges, every package whose tests name a changed path,
//     and internal/arch (every served-doctrine guard lives there).
//   - cmd/compound imports nearly every package. It runs whole when a changed
//     package reaches it; otherwise only its doctrine readers run, by name.
//   - Other root readers are deferred to the canonical battery after land.
const (
	PresubmitCLIPackage      = modulePath + "/cmd/compound"
	PresubmitDoctrinePackage = modulePath + "/internal/arch"
	// PresubmitCLIReaders names the cmd/compound tests that read served
	// doctrine (skills, contracts, AGENTS.md).
	PresubmitCLIReaders = "Ceremony|ContractCompile|Orient|ProofCheck|Doctor"
)

// PresubmitRun is one `go test -short` invocation.
type PresubmitRun struct {
	Packages []string `json:"packages"`
	Run      string   `json:"run,omitempty"`
}

// PresubmitPlan is the go-test part of one presubmit.
type PresubmitPlan struct {
	Scope Scope          `json:"scope"`
	Runs  []PresubmitRun `json:"runs"`
	// Deferred lists root readers presubmit does not run; the canonical
	// battery after land covers them.
	Deferred []string `json:"deferred,omitempty"`
}

// InventoryPackages is the sorted import paths of every package directory in
// a Git tree listing, by the testbattery source-inventory rules.
func InventoryPackages(paths []string) []string {
	return importPaths(packageDirs(paths))
}

// Presubmit derives the go-test runs for a scope. inventory is the head
// package set a Full scope runs (InventoryPackages of the head tree).
func Presubmit(scope Scope, inventory []string) PresubmitPlan {
	plan := PresubmitPlan{Scope: scope}
	if scope.Mode != Affected {
		plan.Runs = []PresubmitRun{{Packages: append([]string(nil), inventory...)}}
		return plan
	}
	reached := make(map[string]bool, len(scope.Reached))
	for _, pkg := range scope.Reached {
		reached[pkg] = true
	}
	selected := make(map[string]bool)
	for _, pkg := range scope.RootReaders {
		if pkg == PresubmitDoctrinePackage {
			selected[pkg] = true // pinned reader, present at head
		}
	}
	for _, list := range [][]string{scope.Reached, scope.PathReaders} {
		for _, pkg := range list {
			selected[pkg] = true
		}
	}
	cliWhole := reached[PresubmitCLIPackage]
	delete(selected, PresubmitCLIPackage)
	affected := PresubmitRun{Packages: sortedKeys(selected)}
	if cliWhole {
		affected.Packages = append(affected.Packages, PresubmitCLIPackage)
		sort.Strings(affected.Packages)
	}
	plan.Runs = append(plan.Runs, affected)
	if !cliWhole {
		plan.Runs = append(plan.Runs, PresubmitRun{Packages: []string{PresubmitCLIPackage}, Run: PresubmitCLIReaders})
	}
	for _, pkg := range scope.RootReaders {
		if !selected[pkg] && pkg != PresubmitCLIPackage {
			plan.Deferred = append(plan.Deferred, pkg)
		}
	}
	return plan
}
