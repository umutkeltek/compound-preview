"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMemo, useRef, useState } from "react";

import { Button, Field, FreshnessChip, IdRef } from "@/components/kit";
import { ago } from "@/lib/time";
import type { AdjudicationCardModel } from "./model";

/*
 * One governed memory-adjudication decision. The candidate is proposed (a
 * "draft" — nothing runs yet); the operator rules park / no-promote / reject /
 * promote. Consumes the serialized `memory-adjudicate` registry spec (options +
 * the promote-only basis field) and posts the SAME payload the Decide queue
 * posts to the one governed endpoint. Optimistic act per DESIGN.md §5.
 *
 * The card carries the amber admission seam (it needs the operator) and a
 * violet memory glyph tile (domain). "Show in graph" seeds the embedded ego
 * graph from this claim's entity — the id is never dead text.
 */

type ExecuteResult = { ok: boolean; status?: number; stdout?: string; stderr?: string };
type ActState = "idle" | "pending" | "confirmed" | "error";

/** Option accent (registry Accent) → semantic token dot — tokens only, no raw hex. */
const DOT_TONE: Record<string, string> = {
  green: "bg-good",
  orange: "bg-seam",
  red: "bg-crit",
  blue: "bg-work",
  violet: "bg-memory",
  pink: "bg-product",
  gray: "bg-faint",
};

/** Chip tone (registry Accent) → semantic token classes. */
const CHIP_TONE: Record<string, string> = {
  green: "border-good-line bg-good-soft text-good",
  orange: "border-seam-line bg-seam-soft text-seam",
  red: "border-crit-line bg-crit-soft text-crit",
  blue: "border-work-line bg-work-soft text-work",
  violet: "border-memory-line bg-memory-soft text-memory",
  pink: "border-product-line bg-product-soft text-product",
  gray: "border-hair bg-soft-2 text-muted",
};

function MetaChip({ label, tone }: { label: string; tone: string }) {
  return (
    <span
      title={label}
      className={`inline-flex max-w-48 items-center truncate rounded-pill border px-2 py-0.5 text-2xs font-semibold ${
        CHIP_TONE[tone] ?? CHIP_TONE.gray
      }`}
    >
      {label}
    </span>
  );
}

