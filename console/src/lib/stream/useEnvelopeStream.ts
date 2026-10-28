"use client";

import { useEffect, useRef, useState } from "react";

import { dedupeKey, type WorkEvent } from "@/lib/stream/tail-parser";

/*
 * Client half of the live envelope-spine stream (/api/stream). EventSource
 * with MANUAL exponential-backoff reconnect (not the browser's fixed ~3s
 * retry) so the UI can show a real "connecting / live / degraded" state
 * instead of silently retrying forever. Never throws into the caller: a
 * stream that never connects (or a backend not exposing raw fallbacks) just
 * means `events` stays at whatever it already had — the caller renders its
 * existing empty/snapshot state, never a blank view.
 *
 * Dedupe is by ts+work_id+to (tail-parser's dedupeKey), tracked in a Set that
 * OUTLIVES reconnects (only reset on unmount) — a reconnect replays the
 * server's backlog burst, and because an already-seen key is never re-added
 * to `events`, its chip is never re-mounted and therefore never re-animates.
 */

export type StreamStatus = "connecting" | "live" | "degraded";

export type MemoryPulse = { seq: string; at: number };

const MAX_EVENTS = 30;
const BACKOFF_START_MS = 1_000;
const BACKOFF_MAX_MS = 20_000;

export function useEnvelopeStream() {
  const [events, setEvents] = useState<WorkEvent[]>([]);
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const [memoryPulse, setMemoryPulse] = useState<MemoryPulse | null>(null);

  const seenRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let cancelled = false;
    let source: EventSource | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let backoffMs = BACKOFF_START_MS;
    let everConnected = false;

    function connect() {
      if (cancelled) return;

      let es: EventSource;
      try {
        es = new EventSource("/api/stream");
      } catch {
        // EventSource unsupported/unavailable: degrade quietly, no reconnect
        // loop against a construction that will never succeed.
        setStatus("degraded");
        return;
      }
      source = es;

      es.addEventListener("open", () => {
        if (cancelled) return;
        everConnected = true;
        backoffMs = BACKOFF_START_MS;
        setStatus("live");
      });

      es.addEventListener("ready", (event) => {
        if (cancelled) return;
        try {
          const payload = JSON.parse((event as MessageEvent).data) as { streaming?: boolean };
          if (payload.streaming === false) setStatus("degraded");
        } catch {
          /* malformed ready payload: ignore, connection state still governs status */
        }
      });

      es.addEventListener("transition", (event) => {
        if (cancelled) return;
        try {
          const parsed = JSON.parse((event as MessageEvent).data) as WorkEvent;
          const key = dedupeKey(parsed);
          if (seenRef.current.has(key)) return; // reconnect replay of a chip already shown
          seenRef.current.add(key);
          setEvents((prev) => [parsed, ...prev].slice(0, MAX_EVENTS));
        } catch {
          /* malformed event on the wire: skip it, never crash the stream */
        }
      });

      es.addEventListener("memory-pulse", (event) => {
        if (cancelled) return;
        try {
          const payload = JSON.parse((event as MessageEvent).data) as { seq?: string };
          setMemoryPulse({ seq: payload.seq ?? "", at: Date.now() });
        } catch {
          /* ignore malformed pulse */
        }
      });

      es.onerror = () => {
        if (cancelled) return;
        es.close();
        setStatus(everConnected ? "connecting" : "degraded");
        const wait = Math.min(backoffMs, BACKOFF_MAX_MS);
        backoffMs = Math.min(backoffMs * 2, BACKOFF_MAX_MS);
        retryTimer = setTimeout(connect, wait);
      };
    }

    connect();

    return () => {
      cancelled = true;
      source?.close();
      if (retryTimer) clearTimeout(retryTimer);
    };
  }, []);

  return { events, status, memoryPulse };
}
