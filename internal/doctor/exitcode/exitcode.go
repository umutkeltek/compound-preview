// Package exitcode defines the SHARED exit-code contract every Compound
// doctor speaks. Living in one place (and named once) is the only way to
// stop three near-identical doctor bodies from drifting apart: a doctor that
// cannot fail is decoration, and a doctor that disagrees with the others
// about what "fail" means teaches every downstream caller a different lesson
// — heartbeat, CI, and the operator's eye must all see the same exit.
//
// CONTRACT (doctors that import this package MUST honor it):
//
//	exit 0 = no problems found. The doctor ran end-to-end and every check
//	        it speaks for is healthy. Warnings (info-only) are surfaced in
//	        the doctor's own output but do NOT change the exit code.
//
//	exit 1 = at least one problem found. The doctor ran end-to-end and at
//	        least one check found a real, actionable defect. When printing
//	        to a terminal, the LAST line of output is exactly:
//
//	            doctor: <N> problem(s) found — <one sentence on what to do first>
//
//	        where <N> is the number of distinct problem rows the doctor is
//	        willing to name (not raw check counts). The "what to do first"
//	        clause names the single highest-leverage remediation a tired
//	        operator can run; it MUST be a compound door, not internal jargon.
//
//	exit 2 = the doctor itself could not run. The body failed before any
//	        check produced a verdict: a bad flag, a malformed home, a missing
//	        dependency, a refused provider, etc. The body prints the cause on
//	        stderr and exits 2 so a caller can distinguish "doctor is fine,
//	        house is on fire" from "doctor is not fine".
//
// JSON SHAPE: every JSON-emitting doctor MUST serialize the warnings it found
// under a "warnings" array (each entry a short string). Buried warnings
// inside `"ok": true` is the failure mode this contract is named for:
// `compound memory doctor` used to print "ok": true while a hop was warning,
// and no caller could tell. The doctor reports a problem when ANY of these
// is true at exit time:
//
//   - any check returned status="fail"
//   - any check returned status="warn" AND the doctor treats warn-as-problem
//     (each doctor documents its own policy; the default is to count warn
//     toward the problem total when the underlying check is actionable)
//   - the doctor could not run (exit 2 path, with a different exit code)
//
// WHY ONE PACKAGE: a copy-paste of this comment into three doctor bodies
// drifts. The internal/doctor package already exists for shared signal
// rendering (binary staleness, harness version drift); this package sits
// next to it so every doctor that imports compound/internal/doctor can
// reach the contract by import path.
package exitcode

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Exit codes are the three values a doctor may emit. They are explicit
// constants so tests and shell callers can compare by name, not by magic
// number. The values mirror the unix convention (0=ok, 1=user-actionable,
// 2=misuse) and stay stable across all Compound doctors.
const (
	// OK (clean) — the doctor ran, every check was healthy (warnings
	// allowed and surfaced under "warnings", but never inflate the count).
	OK = 0

	// ProblemFound — at least one actionable problem. The doctor's
	// terminal line MUST name the count and one remediation, per the
	// package docstring.
	ProblemFound = 1

	// CouldNotRun — the doctor could not produce a verdict. Distinct
	// from ProblemFound so an operator distinguishes "house is on fire"
	// from "my doctor is broken".
	CouldNotRun = 2
)

// Doctor names the package's view of one doctor invocation: a count of
// problems, a count of warnings (info-only), a short human hint naming the
// single first remediation, and the doctor verb (e.g. "doctor", "memory
// doctor", "work proof-chain-doctor") for the terminal count line. Every
// verb composes one Result before exiting.
//
// Warnings stay separate from Problems on purpose: a warning is info-only
// ("this looks off but nothing is broken"), a problem is actionable
// ("this is broken — fix it"). The contract lets a doctor promote a
// warning to a problem (treat-warn-as-problem) at the call site, so the
// shared helper stays policy-agnostic and the policy lives where it is
// documented.
type Doctor struct {
	// Verb is the short name printed at the start of the terminal count
	// line. Convention: `compound doctor` → "doctor"; `compound memory
	// doctor` → "memory doctor"; `compound work proof-chain-doctor` →
	// "proof-chain-doctor". The leading "compound " is omitted so the
	// line reads cleanly when prefixed by a shell prompt.
	Verb string

	// Problems is the named problems the doctor found, each a short
	// human sentence that names the thing wrong AND what to do about
	// it (a verb, not a noun). Order is not significant: Exit sorts by
	// the problem string so the count line is deterministic across
	// runs. Empty Problems + non-empty Warnings => exit 0; a doctor
	// that wants warn-as-problem passes the warnings as Problems.
	Problems []string

	// Warnings is the warnings the doctor found, each a short human
	// sentence. Warnings NEVER cause a non-zero exit. They are
	// surfaced in JSON output under "warnings" and in text output
	// under a "warnings:" heading so an operator can SEE them; the
	// doctor just refuses to fail because of them.
	Warnings []string

	// Note is an optional one-line annotation appended to the terminal
	// count line. Empty omits. The doctor leaves the hint wording to
	// the caller (different doctors have different first actions), so
	// this string is whatever the caller decides is the single
	// highest-leverage first step for the problems it found.
	Note string
}

