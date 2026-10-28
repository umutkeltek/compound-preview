// Command compound-mock-engine is the reference breadth-engine sidecar: a pure-Go process that
// speaks compound's breadth-engine protocol (newline-framed JSON on stdin/stdout, logs to stderr)
// using lexical matching. It is the conformance baseline the real turbovec sidecar must match, and
// it lets `compound memory query` + the adapter tests run end-to-end with zero Rust.
//
// Point compound at it with: COMPOUND_MEM_ENGINE_BIN=compound-mock-engine. The sidecar advertises its
// OWN embedder stamp from MOCK_ENGINE_EMBEDDER (NOT compound's COMPOUND_MEM_ENGINE_EMBEDDER, which is
// compound's record of what it INDEXED with) — so a mismatch between the two exercises the guard.
package main

import (
	"fmt"
	"os"

	"compound/internal/mem"
)

func main() {
	embedder := os.Getenv("MOCK_ENGINE_EMBEDDER")
	if embedder == "" {
		embedder = "reference:lexical:0:0"
	}
	if err := mem.RunReferenceEngine(os.Stdin, os.Stdout, embedder); err != nil {
		fmt.Fprintln(os.Stderr, "mock-engine:", err)
		os.Exit(1)
	}
}
