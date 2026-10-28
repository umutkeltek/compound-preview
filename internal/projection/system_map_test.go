package projection

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildSystemMapReturnsEmptyProjectionWithoutTopologyFile(t *testing.T) {
	report, err := BuildSystemMap(t.TempDir(), t.TempDir(), time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSystemMap: %v", err)
	}
	if report.SchemaVersion != SystemMapSchemaVersion || len(report.Nodes) != 0 || len(report.Edges) != 0 {
		t.Fatalf("report = %+v, want empty system-map projection", report)
	}
	if report.GeneratedAt != "2026-07-05T12:00:00Z" {
		t.Fatalf("generated_at = %q", report.GeneratedAt)
	}
}

func TestBuildSystemMapHydratesTopologyFromHome(t *testing.T) {
	home := t.TempDir()
	writeProjectionFile(t, filepath.Join(home, "config", "system-topology.json"), `{
  "nodes": [
    {"id":"capability","name":"Capability","lane":"meaning","status":"live","projection":"capability-authority"},
    {"id":"memory","name":"Memory","lane":"storage","status":"live","projection":"ledger-stats"},
    {"id":"capture","name":"Capture","lane":"capture","status":"planned","projection":"knowledge-sources"}
  ],
  "edges": [
    {"from":"capture","to":"memory","label":"feeds","status":"live"}
  ]
}`)
	writeProjectionFile(t, filepath.Join(home, "registries", "capability-nodes.jsonl"), "{}\n{}\n")
	writeProjectionFile(t, filepath.Join(home, "registries", "capability-edges.jsonl"), "{}\n")
	writeProjectionFile(t, filepath.Join(home, "registries", "knowledge-sources.jsonl"), `{"source_kind":"chat"}
{"kind":"paper"}
`)
	writeProjectionFile(t, filepath.Join(home, "ledger", "claims.jsonl"), "{}\n")
	writeProjectionFile(t, filepath.Join(home, "ledger", "entities.jsonl"), "{}\n{}\n")
	writeProjectionFile(t, filepath.Join(home, "ledger", "evidence.jsonl"), "{}\n{}\n{}\n")

	report, err := BuildSystemMap(home, t.TempDir(), time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSystemMap: %v", err)
	}
	if len(report.Nodes) != 3 || len(report.Edges) != 1 || len(report.SourceRefs) != 1 {
		t.Fatalf("report shape = %+v", report)
	}
	if report.Stats["meaning"].Live != 1 || report.Stats["capture"].NotLive != 1 {
		t.Fatalf("stats = %+v", report.Stats)
	}
	if report.Nodes[0].Volume != "2 nodes · 1 edges" {
		t.Fatalf("capability volume = %q", report.Nodes[0].Volume)
	}
	if report.Nodes[1].Volume != "1 claims · 2 entities · 3 evidence" {
		t.Fatalf("memory volume = %q", report.Nodes[1].Volume)
	}
	if report.Nodes[2].Volume != "2 sources" || len(report.Nodes[2].EvidenceRefs) != 2 {
		t.Fatalf("knowledge volume/refs = %+v", report.Nodes[2])
	}
}

// nodeByID finds a hydrated node in the projection by id.
func nodeByID(t *testing.T, report SystemMapProjection, id string) SystemMapNode {
	t.Helper()
	for _, n := range report.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not found in %+v", id, report.Nodes)
	return SystemMapNode{}
}

