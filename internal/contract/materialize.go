// Package contract — body materializer (slice 1).
//
// The body materializer turns a ContextPack from a librarian's catalog card (refs +
// ranks + cards) into the librarian's delivery: task-ranked full content packed
// greedily by rank under a token budget, with per-item provenance and an explicit
// omitted ledger. The design obeys one hard dependency-inversion LAW: this file
// never imports internal/corpus. Body sources are behind a small resolver seam:
//
//	type BodyResolver interface { Resolve(task string) ([]BodyCandidate, error) }
//
// cmd/compound owns the resolvers (a corpus-backed wrapper + a source-ref file
// resolver); contract owns the rank-greedy packer and the exclusion laws. Synthetic
// resolvers in tests pin every falsifier (F1–F6) without crossing the seam.
//
// LAWS (load-bearing — pinned by materialize_test.go):
//
//   - Excluded is law: a ref in the pack's Excluded list NEVER materializes a body.
//   - Private-overlay IncludedRefs (SourceClass == "private_overlay") NEVER
//     materialize a body, regardless of budget — the contract.go:1262 law stands
//     verbatim ("refs only, no private body injection").
//   - Whole-item-or-omit, strict greedy-by-rank: each item is independently admitted
//     if it fits the REMAINING budget; oversized items land in omitted with reason
//     budget_exhausted and DO NOT block lower-ranked smaller items from admission
//     (slice 1 design decision #7).
//   - No silent caps: every candidate not packed lands in omitted with ref, rank,
//     estimated_tokens, reason.
//   - Determinism: same candidates + pack + budget → byte-identical section. No
//     clock, no rand. Rank desc is the primary key; ref string asc is the tiebreak.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"compound/internal/envelope"
)

// BodyCandidate is one task-ranked body proposed for packing.
//
//   - Ref is the candidate's identity (corpus item id, source ref, etc.). It is the
//     key the materializer matches against pack.Excluded and pack.Included.
//   - Source labels the resolver that produced it (e.g. "corpus:personal-corpus",
//     "source-ref:AGENTS.md"). Display-only provenance.
//   - Rank is the task-conditioned score from the resolver (corpus similarity,
//     source-ref pack order proxy). Higher = admitted first. Ties broken by Ref asc.
//   - Status is the resolver's per-candidate state ("ok", "suppressed_private",
//     "stale_source", "stale_index", "source_unavailable", "unresolvable"). Only
//     Status=="ok" contributes a body; everything else lands in omitted with a
//     reason mapped from the status.
//   - Text is the resolved body. Empty unless Status=="ok".
type BodyCandidate struct {
	Ref    string
	Source string
	Text   string
	Rank   float64
	Status string
}

// BodyResolver returns the candidates a body source can propose for a task. The
// materializer (not the resolver) is responsible for budget-aware packing, so
// Resolve should NOT pre-truncate by budget — return every candidate; the
// materializer will rank, gate, and pack. Implementations live in cmd/compound.
type BodyResolver interface {
	Resolve(task string) ([]BodyCandidate, error)
}

// Candidate status values emitted by resolvers. The corpus lane surfaces these from
// ResolveChunkText (internal/corpus/chunk.go); the file resolver surfaces
// source_unavailable / unresolvable. The materializer treats anything that is not
// BodyStatusOK as an omission (mapped to a reason).
const (
	BodyStatusOK                = "ok"
	BodyStatusSuppressedPrivate = "suppressed_private"
	BodyStatusStaleSource       = "stale_source"
	BodyStatusStaleIndex        = "stale_index"
	BodyStatusSourceUnavailable = "source_unavailable"
	BodyStatusUnresolvable      = "unresolvable"
)

// Omit reason values (the v1 open enum on BodyPackOmission.Reason).
const (
	OmitBudgetExhausted   = "budget_exhausted"
	OmitSuppressedPrivate = "suppressed_private"
	OmitUnresolvable      = "unresolvable"
	OmitExcluded          = "excluded"
)

// EstimatedTokens returns the deterministic byte/4 (ceil) token estimate used for
// budget accounting. The choice is deliberate: no external tokenizer dependency,
// stable across runs and machines, and the field name on the wire is
// `estimated_tokens`. The repo has NO existing token estimator (only a word-count
// proxy in internal/panel/caller.go:107, not reusable here).
func EstimatedTokens(s string) int {
	return (len(s) + 3) / 4
}

