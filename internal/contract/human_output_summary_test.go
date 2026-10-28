package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

// The orient/hook ContextPack summary is the surface a hook actually injects
// (internal/hook serves pack.SummaryMD verbatim in its orientation section),
// so it must carry the same human-communication semantics as the static
// block. Repo kinds are covered because a normal product task is the common
// case, not the control-plane one.

func TestContextPackSummaryCarriesHumanOutputSemantics(t *testing.T) {
	for _, repoKind := range []string{"product", "control-plane"} {
		t.Run(repoKind, func(t *testing.T) {
			in := Input{
				Harness:     "codex",
				Cwd:         "/repo/example",
				Task:        "synthetic product fixture",
				RepoKind:    repoKind,
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			pack := Pack(in, DefaultPolicy(), fixedNow)
			summary := pack.SummaryMD
			for _, want := range humanOutputSemantics {
				if !strings.Contains(summary, want) {
					t.Fatalf("%s ContextPack summary lost human-communication behavior %q:\n%s", repoKind, want, summary)
				}
			}
			if !strings.Contains(summary, "Begin non-trivial tasks with") {
				t.Fatalf("%s ContextPack summary lost the orient reminder:\n%s", repoKind, summary)
			}
			// The baseline leads; the runtime orientation line follows it.
			leadAt := strings.Index(summary, "Help the person understand and decide.")
			orientedAt := strings.Index(summary, "Oriented as a")
			if leadAt < 0 || orientedAt < 0 || !(leadAt < orientedAt) {
				t.Fatalf("%s ContextPack summary does not lead with the baseline:\n%s", repoKind, summary)
			}
			// The hook serves this string verbatim inside a capped section;
			// a summary longer than that cap would silently cut the tail.
			if len([]rune(summary)) >= 2400 {
				t.Fatalf("%s ContextPack summary (%d runes) no longer fits the hook orientation cap:\n%s", repoKind, len([]rune(summary)), summary)
			}
		})
	}
}

// The summary must not substitute runtime values into the baseline or the
// orient reminder; per-run task text stays below them.
func TestContextPackSummaryBaselineStaysTaskTextFree(t *testing.T) {
	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/example",
		Task:        "the orient path is allowed to carry task text",
		RepoKind:    "product",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack := Pack(in, DefaultPolicy(), fixedNow)
	if !strings.Contains(pack.SummaryMD, "compound orient --harness <supported-name> --cwd <repository> --task <task>") {
		t.Fatalf("orient reminder not verbatim in summary:\n%s", pack.SummaryMD)
	}
	for _, leak := range []string{
		"compound orient --harness the orient",
		"--cwd /repo/example --task",
	} {
		if strings.Contains(pack.SummaryMD, leak) {
			t.Fatalf("summary substituted runtime values into the shared baseline (%q):\n%s", leak, pack.SummaryMD)
		}
	}
	// Task text is still served — just never inside the baseline lead.
	if !strings.Contains(pack.TaskContract.Intent, in.Task) {
		t.Fatalf("pack lost its own task text: %q", pack.TaskContract.Intent)
	}
	if strings.Contains(HumanOutputBaseline(), in.Task) {
		t.Fatalf("task text leaked into the shared baseline:\n%s", HumanOutputBaseline())
	}
}

// The real constitution, not a three-law fixture, shares the hook's existing
// 2400-rune orientation-summary budget with communication guidance.
func TestContextPackSummaryWithConstitutionFitsHookOrientationBudget(t *testing.T) {
	for _, harness := range []string{"codex", "claude_code"} {
		for _, kind := range []string{"product", "control-plane"} {
			t.Run(harness+"/"+kind, func(t *testing.T) {
				in := Input{Harness: harness, Cwd: filepath.Join("..", ".."), Task: "synthetic fixture", RepoKind: kind, RepoKindVia: "flag", Mode: "execute"}
				pack := Pack(in, DefaultPolicy(), fixedNow)
				if !strings.Contains(pack.SummaryMD, "16. Govern in proportion to risk.") {
					t.Fatal("real Tier-0 constitution was not loaded")
				}
				section := "\n## Summary\n" + pack.SummaryMD + "\n"
				if size := len([]rune(section)); size > 2400 {
					t.Fatalf("guidance and Tier-0 laws exceed the existing hook budget: %d > 2400", size)
				}
				t.Logf("guidance and Tier-0 laws fit: %d/2400 runes", len([]rune(section)))
			})
		}
	}
}
