import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

import { arr, COMPOUND_HOME, privateFallbacksEnabled, runJson, str } from "@/lib/compound";

/*
 * Work leases: projection-first via `compound work leases --json`; direct lease
 * file reads are only the opt-in private fallback. Filename == work_id.
 */

export type Lease = {
  work_id: string;
  by: string;
  claimed_at?: string | number;
};

export async function readLeases(): Promise<Lease[]> {
  const projected = await runJson(["work", "leases", "--json"]);
  if (projected) {
    return arr(projected.leases).map((lease) => ({
      work_id: str(lease.work_id),
      by: str(lease.by),
      claimed_at: lease.claimed_at,
    }));
  }

  if (!privateFallbacksEnabled()) return [];

  const dir = path.join(COMPOUND_HOME, "work", "leases");
  let files: string[];
  try {
    files = await readdir(dir);
  } catch {
    return [];
  }
  const out: Lease[] = [];
  for (const f of files) {
    if (!f.endsWith(".json")) continue;
    try {
      const j = JSON.parse(await readFile(path.join(dir, f), "utf8"));
      out.push({
        work_id: String(j.work_id ?? f.replace(/\.json$/, "")),
        by: String(j.by ?? ""),
        claimed_at: j.claimed_at,
      });
    } catch {
      /* skip malformed lease */
    }
  }
  return out;
}
