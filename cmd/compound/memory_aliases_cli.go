package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/mem"
)

func cmdMemoryAliases(homePath string, store *mem.Store, args []string) {
	jsonOut := false
	var pos []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintln(os.Stderr, "usage: compound memory aliases <show|rebuild|expand <query>> [--json]")
			os.Exit(2)
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound memory aliases <show|rebuild|expand <query>> [--json]")
		os.Exit(2)
	}
	switch pos[0] {
	case "show":
		reg, err := mem.LoadRecallAliases(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory aliases show:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(reg)
			return
		}
		fmt.Printf("# memory aliases (%s)\n", mem.RecallAliasesPath(homePath))
		fmt.Printf("- schema: %s\n", reg.SchemaVersion)
		fmt.Printf("- source: %s\n", reg.Source)
		fmt.Printf("- generated_at: %s\n", reg.GeneratedAt)
		fmt.Printf("- entries: %d\n", len(reg.Entries))
		for _, e := range reg.Entries {
			fmt.Printf("  - %q → %v (%s)\n", e.From, e.To, e.Kind)
		}
	case "rebuild":
		reg, err := store.RebuildRecallAliases(time.Now().UTC())
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory aliases rebuild:", err)
			os.Exit(1)
		}
		if err := mem.SaveRecallAliases(homePath, reg); err != nil {
			fmt.Fprintln(os.Stderr, "memory aliases rebuild:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(reg)
			return
		}
		fmt.Printf("# memory aliases rebuilt\n")
		fmt.Printf("- path: %s\n", mem.RecallAliasesPath(homePath))
		fmt.Printf("- entries: %d\n", len(reg.Entries))
		fmt.Printf("- generated_at: %s\n", reg.GeneratedAt)
	case "expand":
		if len(pos) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound memory aliases expand <query> [--json]")
			os.Exit(2)
		}
		query := strings.Join(pos[1:], " ")
		reg, err := mem.LoadRecallAliases(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory aliases expand:", err)
			os.Exit(1)
		}
		exp := mem.ExpandQuery(query, reg)
		if jsonOut {
			printJSON(exp)
			return
		}
		fmt.Printf("# memory aliases expand\n")
		fmt.Printf("- original: %s\n", exp.Original)
		fmt.Printf("- active: %t\n", exp.Active)
		fmt.Printf("- text: %s\n", exp.Text)
		if len(exp.Matched) > 0 {
			fmt.Printf("- matched: %v\n", exp.Matched)
		}
		if len(exp.Added) > 0 {
			fmt.Printf("- added: %v\n", exp.Added)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: compound memory aliases <show|rebuild|expand <query>> [--json]")
		os.Exit(2)
	}
}