// MaterializeBodyPack packs task-ranked candidates under a token budget, applying
// the pack's exclusion laws (Excluded list + private_overlay SourceClass), and
// returns the additive BodyPackSection to assign on pack.BodyPack.
//
// Returns nil when budgetTokens <= 0 — the feature is OFF and the pack stays
// byte-identical to today (no body_pack key on the wire; F4).
//
// Determinism: same candidates + pack + budget → byte-identical section. Rank desc
// is the primary key; ref string asc is the tiebreak (F6).
func MaterializeBodyPack(candidates []BodyCandidate, pack envelope.ContextPack, budgetTokens int) *envelope.BodyPackSection {
	if budgetTokens <= 0 {
		return nil
	}
	excluded := make(map[string]struct{}, len(pack.Excluded))
	for _, e := range pack.Excluded {
		excluded[e.Ref] = struct{}{}
	}
	// The private-overlay law (contract.go:1262) — "refs only, no private body
	// injection". A SourceClass == "private_overlay" IncludedRef NEVER materializes
	// a body, regardless of budget. Defense-in-depth: the file resolver filters
	// these at source too, but the materializer is the load-bearing gate.
	privateOverlay := make(map[string]struct{})
	for _, inc := range pack.Included {
		if inc.SourceClass == "private_overlay" {
			privateOverlay[inc.Ref] = struct{}{}
		}
	}

	// Strict rank-greedy: primary rank desc, secondary ref asc, stable. Stable sort
	// is a no-op here (the comparator is total over ref+rank) but kept as a
	// belt-and-suspenders against a future non-total comparator.
	ordered := append([]BodyCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Rank != ordered[j].Rank {
			return ordered[i].Rank > ordered[j].Rank
		}
		return ordered[i].Ref < ordered[j].Ref
	})

	section := &envelope.BodyPackSection{
		BudgetTokens: budgetTokens,
		Bodies:       []envelope.BodyPackItem{},
		Omitted:      []envelope.BodyPackOmission{},
	}
	used := 0
	for _, c := range ordered {
		// Excluded is law.
		if _, ok := excluded[c.Ref]; ok {
			section.Omitted = append(section.Omitted, envelope.BodyPackOmission{
				Ref:             c.Ref,
				Rank:            c.Rank,
				EstimatedTokens: 0,
				Reason:          OmitExcluded,
			})
			continue
		}
		// Private-overlay is law.
		if _, ok := privateOverlay[c.Ref]; ok {
			section.Omitted = append(section.Omitted, envelope.BodyPackOmission{
				Ref:             c.Ref,
				Rank:            c.Rank,
				EstimatedTokens: 0,
				Reason:          OmitExcluded,
			})
			continue
		}
		// Suppressed/private or otherwise unresolvable — no text, omit with mapped reason.
		if c.Status != BodyStatusOK {
			section.Omitted = append(section.Omitted, envelope.BodyPackOmission{
				Ref:             c.Ref,
				Rank:            c.Rank,
				EstimatedTokens: 0,
				Reason:          omitReasonForStatus(c.Status),
			})
			continue
		}
		// Whole-item-or-omit, strict greedy-by-rank: admit if it fits the REMAINING
		// budget; an oversized item is omitted with budget_exhausted and admission
		// continues to the next-ranked item (slice 1 design decision #7).
		n := EstimatedTokens(c.Text)
		if used+n > budgetTokens {
			section.Omitted = append(section.Omitted, envelope.BodyPackOmission{
				Ref:             c.Ref,
				Rank:            c.Rank,
				EstimatedTokens: n,
				Reason:          OmitBudgetExhausted,
			})
			continue
		}
		section.Bodies = append(section.Bodies, envelope.BodyPackItem{
			Ref:             c.Ref,
			Source:          c.Source,
			Rank:            c.Rank,
			EstimatedTokens: n,
			SHA256:          sha256Hex(c.Text),
			Status:          c.Status,
			Text:            c.Text,
		})
		used += n
	}
	section.EstimatedUsedTokens = used
	return section
}

// omitReasonForStatus maps a non-OK candidate status to an omit reason. Anything
// not specifically private-suppressed lands as unresolvable (the floor for
// unreadable/changed/skewed sources).
func omitReasonForStatus(status string) string {
	if status == BodyStatusSuppressedPrivate {
		return OmitSuppressedPrivate
	}
	return OmitUnresolvable
}

// sha256Hex is the full lowercase hex sha256 of a body's text, carried on
// BodyPackItem.SHA256 for provenance (so a consumer can verify text integrity
// without re-reading the source).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ApplyBodyPack assigns the materialized section to pack.BodyPack and returns the
// mutated pack. A nil section (feature off) leaves the pack untouched. Convenience
// for the CLI path; the materializer's pure form returns the section alone so
// tests can assert on it without an envelope round-trip.
func ApplyBodyPack(pack envelope.ContextPack, section *envelope.BodyPackSection) envelope.ContextPack {
	if section == nil {
		return pack
	}
	pack.BodyPack = section
	return pack
}
