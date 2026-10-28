/*
 * Falsifier for the rendered EvidenceCard component. Proves the markup a
 * human sees: observation first, provenance separate from record identity,
 * raw values behind a labeled disclosure, honest gap lines, and no invented
 * verification or decision reference.
 *
 * Run from console/: `bun scripts/evidence-card-smoke.mjs`
 */

import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { EvidenceCard } from "@/components/memory/EvidenceCard";
import type { MemoryCandidate } from "@/lib/memory-data";

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

export function runEvidenceCardRenderSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];

  // 1) Object-bearing candidate with provenance.
  const objectText = "Invoice INV-1042 paid 2026-04-01 by wire transfer.";
  {
    const candidate = candidateWith({
      object: objectText,
      predicate: "paid_invoice",
      source_trace_ref: "ev_aaa",
      evidence_refs: ["ev_aaa", "ev_bbb"],
      qualifiers: {
        source: "knowledge_import",
        source_kind: "scenario",
        item_ref: "knowledge://crm/invoice/INV-1042",
      },
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    const visible = html.slice(0, html.indexOf("<details"));
    push(checks, "closed-card-keeps-record-identifiers-in-details",
      !visible.includes("cand_fixture") && !visible.includes("paid_invoice"), visible);
    push(checks, "closed-card-explains-evidence-without-queue-machinery",
      visible.includes("Source record") && !visible.includes("decision reference") && !visible.includes("review queue"), visible);
    push(
      checks,
      "lead-object-rendered-first",
      html.includes(objectText) && html.indexOf(objectText) < html.indexOf("paid_invoice"),
      "object text must render before raw predicate in the markup",
    );
    push(
      checks,
      "no-doubled-predicate-object-echo-in-markup",
      !html.includes("paid_invoice: Invoice") && !html.includes("paid_invoice:&nbsp;Invoice"),
      html,
    );
    push(
      checks,
      "explicit-evidence-not-fact-note-in-markup",
      html.includes("Source record") && html.includes("not verified fact"),
      html,
    );
    push(
      checks,
      "raw-provenance-behind-details-disclosure",
      html.includes("<details") && html.includes("ev_aaa") && html.includes("ev_bbb"),
      "raw provenance must live behind the labeled disclosure",
    );
    push(
      checks,
      "technical-details-labeled-and-exact",
      html.includes("technical details") &&
        html.includes("predicate: paid_invoice") &&
        html.includes(`object: ${objectText}`),
      html,
    );
    push(
      checks,
      "human-source-labels-rendered",
      html.includes("knowledge import") && html.includes("scenario"),
      html,
    );
    push(
      checks,
      "no-visible-provenance-gap-when-provenance-exists",
      !html.includes("Source link unavailable"),
      html,
    );
  }

  // 2) Identified row with no provenance: the card explains the source link
  // is unavailable instead of hiding the gap behind the record id.
  {
    const candidate = candidateWith({
      id: "cand_identified",
      object: objectText,
      predicate: "paid_invoice",
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    push(
      checks,
      "no-provenance-renders-visible-gap-line",
      html.includes("Source link unavailable for this row"),
      html,
    );
    push(
      checks,
      "no-provenance-record-id-still-shown-as-identity",
      html.includes("cand_identified"),
      html,
    );
  }

  // 3) Missing object text: the observation gap is stated; no pointer is
  // dressed up as an observation.
  {
    const candidate = candidateWith({
      entity: "knowledge://docs/spec.md",
      qualifiers: { item_ref: "knowledge://docs/spec.md" },
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    push(
      checks,
      "missing-object-renders-observation-gap",
      html.includes("Observation text is unavailable for this row."),
      html,
    );
    push(
      checks,
      "missing-object-does-not-lead-with-pointer",
      html.indexOf("Observation text is unavailable") < html.indexOf("knowledge://docs/spec.md"),
      "the gap line must lead, pointers stay in details",
    );
  }

  // 4) Novel source kind: human label, raw slug only in technical details.
  {
    const candidate = candidateWith({
      object: objectText,
      qualifiers: { source_kind: "support_ticket_export" },
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    push(
      checks,
      "novel-source-kind-renders-human-label-pill",
      html.includes("Source evidence"),
      html,
    );
    push(
      checks,
      "novel-source-kind-raw-slug-only-in-details",
      html.includes("source_kind: support_ticket_export") &&
        !html.includes(">support_ticket_export<"),
      html,
    );
  }

  // 5) Parked candidate: honest label, no invented verification badge.
  {
    const candidate = candidateWith({
      state: "parked",
      predicate: "promotable_to_parked",
      object: "cand_parked",
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    push(
      checks,
      "parked-candidate-not-promoted-in-card",
      !html.toLowerCase().includes("verified memory") &&
        !html.toLowerCase().includes("promoted to canon") &&
        html.includes("parked source evidence"),
      html,
    );
  }

  // 6) Promoted candidate: the note reports the recorded status and cites no
  // reviewed decision, and never says "not promoted".
  {
    const candidate = candidateWith({
      state: "promoted",
      object: "promoted object",
      predicate: "promoted_predicate",
    });
    const html = renderToStaticMarkup(createElement(EvidenceCard, { candidate }));
    push(
      checks,
      "promoted-candidate-note-reports-recorded-status",
      html.includes("Recorded status: promoted.") && html.includes("promoted source evidence"),
      html,
    );
    push(
      checks,
      "promoted-candidate-note-invents-no-decision",
      !html.includes("reviewed decision") && !html.toLowerCase().includes("not promoted"),
      html,
    );
  }

  return {
    ok: checks.every((check) => check.ok),
    details: checks.map((check) => `${check.ok ? "ok" : "FAIL"} ${check.name}: ${check.detail}`),
  };
}
