package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHermesPluginRegistersSessionEnd (F7) drives the real Python plugin module
// through a stub context and a compound shim. The plugin lives at the canonical
// repo copy (adapters/hermes/compound_memory/__init__.py) and is byte-compared to
// the installed copy at ~/.hermes/profiles/compoundcanary/plugins/compound_memory/
// in TestHermesInstalledPluginMatchesRepoCopy (F8).
//
// python3 is required to run this test. We Skip cleanly if absent so the suite
// still builds on a stripped-down CI runner.
func TestHermesPluginRegistersSessionEnd(t *testing.T) {
	python3, err := lookPath("python3")
	if err != nil {
		t.Skipf("python3 not on PATH: %v", err)
	}

	// Find the canonical plugin copy relative to the test binary.
	pluginPath, err := filepath.Abs(filepath.Join("..", "..", "adapters", "hermes", "compound_memory", "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("plugin copy not at %s: %v", pluginPath, err)
	}

	tmp := t.TempDir()
	// Bind HOME/HERMES_HOME/COMPOUND_HOME/COMPOUND_BIN at the test process level.
	// The Python driver reads them; the driver script below sets them back so the
	// Go process state is unchanged for any subsequent assertions.
	t.Setenv("HOME", tmp)
	t.Setenv("HERMES_HOME", tmp)
	t.Setenv("COMPOUND_HOME", tmp)

	shimDir := filepath.Join(tmp, "shimbin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(shimDir, "compound")
	shimScript := `#!/usr/bin/env bash
set -eu
# Record every stdin we receive. Each invocation appends one line of the form
# "ARGV:<json-of-argv> STDIN:<stdin>" to the recorded path the test passes via
# COMPOUND_RECORD.
record="${COMPOUND_RECORD:?}"
{
  printf 'ARGV:'
  printf '%s' "$0"
  for a in "$@"; do
    printf ' %s' "$a"
  done
  printf '\nSTDIN:'
  cat
  printf '\n---\n'
} >> "$record"
exit 0
`
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_BIN", shimPath)

	recordedPath := filepath.Join(tmp, "compound-recorded.log")
	t.Setenv("COMPOUND_RECORD", recordedPath)

	// The driver script:
	//   - Creates ~/.hermes/state.db with 2 messages (one user, one assistant with
	//     a tool_name=Skill carrying skill invocation metadata).
	//   - Imports the plugin module via importlib so we are NOT relying on a
	//     system-wide pip install.
	//   - Stubs ctx.register_hook to capture (hook_name, callback) pairs.
	//   - Calls register(ctx) and then invokes the captured on_session_end.
	//   - Asserts the shim received the expected argv AND transcript_path points at
	//     a JSONL whose lines parse under scanSessionTranscript via the GO side
	//     (the test reads the file back and runs scanSessionTranscript).
	//   - Prints a JSON summary on stdout; Go parses and asserts.
	driver := `
import importlib.util, json, os, sqlite3, sys, traceback
plugin_path = os.environ["PLUGIN_PATH"]
spec = importlib.util.spec_from_file_location("compound_memory_plugin", plugin_path)
plugin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plugin)

hooks = {}
class StubCtx:
    def register_hook(self, name, callback):
        hooks[name] = callback
ctx = StubCtx()
plugin.register(ctx)
if "on_session_end" not in hooks:
    print(json.dumps({"ok": False, "err": "on_session_end not registered"}))
    sys.exit(1)

# Build a hermes-shaped state.db with two messages.
hermes_home = os.environ["HERMES_HOME"]
db_path = os.path.join(hermes_home, "state.db")
conn = sqlite3.connect(db_path)
cur = conn.cursor()
cur.execute("CREATE TABLE schema_version (version INTEGER NOT NULL)")
cur.execute("CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, started_at REAL, ended_at REAL)")
# tool_calls TEXT is the structured-JSON column hermes populates for assistant tool
# invocations; the plugin reads it to render proper tool_use blocks.
cur.execute(
    "CREATE TABLE messages ("
    "id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, "
    "content TEXT, tool_name TEXT, tool_calls TEXT, timestamp REAL NOT NULL, "
    "active INTEGER NOT NULL DEFAULT 1"
    ")"
)
session_id = "sess-hermes-0001"
cur.execute("INSERT INTO sessions (id, cwd, started_at, ended_at) VALUES (?, ?, ?, ?)",
            (session_id, "/repo/hermes-fixture", 1724697600.0, 1724697900.0))
# Message 1: user prompt.
cur.execute(
    "INSERT INTO messages (session_id, role, content, tool_name, tool_calls, timestamp) "
    "VALUES (?, ?, ?, ?, ?, ?)",
    (session_id, "user", "audit the run", None, None, 1724697605.0),
)
# Message 2: assistant with a Skill tool call carrying a structured tool_calls JSON
# payload so the plugin can render the parser-grade tool_use block with skill input.
skill_call = json.dumps([{"name": "Skill", "arguments": {"skill": "example-skill"}}])
cur.execute(
    "INSERT INTO messages (session_id, role, content, tool_name, tool_calls, timestamp) "
    "VALUES (?, ?, ?, ?, ?, ?)",
    (session_id, "assistant", "running example-skill", "Skill", skill_call, 1724697610.0),
)
conn.commit()
conn.close()

# Invoke the captured on_session_end callback.
hooks["on_session_end"](
    session_id=session_id,
    completed=True,
    interrupted=False,
    model="openrouter/hermes-test",
    platform="cli",
)

# Read the exported JSONL the plugin wrote.
state_dir = os.path.join(os.environ["COMPOUND_HOME"], "state", "hermes-sessions")
export_path = os.path.join(state_dir, session_id + ".jsonl")
if not os.path.exists(export_path):
    print(json.dumps({"ok": False, "err": "exported JSONL missing at " + export_path}))
    sys.exit(1)
with open(export_path, "r", encoding="utf-8") as fh:
    lines = [json.loads(line) for line in fh if line.strip()]

# Inspect the compound shim's recorded stdin.
record_path = os.environ["COMPOUND_RECORD"]
with open(record_path, "r", encoding="utf-8") as fh:
    record = fh.read()

print(json.dumps({
    "ok": True,
    "hook_registered": sorted(hooks.keys()),
    "exported_lines": len(lines),
    "exported_first_role": lines[0]["role"] if lines else None,
    "exported_second_role": lines[1]["role"] if len(lines) > 1 else None,
    "exported_second_has_tool_use": any(
        b.get("type") == "tool_use" for b in lines[1].get("content", []) if len(lines) > 1
    ),
    "transcript_path": export_path,
    "compound_record": record,
}))
`
	driverPath := filepath.Join(tmp, "driver.py")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(python3, driverPath)
	cmd.Env = append(os.Environ(),
		"PLUGIN_PATH="+pluginPath,
		"HERMES_HOME="+tmp,
		"COMPOUND_HOME="+tmp,
		"COMPOUND_BIN="+shimPath,
		"COMPOUND_RECORD="+recordedPath,
		"HOME="+tmp,
	)
	// Wait up to 30s so a hung plugin cannot wedge the test.
	timer := time.AfterFunc(30*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python3 driver: %v\nstdout=%s", err, string(out))
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse driver output: %v\nraw=%s", err, string(out))
	}
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("driver reported failure: %v", result)
	}
	hooks, _ := result["hook_registered"].([]any)
	if len(hooks) == 0 {
		t.Fatalf("no hooks registered: %v", result)
	}
	hasOnSessionEnd := false
	for _, h := range hooks {
		if h == "on_session_end" {
			hasOnSessionEnd = true
		}
	}
	if !hasOnSessionEnd {
		t.Fatalf("on_session_end not in registered hooks: %v", hooks)
	}

	// The exported JSONL must have 2 lines: user + assistant with a tool_use block.
	if n, _ := result["exported_lines"].(float64); n != 2 {
		t.Fatalf("exported_lines = %v, want 2", result["exported_lines"])
	}
	if role, _ := result["exported_first_role"].(string); role != "user" {
		t.Fatalf("first role = %q, want user", role)
	}
	if role, _ := result["exported_second_role"].(string); role != "assistant" {
		t.Fatalf("second role = %q, want assistant", role)
	}
	if tu, _ := result["exported_second_has_tool_use"].(bool); !tu {
		t.Fatalf("assistant line missing tool_use block: %v", result)
	}

	// The compound shim must have received argv `hook session-end --live --harness hermes`
	// and stdin that points at the exported JSONL.
	record, _ := result["compound_record"].(string)
	if !strings.Contains(record, "hook session-end --live --harness hermes") {
		t.Fatalf("compound shim argv missing expected pattern:\n%s", record)
	}
	exportPath, _ := result["transcript_path"].(string)
	if !strings.Contains(record, exportPath) {
		t.Fatalf("compound shim stdin does not reference exported path %s:\n%s", exportPath, record)
	}

	// The exported JSONL must parse under the existing scanSessionTranscript and
	// produce one skill. This is the cross-format end-to-end check: hermes's plugin
	// exports to claude role/content shape, and the same parser that powers the
	// claude_session_end path picks up the Skill block.
	summary, err := scanSessionTranscript(exportPath)
	if err != nil {
		t.Fatalf("scanSessionTranscript on exported JSONL: %v", err)
	}
	if !containsString(summary.SkillsFired, "capability://skill:example-skill") {
		t.Fatalf("SkillsFired = %v, want capability://skill:example-skill (hermes-exported Skill block)", summary.SkillsFired)
	}
}

// TestHermesInstalledPluginMatchesRepoCopy (F8) byte-equates the canonical plugin copy
// at adapters/hermes/compound_memory/__init__.py with the installed plugin at
// ~/.hermes/profiles/compoundcanary/plugins/compound_memory/__init__.py when that path
// exists. The check skips cleanly on a fresh clone that has never installed the plugin;
//
// // the install path is documented in this packet's live proof (not the test). We use
// $HOME resolution + an env override so the test does NOT bake a personal path into
// tracked source.
func TestHermesInstalledPluginMatchesRepoCopy(t *testing.T) {
	repoPath, err := filepath.Abs(filepath.Join("..", "..", "adapters", "hermes", "compound_memory", "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	repoBytes, err := os.ReadFile(repoPath)
	if err != nil {
		t.Fatalf("read repo copy: %v", err)
	}

	// Resolve the installed path via $HOME + optional HERMES_PROFILE override so the
	// test never embeds a personal path. Profile defaults to compoundcanary.
	profile := os.Getenv("HERMES_PROFILE")
	if profile == "" {
		profile = "compoundcanary"
	}
	installedPath := os.Getenv("HERMES_PLUGIN_INSTALLED_PATH")
	if installedPath == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			t.Skipf("no $HOME available: %v", herr)
		}
		installedPath = filepath.Join(home, ".hermes", "profiles", profile, "plugins", "compound_memory", "__init__.py")
	}
	installedBytes, err := os.ReadFile(installedPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("installed plugin not at %s (fresh clone); copy step documented in packet", installedPath)
		}
		t.Fatalf("read installed copy: %v", err)
	}
	if string(repoBytes) != string(installedBytes) {
		// Emit the first divergence so the operator can fix one side or the other.
		for i := 0; i < len(repoBytes) && i < len(installedBytes); i++ {
			if repoBytes[i] != installedBytes[i] {
				start := i - 50
				if start < 0 {
					start = 0
				}
				end := i + 50
				if end > len(repoBytes) {
					end = len(repoBytes)
				}
				t.Fatalf(
					"installed plugin diverges from repo copy at byte %d\nrepo: ...%s...\ninstalled: ...%s...",
					i,
					string(repoBytes[start:end]),
					string(installedBytes[start:end]),
				)
			}
		}
		t.Fatalf("installed plugin length %d != repo copy length %d", len(installedBytes), len(repoBytes))
	}
}

// lookPath is a thin wrapper around exec.LookPath so the test can stay self-contained
// without re-importing os/exec at multiple sites. Returns the resolved path.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}
