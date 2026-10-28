package harness

import "os"

// CheckExecutionWorkspaceExists proves a resolved execution workspace is a usable
// directory before a governed advance, dispatch claim, or manual run can mint.
// The returned observation is the content-preserving CLI refusal trace.
func CheckExecutionWorkspaceExists(workspace string) (observed string, ok bool) {
	observed = "stat " + workspace + ": "
	info, statErr := os.Stat(workspace)
	switch {
	case statErr != nil:
		observed += statErr.Error()
	case !info.IsDir():
		observed += "exists but is not a directory"
	default:
		observed += "ok"
	}
	return observed, statErr == nil && info.IsDir()
}
