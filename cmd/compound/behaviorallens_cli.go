package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/behaviorallens"
	"compound/internal/home"
)

// cmdBehavioralLens is the operator/orchestrator door onto the behavioral-lens framework — the
// composable observe->InferredSignal plug. It delegates to runBehavioralLens, which returns an exit
// code (never calls os.Exit) so the visibility gate and fail-loud paths are exercised by tests.
func cmdBehavioralLens(args []string) {
	if code := runBehavioralLens(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func behavioralLensUsage() string {
	return "usage:\n" +
		"  compound behavioral-lens list [--home P] [--json]\n" +
		"    list registered lenses (id, visibility) and which operator_private lenses this home enables\n" +
		"  compound behavioral-lens run --lens ID --observation FILE [--home P] [--record] [--json]\n" +
		"    run ONE lens over an observation; an operator_private lens runs only if this home enables it;\n" +
		"    --record appends the InferredSignal to the ledger under $COMPOUND_HOME (evidence, never a gate)\n" +
		"  compound behavioral-lens compose --observation FILE [--home P] [--record] [--json]\n" +
		"    run ALL registered lenses over one observation, visibility-gated by home config (default off,\n" +
		"    stranger clone fail-closed); --record appends every produced signal under $COMPOUND_HOME\n" +
		"  compound behavioral-lens signals [--home P] [--public] [--json]\n" +
		"    read recorded signals; --public shows ONLY the public projection (the sole set a tracked or\n" +
		"    public-emitting surface may consume — operator_private signals never leave $COMPOUND_HOME)\n" +
		"observations are read from files; this command makes NO provider calls."
}

func runBehavioralLens(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, behavioralLensUsage())
		return 2
	}
	switch args[0] {
	case "list":
		return runBehavioralLensList(args[1:], stdout, stderr)
	case "run":
		return runBehavioralLensRun(args[1:], stdout, stderr)
	case "compose":
		return runBehavioralLensCompose(args[1:], stdout, stderr)
	case "signals":
		return runBehavioralLensSignals(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, behavioralLensUsage())
		return 2
	}
}

func runBehavioralLensList(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, err := behaviorallens.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	enabled := map[string]bool{}
	for _, id := range cfg.EnabledPrivateLenses {
		enabled[strings.TrimSpace(id)] = true
	}
	// RegistryForHome layers the home's operator_private lens types onto the shipped public_safe set,
	// so `list` shows an operator_private lens exists (marked off unless this home enabled it). Its
	// VALUE is never computed here — only its type/visibility is listed.
	reg := behaviorallens.RegistryForHome(cfg)
	type row struct {
		LensID     string `json:"lens_id"`
		Visibility string `json:"visibility"`
		Active     bool   `json:"active"`
	}
	var rows []row
	for _, id := range reg.IDs() {
		l, _ := reg.Lens(id)
		vis := l.Visibility()
		active := vis == behaviorallens.VisibilityPublicSafe || enabled[id]
		rows = append(rows, row{LensID: id, Visibility: string(vis), Active: active})
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, rows); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# behavioral lenses")
	for _, r := range rows {
		state := "active"
		if !r.Active {
			state = "off (operator_private; enable per-home)"
		}
		fmt.Fprintf(stdout, "- %-20s %-16s %s\n", r.LensID, r.Visibility, state)
	}
	fmt.Fprintln(stdout, "note: signals are EVIDENCE per subject, never a gate; the governed modulator is a deferred, separate decision.")
	return 0
}

