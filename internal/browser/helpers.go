//go:build darwin

package browser

// Small CLI helpers the organ needs, mirrored byte-compatible from cmd/compound
// (hasFlag/valueFlag from main.go). The cmd-budget ratchet keeps cmd/compound a
// thin dispatch shell, so the organ carries its own copies rather than
// importing the shell (precedent: internal/store/helpers.go). printJSONBrowser
// already lives in cli.go.

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
