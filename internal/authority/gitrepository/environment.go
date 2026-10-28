package gitrepository

import (
	"os"
	"strings"
)

// ReadEnvironment pins local repository inspections to their explicit -C
// argument and the repository's own configuration. Ambient Git selectors must
// not substitute another index, object directory, worktree or configuration.
// This environment is for local reads, not transport or source mutations.
func ReadEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			env = append(env, value)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0")
}