func runBehavioralLensRun(args []string, stdout, stderr io.Writer) int {
	lensID := strings.TrimSpace(flagValue(args, "--lens"))
	obsPath := strings.TrimSpace(flagValue(args, "--observation"))
	if lensID == "" || obsPath == "" {
		fmt.Fprintln(stderr, "usage: compound behavioral-lens run --lens ID --observation FILE [--home P] [--record] [--json]")
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, err := behaviorallens.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// RegistryForHome includes this home's operator_private lens types; the visibility guard below
	// still enforces that such a lens runs only if this home enabled it (default off, fail-closed).
	reg := behaviorallens.RegistryForHome(cfg)
	lens, ok := reg.Lens(lensID)
	if !ok {
		fmt.Fprintf(stderr, "error: no registered lens %q\n", lensID)
		return 1
	}
	// the visibility gate applies to `run` too: an operator_private lens runs only if this home
	// enabled it — a stranger clone cannot bypass the boundary through a single-lens run.
	if lens.Visibility() == behaviorallens.VisibilityOperatorPrivate && !privateLensEnabled(cfg, lensID) {
		fmt.Fprintf(stderr, "error: lens %q is operator_private and not enabled in this home (default off; add it to enabled_private_lenses)\n", lensID)
		return 1
	}
	obs, err := loadObservation(obsPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	sig, err := lens.Infer(context.Background(), obs, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--record") {
		if err := behaviorallens.RecordSignal(homePath, sig); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, sig); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	printSignal(stdout, sig, hasFlag(args, "--record"))
	return 0
}

func runBehavioralLensCompose(args []string, stdout, stderr io.Writer) int {
	obsPath := strings.TrimSpace(flagValue(args, "--observation"))
	if obsPath == "" {
		fmt.Fprintln(stderr, "usage: compound behavioral-lens compose --observation FILE [--home P] [--record] [--json]")
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, err := behaviorallens.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	obs, err := loadObservation(obsPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// RegistryForHome adds this home's operator_private lens types; Compose's cfg gate still decides
	// whether each private lens actually runs (default off; a stranger clone runs public_safe only).
	res, err := behaviorallens.RegistryForHome(cfg).Compose(context.Background(), obs, cfg, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--record") {
		for _, sig := range res.Signals {
			if err := behaviorallens.RecordSignal(homePath, sig); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, res); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# behavioral-lens compose (subject=%s)\n", obs.Subject)
	for _, sig := range res.Signals {
		printSignal(stdout, sig, hasFlag(args, "--record"))
	}
	for _, sk := range res.Skipped {
		fmt.Fprintf(stdout, "- skipped %-20s %-16s %s\n", sk.LensID, sk.Visibility, sk.Reason)
	}
	return 0
}

func runBehavioralLensSignals(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	var sigs []behaviorallens.InferredSignal
	publicOnly := hasFlag(args, "--public")
	if publicOnly {
		sigs, err = behaviorallens.PublicSignals(homePath)
	} else {
		sigs, err = behaviorallens.LoadSignals(homePath)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, sigs); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	scope := "all (operator-local)"
	if publicOnly {
		scope = "public projection (public_safe only)"
	}
	fmt.Fprintf(stdout, "# behavioral-lens signals — %s\n", scope)
	for _, sig := range sigs {
		printSignal(stdout, sig, false)
	}
	return 0
}

func privateLensEnabled(cfg behaviorallens.Config, id string) bool {
	for _, e := range cfg.EnabledPrivateLenses {
		if strings.TrimSpace(e) == id {
			return true
		}
	}
	return false
}

func loadObservation(path string) (behaviorallens.Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return behaviorallens.Observation{}, fmt.Errorf("read observation %s: %w", path, err)
	}
	var obs behaviorallens.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		return behaviorallens.Observation{}, fmt.Errorf("parse observation %s: %w", path, err)
	}
	if strings.TrimSpace(obs.Subject) == "" {
		return behaviorallens.Observation{}, fmt.Errorf("observation %s has no subject", path)
	}
	return obs, nil
}

func printSignal(w io.Writer, sig behaviorallens.InferredSignal, recorded bool) {
	line := fmt.Sprintf("- lens=%s subject=%s value=%.3f confidence=%.2f visibility=%s",
		sig.LensID, sig.Subject, sig.Value, sig.Confidence, sig.Visibility)
	if strings.TrimSpace(sig.Method) != "" {
		line += " method=" + sig.Method
	}
	if recorded {
		line += " recorded=" + sig.ID
	}
	fmt.Fprintln(w, line)
}
