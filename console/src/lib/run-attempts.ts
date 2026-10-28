import { execFile } from "node:child_process";
import { promisify } from "node:util";

import { COMPOUND_BIN, COMPOUND_HOME, privateFallbacksEnabled, str, type Json } from "@/lib/compound";
import { consoleChildEnv } from "@/lib/console-child-env";

/*
 * Run-attempt door — the "how was this work executed" evidence layer
 * (C1–C5, internal/runattempt). `compound run-attempt list --json` emits a
 * JSON ARRAY of RunAttempt (not an object), so this reads raw stdout via exec
 * rather than runJson's object parser. Fail-soft: a missing verb (older
 * binary) or unparseable output yields null, and the traffic board then shows
 * "execution detail unavailable" — never an invented harness/status. The pure
 * mapper (normalizeRunAttempts) is selftest-tested (run-attempts.selftest.ts).
 */

const exec = promisify(execFile);

export type RunAttempt = {
  id: string;
  workId: string;
  actionIntentId: string;
  actor: string;
  harness: string;
  status: string;
  command: string;
  transcriptRef: string;
  predecessorId: string;
  createdAt: string;
  completedAt: string;
  lastHeartbeatAt: string;
};

/** Pure, fail-soft: drops entries with no id (never fabricates an attempt). */
export function normalizeRunAttempts(raw: unknown): RunAttempt[] {
  if (!Array.isArray(raw)) return [];
  const out: RunAttempt[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const a = item as Json;
    const id = str(a.id);
    if (!id) continue;
    out.push({
      id,
      workId: str(a.work_id),
      actionIntentId: str(a.action_intent_id),
      actor: str(a.actor),
      harness: str(a.harness),
      status: str(a.status),
      command: str(a.command),
      transcriptRef: str(a.transcript_ref),
      predecessorId: str(a.predecessor_id),
      createdAt: str(a.created_at),
      completedAt: str(a.completed_at),
      lastHeartbeatAt: str(a.last_heartbeat_at),
    });
  }
  return out;
}

export async function getRunAttempts(): Promise<RunAttempt[] | null> {
  // Transcript-bearing execution evidence is a private runtime read; the door
  // stays closed (same "detail unavailable" render as an absent verb) unless
  // the operator opts in via privateFallbacksEnabled().
  if (!privateFallbacksEnabled()) return null;
  try {
    const { stdout } = await exec(COMPOUND_BIN, ["run-attempt", "list", "--json"], {
      env: consoleChildEnv({ COMPOUND_HOME }),
      timeout: 15_000,
      maxBuffer: 64 * 1024 * 1024,
    });
    const trimmed = stdout.trim();
    if (!trimmed) return [];
    return normalizeRunAttempts(JSON.parse(trimmed));
  } catch {
    return null; // door absent (older binary) / unparseable: honest fail-soft
  }
}

/** Retry depth: predecessor-chain length ending at `attempt` (0 = first try). */
export function retryDepth(attempt: RunAttempt, byId: Map<string, RunAttempt>): number {
  let depth = 0;
  let cur: RunAttempt | undefined = attempt;
  const seen = new Set<string>();
  while (cur && cur.predecessorId && !seen.has(cur.id)) {
    seen.add(cur.id);
    cur = byId.get(cur.predecessorId);
    depth += 1;
    if (depth > 100) break; // cycle guard, never spin
  }
  return depth;
}
