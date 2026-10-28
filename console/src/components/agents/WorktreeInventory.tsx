import { arr, str, type Json, type ProjectionRead } from "@/lib/compound";
import { FeedDegraded } from "@/components/kit/FeedDegraded";
import { FreshnessChip } from "@/components/kit";

/** Render the existing inventory owner; classification is never recomputed here. */
export function WorktreeInventory({ repo, read }: { repo: string | null; read: ProjectionRead | null }) {
  const report = read?.value;
  const valid = report?.schema_version === "compound.worktree_inventory.v0.1" && Array.isArray(report.rows) &&
    report.rows.every((row: Json | null) => row && typeof row.path === "string" && typeof row.class === "string");
  // The inventory owner can retain zero-value ints when Git probes fail.
  // Only these classes guarantee both count probes completed; main never
  // compares itself for unlanded commits. Live/probe classes short-circuit.
  const countsKnown = (row: Json) => ["landed_clean", "stalled_unlanded"].includes(str(row.class));
  const count = (value: unknown) => Number.isInteger(value) && Number(value) >= 0 ? String(value) : "unknown";
  return (
    <section className="rounded-lg border border-hair bg-panel p-5" aria-label="Branches and worktrees">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-md font-semibold text-ink">Branches &amp; worktrees</h2>
        <FreshnessChip generatedAt={valid && read?.status === "live" ? read.generatedAt ?? undefined : undefined} source="compound executors worktrees --repo PATH --json" />
      </div>
      <p className="mb-3 text-sm text-muted">
        Inventory only. Landed does not mean disposable: ownership, recovery proof and exact retirement review still apply.
      </p>
      {!repo || !read ? (
        <p className="text-sm text-muted">Choose a repository with an absolute COMPOUND_WORKTREE_REPO when starting the console. No repository was inferred from the dashboard checkout.</p>
      ) : read.status === "unreachable" || !valid ? (
        <FeedDegraded label="Worktree inventory" source="compound executors worktrees --repo PATH --json" lastGoodAt={read.lastGoodAt} />
      ) : (
        <>
          <p className="mono mb-3 break-all text-xs text-muted">{str(report.repo) || repo}</p>
          {read.status === "stale" ? <p className="mb-3 text-sm text-seam">Last-known inventory. Recheck before changing any branch or worktree.</p> : null}
          {report.keeps_readable === false ? <p className="mb-3 text-sm text-seam">Keep decisions are unreadable. Absence of a keep marker is not permission to retire.</p> : null}
          {!report.rows.length ? <p className="text-sm text-muted">No worktrees in this reported inventory.</p> : (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead><tr className="border-b border-hair text-faint">
                  <th className="p-2">Branch / checkout</th><th className="p-2">State</th>
                  <th className="p-2">Dirty / unlanded</th><th className="p-2">Work / liveness</th><th className="p-2">Next action</th>
                </tr></thead>
                <tbody>{arr(report.rows).map((row: Json, index) => (
                  <tr key={str(row.path) || index} className="border-b border-hair align-top text-muted">
                    <td className="p-2"><span className="mono text-ink">{str(row.branch) || "detached"}</span><div className="break-all">{str(row.path)}</div></td>
                    <td className="p-2">{str(row.class) || "unknown"}{row.kept === true ? " · kept" : ""}{row.locked === true ? " · locked" : ""}</td>
                    <td className="mono p-2">{countsKnown(row) || row.class === "main" ? count(row.dirty_files) : "unknown"} / {row.class === "main" ? "not compared" : countsKnown(row) ? count(row.unlanded_commits) : "unknown"}</td>
                    <td className="p-2">{str(row.work_id) || (arr(row.work_candidates).length ? "multiple owners" : "unbound")}<div>{str((row.liveness as Json)?.state) || "unknown"}</div></td>
                    <td className="p-2">{str(row.next_action) || "Inspect before acting"}<div className="text-faint">{str(row.reason)}</div></td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  );
}
