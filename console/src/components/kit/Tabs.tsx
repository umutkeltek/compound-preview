"use client";

import * as RadixTabs from "@radix-ui/react-tabs";
import type { ReactNode } from "react";

/**
 * Segmented tab control (DESIGN.md §8 — Radix Tabs, behavior-only, styled with
 * tokens). Used where one surface re-projects under several lenses (the
 * Capabilities graph: Registry | Runtime). Renders as a compact segmented
 * control; `TabsContent` is optional — when the panels below share one canvas
 * the List alone drives selection with correct roving-tabindex + aria.
 *
 * Tokens only — no raw palette, no arbitrary px.
 */
export function Tabs({
  value,
  onValueChange,
  children,
  className = "",
}: {
  value: string;
  onValueChange: (value: string) => void;
  children: ReactNode;
  className?: string;
}) {
  return (
    <RadixTabs.Root value={value} onValueChange={onValueChange} className={className}>
      {children}
    </RadixTabs.Root>
  );
}

export function TabsList({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <RadixTabs.List
      aria-label={label}
      className="inline-flex items-center gap-0.5 rounded-md border border-hair bg-soft p-0.5"
    >
      {children}
    </RadixTabs.List>
  );
}

export function TabsTrigger({
  value,
  children,
  count,
}: {
  value: string;
  children: ReactNode;
  count?: number;
}) {
  return (
    <RadixTabs.Trigger
      value={value}
      className="inline-flex min-h-11 touch-manipulation items-center gap-1.5 rounded-sm px-3 py-1 text-xs font-semibold text-muted
                 transition-colors hover:text-ink
                 data-[state=active]:bg-panel data-[state=active]:text-ink data-[state=active]:shadow-sm
                 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
    >
      {children}
      {count != null ? <span className="mono tabular-nums text-2xs text-faint">{count}</span> : null}
    </RadixTabs.Trigger>
  );
}

export function TabsContent({ value, children }: { value: string; children: ReactNode }) {
  return (
    <RadixTabs.Content
      value={value}
      className="focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
    >
      {children}
    </RadixTabs.Content>
  );
}
