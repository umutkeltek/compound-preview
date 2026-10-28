package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/work"
)

func TestPacketsListJSONUsesWorkPacketProjection(t *testing.T) {
	packetDir := t.TempDir()
	packet := `# Console projection packet
Date: 2026-07-05
Status: ready
Owner: codex
Proof-medium: cli
Success: console consumes packet metadata through compound packets list.

## Objective
Expose packet metadata without console direct docs_private reads.
`
	if err := os.WriteFile(filepath.Join(packetDir, "console-projection.md"), []byte(packet), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdPackets([]string{"list", "--packet-dir", packetDir, "--json"})
	})
	var surface work.OfferedPacketSurface
	if err := json.Unmarshal([]byte(out), &surface); err != nil {
		t.Fatalf("parse packets list json: %v\n%s", err, out)
	}
	if surface.SchemaVersion != work.OfferedPacketSurfaceSchemaVersion || surface.AuthorityRef != "compound://work/offered-packets" {
		t.Fatalf("surface identity = %+v", surface)
	}
	if surface.Counts.Total != 1 || surface.Counts.Listed != 1 || len(surface.Items) != 1 {
		t.Fatalf("surface counts/items = %+v", surface)
	}
	item := surface.Items[0]
	if item.Title != "Console projection packet" || item.CanonicalSurface != "packet:console-projection-packet" || item.EvidenceRef == "" {
		t.Fatalf("packet item = %+v", item)
	}
}
