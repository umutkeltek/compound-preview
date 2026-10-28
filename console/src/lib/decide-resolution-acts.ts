import type { DecideAct, DecideGroup } from "@/lib/decide-model";

type GenericDecideAct = Extract<DecideAct, { mode: "generic" }>;

/** Authority-bound copy faces explain a gate; they never advertise a bypass command. */
export function manualCopyCommandFor(group: DecideGroup): string | null {
  return !group.authorityBound && group.act.mode === "copy" ? group.act.command : null;
}

// Generic resolution controls are derived at render time so the serialized
// DecideGroup stays unchanged. Only an honest-copy card representing exactly
// one Reviews-backed item qualifies; Attention-only and aggregate cards get none.
// The synthetic acts execute through the fixed server-side review-resolution
// adapter (argv is rebuilt server-side) and carry the copy-only owner revision
// that the server must revalidate against a live projection before recording.
//
// This lives apart from decide-model.ts on purpose: it is imported by client
// components, and decide-model's module graph reaches node:child_process via
// @/lib/compound — type-only imports keep this file client-bundle-safe.
export function genericReviewResolutionActs(group: DecideGroup): GenericDecideAct[] {
  if (
    group.authorityBound ||
    group.act.mode !== "copy" ||
    !group.reviewResolutionItemId ||
    !group.reviewResolutionRevision
  ) return [];
  const copyCommand = group.act.command;
  return [{
    mode: "generic",
    area: group.area,
    itemId: group.reviewResolutionItemId,
    actionId: "review_resolution_park",
    expectedRevision: group.reviewResolutionRevision,
    label: "Review disposition",
    argv: null,
    decisionOptions: [
      {
        value: "review_resolution_park",
        label: "Park",
        tone: "orange",
        description: "Files this review as parked with your reason. The underlying authority stays unchanged, and the disposition can be undone.",
      },
      {
        value: "review_resolution_dismiss",
        label: "Dismiss",
        tone: "gray",
        description: "Files this review as dismissed with your reason. The underlying authority stays unchanged, and the disposition can be undone.",
      },
    ],
    defaultOption: "review_resolution_park",
    requiresConfirmation: true,
    copyCommand,
    effectKind: "record",
    reasonRequired: true,
  }];
}
