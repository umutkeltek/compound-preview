import { readFile } from "node:fs/promises";
import path from "node:path";

import { COMPOUND_HOME, num, privateFallbacksEnabled, str, type Json } from "@/lib/compound";
import { readRouteEvidence } from "@/lib/map/route-evidence";

/*
 * Lane traffic — the Work view's CapsuleColumns data (DESIGN.md §9.6):
 * per-provider request traffic (route-evidence, last ~200 rows — the same
 * "traffic not spend" honesty contract as agents/CostLanes.tsx and
 * routing/RouteTraffic.tsx) combined with per-provider account health
 * (projections/account-projection.json).
 */

const ACCOUNT_PROJECTION_FRESHNESS_MS = 6 * 60 * 60 * 1000; // mirrors Go's accountProjectionFreshnessWindow

export type AccountHealthRow = {
  provider: string;
  healthyAccountCount: number;
  totalAccountCount: number;
  bestRemainingSummary: string;
  freshestSampledAt: string;
};

export type AccountProjectionSnapshot = {
  importedAt: string;
  providers: AccountHealthRow[];
};

/**
 * projections/account-projection.json is identity-free BY CONSTRUCTION (the
 * Go normalizer strips every account id/alias/email — see
 * internal/routeevidence/account_projection.go's header comment: "provider-
 * grain counts and timestamps survive. That stripping IS the feature"). There
 * is no `compound … --json` verb that surfaces it yet, so this reads the
 * projection file directly — gated behind privateFallbacksEnabled() like
 * every other raw ledger/projection read in this console (route-evidence.ts,
 * map/projections.ts, memory-data.ts) for CONSISTENCY with that convention,
 * not because this particular file is sensitive.
 */
export async function getAccountProjection(): Promise<AccountProjectionSnapshot | null> {
  if (!privateFallbacksEnabled()) return null;
  try {
    const text = await readFile(path.join(COMPOUND_HOME, "projections", "account-projection.json"), "utf8");
    const doc = JSON.parse(text) as Json;
    const providers = Object.values((doc.providers ?? {}) as Record<string, Json>).map(
      (entry): AccountHealthRow => ({
        provider: str(entry.provider),
        healthyAccountCount: num(entry.healthy_account_count),
        totalAccountCount: num(entry.total_account_count),
        bestRemainingSummary: str(entry.best_remaining_summary),
        freshestSampledAt: str(entry.freshest_sampled_at),
      }),
    );
    return { importedAt: str(doc.imported_at), providers };
  } catch {
    return null; // absent/unparseable projection: fail-open, no gate — same posture as the Go loader.
  }
}

export function accountProjectionIsStale(importedAt: string, now = Date.now()): boolean {
  const t = new Date(importedAt).getTime();
  if (Number.isNaN(t)) return true;
  return now - t > ACCOUNT_PROJECTION_FRESHNESS_MS;
}

export type LaneTrafficRow = {
  provider: string;
  requestCount: number;
  errorCount: number;
  totalTokens: number;
  avgLatencyMs: number;
  healthyAccountCount: number;
  totalAccountCount: number;
  bestRemainingSummary: string;
};

export type LaneTrafficReport = {
  rows: LaneTrafficRow[];
  accountProjection: AccountProjectionSnapshot | null;
};

/** Groups the recent route-evidence tail by selected_target.provider (the
 *  "lane"), joins in account health where known. A provider seen only in
 *  traffic (no projection entry) or only in the projection (no recent
 *  traffic) still gets a row — never dropped for missing half the picture. */
export async function buildLaneTraffic(recentLimit = 200): Promise<LaneTrafficReport> {
  const [{ records }, accountProjection] = await Promise.all([
    readRouteEvidence(recentLimit),
    getAccountProjection(),
  ]);

  type Agg = { count: number; errors: number; tokens: number; latencySum: number; latencyN: number };
  const byProvider = new Map<string, Agg>();
  for (const record of records) {
    const provider = str(record.selected_target?.provider) || "unknown";
    const agg: Agg = byProvider.get(provider) ?? { count: 0, errors: 0, tokens: 0, latencySum: 0, latencyN: 0 };
    agg.count++;
    if (record.status && record.status !== "ok") agg.errors++;
    agg.tokens += num(record.usage?.total_tokens);
    if (typeof record.latency_ms === "number") {
      agg.latencySum += record.latency_ms;
      agg.latencyN++;
    }
    byProvider.set(provider, agg);
  }

  const accountByProvider = new Map((accountProjection?.providers ?? []).map((p) => [p.provider, p]));
  const providers = new Set<string>([...byProvider.keys(), ...accountByProvider.keys()]);

  const rows: LaneTrafficRow[] = [...providers]
    .map((provider) => {
      const traffic = byProvider.get(provider);
      const account = accountByProvider.get(provider);
      return {
        provider,
        requestCount: traffic?.count ?? 0,
        errorCount: traffic?.errors ?? 0,
        totalTokens: traffic?.tokens ?? 0,
        avgLatencyMs: traffic && traffic.latencyN > 0 ? Math.round(traffic.latencySum / traffic.latencyN) : 0,
        healthyAccountCount: account?.healthyAccountCount ?? 0,
        totalAccountCount: account?.totalAccountCount ?? 0,
        bestRemainingSummary: account?.bestRemainingSummary ?? "",
      };
    })
    .sort((a, b) => b.requestCount - a.requestCount);

  return { rows, accountProjection };
}
