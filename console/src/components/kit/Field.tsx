"use client";

import type { FieldSpec } from "@/lib/decision-registry";

/**
 * Renders the existing FieldSpec shape (src/lib/decision-registry.ts) as a
 * labeled, tokens-only form control with a focus-visible ring.
 */
const INPUT_CLASS =
  "rounded-md border bg-panel px-3 py-2 text-sm text-ink outline-none " +
  "focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus";

function borderClass(invalid?: boolean): string {
  return invalid ? "border-crit-line" : "border-hair";
}

function FieldControl({ field, invalid, describedBy, disabled }: { field: FieldSpec; invalid?: boolean; describedBy?: string; disabled?: boolean }) {
  if (field.type === "textarea") {
    return (
      <textarea
        name={field.name}
        required={field.required}
        disabled={disabled}
        aria-invalid={invalid || undefined}
        aria-describedby={invalid ? describedBy : undefined}
        defaultValue={field.defaultValue}
        placeholder={field.placeholder}
        className={`min-h-20 resize-y ${borderClass(invalid)} ${INPUT_CLASS}`}
      />
    );
  }
  if (field.type === "select") {
    return (
      <select
        name={field.name}
        defaultValue={field.defaultValue}
        disabled={disabled}
        aria-invalid={invalid || undefined}
        aria-describedby={invalid ? describedBy : undefined}
        className={`${borderClass(invalid)} ${INPUT_CLASS}`}
      >
        {(field.options ?? []).map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
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
      disabled={disabled}
      aria-invalid={invalid || undefined}
      aria-describedby={invalid ? describedBy : undefined}
      defaultValue={field.defaultValue}
      placeholder={field.placeholder}
      className={`${borderClass(invalid)} ${INPUT_CLASS}`}
    />
  );
}

export function Field({
  field,
  invalid = false,
  describedBy,
  disabled = false,
  errorMessage,
}: {
  field: FieldSpec;
  invalid?: boolean;
  describedBy?: string;
  disabled?: boolean;
  /** Plain-language message rendered directly under this field when invalid — the "next to the field" fix (no silent no-op on empty required submit). */
  errorMessage?: string;
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-2xs font-bold uppercase tracking-wide text-faint">
        {field.label}
        {field.required ? <span className="text-crit"> *</span> : null}
      </span>
      <FieldControl field={field} invalid={invalid} describedBy={describedBy} disabled={disabled} />
      {invalid && errorMessage ? (
        <span id={describedBy} role="alert" className="text-2xs leading-snug text-crit">
          {errorMessage}
        </span>
      ) : null}
    </label>
  );
}
