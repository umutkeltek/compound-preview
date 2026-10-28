"use client";

import { EmptyState, PulseIcon, SectionHeader, StatusChip } from "@/components/kit";
import { MOVE_TYPE_LABELS, TERM_GLOSSARY, movePlain } from "@/lib/glossary";
import { dedupeKey, extractMoveType, type WorkEvent } from "@/lib/stream/tail-parser";
import { useEnvelopeStream, type StreamStatus } from "@/lib/stream/useEnvelopeStream";
import { ago } from "@/lib/time";
import { WORK_COPY } from "@/lib/work-copy";

/**
 * Transitions ticker (DESIGN.md §9.6) — "the console's spectacle is the
 * organism visibly living." One chip per envelope move, streaming in over
 * /api/stream with a slide-in ONLY on a genuinely new event (see
 * useEnvelopeStream's dedupe contract). Plain-language pairing per the
 * operator-legibility standard (matches PipelineLens's voice): every chip
 * shows a human label first, the raw state/move_type only as the mono id
 * underneath, never surfaced bare.
 */

const STATUS_COPY: Record<StreamStatus, { label: string; tone: "work" | "muted" | "seam" }> = {
  live: { label: "live", tone: "work" },
  connecting: { label: "connecting…", tone: "muted" },
  degraded: { label: "offline — showing last known", tone: "seam" },
};

function chipCopy(event: WorkEvent): { label: string; plain: string } {
  const moveType = extractMoveType(event.note);
  if (moveType) {
    return { label: MOVE_TYPE_LABELS[moveType] ?? moveType.replace(/_/g, " "), plain: movePlain(moveType) };
  }
  return {
    label: event.to.replace(/_/g, " ") || "moved",
    plain: TERM_GLOSSARY[event.to] ?? "Moved forward in its lifecycle.",
  };
}

export function TransitionsTicker({ onOpenTrace }: { onOpenTrace: (workId: string) => void }) {
  const { events, status } = useEnvelopeStream();
  const statusCopy = STATUS_COPY[status];

  return (
    <section className="rounded-xl bg-panel p-4 shadow-sm">
      <SectionHeader
        tone="work"
        icon={<PulseIcon />}
        label="Transitions"
        sub="live moves, streaming in"
        right={<StatusChip tone={statusCopy.tone} label={statusCopy.label} pulse={status === "live"} />}
      />

      {events.length ? (
        <ul className="mt-3 flex flex-col gap-1.5">
          {events.map((event) => {
            const copy = chipCopy(event);
            return (
              <li key={dedupeKey(event)} className="chip-in">
                <button
                  type="button"
                  onClick={() => onOpenTrace(event.work_id)}
                  className="flex w-full items-center gap-2 rounded-md border border-hair bg-soft px-2.5 py-1.5 text-left transition-shadow hover:bg-soft-2 hover:shadow"
                >
                  <span className="capitalize shrink-0 text-xs font-semibold text-ink">{copy.label}</span>
                  <span className="truncate text-2xs text-muted">{copy.plain}</span>
                  <span className="mono ml-auto shrink-0 text-2xs tabular-nums text-faint">{ago(event.ts)}</span>
                </button>
              </li>
            );
          })}
        </ul>
      ) : (
        <div className="mt-3">
          <EmptyState>
            {status === "degraded" ? WORK_COPY.transitions.offline : WORK_COPY.transitions.empty}
          </EmptyState>
        </div>
      )}
    </section>
  );
}
