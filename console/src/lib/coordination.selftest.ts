/*
 * Depth tests for the coordination data plane's pure builder (the Me / Others
 * / Waiting-on view's read model over `compound coordination --json`). Would
 * FAIL if the builder invented a fleet when the door is unreachable, dropped a
 * wait edge, let a refused lane read as can-continue-ready data, hardcoded an
 * actor, or re-targeted an explicit me. Run via scripts/coordination-smoke.mjs.
 */

import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { CoordinationBoard } from "../components/coordination/CoordinationBoard";
import { OperatorCoordinationList } from "../components/coordination/OperatorCoordinationList";
import {
  buildCoordinationView,
  buildWait,
  laneGated,
  normalizeOperatorCoordination,
  type CoordinationActor,
} from "./coordination";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
  };
}

function mkActor(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    actor: "lane-x",
    doing: { work_id: "work_a", title: "ship the parser", state: "running", next_action: "run it", claimed_by: "lane-x", owner: "lane-x" },
    next_action: "",
    can_continue: false,
    wait_reason: "run_evidence",
    blocked_on: ["work.run_evidence:work_a"],
    open: 2,
    in_progress: 2,
    needs_review: 0,
    ...over,
  };
}

function mkReport(over: Record<string, unknown> = {}): Record<string, any> {
  return {
    schema_version: "compound.coordination.v0.1",
    generated_at: "2026-07-21T08:00:00Z",
    actor: "lane-x",
    actor_explicit: true,
    me: mkActor(),
    others: [mkActor({ actor: "lane-y", doing: { work_id: "work_b", title: "write the docs", state: "claimed", next_action: "", claimed_by: "lane-y", owner: "lane-y" }, can_continue: true, wait_reason: "", blocked_on: [] })],
    waits: [
      { work: "work_a", actor: "lane-x", reason: "run_evidence", blocked_on: ["work.run_evidence:work_a"] },
    ],
    integrity: [],
    ...over,
  };
}

