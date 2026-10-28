"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useId, useRef, useState } from "react";

import { CopyCmd } from "@/components/CopyCmd";
import { AuthorityFaceDetails } from "@/components/authority/AuthorityFaceDetails";
import { Button, ConfirmDialog, Field } from "@/components/kit";
import type { DecideAct, DecideCapability } from "@/lib/decide-model";
import type { DecisionExecutionEffect, DecisionExecutionRefs } from "@/lib/decision-execute";
import type { DecisionOption, FieldSpec } from "@/lib/decision-registry";
import { buildDecideActionRequest, decideActIdentity, fieldValidationMessage, type GovernedSubmitKind } from "@/lib/governed-action-client";
import type { AuthorityFace } from "@/representation";

export type ExecuteResult = {
  ok: boolean;
  status?: number;
  stdout?: string;
  stderr?: string;
  preview?: boolean;
  refs?: DecisionExecutionRefs;
  effect?: DecisionExecutionEffect | null;
};

type ActState = "idle" | "pending" | "confirmed" | "error";
type SingleAct = Extract<DecideAct, { mode: "bespoke" | "batch-atomic" | "generic" }>;
type OutcomeKind = Exclude<DecideCapability["kind"], "unavailable">;

function actFields(act: SingleAct): FieldSpec[] {
  if (act.mode === "bespoke") return act.fields;
  if (act.mode === "generic" && act.reasonRequired) {
    return [
      {
        name: "reason",
        type: "textarea",
        label: "Reason",
        required: true,
        placeholder: "Why should this review item move out of the active queue?",
      },
    ];
  }
  return [];
}

function fieldsForOption(fields: FieldSpec[], option: string): FieldSpec[] {
  return fields.filter((field) => !field.showForOptions || field.showForOptions.includes(option));
}

function initialOption(act: SingleAct): string {
  if (act.mode === "bespoke") return act.defaultOption || (act.options.length === 1 ? act.options[0].value : "");
  if (act.mode === "generic" && act.decisionOptions?.length) {
    return act.defaultOption || (act.decisionOptions.length === 1 ? act.decisionOptions[0].value : "");
  }
  return "option" in act ? act.option : "";
}

function decisionOptions(act: SingleAct): DecisionOption[] {
  if (act.mode === "bespoke") return act.options;
  if (act.mode === "generic") return act.decisionOptions ?? [];
  return [];
}

function confirmationDescription(act: SingleAct, chosen: DecisionOption | null): string {
  if (chosen?.description) return chosen.description;
  if (act.mode === "generic" && act.effectDescription) return act.effectDescription;
  if (act.mode === "generic") return "Records that you reviewed this item. It does not repair, tidy, or otherwise change the underlying system.";
  if (act.mode === "batch-atomic") return `Applies this decision to all ${act.memberCount} items together after confirmation.`;
  return "Review what will change before confirming this decision.";
}

function idRows(refs?: DecisionExecutionRefs): Array<{ label: string; values: string[] }> {
  if (!refs) return [];
  return [
    { label: "Decision", values: refs.decision_ids },
    { label: "Work", values: refs.work_ids },
    { label: "Action", values: refs.action_intent_ids },
    { label: "Policy", values: refs.policy_decision_ids },
    { label: "Receipt", values: refs.receipt_ids },
    { label: "Transition", values: refs.transition_ids },
  ].filter((row) => row.values.length > 0);
}

function outcomeHeading(result: ExecuteResult, preview: boolean, effectKind: OutcomeKind): string {
  if (preview) return "Write-free preview";
  if (!result.ok) return "Action failed";
  if (result.effect?.changed === false) return "No state change";
  if (effectKind === "record") return "Decision recorded";
  if (effectKind === "track") {
    return result.effect?.kind.endsWith("open_work") ? "Tracked work opened" : "Disposition recorded";
  }
  return "Change applied";
}

