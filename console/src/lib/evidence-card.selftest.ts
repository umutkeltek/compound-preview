/*
 * Falsifier for the pure evidence-card presentation helper. Proves the view
 * never invents content: the object text leads or an honest gap line does,
 * provenance stays separate from record identity, unknown kinds get a human
 * label with the raw value preserved, and the notice never claims a decision.
 *
 * Test: `bun scripts/evidence-card-smoke.mjs` (from console/)
 */

import type { MemoryCandidate } from "@/lib/memory-data";
import {
  buildEvidenceCardObservation,
  type EvidenceCardObservation,
} from "@/lib/evidence-card";

type Check = { name: string; ok: boolean; detail: string };

function candidateWith(fields: Record<string, unknown>): MemoryCandidate {
  return {
    id: "cand_fixture",
    ...fields,
  } as MemoryCandidate;
}

function push(checks: Check[], name: string, ok: boolean, detail = ""): void {
  checks.push({ name, ok, detail });
}

function detailValue(view: EvidenceCardObservation, label: string): string {
  return view.technicalDetails.find((d) => d.label === label)?.value ?? "";
}

export function runEvidenceCardSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];

  // 1) Object-bearing candidate: the object's text leads exactly once and is
  // never echoed as a "predicate: object" string.
  const objectText = "Invoice INV-1042 paid 2026-04-01 by wire transfer.";
  {
    const candidate = candidateWith({
      object: objectText,
      predicate: "paid_invoice",
      source_trace_ref: "ev_aaa",
      evidence_refs: ["ev_aaa", "ev_bbb"],
      qualifiers: {
        source: "knowledge",
        source_kind: "scenario",
        item_ref: "knowledge://crm/invoice/INV-1042",
      },
    });
    const view = buildEvidenceCardObservation(candidate);
    push(
      checks,
      "object-bearer-leads-with-object-text",
      view.observation === objectText,
      view.observation,
    );
    push(
      checks,
      "object-bearer-never-echoes-predicate-prefix",
      !view.observation.includes("paid_invoice:"),
      view.observation,
    );
    push(
      checks,
      "object-bearer-preserves-exact-predicate-and-object",
      detailValue(view, "predicate") === "paid_invoice" && detailValue(view, "object") === objectText,
      JSON.stringify(view.technicalDetails),
    );
    push(
      checks,
      "object-bearer-provenance-refs-present",
      view.refs.includes("ev_aaa") &&
        view.refs.includes("ev_bbb") &&
        view.refs.includes("knowledge://crm/invoice/INV-1042") &&
        view.provenanceMissing === false,
      view.refs.join(","),
    );
  }

  // 2) Record identity is not provenance: a real identified row with no
  // source links must surface the gap instead of counting its own id.
  {
    const view = buildEvidenceCardObservation(
      candidateWith({ id: "cand_identified", object: objectText, predicate: "paid_invoice" }),
    );
    push(
      checks,
      "identified-row-without-provenance-flags-gap",
      view.provenanceMissing === true && view.refs.length === 0,
      `provenanceMissing=${view.provenanceMissing} refs=${JSON.stringify(view.refs)}`,
    );
    push(
      checks,
      "record-id-lives-in-technical-details",
      detailValue(view, "id") === "cand_identified" && !view.refs.includes("cand_identified"),
      `details=${JSON.stringify(view.technicalDetails)} refs=${JSON.stringify(view.refs)}`,
    );
  }

  // 3) Missing object text is a stated gap — an item ref, entity, or id is a
  // pointer, never a human observation.
  {
    const view = buildEvidenceCardObservation(
      candidateWith({
        predicate: "observed",
        entity: "knowledge://docs/spec.md",
        qualifiers: { item_ref: "knowledge://docs/spec.md" },
      }),
    );
    push(
      checks,
      "missing-object-says-observation-unavailable",
      view.observation === "Observation text is unavailable for this row.",
      view.observation,
    );
    push(
      checks,
      "missing-object-does-not-display-pointer-as-observation",
      !view.observation.includes("knowledge://") && !view.observation.includes("cand_"),
      view.observation,
    );
    push(
      checks,
      "missing-object-keeps-pointers-as-technical-details",
      detailValue(view, "entity") === "knowledge://docs/spec.md",
      JSON.stringify(view.technicalDetails),
    );
  }

  // 4) Unknown source kinds get the honest human label, never a raw slug; the
  // raw qualifier survives verbatim in technical details.
  {
    const view = buildEvidenceCardObservation(
      candidateWith({
        object: objectText,
        qualifiers: { source_kind: "support_ticket_export" },
      }),
    );
    push(
      checks,
      "novel-source-kind-gets-human-label",
      view.sourceLabels.length === 1 && view.sourceLabels[0] === "Source evidence",
      view.sourceLabels.join(","),
    );
    push(
      checks,
      "novel-source-kind-raw-value-preserved-in-details",
      detailValue(view, "source_kind") === "support_ticket_export",
      JSON.stringify(view.technicalDetails),
    );
  }

  // 5) Known source kinds still map to their human labels.
  {
    const view = buildEvidenceCardObservation(
      candidateWith({
        object: objectText,
        qualifiers: { source: "knowledge_import", source_kind: "scenario" },
      }),
    );
    push(
      checks,
      "known-source-kinds-keep-human-labels",
      view.sourceLabels.includes("knowledge import") && view.sourceLabels.includes("scenario"),
      view.sourceLabels.join(","),
    );
    push(
      checks,
      "source-labels-capped-to-three",
      view.sourceLabels.length <= 3,
      view.sourceLabels.join(","),
    );
  }

  // 6) Kind labels follow the recorded state; unknown states stay honest.
  for (const [state, want] of [
    ["promoted", "promoted source evidence"],
    ["parked", "parked source evidence"],
    ["rejected", "rejected source evidence"],
    ["weird-state", "source evidence"],
    ["", "source evidence"],
  ] as const) {
    const view = buildEvidenceCardObservation(candidateWith({ state, object: objectText }));
    push(
      checks,
      `kind-label-state-${state || "<empty>"}`,
      view.kindLabel === want,
      `state=${state} got=${view.kindLabel} want=${want}`,
    );
  }

  // 7) The notice is a source-record/evidence-not-fact notice plus the
  // recorded status. It never invents a reviewed decision, never claims the
  // row awaits review (this card sits outside the queue), and never calls a
  // promoted row "not promoted".
  {
    const promoted = buildEvidenceCardObservation(candidateWith({ state: "promoted", object: objectText }));
    push(
      checks,
      "promoted-note-shows-recorded-status",
      promoted.note.includes("Recorded status: promoted."),
      promoted.note,
    );
    push(
      checks,
      "promoted-note-is-evidence-not-fact",
      promoted.note.includes("Source record") && promoted.note.includes("not verified fact"),
      promoted.note,
    );
    push(
      checks,
      "promoted-note-invents-no-decision-reference",
      !promoted.note.includes("reviewed decision"),
      promoted.note,
    );
    push(
      checks,
      "promoted-note-never-says-not-promoted",
      !promoted.note.includes("not promoted"),
      promoted.note,
    );
    push(
      checks,
      "note-never-claims-row-awaits-review",
      !promoted.note.toLowerCase().includes("await"),
      promoted.note,
    );
    const parked = buildEvidenceCardObservation(candidateWith({ state: "parked", object: objectText }));
    push(
      checks,
      "parked-note-shows-recorded-status",
      parked.note.includes("Recorded status: parked."),
      parked.note,
    );
    const unrecorded = buildEvidenceCardObservation(candidateWith({ object: objectText }));
    push(
      checks,
      "missing-state-note-is-honest",
      unrecorded.note.includes("No recorded status"),
      unrecorded.note,
    );
    push(
      checks,
      "note-present-on-every-path",
      buildEvidenceCardObservation(candidateWith({ state: "rejected", object: objectText })).note.length > 0,
      "note must never be empty",
    );
  }

  // 8) Exact raw provenance is preserved verbatim — never truncated,
  // lowercased, or hashed. The id stays out of it.
  {
    const view = buildEvidenceCardObservation(
      candidateWith({
        id: "cand_raw",
        predicate: "P",
        object: "O",
        source_trace_ref: "trace:exact/with/slashes",
        evidence_refs: ["ev_X1", "ev_X2"],
        qualifiers: {
          item_ref: "knowledge://exact/path/with/spaces and CAPS",
          extraction_id: "kx_exact",
        },
      }),
    );
    for (const raw of [
      "trace:exact/with/slashes",
      "ev_X1",
      "ev_X2",
      "knowledge://exact/path/with/spaces and CAPS",
      "kx_exact",
    ]) {
      push(
        checks,
        `raw-ref-preserved-${raw.replace(/[^a-z0-9]/gi, "_")}`,
        view.refs.includes(raw),
        view.refs.join("|"),
      );
    }
    push(
      checks,
      "raw-ref-list-excludes-record-id",
      !view.refs.includes("cand_raw") && detailValue(view, "id") === "cand_raw",
      `refs=${view.refs.join("|")}`,
    );
  }

  // 9) Minimal candidate: no crash, honest gap lines, no invented content.
  {
    const view = buildEvidenceCardObservation(candidateWith({}));
    push(
      checks,
      "minimal-candidate-states-observation-gap",
      view.observation === "Observation text is unavailable for this row.",
      view.observation,
    );
    push(
      checks,
      "minimal-candidate-states-provenance-gap",
      view.provenanceMissing === true,
      `provenanceMissing=${view.provenanceMissing}`,
    );
    push(
      checks,
      "minimal-candidate-honest-kind-label",
      view.kindLabel === "source evidence" && view.sourceLabels.length === 0,
      `${view.kindLabel}/${view.sourceLabels.join(",")}`,
    );
  }

  {
    const candidate = candidateWith({
      object: "  Exact observation\ntext  ",
      predicate: " exact_predicate ",
      source: "source_v2",
      qualifiers: { source_kind: "novel_kind", confidence_note: "not checked", custom: { flag: false } },
    });
    const view = buildEvidenceCardObservation(candidate);
    push(checks, "raw-object-and-predicate-preserve-whitespace",
      detailValue(view, "object") === candidate.object && detailValue(view, "predicate") === candidate.predicate);
    push(checks, "complete-raw-record-retains-unrecognized-fields",
      detailValue(view, "raw record") === JSON.stringify(candidate, null, 2));
  }

  return {
    ok: checks.every((check) => check.ok),
    details: checks.map((check) => `${check.ok ? "ok" : "FAIL"} ${check.name}: ${check.detail}`),
  };
}
