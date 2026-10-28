// orient_body_resolver.go owns the production BodyResolver implementations wired
// into `compound orient --body-budget <n>`. There are two body sources in slice 1
// (per the packet's settled design):
//
//  1. corpus hits: per configured corpus lane, top hits by score, text re-derived
//     per query (the index never stores chunk text). Suppressed-private hits land
//     in omitted with reason suppressed_private — there is no override flag.
//  2. included source refs that resolve to repo files (AGENTS.md, constitution://…,
//     control-plane://…) — ranked BELOW all corpus hits; ordered by pack Included
//     order; ties broken by ref string.
//
// The materializer (internal/contract.MaterializeBodyPack) owns the budget-aware
// packing + the exclusion laws; this resolver only PROPOSES candidates. Private-
// overlay IncludedRefs (SourceClass == "private_overlay") never reach a candidate
// here — the materializer-side law (contract.go:1262) is the load-bearing gate, but
// the resolver filters at source too.
//
// Optional body-source degradation (resolver-side, generic refs only): a
// malformed or unreadable memory-engine config contributes one candidate ref
// `corpus://configuration-unavailable`; a per-lane embedder/query failure
// contributes one opaque, deterministic ref derived from a hash of the lane id
// (never the raw endpoint, query, or lane body). Both surface as
// BodyStatusUnresolvable candidates; the materializer lands them in
// body_pack.omitted with reason unresolvable. The materializer/CLI consumer
// records degradation through OrientConsumerEvidence.DegradationObserved —
// this file owns the ref shapes so the evaluator package stays free of runtime
// resolver knowledge.
//
// This file is the ONLY place internal/corpus is imported by the body-materializer
// surface (the dependency-inversion LAW: internal/contract stays corpus-free).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"compound/internal/contract"
	"compound/internal/corpus"
	"compound/internal/envelope"
)

// corpusQueryTopK is the per-lane hit cap the body resolver asks the corpus for.
// The materializer (not the resolver) is responsible for budget-aware selection,
// so the resolver returns up to this many hits per lane and lets the packer choose.
const corpusQueryTopK = 10

// orientBodyConfigUnavailableRef is the single generic degradation candidate a
// malformed/unreadable memory-engine config contributes to the orient body
// resolver. It carries no path, parse error, or config byte — only that the
// optional configured body source was unavailable. The CLI surfaces this in
// OrientConsumerEvidence.DegradationObserved; the retrievalparity evaluator
// package has no knowledge of this string.
const orientBodyConfigUnavailableRef = "corpus://configuration-unavailable"

// orientBodyLaneUnavailableRef returns one opaque, deterministic per-lane
// degradation ref derived from a hash of the lane id (never the raw endpoint,
// query, or lane body). The 12-hex suffix keeps the wire footprint bounded and
// stable across runs.
func orientBodyLaneUnavailableRef(laneID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(laneID)))
	return "corpus://lane-unavailable/" + hex.EncodeToString(sum[:])[:12]
}

// orientBodyResolver is the production body resolver for `compound orient`: corpus
// hits (per configured lane) plus source-ref file bodies (per pack.Included order).
//
// Degradation is recorded as a per-resolver field, not a return value, because
// the underlying contract.BodyResolver interface is fixed (returns candidates +
// error). After Resolve returns, callers that need the flag read
// `resolver.Degraded()`. The flag is false until Resolve is called.
type orientBodyResolver struct {
	homePath string
	lanes    []corpus.LaneConfig
	pack     envelope.ContextPack
	cwd      string
	degraded bool
}

// Degraded reports whether the optional configured body source emitted any
// degradation candidate during the most recent Resolve call. False before any
// Resolve call. The flag is a snapshot; it does not change after Resolve.
func (r orientBodyResolver) Degraded() bool { return r.degraded }

// newOrientBodyResolver constructs the production resolver from a pack + the memory
// engine config's corpus lanes. The pack's Included ordering drives source-ref rank;
// its Excluded list is honored by the materializer, not here.
//
// A malformed or unreadable memory-engine config degrades the resolver to
// source-refs-only: the caller receives the config-unavailable ref as one
// candidate, the rest of the resolver proceeds normally. A missing config
// (ENOENT) is the normal no-lanes case and is not degradation.
//
// Returns a pointer so the resolver's internal degraded flag (set by the
// constructor and during corpus resolution) is shared with the caller.
func newOrientBodyResolver(homePath string, pack envelope.ContextPack, cwd string) (*orientBodyResolver, error) {
	r := &orientBodyResolver{homePath: homePath, pack: pack, cwd: cwd}
	if strings.TrimSpace(homePath) == "" {
		return r, nil
	}
	cfg, _, err := loadMemoryEngineConfig(homePath)
	if err == nil {
		r.lanes = append(r.lanes, cfg.CorpusLanes...)
		return r, nil
	}
	if isMissingMemoryEngineConfig(err) {
		return r, nil
	}
	// Malformed/unreadable config: degrade to source-refs-only. Surface the
	// config-unavailable ref during Resolve and set the degraded flag.
	r.lanes = nil
	r.degraded = true
	return r, nil
}

