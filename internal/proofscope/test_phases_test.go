package proofscope

import (
	"reflect"
	"testing"
)

func TestTestPackagePhasesKeepCLIAndWorkSeparateAndAllPackages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		packages []string
		want     []TestPackagePhase
	}{
		{"empty", nil, nil},
		{"isolated-only", []string{"compound/internal/home", "compound/internal/release", "compound/internal/work", "compound/cmd/compound"},
			[]TestPackagePhase{{Name: "cli", Packages: []string{"compound/cmd/compound"}}, {Name: "work", Packages: []string{"compound/internal/work"}}, {Name: "release", Packages: []string{"compound/internal/release"}}, {Name: "home", Packages: []string{"compound/internal/home"}}}},
		{"cli-only", []string{"compound/cmd/compound"},
			[]TestPackagePhase{{Name: "cli", Packages: []string{"compound/cmd/compound"}}}},
		{"work-only", []string{"compound/internal/work"},
			[]TestPackagePhase{{Name: "work", Packages: []string{"compound/internal/work"}}}},
		{"exact-work-path-only", []string{"compound/internal/work/child", "compound/internal/workbench", "compound/internal/work"},
			[]TestPackagePhase{{Name: "general", Packages: []string{"compound/internal/work/child", "compound/internal/workbench"}}, {Name: "work", Packages: []string{"compound/internal/work"}}}},
		{"exact-cli-path-only", []string{"compound/cmd/compound/child", "compound/cmd/compound-install-verify", "compound/cmd/compound"},
			[]TestPackagePhase{{Name: "general", Packages: []string{"compound/cmd/compound-install-verify", "compound/cmd/compound/child"}}, {Name: "cli", Packages: []string{"compound/cmd/compound"}}}},
		{"mixed", []string{"compound/internal/home", "compound/scripts/testbattery", "compound/internal/work", "compound/cmd/compound", "compound/internal/release"},
			[]TestPackagePhase{{Name: "general", Packages: []string{"compound/scripts/testbattery"}}, {Name: "cli", Packages: []string{"compound/cmd/compound"}}, {Name: "work", Packages: []string{"compound/internal/work"}}, {Name: "release", Packages: []string{"compound/internal/release"}}, {Name: "home", Packages: []string{"compound/internal/home"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.packages...)
			if got := TestPackagePhases(tt.packages); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("phases=%v; want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.packages, original) {
				t.Fatal("phase selection mutated the source inventory")
			}
		})
	}
}

func TestTestPackagePhasesIsolatesHookAndPreservesMembership(t *testing.T) {
	t.Parallel()
	const hook = "compound/internal/hook"
	const home = "compound/internal/home"
	const release = "compound/internal/release"
	const work = "compound/internal/work"
	const cli = "compound/cmd/compound"
	tests := []struct {
		name     string
		packages []string
		want     []TestPackagePhase
	}{
		{"empty", nil, nil},
		{"hook-only", []string{hook}, []TestPackagePhase{{Name: "hook", Packages: []string{hook}}}},
		{"hook-absent", []string{home, release, work, cli}, []TestPackagePhase{
			{Name: "cli", Packages: []string{cli}}, {Name: "work", Packages: []string{work}},
			{Name: "release", Packages: []string{release}}, {Name: "home", Packages: []string{home}},
		}},
		{"exact-hook-path-only", []string{"compound/internal/hookplan", hook, "compound/internal/hook/child"}, []TestPackagePhase{
			{Name: "general", Packages: []string{"compound/internal/hook/child", "compound/internal/hookplan"}},
			{Name: "hook", Packages: []string{hook}},
		}},
		{"mixed", []string{home, hook, "compound/scripts/testbattery", work, cli, release}, []TestPackagePhase{
			{Name: "general", Packages: []string{"compound/scripts/testbattery"}},
			{Name: "cli", Packages: []string{cli}}, {Name: "work", Packages: []string{work}},
			{Name: "release", Packages: []string{release}}, {Name: "hook", Packages: []string{hook}},
			{Name: "home", Packages: []string{home}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]string(nil), tt.packages...)
			got := TestPackagePhases(tt.packages)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("phases = %#v; want %#v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.packages, before) {
				t.Fatalf("mutated selection: %v; want %v", tt.packages, before)
			}
			seen := make(map[string]int)
			for _, phase := range got {
				if len(phase.Packages) == 0 {
					t.Fatalf("empty phase %q", phase.Name)
				}
				for _, pkg := range phase.Packages {
					seen[pkg]++
				}
			}
			if len(seen) != len(tt.packages) {
				t.Fatalf("coverage = %v", seen)
			}
			for _, pkg := range tt.packages {
				if seen[pkg] != 1 {
					t.Fatalf("%s count = %d", pkg, seen[pkg])
				}
			}
		})
	}
}
