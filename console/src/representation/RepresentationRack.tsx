/**
 * The instrument rack — the cluster's public mount. A pure server component: it
 * takes already-normalized `Evidence[]` and composes each through the frozen
 * interface (`renderRepresentation`). It fetches nothing itself, so it stays
 * testable and reaches no projection or act client; the consumer route maps a
 * live projection into `Evidence[]` and hands it here.
 */
import { Fragment } from "react";

import { renderRepresentation } from "./registry";
import type { Evidence } from "./types";

export function RepresentationRack({ evidence }: { evidence: Evidence[] }) {
  if (evidence.length === 0) {
    return <p className="text-sm text-muted">Nothing to represent right now.</p>;
  }
  return (
    <div className="grid gap-4" style={{ gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))" }}>
      {evidence.map((ev) => {
        const result = renderRepresentation(ev);
        return (
          <Fragment key={ev.id}>
            {result.ok ? (
              result.element
            ) : (
              <div className="rounded-lg border border-crit-line bg-crit-soft p-3 text-xs text-crit">{result.error}</div>
            )}
          </Fragment>
        );
      })}
    </div>
  );
}