// SortedProblems returns the problems in a stable order so tests and the
// terminal count line never depend on map-iteration order. The verb+note
// header is unaffected.
func (d Doctor) SortedProblems() []string {
	out := make([]string, len(d.Problems))
	copy(out, d.Problems)
	sort.Strings(out)
	return out
}

// ProblemCount is the number the terminal count line prints. Equal to
// len(d.Problems) — Problems is the source of truth; this helper is the
// single read site so a future "deduplicate identical problem strings"
// ratchet lands here.
func (d Doctor) ProblemCount() int { return len(d.Problems) }

// Exit returns the numeric exit code for this doctor state. OK on no
// problems; ProblemFound otherwise. CouldNotRun is reserved for the
// caller's own prelude (bad flag, missing dep) and is set explicitly by
// callers via ExitWith — see package docstring.
//
// This function does NOT print anything. The terminal count line is the
// caller's job so each doctor can compose it with its existing text body
// without losing the surrounding sections.
func (d Doctor) Exit() int {
	if d.ProblemCount() == 0 {
		return OK
	}
	return ProblemFound
}

// FinalLine returns the final plain-language line the doctor's text mode
// MUST print before exiting non-zero. The format is fixed (a tired human
// learns to read it after one bad night) and reads:
//
//	<verb>: <N> problem(s) found — <one sentence on what to do first>
//
// where <N> is d.ProblemCount() and the trailing sentence is d.Note (or
// "see lines above" if Note is empty). The line is deterministic so a
// shell caller grepping for the exact phrase (`grep "^doctor:") can
// rely on it. Callers writing the text mode output append this AFTER
// their existing sections; the line is the LAST thing the doctor emits.
func (d Doctor) FinalLine() string {
	hint := strings.TrimSpace(d.Note)
	if hint == "" {
		hint = "see lines above"
	}
	return fmt.Sprintf("%s: %d problem(s) found — %s", d.Verb, d.ProblemCount(), hint)
}

// ExitWith is the one-call path: it writes the warnings block (if any),
// writes the terminal count line (only when there ARE problems, so the
// ok path stays quiet), and returns the exit code. The caller passes the
// writer because each doctor already has a stdout/stderr seam it owns;
// the helper does not write to os.Stdout implicitly (tests would race
// on the package-level file). Printers are the right shape because
// doctors speak to humans; structured stdout output lives in the
// doctor's own JSON path, never here.
func (d Doctor) ExitWith(w io.Writer) int {
	if w == nil {
		w = io.Discard
	}
	if len(d.Problems) > 0 {
		fmt.Fprintln(w, "problems:")
		for _, line := range d.SortedProblems() {
			fmt.Fprintf(w, "  - %s\n", line)
		}
	}
	if len(d.Warnings) > 0 {
		fmt.Fprintln(w, "warnings:")
		for _, line := range d.SortedWarnings() {
			fmt.Fprintf(w, "  - %s\n", line)
		}
	}
	code := d.Exit()
	if code != OK {
		fmt.Fprintln(w, d.FinalLine())
	}
	return code
}

// SortedWarnings returns the warnings in a stable order so the
// "warnings:" block is deterministic across runs.
func (d Doctor) SortedWarnings() []string {
	out := make([]string, len(d.Warnings))
	copy(out, d.Warnings)
	sort.Strings(out)
	return out
}
