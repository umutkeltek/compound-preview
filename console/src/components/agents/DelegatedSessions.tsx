import { ago } from "@/lib/time";

import {
  delegatedSessionViews,
  type DelegatedSessionCounts,
  type DelegatedSessionRow,
  type DelegatedTone,
} from "./executor-model";

/**
 * The delegated supervised-session board (Agents page). One safe row per work,
 * folded server-side from work.delegate.{spawned,steered,refused,exited} onto
 * `compound executors --json`, retaining the latest session per work+lane.
 * Compare → table doctrine: homogeneous columns
 * scanned top-to-bottom. Privacy law: this component only ever draws the
 * reduced fields the projection exposes — lane, session name, status, last
 * steer TIME, and the exact RunAttempt id/status. It NEVER shows prompt text,
 * pane content, argv, credentials, or provider env (none reach the row).
 */

const TONE_TEXT: Record<DelegatedTone, string> = {
  live: "text-work",
  done: "text-good",
  seam: "text-seam",
  faint: "text-faint",
};

const TONE_DOT: Record<DelegatedTone, string> = {
  live: "bg-work",
  done: "bg-good",
  seam: "bg-seam",
  faint: "bg-faint",
};

export function DelegatedSessions({
  rows,
  counts,
}: {
  rows: DelegatedSessionRow[];
  counts: DelegatedSessionCounts;
}) {
  const views = delegatedSessionViews(rows);
  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-baseline justify-between">
        <h2 className="text-sm font-bold uppercase tracking-wide text-muted">Delegated sessions</h2>
        <span className="mono text-2xs text-faint">
          running {counts.running} · exited {counts.exited} · refused {counts.refused} · unknown {counts.unknown}
        </span>
      </div>
      <p className="text-2xs text-faint">
        Experimental only: <span className="mono">compound work delegate --experimental-delegate-mutations</span> is unsupported in v0.1.
      </p>

      {views.length === 0 ? (
        <div className="rounded-lg border border-dashed border-hair bg-page px-6 py-10 text-center">
          <p className="text-sm text-muted">No delegated sessions yet.</p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-hair bg-panel">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-hair text-left text-2xs uppercase tracking-wide text-faint">
                <th className="px-3 py-2 font-bold">Status</th>
                <th className="px-3 py-2 font-bold">Lane</th>
                <th className="px-3 py-2 font-bold">Work</th>
                <th className="px-3 py-2 font-bold">Session</th>
                <th className="px-3 py-2 font-bold">Run attempt</th>
                <th className="px-3 py-2 font-bold">Progress</th>
                <th className="px-3 py-2 font-bold">Detail</th>
                <th className="px-3 py-2 font-bold">Last steer</th>
              </tr>
            </thead>
            <tbody>
              {views.map((v) => (
                <tr key={`${v.workId}:${v.lane}`} className="border-b border-hair/60 last:border-0">
                  <td className="px-3 py-2">
                    <span className={`inline-flex items-center gap-1.5 font-medium ${TONE_TEXT[v.tone]}`}>
                      <span className={`h-2 w-2 shrink-0 rounded-full ${TONE_DOT[v.tone]}`} aria-hidden />
                      {v.statusLabel}
                    </span>
                  </td>
                  <td className="px-3 py-2">
                    <span className="rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-medium text-muted">
                      {v.lane || "—"}
                    </span>
                  </td>
                  <td className="mono px-3 py-2 text-2xs text-muted">{v.workId}</td>
                  <td className="mono px-3 py-2 text-2xs text-muted">{v.session || "—"}</td>
                  <td className="mono px-3 py-2 text-2xs text-muted">{v.runAttemptLabel || "—"}</td>
                  <td className="mono px-3 py-2 text-2xs text-muted">{v.progressLabel || "—"}</td>
                  <td className="px-3 py-2 text-2xs text-muted">{v.detail || "—"}</td>
                  <td className="px-3 py-2 text-2xs text-faint">{v.lastSteerAt ? ago(v.lastSteerAt) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