export function DecisionOutcome({
  result,
  preview = false,
  effectKind = "apply",
}: {
  result: ExecuteResult;
  preview?: boolean;
  effectKind?: OutcomeKind;
}) {
  const effect = result.effect;
  const refs = result.refs;
  const rows = idRows(refs);
  const workId = effect?.workId || refs?.work_ids[0] || "";
  const tone = result.ok ? (preview ? "border-work-line bg-work-soft" : "border-good-line bg-good-soft") : "border-crit-line bg-crit-soft";

  return (
    <section className={`rounded-md border px-3 py-3 ${tone}`} aria-live="polite">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="text-2xs font-bold uppercase text-faint">
            {outcomeHeading(result, preview, effectKind)}
          </div>
          <div className="mt-0.5 text-sm font-semibold text-ink">
            {effect?.summary || result.stderr || result.stdout || (preview ? "Preview resolved." : "Decision recorded.")}
          </div>
        </div>
        {effect?.status ? (
          <span className="mono rounded-pill border border-hair bg-panel px-2 py-0.5 text-2xs text-muted">
            {effect.status}
          </span>
        ) : null}
      </div>

      {effect?.before || effect?.after ? (
        <div className="mt-2 flex items-center gap-2 text-xs text-muted">
          <span className="mono">{effect.before || "current"}</span>
          <span className="text-faint">→</span>
          <span className="mono font-semibold text-ink">{effect.after || "unchanged"}</span>
        </div>
      ) : null}

      {rows.length || refs?.proof_refs.length ? (
        <details className="mt-2 border-t border-hair pt-2">
          <summary className="cursor-pointer text-2xs font-semibold text-muted">Receipt details</summary>
          {rows.length ? (
            <dl className="mt-2 grid gap-1.5">
              {rows.map((row) => (
                <div key={row.label} className="grid grid-cols-4 gap-2 text-2xs">
                  <dt className="font-semibold text-faint">{row.label}</dt>
                  <dd className="col-span-3 flex flex-wrap gap-x-2 gap-y-1">
                    {row.values.map((value) =>
                      row.label === "Work" && !preview ? (
                        <Link key={value} href={`/work/${value}`} className="mono text-work hover:underline" title={value}>
                          {value}
                        </Link>
                      ) : (
                        <code key={value} className="mono text-muted" title={value}>{value}</code>
                      ),
                    )}
                  </dd>
                </div>
              ))}
            </dl>
          ) : null}
          {refs?.proof_refs.length ? (
            <ul className="mt-2 grid gap-1 border-t border-hair pt-2">
              {refs.proof_refs.map((ref) => <li key={ref} className="mono break-all text-2xs text-faint">{ref}</li>)}
            </ul>
          ) : null}
        </details>
      ) : null}

      {workId && !preview ? (
        <Link href={`/work/${workId}`} className="mt-3 inline-flex text-xs font-semibold text-work hover:underline">
          Open proposed work
        </Link>
      ) : null}
    </section>
  );
}

