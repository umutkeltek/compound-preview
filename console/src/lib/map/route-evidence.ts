import { readFile, stat } from "node:fs/promises";
import path from "node:path";

import { arr, COMPOUND_HOME, num, privateFallbacksEnabled, runJson, str } from "@/lib/compound";

/*
 * Execution route evidence — metadata only (no prompts/responses/secrets).
 * "Route evidence, not task success." Many rows have NO work_id and NO
 * cost_micros — only usage.total_tokens + latency_ms. Read defensively.
 */

export type RouteRecord = {
  id?: string;
  request_id?: string;
  work_id?: string;
  transition_id?: string;
  client_profile?: string;
  protocol?: string;
  selected_target?: { provider?: string; model?: string; protocol?: string };
  usage?: { total_tokens?: number };
  cost_micros?: number;
  latency_ms?: number;
  status?: string;
};

export async function readRouteEvidence(
  limit = 200,
): Promise<{ records: RouteRecord[]; mtime: string | null }> {
  const projected = await runJson(["route", "evidence-authority", "--json", "--recent", String(limit)]);
  if (projected) {
    return {
      records: arr(projected.recent).map((row) => ({
        id: str(row.id),
        request_id: str(row.request_id),
        work_id: str(row.work_id),
        transition_id: str(row.transition_id),
        client_profile: str(row.client_profile),
        protocol: str(row.protocol),
        selected_target: {
          provider: str(row.provider),
          model: str(row.model),
          protocol: str(row.protocol),
        },
        usage: { total_tokens: num(row.total_tokens) },
        cost_micros: num(row.cost_micros),
        status: str(row.status),
      })),
      mtime: null,
    };
  }

  if (!privateFallbacksEnabled()) return { records: [], mtime: null };

  const file = path.join(
    COMPOUND_HOME,
    "ledger",
    "execution-route-evidence.jsonl",
  );
  let text: string;
  let mtime: string | null = null;
  try {
    text = await readFile(file, "utf8");
    try {
      mtime = (await stat(file)).mtime.toISOString();
    } catch {
      /* mtime optional */
    }
  } catch {
    return { records: [], mtime: null };
  }
  const lines = text.split(/\r?\n/).filter((l) => l.trim());
  const tail = lines.slice(-limit);
  const records: RouteRecord[] = [];
  for (const line of tail) {
    try {
      records.push(JSON.parse(line) as RouteRecord);
    } catch {
      /* skip */
    }
  }
  return { records, mtime };
}
