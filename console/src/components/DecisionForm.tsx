"use client";

import { useRouter } from "next/navigation";
import { FormEvent, useState } from "react";

import { CliCaption } from "@/components/CopyCmd";
import { accentColor } from "@/components/ui";
import type { DecisionFormSpec, FieldSpec } from "@/lib/decision-registry";

type Result = { ok: boolean; stdout?: string; stderr?: string };

function renderField(field: FieldSpec) {
  const cls =
    "rounded-lg border border-line bg-white px-3 py-2 text-[12.5px] text-ink outline-none focus:border-slate-300";
  if (field.type === "textarea") {
    return (
      <textarea
        name={field.name}
        required={field.required}
        defaultValue={field.defaultValue}
        placeholder={field.placeholder}
        className={`min-h-[72px] resize-y ${cls}`}
      />
    );
  }
  if (field.type === "select") {
    return (
      <select name={field.name} defaultValue={field.defaultValue} className={cls}>
        {(field.options ?? []).map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    );
  }
  return (
    <input
      name={field.name}
      type={field.type === "number" ? "number" : field.type === "datetime-local" ? "datetime-local" : "text"}
      min={field.min}
      max={field.max}
      step={field.step}
      required={field.required}
      defaultValue={field.defaultValue}
      placeholder={field.placeholder}
      className={cls}
    />
  );
}

/**
 * The interactive half of a decision. Receives only the serializable spec
 * subset — no server-only functions/fs cross the boundary. Posts to the single
 * generic /api/decision/execute endpoint.
 */
export function DecisionForm({
  formSpec,
  itemId,
  argvPreview,
}: {
  formSpec: DecisionFormSpec;
  itemId: string;
  argvPreview?: string[] | null;
}) {
  const router = useRouter();
  const [result, setResult] = useState<Result | null>(null);
  const [pending, setPending] = useState(false);
  const [option, setOption] = useState(
    formSpec.defaultOption ?? formSpec.options[0]?.value ?? "",
  );

  const selectedOpt = formSpec.options.find((o) => o.value === option);
  const captionArgv = selectedOpt?.argv ?? argvPreview ?? null;
  const visibleFields = formSpec.fields.filter(
    (f) => !f.showForOptions || f.showForOptions.includes(option),
  );

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPending(true);
    setResult(null);

    const form = new FormData(event.currentTarget);
    const fields: Record<string, string> = {};
    for (const field of formSpec.fields) {
      fields[field.name] = String(form.get(field.name) ?? "");
    }

    const response = await fetch("/api/decision/execute", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ area: formSpec.area, itemId, option, fields }),
    });
    const body = (await response.json()) as Result;
    setResult(body);
    setPending(false);
    if (body.ok) router.refresh();
  }

  const selectedLabel =
    formSpec.options.find((o) => o.value === option)?.label ?? option;

  return (
    <form onSubmit={submit} className="mt-3 flex flex-col gap-3 border-t border-line pt-3">
      <div className="flex flex-col gap-1.5">
        <span className="section-label">Decision</span>
        <div className="grid gap-1.5 sm:grid-cols-2">
          {formSpec.options.map((opt) => {
            const selected = opt.value === option;
            return (
              <button
                key={opt.value}
                type="button"
                onClick={() => setOption(opt.value)}
                aria-pressed={selected}
                className={`flex flex-col gap-0.5 rounded-xl border px-3 py-2 text-left transition-colors ${
                  selected ? "border-ink bg-canvas" : "border-line bg-white hover:bg-canvas/50"
                }`}
              >
                <span className="flex items-center gap-1.5">
                  <span
                    className="h-2 w-2 shrink-0 rounded-full"
                    style={{ background: accentColor(opt.tone) }}
                  />
                  <span className="text-[12.5px] font-semibold text-ink">{opt.label}</span>
                </span>
                <span className="text-[11.5px] leading-snug text-ink-3">{opt.description}</span>
              </button>
            );
          })}
        </div>
      </div>

      {visibleFields.length > 0 ? (
        <div className="grid gap-3">
          {visibleFields.map((field) => (
            <label key={field.name} className="flex flex-col gap-1">
              <span className="section-label">
                {field.label}
                {field.required ? <span className="text-red-400"> *</span> : null}
              </span>
              {renderField(field)}
            </label>
          ))}
        </div>
      ) : null}

      <div className="flex flex-wrap items-center gap-2">
        <button
          type="submit"
          disabled={pending}
          className="rounded-lg bg-ink px-3 py-2 text-[12px] font-semibold text-white disabled:cursor-wait disabled:opacity-55"
        >
          {pending ? "Recording…" : `Record · ${selectedLabel}`}
        </button>
        <CliCaption argv={captionArgv} />
      </div>

      {result ? (
        <div
          className={`rounded-lg border px-3 py-2 text-[12px] leading-relaxed ${
            result.ok
              ? "border-emerald-200 bg-emerald-50 text-emerald-700"
              : "border-red-200 bg-red-50 text-red-600"
          }`}
        >
          {result.ok ? "Recorded." : result.stderr || result.stdout || "Command failed."}
        </div>
      ) : null}
    </form>
  );
}
