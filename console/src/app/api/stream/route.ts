import { spawn } from "node:child_process";

import { COMPOUND_BIN, COMPOUND_HOME, runJson } from "@/lib/compound";
import { consoleChildEnv } from "@/lib/console-child-env";
import { dedupeKey, parseWorkEventLine } from "@/lib/stream/tail-parser";

/*
 * Live envelope-spine stream (DESIGN.md §9.6 "Information flow") — Server-Sent
 * Events over the governed `compound stream` read-only verb. GET only, no
 * mutation, ever.
 *
 * This path NO LONGER depends on privateFallbacksEnabled(): the raw-tail
 * fallback is retired. Instead the console drives a first-class CLI surface —
 *   - backlog seed: the `stream --once --json` door (bounded, memoized,
 *     doors-budgeted) returns the last N events + the end-of-log byte cursor;
 *   - live tail: one per-client `compound stream --follow --cursor <cursor>`
 *     child, which opens events/work.jsonl READ-ONLY (never the work-event
 *     write lock) and emits JSONL frames. The child is killed on client
 *     disconnect.
 * The child's JSONL frames are translated back into the SAME SSE vocabulary the
 * client hook (useEnvelopeStream) already speaks — ready / transition /
 * memory-pulse / heartbeat — so the hook needs no changes.
 */

export const dynamic = "force-dynamic";

const HEARTBEAT_MS = 25_000;

function sseFrame(event: string, data: unknown, id?: string): string {
  const lines = [`event: ${event}`];
  if (id) lines.push(`id: ${id}`);
  lines.push(`data: ${JSON.stringify(data)}`, "", "");
  return lines.join("\n");
}

export async function GET(request: Request) {
  const encoder = new TextEncoder();
  let closed = false;
  let child: ReturnType<typeof spawn> | null = null;
  let heartbeat: ReturnType<typeof setInterval> | null = null;

  // Teardown shared by the abort listener and the stream's cancel(): stop the
  // heartbeat and kill the per-client follow child so no orphaned `compound
  // stream --follow` process survives a disconnect.
  const teardown = () => {
    closed = true;
    if (heartbeat) {
      clearInterval(heartbeat);
      heartbeat = null;
    }
    if (child) {
      try {
        child.kill("SIGTERM");
      } catch {
        /* already gone */
      }
      child = null;
    }
  };

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const send = (chunk: string) => {
        if (closed) return;
        try {
          controller.enqueue(encoder.encode(chunk));
        } catch {
          /* client already gone; the abort handler finishes cleanup */
        }
      };

      const cleanup = () => {
        if (closed) return;
        teardown();
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      };
      request.signal.addEventListener("abort", cleanup);

      send(`retry: 3000\n\n`);
      // A follow child is always spawned below, so streaming is live in the
      // DEFAULT launch — no private-fallback gate on this path anymore.
      send(sseFrame("ready", { streaming: true }));

      // Seed the backlog through the --once door so first paint isn't empty; its
      // cursor is exactly where live tailing resumes. Anything appended in the
      // gap between the snapshot and the follow child is re-emitted by the
      // child from that cursor and deduped client-side by ts:work_id:to — so the
      // seam is lossless without double-showing a chip.
      let cursor = 0;
      try {
        const snap = await runJson(["stream", "--once", "--json"]);
        if (snap && typeof snap.cursor === "number") cursor = snap.cursor;
        const events = Array.isArray(snap?.events) ? snap.events : [];
        for (const raw of events) {
          const parsed = parseWorkEventLine(JSON.stringify(raw));
          if (parsed) send(sseFrame("transition", parsed, dedupeKey(parsed)));
        }
      } catch {
        cursor = 0; // snapshot failed: follow from the top; dedupe covers overlap
      }

      child = spawn(COMPOUND_BIN, ["stream", "--follow", "--cursor", String(cursor), "--json"], {
        env: consoleChildEnv({ COMPOUND_HOME }),
        stdio: ["ignore", "pipe", "ignore"],
      });

      const out = child.stdout;
      if (!out) {
        cleanup();
        return;
      }
      out.setEncoding("utf8");
      let buffer = "";
      out.on("data", (piece: string) => {
        buffer += piece;
        let nl = buffer.indexOf("\n");
        while (nl >= 0) {
          const line = buffer.slice(0, nl);
          buffer = buffer.slice(nl + 1);
          translateFrame(line, send);
          nl = buffer.indexOf("\n");
        }
      });
      // The child ending (exit / spawn error) is a real end of the live tail:
      // close the SSE so the client hook reconnects rather than hanging.
      child.on("error", cleanup);
      child.on("close", cleanup);

      // Client-facing heartbeat keeps the connection warm independent of the
      // child's own (slower) heartbeat frames, which we drop below.
      heartbeat = setInterval(() => send(`: heartbeat\n\n`), HEARTBEAT_MS);
    },
    cancel() {
      teardown();
    },
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}

/**
 * Translates one JSONL frame from `compound stream --follow` into the SSE
 * vocabulary the client hook speaks. work_event -> transition (re-normalized
 * through parseWorkEventLine so the wire stays the clean WorkEvent shape, SHA
 * fields stripped); seq_pulse -> memory-pulse. heartbeat/warning frames are the
 * CLI's concern (the route runs its own heartbeat and the ticker has no warning
 * channel), so they are dropped.
 */
function translateFrame(line: string, send: (chunk: string) => void): void {
  const trimmed = line.trim();
  if (!trimmed) return;
  let frame: { kind?: string; event?: unknown; seq?: unknown };
  try {
    frame = JSON.parse(trimmed);
  } catch {
    return; // never crash the tail on a partial/garbled frame
  }
  if (!frame || typeof frame !== "object") return;
  switch (frame.kind) {
    case "work_event": {
      const parsed = parseWorkEventLine(JSON.stringify(frame.event));
      if (parsed) send(sseFrame("transition", parsed, dedupeKey(parsed)));
      break;
    }
    case "seq_pulse":
      if (typeof frame.seq === "string") send(sseFrame("memory-pulse", { seq: frame.seq }));
      break;
    default:
      break; // heartbeat / warning: intentionally not forwarded
  }
}
