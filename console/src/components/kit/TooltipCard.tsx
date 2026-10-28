"use client";

import * as HoverCard from "@radix-ui/react-hover-card";
import type { ReactNode } from "react";

import { type Tone, TONE_FILL } from "@/components/kit/tone";

/**
 * TooltipCard (DESIGN.md §9.3) — the rich hover surface every chart mark,
 * dot-matrix cell and capsule answers with. Built on Radix HoverCard, NOT
 * Tooltip: HoverCard is the primitive for supplementary rich content
 * (title + series rows + a derived line), with correct focus/pointer-into-card
 * behavior; Radix Tooltip is `role="tooltip"` for a short string label and
 * closes when you reach into it. Hover reveals depth, never hides essentials
 * (§9.4) — the trigger must stand on its own; this only rewards curiosity.
 *
 * Content: panel surface, --shadow-lg, radius-md, 12px pad; a title row → series
 * rows (dot + label + mono value) → hairline → derived line (delta / margin).
 * Pass `rows`/`derived` for the standard shape, or `children` to render bespoke
 * content inside the same styled shell.
 */
export type TooltipRow = { label: ReactNode; value: ReactNode; tone?: Tone };

export function TooltipCard({
  trigger,
  title,
  rows,
  derived,
  children,
  side = "top",
  align = "center",
  openDelay = 120,
  closeDelay = 60,
}: {
  trigger: ReactNode;
  title?: ReactNode;
  rows?: TooltipRow[];
  derived?: { label: ReactNode; value: ReactNode };
  children?: ReactNode;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  openDelay?: number;
  closeDelay?: number;
}) {
  return (
    <HoverCard.Root openDelay={openDelay} closeDelay={closeDelay}>
      <HoverCard.Trigger asChild>{trigger}</HoverCard.Trigger>
      <HoverCard.Portal>
        <HoverCard.Content
          side={side}
          align={align}
          sideOffset={6}
          collisionPadding={8}
          className="hovercard-in z-50 w-max max-w-72 rounded-md border border-hair bg-panel p-3 text-xs shadow-lg"
        >
          {children ?? (
            <div className="flex flex-col gap-1.5">
              {title != null ? <div className="font-semibold leading-snug text-ink">{title}</div> : null}
              {rows?.length ? (
                <div className="flex flex-col gap-1">
                  {rows.map((row, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <span
                        aria-hidden
                        className={`h-1.5 w-1.5 shrink-0 rounded-full ${TONE_FILL[row.tone ?? "neutral"]}`}
                      />
                      <span className="text-muted">{row.label}</span>
                      <span className="mono ml-auto tabular-nums text-ink">{row.value}</span>
                    </div>
                  ))}
                </div>
              ) : null}
              {derived != null ? (
                <div className="mt-1 flex items-center gap-2 border-t border-hair pt-1.5 text-2xs text-faint">
                  <span>{derived.label}</span>
                  <span className="mono ml-auto tabular-nums">{derived.value}</span>
                </div>
              ) : null}
            </div>
          )}
          <HoverCard.Arrow className="fill-hair" />
        </HoverCard.Content>
      </HoverCard.Portal>
    </HoverCard.Root>
  );
}