export function runCoordinationSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];

  // F1: door unreachable => present=false, NOTHING invented (the honesty law).
  {
    const view = buildCoordinationView(null);
    checks.push({
      name: "unreachable door invents nothing",
      ok: view.present === false && view.others.length === 0 && view.waits.length === 0 && view.me.actor === "" && view.actorOptions.length === 0,
      detail: `present=${view.present} others=${view.others.length} waits=${view.waits.length} me.actor=${JSON.stringify(view.me.actor)}`,
    });
    const wrongSchema = buildCoordinationView({ schema_version: "something.else.v9" } as any);
    checks.push({
      name: "foreign schema is not the coordination door",
      ok: wrongSchema.present === false,
      detail: `present=${wrongSchema.present}`,
    });
  }

  // F2: actors flow through generically — lane-x/lane-y, no hardcoded names.
  {
    const view = buildCoordinationView(mkReport() as any);
    const names = [view.me.actor, ...view.others.map((o) => o.actor)];
    checks.push({
      name: "generic actors pass through unmodified",
      ok: view.present && names.includes("lane-x") && names.includes("lane-y"),
      detail: `actors=${names.join(",")}`,
    });
    checks.push({
      name: "actorOptions offers every actor, me first",
      ok: view.actorOptions[0] === "lane-x" && view.actorOptions.includes("lane-y"),
      detail: `options=${view.actorOptions.join(",")}`,
    });
  }

  // F3: wait edges survive the mapping — work, actor, reason, blocked_on.
  {
    const view = buildCoordinationView(mkReport() as any);
    const runEvidence = view.waits.find((w) => w.work === "work_a");
    const claimed = buildWait({ work: "work_selected_foreign", actor: "lane-x", reason: "claimed_by_other_actor", blocked_on: ["work.claimed_by_other_actor:work_selected_foreign"] });
    checks.push({
      name: "work-scoped wait edge maps with blocked_on",
      ok: !!runEvidence && runEvidence.actor === "lane-x" && runEvidence.reason === "run_evidence" && runEvidence.blockedOn.includes("work.run_evidence:work_a"),
      detail: JSON.stringify(runEvidence),
    });
    checks.push({
      name: "an exactly supplied foreign-item constraint survives mapping",
      ok: !!claimed && claimed.work === "work_selected_foreign" && claimed.blockedOn.includes("work.claimed_by_other_actor:work_selected_foreign"),
      detail: JSON.stringify(claimed),
    });
    checks.push({
      name: "me doing + wait truth maps",
      ok: view.me.doing?.workId === "work_a" && view.me.canContinue === false && view.me.waitReason === "run_evidence" && view.me.blockedOn.includes("work.run_evidence:work_a"),
      detail: JSON.stringify({ doing: view.me.doing, wait: view.me.waitReason }),
    });
  }

  // Shared queue counts do not become constraints on an idle actor.
  {
    const view = buildCoordinationView(mkReport({
      actor: "lane-idle",
      me: mkActor({ actor: "lane-idle", doing: null, wait_reason: "no_dispatchable_work", blocked_on: [], open: 500, in_progress: 300 }),
      waits: [],
    }) as any);
    checks.push({
      name: "idle actor keeps shared queue counts without invented waits or chips",
      ok: view.me.open === 500 && view.me.inProgress === 300 && view.me.blockedOn.length === 0 && view.waits.length === 0,
      detail: JSON.stringify({ open: view.me.open, inProgress: view.me.inProgress, blockedOn: view.me.blockedOn, waits: view.waits }),
    });
    const markup = renderToStaticMarkup(createElement(CoordinationBoard, {
      view, doorStatus: "live", doorError: null, execDetail: {}, runAttemptsPresent: true,
    }));
    checks.push({
      name: "empty constraints do not promise every actor can continue",
      ok: markup.includes("No specific work or lane constraints to show.") && !markup.includes("every involved actor can continue"),
      detail: JSON.stringify({ canContinue: view.me.canContinue, emptyStatePresent: markup.includes("No specific work or lane constraints to show.") }),
    });
  }

  // F4: a refused / confirmation-held lane is legible as a gate.
  {
    const refused = buildCoordinationView(
      mkReport({
        me: mkActor({ can_continue: false, wait_reason: "capacity_refused", blocked_on: ["lane.capacity_refused:lane-x"], lane: { state: "walled", remaining: 0, refused: true, message: "budget exhausted" } }),
        waits: [{ actor: "lane-x", reason: "capacity_refused", blocked_on: ["lane.capacity_refused:lane-x"], lane: { state: "walled", remaining: 0, refused: true } }],
      }) as any,
    );
    checks.push({
      name: "refused lane maps to a gated lane",
      ok: refused.me.lane !== null && refused.me.lane.refused === true && laneGated(refused.me.lane) === true && refused.me.canContinue === false,
      detail: JSON.stringify(refused.me.lane),
    });
    const held = buildWait({ actor: "lane-y", reason: "capacity_confirmation_required", lane: { state: "open", remaining: 1, confirmation_required: true } });
    checks.push({
      name: "confirmation hold reads as gated",
      ok: held.lane !== null && held.lane.confirmationRequired === true && laneGated(held.lane) === true,
      detail: JSON.stringify(held.lane),
    });
    const open = buildWait({ actor: "lane-z", reason: "run_evidence", lane: { state: "open", remaining: 5 } });
    checks.push({
      name: "open lane is not a gate",
      ok: open.lane !== null && laneGated(open.lane) === false,
      detail: JSON.stringify(open.lane),
    });
  }

  // F5: defensive mapping — ragged rows never throw, never fabricate fields.
  {
    let threw = false;
    let view: ReturnType<typeof buildCoordinationView> | null = null;
    try {
      view = buildCoordinationView({
        schema_version: "compound.coordination.v0.1",
        actor: "lane-q",
        me: { actor: "lane-q" },
        others: [null, 42, { actor: "lane-r", blocked_on: "not-an-array" }],
        waits: [{ reason: "run_evidence" }],
      } as any);
    } catch {
      threw = true;
    }
    const other = (view?.others ?? [])[0] as CoordinationActor | undefined;
    checks.push({
      name: "ragged payload maps defensively",
      ok: !threw && view !== null && view.present === true && view.others.length === 3 && (other?.blockedOn.length ?? 1) === 0 && view.waits[0]?.actor === "",
      detail: `threw=${threw} others=${view?.others.length} waits=${JSON.stringify(view?.waits)}`,
    });
  }

  // F6: operator coordination is a six-field, reference-only, bounded view.
  {
    const hostileItems = Array.from({ length: 10 }, (_, index) => ({
      event_ref: `wco_safe_${index}`,
      work_ref: `work_safe_${index}`,
      delivery_class: index % 2 === 0 ? "human_attention" : "silent_state",
      delta: {
        kind: "scope",
        subject_ref: `scope:safe:${index}`,
        before: "open",
        after: "reserved",
        body: "body sentinel in hostile delta",
        argv: ["compound", "steer"],
      },
      artifact_refs: [`artifact:sha256:${String(index).padStart(64, "0")}`],
      semantic_revision: `sha256:${String(index + 10).padStart(64, "0")}`,
      body: "body sentinel",
      actor: "hostile-actor",
      session_id: "hostile-session",
      run_attempt_id: "ratt_hostile",
      cursor: 99,
      report_ref: "hostile-report",
      native_context: "hostile-context",
      argv: ["compound", "steer"],
      acknowledgement: "acknowledge this",
    }));
    const projection = normalizeOperatorCoordination({
      schema_version: "compound.operator_coordination_projection.v0.1",
      revision: "sha256:projection",
      total: 10,
      truncated: true,
      items: hostileItems,
      body: "projection body sentinel",
    });
    const first = projection.items[0];
    const itemKeys = Object.keys(first ?? {}).sort();
    const deltaKeys = Object.keys(first?.delta ?? {}).sort();
    const serialized = JSON.stringify(projection);
    checks.push({
      name: "operator projection caps eight and whitelists six item fields",
      ok:
        projection.items.length === 8 &&
        projection.total === 10 &&
        projection.truncated === true &&
        JSON.stringify(itemKeys) ===
          JSON.stringify(["artifactRefs", "deliveryClass", "delta", "eventRef", "semanticRevision", "workRef"]) &&
        JSON.stringify(deltaKeys) === JSON.stringify(["after", "before", "kind", "subjectRef"]) &&
        !serialized.includes("body sentinel") &&
        !serialized.includes("hostile-actor") &&
        !serialized.includes("hostile-session") &&
        !serialized.includes("ratt_hostile") &&
        !serialized.includes("acknowledge this"),
      detail: `items=${projection.items.length} itemKeys=${itemKeys.join(",")} deltaKeys=${deltaKeys.join(",")}`,
    });

    const human = normalizeOperatorCoordination(
      {
        schema_version: "compound.operator_attention_projection.v0.1",
        revision: "sha256:attention",
        total: 10,
        items: hostileItems,
      },
      true,
    );
    checks.push({
      name: "human-only filter keeps only human_attention",
      ok: human.items.length === 5 && human.items.every((item) => item.deliveryClass === "human_attention"),
      detail: `items=${human.items.length} classes=${human.items.map((item) => item.deliveryClass).join(",")}`,
    });

    const valid = hostileItems[0];
    const invalidItems = [
      null,
      {},
      { ...valid, delivery_class: "urgent_message" },
      { ...valid, event_ref: "" },
      { ...valid, event_ref: "wco_" },
      { ...valid, event_ref: "event_not_wco" },
      { ...valid, work_ref: "" },
      { ...valid, work_ref: "work_" },
      { ...valid, work_ref: "not_work" },
      { ...valid, delta: { ...valid.delta, kind: "" } },
      { ...valid, delta: { ...valid.delta, kind: "<script>" } },
      { ...valid, delta: { ...valid.delta, subject_ref: "" } },
      { ...valid, delta: { ...valid.delta, subject_ref: { arbitrary: true } } },
      { ...valid, delta: { ...valid.delta, before: { arbitrary: true } } },
      { ...valid, delta: { ...valid.delta, after: "<script>" } },
      { ...valid, semantic_revision: "" },
      { ...valid, semantic_revision: "sha256:not-a-digest" },
      { ...valid, artifact_refs: [] },
      { ...valid, artifact_refs: [{ arbitrary: true }] },
      { ...valid, artifact_refs: ["body prose, not a reference"] },
    ];
    const rejected = normalizeOperatorCoordination({
      schema_version: "compound.operator_coordination_projection.v0.1",
      revision: "sha256:rejected",
      total: invalidItems.length,
      items: invalidItems,
    });
    const rejectedHuman = normalizeOperatorCoordination({
      schema_version: "compound.operator_attention_projection.v0.1",
      revision: "sha256:rejected-human",
      total: invalidItems.length,
      items: invalidItems,
    }, true);
    const rejectedView = buildCoordinationView(
      mkReport({
        operator_coordination: {
          schema_version: "compound.operator_coordination_projection.v0.1",
          revision: "sha256:rejected-view",
          total: invalidItems.length,
          items: invalidItems,
        },
      }) as any,
    );
    checks.push({
      name: "malformed operator items are rejected from every surface",
      ok:
        rejected.items.length === 0 &&
        rejectedHuman.items.length === 0 &&
        rejectedView.operatorCoordination.items.length === 0,
      detail: `general=${rejected.items.length} human=${rejectedHuman.items.length} board=${rejectedView.operatorCoordination.items.length}`,
    });

    const view = buildCoordinationView(
      mkReport({
        operator_coordination: {
          schema_version: "compound.operator_coordination_projection.v0.1",
          revision: "sha256:view",
          total: 1,
          items: hostileItems.slice(0, 1),
        },
      }) as any,
    );
    checks.push({
      name: "coordination loader maps operator_coordination separately",
      ok: view.operatorCoordination.items.length === 1 && view.operatorCoordination.items[0]?.eventRef === "wco_safe_0",
      detail: JSON.stringify(view.operatorCoordination),
    });

    const markup = renderToStaticMarkup(
      OperatorCoordinationList({
        projection: {
          ...projection,
          items: projection.items.slice(0, 1),
          total: 1,
          truncated: false,
        },
        label: "Recorded coordination",
      }),
    );
    checks.push({
      name: "operator renderer is static references and delta only",
      ok:
        markup.includes("wco_safe_0") &&
        markup.includes("work_safe_0") &&
        markup.includes("scope:safe:0") &&
        markup.includes("artifact:sha256:") &&
        !/<(?:form|button|select|input)\b/i.test(markup) &&
        !/\b(?:ack|steer)\b/i.test(markup) &&
        !markup.includes("body sentinel"),
      detail: markup,
    });
  }

  return finalize(checks);
}
