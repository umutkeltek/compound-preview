package main

import (
	"fmt"
	"os"
	"strings"

	"compound/internal/work"
)

func cmdPackets(args []string) {
	if len(args) == 0 || hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Fprintln(os.Stderr, "usage: compound packets list [--packet-dir D] [--json]")
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	switch args[0] {
	case "list":
		cmdPacketsList(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: compound packets list [--packet-dir D] [--json]")
		os.Exit(2)
	}
}

func cmdPacketsList(args []string) {
	packetDir := defaultWorkbenchPacketDir()
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--packet-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: compound packets list [--packet-dir D] [--json]")
				os.Exit(2)
			}
			i++
			packetDir = strings.TrimSpace(args[i])
			if packetDir == "" {
				fmt.Fprintln(os.Stderr, "packets list: --packet-dir must be non-empty")
				os.Exit(2)
			}
		default:
			fmt.Fprintln(os.Stderr, "usage: compound packets list [--packet-dir D] [--json]")
			os.Exit(2)
		}
	}
	surface, err := work.OfferedPackets(packetDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "packets list:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(surface)
		return
	}
	fmt.Printf("# packets list\n\n")
	fmt.Printf("- source_dir: %s\n", surface.SourceDir)
	fmt.Printf("- total: %d\n", surface.Counts.Total)
	fmt.Printf("- listed: %d\n", surface.Counts.Listed)
	fmt.Printf("- skipped: %d\n", surface.Counts.Skipped)
	for _, item := range surface.Items {
		fmt.Printf("- packet: %s surface=%s evidence=%s status=%s\n", item.Title, item.CanonicalSurface, item.EvidenceRef, item.Status)
	}
	for _, warning := range surface.Warnings {
		fmt.Printf("- warning: %s\n", warning)
	}
}
