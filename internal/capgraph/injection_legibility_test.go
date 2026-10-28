package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPack writes a minimal valid ContextPack under homePath/context/packs.
func writeTestPack(t *testing.T, homePath, id string, included, excluded, memory, positions, offerCount, decisions int) string {
	t.Helper()
	dir := filepath.Join(homePath, "context", "packs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	packID := id
	if !strings.HasPrefix(packID, "ctx_") {
		packID = "ctx_" + packID
	}
	inc := make([]map[string]any, 0, included)
	for i := 0; i < included; i++ {
		src := "source"
		if i%3 == 0 {
			src = "private_overlay"
		}
		inc = append(inc, map[string]any{"ref": "ref://" + id + "/" + itoa(i), "source_class": src, "reason": "test", "confidence": 1, "status": "current"})
	}
	exc := make([]map[string]any, 0, excluded)
	for i := 0; i < excluded; i++ {
		reason := "accepted decision from Compound ledger omitted: no task relevance"
		if i%2 == 0 {
			reason = "governance-map context (ambient for control-plane) is out of scope in a product repo"
		}
		exc = append(exc, map[string]any{"ref": "decision://DEC-" + itoa(i), "reason": reason})
	}
	mem := make([]string, 0, memory)
	for i := 0; i < memory; i++ {
		mem = append(mem, "claim://"+id+"/m"+itoa(i))
	}
	pos := make([]map[string]any, 0, positions)
	for i := 0; i < positions; i++ {
		pos = append(pos, map[string]any{"ref": "position://" + itoa(i), "entity": "e", "contested": false})
	}
	offers := make([]map[string]any, 0, offerCount)
	for i := 0; i < offerCount; i++ {
		offers = append(offers, map[string]any{"ref": "capability://offer/" + itoa(i), "kind": "tool", "name": "offer" + itoa(i)})
	}
	dec := make([]string, 0, decisions)
	for i := 0; i < decisions; i++ {
		dec = append(dec, "decision://op-"+itoa(i))
	}
	pack := map[string]any{
		"schema_version":      "compound.context_pack.v0.1",
		"id":                  packID,
		"created_at":          "2026-08-08T00:00:00Z",
		"kind":                "context_pack",
		"privacy":             "private",
		"risk":                "tier0",
		"authority":           map[string]any{"source_class": "runtime", "status": "current"},
		"uncertainty":         map[string]any{"level": "unknown"},
		"producer":            map[string]any{"kind": "test", "name": "fixture"},
		"scope":               map[string]any{"surface": "/tmp/test"},
		"included":            inc,
		"excluded":            exc,
		"active_memory":       mem,
		"active_positions":    pos,
		"capability_offers":   offers,
		"active_decisions":    dec,
		"summary_md":          "summary",
		"task_contract":       map[string]any{"mode": "execute", "goal": "g"},
		"active_instructions": []string{},
		"active_hooks":        []string{},
		"required_checks":     []string{},
		"avoid":               []string{},
	}
	data, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(i int) string {
	return string(rune('0' + i))
}

func TestLatestInjectionLegibilityComposesFromLatestPack(t *testing.T) {
	home := t.TempDir()
	// Two packs; the second is newer so it wins.
	writeTestPack(t, home, "ctx_older", 5, 3, 1, 0, 0, 0)
	writeTestPack(t, home, "ctx_newer", 38, 82, 8, 5, 4, 2)

	// Injections dir with a rendered artifact for the byte count.
	injDir := filepath.Join(home, "context", "injections")
	if err := os.MkdirAll(injDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 5500)
	if err := os.WriteFile(filepath.Join(injDir, "hook_abc.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	leg, err := LatestInjectionLegibility(home)
	if err != nil {
		t.Fatalf("LatestInjectionLegibility: %v", err)
	}
	if !strings.HasSuffix(leg.PackID, "newer.json") {
		t.Fatalf("selected pack = %q, want the newer one", leg.PackID)
	}
	if leg.ServedRefs != 38 {
		t.Fatalf("served_refs = %d, want 38", leg.ServedRefs)
	}
	if leg.ServedMemory != 8 || leg.ServedPositions != 5 || leg.ServedOffers != 4 || leg.ServedDecisions != 2 {
		t.Fatalf("served breakdown = m%d p%d o%d d%d, want 8/5/4/2", leg.ServedMemory, leg.ServedPositions, leg.ServedOffers, leg.ServedDecisions)
	}
	if leg.ExcludedRefs != 82 {
		t.Fatalf("excluded_refs = %d, want 82", leg.ExcludedRefs)
	}
	if leg.ExcludedNoTask == 0 || leg.ExcludedAmbient == 0 {
		t.Fatalf("excluded breakdown no_task=%d ambient=%d, want both nonzero", leg.ExcludedNoTask, leg.ExcludedAmbient)
	}
	if leg.InjectionBytes != 5500 {
		t.Fatalf("injection_bytes = %d, want 5500", leg.InjectionBytes)
	}
	glance := leg.Glance()
	for _, want := range []string{"38 served", "8 memory", "5 positions", "4 offers", "82 excluded", "~6KB"} {
		if !strings.Contains(glance, want) {
			t.Fatalf("glance missing %q: %s", want, glance)
		}
	}
}

func TestLatestInjectionLegibilityNoPackIsEmptyNotError(t *testing.T) {
	leg, err := LatestInjectionLegibility(t.TempDir())
	if err != nil {
		t.Fatalf("empty home must not error: %v", err)
	}
	if leg.PackID != "" {
		t.Fatalf("empty home pack_id = %q, want empty", leg.PackID)
	}
	if g := leg.Glance(); g != "injection: no ContextPack observed yet" {
		t.Fatalf("empty glance = %q", g)
	}
}
