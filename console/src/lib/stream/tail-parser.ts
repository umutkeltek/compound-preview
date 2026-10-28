/**
 * Pure parsing/buffering primitives for tailing events/work.jsonl over SSE
 * (console/src/app/api/stream/route.ts). Kept node-free and side-effect-free
 * so it is unit-testable without touching a real filesystem (see
 * tail-parser.selftest.ts, run via scripts/stream-tail-smoke.mjs) and safe to
 * import from the client hook (useEnvelopeStream) for the shared
 * WorkEvent/dedupeKey contract — no `@/lib/compound` import here on purpose
 * (that pulls node:child_process/fs and cannot be imported by a client
 * component; same reasoning as @/lib/time and @/lib/move-mix).
 *
 * WorkEvent mirrors internal/work/work.go's Event struct EXACTLY — never
 * invent a field the ledger does not write.
 */

export type WorkEvent = {
  ts: string;
  work_id: string;
  from: string;
  to: string;
  by?: string;
  note?: string;
  attached?: string[];
};

function isString(value: unknown): value is string {
  return typeof value === "string";
}

/**
 * Parses one complete JSONL line into a WorkEvent, or null if the line is
 * blank, not valid JSON, or missing a required field. Never throws — a
 * malformed line is dropped, never crashes the tail.
 */
export function parseWorkEventLine(line: string): WorkEvent | null {
  const trimmed = line.trim();
  if (!trimmed) return null;

  let raw: unknown;
  try {
    raw = JSON.parse(trimmed);
  } catch {
    return null;
  }
  if (typeof raw !== "object" || raw === null) return null;

  const obj = raw as Record<string, unknown>;
  if (!isString(obj.ts) || !isString(obj.work_id) || !isString(obj.to)) return null;

  return {
    ts: obj.ts,
    work_id: obj.work_id,
    from: isString(obj.from) ? obj.from : "",
    to: obj.to,
    by: isString(obj.by) ? obj.by : undefined,
    note: isString(obj.note) ? obj.note : undefined,
    attached: Array.isArray(obj.attached) ? obj.attached.filter(isString) : undefined,
  };
}

/**
 * The close writer (internal/work/work.go ~line 1379) encodes move_type into
 * the note as `"closed: <move_type> (policy ...)"`. Advance/claim/action
 * events carry no move_type on the wire — this returns null for those rather
 * than fabricating one; the ticker falls back to the plain `to`-state copy.
 */
export function extractMoveType(note: string | undefined): string | null {
  if (!note) return null;
  const match = /^closed:\s*(\S+)/.exec(note);
  return match ? match[1] : null;
}

/**
 * Dedupe key for the ticker: ts+work_id+to, exactly as specified. `ts` is the
 * ledger's own event timestamp (not a client clock), so this is stable across
 * a reconnect replaying the same backlog line.
 */
export function dedupeKey(event: WorkEvent): string {
  return `${event.ts}:${event.work_id}:${event.to}`;
}

/**
 * Buffers partial lines across read/chunk boundaries: feedLines(prevRemainder,
 * chunk) returns every complete line found in remainder+chunk, plus whatever
 * incomplete tail is left over (fed back in as `remainder` on the next call).
 * Pure — no I/O, no Node dependency.
 */
export function feedLines(remainder: string, chunk: string): { lines: string[]; remainder: string } {
  const combined = remainder + chunk;
  const parts = combined.split("\n");
  const nextRemainder = parts.pop() ?? "";
  return { lines: parts.filter((line) => line.trim().length > 0), remainder: nextRemainder };
}

/**
 * Decides the byte offset to resume tailing from, given the previously-read
 * offset and the file's current size. A file that shrank below the last
 * offset was truncated or rotated — the caller resets its line buffer and
 * re-reads from 0 rather than seeking past the new end.
 */
export function resolveReadOffset(
  previousOffset: number,
  currentSize: number,
): { offset: number; truncated: boolean } {
  if (currentSize < previousOffset) return { offset: 0, truncated: true };
  return { offset: previousOffset, truncated: false };
}