export function GovernedAct({
  act,
  authority,
  mutationsEnabled,
  registerTrigger,
  onExecuted,
  retainedOutcome,
  effectKind,
}: {
  act: SingleAct;
  authority?: AuthorityFace | null;
  mutationsEnabled: boolean;
  registerTrigger?: (fn: (() => void) | null) => void;
  onExecuted?: (result: ExecuteResult) => void;
  retainedOutcome?: ExecuteResult | null;
  effectKind: OutcomeKind;
}) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<ActState>("idle");
  const [message, setMessage] = useState<string | null>(null);
  const [selectedOption, setSelectedOption] = useState(() => initialOption(act));
  const [previewResult, setPreviewResult] = useState<ExecuteResult | null>(null);
  const [localOutcome, setLocalOutcome] = useState<ExecuteResult | null>(null);
  const [fellBackToCopy, setFellBackToCopy] = useState(false);
  const [invalidField, setInvalidField] = useState<string | null>(null);
  const [confirmationFieldValues, setConfirmationFieldValues] = useState<Record<string, string>>({});
  const formRef = useRef<HTMLFormElement | null>(null);
  const validationMessageId = useId();

  const fields = actFields(act);
  const options = decisionOptions(act);
  const visibleFields = fieldsForOption(fields, selectedOption);
  const outcome = retainedOutcome ?? localOutcome;
  const actIdentity = decideActIdentity(act);
  const latestActIdentity = useRef(actIdentity);
  latestActIdentity.current = actIdentity;

  useEffect(() => {
    setOpen(false);
    setState("idle");
    setSelectedOption(initialOption(act));
    setPreviewResult(null);
    setLocalOutcome(null);
    setFellBackToCopy(false);
    setInvalidField(null);
    setConfirmationFieldValues({});
    setMessage(null);
    // The server refreshes projections every 30 seconds and recreates the act
    // object. Reset only when the owner item/revision changes, not while the
    // operator is writing a decision against the same revision.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [actIdentity]);

  function collectedFields(): Record<string, string> {
    const collected: Record<string, string> = {};
    if (act.mode === "batch-atomic") Object.assign(collected, act.fields);
    if (formRef.current) {
      const data = new FormData(formRef.current);
      for (const [key, value] of data.entries()) collected[key] = typeof value === "string" ? value : "";
    }
    return collected;
  }

  function validateDraft(): boolean {
    if (options.length > 1 && !selectedOption) {
      setState("error");
      setInvalidField("decision_option");
      setMessage("Choose one decision before continuing.");
      formRef.current?.querySelector<HTMLInputElement>('input[name="decision_option"]')?.focus();
      return false;
    }
    const values = collectedFields();
    for (const field of visibleFields) {
      if (field.required && !String(values[field.name] ?? "").trim()) {
        setState("error");
        setInvalidField(field.name);
        setMessage(fieldValidationMessage(field));
        const control = formRef.current?.elements.namedItem(field.name);
        if (control instanceof HTMLElement) control.focus();
        return false;
      }
    }
    setInvalidField(null);
    return true;
  }

  async function request(
    kind: GovernedSubmitKind,
    fieldValues: Record<string, string> = collectedFields(),
  ): Promise<ExecuteResult> {
    const response = await fetch("/api/decision/execute", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(buildDecideActionRequest(act, selectedOption, fieldValues, kind)),
    });
    const parsed = (await response.json().catch(() => null)) as ExecuteResult | null;
    return parsed
      ? { ...parsed, status: response.status }
      : { ok: false, status: response.status, stderr: `Request failed with ${response.status}` };
  }

  async function trigger() {
    if (!mutationsEnabled || state === "pending" || !validateDraft()) return;
    const fieldValues = collectedFields();
    setMessage(null);
    if (act.mode === "bespoke" && act.supportsPreview) {
      const previewIdentity = actIdentity;
      setState("pending");
      try {
        const result = await request("preview", fieldValues);
        if (latestActIdentity.current !== previewIdentity) return;
        if (!result.ok) {
          setState("error");
          setMessage(result.stderr || "Preview failed.");
          return;
        }
        setPreviewResult(result);
        setConfirmationFieldValues(fieldValues);
        setState("idle");
        setOpen(true);
      } catch (error) {
        setState("error");
        setMessage(error instanceof Error ? error.message : "Preview failed.");
      }
      return;
    }
    if (act.requiresConfirmation || fields.length > 0 || (act.mode === "bespoke" && act.options.length > 1)) {
      setConfirmationFieldValues(fieldValues);
      setOpen(true);
      return;
    }
    await submit("direct", fieldValues);
  }

  useEffect(() => {
    registerTrigger?.(() => void trigger());
    return () => registerTrigger?.(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [act, mutationsEnabled, selectedOption, state]);

  async function submit(
    kind: Exclude<GovernedSubmitKind, "preview">,
    fieldValues?: Record<string, string>,
  ) {
    if (!validateDraft()) return;
    setState("pending");
    setMessage(null);
    try {
      const result = await request(kind, fieldValues);
      onExecuted?.(result);
      if (result.ok) {
        setLocalOutcome(result);
        setState("confirmed");
        setOpen(false);
        router.refresh();
        return;
      }
      if (act.mode === "generic" && result.status === 403) {
        setFellBackToCopy(true);
        setOpen(false);
        setState("idle");
        return;
      }
      setState("error");
      setMessage(result.stderr || result.stdout || "Command failed.");
    } catch (error) {
      setState("error");
      setMessage(error instanceof Error ? error.message : "Request failed.");
    }
  }

  if (fellBackToCopy && act.mode === "generic") {
    return (
      <div className="flex flex-col items-start gap-1">
        <CopyCmd cmd={act.copyCommand} compact />
        <span className="text-2xs text-faint">The owner action is not allowlisted for console execution.</span>
      </div>
    );
  }

  const stateClass = state === "pending" ? "action-pending" : state === "confirmed" ? "action-confirmed" : state === "error" ? "action-error" : "";
  const chosen = options.find((option) => option.value === selectedOption) ?? null;
  const buttonLabel = state === "pending"
    ? (act.mode === "bespoke" && act.supportsPreview ? "Preparing preview…" : "Working…")
    : act.mode === "bespoke" && act.supportsPreview
      ? selectedOption ? "Review decision" : "Choose a decision"
      : options.length > 1
        ? selectedOption ? "Review decision" : "Choose a decision"
        : chosen?.label ?? act.label;
  const confirmationFields = Object.entries(confirmationFieldValues);

  return (
    <div className="flex flex-col gap-3">
      <form
        key={actIdentity}
        ref={formRef}
        className="flex flex-col gap-3"
        onSubmit={(event) => event.preventDefault()}
        onInput={() => { setInvalidField(null); if (state === "error") { setState("idle"); setMessage(null); } }}
      >
        {options.length > 1 ? (
          <fieldset
            aria-invalid={invalidField === "decision_option" || undefined}
            aria-describedby={invalidField === "decision_option" ? validationMessageId : undefined}
          >
            <legend className="mb-1 text-2xs font-bold uppercase text-faint">Your decision</legend>
            <div className="grid gap-1.5">
              {options.map((option) => {
                const selected = selectedOption === option.value;
                return (
                  <label
                    key={option.value}
                    className={`grid cursor-pointer grid-cols-[auto_1fr] gap-x-3 gap-y-1 rounded-md border px-3 py-2 transition-colors ${
                      selected ? "border-work-line bg-work-soft" : "border-hair bg-panel hover:bg-soft"
                    }`}
                  >
                    <input
                      type="radio"
                      name="decision_option"
                      value={option.value}
                      checked={selected}
                      disabled={state === "pending"}
                      onChange={() => { setSelectedOption(option.value); setPreviewResult(null); setInvalidField(null); setMessage(null); }}
                      className="mt-0.5 h-4 w-4 accent-current"
                    />
                    <span className="text-sm font-semibold text-ink">{option.label}</span>
                    {selected && option.description ? (
                      <span className="col-start-2 text-xs leading-relaxed text-muted">{option.description}</span>
                    ) : null}
                  </label>
                );
              })}
            </div>
          </fieldset>
        ) : null}

        {visibleFields.length ? (
          <div className="grid gap-3 border-t border-hair pt-3">
            {visibleFields.map((field) => (
              <Field
                key={field.name}
                field={field}
                disabled={state === "pending"}
                invalid={invalidField === field.name}
                describedBy={invalidField === field.name ? validationMessageId : undefined}
                errorMessage={invalidField === field.name ? message ?? undefined : undefined}
              />
            ))}
          </div>
        ) : null}
      </form>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="primary"
          disabled={!mutationsEnabled || state === "pending" || (options.length > 1 && !selectedOption)}
          onClick={() => void trigger()}
          className={stateClass}
          title={mutationsEnabled ? undefined : "Launch via 'compound console --operator' to act from a pinned console."}
        >
          {buttonLabel}
        </Button>
      </div>
      {/* A field-level error already renders inline next to its own Field control
          (above) — this banner only covers non-field errors: the decision-option
          choice, or a server-side failure surfaced after submit. */}
      {!open && state === "error" && message && !(invalidField && invalidField !== "decision_option") ? (
        <span id={validationMessageId} role="alert" className="text-xs leading-snug text-crit">{message}</span>
      ) : null}
      {outcome ? <DecisionOutcome result={outcome} effectKind={effectKind} /> : null}

      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={`Confirm ${chosen?.label ?? act.label}`}
        body={authority?.wording ?? confirmationDescription(act, chosen)}
        confirmLabel="Confirm decision"
        pending={state === "pending"}
        onConfirm={() => void submit("confirm", Object.fromEntries(confirmationFields))}
      >
        <div className="grid gap-3">
          {authority ? <AuthorityFaceDetails authority={authority} embedded /> : null}

          {previewResult ? <DecisionOutcome result={previewResult} preview effectKind={effectKind} /> : null}

          {act.mode === "batch-atomic" ? (
            <div className="rounded-md border border-seam-line bg-seam-soft px-3 py-2 text-xs leading-relaxed text-seam">
              Applies to all {act.memberCount} members together. Members cannot be excluded individually.
            </div>
          ) : null}

          {confirmationFields.length ? (
            <section className="rounded-md border border-work-line bg-work-soft px-3 py-2.5" aria-label="Values to submit">
              <h3 className="text-2xs font-bold uppercase tracking-wide text-work">Values to submit</h3>
              <dl className="mt-2 grid gap-2">
                {confirmationFields.map(([name, value]) => {
                  const label = fields.find((field) => field.name === name)?.label ?? name.replaceAll("_", " ");
                  return (
                    <div key={name} className="grid grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)] gap-3 text-xs">
                      <dt className="font-semibold capitalize text-muted" title={name}>{label}</dt>
                      <dd className="min-w-0 text-ink">
                        <code className="mono whitespace-pre-wrap break-words">{value}</code>
                      </dd>
                    </div>
                  );
                })}
              </dl>
            </section>
          ) : null}

          {(act.mode !== "generic" && act.policyNote) || act.argv?.length ? (
            <details className="rounded-md border border-hair bg-soft px-3 py-2">
              <summary className="cursor-pointer text-xs font-semibold text-muted">Technical details</summary>
              {act.mode !== "generic" && act.policyNote ? (
                <p className="mt-2 text-xs leading-relaxed text-muted">{act.policyNote}</p>
              ) : null}
              {act.argv?.length ? (
                <pre className="mono mt-2 overflow-x-auto border-t border-hair pt-2 text-2xs leading-relaxed text-muted">
                  compound {act.argv.join(" ")}
                </pre>
              ) : null}
            </details>
          ) : null}

          {state === "error" && message ? (
            <div id={validationMessageId} role="alert" aria-live="assertive" className="rounded-md border border-crit-line bg-crit-soft px-3 py-2 text-xs leading-relaxed text-crit">
              {message}
            </div>
          ) : null}
        </div>
      </ConfirmDialog>
    </div>
  );
}
