package work

import "strings"

// goProofSubcommands are the `go` subcommands that make a predicate a re-runnable proof. "go" is
// also an ordinary English word ("go to the dashboard"), so a bare "go" head qualifies ONLY when
// followed by one of these -- that is what keeps prose from masquerading as a command.
var goProofSubcommands = map[string]bool{
	"test": true, "build": true, "vet": true, "run": true, "generate": true,
}

// commandProofHeads are the executables whose presence as the FIRST token makes a predicate a
// re-runnable proof command. Deliberately conservative and unambiguous (no ordinary English words
// like "make"/"swift" that would false-positive on prose): the compound binary itself, the
// proof-check gate, the JS/TS runners this repo uses, and the common test runners. A bare `go`
// head is handled separately (goProofSubcommands) because "go" IS an English word.
var commandProofHeads = map[string]bool{
	"compound": true, "proof-check": true,
	"bun": true, "bunx": true, "npm": true, "npx": true, "pnpm": true, "yarn": true,
	"pytest": true, "cargo": true,
}

// CommandShapedPredicate classifies a WorkEnvelope.SuccessPredicate as a machine-checkable,
// re-runnable proof command -- returning the command verbatim and ok=true -- or reports ok=false.
//
// This is the conservative gate for standing-proof graduation (see the cmd-layer close exhaust
// offerStandingProofGraduation and mem.OfferStandingProofGraduation). SuccessPredicate is REQUIRED
// on every envelope and is usually free prose ("candidate reaches main", "a state delta exists"),
// so gating on non-empty alone would fire on every close. This classifies the WHOLE trimmed
// predicate as either a single command line or not -- it NEVER parses a command out of a sentence.
// A predicate qualifies only when:
//   - it is a single line (a multi-line value is a spec/prose block, never one command), and
//   - its first token is a path-form invocation (./script or /abs/path), a recognized executable
//     from commandProofHeads with at least one argument, or `go` followed by a real go subcommand.
//
// Everything else -- the overwhelming majority of real predicates -- returns ok=false and mints
// nothing. False negatives (a real command missed) only mean fewer graduation offers; the
// dangerous direction (prose minting a spurious offer) is what the allowlist + go-subcommand
// gating shut out. Adjudication, never this classifier, decides whether an offer becomes a
// standing proof, so being command-anchored (not perfectly runnable) is acceptable.
func CommandShapedPredicate(pred string) (command string, ok bool) {
	command = strings.TrimSpace(pred)
	if command == "" {
		return "", false
	}
	if strings.ContainsAny(command, "\n\r") {
		return "", false // multi-line value is a spec/prose block, not a single runnable command
	}
	fields := strings.Fields(command)
	head := fields[0]
	switch {
	case strings.HasPrefix(head, "./") || strings.HasPrefix(head, "/"):
		return command, true
	case head == "go":
		if len(fields) >= 2 && goProofSubcommands[fields[1]] {
			return command, true
		}
		return "", false
	case commandProofHeads[head]:
		// a bare executable name is not a runnable proof -- require at least one argument
		return command, len(fields) >= 2
	default:
		return "", false
	}
}
