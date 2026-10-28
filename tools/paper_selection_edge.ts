#!/usr/bin/env bun

import { readFileSync, writeFileSync } from "node:fs";

type KnowledgeExport = {
  schema_version: "compound.knowledge_export.v0.1";
  source: {
    kind: "paper-selection";
    ref: string;
    captured_at: string;
  };
  privacy: "public";
  producer: {
    kind: "edge-script";
    name: "paper-selection-edge";
    version: "0.1";
  };
  items: KnowledgeItem[];
};

type KnowledgeItem = {
  ref: string;
  url?: string;
  title?: string;
  authors?: string[];
  published_at?: string;
  topics?: string[];
  content: string;
  lang: "en";
};

type HFDailyPaper = {
  paper?: {
    id?: string;
    title?: string;
    summary?: string;
    ai_summary?: string;
    ai_keywords?: string[];
    authors?: Array<{ name?: string } | string>;
    publishedAt?: string;
    submittedOnDailyAt?: string;
    upvotes?: number;
    githubStars?: number;
  };
};

type SemanticScholarPaper = {
  paperId?: string;
  externalIds?: { ArXiv?: string; DOI?: string; CorpusId?: number | string };
  url?: string;
  title?: string;
  abstract?: string | null;
  authors?: Array<{ name?: string } | string>;
  year?: number;
  publicationDate?: string;
  citationCount?: number;
  fieldsOfStudy?: string[];
};

type SemanticScholarResponse = {
  data?: SemanticScholarPaper[];
};

type SelectionOptions = {
  topic: string;
  maxItems: number;
  maxPerCategory: number;
  capturedAt: string;
};

type Candidate = {
  ref: string;
  title: string;
  url: string;
  authors: string[];
  publishedAt: string;
  topics: string[];
  content: string;
  sourceLane: "trending" | "fundamental";
  primaryCategory: string;
  trendScore: number;
  fundamentalScore: number;
  score: number;
};

type SelectionReport = {
  export: KnowledgeExport;
  selected: Array<{
    ref: string;
    title: string;
    source_lane: string;
    primary_category: string;
    score: number;
    trend_score: number;
    fundamental_score: number;
  }>;
};

const exportSchema = "compound.knowledge_export.v0.1";
const producerName = "paper-selection-edge";

function usage(): never {
  console.error(`usage: bun tools/paper_selection_edge.ts --topic <query> [--out export.json] [--max 10] [--max-per-category 2] [--hf-limit 20] [--semantic-limit 20] [--hf-json fixture.json] [--semantic-json fixture.json] [--captured-at RFC3339]

Selects research papers from two rented sources and emits compound.knowledge_export.v0.1.

Selection scoring:
- trending lane: Hugging Face Daily Papers rank -> trend_score in [0,1]
- fundamental lane: Semantic Scholar citation percentile within fetched set -> fundamental_score in [0,1]
- final score: trend_score + fundamental_score
- diversity cap: no more than --max-per-category papers per inferred primary category

This edge does not import, summarize, adjudicate, promote memory, or write COMPOUND_HOME.
Pipe or save the output, then use: compound knowledge import <export.json>`);
  process.exit(2);
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) usage();
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith("--")) {
      out[key] = true;
    } else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

async function fetchJSON(url: string): Promise<unknown> {
  const response = await fetch(url, {
    headers: { accept: "application/json", "user-agent": "compound-paper-selection-edge/0.1" },
  });
  if (!response.ok) {
    throw new Error(`GET ${url} failed: ${response.status} ${response.statusText}`);
  }
  return response.json();
}

async function fetchHFDaily(limit: number): Promise<unknown> {
  const url = new URL("https://huggingface.co/api/daily_papers");
  url.searchParams.set("limit", String(limit));
  return fetchJSON(url.toString());
}

async function fetchSemanticScholar(topic: string, limit: number): Promise<unknown> {
  const url = new URL("https://api.semanticscholar.org/graph/v1/paper/search/bulk");
  url.searchParams.set("query", topic);
  url.searchParams.set("fields", "title,abstract,authors,year,publicationDate,citationCount,url,externalIds,fieldsOfStudy");
  url.searchParams.set("limit", String(limit));
  url.searchParams.set("sort", "citationCount:desc");
  return fetchJSON(url.toString());
}

function loadJSON(path: string): unknown {
  return JSON.parse(readFileSync(path, "utf8"));
}

