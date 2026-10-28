package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"compound/internal/routeevidence"
)

type stringListFlag []string

func (s *stringListFlag) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringListFlag) Set(v string) error { *s = append(*s, v); return nil }

func tryRouteExperimentPlanCommand(args []string) bool {
	if len(args) == 0 || args[0] != "experiment-plan" {
		return false
	}
	code := runRouteExperimentPlanCommand(args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
	return true
}

func runRouteExperimentPlanCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("route experiment-plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("profiles-root", "", "Switchback non-secret conformance projection root (required)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	var profiles, roles, efforts stringListFlag
	fs.Var(&profiles, "profile", "launch profile id (repeat; trials are profile×role)")
	fs.Var(&roles, "role", "Claude role alias (repeat): default|opus|sonnet|haiku|subagent; defaults to opus")
	fs.Var(&efforts, "effort", "owner-published requested effort to include (repeat); unpublished variants are refused")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "route experiment-plan: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if strings.TrimSpace(*root) == "" {
		fmt.Fprintln(stderr, "route experiment-plan: --profiles-root is required")
		return 2
	}
	plan, err := routeevidence.PlanRouteExperimentMatrix(*root, roles, efforts, profiles)
	if err != nil {
		fmt.Fprintln(stderr, "route experiment-plan:", err)
		return 2
	}
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			return 1
		}
		return 0
	}
	routeevidence.WriteExperimentPlanText(stdout, plan)
	return 0
}
