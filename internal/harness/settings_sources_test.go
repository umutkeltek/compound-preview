package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func settingsSourceFixture(t *testing.T, body string) (string, string, string) {
	t.Helper()
	h := t.TempDir()
	target := filepath.Join(t.TempDir(), "config.toml")
	source := filepath.Join(t.TempDir(), "launch.env")
	if err := os.WriteFile(target, []byte("model = \"gpt-example\"\nmodel_reasoning_effort = \"xhigh\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, h, target, codexTOMLAuthorityFixture(map[string]any{"model": "gpt-example", "model_reasoning_effort": "xhigh"}))
	path := PermissionAuthorityPath(h, "codex")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var authority map[string]any
	if err := yaml.Unmarshal(b, &authority); err != nil {
		t.Fatal(err)
	}
	authority["setting_sources"] = []any{map[string]any{"id": "interactive-launch", "owner": "executor:test", "format": "shell_env", "path": source, "mappings": map[string]string{"model": "LAUNCH_MODEL", "model_reasoning_effort": "LAUNCH_EFFORT"}}}
	b, err = yaml.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return h, target, source
}

func TestPermissionDriftDetectsDeclaredLauncherOverride(t *testing.T) {
	h, target, source := settingsSourceFixture(t, "export LAUNCH_MODEL=\"gpt-old\"\nexport LAUNCH_EFFORT=xhigh # literal\nUNMAPPED_SECRET=do-not-render-this-value\n")
	before, _ := os.ReadFile(target)
	sb, _ := os.ReadFile(source)
	report, err := PermissionDrift(h, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted {
		t.Fatal("native defaults match but declared launcher disagrees; drift must fail")
	}
	out, _ := json.Marshal(report)
	if bytes.Contains(out, []byte("do-not-render-this-value")) || bytes.Contains(out, []byte("gpt-old")) {
		t.Fatal("probe disclosed source values")
	}
	after, _ := os.ReadFile(target)
	sa, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) || !bytes.Equal(sb, sa) {
		t.Fatal("read-only probe mutated inputs")
	}
	if err := os.WriteFile(source, []byte("export LAUNCH_MODEL='gpt-example'\nLAUNCH_EFFORT=\"xhigh\" # comment\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assertCodexTOMLDrift(t, h, false, "")
}

func TestPermissionDriftUnavailableLauncherIsNotClean(t *testing.T) {
	for _, body := range []string{
		"LAUNCH_MODEL=gpt-example\n",
		"LAUNCH_MODEL=gpt-example\nLAUNCH_MODEL=gpt-example\nLAUNCH_EFFORT=xhigh\n",
		"LAUNCH_MODEL=$(touch marker)\nLAUNCH_EFFORT=xhigh\n",
		"LAUNCH_MODEL=\"$SECRET\"\nLAUNCH_EFFORT=xhigh\n",
		"LAUNCH_MODEL=gpt-example; echo surprise\nLAUNCH_EFFORT=xhigh\n",
	} {
		t.Run(fmt.Sprintf("case-%d", len(body)), func(t *testing.T) {
			h, _, _ := settingsSourceFixture(t, body)
			r, err := PermissionDrift(h, "codex")
			if err == nil && !r.Drifted {
				t.Fatal("unavailable or dynamic launcher appeared clean")
			}
			if _, err := os.Stat("marker"); !os.IsNotExist(err) {
				t.Fatal("shell source was executed")
			}
		})
	}
	for _, kind := range []string{"missing", "symlink", "unsupported", "unowned"} {
		t.Run(kind, func(t *testing.T) {
			h, _, source := settingsSourceFixture(t, "LAUNCH_MODEL=gpt-example\nLAUNCH_EFFORT=xhigh\n")
			switch kind {
			case "missing":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(t.TempDir(), "other.env")
				if err := os.Rename(source, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, source); err != nil {
					t.Fatal(err)
				}
			default:
				path := PermissionAuthorityPath(h, "codex")
				b, _ := os.ReadFile(path)
				if kind == "unsupported" {
					b = bytes.ReplaceAll(b, []byte("format: shell_env"), []byte("format: unknown"))
				} else {
					b = []byte(strings.Replace(string(b), "model: LAUNCH_MODEL", "not_owned: LAUNCH_MODEL", 1))
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := PermissionDrift(h, "codex")
			if err == nil && !r.Drifted {
				t.Fatal("bad declaration/source appeared clean")
			}
		})
	}
}
