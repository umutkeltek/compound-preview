"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";

import { Button } from "@/components/kit";

/**
 * Seeds the embedded memory ego-graph. Navigates to /memory?graph=<seed>
 * (server re-projects the graph for the new seed and re-opens the panel) — the
 * same URL a candidate card's "Show in graph" affordance targets, so both entry
 * points share one code path.
 */
export function GraphSeedSearch({ initial }: { initial: string }) {
  const router = useRouter();
  const [q, setQ] = useState(initial);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const seed = q.trim();
    if (seed) router.push(`/memory?graph=${encodeURIComponent(seed)}#memory-graph`);
  }

  return (
    <form onSubmit={submit} className="flex gap-2">
      <input
        value={q}
        onChange={(e) => setQ(e.target.value)}
        aria-label="Graph seed"
        placeholder="seed: an entity slug (example-os, knowledge:arxiv:…) or a claim id (cl_…)"
        className="min-w-0 flex-1 rounded-md border border-hair bg-panel px-3 py-2 text-sm text-ink outline-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
      />
      <Button type="submit" variant="primary">
        Show
      </Button>
    </form>
  );
}
