import { ContinueButton } from "@/components/ContinueButton";
import { formatBlockedRef } from "@/lib/work-labels";
import type { ContinueModel, ContinueTone } from "@/lib/briefing";

/**
 * Continue card (redesign plan Part 3) — the "Continue" panel restyled onto kit
 * tokens. It reuses the governed ContinueButton and the ContinuePanel state
 * machine (ported pure into lib/briefing.deriveContinue) WITHOUT editing the
 * shared ContinuePanel component the Work screen also renders. The left seam
 * encodes STATE here (DESIGN.md §3 work-card rule): blue moving, amber your
 * gate, green ready.
 */

const SEAM: Record<ContinueTone, string> = {
  live: "var(--work-fg)",
  seam: "var(--seam-fg)",
  done: "var(--good-fg)",
  draft: "var(--hair)",
  queued: "var(--hair)",
  failed: "var(--crit-fg)",
  dropped: "var(--hair)",
};

function CountChip({ label, active }: { label: string; active: boolean }) {
  return (
    <span
      className={`inline-flex items-center rounded-pill border px-2.5 py-0.5 text-2xs font-semibold tabular-nums ${
        active ? "border-hair bg-soft-2 text-muted" : "border-hair bg-soft text-faint"
      }`}
    >
      {label}
    </span>
  );
}

export function ContinueCard({ model, mutationsReady }: { model: ContinueModel; mutationsReady: boolean }) {
  return (
    <section
      className="rounded-lg border border-hair bg-panel px-5 py-4 shadow-sm"
      style={{ borderLeftWidth: "3px", borderLeftColor: SEAM[model.tone] }}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="text-xs font-bold uppercase tracking-wide text-faint">Continue</div>
          <h2 className="mt-1 text-md font-semibold leading-snug text-ink">{model.headline}</h2>
          <p className="mt-1 text-sm leading-relaxed text-muted">{model.body}</p>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {model.chips.map((chip) => (
            <CountChip key={chip.label} label={chip.label} active={chip.active} />
          ))}
          <CountChip label={model.actor} active />
        </div>
      </div>

      <div className="mt-3">
        <ContinueButton request={model.request} mutationsReady={mutationsReady} />
      </div>

      {model.blockedOn.length > 0 ? (
        <p className="mt-2 text-2xs leading-relaxed text-faint">
          Waiting on {model.blockedOn.slice(0, 4).map(formatBlockedRef).join(" / ")}
          {model.blockedOn.length > 4 ? " / …" : ""}
        </p>
      ) : null}
    </section>
  );
}