function buildPaperSelectionExport(
  hfData: unknown,
  semanticData: unknown,
  opts: SelectionOptions,
): SelectionReport {
  const hfCandidates = normalizeHFCandidates(hfData);
  const semanticCandidates = normalizeSemanticCandidates(semanticData);
  const merged = dedupeCandidates([...hfCandidates, ...semanticCandidates]);
  const selected = selectWithDiversity(merged, opts.maxItems, opts.maxPerCategory);
  if (selected.length === 0) {
    throw new Error("no papers selected");
  }
  const sourceRef = `topic:${slug(opts.topic)}`;
  return {
    export: {
      schema_version: exportSchema,
      source: {
        kind: "paper-selection",
        ref: sourceRef,
        captured_at: opts.capturedAt,
      },
      privacy: "public",
      producer: {
        kind: "edge-script",
        name: producerName,
        version: "0.1",
      },
      items: selected.map((candidate) => ({
        ref: candidate.ref,
        url: candidate.url,
        title: candidate.title,
        authors: candidate.authors,
        published_at: candidate.publishedAt,
        topics: unique([
          opts.topic,
          candidate.sourceLane,
          candidate.primaryCategory,
          ...candidate.topics,
        ]),
        content: candidate.content,
        lang: "en",
      })),
    },
    selected: selected.map((candidate) => ({
      ref: candidate.ref,
      title: candidate.title,
      source_lane: candidate.sourceLane,
      primary_category: candidate.primaryCategory,
      score: round(candidate.score),
      trend_score: round(candidate.trendScore),
      fundamental_score: round(candidate.fundamentalScore),
    })),
  };
}

function normalizeHFCandidates(data: unknown): Candidate[] {
  const rows = Array.isArray(data) ? (data as HFDailyPaper[]) : [];
  const window = Math.max(rows.length, 1);
  return rows.flatMap((row, index) => {
    const paper = row.paper ?? {};
    const title = clean(paper.title);
    const id = clean(paper.id);
    const content = clean(paper.summary) || clean(paper.ai_summary);
    if (!title || !content) return [];
    const keywords = (paper.ai_keywords ?? []).map(clean).filter(Boolean);
    const topics = unique(["huggingface-daily", ...keywords]);
    const primaryCategory = inferCategory([title, ...keywords]);
    const trendScore = (window - index) / window;
    const upvoteBoost = clamp((paper.upvotes ?? 0) / 100, 0, 0.2);
    const githubBoost = clamp((paper.githubStars ?? 0) / 50000, 0, 0.1);
    return [{
      ref: id ? `arxiv:${id}` : `hf:${slug(title)}`,
      title,
      url: id ? `https://huggingface.co/papers/${id}` : "https://huggingface.co/papers",
      authors: normalizeAuthors(paper.authors),
      publishedAt: dateOnly(paper.publishedAt) || dateOnly(paper.submittedOnDailyAt),
      topics,
      content,
      sourceLane: "trending" as const,
      primaryCategory,
      trendScore: clamp(trendScore + upvoteBoost + githubBoost, 0, 1),
      fundamentalScore: 0,
      score: clamp(trendScore + upvoteBoost + githubBoost, 0, 1),
    }];
  });
}

function normalizeSemanticCandidates(data: unknown): Candidate[] {
  const rows = Array.isArray(data)
    ? (data as SemanticScholarPaper[])
    : ((data as SemanticScholarResponse).data ?? []);
  const maxCitations = Math.max(1, ...rows.map((paper) => paper.citationCount ?? 0));
  return rows.flatMap((paper) => {
    const title = clean(paper.title);
    if (!title) return [];
    const ref = semanticRef(paper);
    if (!ref) return [];
    const fields = (paper.fieldsOfStudy ?? []).map(clean).filter(Boolean);
    const primaryCategory = inferCategory([title, ...fields]);
    const citationCount = paper.citationCount ?? 0;
    const fundamentalScore = clamp(citationCount / maxCitations, 0, 1);
    const content = clean(paper.abstract) || `${title}. Semantic Scholar citation count: ${citationCount}.`;
    return [{
      ref,
      title,
      url: paper.url || urlFromRef(ref),
      authors: normalizeAuthors(paper.authors),
      publishedAt: paper.publicationDate || (paper.year ? String(paper.year) : ""),
      topics: unique(["semantic-scholar", ...fields, `citations:${citationCount}`]),
      content,
      sourceLane: "fundamental" as const,
      primaryCategory,
      trendScore: 0,
      fundamentalScore,
      score: fundamentalScore,
    }];
  });
}

function semanticRef(paper: SemanticScholarPaper): string {
  const arxiv = clean(paper.externalIds?.ArXiv);
  if (arxiv) return `arxiv:${arxiv}`;
  const doi = clean(paper.externalIds?.DOI);
  if (doi) return `doi:${doi}`;
  const paperId = clean(paper.paperId);
  return paperId ? `semanticscholar:${paperId}` : "";
}

function urlFromRef(ref: string): string {
  if (ref.startsWith("arxiv:")) return `https://arxiv.org/abs/${ref.slice("arxiv:".length)}`;
  if (ref.startsWith("doi:")) return `https://doi.org/${ref.slice("doi:".length)}`;
  if (ref.startsWith("semanticscholar:")) return `https://www.semanticscholar.org/paper/${ref.slice("semanticscholar:".length)}`;
  return "";
}

