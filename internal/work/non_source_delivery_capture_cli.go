package work

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// RunWorkDeliveryCaptureCLI always prints the structured outcome, including
// partial custody and the exact retry route. Capture is not binding or close.
func RunWorkDeliveryCaptureCLI(args []string, store *Store, now time.Time, stdout, stderr io.Writer) int {
	var workID, requestPath, actor string
	seen := map[string]bool{}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: compound work delivery-capture <work_id> --request <file> --by <actor> [--json]")
		return 2
	}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
		case "--request", "--by":
			if seen[arg] || i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") || strings.TrimSpace(args[i+1]) == "" {
				return usage()
			}
			seen[arg] = true
			i++
			if arg == "--request" {
				requestPath = args[i]
			} else {
				actor = args[i]
			}
		default:
			if strings.HasPrefix(arg, "-") || workID != "" {
				return usage()
			}
			workID = arg
		}
	}
	if workID == "" || requestPath == "" || actor == "" {
		return usage()
	}
	var err error
	requestPath, err = filepath.Abs(requestPath)
	if err != nil {
		fmt.Fprintln(stderr, "error: resolve capture request:", err)
		return 1
	}
	result, err := store.CaptureNonSourceDeliveryFile(workID, requestPath, actor, now)
	result.RetryCommand = []string{"compound", "work", "delivery-capture", workID, "--request", requestPath, "--by", actor, "--json"}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		fmt.Fprintln(stderr, "error: write capture outcome:", encodeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: delivery capture:", err, "(partial state may exist; retry the unchanged request using retry_command)")
		return 1
	}
	return 0
}
