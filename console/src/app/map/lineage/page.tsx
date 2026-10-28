import { LineageSearch } from "@/components/map/LineageSearch";
import { Card, FreshnessChip } from "@/components/ui";

export const dynamic = "force-dynamic";

export default function LineageIndex() {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-end justify-between gap-4">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">Lineage</h1>
          <p className="mt-0.5 text-[12.5px] text-ink-3">
            How a datum arrived and what it feeds — source → evidence → candidate → adjudication → claim → outward.
          </p>
        </div>
        <FreshnessChip source="compound system lineage --json" />
      </div>

      <Card>
        <p className="mb-3 text-[12.5px] text-ink-2">
          Paste any id from the system — a memory candidate (<code className="mono">cand_…</code>), a claim
          (<code className="mono">cl_…</code> / <code className="mono">claim_…</code> /{" "}
          <code className="mono">DEC-…</code>), evidence (<code className="mono">ev_…</code>), work
          (<code className="mono">work_…</code>), or an entity token — to trace its full path.
        </p>
        <LineageSearch />
      </Card>
    </div>
  );
}
