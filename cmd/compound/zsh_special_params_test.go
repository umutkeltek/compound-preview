package main

// zsh special-parameter guard. In zsh a handful of lowercase parameter names are not
// ordinary variables: they are TIED to shell state. Assigning one as a scalar does not
// shadow it, it corrupts the thing it is tied to -- and the corruption is silent at the
// assignment site, surfacing later as an unrelated failure.
//
// Measured 2026-07-27 (probe over all 18 candidate specials, `local <p>=/tmp/scratch`
// followed by an external command): exactly TWO break:
//
//	path    -> tied to $PATH. `local path=...` empties command lookup for the rest of the
//	           function: `command not found: head`, thrown from a line that has nothing to
//	           do with the assignment. This is the one that bites, because `path` is the
//	           natural name for a variable holding a file path. Lived: a citation resolver
//	           used `local path="${cite%%:*}"` and died seven lines later on a missing
//	           `head`.
//	status  -> read-only. `local status=...` aborts the function with
//	           `read-only variable: status`.
//
// The other tied parameters (cdpath, fpath, manpath, module_path, fignore, mailpath,
// psvar, watch, prompt, argv, options, commands, functions, aliases, parameters,
// dirstack) do NOT break command execution when assigned locally, so they are
// deliberately NOT flagged -- a guard that cries wolf on a harmless `prompt=` teaches
// people to ignore it.
//
// Re-probing warning: probe a tied parameter with an EXTERNAL command, never a builtin.
// The first probe run for this guard reported a false "ok" for `path`, because `echo`
// survives an empty $PATH.
//
// This lives as a Go test because `go test ./...` is what actually runs in this repo:
// there is no CI workflow and the Makefile has two targets (install, release-artifacts),
// so a standalone shell linter would be a surface nobody invokes. The rule needs to run
// itself, not wait to be remembered.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// zshSpecialAssignmentRe matches an assignment to a fatal zsh special parameter.
//
// The leading (^|[;&|(){}[:space:]]) is load-bearing: without it this fires on every
// ordinary name that merely ENDS in one of the words -- `cite_path=`, `repo_path=`,
// `exit_status=` -- which is most of them, and a guard with that false-positive rate gets
// deleted rather than obeyed. Pinned by TestZshSpecialAssignmentDetector.
var zshSpecialAssignmentRe = regexp.MustCompile(
	`(^|[;&|(){}[:space:]])(local[[:space:]]+|typeset[[:space:]]+|declare[[:space:]]+|export[[:space:]]+)?(path|status)=`,
)

type zshAssignmentHit struct {
	file string
	line int
	text string
}

// isZshScript reports whether a file is interpreted by zsh: a `.zsh` extension, or a
// shebang naming zsh. Bash treats `path` as an ordinary name, so a bash script assigning
// it is not a defect and must not be reported.
func isZshScript(name string, firstLine string) bool {
	if strings.HasSuffix(name, ".zsh") {
		return true
	}
	return strings.HasPrefix(firstLine, "#!") && strings.Contains(firstLine, "zsh")
}

// TestZshScriptsDoNotAssignSpecialParameters scans every tracked zsh script and fails on an
// assignment to a fatal special parameter, naming file:line and the fix.
//
// Falsifier: add `local path=/tmp/x` to any tracked zsh script and this goes RED naming that
// file:line; remove it and this goes GREEN. Runtime/private/untracked scripts are deliberately
// outside this source invariant.
func TestZshScriptsDoNotAssignSpecialParameters(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")

	hits, err := zshSpecialAssignmentHits(repoRoot)
	if err != nil {
		t.Fatalf("scan repo: %v", err)
	}

	for _, h := range hits {
		t.Errorf(
			"%s:%d assigns a tied zsh special parameter -- rename it (e.g. cite_path, exit_status):\n    %s\n"+
				"    `path` is tied to $PATH: assigning it empties command lookup for the rest of the\n"+
				"    function, and the failure surfaces later as `command not found` on an unrelated line.\n"+
				"    `status` is read-only: assigning it aborts the function.",
			h.file, h.line, h.text,
		)
	}
}

