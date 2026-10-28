/**
 * Representation cluster v1 falsifiers (packet 2026-07-11-representation-cluster-v1).
 * These encode the CORRECT behaviour for the five recorded playtest findings
 * (pf1-pf5). Each was RED on the pre-cluster console; this suite is the depth
 * test that the cluster actually kills them — not a typecheck. Fixture ids are
 * synthetic (privacy: no ledger ids in tracked files). Run under `bun` (shapes
 * are `.tsx`; FR4 renders to markup):
 * `scripts/representation-falsifiers-smoke.mjs`.
 *
 * FR5 (the cluster import boundary) is a filesystem guard and lives in its own
 * runner (`scripts/representation-cluster-guard-smoke.mjs`) — it cannot be an
 * in-module selftest without importing the very thing it forbids.
 */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { LatestMoment } from "@/components/briefing/LatestMoment";
import {
  actRequestFromAction,
  boardHeadline,
  homeAccount,
  Instrument,
  instrumentActRequest,
  latestProofBackedChange,
  ledgerStateFrom,
  renderRepresentation,
  resolveCard,
  resolveStateChip,
  type InstrumentSetting,
  type RepresentationCard,
} from "./index";

export type SelfTestResult = { ok: boolean; details: string[] };

export function runRepresentationFalsifiers(): SelfTestResult {
  const details: string[] = [];
  let ok = true;
  const check = (cond: boolean, pass: string, fail: string) => {
    if (cond) {
      details.push(`ok: ${pass}`);
    } else {
      ok = false;
      details.push(`FAIL: ${fail}`);
    }
  };

  // ---------- FR1 (pf1 kill): a state chip is a pure function of the server's
  // receipt-or-error result — NEVER of client optimism. ----------
  {
    // server unreachable / act failed -> chip does NOT move, error is visible.
    const failed = resolveStateChip("proposed", { ok: false, error: "server unreachable" });
    check(
      failed.state === "proposed" && failed.error !== null && failed.receiptRef === null,
      "FR1: failed act leaves the chip at 'proposed' with a visible error",
      `FR1: failed act mutated the chip to "${failed.state}" (error=${failed.error})`,
    );

    // the exact pf1 shape: an "ok" response that carries NO receipt is a
    // phantom resolution — the chip must stay put, not flip to the new state.
    const phantom = resolveStateChip("proposed", { ok: true, receipt_ref: "", new_state: "admitted" });
    check(
      phantom.state === "proposed" && phantom.error !== null && phantom.receiptRef === null,
      "FR1: receipt-less success is a phantom — chip stays 'proposed'",
      `FR1: receipt-less success advanced the chip to "${phantom.state}" (pf1 regression)`,
    );

    // a real receipted success advances the chip, backed by the receipt.
    const landed = resolveStateChip("proposed", { ok: true, receipt_ref: "rec_abc", new_state: "admitted" });
    check(
      landed.state === "admitted" && landed.error === null && landed.receiptRef === "rec_abc",
      "FR1: receipted success advances the chip to 'admitted' with a receipt ref",
      `FR1: receipted success failed to advance (state=${landed.state}, ref=${landed.receiptRef})`,
    );

    // ledger-diff invariant: the UI-reported state == the ledger state for every
    // outcome. Because the chip only advances on a receipt, they can never
    // diverge (this is the structural fix for pf1's "resolved 3, ledger shows 1").
    const cases = [
      { current: "proposed", result: { ok: false as const, error: "x" } },
      { current: "proposed", result: { ok: true as const, receipt_ref: "", new_state: "admitted" } },
      { current: "proposed", result: { ok: true as const, receipt_ref: "rec_1", new_state: "admitted" } },
    ];
    const coherent = cases.every(
      (c) => resolveStateChip(c.current, c.result).state === ledgerStateFrom(c.current, c.result),
    );
    check(coherent, "FR1: UI-reported chip state == ledger state for every outcome", "FR1: UI chip diverged from the ledger state");
  }

  // ---------- FR2 (pf2): the home number IS the count of actionable cards;
  // resolving one decrements BOTH, in lockstep. ----------
  {
    const cards: RepresentationCard[] = [
      { id: "work_a", band: "needs_you", actionable: true },
      { id: "work_b", band: "needs_you", actionable: true },
      { id: "work_c", band: "watching", actionable: false }, // not actionable -> not counted
      { id: "work_d", band: "needs_you", actionable: true },
    ];
    const acct = homeAccount(cards);
    check(
      acct.number === 3 && acct.actionable.length === 3 && acct.number === acct.actionable.length,
      "FR2: home number (3) == rendered actionable cards (3)",
      `FR2: home number ${acct.number} != actionable ${acct.actionable.length}`,
    );

    const after = resolveCard(acct, "work_a");
    check(
      after.number === 2 && after.actionable.length === 2 && after.number === after.actionable.length,
      "FR2: resolving one decrements BOTH the number and the list to 2",
      `FR2: after resolve, number ${after.number} vs actionable ${after.actionable.length} (pf2 incoherence)`,
    );

    // resolving a non-actionable / unknown id must not change the count.
    const noop = resolveCard(acct, "work_c");
    check(noop.number === acct.number, "FR2: resolving a non-actionable card does not change the number", "FR2: a non-actionable resolve moved the count");
  }

  // ---------- FR3 (pf3): a success predicate on an UNCLOSED envelope is labelled
  // as a goal, never rendered as an accomplished fact; unbacked running/done
  // status renders claim-styled (dashed) with a last-receipt age. ----------
  {
    const unclosed = boardHeadline({
      id: "work_examplealpha",
      subject: "svc-alpha",
      state: "needs_claim",
      success_predicate: "crash_loop divergence remediated: probe shows active_running",
      receipt_refs: [],
    });
    check(
      unclosed.predicateLabeled && !unclosed.rendersAsFact,
      "FR3: unclosed envelope labels its success_predicate as a goal (not a fact)",
      `FR3: unclosed predicate rendered as fact (labeled=${unclosed.predicateLabeled}, asFact=${unclosed.rendersAsFact})`,
    );
    check(
      !unclosed.text.startsWith("crash_loop divergence remediated"),
      "FR3: headline text does not open with the bare predicate as a completed statement",
      `FR3: headline opens with the raw predicate: "${unclosed.text}"`,
    );
    check(
      unclosed.claimStyled && unclosed.lastReceiptAge !== null,
      "FR3: an unbacked needs_claim status renders claim-styled with a last-receipt age",
      "FR3: unbacked status did not render claim-styled with a receipt age",
    );

    // control: a genuinely closed envelope WITH receipts may render as fact.
    const closed = boardHeadline({
      id: "work_done",
      subject: "svc-gamma",
      state: "completed",
      success_predicate: "migration applied",
      receipt_refs: ["rec_x", "rec_y"],
      last_receipt_at: new Date().toISOString(),
    });
    check(closed.rendersAsFact && !closed.claimStyled, "FR3: closed+receipted envelope may render as fact", "FR3: closed+receipted envelope refused to render as fact");
  }

  // ---------- FR4 (pf5/a11y): an actionable representation exposes its
  // affordance as a real interactive element in the accessibility tree — never
  // a <details>/<summary>-only disclosure. ----------
  {
    const rep = renderRepresentation({
      id: "work_examplebeta",
      subject: "svc-beta",
      state: "needs_claim",
      summary: "svc-beta disabled; re-enable requires your call",
      receipt_refs: [],
      action: {
        area: "workbench",
        itemId: "work_examplebeta",
        actionId: "claim",
        label: "Claim",
        requiresConfirmation: false,
        effect: "record",
        consequence: "Assigns this work to you",
        reversibility: "reversible",
      },
    });
    check(rep.ok, "FR4: an actionable evidence composes to a representation", `FR4: renderRepresentation returned an error (${rep.ok ? "" : rep.error})`);
    if (rep.ok) {
      const html = renderToStaticMarkup(rep.element);
      const hasButton = /<button\b/.test(html) || /role="button"/.test(html);
      const summaryOnly = /<summary\b/.test(html) && !hasButton;
      check(hasButton, "FR4: the actionable affordance renders as a real <button> (in the a11y tree)", "FR4: no button/role=button in the rendered actionable representation");
      check(!summaryOnly, "FR4: the affordance is not hidden inside a <summary>-only disclosure", "FR4: the only affordance sits in a <details>/<summary> (a11y-invisible)");
    }
  }

  // ---------- Cluster act-seam + instruments (settled decisions #1 & #3) ----------
  {
    // The ONE way an act leaves the cluster is a typed ActRequest.
    const req = actRequestFromAction({
      area: "workbench",
      itemId: "work_x",
      actionId: "claim",
      label: "Claim",
      requiresConfirmation: false,
      effect: "record",
      consequence: "Assigns this work to you",
      reversibility: "reversible",
    });
    check(
      req.area === "workbench" && req.actionId === "claim" && req.effect === "record",
      "SEAM: actRequestFromAction emits a typed ActRequest (the only act exit)",
      "SEAM: ActRequest was malformed",
    );

    // Turning an instrument mints a record that SUPERSEDES the prior one, through
    // the seam — receipted and undoable (SD3).
    const base: InstrumentSetting = {
      recordId: "claim_examplejobhealth",
      label: "Auto-admit job_health",
      kind: "switch",
      area: "policy",
      itemId: "grant_job_health",
      actionId: "set",
      requiresConfirmation: true,
      value: true,
    };
    const inst = instrumentActRequest(base, false);
    check(
      inst.supersedes === "claim_examplejobhealth" && inst.effect === "record" && inst.requiresConfirmation === true,
      "SD3: turning an instrument mints a superseding decision record via the seam",
      "SD3: instrument act-request missing supersedes/record/confirm",
    );

    // Honest degradation: the counterfactual line appears ONLY when supplied
    // (computed from receipts) — never fabricated (pf3).
    const withCf = renderToStaticMarkup(createElement(Instrument, { setting: { ...base, counterfactual: "would save ~4 admits/day" } }));
    const withoutCf = renderToStaticMarkup(createElement(Instrument, { setting: base }));
    check(
      withCf.includes("would save") && !withoutCf.includes("would save"),
      "SD3: counterfactual preview shown only when computed (honest degradation)",
      "SD3: counterfactual was fabricated when absent, or dropped when present",
    );
  }

  // FR6: a receipt-backed change does not prove later reuse or benefit.
  {
    const eligible = {
      work_id: "work_examplemoment",
      state: "completed",
      changed: true,
      move_type: "rule_delta",
      claimed_by: "codex",
      desired_outcome: "The operator sees the latest proven change in one glance",
      receipt_id: "rec_examplemoment",
      transition_id: "tr_examplemoment",
      claimed_at: "200",
      evidence_refs: ["check:moment-selector", "live:briefing-render"],
    };
    const rejected = [
      { ...eligible, work_id: "work_audit", move_type: "audit_only" },
      { ...eligible, work_id: "work_unchanged", changed: false },
      { ...eligible, work_id: "work_open", state: "running" },
      { ...eligible, work_id: "work_no_receipt", receipt_id: "" },
      { ...eligible, work_id: "work_no_transition", transition_id: "" },
    ];

    const moment = latestProofBackedChange([
      { ...eligible, work_id: "work_older", claimed_at: "100" },
      ...rejected,
      eligible,
    ]);
    check(
      moment?.kind === "proof_backed_change" &&
        moment.evidenceLevel === "proof_backed_change" &&
        moment.workId === "work_examplemoment",
      "FR6: latest eligible proof-backed change remains at the proof-backed evidence level",
      `FR6: wrong latest moment selected (${moment?.workId ?? "none"})`,
    );
    check(
      moment?.receiptRef === "rec_examplemoment" && moment.transitionRef === "tr_examplemoment",
      "FR6: the moment carries both receipt and transition provenance",
      "FR6: the moment lost receipt or transition provenance",
    );
    check(
      moment?.headline === "The operator sees the latest proven change in one glance" && !moment.headline.includes("work_"),
      "FR6: primary copy is human-readable and keeps the machine id secondary",
      `FR6: primary copy exposed a machine id (${moment?.headline ?? "none"})`,
    );
    check(
      latestProofBackedChange(rejected) === null,
      "FR6: audit, unchanged, open, and proofless rows cannot render as proof-backed changes",
      "FR6: an ineligible row was laundered into a proof-backed change",
    );

    if (moment) {
      const html = renderToStaticMarkup(
        createElement(LatestMoment, {
          moment,
          read: { status: "live", lastGoodAt: null },
        }),
      );
      const visible = html.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ");
      check(
        visible.includes("Proof-backed change") &&
          visible.includes("Reuse and downstream benefit have not been verified") &&
          !visible.includes("What compounded") &&
          visible.includes(moment.headline) &&
          visible.includes("Rule changed") &&
          visible.includes("by codex"),
        "FR6: the Briefing moment names the human outcome, move, and producer",
        `FR6: the rendered moment lost its operator-readable hierarchy (${visible})`,
      );
      check(
        html.includes('aria-label="Proof chain: changed, receipted, transitioned"') &&
          visible.includes("Receipt") &&
          visible.includes("Transition"),
        "FR6: the moment renders a segmented proof chain and split Receipt/Transition footer",
        "FR6: the rendered moment lost its proof chain or provenance footer",
      );
      check(
        !visible.includes(moment.workId),
        "FR6: the machine work id stays out of the visible primary story",
        `FR6: the visible moment exposed the machine id (${moment.workId})`,
      );
    }

    const emptyHtml = renderToStaticMarkup(
      createElement(LatestMoment, {
        moment: null,
        read: { status: "live", lastGoodAt: null },
      }),
    );
    const unreachableHtml = renderToStaticMarkup(
      createElement(LatestMoment, {
        moment: null,
        read: { status: "unreachable", lastGoodAt: null },
      }),
    );
    check(
      emptyHtml.includes("No proof-backed change has been recorded yet") &&
        unreachableHtml.includes("feed is unreachable") &&
        !unreachableHtml.includes("No proof-backed change has been recorded yet"),
      "FR6: live-empty and unreachable remain honestly different states",
      "FR6: the moment conflated a dead workbench door with a confirmed empty ledger",
    );
  }

  return { ok, details };
}
