import Link from "next/link";

import { Chip, type Accent } from "@/components/ui";
import { ago } from "@/lib/time";
import type { LineageStep } from "@/lib/map/lineage";

const KIND_META: Record<LineageStep["kind"], { label: string; tone: Accent }> = {
  source: { label: "Source", tone: "orange" },
  evidence: { label: "Evidence", tone: "blue" },
  candidate: { label: "Candidate", tone: "violet" },
  adjudication: { label: "Adjudication", tone: "pink" },
  claim: { label: "Claim", tone: "green" },
  edge: { label: "Edge", tone: "gray" },
  work: { label: "Work", tone: "blue" },
  receipt: { label: "Receipt", tone: "green" },
};

export function TimelineStep({ step, last }: { step: LineageStep; last?: boolean }) {
  const meta = KIND_META[step.kind];
  return (
    <li className="flex gap-3">
      <div className="flex flex-col items-center pt-1">
        <span className="h-2.5 w-2.5 rounded-full border-2 border-ink bg-white" />
        {!last ? <span className="mt-1 w-px flex-1 bg-line" /> : null}
      </div>
      <div className="min-w-0 flex-1 pb-5">
        <div className="flex flex-wrap items-center gap-2">
          <Chip tone={meta.tone}>{meta.label}</Chip>
          <span className="mono text-[11px] text-ink-2">{step.label}</span>
          {step.timestamp ? (
            <span className="text-[10.5px] text-ink-3">{ago(step.timestamp)}</span>
          ) : null}
        </div>
        {step.detail ? (
          <p className="mt-1 text-[12px] leading-relaxed text-ink-2">{step.detail}</p>
        ) : null}
        {step.refs.length ? (
          <div className="mt-1.5 flex flex-wrap gap-1">
            {step.refs.map((ref) =>
              ref.startsWith("work_") ? (
                <Link
                  key={ref}
                  href={`/work/${ref}`}
                  className="mono rounded-md bg-canvas px-1.5 py-0.5 text-[10px] text-ink-2 hover:underline"
                >
                  {ref}
                </Link>
              ) : (
                <span key={ref} className="mono rounded-md bg-canvas px-1.5 py-0.5 text-[10px] text-ink-3">
                  {ref}
                </span>
              ),
            )}
          </div>
        ) : null}
      </div>
    </li>
  );
}
