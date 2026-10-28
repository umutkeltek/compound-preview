package contract

import (
	"os"
	"path/filepath"
	"strings"
)

// StageSyntheticHookHandlers parses a hook-registration fixture (.ts) or codex hooks JSON fixture
// and writes a synthetic regular file under home at every concrete handler ref the fixture declares.
//
// Tests stage these synthetic handler bytes so installed-handler attestation resolves against the
// test's own HOME instead of the operator's real $HOME. Without this, the same import passes on a
// machine that has the real harness handlers installed ($HOME/.claude/hooks/*, $HOME/.local/bin/
// compound, …) and fails in a fresh CI clone where those files are absent: every concrete handler
// attests as handler_unavailable and is excluded from active hooks, so tests that assert an
// installed hook is live pass locally and red on the CI runner.
//
// Production attestation is unchanged — when this helper is not used, installedHookHandlerRevision
// attests against os.UserHomeDir() exactly as before. The caller must point HOME at home
// (t.Setenv("HOME", home)) before calling, so the fixture's $HOME-qualified refs expand under home.
func StageSyntheticHookHandlers(home, fixturePath string) error {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return err
	}
	manifest, err := parseHookRegistrationFixture(fixturePath, data)
	if err != nil {
		return err
	}
	sep := string(os.PathSeparator)
	homePrefix := home
	if !strings.HasSuffix(homePrefix, sep) {
		homePrefix += sep
	}
	for _, hook := range manifest.Hooks {
		ref := strings.TrimSpace(hook.Ref)
		if !concreteInstalledHookHandlerRef(ref) {
			continue
		}
		// Only stage refs the fixture expanded under our temp home; an opaque or unrelated ref
		// (handler:/hook: scheme, or somehow rooted elsewhere) is left alone.
		if ref != home && !strings.HasPrefix(ref, homePrefix) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(ref, []byte("// compound test fixture: synthetic handler bytes\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func parseHookRegistrationFixture(fixturePath string, data []byte) (InstalledHooksManifest, error) {
	if strings.HasSuffix(fixturePath, ".ts") {
		return parseHookRegistrationTS(data)
	}
	return parseCodexHooksJSON(data)
}
