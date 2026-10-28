import type { AttentionReport } from "@/lib/attention";
import { REGISTRY } from "@/lib/decision-registry";
import { GENERIC_ACTION_ALLOWLIST } from "@/lib/generic-actions";

/**
 * Pure, node-free shaping for the Settings area (redesign plan Part 3). Kept
 * import-clean of `@/lib/compound` (which pulls node builtins) so it can be
 * pulled from any settings component. Two jobs:
 *   1. parse `compound harness list` (text output — it ignores --json) into
 *      structured capability cards;
 *   2. compute THE decision-registry coverage table — the transparency matrix
 *      of which live review areas have a server adapter vs a copy-only fallback.
 */

const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const num = (v: unknown): number => (typeof v === "number" ? v : 0);

/* ---------- harness registry (text projection) ---------- */

export type HarnessCard = {
  name: string;
  capabilities: string;
  hooks: string;
  fallback: boolean;
  limitations: string[];
};

/**
 * `compound harness list` prints, per harness: a header line
 * `name  (capabilities)` (optionally with a trailing `[fallback]`), then an
 * indented `hooks: …` line and any number of indented `limitation: …` lines.
 * Defensive: an unparseable body yields an empty list and the caller shows the
 * raw text instead — never a fabricated card.
 */
export function parseHarnessList(text: string): HarnessCard[] {
  const cards: HarnessCard[] = [];
  let current: HarnessCard | null = null;
  for (const raw of text.split("\n")) {
    if (!raw.trim()) continue;
    const indented = /^\s/.test(raw);
    const line = raw.trim();
    if (!indented) {
      const match = line.match(/^(\S+)\s*(?:\(([^)]*)\))?\s*(\[fallback\])?/);
      if (!match) continue;
      current = {
        name: match[1],
        capabilities: match[2] ?? "",
        hooks: "",
        fallback: Boolean(match[3]),
        limitations: [],
      };
      cards.push(current);
      continue;
    }
    if (!current) continue;
    if (line.startsWith("hooks:")) current.hooks = line.slice("hooks:".length).trim();
    else if (line.startsWith("limitation:")) current.limitations.push(line.slice("limitation:".length).trim());
  }
  return cards;
}

/* ---------- decision-registry coverage (THE transparency table) ---------- */

export type AreaCoverage = "bespoke" | "generic" | "fallback";

export type CoverageRow = {
  area: string;
  /** wired as a bespoke DecisionAreaSpec in the console decision-registry. */
  bespoke: boolean;
  /** certified for the generic governed pathway (GENERIC_ACTION_ALLOWLIST). */
  generic: boolean;
  /** present in the live attention projection right now. */
  live: boolean;
  liveCount: number;
  /** the registry `source` for a bespoke area, else "". */
  source: string;
  coverage: AreaCoverage;
};

export type CoverageReport = {
  rows: CoverageRow[];
  bespokeAreas: { area: string; source: string }[];
  genericAreas: string[];
  liveAreas: number;
  serverAdapterAreas: number;
  fallbackAreas: number;
};

/**
 * Cross-reference the two console-owned certification registries against the
 * live review areas the attention projection is actually surfacing. An area has
 * server-adapter coverage if it is either a bespoke spec OR a generic-certified
 * verb; a live area that is neither is honest copy-only fallback. This report
 * does not claim that the standard browser is authorized to invoke the adapter.
 */
export function buildCoverage(attention: AttentionReport | null): CoverageReport {
  const bespokeAreas = Object.entries(REGISTRY).map(([area, spec]) => ({
    area,
    source: str((spec as { source?: unknown }).source),
  }));
  const bespokeSet = new Map(bespokeAreas.map((b) => [b.area, b.source]));
  const genericAreas = Object.keys(GENERIC_ACTION_ALLOWLIST);
  const genericSet = new Set(genericAreas);

  const liveCounts = new Map<string, number>();
  for (const group of arr(attention?.groups) as Array<Record<string, unknown>>) {
    if (str(group.band) !== "needs_you") continue;
    const area = str(group.area);
    if (!area) continue;
    liveCounts.set(area, (liveCounts.get(area) ?? 0) + (num(group.count) || 1));
  }

  const areas = new Set<string>([...bespokeSet.keys(), ...genericSet, ...liveCounts.keys()]);
  const rows: CoverageRow[] = [...areas]
    .map((area) => {
      const bespoke = bespokeSet.has(area);
      const generic = genericSet.has(area);
      const live = liveCounts.has(area);
      const coverage: AreaCoverage = bespoke ? "bespoke" : generic ? "generic" : "fallback";
      return {
        area,
        bespoke,
        generic,
        live,
        liveCount: liveCounts.get(area) ?? 0,
        source: bespoke ? (bespokeSet.get(area) ?? "") : "",
        coverage,
      };
    })
    .sort((a, b) => {
      // live areas first (that is where coverage gaps actually bite), then by
      // count desc, then name — deterministic.
      if (a.live !== b.live) return a.live ? -1 : 1;
      if (a.liveCount !== b.liveCount) return b.liveCount - a.liveCount;
      return a.area.localeCompare(b.area);
    });

  return {
    rows,
    bespokeAreas: bespokeAreas.sort((a, b) => a.area.localeCompare(b.area)),
    genericAreas: genericAreas.sort(),
    liveAreas: liveCounts.size,
    serverAdapterAreas: rows.filter((r) => r.live && r.coverage !== "fallback").length,
    fallbackAreas: rows.filter((r) => r.live && r.coverage === "fallback").length,
  };
}
