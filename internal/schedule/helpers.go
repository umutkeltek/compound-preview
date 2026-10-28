//go:build darwin

package schedule

import (
	"encoding/json"
	"fmt"
	"os"
)

// Small CLI helpers the organ needs, mirrored byte-compatible from cmd/compound
// (hasFlag/valueFlag from main.go, printJSON from main.go). The cmd-budget
// ratchet keeps cmd/compound a thin dispatch shell, so the organ carries its
// own copies rather than importing the shell (precedent: internal/store/helpers.go).

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func valueFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}
