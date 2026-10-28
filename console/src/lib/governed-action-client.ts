import type { HeadlineActDescriptor } from "./briefing";
import type { DecideAct } from "./decide-model";
import type { FieldSpec } from "./decision-registry";

export type ClientDecideAct = Extract<DecideAct, { mode: "bespoke" | "batch-atomic" | "generic" }>;
export type GovernedSubmitKind = "preview" | "direct" | "confirm";

function confirmedFor(kind: GovernedSubmitKind): boolean {
  return kind === "confirm";
}

/**
 * Plain-language copy for a required field the operator left empty — the
 * fix for the silent no-op: clicking the primary act with an empty required
 * field (most often the rationale/reason textbox) must show this message
 * right next to the field, not just block the click invisibly.
 */
export const RATIONALE_REQUIRED_MESSAGE =
  "Add one line of reasoning first — decisions are recorded with their why.";

export function fieldValidationMessage(field: Pick<FieldSpec, "name" | "label">): string {
  if (field.name === "rationale" || field.name === "reason") return RATIONALE_REQUIRED_MESSAGE;
  return `${field.label} is required before you can continue.`;
}

export function decideActIdentity(act: ClientDecideAct): string {
  if (act.mode === "bespoke") {
    return JSON.stringify([
      act.mode,
      act.area,
      act.itemId,
      act.expectedRevision,
      act.actionIntentId ?? "",
      act.expectedAuthorityRevision ?? "",
      act.options.map((option) => option.value),
    ]);
  }
  if (act.mode === "generic") {
    return JSON.stringify([
      act.mode,
      act.area,
      act.itemId,
      act.actionId,
      act.decisionOptions?.map((option) => option.value) ?? [],
      act.expectedRevision,
      act.actionIntentId ?? "",
      act.expectedAuthorityRevision ?? "",
      act.argv,
      act.requiresConfirmation,
      act.effectKind,
    ]);
  }
  return JSON.stringify([
    act.mode,
    act.area,
    act.itemId,
    act.option,
    act.actionIntentId ?? "",
    act.expectedAuthorityRevision ?? "",
    act.argv,
    act.requiresConfirmation,
  ]);
}

export function headlineActIdentity(act: HeadlineActDescriptor): string {
  return JSON.stringify([
    act.pathway,
    act.area,
    act.itemId,
    act.option ?? "",
    act.actionId ?? "",
    act.expectedRevision ?? "",
    act.actionIntentId ?? "",
    act.expectedAuthorityRevision ?? "",
    act.argv,
    act.requiresConfirmation,
    act.rationaleRequired,
  ]);
}

export function buildDecideActionRequest(
  act: ClientDecideAct,
  selectedOption: string,
  fields: Record<string, string>,
  kind: GovernedSubmitKind,
): Record<string, unknown> {
  const confirmed = confirmedFor(kind);
  if (act.mode === "generic") {
    const selectedAction = act.decisionOptions?.some((option) => option.value === selectedOption)
      ? selectedOption
      : act.actionId;
    const request: Record<string, unknown> = {
      area: act.area,
      itemId: act.itemId,
      actionId: selectedAction,
      expectedRevision: act.expectedRevision,
      actionIntentId: act.actionIntentId ?? "",
      expectedAuthorityRevision: act.expectedAuthorityRevision ?? "",
      confirmed,
    };
    // The fixed review-resolution adapter is the only generic consumer of a
    // reason; it validates presence server-side, so send it only when asked.
    if (act.reasonRequired) request.reason = String(fields.reason ?? "");
    return request;
  }
  return {
    area: act.area,
    itemId: act.itemId,
    option: act.mode === "bespoke" ? selectedOption : act.option,
    fields,
    expectedRevision: act.mode === "bespoke" ? act.expectedRevision : "",
    actionIntentId: act.actionIntentId ?? "",
    expectedAuthorityRevision: act.expectedAuthorityRevision ?? "",
    preview: kind === "preview",
    confirmed,
  };
}

export function buildHeadlineActionRequest(
  act: HeadlineActDescriptor,
  rationale: string,
  kind: Exclude<GovernedSubmitKind, "preview">,
): Record<string, unknown> {
  const confirmed = confirmedFor(kind);
  if (act.pathway === "bespoke") {
    return {
      area: act.area,
      itemId: act.itemId,
      option: act.option,
      fields: act.fields ?? {},
      actionIntentId: act.actionIntentId ?? "",
      expectedAuthorityRevision: act.expectedAuthorityRevision ?? "",
      confirmed,
    };
  }
  return {
    area: act.area,
    itemId: act.itemId,
    actionId: act.actionId,
    expectedRevision: act.expectedRevision ?? "",
    actionIntentId: act.actionIntentId ?? "",
    expectedAuthorityRevision: act.expectedAuthorityRevision ?? "",
    confirmed,
    rationale,
  };
}

export function buildEnumerableActionRequest(
  area: string,
  member: Pick<
    import("./decide-model").DecideMember,
    "id" | "actionId" | "expectedRevision" | "actionIntentId" | "expectedAuthorityRevision"
  >,
  confirmed: boolean,
  rationale: string,
): Record<string, unknown> {
  return {
    area,
    itemId: member.id,
    actionId: member.actionId,
    expectedRevision: member.expectedRevision,
    actionIntentId: member.actionIntentId ?? "",
    expectedAuthorityRevision: member.expectedAuthorityRevision ?? "",
    confirmed,
    rationale,
  };
}
