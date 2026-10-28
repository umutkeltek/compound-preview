package hookplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Installed PreToolUse postures. The exact --enforce word is part of a
// binding's identity: two configs can share one binary while deliberately
// asking it for different postures.
const (
	HookPostureObservationOnly = "observation_only"
	HookPostureEnforced        = "enforced"
)

// InstalledHookPosture is one Compound PreToolUse binding a harness config would execute.
type InstalledHookPosture struct {
	Binary  string
	Args    []string
	Posture string
}

// InstalledHookPostures reads a harness config and returns every Compound
// PreToolUse binding it would execute. It is THE reader: `compound doctor`
// probes these bindings and the launch stamp records the posture they name,
// so the two can never describe different installs.
func InstalledHookPostures(configPath string) []InstalledHookPosture {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	return installedHookPosturesFrom(data)
}

func installedHookPosturesFrom(data []byte) []InstalledHookPosture {
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	var installed []InstalledHookPosture
	for _, group := range root.Hooks["PreToolUse"] {
		for _, entry := range group.Hooks {
			words, ok := HookCommandWords(entry.Command)
			if !ok || len(words) < 4 ||
				words[1] != "hook" ||
				words[2] != "pre-tool-use" ||
				!containsWord(words[3:], "--live") {
				continue
			}
			posture := HookPostureObservationOnly
			if containsWord(words[3:], "--enforce") {
				posture = HookPostureEnforced
			}
			installed = append(installed, InstalledHookPosture{
				Binary:  words[0],
				Args:    append([]string(nil), words[1:]...),
				Posture: posture,
			})
		}
	}
	return installed
}

func containsWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}

// HookConfigPath names the native config Compound installs a harness's hooks
// into, under baseDir, or ("", false) when Compound installs no hooks for it.
// Registry names (claude_minimax) and installer labels (claude-minimax) both resolve.
func HookConfigPath(baseDir, harnessName string) (string, bool) {
	if path, ok := LaneHookSettingsPath(baseDir, harnessName); ok {
		return path, true
	}
	switch strings.TrimSpace(harnessName) {
	case "claude_code", "claude-code":
		return filepath.Join(baseDir, ConfigRel("claude-code", "")), true
	case "codex":
		return filepath.Join(baseDir, ConfigRel("codex", "")), true
	}
	return "", false
}

// LiveHookPosture is the harness's installed PreToolUse posture at this moment.
type LiveHookPosture struct {
	// Posture is enforce | observe | none.
	Posture    string
	ConfigPath string
	// ConfigDigest is sha256:<hex> of the config bytes read, empty when absent.
	ConfigDigest string
}

// ReadLiveHookPosture reads the harness's hook config with the same reader
// doctor uses. Any enforced binding makes the harness "enforce"; otherwise any
// observation binding makes it "observe"; an absent config or one with no
// Compound binding is "none". ok=false when Compound knows no config path for
// the harness: that posture is unknown, not "none".
func ReadLiveHookPosture(baseDir, harnessName string) (LiveHookPosture, bool) {
	path, ok := HookConfigPath(baseDir, harnessName)
	if !ok || strings.TrimSpace(baseDir) == "" {
		return LiveHookPosture{}, false
	}
	live := LiveHookPosture{Posture: "none", ConfigPath: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return live, true
	}
	sum := sha256.Sum256(data)
	live.ConfigDigest = "sha256:" + hex.EncodeToString(sum[:])
	for _, installed := range installedHookPosturesFrom(data) {
		if installed.Posture == HookPostureEnforced {
			live.Posture = "enforce"
			break
		}
		live.Posture = "observe"
	}
	return live, true
}

// HookCommandWords is the deliberately small POSIX word parser needed for commands written by
// hookplan. It handles whitespace, single- and double-quoted words, and backslash escapes. In
// particular, it understands the installer's close-quote/backslash-quote/reopen form for an
// apostrophe inside a path. Shell expansions and operators are outside the canonical hook command
// alphabet and are not evaluated.
func HookCommandWords(command string) ([]string, bool) {
	var (
		words       []string
		word        strings.Builder
		quote       byte
		wordStarted bool
	)
	flush := func() {
		if !wordStarted {
			return
		}
		words = append(words, word.String())
		word.Reset()
		wordStarted = false
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch quote {
		case '\'':
			if ch == '\'' {
				quote = 0
				continue
			}
			word.WriteByte(ch)
			wordStarted = true
		case '"':
			if ch == '"' {
				quote = 0
				continue
			}
			if ch == '\\' {
				i++
				if i >= len(command) {
					return nil, false
				}
				word.WriteByte(command[i])
				wordStarted = true
				continue
			}
			word.WriteByte(ch)
			wordStarted = true
		default:
			switch {
			case unicode.IsSpace(rune(ch)):
				flush()
			case ch == '\'' || ch == '"':
				quote = ch
				wordStarted = true
			case ch == '\\':
				i++
				if i >= len(command) {
					return nil, false
				}
				word.WriteByte(command[i])
				wordStarted = true
			default:
				word.WriteByte(ch)
				wordStarted = true
			}
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return words, true
}
