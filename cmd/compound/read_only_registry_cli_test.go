package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const readRegistryGuardSubEnv = "COMPOUND_READ_REGISTRY_GUARD_SUB"

// readRegistryDispatch maps a guard case name to the registry entry point it
// exercises. Presence has its own dedicated exec-self tests
// (presence_cli_test.go); it is listed here too so the guard table is the
// single exhaustive inventory of every read-classified command that touches
// COMPOUND_HOME. Commands that never touch home (version, the retired verbs)
// are not listed — there is nothing to guard.
var readRegistryDispatch = map[string]func([]string){
	"presence":             cmdPresence,
	"workbench":            cmdWorkbench,
	"workbench-capability": cmdWorkbench,
	"attention":            cmdAttention,
	"coordination":         cmdCoordination,
	"narrate":              cmdNarrate,
	"executors":            cmdExecutors,
	"stream":               cmdStream,
	"doctor":               cmdDoctor,
	"placement":            cmdPlacement,
	"retention":            cmdRetention,
	"transition":           cmdTransition,
}

// readRegistryCase is one read-classified registry command run against a home
// whose runtime state (work/, events/, registries/) has been stripped.
// exits lists acceptable exit codes: the invariant under test is "creates no
// runtime state". Most commands must exit 0; a few legitimately exit nonzero
// on a stripped home because there is nothing to show (e.g. `transition show`
// of an unknown id). wantStderr, when set, must appear in the combined output.
type readRegistryCase struct {
	name string
	// mkArgs yields the flags passed to the dispatched cmd function — the
	// command name itself is carried by name and must NOT be repeated here.
	mkArgs     func(homePath string) []string
	exits      []int
	wantStderr string
}

func readRegistryGuardCases() []readRegistryCase {
	return []readRegistryCase{
		{
			name:   "presence",
			mkArgs: func(string) []string { return []string{"--json"} },
			exits:  []int{0},
		},
		{
			name:   "workbench",
			mkArgs: func(string) []string { return []string{"--json"} },
			exits:  []int{0},
		},
		{
			// --capability-task on a home with no graph must skip the surface
			// with a visible warning and must NOT scaffold registries/.
			name: "workbench-capability",
			mkArgs: func(string) []string {
				return []string{"--json", "--capability-task", "T", "--capability-harness", "H"}
			},
			exits:      []int{0},
			wantStderr: "no capability graph",
		},
		{
			name:   "attention",
			mkArgs: func(h string) []string { return []string{"--json", "--home", h} },
			exits:  []int{0},
		},
		{
			name:   "coordination",
			mkArgs: func(h string) []string { return []string{"--json", "--home", h} },
			exits:  []int{0},
		},
		{
			name:   "narrate",
			mkArgs: func(h string) []string { return []string{"--json", "--home", h} },
			exits:  []int{0},
		},
		{
			name:   "executors",
			mkArgs: func(string) []string { return []string{"--json"} },
			exits:  []int{0},
		},
		{
			name:   "stream",
			mkArgs: func(h string) []string { return []string{"--once", "--json", "--home", h} },
			exits:  []int{0},
		},
		{
			name:   "doctor",
			mkArgs: func(string) []string { return []string{"--json"} },
			exits:  []int{0, 1}, // 0 = no problems; 1 = problems found (R2B exit contract). Host hook/harness drift is a real finding on a dev box; this test only proves the read-only face materializes nothing,
		},
		{
			name: "placement",
			mkArgs: func(h string) []string {
				return []string{"resolve", filepath.Join(h, "observed", "activity", "x.ndjson"), "--json"}
			},
			exits: []int{0},
		},
		{
			name:   "retention",
			mkArgs: func(string) []string { return []string{"classes", "--json"} },
			exits:  []int{0},
		},
		{
			name:   "transition",
			mkArgs: func(string) []string { return []string{"show", "tr_does_not_exist"} },
			exits:  []int{1},
		},
	}
}

// TestReadOnlyRegistryCommandsDoNotMaterializeState is the registry-level
// guard for the read-only admission class: EVERY readCommand that touches
// COMPOUND_HOME, run against a home whose work/, events/, and registries/
// have been stripped, must exit cleanly and leave all three directories
// ABSENT. This is the invariant the external audit found `presence`
// violating (and this guard found five more commands violating, through
// work.Open plus the shared reviewqueue stamp helpers): a read-classified
// command materializing runtime state turns "just looking" into a mutation
// of the operator's home.
//
// The subprocess (exec-self) pattern is required because the cmd* entry
// points os.Exit on their own error paths; running them in-process would
// kill the test.
func TestReadOnlyRegistryCommandsDoNotMaterializeState(t *testing.T) {
	t.Parallel()
	if raw := os.Getenv(readRegistryGuardSubEnv); raw != "" {
		parts := strings.Split(raw, "\x1f")
		fn, ok := readRegistryDispatch[parts[0]]
		if !ok {
			fmt.Fprintln(os.Stderr, "read registry guard: no dispatch for", parts[0])
			os.Exit(127)
		}
		fn(parts[1:])
		os.Exit(0)
	}

	for _, tc := range readRegistryGuardCases() {
		t.Run(tc.name, func(t *testing.T) {
			homePath := initializedTestHome(t)
			// fresh init scaffolds the habitat; strip the runtime state a read
			// must not re-materialize.
			for _, d := range []string{"work", "events", "registries"} {
				if err := os.RemoveAll(filepath.Join(homePath, d)); err != nil {
					t.Fatalf("strip %s: %v", d, err)
				}
			}

			childArgs := append([]string{tc.name}, tc.mkArgs(homePath)...)
			cmd := exec.Command(os.Args[0], "-test.run=^TestReadOnlyRegistryCommandsDoNotMaterializeState$")
			cmd.Env = append(os.Environ(),
				readRegistryGuardSubEnv+"="+strings.Join(childArgs, "\x1f"),
				"COMPOUND_HOME="+homePath,
			)
			output, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					t.Fatalf("spawn %s: %v", tc.name, runErr)
				}
				code = exitErr.ExitCode()
			}
			exitOK := false
			for _, want := range tc.exits {
				if code == want {
					exitOK = true
					break
				}
			}
			if !exitOK {
				t.Fatalf("%s exit = %d, want one of %v\noutput:\n%s", tc.name, code, tc.exits, output)
			}
			if tc.wantStderr != "" && !strings.Contains(string(output), tc.wantStderr) {
				t.Errorf("%s output missing %q:\n%s", tc.name, tc.wantStderr, output)
			}
			for _, d := range []string{"work", "events", "registries"} {
				if _, statErr := os.Stat(filepath.Join(homePath, d)); !os.IsNotExist(statErr) {
					t.Errorf("read-only %s must not create %s/ (stat err=%v)\noutput:\n%s", tc.name, d, statErr, output)
				}
			}
		})
	}
}