export function AdjudicationCard({
  model,
  mutationsEnabled,
}: {
  model: AdjudicationCardModel;
  mutationsEnabled: boolean;
}) {
  const router = useRouter();
  const { formSpec } = model;
  const [option, setOption] = useState(formSpec.defaultOption ?? formSpec.options[0]?.value ?? "");
  const [state, setState] = useState<ActState>("idle");
  const [message, setMessage] = useState<string | null>(null);
  const formRef = useRef<HTMLFormElement | null>(null);

  const selected = formSpec.options.find((o) => o.value === option) ?? formSpec.options[0];
  const captionArgv = selected?.argv ?? model.argvPreview ?? null;
  const visibleFields = useMemo(
    () => formSpec.fields.filter((f) => !f.showForOptions || f.showForOptions.includes(option)),
    [formSpec.fields, option],
  );

  async function submit() {
    if (!mutationsEnabled) return;
    // required-field guard (endpoint enforces too; stop here so the operator
    // sees which field before the round-trip).
    if (formRef.current) {
      for (const f of visibleFields) {
        if (!f.required) continue;
        const value = new FormData(formRef.current).get(f.name);
        if (!String(value ?? "").trim()) {
          setState("error");
          setMessage(`${f.label} is required.`);
          window.setTimeout(() => setState("idle"), 1600);
          return;
        }
      }
    }
    setState("pending");
    setMessage(null);
    const fields: Record<string, string> = {};
    if (formRef.current) {
      const data = new FormData(formRef.current);
      for (const f of formSpec.fields) fields[f.name] = String(data.get(f.name) ?? "");
    }
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ area: formSpec.area, itemId: model.id, option, fields }),
      });
      const parsed = (await response.json().catch(() => null)) as ExecuteResult | null;
      const result: ExecuteResult = parsed ?? { ok: false, stderr: `Request failed with ${response.status}` };
      if (result.ok) {
        setState("confirmed");
        router.refresh();
        window.setTimeout(() => setState("idle"), 900);
        return;
      }
      setState("error");
      setMessage(result.stderr || result.stdout || "Command failed.");
      window.setTimeout(() => setState("idle"), 1600);
    } catch (error) {
      setState("error");
      setMessage(error instanceof Error ? error.message : "Request failed.");
      window.setTimeout(() => setState("idle"), 1600);
    }
  }

  const stateClass =
    state === "pending"
      ? "action-pending"
      : state === "confirmed"
        ? "action-confirmed"
        : state === "error"
          ? "action-error"
          : "";

  return (
    <article className="admission-seam rounded-lg border border-hair bg-panel px-4 py-3.5 shadow-sm">
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 items-start gap-2">
          <span
            aria-hidden
            className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-sm border border-memory-line bg-memory-soft text-xs font-bold text-memory"
          >
            {model.glyph}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-1.5">
              <span className="text-2xs font-bold uppercase tracking-wide text-faint">{model.kindLabel}</span>
            </div>
            <h3 className="mt-0.5 text-md font-semibold leading-snug text-ink">{model.headline}</h3>
          </div>
        </div>
        <span className="shrink-0 rounded-pill border border-hair bg-transparent px-2.5 py-0.5 text-xs font-semibold text-muted">
          {model.stateLabel}
        </span>
      </div>

      {model.body ? (
        <p className="mt-2 text-sm leading-relaxed text-muted" style={{ maxWidth: "var(--read-max)" }}>
          {model.body}
        </p>
      ) : null}

      {model.chips.length ? (
        <div className="mt-2 flex flex-wrap gap-1">
          {model.chips.map((c, i) => (
            <MetaChip key={i} label={c.label} tone={c.tone} />
          ))}
        </div>
      ) : null}

      {model.consequences ? (
        <div className="mt-2 rounded-md bg-soft px-3 py-2 text-xs leading-relaxed">
          <div className="text-ink">
            <span className="font-semibold text-good">if you {model.approveLabel}</span>
            <span className="text-faint"> → </span>
            {model.consequences.approve}
          </div>
          <div className="mt-0.5 text-muted">
            <span className="font-semibold">if you wait</span>
            <span> → </span>
            {model.consequences.hold}
          </div>
        </div>
      ) : null}

      {/* verdict selector — one primary act, options never auto-fire */}
      <div className="mt-3 flex flex-col gap-1.5">
        <span className="text-2xs font-bold uppercase tracking-wide text-faint">Verdict</span>
        <div className="grid gap-1.5 sm:grid-cols-2">
          {formSpec.options.map((opt) => {
            const isSel = opt.value === option;
            return (
              <button
                key={opt.value}
                type="button"
                onClick={() => setOption(opt.value)}
                aria-pressed={isSel}
                className={`flex flex-col gap-0.5 rounded-md border px-3 py-2 text-left transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus ${
                  isSel ? "border-strong bg-soft-2" : "border-hair bg-panel hover:bg-soft"
                }`}
              >
                <span className="flex items-center gap-1.5">
                  <span className={`h-2 w-2 shrink-0 rounded-full ${DOT_TONE[opt.tone] ?? DOT_TONE.gray}`} />
                  <span className="text-sm font-semibold text-ink">{opt.label}</span>
                </span>
                <span className="text-xs leading-snug text-muted">{opt.description}</span>
              </button>
            );
          })}
        </div>
      </div>

      {/* one stable form node; fields appear only for the option that needs them */}
      <form ref={formRef} className={visibleFields.length ? "mt-3 flex flex-col gap-3" : "hidden"}>
        {visibleFields.map((f) => (
          <Field key={f.name} field={f} />
        ))}
      </form>

      {captionArgv?.length ? (
        <pre className="mono mt-3 overflow-x-auto rounded-md border border-hair bg-soft px-3 py-2 text-2xs leading-relaxed text-muted">
          compound {captionArgv.join(" ")}
        </pre>
      ) : null}

      <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-xs tabular-nums text-faint">{model.createdAt ? ago(model.createdAt) : "age unknown"}</span>
          <FreshnessChip source="compound memory candidates --json" />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Link
            href={`/memory?graph=${encodeURIComponent(model.seed)}#memory-graph`}
            scroll
            className="inline-flex items-center gap-1.5 rounded-md border border-hair bg-panel px-3 py-2 text-xs font-semibold text-ink transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
          >
            Show in graph
          </Link>
          <Button
            variant="primary"
            disabled={!mutationsEnabled || state === "pending"}
            onClick={() => void submit()}
            className={stateClass}
            title={mutationsEnabled ? undefined : "Launch via 'compound console --operator' to act from a pinned console."}
          >
            {state === "pending" ? "Recording…" : `Record · ${selected?.label ?? option}`}
          </Button>
        </div>
      </div>

      {state === "error" && message ? (
        <p className="mt-1.5 text-2xs leading-snug text-crit">{message}</p>
      ) : null}

      <div className="mt-2 flex items-center justify-between gap-2 border-t border-hair pt-2">
        <IdRef id={model.id} kind="candidate" />
        {model.evidence.length ? (
          <details className="min-w-0">
            <summary className="cursor-pointer text-2xs font-medium text-faint hover:text-muted">
              evidence · {model.evidence.length}
            </summary>
            <ul className="mt-1 flex flex-col gap-0.5">
              {model.evidence.map((ref, i) => (
                <li key={i} className="mono truncate text-2xs text-faint" title={ref}>
                  {ref}
                </li>
              ))}
            </ul>
          </details>
        ) : null}
      </div>
    </article>
  );
}
