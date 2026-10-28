import Link from "next/link";

import { HeadlineAct } from "@/components/briefing/HeadlineAct";
import { DomainKicker } from "@/components/briefing/primitives";
import { FeedDegraded, feedRenderModel, type FeedProjectionStatus } from "@/components/kit";
import type { AttentionGroup } from "@/lib/attention";
import type { HeadlineActDescriptor } from "@/lib/briefing";
import { str } from "@/lib/compound";
import { headlineActIdentity } from "@/lib/governed-action-client";

/**
 * THE headline (redesign plan Part 3): the single highest-leverage gate stated
 * as one plain sentence, its consequence line, and its one-click governed act
 * inline. When something waits, the hero wears the admission-seam language
 * (DESIGN.md §0 signature B): a 3px amber left seam + a left-to-right amber
 * wash — scanning the left edge answers "what needs me". When nothing waits,
 * the empty state speaks doctrine, never "No data" (§7.9).
 *
 * Honest degradation (packet: console-honest-degradation): an `unreachable`
 * attention read ALWAYS renders FeedDegraded, even though `top` would already
 * be null in that case — never let the same "nothing here" shape carry two
 * different meanings (a dead feed vs. a genuinely clean seam).
 */
export function Headline({
  top,
  act,
  mutationsEnabled,
  runningAgents,
  classA = false,
  read,
}: {
  top: AttentionGroup | null;
  act: HeadlineActDescriptor | null;
  mutationsEnabled: boolean;
  runningAgents: number;
  /** any Class-A gate waiting → the hero glows amber (`--heat-seam`), else work-blue. */
  classA?: boolean;
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  const summary = str(top?.operator_summary);

  if (feedRenderModel(read.status, Boolean(top && summary)) === "degraded") {
    return (
      <section className="rounded-lg border border-hair bg-panel px-6 py-8 shadow-sm">
        <FeedDegraded label="Decide" source="compound attention --json" lastGoodAt={read.lastGoodAt} />
      </section>
    );
  }

  if (!top || !summary) {
    return (
      <section
        className="rounded-lg border border-hair bg-panel px-6 py-8 shadow-sm"
        style={{ borderLeftWidth: "3px", borderLeftColor: "var(--good-fg)" }}
      >
        <p className="max-w-prose text-xl font-medium leading-snug text-balance text-ink">
          Clean seam. Nothing waits on you.
        </p>
        <p className="mt-2 text-sm text-muted">
          {runningAgents > 0
            ? `${runningAgents} agent${runningAgents === 1 ? "" : "s"} working — propose-only holds the line.`
            : "Propose-only holds the line."}
        </p>
      </section>
    );
  }

  const unlock = str(top.operator_unlock);
  const decisionKey = str(top.key);
  // THE one heat glow of the briefing viewport (DESIGN.md §9.2/§9.4): a
  // bottom-rising domain ambience over the panel — amber when a Class-A gate
  // waits, work-blue otherwise. The 3px amber left seam (admission signature,
  // §0) stays as the scannable "needs me" edge; the glow is background-image so
  // it composes with the bg-panel color without a second competing gradient.
  return (
    <section
      className={`${classA ? "heat-seam" : "heat-work"} focus-hero rounded-xl border border-hair bg-panel px-6 py-7`}
      style={{
        borderLeftWidth: "3px",
        borderLeftColor: "var(--seam-fg)",
      }}
    >
      <DomainKicker area={str(top.area)} />
      <p className="mt-3 text-hero font-medium text-balance text-strong" style={{ maxWidth: "44ch" }}>
        {summary}
        {unlock ? (
          <>
            {" "}
            <strong className="font-semibold text-good">→ {unlock}.</strong>
          </>
        ) : null}
      </p>

      <div className="mt-5 flex flex-wrap items-center gap-x-4 gap-y-2">
        {act ? <HeadlineAct key={headlineActIdentity(act)} act={act} mutationsEnabled={mutationsEnabled} /> : null}
        <Link
          href={decisionKey ? `/reviews?decision=${encodeURIComponent(decisionKey)}` : "/reviews"}
          className="text-sm font-semibold text-work transition-colors hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
        >
          {act ? "Review in Decide →" : "Open in Decide →"}
        </Link>
        {top.count > 1 ? (
          <span className="text-xs tabular-nums text-faint">×{top.count.toLocaleString("en-US")} together</span>
        ) : null}
        <span className="mono truncate text-2xs text-faint" title={str(top.key)} style={{ maxWidth: "22ch" }}>
          {str(top.key)}
        </span>
      </div>
    </section>
  );
}