function dedupeCandidates(candidates: Candidate[]): Candidate[] {
  const byKey = new Map<string, Candidate>();
  for (const candidate of candidates) {
    const key = dedupeKey(candidate);
    const existing = byKey.get(key);
    if (!existing) {
      byKey.set(key, candidate);
      continue;
    }
    byKey.set(key, {
      ...existing,
      authors: unique([...existing.authors, ...candidate.authors]),
      topics: unique([...existing.topics, ...candidate.topics]),
      content: existing.content.length >= candidate.content.length ? existing.content : candidate.content,
      trendScore: Math.max(existing.trendScore, candidate.trendScore),
      fundamentalScore: Math.max(existing.fundamentalScore, candidate.fundamentalScore),
      score: Math.max(existing.trendScore, candidate.trendScore) + Math.max(existing.fundamentalScore, candidate.fundamentalScore),
      sourceLane: existing.sourceLane === "trending" ? existing.sourceLane : candidate.sourceLane,
    });
  }
  return [...byKey.values()].sort(compareCandidate);
}

function dedupeKey(candidate: Candidate): string {
  if (candidate.ref.startsWith("arxiv:")) return candidate.ref.toLowerCase();
  return slug(candidate.title);
}

function selectWithDiversity(candidates: Candidate[], maxItems: number, maxPerCategory: number): Candidate[] {
  const selected: Candidate[] = [];
  const byCategory = new Map<string, number>();
  for (const candidate of candidates.sort(compareCandidate)) {
    if (selected.length >= maxItems) break;
    const used = byCategory.get(candidate.primaryCategory) ?? 0;
    if (used >= maxPerCategory) continue;
    selected.push(candidate);
    byCategory.set(candidate.primaryCategory, used + 1);
  }
  return selected;
}

function compareCandidate(a: Candidate, b: Candidate): number {
  return b.score - a.score || b.fundamentalScore - a.fundamentalScore || b.trendScore - a.trendScore || a.title.localeCompare(b.title);
}

function inferCategory(values: string[]): string {
  const haystack = values.join(" ").toLowerCase();
  const groups: Array<[string, string[]]> = [
    ["agents", ["agent", "multi-agent", "tool use", "planning"]],
    ["retrieval", ["retrieval", "search", "nearest neighbor", "rag", "index"]],
    ["language-models", ["language model", "llm", "transformer", "attention", "prompt"]],
    ["vision", ["vision", "image", "video", "multimodal"]],
    ["robotics", ["robot", "embodied"]],
    ["datasets", ["dataset", "benchmark", "corpus"]],
    ["systems", ["system", "serving", "inference", "distributed"]],
  ];
  for (const [category, needles] of groups) {
    if (needles.some((needle) => haystack.includes(needle))) return category;
  }
  const fallback = values.map(clean).find(Boolean) ?? "general";
  return slug(fallback).slice(0, 48) || "general";
}

function normalizeAuthors(input: Array<{ name?: string } | string> | undefined): string[] {
  return unique((input ?? []).map((author) => typeof author === "string" ? author : author.name ?? "").map(clean).filter(Boolean));
}

function clean(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function dateOnly(value: unknown): string {
  const text = clean(value);
  if (!text) return "";
  return text.slice(0, 10);
}

function unique(values: string[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const value of values) {
    const text = clean(value);
    const key = text.toLowerCase();
    if (!text || seen.has(key)) continue;
    seen.add(key);
    out.push(text);
  }
  return out;
}

function slug(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "") || "paper";
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}

function round(value: number): number {
  return Math.round(value * 10000) / 10000;
}

function positiveInt(value: unknown, fallback: number): number {
  if (typeof value !== "string") return fallback;
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  if (typeof args.topic !== "string") usage();
  const maxItems = Math.min(positiveInt(args.max, 10), 10);
  const maxPerCategory = positiveInt(args["max-per-category"], 2);
  const hfLimit = positiveInt(args["hf-limit"], 20);
  const semanticLimit = positiveInt(args["semantic-limit"], 20);
  const capturedAt = typeof args["captured-at"] === "string" ? args["captured-at"] : new Date().toISOString();
  const hfData = typeof args["hf-json"] === "string" ? loadJSON(args["hf-json"]) : await fetchHFDaily(hfLimit);
  const semanticData = typeof args["semantic-json"] === "string" ? loadJSON(args["semantic-json"]) : await fetchSemanticScholar(args.topic, semanticLimit);
  const report = buildPaperSelectionExport(hfData, semanticData, {
    topic: args.topic,
    maxItems,
    maxPerCategory,
    capturedAt,
  });
  const exportJSON = `${JSON.stringify(report.export, null, 2)}\n`;
  if (typeof args.out === "string") {
    writeFileSync(args.out, exportJSON);
    console.log(JSON.stringify({
      status: "ok",
      out: args.out,
      items: report.export.items.length,
      selected: report.selected,
    }, null, 2));
    return;
  }
  process.stdout.write(exportJSON);
}

export {
  buildPaperSelectionExport,
  normalizeHFCandidates,
  normalizeSemanticCandidates,
  selectWithDiversity,
};

if (import.meta.main) {
  main().catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  });
}
