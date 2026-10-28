package actuator

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Small CLI helpers, mirrored from cmd/compound (the flag readers from
// main.go, encodeJSON from bench.go, indexOf from harness_exec_cli.go). The
// cmd-budget ratchet keeps cmd/compound a thin shell, so an organ that grows a
// CLI face carries its own copies rather than importing the shell -- the same
// arrangement as internal/store/helpers.go. Keep them byte-compatible with the
// canonical versions.

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func hasUsageFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	return valueFlag(args, name)
}

func valueFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func validateKnownFlags(args []string, valueFlags, boolFlags map[string]bool) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if boolFlags[arg] {
			continue
		}
		if valueFlags[arg] {
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			i++
			continue
		}
		return fmt.Errorf("unknown flag %s", arg)
	}
	return nil
}

func encodeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}
