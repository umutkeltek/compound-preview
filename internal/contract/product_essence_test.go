package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/product"
)

// essenceWorkspace builds a product checkout whose project contract names
// widget-alpha, and a home whose ledger records the widget_alpha product (and a
// sibling), optionally with a declared essence on each.
func essenceWorkspace(t *testing.T, withEssence bool) (home, workspace string) {
	t.Helper()
	home = t.TempDir()
	root := filepath.Join(t.TempDir(), "widget-alpha")
	workspace = filepath.Join(root, "Sources")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProductProjectContract(t, root, "widget-alpha")
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"widget_alpha", "widget_beta"} {
		b := product.Bundle{
			SchemaVersion: product.SchemaVersion,
			Product: product.Product{
				ID: "product:" + id, Name: id, OwnerEntity: "project:" + id,
				Surfaces:    []product.Surface{{ID: "site", Kind: "web", Ref: "https://" + id + ".example.invalid"}},
				Channels:    []string{"web"},
				Positioning: id + " positioning", PrivacyPosture: "local_first",
			},
			EvidenceRefs: []string{"fixture:essence"},
		}
		if withEssence {
			b.Product.Essence = product.Essence{
				Audience:    strings.ToUpper(id) + "-AUDIENCE field crews",
				Problem:     strings.ToUpper(id) + "-PROBLEM paper timesheets",
				Value:       strings.ToUpper(id) + "-VALUE never served in the summary",
				Goal:        product.Goal{IncomeTargetUSDMonth: 2000, Horizon: "2027-01"},
				Constraints: []string{"one-time purchase"},
				AntiGoals:   []string{strings.ToUpper(id) + "-ANTIGOAL never served"},
				Stage:       "live",
			}
		}
		if _, err := product.Apply(home, store, b, id+".yaml", fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	return home, workspace
}

func essenceIncluded(pack envelope.ContextPack) (envelope.IncludedRef, bool) {
	for _, ref := range pack.Included {
		if strings.HasPrefix(ref.Ref, productEssenceRefPrefix) {
			return ref, true
		}
	}
	return envelope.IncludedRef{}, false
}

// TestProductRepoContextPackCarriesTheProductEssence is the orient depth test:
// a product checkout's ContextPack names its own product's essence (audience,
// problem, goal, stage, constraints) and nothing of a sibling product's.
func TestProductRepoContextPackCarriesTheProductEssence(t *testing.T) {
	home, workspace := essenceWorkspace(t, true)
	pack, err := PackWithCapabilities(Input{
		Harness: "codex", Cwd: workspace, Task: "ship the onboarding fix",
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := essenceIncluded(pack)
	if !ok {
		t.Fatalf("product ContextPack has no essence ref; included=%+v", pack.Included)
	}
	if ref.Ref != productEssenceRefPrefix+"product:widget_alpha" || ref.SourceClass != "runtime" || ref.Status != "current" {
		t.Errorf("essence ref = %+v", ref)
	}
	for _, want := range []string{
		"Product essence (product:widget_alpha):",
		"audience: WIDGET_ALPHA-AUDIENCE field crews",
		"problem: WIDGET_ALPHA-PROBLEM paper timesheets",
		"goal: USD 2000/month by 2027-01",
		"stage: live",
		"constraints: one-time purchase",
	} {
		if !strings.Contains(pack.SummaryMD, want) {
			t.Errorf("SummaryMD missing %q:\n%s", want, pack.SummaryMD)
		}
	}
	for _, forbidden := range []string{"WIDGET_BETA-AUDIENCE", "WIDGET_ALPHA-VALUE", "WIDGET_ALPHA-ANTIGOAL"} {
		if strings.Contains(essenceBlock(pack.SummaryMD), forbidden) {
			t.Errorf("essence block carries %q beyond the short summary:\n%s", forbidden, pack.SummaryMD)
		}
	}
	if strings.Contains(pack.SummaryMD, "WIDGET_BETA") {
		t.Errorf("sibling product essence leaked into the pack:\n%s", pack.SummaryMD)
	}
	if err := pack.Validate(); err != nil {
		t.Errorf("pack with essence block fails validation: %v", err)
	}

	// The same essence appears through MaterializeRuntime, the orient path.
	runtime, err := MaterializeRuntime(Input{
		Harness: "codex", Cwd: workspace, Task: "ship the onboarding fix",
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := essenceIncluded(runtime.ContextPack); !ok {
		t.Errorf("MaterializeRuntime ContextPack lacks the essence ref")
	}
}

// TestContextPackWithoutEssenceIsUnchanged: no essence recorded, or a repo that
// is not a product checkout, yields no essence block at all.
func TestContextPackWithoutEssenceIsUnchanged(t *testing.T) {
	home, workspace := essenceWorkspace(t, false)
	pack, err := PackWithCapabilities(Input{
		Harness: "codex", Cwd: workspace, Task: "ship the onboarding fix",
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := essenceIncluded(pack); ok {
		t.Errorf("pack without essence carries %+v", ref)
	}
	if strings.Contains(pack.SummaryMD, "Product essence") {
		t.Errorf("pack without essence mentions one:\n%s", pack.SummaryMD)
	}

	home, workspace = essenceWorkspace(t, true)
	pack, err = PackWithCapabilities(Input{
		Harness: "codex", Cwd: workspace, Task: "ship the onboarding fix",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := essenceIncluded(pack); ok || strings.Contains(pack.SummaryMD, "Product essence") {
		t.Errorf("non-product repo kind served a product essence:\n%s", pack.SummaryMD)
	}
}

func essenceBlock(summary string) string {
	i := strings.Index(summary, "Product essence (")
	if i < 0 {
		return ""
	}
	rest := summary[i:]
	if j := strings.Index(rest, ". "); j >= 0 {
		return rest[:j]
	}
	return rest
}
