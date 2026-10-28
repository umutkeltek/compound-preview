package lanematrix

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"compound/internal/harness"
)

const lanesUsage = "usage: compound lanes matrix [--json] [--projection-dir DIR] [--harness H] [--route R]\n" +
	"       DIR defaults to $COMPOUND_LANE_PROJECTION_DIR (the launch owner's published\n" +
	"       profile-conformance directory). No path is assumed: compound is a consumer here."

// projectionDirEnv names the published projection directory. It is an env/flag
// rather than a baked-in default on purpose -- a tracked default would hardcode
// one deployment's launch owner and layout into a repo meant to run anywhere.
const projectionDirEnv = "COMPOUND_LANE_PROJECTION_DIR"

// Run is the organ CLI face for `compound lanes ...`, matching the
// schedule.Run / browser.Run pattern. The command body lives with its organ so
// cmd/compound stays a thin dispatch layer under its ratcheting size budget.
func Run(args []string) {
	if len(args) == 0 || args[0] != "matrix" {
		fmt.Fprintln(os.Stderr, lanesUsage)
		os.Exit(2)
	}
	args = args[1:]

	jsonOut := false
	projectionDir := strings.TrimSpace(os.Getenv(projectionDirEnv))
	harnessFilter := ""
	routeFilter := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--projection-dir", "--harness", "--route":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, lanesUsage)
				os.Exit(2)
			}
			switch arg {
			case "--projection-dir":
				projectionDir = strings.TrimSpace(args[i])
			case "--harness":
				harnessFilter = strings.TrimSpace(args[i])
			case "--route":
				routeFilter = strings.TrimSpace(args[i])
			}
		default:
			fmt.Fprintln(os.Stderr, lanesUsage)
			os.Exit(2)
		}
	}

	if projectionDir == "" {
		fmt.Fprintf(os.Stderr, "error: no projection directory (set %s or pass --projection-dir)\n%s\n",
			projectionDirEnv, lanesUsage)
		os.Exit(2)
	}

	lanes, skipped, err := LoadLanes(projectionDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	reg := harness.DefaultRegistry()
	names := reg.Names()

	m := Build(names, lanes, nil)
	for _, s := range skipped {
		m.Limits = append(m.Limits, "skipped projection: "+s)
	}
	if len(lanes) == 0 {
		// An empty deployment is a legitimate state, not a failure: a fresh
		// clone with no accounts must run and say so plainly.
		m.Limits = append(m.Limits, "no published lane projections found in "+projectionDir)
	}
	m = filterMatrix(m, harnessFilter, routeFilter)

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(m); encErr != nil {
			fmt.Fprintln(os.Stderr, "error:", encErr)
			os.Exit(1)
		}
		return
	}
	printLaneMatrix(m)
}

func filterMatrix(m Matrix, harnessFilter, routeFilter string) Matrix {
	if harnessFilter == "" && routeFilter == "" {
		return m
	}
	nh := NormalizeHarness(harnessFilter)
	var cells []Cell
	for _, c := range m.Cells {
		if harnessFilter != "" && NormalizeHarness(c.Harness) != nh {
			continue
		}
		if routeFilter != "" && c.Route != routeFilter {
			continue
		}
		cells = append(cells, c)
	}
	m.Cells = cells
	return m
}

func printLaneMatrix(m Matrix) {
	fmt.Println("# lane matrix (read-only; compound consumes published launch projections)")
	fmt.Printf("- harnesses: %d · lanes: %d · cells: %d\n", len(m.Harnesses), len(m.Lanes), len(m.Cells))

	counts := m.Counts()
	states := make([]string, 0, len(counts))
	for s := range counts {
		states = append(states, s)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, s := range states {
		parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
	}
	if len(parts) > 0 {
		fmt.Printf("- cells by state: %s\n", strings.Join(parts, " · "))
	}

	if len(m.Lanes) > 0 {
		fmt.Println("\n## lanes (canonical route identity)")
		for _, l := range m.Lanes {
			fmt.Printf("  %-34s profile=%-32s capture=%s\n", l.Route, l.ProfileID, l.CapturePolicy)
		}
	}

	realized := map[string][]string{}
	for _, c := range m.Cells {
		if c.State == StateRealized {
			realized[c.Harness] = append(realized[c.Harness], c.Route)
		}
	}
	if len(realized) > 0 {
		fmt.Println("\n## realized bindings")
		keys := make([]string, 0, len(realized))
		for k := range realized {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-24s %s\n", k, strings.Join(realized[k], ", "))
		}
	}

	// Harnesses with no realized binding are the actionable output: they are
	// the candidates for a new combination. They are NOT asserted runnable --
	// see Limits.
	var unbound []string
	for _, h := range m.Harnesses {
		if len(realized[h]) == 0 {
			unbound = append(unbound, h)
		}
	}
	if len(unbound) > 0 {
		fmt.Printf("\n## harnesses with no realized lane binding\n  %s\n", strings.Join(unbound, ", "))
	}

	if len(m.Limits) > 0 {
		fmt.Println("\n## limits (what this projection could not decide)")
		for _, l := range m.Limits {
			fmt.Printf("  - %s\n", l)
		}
	}
}
