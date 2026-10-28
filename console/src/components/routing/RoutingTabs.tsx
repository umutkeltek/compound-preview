"use client";

import * as Tabs from "@radix-ui/react-tabs";
import type { ReactNode } from "react";

/**
 * The analytical shelf beneath the promotion band: Lanes (the roster) ·
 * Traffic & cost · Bench. Radix Tabs for correct roving-focus/aria (DESIGN.md
 * §8, behavior-only); the panels themselves are server-rendered and passed in
 * as props so the tables/bars stay on the server. Tokened triggers — a work-blue
 * active underline, muted rest; no palette color in markup (§7).
 */

function Trigger({ value, children }: { value: string; children: ReactNode }) {
  return (
    <Tabs.Trigger
      value={value}
      className="-mb-px border-b-2 border-transparent px-3 py-2 text-sm font-medium text-muted transition-colors data-[state=active]:border-work data-[state=active]:text-ink hover:text-ink focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-work"
    >
      {children}
    </Tabs.Trigger>
  );
}

export function RoutingTabs({
  lanes,
  traffic,
  bench,
}: {
  lanes: ReactNode;
  traffic: ReactNode;
  bench: ReactNode;
}) {
  return (
    <Tabs.Root defaultValue="lanes" className="flex flex-col gap-5">
      <Tabs.List className="flex flex-wrap gap-1 border-b border-hair" aria-label="Routing analytics">
        <Trigger value="lanes">Lanes</Trigger>
        <Trigger value="traffic">Traffic &amp; cost</Trigger>
        <Trigger value="bench">Bench</Trigger>
      </Tabs.List>
      <Tabs.Content value="lanes" className="focus-visible:outline-none">
        {lanes}
      </Tabs.Content>
      <Tabs.Content value="traffic" className="focus-visible:outline-none">
        {traffic}
      </Tabs.Content>
      <Tabs.Content value="bench" className="focus-visible:outline-none">
        {bench}
      </Tabs.Content>
    </Tabs.Root>
  );
}