// TestBuildSystemMapHostReachabilityLiveVsDown is the depth test: a
// host-reachability node whose probe hits an open listener must render "live",
// while an identically-shaped node whose probe hits a closed port must render
// "divergent". If the projection stopped actually probing (e.g. echoed the
// fixture status), both would collapse to the same value and this fails.
func TestBuildSystemMapHostReachabilityLiveVsDown(t *testing.T) {
	// One listener stays open (reachable); the other is opened only to reserve a
	// port, then closed so the port is guaranteed refused (unreachable).
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	upAddr := up.Addr().String()

	down, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	downAddr := down.Addr().String()
	down.Close()

	home := t.TempDir()
	writeProjectionFile(t, filepath.Join(home, "runtime", "lan-hosts.yaml"), fmt.Sprintf(`runtime_binding:
  service_id: service:demo-hosts
  owner_entity: infra:demo
service_contract:
  service_id: service:demo-hosts
  owner_entity: infra:demo
runtime_probes:
  - id: up
    kind: tcp
    target: %s
    timeout_ms: 1000
  - id: down
    kind: tcp
    target: %s
    timeout_ms: 1000
`, upAddr, downAddr))

	writeProjectionFile(t, filepath.Join(home, "config", "system-topology.json"), `{
  "nodes": [
    {"id":"host-up","name":"Up host","lane":"infra","status":"planned","projection":"host-reachability","target":"runtime/lan-hosts.yaml#up"},
    {"id":"host-down","name":"Down host","lane":"infra","status":"planned","projection":"host-reachability","target":"runtime/lan-hosts.yaml#down"},
    {"id":"hosts-all","name":"All hosts","lane":"infra","status":"planned","projection":"host-reachability","target":"runtime/lan-hosts.yaml"}
  ],
  "edges": []
}`)

	report, err := BuildSystemMap(home, t.TempDir(), time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSystemMap: %v", err)
	}

	upNode := nodeByID(t, report, "host-up")
	downNode := nodeByID(t, report, "host-down")
	if upNode.Status == downNode.Status {
		t.Fatalf("live and down hosts collapsed to the same status %q — probe not exercised", upNode.Status)
	}
	if upNode.Status != "live" || upNode.Volume != "reachable" {
		t.Fatalf("up host = status %q volume %q, want live/reachable", upNode.Status, upNode.Volume)
	}
	if downNode.Status != "divergent" || downNode.Volume != "unreachable" {
		t.Fatalf("down host = status %q volume %q, want divergent/unreachable", downNode.Status, downNode.Volume)
	}

	all := nodeByID(t, report, "hosts-all")
	if all.Status != "divergent" || all.Volume != "1/2 reachable" {
		t.Fatalf("aggregate = status %q volume %q, want divergent/\"1/2 reachable\"", all.Status, all.Volume)
	}
	foundDown := false
	for _, ref := range all.EvidenceRefs {
		if ref == "unreachable:down" {
			foundDown = true
		}
	}
	if !foundDown {
		t.Fatalf("aggregate evidence = %v, want an unreachable:down ref", all.EvidenceRefs)
	}

	// Infra lane rolls up: one live host, two not-live (down host + aggregate).
	if report.Stats["infra"].Live != 1 || report.Stats["infra"].NotLive != 2 {
		t.Fatalf("infra stats = %+v, want live=1 notLive=2", report.Stats["infra"])
	}
}

// TestBuildSystemMapHostReachabilityDegrades proves the projection fails safe:
// a missing bundle, an unknown probe id, and an empty target each degrade to a
// non-live status with an explanatory volume — never a panic, never a false
// "live".
func TestBuildSystemMapHostReachabilityDegrades(t *testing.T) {
	home := t.TempDir()
	writeProjectionFile(t, filepath.Join(home, "config", "system-topology.json"), `{
  "nodes": [
    {"id":"missing-bundle","name":"Missing","lane":"infra","status":"planned","projection":"host-reachability","target":"runtime/does-not-exist.yaml#x"},
    {"id":"unknown-probe","name":"Unknown probe","lane":"infra","status":"planned","projection":"host-reachability","target":"runtime/lan-hosts.yaml#ghost"},
    {"id":"no-target","name":"No target","lane":"infra","status":"planned","projection":"host-reachability"}
  ],
  "edges": []
}`)
	// A bundle with no matching probe id so "unknown-probe" resolves the bundle
	// but not the probe.
	writeProjectionFile(t, filepath.Join(home, "runtime", "lan-hosts.yaml"), `runtime_probes:
  - id: real
    kind: tcp
    target: 127.0.0.1:1
    timeout_ms: 500
`)

	report, err := BuildSystemMap(home, t.TempDir(), time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSystemMap: %v", err)
	}

	missing := nodeByID(t, report, "missing-bundle")
	if missing.Status == "live" {
		t.Fatalf("missing bundle rendered live: %+v", missing)
	}
	if missing.Status != "divergent" || missing.Volume != "probe bundle unavailable" {
		t.Fatalf("missing bundle = status %q volume %q", missing.Status, missing.Volume)
	}

	unknown := nodeByID(t, report, "unknown-probe")
	if unknown.Status != "divergent" || unknown.Volume != "probe ghost not found" {
		t.Fatalf("unknown probe = status %q volume %q", unknown.Status, unknown.Volume)
	}

	noTarget := nodeByID(t, report, "no-target")
	if noTarget.Status != "divergent" || noTarget.Volume != "no probe bound" {
		t.Fatalf("no target = status %q volume %q", noTarget.Status, noTarget.Volume)
	}
}

func writeProjectionFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
