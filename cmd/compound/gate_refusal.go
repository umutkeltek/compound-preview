package main

import (
	"fmt"
	"io"
	"strings"
)

// gateRefusal renders the refusal text of a fail-closed gate.
//
// A gate that fails closed has already made the correct decision; what it usually
// gets wrong is the sentence it prints. "error: cannot determine execution
// workspace; pass --workspace <path>" names a symptom and a flag, which leaves the
// reader to guess which resolution step failed, whether their own invocation was
// wrong, and whether the gate protects anything worth respecting. The predictable
// next move is to re-run with a plausible-looking value and hope — or to reach for
// an ungoverned path that produces a similar-looking artifact.
//
// So a refusal carries four parts, the shape internal/policy's placement guard
// already uses (placementGuardReason):
//
//	Refused  — what did not happen, in the caller's vocabulary
//	Tried    — each resolution step IN ORDER with its observed result, so the reader
//	           can see which step failed rather than inferring it
//	DoThis   — an exact command, not a description of one
//	Prevents — the named failure the gate exists to stop, so the refusal reads as a
//	           protection rather than an obstacle to route around
//
// Tried is the part that carries the most weight and the part most often omitted:
// a resolution ladder that reports only its final verdict is indistinguishable from
// one that never ran.
type gateRefusal struct {
	// Command is the user-facing command that was refused, e.g. "work advance --execute".
	Command string
	// Gate names the gate in stable, greppable form, e.g. "workspace_resolution".
	Gate string
	// Refused states what did not happen.
	Refused string
	// Tried lists each resolution step in attempt order with its observed result.
	Tried []string
	// DoThis lists exact commands the reader can run.
	DoThis []string
	// Prevents names the concrete failure this gate stops.
	Prevents string
}

func (r gateRefusal) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: %s refused at gate %s\n", r.Command, r.Gate)
	if refused := strings.TrimSpace(r.Refused); refused != "" {
		fmt.Fprintf(&b, "  refused: %s\n", refused)
	}
	if len(r.Tried) > 0 {
		b.WriteString("  tried in order:\n")
		for i, step := range r.Tried {
			fmt.Fprintf(&b, "    %d. %s\n", i+1, step)
		}
	}
	if len(r.DoThis) > 0 {
		b.WriteString("  do this:\n")
		for _, cmd := range r.DoThis {
			fmt.Fprintf(&b, "    %s\n", cmd)
		}
	}
	if prevents := strings.TrimSpace(r.Prevents); prevents != "" {
		fmt.Fprintf(&b, "  prevents: %s\n", prevents)
	}
	return b.String()
}

func (r gateRefusal) writeTo(w io.Writer) {
	fmt.Fprint(w, r.String())
}

// firstNonEmptyRefusalValue keeps a refusal's suggested command runnable when the
// value it would interpolate is itself missing: a placeholder the reader must
// replace is honest, an empty gap in a command line is not.
func firstNonEmptyRefusalValue(value, placeholder string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return placeholder
}
