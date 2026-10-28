package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// RecallAliasSchemaVersion is the on-disk registry schema for E1 deterministic expansion.
const RecallAliasSchemaVersion = "compound.recall_aliases.v0.1"

// ExpansionScoreDiscount multiplies scores from expansion-only term hits so original
// query terms always dominate (boost-never-gate).
const ExpansionScoreDiscount = 0.5

// RecallAliasRegistry is the compound-owned alias map under
// COMPOUND_HOME/registries/recall-aliases.json. Absent/empty = expansion off (fail-open).
type RecallAliasRegistry struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   string             `json:"generated_at,omitempty"`
	Source        string             `json:"source,omitempty"`
	Entries       []RecallAliasEntry `json:"entries"`
}

// RecallAliasEntry maps a casual phrase/token to system vocabulary terms.
// Kind: "phrase" (substring match) or "token" (whole-token match).
type RecallAliasEntry struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Kind string   `json:"kind,omitempty"` // phrase|token; empty => phrase if multi-word, else token
}

// ExpandedQuery is the preprocessing result for QueryBreadth.
type ExpandedQuery struct {
	Original string   `json:"original"`
	Text     string   `json:"text"` // original + added terms (for diagnostics)
	Added    []string `json:"added,omitempty"`
	Active   bool     `json:"active"`
	Matched  []string `json:"matched,omitempty"` // from-phrases that fired
}

// RecallAliasesPath returns COMPOUND_HOME/registries/recall-aliases.json.
func RecallAliasesPath(homePath string) string {
	return filepath.Join(homePath, "registries", "recall-aliases.json")
}

// LoadRecallAliases loads the registry. Missing file → empty registry (fail-open, no error).
func LoadRecallAliases(homePath string) (RecallAliasRegistry, error) {
	path := RecallAliasesPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RecallAliasRegistry{SchemaVersion: RecallAliasSchemaVersion}, nil
		}
		return RecallAliasRegistry{}, err
	}
	var reg RecallAliasRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return RecallAliasRegistry{}, fmt.Errorf("parse recall aliases: %w", err)
	}
	if reg.SchemaVersion != "" && reg.SchemaVersion != RecallAliasSchemaVersion {
		return RecallAliasRegistry{}, fmt.Errorf("unsupported recall alias schema %q", reg.SchemaVersion)
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = RecallAliasSchemaVersion
	}
	return reg, nil
}

// SaveRecallAliases writes the registry atomically-ish (write then rename not required for v1).
func SaveRecallAliases(homePath string, reg RecallAliasRegistry) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = RecallAliasSchemaVersion
	}
	path := RecallAliasesPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// ExpandQuery applies deterministic aliases. Expansion widens the query; it never
// changes the visibility gate. Empty registry or no matches → Active=false and
// Text == original (byte-identical query path).
func ExpandQuery(text string, reg RecallAliasRegistry) ExpandedQuery {
	out := ExpandedQuery{Original: text, Text: text}
	if strings.TrimSpace(text) == "" || len(reg.Entries) == 0 {
		return out
	}
	lower := strings.ToLower(text)
	tokens := tokenizeQuery(text)
	tokenSet := map[string]bool{}
	for _, t := range tokens {
		tokenSet[t] = true
	}

	// Longest phrase first so multi-word entries win over single tokens.
	entries := append([]RecallAliasEntry(nil), reg.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].From) > len(entries[j].From)
	})

	added := map[string]bool{}
	var matched []string
	for _, e := range entries {
		from := strings.TrimSpace(strings.ToLower(e.From))
		if from == "" || len(e.To) == 0 {
			continue
		}
		kind := strings.TrimSpace(strings.ToLower(e.Kind))
		if kind == "" {
			if strings.Contains(from, " ") {
				kind = "phrase"
			} else {
				kind = "token"
			}
		}
		hit := false
		switch kind {
		case "phrase":
			hit = strings.Contains(lower, from)
		case "token":
			hit = tokenSet[from]
		default:
			hit = strings.Contains(lower, from)
		}
		if !hit {
			continue
		}
		matched = append(matched, e.From)
		for _, t := range e.To {
			t = strings.TrimSpace(strings.ToLower(t))
			if t == "" || tokenSet[t] {
				continue
			}
			// Don't re-add terms already in the original query: exact-token
			// presence only — a single token appearing as a substring of a
			// longer original word does not count as present.
			present := false
			for _, ot := range tokens {
				if ot == t {
					present = true
					break
				}
			}
			if present {
				continue
			}
			added[t] = true
		}
	}
	if len(added) == 0 {
		return out
	}
	addList := make([]string, 0, len(added))
	for t := range added {
		addList = append(addList, t)
	}
	sort.Strings(addList)
	out.Added = addList
	out.Matched = matched
	out.Active = true
	out.Text = strings.TrimSpace(text + " " + strings.Join(addList, " "))
	return out
}

