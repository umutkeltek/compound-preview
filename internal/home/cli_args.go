package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CLIArgs is the home command's parsed placement and positional grammar. It
// owns the flags whose values must never be mistaken for subcommands by either
// dispatch or mutation admission.
type CLIArgs struct {
	HomePath             string
	HomeKind             string
	Subcommand           string
	RemainingPositionals []string
}

func ParseCLIArgs(args []string) CLIArgs {
	parsed := CLIArgs{HomeKind: HomeKindGlobal}
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--home" && index+1 < len(args):
			parsed.HomePath = args[index+1]
			index++
		case (arg == "--target" || arg == "--archive" || arg == "--format") && index+1 < len(args):
			index++
		case arg == "--local":
			parsed.HomeKind = HomeKindLocal
		case arg == "--global":
			parsed.HomeKind = HomeKindGlobal
		case strings.HasPrefix(arg, "-"):
			// Command-specific boolean flags are consumed by their handlers.
		default:
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) > 0 {
		parsed.Subcommand = positionals[0]
		parsed.RemainingPositionals = positionals[1:]
	}
	return parsed
}

func (args CLIArgs) ResolveHome() (string, error) {
	if args.HomePath == "" && args.HomeKind == HomeKindLocal {
		return LocalPath()
	}
	return Resolve(args.HomePath)
}

// PrepareGlobalCLIArgs extracts every exact --home value before command
// dispatch and binds it to the process environment used by all owners.
// It deliberately preserves the existing CLI grammar: an exact --home token
// is global even when a caller intended it as another flag's value.
func PrepareGlobalCLIArgs(args []string) ([]string, error) {
	prepared := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] != "--home" {
			prepared = append(prepared, args[index])
			continue
		}
		if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
			return nil, fmt.Errorf("--home requires a non-blank value")
		}
		absHome, err := filepath.Abs(args[index+1])
		if err != nil {
			return nil, fmt.Errorf("resolve --home: %w", err)
		}
		if err := os.Setenv("COMPOUND_HOME", absHome); err != nil {
			return nil, fmt.Errorf("set COMPOUND_HOME: %w", err)
		}
		index++
	}
	return prepared, nil
}
