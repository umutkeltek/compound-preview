import { FreshnessChip, IdRef, StateBadge } from "@/components/kit";

export type AskCausalBriefProps = {
  title: string;
  summary: string;
  unlock?: string;
  count: number;
  blastRadius?: string;
  reversibility?: string;
  generatedAt?: string;
  source?: string;
};

export function AskCausalBrief(props: AskCausalBriefProps) {
  return <article className="overflow-hidden rounded-lg border border-seam-line bg-seam-soft/25">
    <div className="border-b border-seam-line px-3 py-2.5"><div className="section-label text-seam">{props.title}</div><p className="mt-1 text-sm font-semibold leading-snug text-ink">{props.summary}</p></div>
    <div className="grid grid-cols-2 gap-2 px-3 py-3 text-xs"><div className="rounded-md bg-panel px-2.5 py-2"><div className="section-label">Items</div><div className="mono mt-1 text-lg text-ink">{props.count}</div></div><div className="rounded-md bg-panel px-2.5 py-2"><div className="section-label">Risk shape</div><div className="mt-1 capitalize text-ink">{props.blastRadius?.replace(/_/g, " ") || "not projected"}</div><div className="text-2xs text-faint">{props.reversibility || "reversibility unknown"}</div></div></div>
    <div className="px-3 pb-3"><div className="section-label">What moves</div><p className="mt-1 text-xs leading-relaxed text-muted">{props.unlock || "No owner-projected unlock is available."}</p><div className="mt-2"><FreshnessChip generatedAt={props.generatedAt} source={props.source} /></div></div>
  </article>;
}

export type AskComparisonBriefProps = {
  title: string;
  actLabel: string;
  actConsequence?: string;
  waitConsequence?: string;
  evidenceCount?: number;
  generatedAt?: string;
  source?: string;
};

export function AskComparisonBrief(props: AskComparisonBriefProps) {
  return <article className="overflow-hidden rounded-lg border border-hair bg-panel">
    <div className="border-b border-hair px-3 py-2.5"><div className="section-label">Compare paths</div><p className="mt-1 text-sm font-semibold leading-snug text-ink">{props.title}</p></div>
    <div className="grid gap-px bg-hair sm:grid-cols-2"><div className="bg-good-soft px-3 py-3"><div className="text-xs font-semibold text-good">{props.actLabel}</div><p className="mt-2 text-xs leading-relaxed text-ink">{props.actConsequence || "Consequence not projected by the owner."}</p></div><div className="bg-memory-soft px-3 py-3"><div className="text-xs font-semibold text-memory">If you wait</div><p className="mt-2 text-xs leading-relaxed text-ink">{props.waitConsequence || "Consequence not projected by the owner."}</p></div></div>
    <div className="flex flex-wrap items-center justify-between gap-2 border-t border-hair px-3 py-2"><span className="mono text-2xs text-faint">{props.evidenceCount == null ? "Evidence count not projected" : `${props.evidenceCount} evidence refs`}</span><FreshnessChip generatedAt={props.generatedAt} source={props.source} /></div>
  </article>;
}

export type AskWorkListProps = {
  label: string;
  items: Array<{ id: string; title: string; state: string; owner?: string }>;
};

export function AskWorkList({ label, items }: AskWorkListProps) {
  return <article className="overflow-hidden rounded-lg border border-hair bg-panel"><div className="border-b border-hair px-3 py-2.5"><div className="section-label">{label}</div></div><div>{items.length ? items.map((item) => <a key={item.id} href={`/work?trace=${item.id}`} className="block border-b border-hair px-3 py-2.5 last:border-b-0 hover:bg-soft"><div className="flex items-start justify-between gap-2"><div className="line-clamp-2 text-xs font-semibold leading-snug text-ink">{item.title}</div><StateBadge state={item.state} /></div><div className="mt-1 flex items-center justify-between gap-2"><IdRef id={item.id} kind="work" plain truncate />{item.owner ? <span className="max-w-28 truncate text-2xs text-faint">{item.owner}</span> : null}</div></a>) : <p className="px-3 py-6 text-center text-xs text-muted">No matching work is projected.</p>}</div></article>;
}
