package arch

import (
	"fmt"
	"os"
)

// Run is the organ entry point behind `compound architecture` (the cmd-budget
// ratchet keeps cmd/compound a dispatch layer; the surface lives here).
// It runs architecture doctor: candidate-map integrity, total package
// classification (a new unclassified package fails), public-doc leak gate, and
// visible memory-stack convergence debt. Exits non-zero if any check FAILs.
func Run(args []string) {
	if len(args) == 0 || args[0] != "doctor" {
		fmt.Fprintln(os.Stderr, "usage: compound architecture doctor [--repo <path>] [--strict]")
		os.Exit(2)
	}
	repo, strict := ".", false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--strict":
			strict = true
		case "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
			}
		}
	}
	r := Doctor(repo, strict)
	for _, f := range r.Findings {
		fmt.Printf("  [%-4s] %-14s %s\n", f.Severity, f.Check, f.Message)
	}
	if r.OK {
		fmt.Println("verdict: ok")
	} else {
		fmt.Println("verdict: FAIL")
		os.Exit(1)
	}
}