// mergeHitsWithExpansionDiscount merges primary (original query) hits with
// expansion-term hits. Expansion scores are discounted; primary dominates ties.
func mergeHitsWithExpansionDiscount(primary, expansion []Hit, k int) []Hit {
	byID := map[string]float64{}
	for _, h := range primary {
		if h.ClaimID == "" {
			continue
		}
		if cur, ok := byID[h.ClaimID]; !ok || h.Score > cur {
			byID[h.ClaimID] = h.Score
		}
	}
	for _, h := range expansion {
		if h.ClaimID == "" {
			continue
		}
		disc := h.Score * ExpansionScoreDiscount
		if cur, ok := byID[h.ClaimID]; !ok || disc > cur {
			// Only raise if expansion discounted score beats current.
			// If primary already has the claim, keep the better of primary vs discounted.
			if !ok {
				byID[h.ClaimID] = disc
			} else if disc > cur {
				byID[h.ClaimID] = disc
			}
		}
	}
	out := make([]Hit, 0, len(byID))
	for id, score := range byID {
		out = append(out, Hit{ClaimID: id, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ClaimID < out[j].ClaimID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// RebuildRecallAliases mines entity aliases + capability-node name/trigger seeds
// into a deterministic registry. Does not import private glossary content (operator
// can merge private seeds via COMPOUND_HOME after rebuild).
func (s *Store) RebuildRecallAliases(now time.Time) (RecallAliasRegistry, error) {
	if now.IsZero() {
		now = time.Now()
	}
	reg := RecallAliasRegistry{
		SchemaVersion: RecallAliasSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Source:        "rebuild",
	}
	// Preserve manually-seeded entries (source=seed or kind annotated) when re-merging:
	// load existing and keep entries marked source seed in comments — we keep all
	// entries whose kind is "phrase" with multi-word from that aren't entity-derived.
	existing, _ := LoadRecallAliases(s.home)
	seeded := filterSeedEntries(existing.Entries)

	ents, err := s.Entities()
	if err != nil {
		return RecallAliasRegistry{}, err
	}
	seen := map[string]bool{} // from|toKey
	add := func(from string, to []string, kind string) {
		from = strings.TrimSpace(from)
		if from == "" || len(to) == 0 {
			return
		}
		cleanTo := make([]string, 0, len(to))
		for _, t := range to {
			t = strings.TrimSpace(t)
			if t == "" || strings.EqualFold(t, from) {
				continue
			}
			cleanTo = append(cleanTo, t)
		}
		if len(cleanTo) == 0 {
			return
		}
		key := strings.ToLower(from) + "|" + strings.Join(cleanTo, ",")
		if seen[key] {
			return
		}
		seen[key] = true
		reg.Entries = append(reg.Entries, RecallAliasEntry{From: from, To: cleanTo, Kind: kind})
	}

	for _, e := range ents {
		slugTerms := tokenizeQuery(strings.ReplaceAll(e.Slug, ":", " "))
		for _, a := range e.Aliases {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			kind := "token"
			if strings.Contains(a, " ") {
				kind = "phrase"
			}
			add(a, slugTerms, kind)
		}
		// Bare slug tail (project:compound → compound) already in slug terms; skip noise.
	}

	// Capability nodes: name → top distinct multi-char triggers (bounded).
	nodesPath := filepath.Join(s.home, "registries", "capability-nodes.jsonl")
	if data, err := os.ReadFile(nodesPath); err == nil {
		type capNode struct {
			Name     string   `json:"name"`
			Triggers []string `json:"triggers"`
			ID       string   `json:"id"`
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var n capNode
			if json.Unmarshal([]byte(line), &n) != nil {
				continue
			}
			name := strings.TrimSpace(n.Name)
			if name == "" {
				continue
			}
			// kebab/name form as phrase when multi-part
			from := strings.ReplaceAll(name, "-", " ")
			// Prefer a few informative triggers (≥4 chars), not stop-words.
			var to []string
			for _, t := range n.Triggers {
				t = strings.ToLower(strings.TrimSpace(t))
				if len(t) < 4 {
					continue
				}
				if t == "skill" || t == "recipe" || t == "agents" || t == "brain" || t == "recipes" {
					continue
				}
				to = append(to, t)
				if len(to) >= 6 {
					break
				}
			}
			if len(to) == 0 {
				// Fall back to id tail tokens
				to = tokenizeQuery(strings.ReplaceAll(n.ID, ":", " "))
			}
			kind := "token"
			if strings.Contains(from, " ") {
				kind = "phrase"
			}
			add(from, to, kind)
		}
	}

	// Re-attach seeded phrase entries (e.g. planted golden pairs) not overwritten.
	for _, e := range seeded {
		key := strings.ToLower(strings.TrimSpace(e.From)) + "|" + strings.Join(e.To, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		reg.Entries = append(reg.Entries, e)
	}

	sort.SliceStable(reg.Entries, func(i, j int) bool {
		if reg.Entries[i].From != reg.Entries[j].From {
			return reg.Entries[i].From < reg.Entries[j].From
		}
		return strings.Join(reg.Entries[i].To, ",") < strings.Join(reg.Entries[j].To, ",")
	})
	return reg, nil
}

func filterSeedEntries(entries []RecallAliasEntry) []RecallAliasEntry {
	var out []RecallAliasEntry
	for _, e := range entries {
		// Convention: kind "seed" marks operator/golden planted pairs that rebuild must keep.
		if strings.EqualFold(strings.TrimSpace(e.Kind), "seed") {
			out = append(out, e)
		}
	}
	return out
}

func tokenizeQuery(text string) []string {
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, strings.ToLower(b.String()))
		b.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}
