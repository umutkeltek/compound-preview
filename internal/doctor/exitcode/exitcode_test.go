// Tests pinning the shared doctor exit-code contract. The contract is
// named in the package docstring; these tests fail loudly if a future
// change drifts. The doctor's three callers (compound doctor, compound
// memory doctor, compound work proof-chain-doctor) share this contract
// by import, not by copy — when a change breaks one, the fix lives here.
package exitcode

import (
	"bytes"
	"strings"
	"testing"
)

// OK state never writes the count line (it would train the operator to
// look for it). The doctor's text mode stays quiet on success.
func TestDoctorExitWith_OKStaysQuiet(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	got := Doctor{Verb: "doctor"}.ExitWith(&out)
	if got != OK {
		t.Fatalf("ExitWith on empty doctor = %d, want %d", got, OK)
	}
	if out.Len() != 0 {
		t.Fatalf("ok path emitted output; want silence:\n%s", out.String())
	}
}

// Non-empty Problems emit the canonical count line, in the exact shape a
// tired human (and a shell caller grepping for it) learns to read. Drift
// here means every downstream grep stops working.
func TestDoctorExitWith_ProblemsPrintsFinalLine(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := Doctor{
		Verb:     "doctor",
		Problems: []string{"stale worktree lock at /tmp/lane"},
		Note:     "run `compound work delegate sweep`",
	}
	got := d.ExitWith(&out)
	if got != ProblemFound {
		t.Fatalf("ExitWith on problem = %d, want %d", got, ProblemFound)
	}
	want := "doctor: 1 problem(s) found — run `compound work delegate sweep`"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("missing canonical line %q in:\n%s", want, out.String())
	}
}

// Warnings print under a `warnings:` heading BEFORE the count line, so
// an operator scanning the bottom of the output sees both without
// scrolling. Warnings NEVER change the exit code.
func TestDoctorExitWith_WarningsBlockBeforeCountLine(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := Doctor{
		Verb:     "memory doctor",
		Warnings: []string{"synthesized capture warning"},
		Problems: []string{"real promotion failure"},
		Note:     "see hops above",
	}
	got := d.ExitWith(&out)
	if got != ProblemFound {
		t.Fatalf("exit = %d, want %d", got, ProblemFound)
	}
	rendered := out.String()
	warnIdx := strings.Index(rendered, "warnings:")
	countIdx := strings.Index(rendered, "memory doctor: 1 problem(s) found")
	if warnIdx < 0 || countIdx < 0 || warnIdx > countIdx {
		t.Fatalf("expected warnings: before count line, got:\n%s", rendered)
	}
}

// Warnings only (no problems) exit 0 — the contract is explicit that
// warnings are info-only and never cause a non-zero exit. Buried
// warnings inside "ok": true were the failure mode this contract
// opens with; the doctor that honors this test refuses to recreate it.
func TestDoctorExitWith_WarningsOnlyExitsZero(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	got := Doctor{Verb: "doctor", Warnings: []string{"info-only note"}}.ExitWith(&out)
	if got != OK {
		t.Fatalf("exit = %d, want %d (warnings must not fail)", got, OK)
	}
	if !strings.Contains(out.String(), "warnings:") {
		t.Fatalf("warnings block missing on warnings-only path:\n%s", out.String())
	}
	if strings.Contains(out.String(), "problem(s) found") {
		t.Fatalf("warnings-only path emitted the count line:\n%s", out.String())
	}
}

// SortedProblems is stable across runs. A doctor that emits the count
// line from Problems directly would print different orders on different
// runs (map iteration); this guard makes the count line reproducible.
func TestDoctorSortedProblems_Deterministic(t *testing.T) {
	t.Parallel()
	d := Doctor{Problems: []string{"b", "a", "c"}}
	got := d.SortedProblems()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("SortedProblems[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// FinalLine is the exact string the doctor emits on a problem; the
// packet documents it as `doctor: N problem(s) found — <hint>`. If a
// future refactor rewrites it, the doctor's grep targets in scripts and
// tests stop matching.
func TestDoctorFinalLine_FormatLocked(t *testing.T) {
	t.Parallel()
	d := Doctor{Verb: "doctor", Problems: []string{"x", "y"}, Note: "fix it"}
	got := d.FinalLine()
	want := "doctor: 2 problem(s) found — fix it"
	if got != want {
		t.Fatalf("FinalLine = %q, want %q", got, want)
	}
}

// CouldNotRun is a distinct exit so an operator distinguishes "house is
// on fire" from "my doctor is broken". The constant must NOT share a
// value with OK or ProblemFound.
func TestExitCodesDistinct(t *testing.T) {
	t.Parallel()
	seen := map[int]string{}
	for _, c := range []struct {
		code int
		name string
	}{
		{OK, "OK"},
		{ProblemFound, "ProblemFound"},
		{CouldNotRun, "CouldNotRun"},
	} {
		if other, dup := seen[c.code]; dup {
			t.Fatalf("exit code %d shared by %s and %s", c.code, other, c.name)
		}
		seen[c.code] = c.name
	}
}