// isMissingMemoryEngineConfig returns true when loadMemoryEngineConfig's error
// indicates a missing file (not a parse/schema/lane error). loadMemoryEngineConfig
// uses os.IsNotExist for that case; the other branches (parse, schema, lane
// validation) are real degradation.
func isMissingMemoryEngineConfig(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// Resolve implements contract.BodyResolver. The corpus half queries every configured
// lane with the corpus query's privacy-safe DEFAULT: public snippets return their
// re-derived text, while private (privacy=private) hits are suppressed at the corpus
// layer — the resolver receives no private text, only a suppressed_private status the
// materializer records as an omission. Admitting private body text would require an
// explicit private-text opt-in, which orient never asks for. The source-ref half reads
// files for non-private-overlay Included refs. Corpus ranks above source refs; source
// refs rank by Included index (lower index = higher rank). Ties broken by ref asc
// inside the materializer.
//
// The optional configured body source's degradation (malformed config, embedder
// construction, query failure) is reported via Degraded() — the interface
// signature is fixed; the resolver struct carries the flag for callers that
// need it.
func (r *orientBodyResolver) Resolve(task string) ([]contract.BodyCandidate, error) {
	corpusCands, minCorpusRank, err := r.resolveCorpus(task)
	if err != nil {
		return nil, err
	}
	sourceCands := r.resolveSourceRefs(minCorpusRank)
	if len(corpusCands) == 0 {
		return sourceCands, nil
	}
	out := make([]contract.BodyCandidate, 0, len(corpusCands)+len(sourceCands))
	out = append(out, corpusCands...)
	out = append(out, sourceCands...)
	return out, nil
}

// resolveCorpus queries each configured lane and emits one BodyCandidate per hit.
// Returns the minimum hit score observed (used to anchor source-ref ranks BELOW all
// corpus hits). Per-lane embedder construction or query failures degrade to a
// single opaque per-lane ref; other lanes continue. A nil task or zero lanes
// short-circuits to an empty result. The configured corpus body source is
// OPTIONAL — orient must succeed on a healthy public/source-ref path even when
// every lane is unhealthy. The resolver's degraded flag is set when ANY lane
// failed or when the constructor saw a malformed config; the caller reads
// Degraded() after Resolve returns.
func (r *orientBodyResolver) resolveCorpus(task string) ([]contract.BodyCandidate, float64, error) {
	if strings.TrimSpace(task) == "" || len(r.lanes) == 0 {
		return nil, 0, nil
	}
	var out []contract.BodyCandidate
	minRank := 0.0
	gotAny := false
	for _, lane := range r.lanes {
		embedder, err := newCorpusEmbedder(lane)
		if err != nil {
			r.degraded = true
			out = append(out, contract.BodyCandidate{
				Ref:    orientBodyLaneUnavailableRef(lane.Lane),
				Source: "corpus:" + lane.Lane,
				Rank:   0,
				Status: contract.BodyStatusUnresolvable,
			})
			continue
		}
		report, err := corpus.Query(context.Background(), r.homePath, lane, embedder, task, corpusQueryTopK)
		if err != nil {
			r.degraded = true
			out = append(out, contract.BodyCandidate{
				Ref:    orientBodyLaneUnavailableRef(lane.Lane),
				Source: "corpus:" + lane.Lane,
				Rank:   0,
				Status: contract.BodyStatusUnresolvable,
			})
			continue
		}
		for _, hit := range report.Hits {
			out = append(out, contract.BodyCandidate{
				Ref:    hit.ChunkID,
				Source: "corpus:" + lane.Lane,
				Text:   hit.Text,
				Rank:   hit.Score,
				Status: hit.TextStatus,
			})
			if !gotAny || hit.Score < minRank {
				minRank = hit.Score
				gotAny = true
			}
		}
	}
	return out, minRank, nil
}

// resolveSourceRefs emits one BodyCandidate per Included ref that (a) is NOT a
// private-overlay ref and (b) resolves to one or more repo files via
// contract.SourceRefPaths. Source-ref rank is anchored BELOW all corpus hits: each
// ref gets minCorpusRank - 1 - (index*0.001), so lower Included indices (earlier in
// the pack) rank higher. Missing/unreadable files surface status source_unavailable;
// unmapped refs surface unresolvable — neither contributes body text, and the
// materializer lands them in omitted accordingly.
func (r orientBodyResolver) resolveSourceRefs(minCorpusRank float64) []contract.BodyCandidate {
	if len(r.pack.Included) == 0 {
		return nil
	}
	out := make([]contract.BodyCandidate, 0, len(r.pack.Included))
	idx := 0
	for _, inc := range r.pack.Included {
		ref := strings.TrimSpace(inc.Ref)
		if ref == "" {
			continue
		}
		// Private-overlay law: never propose as a body candidate. The materializer
		// also gates this (defense-in-depth), but filtering at source keeps the
		// candidate list honest.
		if inc.SourceClass == "private_overlay" {
			continue
		}
		rank := minCorpusRank - 1.0 - float64(idx)*0.001
		idx++
		paths := contract.SourceRefPaths(ref, r.cwd)
		if len(paths) == 0 {
			out = append(out, contract.BodyCandidate{
				Ref:    ref,
				Source: "source-ref",
				Rank:   rank,
				Status: contract.BodyStatusUnresolvable,
			})
			continue
		}
		text, status := readSourceRefBody(paths)
		out = append(out, contract.BodyCandidate{
			Ref:    ref,
			Source: "source-ref",
			Text:   text,
			Rank:   rank,
			Status: status,
		})
	}
	return out
}

// readSourceRefBody concatenates the file contents backing a source ref. The ref →
// paths mapping is many-to-one for some refs (e.g. control-plane://orientation-map
// → docs/ARCHITECTURE.md + docs/LAYERS.md), so a multi-path ref materializes ONE
// concatenated body. A clear separator keeps provenance legible to a model.
//
// Returns BodyStatusOK only when every path read successfully; any unreadable path
// degrades to source_unavailable with empty text (the materializer lands it in
// omitted — never a partial body, never a silent blank).
func readSourceRefBody(paths []string) (string, string) {
	var b strings.Builder
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", contract.BodyStatusSourceUnavailable
		}
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.Write(data)
	}
	return b.String(), contract.BodyStatusOK
}

// compile-time assertion that *orientBodyResolver satisfies the seam.
var _ contract.BodyResolver = (*orientBodyResolver)(nil)
