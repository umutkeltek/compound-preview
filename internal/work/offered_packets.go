package work

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const OfferedPacketSurfaceSchemaVersion = "compound.work.offered_packets.v0.1"

type OfferedPacketSurface struct {
	SchemaVersion string              `json:"schema_version"`
	AuthorityRef  string              `json:"authority_ref"`
	SourceDir     string              `json:"source_dir,omitempty"`
	Counts        OfferedPacketCounts `json:"counts"`
	Items         []OfferedPacketItem `json:"items"`
	Warnings      []string            `json:"warnings,omitempty"`
}

type OfferedPacketCounts struct {
	Total    int            `json:"total"`
	Listed   int            `json:"listed"`
	Skipped  int            `json:"skipped"`
	ByStatus map[string]int `json:"by_status"`
}

type OfferedPacketItem struct {
	Path             string   `json:"path"`
	SourceRef        string   `json:"source_ref"`
	Title            string   `json:"title"`
	Status           string   `json:"status,omitempty"`
	Owner            string   `json:"owner,omitempty"`
	ProofMedium      string   `json:"proof_medium,omitempty"`
	CanonicalSurface string   `json:"canonical_surface"`
	EvidenceRef      string   `json:"evidence_ref"`
	SourceDigest     string   `json:"source_digest"`
	Gates            []string `json:"gates,omitempty"`
}

func OfferedPackets(packetDir string) (OfferedPacketSurface, error) {
	packetDir = strings.TrimSpace(packetDir)
	surface := OfferedPacketSurface{
		SchemaVersion: OfferedPacketSurfaceSchemaVersion,
		AuthorityRef:  "compound://work/offered-packets",
		SourceDir:     packetDir,
		Counts: OfferedPacketCounts{
			ByStatus: map[string]int{},
		},
		Items: []OfferedPacketItem{},
	}
	if packetDir == "" {
		return surface, nil
	}
	entries, err := os.ReadDir(packetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return surface, nil
		}
		return surface, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		surface.Counts.Total++
		fullPath := filepath.Join(packetDir, entry.Name())
		packet, err := LoadPacket(fullPath)
		if err != nil {
			surface.Counts.Skipped++
			surface.Warnings = append(surface.Warnings, entry.Name()+": "+err.Error())
			continue
		}
		if strings.TrimSpace(packet.Title) == "" {
			surface.Counts.Skipped++
			surface.Warnings = append(surface.Warnings, entry.Name()+": missing packet title")
			continue
		}
		item := OfferedPacketItem{
			Path:             fullPath,
			SourceRef:        packet.SourceRef,
			Title:            packet.Title,
			Status:           packet.Status,
			Owner:            packet.Owner,
			ProofMedium:      packet.ProofMedium,
			CanonicalSurface: "packet:" + slug(packet.Title),
			EvidenceRef:      packet.EvidenceRef,
			SourceDigest:     packet.Digest,
			Gates:            packetGates(packet),
		}
		surface.Items = append(surface.Items, item)
		surface.Counts.Listed++
		surface.Counts.ByStatus[firstNonEmpty(packet.Status, "unspecified")]++
	}
	return surface, nil
}

func packetGates(packet packetSource) []string {
	var gates []string
	if packet.Status != "" {
		gates = append(gates, "status:"+packet.Status)
	}
	if packet.Owner != "" {
		gates = append(gates, "owner:"+packet.Owner)
	}
	if packet.ProofMedium != "" {
		gates = append(gates, "proof_medium:"+packet.ProofMedium)
	}
	return gates
}
