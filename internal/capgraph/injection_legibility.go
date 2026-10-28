package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/envelope"
)

// InjectionLegibilitySchemaVersion tags the glanceable per-turn injection meter.
// It is the operator-legibility half of the memory-injection observation work
// (packet-memory-injection-observation): the full report (BuildInjectionObservationReport)
// answers the experiment's discriminator question, while this meter answers the
// operator's glanceable question -- "this turn, how much compound context was
// served, how much was excluded, and how much did it cost?" -- from the LATEST
// ContextPack and its rendered injection artifact alone.
const InjectionLegibilitySchemaVersion = "compound.memory_injection_legibility.v0.1"

// InjectionLegibility is one compact snapshot of what a turn's ContextPack
// injected. Every number is derived from the pack or its rendered injection
// artifact on disk; nothing is guessed.
type InjectionLegibility struct {
	SchemaVersion string `json:"schema_version"`
	PackID        string `json:"pack_id"`
	GeneratedAt   string `json:"generated_at"`

	ServedRefs        int `json:"served_refs"`         // len(Included)
	ServedSource      int `json:"served_source"`       // Included with SourceClass "source"
	ServedMemory      int `json:"served_memory"`       // len(ActiveMemory)
	ServedPositions   int `json:"served_positions"`    // len(ActivePositions)
	ServedOffers      int `json:"served_offers"`       // len(CapabilityOffers)
	ServedDecisions   int `json:"served_decisions"`    // len(ActiveDecisions)
	ExcludedRefs      int `json:"excluded_refs"`       // len(Excluded)
	ExcludedNoTask    int `json:"excluded_no_task"`    // excluded refs whose reason says "no task relevance"
	ExcludedAmbient   int `json:"excluded_ambient"`    // excluded refs whose reason says "out of scope"/"ambient"
	InjectionBytes    int `json:"injection_bytes"`     // bytes of the latest rendered injection .md
	InjectionBudgetKB int `json:"injection_budget_kb"` // harness injection budget in KB, 0 = unknown
}

// SourceClasses the pack uses for Included refs (envelope.SourceClasses).
const (
	injectionSourceClassSource  = "source"
	injectionSourceClassPrivate = "private_overlay"
)

// LatestInjectionLegibility reads the newest ContextPack under
// homePath/context/packs and its rendered injection artifact, and reduces both to
// the glanceable meter. Read-only and best-effort: a home that never oriented
// returns a zero-valued snapshot with PackID "" (never an error) so a caller can
// omit the beat cleanly; an unreadable pack is likewise "no meter" rather than a
// hard failure -- this is a glanceable surface, not a gate.
func LatestInjectionLegibility(homePath string) (InjectionLegibility, error) {
	out := InjectionLegibility{SchemaVersion: InjectionLegibilitySchemaVersion}
	packPath := latestContextPackPath(homePath)
	if packPath == "" {
		return out, nil
	}
	data, err := os.ReadFile(packPath)
	if err != nil {
		return out, nil
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(data, &pack); err != nil {
		return out, nil
	}
	if pack.Validate() != nil {
		return out, nil
	}
	out.PackID = filepath.Base(packPath)
	out.GeneratedAt = pack.CreatedAt
	out.ServedRefs = len(pack.Included)
	out.ServedMemory = len(pack.ActiveMemory)
	out.ServedPositions = len(pack.ActivePositions)
	out.ServedOffers = len(pack.CapabilityOffers)
	out.ServedDecisions = len(pack.ActiveDecisions)
	out.ExcludedRefs = len(pack.Excluded)
	for _, inc := range pack.Included {
		switch inc.SourceClass {
		case injectionSourceClassSource, injectionSourceClassPrivate:
			out.ServedSource++
		}
	}
	for _, ex := range pack.Excluded {
		reason := ex.Reason
		switch {
		case strings.Contains(reason, "no task relevance"):
			out.ExcludedNoTask++
		case strings.Contains(reason, "out of scope"), strings.Contains(reason, "ambient"):
			out.ExcludedAmbient++
		}
	}
	out.InjectionBytes = latestInjectionBytes(homePath)
	return out, nil
}

// latestContextPackPath returns the newest pack file under homePath/context/packs
// (by mtime), or "" when none exists.
func latestContextPackPath(homePath string) string {
	dir := filepath.Join(homePath, "context", "packs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "ctx_") && strings.HasSuffix(name, ".json") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool {
		ai, aerr := os.Stat(files[i])
		bi, berr := os.Stat(files[j])
		if aerr != nil || berr != nil {
			return files[i] < files[j]
		}
		return ai.ModTime().After(bi.ModTime())
	})
	return files[0]
}

// latestInjectionBytes returns the byte size of the newest rendered injection
// artifact under homePath/context/injections (by mtime), or 0 when none exists.
// This is the actual served injection body the model received -- the glanceable
// "how much did this turn cost" number.
func latestInjectionBytes(homePath string) int {
	dir := filepath.Join(homePath, "context", "injections")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "hook_") && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		return 0
	}
	sort.Slice(files, func(i, j int) bool {
		ai, aerr := os.Stat(files[i])
		bi, berr := os.Stat(files[j])
		if aerr != nil || berr != nil {
			return files[i] < files[j]
		}
		return ai.ModTime().After(bi.ModTime())
	})
	info, err := os.Stat(files[0])
	if err != nil {
		return 0
	}
	return int(info.Size())
}

// Glance is the one-line human render -- the operator-legibility surface. Example:
//
//	injection: 38 served (8 memory / 5 positions / 4 offers), 82 excluded (6 no-task), ~5.4KB
func (l InjectionLegibility) Glance() string {
	if l.PackID == "" {
		return "injection: no ContextPack observed yet"
	}
	var parts []string
	if l.ServedMemory > 0 {
		parts = append(parts, fmt.Sprintf("%d memory", l.ServedMemory))
	}
	if l.ServedPositions > 0 {
		parts = append(parts, fmt.Sprintf("%d positions", l.ServedPositions))
	}
	if l.ServedOffers > 0 {
		parts = append(parts, fmt.Sprintf("%d offers", l.ServedOffers))
	}
	served := fmt.Sprintf("%d served", l.ServedRefs)
	if len(parts) > 0 {
		served += " (" + strings.Join(parts, " / ") + ")"
	}
	excluded := ""
	if l.ExcludedRefs > 0 {
		excluded = fmt.Sprintf(", %d excluded", l.ExcludedRefs)
		if l.ExcludedNoTask > 0 {
			excluded += fmt.Sprintf(" (%d no-task)", l.ExcludedNoTask)
		}
	}
	bytes := "0KB"
	if l.InjectionBytes > 0 {
		bytes = fmt.Sprintf("~%dKB", (l.InjectionBytes+1023)/1024)
	}
	return fmt.Sprintf("injection: %s%s, %s", served, excluded, bytes)
}
