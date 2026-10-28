package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
)

// TestLargeToolOutputExternalizedAsObservedNotAmbient drives a PostToolUse hook with a ~340KB
// tool body and proves C3's contract at the real seam: the body is externalized to a
// content-addressed rebuildable blob + ONE observed-class candidate carrying a compact pointer;
// it never floods the summary, never touches the ledger, never enters ambient recall, and is
// retrievable only via ReadToolOutput.
func TestLargeToolOutputExternalizedAsObservedNotAmbient(t *testing.T) {
	dir := t.TempDir()
	if err := home.Init(dir, "test", fixedNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	deps := baseDeps()
	deps.HomePath = dir

	store, err := mem.Open(dir)
	if err != nil {
		t.Fatalf("mem open: %v", err)
	}
	before, err := store.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	body := strings.Repeat("SECRETLARGEOUTPUT", 20000) // ~340 KB
	cid := mem.ToolOutputCID([]byte(body))

	res, err := Run(Fixture{
		Hook:       "post-tool-use",
		Harness:    "claude_code",
		Cwd:        "/repo/x",
		Tool:       "Bash",
		WorkID:     "work_1177cb550af3",
		ToolOutput: body,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("post-tool-use run: %v", err)
	}

	// (a) The emitted summary is bounded and does NOT carry the body verbatim.
	if strings.Contains(res.Summary, body[:128]) {
		t.Fatal("summary leaked the tool-output body into context")
	}
	if len(res.Summary) > 400 {
		t.Fatalf("summary must stay bounded, got %d chars: %q", len(res.Summary), res.Summary)
	}
	if !strings.Contains(res.Summary, cid) {
		t.Fatalf("summary must carry the pointer cid, got %q", res.Summary)
	}

	// (b) Policy files a bare-cid pointer terminal (auto_file -> no_promote), so NO candidate file
	// is written: a dead row can never be corroborated and only fills the inbox. The body is still
	// externalized (checked below via OutputRef + private custody).
	candDir := filepath.Join(dir, "inbox", "memory-candidates")
	if entries, err := os.ReadDir(candDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				t.Fatalf("policy-terminal tool-output pointer wrote a candidate file: %s", e.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read candidate inbox: %v", err)
	}
	if _, err := store.ReadToolOutput(cid); err != nil {
		t.Fatalf("body must still be retrievable from private custody: %v", err)
	}

	if res.Event.OutputRef != mem.ToolOutputPrivateRef(cid) {
		t.Fatalf("output_ref = %q, want private opaque ref", res.Event.OutputRef)
	}
	if strings.Contains(res.Event.OutputRef, "/") || strings.HasPrefix(res.Event.OutputRef, "observed/tool-output/") {
		t.Fatalf("output_ref must be opaque and non-path, got %q", res.Event.OutputRef)
	}
	if !containsString(res.Event.SideEffects, mem.ToolOutputPrivateRef(cid)) {
		t.Fatalf("side effects = %v, want private opaque tool-output ref", res.Event.SideEffects)
	}
	if _, err := os.Stat(filepath.Join(dir, "observed", "tool-output", cid)); !os.IsNotExist(err) {
		t.Fatalf("new hook capture wrote observed blob: %v", err)
	}

	// (c) Ledger byte-identical: capture added zero ledger bytes.
	after, err := store.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed by tool-output capture: %s -> %s", before, after)
	}

	// (d) A fresh ContextPack (session-start) leaks the body/cid into NEITHER Included NOR ActiveMemory.
	ss, err := Run(Fixture{Hook: "session-start", Harness: "claude_code", Cwd: "/repo/x", Task: "audit", RepoKind: "product"}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-start run: %v", err)
	}
	var pack *envelope.ContextPack
	for _, e := range ss.Produced {
		if p, ok := e.(*envelope.ContextPack); ok {
			pack = p
		}
	}
	if pack == nil {
		t.Fatal("session-start produced no context pack")
	}
	for _, inc := range pack.Included {
		if strings.Contains(inc.Ref, cid) || strings.Contains(inc.Ref, body[:64]) {
			t.Fatalf("context pack Included leaked externalized tool output: %q", inc.Ref)
		}
	}
	for _, am := range pack.ActiveMemory {
		if strings.Contains(am, cid) || strings.Contains(am, body[:64]) {
			t.Fatalf("context pack ActiveMemory leaked externalized tool output: %q", am)
		}
	}

	// (e) ReadToolOutput returns the exact original bytes.
	got, err := store.ReadToolOutput(cid)
	if err != nil {
		t.Fatalf("read tool output: %v", err)
	}
	if string(got) != body {
		t.Fatalf("read round-trip mismatch: %d vs %d bytes", len(got), len(body))
	}

	// (f) Re-running the identical PostToolUse is idempotent: still no candidate file (policy-terminal pointer).
	if _, err := Run(Fixture{
		Hook: "post-tool-use", Harness: "claude_code", Cwd: "/repo/x", Tool: "Bash",
		WorkID: "work_1177cb550af3", ToolOutput: body,
	}, deps, fixedNow); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	entries2, _ := os.ReadDir(candDir)
	n := 0
	for _, e := range entries2 {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("re-run must be idempotent; want 0 candidates, got %d", n)
	}
}

// TestLargeToolOutputCandidateOnlyWhenPolicyWouldQueueIt drives the same live post-tool-use path
// twice. A normal tool pointer is filed terminal by adjudication policy, so it writes no
// candidate file. A pointer that policy would park (cheap-lane provenance) still creates exactly
// the candidate it created before: the write decision comes from the real policy, not a copy.
func TestLargeToolOutputCandidateOnlyWhenPolicyWouldQueueIt(t *testing.T) {
	dir := t.TempDir()
	if err := home.Init(dir, "test", fixedNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	deps := baseDeps()
	deps.HomePath = dir
	inbox := filepath.Join(dir, "inbox", "memory-candidates")
	names := func() []string {
		entries, err := os.ReadDir(inbox)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("read inbox: %v", err)
		}
		var out []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				out = append(out, strings.TrimSuffix(e.Name(), ".json"))
			}
		}
		return out
	}
	run := func(tool, body string) {
		t.Helper()
		if _, err := Run(Fixture{Hook: "post-tool-use", Harness: "claude_code", Cwd: "/repo/x", Tool: tool, ToolOutput: body}, deps, fixedNow); err != nil {
			t.Fatalf("post-tool-use %s: %v", tool, err)
		}
	}

	run("Bash", strings.Repeat("filed-terminal ", 20000))
	if got := names(); len(got) != 0 {
		t.Fatalf("policy no_promote pointer wrote candidate files %v, want none", got)
	}

	body := strings.Repeat("parked-by-policy ", 20000)
	run("cheap-lane", body)
	got := names()
	if len(got) != 1 {
		t.Fatalf("policy-parked pointer wrote %d candidate files, want 1", len(got))
	}
	var c mem.Candidate
	if err := candidates.ReadCandidate(dir, candidates.Memory, got[0], &c); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if c.State != "parked" || c.SourceTraceRef != "tool-output:"+mem.ToolOutputCID([]byte(body)) || c.PromotionBasis != "" {
		t.Fatalf("parked pointer candidate changed shape: state=%q trace=%q basis=%q", c.State, c.SourceTraceRef, c.PromotionBasis)
	}
}
