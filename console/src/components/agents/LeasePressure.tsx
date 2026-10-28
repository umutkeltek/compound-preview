import { CapsuleColumn, ClockIcon, SectionHeader } from "@/components/kit";
import { TONE_TEXT } from "@/components/kit/tone";

import { leasePressureBars, type ExecutorRow } from "./executor-model";

/**
 * Lease pressure (DESIGN.md §9.3 CapsuleColumn) — the ONE place on Agents with a
 * genuine capacity semantic: each executor's oldest lease age measured against
 * the staleness threshold (default 48h). A capsule fills toward the ceiling and
 * turns seam→crit as it approaches/passes it, so the fleet's lease hygiene reads
 * as a shape, not a column of numbers. Skipped entirely when no executor holds a
 * timed lease (never forced — §9.3 rule).
 */
export function LeasePressure({
  executors,
  staleAfterSeconds,
}: {
  executors: ExecutorRow[];
  staleAfterSeconds: number;
}) {
  const { bars, thresholdHours } = leasePressureBars(executors, staleAfterSeconds);
  if (!bars.length) return null;

  return (
    <section className="rounded-xl bg-panel p-4 shadow-sm">
      <SectionHeader
        tone="seam"
        icon={<ClockIcon />}
        label="Lease pressure"
        sub={`oldest lease vs ${thresholdHours}h threshold`}
      />
      <div className="mt-4 flex flex-wrap items-end gap-4">
        {bars.map((bar) => (
          <div key={bar.id} className="flex flex-col items-center gap-1.5">
            {/* True age as the visible value label (§9.4: don't hide essentials
                behind hover). Bar height is a bounded scale; this reads the age. */}
            <span className={`mono text-2xs font-semibold tabular-nums ${TONE_TEXT[bar.tone]}`}>{bar.ageLabel}</span>
            <CapsuleColumn
              value={bar.ageHours}
              max={bar.ceilingHours}
              tone={bar.tone}
              unit="hours held"
              caption={bar.label}
              label={bar.shortLabel}
              extraRows={[
                { label: "held", value: bar.ageLabel, tone: bar.tone },
                { label: "threshold", value: `${thresholdHours}h`, tone: "neutral" },
                { label: "oldest lease", value: bar.oldestLeaseWorkId || "—", tone: "neutral" },
                { label: "leases", value: bar.leaseCount, tone: "neutral" },
              ]}
            />
          </div>
        ))}
      </div>
    </section>
  );
}
