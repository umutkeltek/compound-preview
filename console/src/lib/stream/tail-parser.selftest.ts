/*
 * Depth test for the SSE tail parser (console/src/app/api/stream/route.ts's
 * data layer). These assertions would FAIL if the tailing were shallow — i.e.
 * if a partial line got parsed as if complete, a truncated ledger kept
 * reading from a stale offset, or a reconnect replay produced a dedupe key
 * that collides with (or fails to collide with) the real thing. Pure
 * (synthetic + one real sample line); run via scripts/stream-tail-smoke.mjs.
 */

import {
  dedupeKey,
  extractMoveType,
  feedLines,
  parseWorkEventLine,
  resolveReadOffset,
  type WorkEvent,
} from "./tail-parser";

type Check = { name: string; ok: boolean; detail: string };

export function runTailParserSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // 1. A real close-event line (verbatim shape from internal/work/work.go's
  //    appendEvent call) parses cleanly and the required fields survive.
  const realLine =
    '{"ts":"1783878151988936000","work_id":"work_bee2f7925e82","from":"verifying","to":"completed",' +
    '"note":"closed: control_plane_repair (policy allow_with_obligations)",' +
    '"attached":["act_0b742c44a8da","pol_22c75645fb39","rec_7e8a1874951f","tr_7e8a1874951f"]}';
  const parsedReal = parseWorkEventLine(realLine);
  push(
    "parses-real-close-event-line",
    parsedReal?.work_id === "work_bee2f7925e82" && parsedReal?.to === "completed" && parsedReal?.attached?.length === 4,
    `parsed=${JSON.stringify(parsedReal)}`,
  );

  // 2. move_type is extracted from the close note, never fabricated for a
  //    plain advance note.
  push(
    "extracts-move-type-from-close-note",
    extractMoveType(parsedReal?.note) === "control_plane_repair",
    `extracted=${extractMoveType(parsedReal?.note)}`,
  );
  push(
    "no-move-type-on-plain-advance-note",
    extractMoveType("review finding artifact written (policy allow_with_obligations)") === null,
    "advance notes never carry a closed: prefix",
  );
  push("no-move-type-when-note-absent", extractMoveType(undefined) === null, "undefined note -> null, never throws");

  // 3. Malformed/partial/blank lines never parse — a torn write mid-flush
  //    must be dropped, not surfaced as a bogus event.
  push("blank-line-is-null", parseWorkEventLine("   ") === null, "whitespace-only line");
  push(
    "truncated-json-is-null",
    parseWorkEventLine('{"ts":"123","work_id":"work_x","from":"a","to":"b') === null,
    "a line cut mid-write (no closing brace) must not parse",
  );
  push(
    "missing-required-field-is-null",
    parseWorkEventLine('{"ts":"123","from":"a","to":"b"}') === null,
    "work_id absent -> reject rather than emit a half event",
  );

  // 4. Dedupe key is exactly ts+work_id+to (the spec'd identity) — same event
  //    replayed on reconnect produces the identical key; a genuinely
  //    different transition (different `to`) never collides.
  const closed: WorkEvent = { ts: "100", work_id: "work_a", from: "running", to: "completed" };
  const replay: WorkEvent = { ts: "100", work_id: "work_a", from: "running", to: "completed" };
  const different: WorkEvent = { ts: "100", work_id: "work_a", from: "running", to: "failed" };
  push("dedupe-key-format", dedupeKey(closed) === "100:work_a:completed", dedupeKey(closed));
  push("dedupe-key-collides-on-replay", dedupeKey(closed) === dedupeKey(replay), "reconnect replay of the same line");
  push("dedupe-key-distinguishes-different-to", dedupeKey(closed) !== dedupeKey(different), "completed vs failed");

  // 5. Partial-line buffering: a line split across two chunk reads is held
  //    back (not parsed early) until its newline arrives, then completes
  //    exactly once — this is the depth the raw fs.watch/poll tailing in
  //    route.ts leans on.
  const wholeLine = '{"ts":"1","work_id":"work_p","from":"","to":"ready"}\n';
  const splitAt = 30;
  const chunkA = wholeLine.slice(0, splitAt);
  const chunkB = wholeLine.slice(splitAt);
  const stepA = feedLines("", chunkA);
  push(
    "partial-line-buffered-not-parsed-early",
    stepA.lines.length === 0 && stepA.remainder === chunkA,
    `lines=${stepA.lines.length} remainder=${JSON.stringify(stepA.remainder)}`,
  );
  const stepB = feedLines(stepA.remainder, chunkB);
  push(
    "partial-line-completes-on-next-chunk",
    stepB.lines.length === 1 && parseWorkEventLine(stepB.lines[0])?.work_id === "work_p" && stepB.remainder === "",
    `lines=${JSON.stringify(stepB.lines)} remainder=${JSON.stringify(stepB.remainder)}`,
  );

  // 6. Multiple complete lines plus a trailing partial line in ONE chunk:
  //    both complete lines surface, the partial tail is held.
  const multi = feedLines(
    "",
    '{"ts":"1","work_id":"work_a","from":"","to":"ready"}\n' +
      '{"ts":"2","work_id":"work_b","from":"ready","to":"claimed"}\n' +
      '{"ts":"3","work_id":"work_c","from":"claimed"',
  );
  push(
    "multi-line-chunk-yields-both-complete-lines",
    multi.lines.length === 2 && multi.remainder === '{"ts":"3","work_id":"work_c","from":"claimed"',
    `lines=${multi.lines.length} remainder=${JSON.stringify(multi.remainder)}`,
  );

  // 7. A chunk with no newline at all is pure remainder (no line fabricated).
  const noNewline = feedLines("", '{"ts":"1","work_id":"wo');
  push(
    "no-newline-chunk-is-pure-remainder",
    noNewline.lines.length === 0 && noNewline.remainder === '{"ts":"1","work_id":"wo',
    `lines=${noNewline.lines.length}`,
  );

  // 8. Cursor reset on truncation: file shrank below the last-read offset
  //    (rotation/compaction) -> reset to 0 and flag it; growth just advances.
  push(
    "truncation-resets-offset-to-zero",
    (() => {
      const r = resolveReadOffset(5000, 120);
      return r.truncated === true && r.offset === 0;
    })(),
    "currentSize(120) < previousOffset(5000)",
  );
  push(
    "growth-keeps-offset-unflagged",
    (() => {
      const r = resolveReadOffset(5000, 5400);
      return r.truncated === false && r.offset === 5000;
    })(),
    "currentSize(5400) > previousOffset(5000)",
  );
  push(
    "unchanged-size-keeps-offset-unflagged",
    (() => {
      const r = resolveReadOffset(5000, 5000);
      return r.truncated === false && r.offset === 5000;
    })(),
    "no growth this tick -> no-op, not a truncation",
  );

  const details = checks.map((c) => `${c.ok ? "ok  " : "FAIL"} ${c.name} — ${c.detail}`);
  return { ok: checks.every((c) => c.ok), details };
}
