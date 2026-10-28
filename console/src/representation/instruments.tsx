/**
 * Instruments for standing envelopes (settled decision #3). A dial / fader /
 * switch surfaces a REAL decision record (e.g. a job_health auto-admit grant,
 * shown with the synthetic id claim_examplejobhealth). Turning it does not act
 * directly — it mints an
 * `ActRequest` that SUPERSEDES the current record through the governed path, so
 * the change is receipted and undoable. The counterfactual preview line is
 * shown ONLY when a projection actually computed it from receipts; otherwise it
 * is omitted — an honest blank, never a fabricated number (pf3's law applies to
 * us too).
 *
 * Like the shapes, an instrument renders a real, a11y-reachable control and
 * reaches no act client (FR5): `instrumentActRequest` produces the request; a
 * consumer outside the cluster dispatches it.
 */
import type { ActRequest } from "./types";

export type InstrumentKind = "dial" | "fader" | "switch";

export interface InstrumentSetting {
  /** the decision record this instrument currently reflects. */
  recordId: string;
  label: string;
  kind: InstrumentKind;
  /** governed routing for the superseding record. */
  area: string;
  itemId: string;
  actionId: string;
  requiresConfirmation: boolean;
  /** dial/fader carry a number; switch carries on/off. */
  value: number | boolean;
  min?: number;
  max?: number;
  unit?: string;
  /** present ONLY when a projection computed it from receipts; omit otherwise. */
  counterfactual?: string;
}

/**
 * Mint the superseding decision record request. `effect: "record"` and
 * `supersedes` name the prior record — the governed path records it and can
 * roll it back.
 */
export function instrumentActRequest(setting: InstrumentSetting, next: number | boolean): ActRequest {
  return {
    area: setting.area,
    itemId: setting.itemId,
    actionId: setting.actionId,
    effect: "record",
    requiresConfirmation: setting.requiresConfirmation,
    supersedes: setting.recordId,
    rationale: `instrument ${setting.kind} "${setting.label}" set to ${String(next)}`,
  };
}

function Control({ setting }: { setting: InstrumentSetting }) {
  if (setting.kind === "switch") {
    const on = setting.value === true;
    return (
      <button
        type="button"
        role="switch"
        aria-checked={on}
        aria-label={setting.label}
        data-record={setting.recordId}
        className={on ? "rounded-pill border border-good-line bg-good-soft px-3 py-0.5 text-xs font-semibold text-good" : "rounded-pill border border-hair px-3 py-0.5 text-xs font-semibold text-muted"}
      >
        {on ? "on" : "off"}
      </button>
    );
  }
  return (
    <button
      type="button"
      aria-label={`${setting.label}: ${String(setting.value)}${setting.unit ?? ""}`}
      data-record={setting.recordId}
      data-min={setting.min}
      data-max={setting.max}
      className="rounded-md border border-hair bg-soft-2 px-3 py-0.5 text-sm font-semibold tabular-nums text-strong"
    >
      {String(setting.value)}
      {setting.unit ?? ""}
    </button>
  );
}

export function Instrument({ setting }: { setting: InstrumentSetting }) {
  return (
    <div className="rounded-lg border border-hair bg-soft p-4" data-instrument={setting.kind} data-record={setting.recordId}>
      <div className="flex items-center justify-between gap-3">
        <span className="text-sm font-semibold text-strong">{setting.label}</span>
        <Control setting={setting} />
      </div>
      <p className="mt-1 text-2xs text-faint">
        reflects decision record {setting.recordId} — turning it records a new one that supersedes it (undoable)
      </p>
      {setting.counterfactual ? <p className="mt-1 text-2xs text-muted">{setting.counterfactual}</p> : null}
    </div>
  );
}
