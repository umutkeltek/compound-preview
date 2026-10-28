package policy

import (
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestResolveKindComposesFromRealOwners(t *testing.T) {
	// A candidate kind resolves from candidates.Spec — its REAL owner — not placement (Amendment A).
	mem, err := ResolveKind("memory")
	if err != nil {
		t.Fatal(err)
	}
	if mem.Mode != "governed_door" || mem.Source != "candidates.Spec" {
		t.Fatalf("candidate kind must resolve from candidates.Spec: %+v", mem)
	}
	if mem.Target != candidates.InboxDir(candidates.Memory) {
		t.Fatalf("target %q must match candidates.Spec inbox %q", mem.Target, candidates.InboxDir(candidates.Memory))
	}

	// The operational finding resolves to the direct-write finding home.
	f, err := ResolveKind(FindingKind)
	if err != nil {
		t.Fatal(err)
	}
	if f.Mode != "direct_write" || f.Source != "finding-home" || f.Target != "observed/findings" {
		t.Fatalf("finding kind resolve wrong: %+v", f)
	}
	if !strings.Contains(f.Consumer, "record") {
		t.Fatalf("Consumer must name the record verb for ordinary-agent discovery: %q", f.Consumer)
	}
	if f.Command == "" {
		t.Fatalf("operational-finding kind must expose a copyable write command")
	}
	if !strings.Contains(f.Command, "finding record") {
		t.Fatalf("Command must be the canonical finding record command: %q", f.Command)
	}

	if _, err := ResolveKind("nonsense-kind"); err == nil {
		t.Fatal("unknown kind must be an explicit error")
	}
}

// TestPlacementFindingCommandTemplateContract — falsifier 14: the placement-target JSON
// includes the exact operational-finding write-command template and names record/list/show.
func TestPlacementFindingCommandTemplateContract(t *testing.T) {
	res, err := ResolveKind(FindingKind)
	if err != nil {
		t.Fatal(err)
	}
	want := "compound finding record --topic <topic> --stdin --work-id <work_id> --produced-by agent:<id> --json"
	if res.Command != want {
		t.Fatalf("Command = %q, want %q", res.Command, want)
	}
	for _, v := range []string{"record", "list", "show"} {
		if !strings.Contains(res.Consumer, v) {
			t.Fatalf("Consumer must name %q verb: %q", v, res.Consumer)
		}
	}
}

func TestResolvePathMatchesGuard(t *testing.T) {
	home := t.TempDir()
	// An unowned path under the governed home resolves as blocked + redirect — the SAME verdict the
	// PreToolUse guard produces (ResolvePath runs the same PlacementContextFromHome).
	res, err := ResolvePath(home, filepath.Join(home, "private", "stray.md"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed || res.Reason != "unowned-under-governed-root" || res.Redirect == "" {
		t.Fatalf("unowned path must resolve as blocked+redirect: %+v", res)
	}
	ok, err := ResolvePath(home, filepath.Join(home, "reviews", "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Allowed {
		t.Fatalf("typed habitat must resolve allowed: %+v", ok)
	}
}

// TestEveryCandidateInboxIsPlacementAllowed is Amendment B — the cheap drift guard that replaces the
// whole projection machinery. Every candidate inbox (candidates.Spec) must resolve as ALLOWED against
// the synthesized governed home root; otherwise the guard would flag a LEGITIMATE registered inbox as
// unowned. It holds by construction (HabitatCatalog derives its allow-set from candidates.AllInboxDirs), so
// adding a candidate kind can never silently create an ungoverned-looking inbox — and this fails loud
// if that derivation ever breaks.
func TestEveryCandidateInboxIsPlacementAllowed(t *testing.T) {
	home := t.TempDir()
	for _, inbox := range candidates.AllInboxDirs() {
		p := filepath.Join(home, inbox, "sample.json")
		res, err := ResolvePath(home, p)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("candidate inbox %q must be placement-allowed (drift: enforcement not mirroring candidates.Spec): %+v", inbox, res)
		}
	}
}
