package tooltopology

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/lanematrix"
)

const doorsUsage = "usage: compound doors <refresh|show|serve> [--json]\n" +
	"  refresh [--doctor-json FILE] [--lanes-dir DIR]\n" +
	"          observe the machine into COMPOUND_HOME/observed/door-map (read-only: nothing is executed).\n" +
	"          FILE defaults to $COMPOUND_DOOR_DOCTOR_JSON; DIR to $COMPOUND_LANE_PROJECTION_DIR.\n" +
	"          Without FILE the previous door statuses are carried forward with their ORIGINAL observation time.\n" +
	"  show    print the full snapshot (doors with recipes, MCP servers, versions, models, changes)\n" +
	"  serve   print the exact block a harness receives at session start"

// RunDoors is the organ CLI face for `compound doors ...`.
func RunDoors(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, doorsUsage)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	opts, err := parseDoorsFlags(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, doorsUsage)
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	now := time.Now()
	switch sub {
	case "refresh":
		m, err := refreshFromEnvironment(homePath, opts, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if opts.json {
			printDoorMapJSON(m)
			return
		}
		fmt.Printf("door map refreshed: %d doors, %d MCP configs, %d harness versions, %d launchers, %d changed since last\n  %s\n",
			len(m.Doors), len(m.MCP), len(m.Harnesses), len(m.Models), len(m.ChangedSinceLast), SnapshotPath(homePath))
		for _, g := range m.Gaps {
			fmt.Println("  gap:", g)
		}
	case "show":
		m, err := LoadDoorMap(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound doors refresh`)")
			os.Exit(1)
		}
		if opts.json {
			printDoorMapJSON(m)
			return
		}
		fmt.Print(FormatDoorMap(m, now))
	case "serve":
		fmt.Println(ServeDoorMap(homePath, now, DefaultDoorMapTTL))
	default:
		fmt.Fprintln(os.Stderr, doorsUsage)
		os.Exit(2)
	}
}

type doorsFlags struct {
	json       bool
	doctorJSON string
	lanesDir   string
}

func parseDoorsFlags(args []string) (doorsFlags, error) {
	f := doorsFlags{
		doctorJSON: strings.TrimSpace(os.Getenv("COMPOUND_DOOR_DOCTOR_JSON")),
		lanesDir:   strings.TrimSpace(os.Getenv("COMPOUND_LANE_PROJECTION_DIR")),
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json":
			f.json = true
		case "--doctor-json", "--lanes-dir":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				return f, fmt.Errorf("%s needs a value", a)
			}
			if a == "--doctor-json" {
				f.doctorJSON = strings.TrimSpace(args[i])
			} else {
				f.lanesDir = strings.TrimSpace(args[i])
			}
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	return f, nil
}

func refreshFromEnvironment(homePath string, f doorsFlags, now time.Time) (DoorMap, error) {
	userHome, _ := os.UserHomeDir()
	in := Inputs{
		MCPSources: DefaultMCPSources(userHome),
		Binaries:   DefaultBinaries,
		PathDirs:   filepath.SplitList(os.Getenv("PATH")),
		HomeDir:    userHome,
	}
	if f.doctorJSON != "" {
		data, err := os.ReadFile(f.doctorJSON)
		if err != nil {
			return DoorMap{}, fmt.Errorf("read door report: %w", err)
		}
		in.DoorsJSON = data
		if fi, err := os.Stat(f.doctorJSON); err == nil {
			in.DoorsObservedAt = fi.ModTime()
		}
	}
	if f.lanesDir != "" {
		lanes, _, err := lanematrix.LoadLanes(f.lanesDir)
		if err != nil {
			return DoorMap{}, fmt.Errorf("read lane projection: %w", err)
		}
		for _, l := range lanes {
			in.Models = append(in.Models, ModelLane{Launcher: l.ProfileID, Route: l.Route, Harness: l.Harness})
		}
		in.ModelsObserved = true
	}
	return Refresh(homePath, in, now)
}

func printDoorMapJSON(m DoorMap) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(m)
}

// FormatDoorMap is the full drilldown: every door with its recipe, nothing truncated.
func FormatDoorMap(m DoorMap, now time.Time) string {
	var b strings.Builder
	age, _, ok := observedAge(m, now)
	fmt.Fprintf(&b, "# Door map (schema %s)\ngenerated: %s\ndoors observed: %s", m.SchemaVersion, m.GeneratedAt, firstNonEmpty(m.DoorsObservedAt, "never"))
	if ok {
		fmt.Fprintf(&b, " (%s old)", humanAge(age))
	}
	b.WriteString("\n\n## Doors\n")
	for _, d := range m.Doors {
		fmt.Fprintf(&b, "- %s %s: %s\n    use: %s\n", strings.ToUpper(d.Status), d.Name, d.Detail, d.Use)
	}
	b.WriteString("\n## MCP servers (names only)\n")
	for _, s := range m.MCP {
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Harness, s.Source, strings.Join(s.Servers, ", "))
	}
	b.WriteString("\n## Harness versions\n")
	for _, h := range m.Harnesses {
		fmt.Fprintf(&b, "- %s (%s): %s\n", h.Harness, h.Binary, h.Version)
	}
	b.WriteString("\n## Launcher models\n")
	for _, l := range m.Models {
		fmt.Fprintf(&b, "- %s: %s (%s)\n", l.Launcher, l.Route, l.Harness)
	}
	b.WriteString("\n## Changed since last snapshot\n")
	for _, c := range m.ChangedSinceLast {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	b.WriteString("\n## Recent changes (72h)\n")
	for _, c := range m.RecentChanges {
		fmt.Fprintf(&b, "- %s  %s\n", c.At, c.What)
	}
	if len(m.Gaps) > 0 {
		b.WriteString("\n## Gaps\n")
		for _, g := range m.Gaps {
			fmt.Fprintf(&b, "- %s\n", g)
		}
	}
	return b.String()
}