func zshSpecialAssignmentHits(repoRoot string) ([]zshAssignmentHit, error) {
	var hits []zshAssignmentHit
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--", "*.sh", "*.zsh")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked shell scripts: %w", err)
	}
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		rel := filepath.Clean(filepath.FromSlash(string(rawPath)))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("git returned shell script outside repository: %q", rel)
		}
		path := filepath.Join(repoRoot, rel)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// A tracked file deleted in the working tree cannot contain an assignment.
			if os.IsNotExist(readErr) {
				continue
			}
			return nil, fmt.Errorf("read tracked shell script %s: %w", rel, readErr)
		}
		lines := strings.Split(string(body), "\n")
		if len(lines) == 0 || !isZshScript(path, lines[0]) {
			continue
		}
		for i, line := range lines {
			code := line
			if idx := strings.Index(code, "#"); idx >= 0 {
				code = code[:idx] // a comment naming the trap is not the trap
			}
			if zshSpecialAssignmentRe.MatchString(code) {
				hits = append(hits, zshAssignmentHit{file: rel, line: i + 1, text: strings.TrimSpace(line)})
			}
		}
	}
	return hits, nil
}

// TestZshSpecialAssignmentDetector pins the detector itself, because a guard that
// over-reports is a guard that gets switched off. The negative cases are the ones that
// matter: every one of them appeared in real scripts in this repo.
func TestZshSpecialAssignmentDetector(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		// Positives -- the defect in each form it can take.
		{"bare", `path=/tmp/x`, true},
		{"local", `  local path="${cite%%:*}"`, true},
		{"typeset", `typeset path=/tmp/x`, true},
		{"export", `export path=/tmp/x`, true},
		{"status", `local status=done`, true},
		{"after semicolon", `cd /tmp; path=/x`, true},
		{"inside braces", `f() { path=/x; }`, true},

		// Negatives -- ordinary names that merely END in a special word. Without the
		// leading boundary these all false-positive, which is most real code.
		{"suffix cite_path", `local cite_path="${cite%%:*}"`, false},
		{"suffix repo_path", `repo_path=/tmp/x`, false},
		{"suffix exit_status", `exit_status=1`, false},
		{"uppercase PATH", `PATH=/usr/bin:$PATH`, false},
		{"other tied param", `fpath=(/usr/share/zsh $fpath)`, false},
		{"reference not assignment", `echo "$path"`, false},
		{"comparison", `[[ "$status" == ok ]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zshSpecialAssignmentRe.MatchString(tc.line); got != tc.want {
				t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestZshSpecialAssignmentGuardScansTrackedScriptsOnly(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	runZshGuardGit(t, repoRoot, "init", "--quiet")

	trackedPath := filepath.Join(repoRoot, "tracked.zsh")
	if err := os.WriteFile(trackedPath, []byte("#!/bin/zsh\nlocal cite_path=/tmp/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runZshGuardGit(t, repoRoot, "add", "tracked.zsh")

	untrackedPath := filepath.Join(repoRoot, "runtime.zsh")
	if err := os.WriteFile(untrackedPath, []byte("#!/bin/zsh\nlocal path=/tmp/runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, err := zshSpecialAssignmentHits(repoRoot)
	if err != nil {
		t.Fatalf("scan with untracked runtime script: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("untracked/runtime script must be outside the source guard, got %+v", hits)
	}

	if err := os.WriteFile(trackedPath, []byte("#!/bin/zsh\nlocal path=/tmp/source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err = zshSpecialAssignmentHits(repoRoot)
	if err != nil {
		t.Fatalf("scan tracked source script: %v", err)
	}
	if len(hits) != 1 || hits[0].file != "tracked.zsh" {
		t.Fatalf("tracked source defect must be caught exactly once, got %+v", hits)
	}
}

func runZshGuardGit(t *testing.T, repoRoot string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
