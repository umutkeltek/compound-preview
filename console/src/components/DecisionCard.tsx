import { AccentSquare, Chip } from "@/components/ui";
import { DecisionDetailsDialog } from "@/components/DecisionDetailsDialog";
import { DecisionForm } from "@/components/DecisionForm";
import { specFor } from "@/lib/decision-registry";
import { arr, type Json, str } from "@/lib/compound";

/**
 * Server component. Renders one pending item as a plain-language decision card:
 * what it IS (headline + body), its limits (boundaries), its proof (evidence
 * refs), and a one-click decision form. The interactive half (DecisionForm) is
 * a client island receiving only serializable data.
 *
 * Unknown areas (no registry entry) fall back to a read-only card that explains
 * what the area is and shows the projection's own next_action — never a silent
 * CopyCmd.
 */
export function DecisionCard({ area, item }: { area: string; item: Json }) {
  const spec = specFor(area);
  const id = str(item.id) || str(item.review_id) || str(item.work_id);

  if (!spec) {
    return (
      <div className="rounded-xl border border-line bg-white px-4 py-3">
        <div className="flex items-center justify-between gap-2">
          <span className="text-[12.5px] font-semibold text-ink">{area}</span>
          <Chip tone="gray">{str(item.status) || "open"}</Chip>
        </div>
        <p className="mt-1 text-[12px] text-ink-3">
          {str(item.next_action) || "No registry entry yet — this area needs a terminal action for now."}
        </p>
        <div className="mt-2 rounded-lg bg-canvas px-2.5 py-1.5 text-[11px] leading-relaxed">
          <div className="text-ink-2">
            <span className="font-semibold text-emerald-700">if you act</span>
            <span className="text-ink-3"> → </span>
            {str(item.next_action) || "the recorded review unblocks this area's next step — nothing runs from this card"}
          </div>
          <div className="text-ink-3">
            <span className="font-semibold">if you wait</span>
            <span> → </span>
            nothing happens; it stays in this queue
          </div>
        </div>
        <code className="mono mt-1 block truncate text-[10.5px] text-ink-3">{id}</code>
      </div>
    );
  }

  const pl = spec.plainLanguage(item);
  const options = spec.optionsFor ? spec.optionsFor(item) : spec.options;
  const argvPreview = spec.previewArgv
    ? spec.previewArgv(item)
    : options[0]?.argv ?? null;
  const formSpec = {
    area: spec.area,
    options,
    fields: spec.fields,
    defaultOption: spec.defaultOption ?? options[0]?.value,
  };

  return (
    <div className="rounded-xl border border-line bg-white px-4 py-3.5">
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <AccentSquare accent={spec.accent}>{spec.glyph}</AccentSquare>
          <div className="min-w-0">
            <div className="section-label text-[10px]">{spec.title}</div>
            <div className="truncate text-[13px] font-semibold text-ink">{pl.headline}</div>
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1">
          {pl.chips?.map((chip, i) => (
            <Chip key={i} tone={chip.tone} title={chip.label}>
              {chip.label}
            </Chip>
          ))}
        </div>
      </div>

      {pl.body ? <p className="mt-2 text-[12.5px] leading-relaxed text-ink-2">{pl.body}</p> : null}

      {pl.consequences ? (
        <div className="mt-2 rounded-lg bg-canvas px-2.5 py-1.5 text-[11px] leading-relaxed">
          <div className="text-ink-2">
            <span className="font-semibold text-emerald-700">if you act</span>
            <span className="text-ink-3"> → </span>
            {pl.consequences.approve}
          </div>
          <div className="text-ink-3">
            <span className="font-semibold">if you wait</span>
            <span> → </span>
            {pl.consequences.hold}
          </div>
        </div>
      ) : null}

      <div className="mt-3 flex flex-wrap items-center gap-2">
        <DecisionDetailsDialog area={area} itemId={id} item={item} pl={pl} formSpec={formSpec} />
        <code className="mono min-w-0 break-all text-[10.5px] text-ink-3">{id}</code>
      </div>

      {pl.boundaries?.length ? (
        <div className="mt-2 flex flex-wrap gap-1">
          {pl.boundaries.map((b) => (
            <span
              key={b}
              className="rounded-md bg-canvas px-1.5 py-0.5 text-[10.5px] text-ink-3"
              title="boundary"
            >
              {b}
            </span>
          ))}
        </div>
      ) : null}

      {pl.evidence?.length ? (
        <details className="mt-2 group">
          <summary className="cursor-pointer text-[11px] text-ink-3 hover:text-ink-2">
            evidence · {pl.evidence.length} ref{pl.evidence.length === 1 ? "" : "s"}
          </summary>
          <ul className="mt-1 flex flex-col gap-0.5">
            {pl.evidence.map((ref, i) => (
              <li key={i} className="mono truncate text-[10.5px] text-ink-3" title={ref}>
                {ref}
              </li>
            ))}
          </ul>
        </details>
      ) : null}

      <DecisionForm formSpec={formSpec} itemId={id} argvPreview={argvPreview} />

      {arr(item.actions).length === 0 && !argvPreview ? (
        <p className="mt-1 text-[10.5px] text-ink-3">
          id <code className="mono">{id}</code>
        </p>
      ) : null}
    </div>
  );
}
