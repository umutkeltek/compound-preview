"use client";

import type { KeyboardEvent } from "react";

import { IdRef, SegmentedProgress, StateBadge } from "@/components/kit";
import type { BoardCard } from "@/lib/work-pipeline";

/**
 * One work card on the pipeline / board (DESIGN.md §3 work-card). The left
 * seam encodes STATE (the one place seam == state, because a board is scanned
 * by state) via the token-driven `.work-seam[data-tone]` class. A traceable
 * card (real work_id) is the Trace-drawer trigger: click OR Enter/Space opens
 * it, with a focus-visible ring. Pure suggestions (director proposals, no
 * work_id yet) render as static cards — honest: there is nothing to trace.
 */
export function WorkBoardCard({
  card,
  onOpen,
  compact = false,
}: {
  card: BoardCard;
  onOpen: (workId: string) => void;
  compact?: boolean;
}) {
  const interactive = card.traceable;

  function open() {
    if (interactive) onOpen(card.workId);
  }
  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (!interactive) return;
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      onOpen(card.workId);
    }
  }

  return (
    <div
      className={`work-seam group flex flex-col gap-2 rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm transition-shadow ${
        interactive ? "cursor-pointer hover:shadow" : ""
      }`}
      data-tone={card.tone}
      role={interactive ? "button" : undefined}
      tabIndex={interactive ? 0 : undefined}
      aria-label={interactive ? `Open trace for ${card.title}` : undefined}
      onClick={open}
      onKeyDown={onKeyDown}
    >
      <div className="flex items-start justify-between gap-2">
        {/* Clamped (DESIGN.md work-card anatomy): a full predicate as title in a
            narrow pipeline column otherwise wraps one word per line. */}
        <span className="min-w-0 line-clamp-3 text-sm font-semibold leading-snug text-ink" title={card.title}>
          {card.title}
        </span>
        <span className="shrink-0">
          <StateBadge state={card.state} />
        </span>
      </div>

      {card.note ? (
        <p className={`text-xs leading-snug text-muted ${compact ? "line-clamp-2" : ""}`}>{card.note}</p>
      ) : null}

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-2xs text-faint">
        {card.subtitle ? <span className="truncate font-medium text-muted">{card.subtitle}</span> : null}
        {card.ageLabel ? <span className="tabular-nums">{card.ageLabel}</span> : null}
        {card.moveType ? <span>{card.moveType.replace(/_/g, " ")}</span> : null}
      </div>

      {card.displayId ? (
        <IdRef id={card.displayId} kind={card.traceable ? "work" : "plan"} plain truncate />
      ) : null}

      {/* Envelope-stage position (§9.3) — where this item sits in the 7-stage
          spine; the current stage breathes only when genuinely running. */}
      <SegmentedProgress
        className="mt-0.5"
        total={card.progress.total}
        done={card.progress.done}
        activeIndex={card.progress.active}
        pulse={card.running}
      />

      {card.running ? <div className="runbar" aria-hidden /> : null}
    </div>
  );
}
