package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/projection"
)

func TestSystemMapJSONCommand(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	writeCmdTestFile(t, filepath.Join(homePath, "config", "system-topology.json"), `{
  "nodes": [
    {"id":"runtime","name":"Runtime","lane":"meaning","status":"live","projection":"static"}
  ],
  "edges": []
}`)

	out := captureStdout(t, func() {
		cmdSystem([]string{"map", "--json"})
	})
	var report projection.SystemMapProjection
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse system map json: %v\n%s", err, out)
	}
	if report.SchemaVersion != projection.SystemMapSchemaVersion || len(report.Nodes) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Nodes[0].ID != "runtime" || report.Stats["meaning"].Live != 1 {
		t.Fatalf("node/stats = %+v %+v", report.Nodes, report.Stats)
	}
}

func TestSystemStatsJSONCommand(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "claims.jsonl"), "{}\n{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "entities.jsonl"), "{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "evidence.jsonl"), "{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl"), "{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "no-promote-decisions.jsonl"), "{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "knowledge-imports.jsonl"), "{}\n")
	writeCmdTestFile(t, filepath.Join(homePath, "registries", "knowledge-sources.jsonl"), `{"source_kind":"chatgpt"}`+"\n"+`{"source_kind":"arxiv"}`+"\n")
	if err := os.MkdirAll(filepath.Join(homePath, "inbox", "external-repo-intake", "repo-one"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdSystem([]string{"stats", "--json"}) })
	var report projection.SystemStatsProjection
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse system stats json: %v\n%s", err, out)
	}
	if report.SchemaVersion != projection.SystemStatsSchemaVersion || report.Claims != 2 || report.KnowledgeSources != 2 || report.IntakeRepos != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.KnowledgeSourceKinds) != 2 {
		t.Fatalf("source kinds = %+v", report.KnowledgeSourceKinds)
	}
}

func TestSystemMemoryGraphAndLineageJSONCommands(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "claims.jsonl"),
		`{"id":"claim_a","entity":"product:demo","predicate":"has_signal","object":"downloads rose","evidence_refs":["ev_a"]}`+"\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "edges.jsonl"), "")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "evidence.jsonl"),
		`{"id":"ev_a","source_kind":"observation","source_ref":"obs_a","quote_text":"downloads rose"}`+"\n")
	writeCmdTestFile(t, filepath.Join(homePath, "ledger", "adjudications.jsonl"), "")

	graphOut := captureStdout(t, func() { cmdSystem([]string{"memory-graph", "product:demo", "--json"}) })
	var graph projection.MemoryEgoGraphProjection
	if err := json.Unmarshal([]byte(graphOut), &graph); err != nil {
		t.Fatalf("parse memory graph json: %v\n%s", err, graphOut)
	}
	if graph.SchemaVersion != projection.MemoryEgoGraphSchemaVersion || !graph.Found || len(graph.Nodes) != 1 {
		t.Fatalf("graph = %+v", graph)
	}

	lineageOut := captureStdout(t, func() { cmdSystem([]string{"lineage", "ev_a", "--json"}) })
	var lineage projection.LineageProjection
	if err := json.Unmarshal([]byte(lineageOut), &lineage); err != nil {
		t.Fatalf("parse lineage json: %v\n%s", err, lineageOut)
	}
	if lineage.SchemaVersion != projection.LineageSchemaVersion || !lineage.Found || len(lineage.Steps) < 2 {
		t.Fatalf("lineage = %+v", lineage)
	}
}

func writeCmdTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
