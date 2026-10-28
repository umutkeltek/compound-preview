package negatives

import (
	"encoding/json"
	"fmt"
	"os"

	"compound/internal/home"
)

// Usage is the operator-facing usage line for `compound negatives`. It lives
// HERE (not in cmd/compound) because this subcommand's CLI body lives in this
// organ: the cmd layer stays a thin dispatch, and the cmd/compound LOC budget
// ratchet (internal/arch.checkCmdCompoundBudget) only lets cmd/compound shrink.
const Usage = "usage: compound negatives <build [--json] [--home P]|list [--json] [--home P]>"

// RunCLI runs the `compound negatives` subcommand over args and exits the
// process with the matching status (2 on usage error, 1 on runtime error).
// It is the rebuild/inspect door onto the negatives projection: `build`
// rebuilds from scratch and persists under projections/negatives.json (never
// the ledger); `list` reads the persisted projection without touching either
// producer. Orient (internal/contract) reads the persisted file directly —
// this command is the operator/cron door that keeps it fresh, not something
// orient itself calls inline.
func RunCLI(args []string) {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(os.Stderr, Usage)
		os.Exit(2)
	}
	sub := args[0]
	jsonOut := false
	homeFlag := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--home":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "negatives: --home requires a path")
				os.Exit(2)
			}
			homeFlag = args[i+1]
			i++
		default:
			fmt.Fprintln(os.Stderr, Usage)
			os.Exit(2)
		}
	}
	homePath, err := resolveNegativesHome(homeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "negatives:", err)
		os.Exit(1)
	}
	switch sub {
	case "build":
		proj, err := Build(homePath, BuildOptions{})
		if err != nil {
			fmt.Fprintln(os.Stderr, "negatives build:", err)
			os.Exit(1)
		}
		path, err := WriteProjection(homePath, proj)
		if err != nil {
			fmt.Fprintln(os.Stderr, "negatives build:", err)
			os.Exit(1)
		}
		if jsonOut {
			printProjectionJSON(proj)
			return
		}
		fmt.Println("# negatives build")
		fmt.Printf("- wrote: %s\n", path)
		printSummary(proj)
	case "list":
		proj, ok, err := ReadProjection(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "negatives list:", err)
			os.Exit(1)
		}
		if !ok {
			if jsonOut {
				printProjectionJSON(Projection{
					SchemaVersion: SchemaVersion,
					ProjectionRef: ProjectionRef,
					Records:       []Record{},
				})
				return
			}
			fmt.Println("# negatives list")
			fmt.Println("- projection not built yet; run `compound negatives build`")
			return
		}
		if jsonOut {
			printProjectionJSON(proj)
			return
		}
		fmt.Println("# negatives list")
		printSummary(proj)
	default:
		fmt.Fprintln(os.Stderr, Usage)
		os.Exit(2)
	}
}

func hasUsageFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func printSummary(proj Projection) {
	fmt.Printf("- computed_at: %s\n", proj.ComputedAt)
	fmt.Printf("- records: %d\n", len(proj.Records))
	for _, r := range proj.Records {
		fmt.Printf("  %s  %s  approach=%s ref=%s\n", r.Source, r.RefID, r.ApproachKey, r.Ref)
	}
}

// resolveNegativesHome resolves the home the command operates on; the empty
// flag means the ambient home (COMPOUND_HOME / default).
func resolveNegativesHome(homeFlag string) (string, error) {
	return home.Resolve(homeFlag)
}

func printProjectionJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}
