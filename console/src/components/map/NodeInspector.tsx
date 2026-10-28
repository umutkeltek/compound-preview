"use client";

import { CopyCmd } from "@/components/CopyCmd";
import { IdRef } from "@/components/kit";
import type { LensNode } from "@/lib/map/lens";

/*
 * Slide-over inspector for a clicked graph node. Non-modal (the canvas stays
 * interactive behind it), restyled onto the token system. Shows the REAL
 * object, a copyable `compound …` command (never auto-run — DEC-0021), proof
 * refs (work/receipt/transition ids via IdRef → traceable, never dead text),
 * and the projection sources this node came from — the truth contract.
 */

/** work_/receipt_/transition_/policy_/action_ ids are machine ids → IdRef. */
const MACHINE_ID = /^(work|rec|receipt|tr|transition|pol|pd|act|cl|cand|ev)[_:]/;

export function NodeInspector({ node, onClose }: { node: LensNode | null; onClose: () => void }) {
  if (!node) return null;

  return (
    <aside
      className="fixed right-0 top-0 z-40 flex h-screen flex-col gap-4 overflow-y-auto border-l border-hair bg-panel px-5 py-5 shadow-lg"
      style={{ width: "min(var(--drawer-w), 94vw)" }}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="mono text-2xs font-semibold uppercase tracking-wide text-faint">{node.kind}</div>
          <h2 className="mt-0.5 text-lg font-semibold leading-tight text-ink">{node.label}</h2>
          {node.sublabel ? <div className="mono mt-0.5 text-2xs text-faint">{node.sublabel}</div> : null}
        </div>
        <button
          onClick={onClose}
          aria-label="Close"
          className="shrink-0 rounded-md border border-hair px-2 py-1 text-xs font-semibold text-muted hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
        >
          Close
        </button>
      </div>

      <div className="flex flex-wrap items-center gap-1.5">
        {node.status ? (
          <span className="inline-flex items-center rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-semibold text-muted">
            {node.status}
          </span>
        ) : null}
        {node.group ? (
          <span className="inline-flex items-center rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-semibold text-muted">
            {node.group}
          </span>
        ) : null}
        <IdRef id={node.id} />
      </div>

      {node.cli ? (
        <div>
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">Command · copy, never auto-run</div>
          <div className="mt-1.5">
            <CopyCmd cmd={node.cli} />
          </div>
        </div>
      ) : null}

      {node.proofRefs?.length ? (
        <div>
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">Proof refs</div>
          <ul className="mt-1.5 flex flex-col gap-1">
            {node.proofRefs.map((ref) => (
              <li key={ref}>
                {MACHINE_ID.test(ref) ? (
                  <IdRef id={ref} />
                ) : (
                  <span className="mono text-2xs text-faint" title={ref}>
                    {ref}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div>
        <div className="text-2xs font-bold uppercase tracking-wide text-faint">Projection source</div>
        <ul className="mt-1.5 flex flex-col gap-1">
          {node.sources.map((s, i) => (
            <li key={i} className="mono text-2xs leading-snug text-faint">
              {s}
            </li>
          ))}
        </ul>
      </div>

      <div className="min-h-0">
        <div className="text-2xs font-bold uppercase tracking-wide text-faint">Raw object</div>
        <pre
          className="mono mt-1.5 overflow-auto rounded-md border border-hair bg-soft p-3 text-2xs leading-snug text-muted"
          style={{ maxHeight: "40vh" }}
        >
          {JSON.stringify(node.raw ?? {}, null, 2)}
        </pre>
      </div>
    </aside>
  );
}
