// testprofile summarizes go test -json evidence; it never runs or certifies tests.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type measurement struct {
	Package string  `json:"package"`
	Test    string  `json:"test,omitempty"`
	Status  string  `json:"status"`
	Seconds float64 `json:"elapsed_seconds"`
}

type profile struct {
	SchemaVersion      string        `json:"schema_version"`
	DiagnosticOnly     bool          `json:"diagnostic_only"`
	Complete           bool          `json:"complete"`
	Packages           []measurement `json:"packages"`
	Tests              []measurement `json:"top_level_tests"`
	Failures           []measurement `json:"failed_tests"`
	SkippedTests       int           `json:"skipped_tests"`
	IncompletePackages []string      `json:"incomplete_packages"`
	IncompleteTests    []string      `json:"incomplete_tests"`
	ParseError         string        `json:"parse_error,omitempty"`
}

type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
}

func readProfile(input io.Reader) (profile, error) {
	report := profile{SchemaVersion: "compound.go_test_profile.v0.1", DiagnosticOnly: true}
	packages, tests := map[string]bool{}, map[string]bool{}
	decoder := json.NewDecoder(input)
	var parseErr error
	for {
		var e event
		if err := decoder.Decode(&e); err != nil {
			if err != io.EOF {
				parseErr = fmt.Errorf("decode Go test event: %w", err)
				report.ParseError = parseErr.Error()
			}
			break
		}
		if e.Package == "" {
			continue
		}
		key := e.Package + "/" + e.Test
		if e.Action == "start" && e.Test == "" {
			packages[e.Package] = true
		}
		if e.Action == "run" && e.Test != "" {
			tests[key] = true
		}
		switch e.Action {
		case "pass", "fail", "skip":
			row := measurement{Package: e.Package, Test: e.Test, Status: e.Action, Seconds: e.Elapsed}
			if e.Test == "" {
				delete(packages, e.Package)
				report.Packages = append(report.Packages, row)
			} else {
				delete(tests, key)
				// Parent and child times overlap. Rank parents separately; retain
				// every failed child without pretending these are additive costs.
				if !strings.Contains(e.Test, "/") {
					report.Tests = append(report.Tests, row)
				}
				if e.Action == "fail" {
					report.Failures = append(report.Failures, row)
				}
				if e.Action == "skip" {
					report.SkippedTests++
				}
			}
		}
	}
	for name := range packages {
		report.IncompletePackages = append(report.IncompletePackages, name)
	}
	for name := range tests {
		report.IncompleteTests = append(report.IncompleteTests, name)
	}
	sort.Strings(report.IncompletePackages)
	sort.Strings(report.IncompleteTests)
	for _, rows := range [][]measurement{report.Packages, report.Tests, report.Failures} {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Seconds != rows[j].Seconds {
				return rows[i].Seconds > rows[j].Seconds
			}
			if rows[i].Package != rows[j].Package {
				return rows[i].Package < rows[j].Package
			}
			return rows[i].Test < rows[j].Test
		})
	}
	report.Complete = parseErr == nil && len(report.Packages) > 0 && len(packages) == 0 && len(tests) == 0
	return report, parseErr
}

func run(input io.Reader, output, diagnostics io.Writer) int {
	report, err := readProfile(input)
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if writeErr := encoder.Encode(report); writeErr != nil {
		fmt.Fprintln(diagnostics, writeErr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 2
	}
	if !report.Complete || len(report.Failures) > 0 {
		return 1
	}
	for _, row := range report.Packages {
		if row.Status == "fail" {
			return 1
		}
	}
	return 0
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/testprofile <go-test-events.jsonl> (diagnostic only)")
		os.Exit(2)
	}
	input, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	code := run(input, os.Stdout, os.Stderr)
	input.Close()
	os.Exit(code)
}
