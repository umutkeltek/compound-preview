/**
 * The v1 shape vocabulary (settled decision #2). Six evidence-shapes, each a
 * pure server-renderable component that SELF-DECLARES its form —
 * "shape: X — chosen because …" — like the reference sim. Shapes hold no state
 * and reach no act client: an actionable evidence renders a plain semantic
 * <button> carrying the act as `data-act-*`, and a consumer OUTSIDE the cluster
 * turns that into a governed ActRequest. That keeps the affordance real and
 * reachable in the accessibility tree (FR4) with its consequence + reversibility
 * stated on the button itself (pf4).
 */
import { Sparkline as KitSparkline, StateBadge } from "@/components/kit";

import { boardHeadline } from "./laws";
import type { Evidence, EvidenceAction, ShapeName, ShapeRenderer } from "./types";

/* ---------- shared building blocks ---------- */

function ShapeTag({ name, reason }: { name: ShapeName; reason: string }) {
  return (
    <p className="mt-2 text-2xs text-faint" data-shape={name}>
      shape: {name} — chosen because {reason}
    </p>
  );
}

function Subject({ evidence }: { evidence: Evidence }) {
  return (
    <div className="flex items-center gap-2">
      <span className="text-sm font-semibold text-strong">{evidence.subject || evidence.id}</span>
      {evidence.state ? <StateBadge state={evidence.state} /> : null}
    </div>
  );
}

/**
 * A self-explaining act affordance. A REAL <button> (a11y-reachable, FR4) that
 * states what it does and whether it can be undone (pf4). It carries the act as
 * data attributes only — it does not, and cannot, submit it.
 */
function ActAffordance({ action }: { action: EvidenceAction }) {
  return (
    <button
      type="button"
      data-act-area={action.area}
      data-act-item={action.itemId}
      data-act-id={action.actionId}
      data-act-effect={action.effect}
      data-act-confirm={String(action.requiresConfirmation)}
      className="mt-3 flex w-full flex-col items-start gap-0.5 rounded-md border border-hair bg-soft-2 px-3 py-2 text-left"
    >
      <span className="text-sm font-semibold text-strong">{action.label}</span>
      <span className="text-xs text-muted">{action.consequence}</span>
      <span className="text-2xs text-faint">
        {action.reversibility === "reversible" ? "can be undone" : "cannot be undone"}
        {action.requiresConfirmation ? " · asks you to confirm first" : ""}
      </span>
    </button>
  );
}

function maybeAction({ evidence }: { evidence: Evidence }) {
  return evidence.action ? <ActAffordance action={evidence.action} /> : null;
}

function Frame({ evidence, reason, name, children }: { evidence: Evidence; reason: string; name: ShapeName; children: React.ReactNode }) {
  return (
    <article className="rounded-lg border border-hair bg-soft p-4" data-representation={name} data-evidence-id={evidence.id}>
      <Subject evidence={evidence} />
      <div className="mt-2">{children}</div>
      {maybeAction({ evidence })}
      <ShapeTag name={name} reason={reason} />
    </article>
  );
}

/* ---------- the six shapes ---------- */

const Story: ShapeRenderer = ({ evidence, reason }) => (
  <Frame evidence={evidence} reason={reason} name="story">
    <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
      {evidence.summary || evidence.success_predicate || "No detail on this item yet."}
    </p>
  </Frame>
);

const SplitTimeline: ShapeRenderer = ({ evidence, reason }) => (
  <Frame evidence={evidence} reason={reason} name="split-timeline">
    <div className="grid grid-cols-2 gap-3">
      <div className="rounded-md border border-hair px-3 py-2">
        <p className="text-2xs uppercase tracking-wide text-faint">expected</p>
        <p className="text-sm text-muted">{evidence.expected_state}</p>
      </div>
      <div className="rounded-md border border-seam-line bg-seam-soft px-3 py-2">
        <p className="text-2xs uppercase tracking-wide text-faint">observed</p>
        <p className="text-sm text-seam">{evidence.observed_state}</p>
      </div>
    </div>
  </Frame>
);

const Heartbeat: ShapeRenderer = ({ evidence, reason }) => {
  const headline = boardHeadline(evidence);
  return (
    <Frame evidence={evidence} reason={reason} name="heartbeat">
      {/* claim-styled: dashed, because no receipts back it (pf3). */}
      <div className={headline.claimStyled ? "rounded-md border border-dashed border-hair px-3 py-2" : "rounded-md border border-hair px-3 py-2"}>
        {evidence.success_predicate ? (
          <p className="text-sm text-muted">
            <span className="text-2xs uppercase tracking-wide text-faint">goal</span>{" "}
            {evidence.success_predicate}
          </p>
        ) : (
          <p className="text-sm text-muted">{evidence.summary || headline.text}</p>
        )}
        <p className="mt-1 text-2xs text-faint">last receipt: {headline.lastReceiptAge}</p>
      </div>
    </Frame>
  );
};

const Collision: ShapeRenderer = ({ evidence, reason }) => {
  const [a, b] = evidence.statements ?? ["", ""];
  return (
    <Frame evidence={evidence} reason={reason} name="collision">
      <div className="flex flex-col gap-1">
        <p className="rounded-md border border-hair px-3 py-1.5 text-sm text-muted">{a}</p>
        <p className="text-center text-2xs uppercase tracking-widest text-crit">cancels</p>
        <p className="rounded-md border border-crit-line bg-crit-soft px-3 py-1.5 text-sm text-crit">{b}</p>
      </div>
    </Frame>
  );
};

const AsciiPipe: ShapeRenderer = ({ evidence, reason }) => {
  const flow = evidence.flow ?? { nodes: [] };
  const line = flow.nodes.join("  →  ");
  const changed = flow.changed;
  return (
    <Frame evidence={evidence} reason={reason} name="ascii-pipe">
      <pre className="overflow-x-auto rounded-md border border-hair bg-soft-2 px-3 py-2 text-xs text-muted">{line}</pre>
      {changed ? (
        <p className={changed.gained ? "mt-1 text-2xs text-good" : "mt-1 text-2xs text-crit"}>
          {changed.gained ? "+ gained" : "− lost"} edge {changed.edge[0]} → {changed.edge[1]}
        </p>
      ) : null}
    </Frame>
  );
};

const SparklineShape: ShapeRenderer = ({ evidence, reason }) => {
  const series = (evidence.series ?? []).filter((v) => Number.isFinite(v));
  const last = series.length ? series[series.length - 1] : null;
  return (
    <Frame evidence={evidence} reason={reason} name="sparkline">
      <div className="flex items-center gap-3">
        <span className="text-work">
          <KitSparkline values={series} />
        </span>
        <div>
          <p className="text-sm font-semibold tabular-nums text-strong">{last ?? "—"}</p>
          {evidence.series_label ? <p className="text-2xs text-faint">{evidence.series_label}</p> : null}
        </div>
      </div>
    </Frame>
  );
};

/** The registry consumes THIS map (one-directional: registry -> shapes). */
export const SHAPES: Record<ShapeName, ShapeRenderer> = {
  "split-timeline": SplitTimeline,
  heartbeat: Heartbeat,
  collision: Collision,
  "ascii-pipe": AsciiPipe,
  sparkline: SparklineShape,
  story: Story,
};
