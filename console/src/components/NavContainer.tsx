import { Nav } from "@/components/Nav";
import { attentionNeedsYouGroups, getAttention } from "@/lib/attention";

/**
 * Server wrapper for Nav: reads badge counts from the ONE attention projection
 * (`compound attention --json`) through the existing runJson TTL memo.
 *
 * Phase 2 cutover: the badge — and EVERY other "what needs you" consumer
 * (briefing headline + Decide list, Reviews band ordering) — MUST read this
 * same projection. Do not reintroduce a second client-side "needs me"
 * heuristic; that divergence is the drift this phase deleted.
 *
 * - Decide badge = `counts.needs_you_groups` (system-wide, all sources).
 * - Work badge = sum of counts over needs-you groups sourced from the
 *   workbench. This is identical today to the old `workbench.counts.needs_claim`
 *   (both = 3 live), but it is now read off the single projection so there is
 *   one source of truth instead of two fetches that could drift.
 */
export async function NavContainer({ variant = "desktop" }: { variant?: "desktop" | "mobile" }) {
  const attention = await getAttention();
  const decideCount = attention?.counts?.needs_you_groups ?? 0;
  const workCount = attentionNeedsYouGroups(attention)
    .filter((group) => group.source === "workbench")
    .reduce((sum, group) => sum + group.count, 0);
  return <Nav decideCount={decideCount} workCount={workCount} variant={variant} />;
}
