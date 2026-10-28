// Command testbattery runs the complete Go inventory with release and home isolated.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"compound/internal/proofscope"
)

type commandRunner func(context.Context, []string, io.Writer, io.Writer) error

// usageLine is the single refusal message for every malformed invocation.
// A help request prints the same line and exits 0.
const usageLine = "usage: testbattery [test|general-release|home|vet|build|credential-password-alignment-native] [--source-head SHA] (source-owned inventories; no caller selectors)"

// supportedModes is the closed set of explicit gates. There is deliberately no
// default mode: a bare invocation must never start the whole inventory.
var supportedModes = map[string]bool{
	"test":                       true,
	"general-release":            true,
	"home":                       true,
	"vet":                        true,
	"build":                      true,
	credentialAlignmentProofMode: true,
}

// invocation is the fully resolved CLI request. An explicit mode is always
// present; help carries no mode because it never reaches the source tree.
type invocation struct {
	mode       string
	sourceHead string
	help       bool
}

// parseInvocation resolves the arguments before any inventory or child process
// can exist. A missing mode is a caller mistake, not a request to run the
// whole battery, so it is refused here rather than defaulted to "test".
func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 {
		return invocation{}, errors.New("no mode given")
	}
	mode := ""
	rest := args
	if !strings.HasPrefix(args[0], "-") {
		mode, rest = args[0], args[1:]
	}
	flags := flag.NewFlagSet("testbattery", flag.ContinueOnError)
	// The driver owns every message it prints; the flag package must stay quiet.
	flags.SetOutput(io.Discard)
	sourceHead := flags.String("source-head", "", "require this sealed commit and clean source before and after the gate")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return invocation{help: true}, nil
		}
		return invocation{}, err
	}
	if mode == "" {
		return invocation{}, errors.New("no mode given")
	}
	if !supportedModes[mode] {
		return invocation{}, fmt.Errorf("unsupported mode %q", mode)
	}
	if flags.NArg() != 0 {
		return invocation{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *sourceHead != "" && !sourceCommit.MatchString(*sourceHead) {
		return invocation{}, errors.New("source snapshot requires a full commit SHA")
	}
	return invocation{mode: mode, sourceHead: *sourceHead}, nil
}

func runBattery(ctx context.Context, run commandRunner, stdout, stderr io.Writer) error {
	return runBatteryPhase(ctx, "test", run, stdout, stderr)
}

func runBatteryPhase(ctx context.Context, mode string, run commandRunner, stdout, stderr io.Writer) error {
	if mode != "test" && mode != "general-release" && mode != "home" {
		return fmt.Errorf("unsupported test phase %q", mode)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var inventory bytes.Buffer
	err := run(ctx, []string{"list", "./..."}, &inventory, stderr)
	if err != nil || ctx.Err() != nil {
		return fmt.Errorf("inventory: %w", errors.Join(err, ctx.Err()))
	}
	phases, err := partitionPackages(inventory.String())
	if err != nil {
		return err
	}
	if mode != "test" {
		selected := phases[:0]
		for _, phase := range phases {
			if (mode == "home") == (phase.Name == "home") {
				selected = append(selected, phase)
			}
		}
		phases = selected
	}
	var failures []error
	for i, phase := range phases {
		packages := phase.Packages
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		fmt.Fprintf(stdout, "testbattery: phase %d/%d packages=%d\n", i+1, len(phases), len(packages))
		args := append([]string{"test", "-count=1", "-timeout=40m"}, packages...)
		err := run(ctx, args, stdout, stderr)
		if err != nil {
			failures = append(failures, fmt.Errorf("phase %d: %w", i+1, err))
		}
		if cancelErr := ctx.Err(); cancelErr != nil {
			return errors.Join(append(failures, cancelErr)...)
		}
		if err != nil {
			var exitErr *exec.ExitError
			// An ordinary test failure must not suppress the other phase. A
			// signal, start failure or wait failure must stop later launches.
			if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
				return errors.Join(failures...)
			}
		}
	}
	return errors.Join(failures...)
}

// This is the current module's inventory, not caller-supplied patterns or flags.
var packagePath = regexp.MustCompile(`^compound(/[A-Za-z0-9_-][A-Za-z0-9_.-]*)*$`)

func partitionPackages(inventory string) ([]proofscope.TestPackagePhase, error) {
	var packages []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(inventory, "\n"), "\n") {
		pkg := strings.TrimSuffix(line, "\r")
		if !packagePath.MatchString(pkg) || seen[pkg] {
			return nil, fmt.Errorf("invalid or duplicate package inventory entry %q", pkg)
		}
		seen[pkg] = true
		packages = append(packages, pkg)
	}
	phases := proofscope.TestPackagePhases(packages)
	if !seen["compound/internal/release"] || !seen["compound/internal/home"] || len(packages) < 3 {
		return nil, errors.New("inventory requires release, home, and at least one other package")
	}
	return phases, nil
}

func runGo(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Inherit the canonical proof gate's process group and execution barrier.
	// The proof runner owns cancellation and joining the whole group. Do not
	// add CommandContext, Setpgid, a child killer or a separate timeout here.
	cmd := exec.Command("go", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

// runMain owns the CLI contract: a help request exits 0, a mode-less or
// malformed invocation exits 2, and only an explicit mode can reach a child
// process.
func runMain(args []string, stdout, stderr io.Writer) int {
	inv, err := parseInvocation(args)
	if err != nil {
		fmt.Fprintln(stderr, "testbattery:", err)
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	if inv.help {
		fmt.Fprintln(stdout, usageLine)
		return 0
	}
	// Only prevent subsequent launches after the owner's signal; children
	// remain in its group. Standalone interruption likewise uses group signals.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = runBoundSourceGate(ctx, inv.mode, inv.sourceHead, stdout, stderr)
	stop()
	if err != nil {
		fmt.Fprintln(stderr, "testbattery:", err)
		return 1
	}
	return 0
}
