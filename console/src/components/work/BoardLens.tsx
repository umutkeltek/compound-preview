"use client";

import { EmptyState } from "@/components/kit/EmptyState";
import { WorkBoardCard } from "@/components/work/WorkBoardCard";
import type { BoardColumnModel } from "@/lib/work-pipeline";

/**
 * Board lens — the by-state kanban, kept as a secondary view and restyled onto
 * kit components. Columns are scanned by state; each card opens the same Trace
 * drawer. `moves → board` (DESIGN.md §4).
 */
export function BoardLens({
  columns,
  onOpenTrace,
}: {
  columns: BoardColumnModel[];
  onOpenTrace: (workId: string) => void;
}) {
  return (
    <div className="overflow-x-auto pb-2">
      <div className="flex min-w-max gap-4">
        {columns.map((column) => (
          <div key={column.key} className="flex w-72 shrink-0 flex-col gap-3">
            <div className="work-seam flex items-center gap-2 rounded-sm border-b border-hair pb-2 pl-2" data-tone={column.tone}>
              <div className="min-w-0">
                <h3 className="text-sm font-semibold text-ink">{column.label}</h3>
                <p className="text-2xs text-faint">{column.description}</p>
              </div>
              <span className="mono ml-auto text-2xs tabular-nums text-faint">{column.cards.length}</span>
            </div>
            {column.cards.length ? (
              <div className="flex flex-col gap-2">
                {column.cards.map((card) => (
                  <WorkBoardCard key={`${column.key}:${card.key}`} card={card} onOpen={onOpenTrace} />
                ))}
              </div>
            ) : (
              <EmptyState>{column.emptyCopy}</EmptyState>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
